// bubble tea model, update loop, and commands.
//
// follows the Elm architecture: model holds all state, Update is a pure
// state transition (returns new model + command), View renders to string.
// all side effects happen in commands (tea.Cmd functions).
//
// cursor uses (col, row) addressing where col selects the display and
// row selects the space within that display. h/l moves between displays,
// j/k moves within. this mirrors the physical monitor layout.

package main

import (
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// -- messages --

type dataMsg fetchResult
type tickMsg time.Time

// -- derived view data --

type displayGroup struct {
	index     int
	spaces    []spaceRow
	freeCount int
	termCount int
}

type spaceRow struct {
	space   Space
	windows []Window
}

// -- model --

type model struct {
	// raw data from queries
	spaces  []Space
	windows []Window
	tmux    []TmuxSession

	// derived (rebuilt on each data refresh)
	displayGroups []displayGroup

	// cursor: (col, row) where col = display index, row = space within display
	cursorCol int
	cursorRow int

	width  int
	height int
	err    error
	ready  bool
}

func newModel() model {
	return model{}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchCmd, tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case dataMsg:
		return m.handleData(fetchResult(msg))
	case tickMsg:
		return m, tea.Batch(fetchCmd, tickCmd())
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "q" || msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if len(m.displayGroups) == 0 {
		return m, nil
	}

	switch msg.String() {
	case "j", "down":
		dg := m.displayGroups[m.cursorCol]
		if m.cursorRow < len(dg.spaces)-1 {
			m.cursorRow++
		}
	case "k", "up":
		if m.cursorRow > 0 {
			m.cursorRow--
		}
	case "l", "right":
		if m.cursorCol < len(m.displayGroups)-1 {
			m.cursorCol++
			// clamp row to new display's row count
			dg := m.displayGroups[m.cursorCol]
			if m.cursorRow >= len(dg.spaces) && len(dg.spaces) > 0 {
				m.cursorRow = len(dg.spaces) - 1
			}
		}
	case "h", "left":
		if m.cursorCol > 0 {
			m.cursorCol--
			dg := m.displayGroups[m.cursorCol]
			if m.cursorRow >= len(dg.spaces) && len(dg.spaces) > 0 {
				m.cursorRow = len(dg.spaces) - 1
			}
		}
	case "g":
		m.cursorRow = 0
	case "G":
		dg := m.displayGroups[m.cursorCol]
		if len(dg.spaces) > 0 {
			m.cursorRow = len(dg.spaces) - 1
		}
	case "enter":
		if idx, ok := m.selectedSpaceIndex(); ok {
			return m, focusSpaceCmd(idx)
		}
	}
	return m, nil
}

func (m model) handleData(result fetchResult) (tea.Model, tea.Cmd) {
	if result.err != nil {
		m.err = result.err
		return m, nil
	}
	m.spaces = result.spaces
	m.windows = result.windows
	m.tmux = result.tmux
	m.err = nil
	m.ready = true
	m.displayGroups = buildDisplayGroups(m.spaces, m.windows)

	// clamp cursor after data change (spaces may have been added/removed)
	if len(m.displayGroups) == 0 {
		m.cursorCol = 0
		m.cursorRow = 0
	} else {
		if m.cursorCol >= len(m.displayGroups) {
			m.cursorCol = len(m.displayGroups) - 1
		}
		dg := m.displayGroups[m.cursorCol]
		if m.cursorRow >= len(dg.spaces) && len(dg.spaces) > 0 {
			m.cursorRow = len(dg.spaces) - 1
		}
	}
	return m, nil
}

// -- derived data computation --

// buildDisplayGroups organizes spaces by display and attaches their
// visible (non-hidden, non-minimized) windows. each group gets its
// own free/terminal counts for the per-display summary.
func buildDisplayGroups(spaces []Space, windows []Window) []displayGroup {
	// index windows by space, filtering hidden and minimized
	windowsBySpace := make(map[int][]Window)
	for _, w := range windows {
		if w.Space <= 0 || w.IsHidden || w.IsMinimized {
			continue
		}
		windowsBySpace[w.Space] = append(windowsBySpace[w.Space], w)
	}

	// group spaces by display
	displayMap := make(map[int][]spaceRow)
	for _, s := range spaces {
		displayMap[s.Display] = append(displayMap[s.Display], spaceRow{
			space:   s,
			windows: windowsBySpace[s.Index],
		})
	}

	// sort displays, then spaces within each display
	var displayIndices []int
	for d := range displayMap {
		displayIndices = append(displayIndices, d)
	}
	sort.Ints(displayIndices)

	var groups []displayGroup
	for _, d := range displayIndices {
		rows := displayMap[d]
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].space.Index < rows[j].space.Index
		})

		freeCount := 0
		termCount := 0
		for _, row := range rows {
			if len(row.windows) == 0 {
				freeCount++
			}
			for _, w := range row.windows {
				if isTerminal(w.App) {
					termCount++
					break
				}
			}
		}

		groups = append(groups, displayGroup{
			index:     d,
			spaces:    rows,
			freeCount: freeCount,
			termCount: termCount,
		})
	}

	return groups
}

// -- navigation --

func (m model) selectedSpaceIndex() (int, bool) {
	if m.cursorCol >= len(m.displayGroups) {
		return 0, false
	}
	dg := m.displayGroups[m.cursorCol]
	if m.cursorRow >= len(dg.spaces) {
		return 0, false
	}
	return dg.spaces[m.cursorRow].space.Index, true
}

// -- commands --

func fetchCmd() tea.Msg {
	return dataMsg(fetchAll())
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func focusSpaceCmd(index int) tea.Cmd {
	return func() tea.Msg {
		focusSpace(index)
		// refresh immediately after switching so the view updates
		return dataMsg(fetchAll())
	}
}

// rendering: the View() method and all display formatting.
//
// displays are rendered as separate columns and joined horizontally
// with lipgloss, mirroring the physical monitor layout. each column
// has its own header, space rows, and summary stats.
//
// spaces are numbered from 1 within each display (relative index)
// since that's how they map to keyboard shortcuts. the absolute yabai
// index is shown in dim parentheses when it differs.

package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// -- styles --

var (
	displayStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	termStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cursorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	freeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	keyStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	helpStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// -- view --

func (m model) View() string {
	if !m.ready {
		if m.err != nil {
			return fmt.Sprintf("\n  error: %v\n\n  is yabai running?\n", m.err)
		}
		return "\n  loading...\n"
	}

	numDisplays := len(m.displayGroups)
	if numDisplays == 0 {
		return "\n  no displays found\n"
	}

	// compute column width from terminal width
	margin := 2
	gap := 6
	availWidth := m.width - 2*margin
	colWidth := availWidth
	if numDisplays > 1 {
		colWidth = (availWidth - gap*(numDisplays-1)) / numDisplays
	}
	if colWidth < 30 {
		colWidth = 30
	}

	// render each display as a separate column
	colStyle := lipgloss.NewStyle().Width(colWidth)
	var styledColumns []string
	for i, dg := range m.displayGroups {
		activeRow := -1
		if i == m.cursorCol {
			activeRow = m.cursorRow
		}
		col := renderDisplayColumn(dg, activeRow, colWidth)
		styledColumns = append(styledColumns, colStyle.Render(col))
	}

	// join columns horizontally with gap (mirrors physical monitor layout)
	var body string
	if numDisplays == 1 {
		body = styledColumns[0]
	} else {
		args := make([]string, 0, numDisplays*2-1)
		gapStr := strings.Repeat(" ", gap)
		for i, col := range styledColumns {
			if i > 0 {
				args = append(args, gapStr)
			}
			args = append(args, col)
		}
		body = lipgloss.JoinHorizontal(lipgloss.Top, args...)
	}

	var b strings.Builder
	b.WriteString("\n")
	pad := strings.Repeat(" ", margin)
	for _, line := range strings.Split(body, "\n") {
		b.WriteString(pad)
		b.WriteString(line)
		b.WriteString("\n")
	}

	// tmux sessions (global, below columns)
	if len(m.tmux) > 0 {
		var parts []string
		for _, s := range m.tmux {
			parts = append(parts, fmt.Sprintf("%s:%dw", s.Name, s.Windows))
		}
		b.WriteString("\n")
		b.WriteString(pad)
		b.WriteString(dimStyle.Render("tmux: " + strings.Join(parts, "  ")))
		b.WriteString("\n")
	}

	// keybinds
	b.WriteString("\n")
	b.WriteString(pad)
	b.WriteString(renderHelp(numDisplays > 1))
	b.WriteString("\n")

	return b.String()
}

// -- column rendering --

func renderDisplayColumn(dg displayGroup, cursorRow int, colWidth int) string {
	var b strings.Builder

	// header
	b.WriteString(displayStyle.Render(fmt.Sprintf("display %d", dg.index)))
	b.WriteString("  ")
	b.WriteString(dimStyle.Render(fmt.Sprintf("%d spaces", len(dg.spaces))))
	b.WriteString("\n")

	// how much room for window titles after the fixed-width prefix
	// rough overhead: "  > " (4) + "1(10)" (5) + " * " (3) + "kitty: " (7) ≈ 19
	maxTitleLen := colWidth - 22
	if maxTitleLen < 10 {
		maxTitleLen = 10
	}

	// space rows
	for i, row := range dg.spaces {
		relIdx := i + 1
		absIdx := row.space.Index
		isSelected := i == cursorRow
		b.WriteString(renderSpaceRow(row, relIdx, absIdx, isSelected, maxTitleLen))
		b.WriteString("\n")
	}

	// per-display summary
	b.WriteString("\n")
	if dg.freeCount > 0 {
		b.WriteString(freeStyle.Render(fmt.Sprintf("%d free", dg.freeCount)))
	} else {
		b.WriteString(warnStyle.Render("0 free"))
	}
	b.WriteString("  ")
	b.WriteString(fmt.Sprintf("%d terminals", dg.termCount))

	return b.String()
}

// -- row rendering --

func renderSpaceRow(row spaceRow, relIdx, absIdx int, isSelected bool, maxTitleLen int) string {
	cursor := "  "
	if isSelected {
		cursor = cursorStyle.Render("> ")
	}

	// focus indicator: * = focused, · = visible on other display
	indicator := " "
	if row.space.HasFocus {
		indicator = "*"
	}
	if !row.space.HasFocus && row.space.IsVisible {
		indicator = "\u00b7"
	}

	// relative index prominent, absolute in dim parens only when they differ
	indexStr := fmt.Sprintf("%2d", relIdx)
	if relIdx != absIdx {
		indexStr += dimStyle.Render(fmt.Sprintf("(%d)", absIdx))
	}

	// optional space label from yabai config
	label := ""
	if row.space.Label != "" {
		label = dimStyle.Render(fmt.Sprintf("[%s] ", row.space.Label))
	}

	windowText := renderWindows(row.windows, maxTitleLen)

	return fmt.Sprintf("%s%s %s  %s%s", cursor, indexStr, indicator, label, windowText)
}

// -- window rendering --

func renderWindows(windows []Window, maxTitleLen int) string {
	if len(windows) == 0 {
		return dimStyle.Render("--")
	}

	var terminals, browsers, others []Window
	for _, w := range windows {
		if isTerminal(w.App) {
			terminals = append(terminals, w)
		} else if isBrowser(w.App) {
			browsers = append(browsers, w)
		} else {
			others = append(others, w)
		}
	}

	var parts []string

	// terminals: green, app + title (title often reveals tmux session or cwd)
	for _, w := range terminals {
		title := strings.TrimSpace(w.Title)
		title = truncateStr(title, maxTitleLen)
		if title != "" {
			parts = append(parts, termStyle.Render(fmt.Sprintf("%s: %s", w.App, title)))
		} else {
			parts = append(parts, termStyle.Render(w.App))
		}
	}

	// browsers: show individual page titles (cleaned of " — Firefox" etc.)
	for _, w := range browsers {
		title := cleanBrowserTitle(strings.TrimSpace(w.Title))
		title = truncateStr(title, maxTitleLen)
		if title != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", w.App, title))
		} else {
			parts = append(parts, w.App)
		}
	}

	// everything else: group by app name, show count when duplicated
	appCounts := make(map[string]int)
	for _, w := range others {
		appCounts[w.App]++
	}
	var appNames []string
	for app := range appCounts {
		appNames = append(appNames, app)
	}
	sort.Strings(appNames)

	for _, app := range appNames {
		count := appCounts[app]
		if count > 1 {
			parts = append(parts, fmt.Sprintf("%s (%d)", app, count))
		} else {
			parts = append(parts, app)
		}
	}

	return strings.Join(parts, "  ")
}

// -- helpers --

func renderHelp(multiDisplay bool) string {
	binds := []struct{ key, desc string }{
		{"q", "quit"},
		{"j/k", "navigate"},
	}
	if multiDisplay {
		binds = append(binds, struct{ key, desc string }{"h/l", "display"})
	}
	binds = append(binds, struct{ key, desc string }{"enter", "focus"})

	var parts []string
	for _, b := range binds {
		parts = append(parts, keyStyle.Render(b.key)+" "+helpStyle.Render(b.desc))
	}
	return strings.Join(parts, "  ")
}

func truncateStr(s string, maxLen int) string {
	if maxLen < 4 {
		maxLen = 4
	}
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}

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
	"time"

	"github.com/charmbracelet/lipgloss"
)

// -- styles --

var (
	displayStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
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

	// compute per-session staleness for bubbling up to space rows.
	// uses most recent pane activity per session (freshest pane wins).
	productiveActivity := bestProductiveActivity(m.tmuxPanes)

	// render each display as a separate column
	colStyle := lipgloss.NewStyle().Width(colWidth)
	var styledColumns []string
	for i, dg := range m.displayGroups {
		activeRow := -1
		if i == m.cursorCol {
			activeRow = m.cursorRow
		}
		col := renderDisplayColumn(dg, activeRow, colWidth, m.tmuxByDisplay[dg.index], productiveActivity)
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

	// detached tmux sessions (not attached to any terminal on a display)
	if len(m.detachedTmux) > 0 {
		b.WriteString(renderTmuxSessions(m.detachedTmux, "detached"))
	}

	// keybinds
	b.WriteString("\n")
	b.WriteString(pad)
	b.WriteString(renderHelp(numDisplays > 1))
	b.WriteString("\n")

	return b.String()
}

// -- column rendering --

func renderDisplayColumn(dg displayGroup, cursorRow int, colWidth int, tmuxPanes []TmuxPane, productiveActivity map[string]time.Time) string {
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

	// build session name → panes lookup so space rows can inline tmux detail
	tmuxBySession := make(map[string][]TmuxPane)
	for _, p := range tmuxPanes {
		tmuxBySession[p.SessionName] = append(tmuxBySession[p.SessionName], p)
	}

	// space rows
	for i, row := range dg.spaces {
		relIdx := i + 1
		absIdx := row.space.Index
		isSelected := i == cursorRow
		b.WriteString(renderSpaceRow(row, relIdx, absIdx, isSelected, maxTitleLen, productiveActivity, tmuxBySession))
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

func renderSpaceRow(row spaceRow, relIdx, absIdx int, isSelected bool, maxTitleLen int, productiveActivity map[string]time.Time, tmuxBySession map[string][]TmuxPane) string {
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

	// compute worst (most stale) productive session on this space.
	// only productive panes contribute — bash/btop sitting idle isn't meaningful.
	var worstProductiveActivity time.Time
	hasProductiveSession := false
	for _, w := range row.windows {
		if !isTerminal(w.App) {
			continue
		}
		if activity, ok := productiveActivity[w.Title]; ok {
			if !hasProductiveSession || activity.Before(worstProductiveActivity) {
				worstProductiveActivity = activity
				hasProductiveSession = true
			}
		}
	}

	// relative index colored only when productive work is happening on this space
	indexStr := fmt.Sprintf("%2d", relIdx)
	if hasProductiveSession {
		indexStr = stalenessStyle(worstProductiveActivity).Render(fmt.Sprintf("%2d", relIdx))
	}
	if relIdx != absIdx {
		indexStr += dimStyle.Render(fmt.Sprintf("(%d)", absIdx))
	}

	// optional space label from yabai config
	label := ""
	if row.space.Label != "" {
		label = dimStyle.Render(fmt.Sprintf("[%s] ", row.space.Label))
	}

	windowText := renderWindows(row.windows, maxTitleLen, productiveActivity)

	mainLine := fmt.Sprintf("%s%s %s  %s%s", cursor, indexStr, indicator, label, windowText)

	// inline tmux pane detail under terminals on this space.
	// matches terminal window titles to tmux session names.
	// prefix aligns with content after the fixed-width space row prefix:
	// cursor(2) + index(2) + space(1) + indicator(1) + gap(2) = 8 chars
	indent := "        "
	var tmuxLines []string
	for _, w := range row.windows {
		if !isTerminal(w.App) {
			continue
		}
		sessionPanes, ok := tmuxBySession[strings.TrimSpace(w.Title)]
		if !ok {
			continue
		}
		for _, win := range groupPanesByWindow(sessionPanes) {
			windowLabel := fmt.Sprintf("%d:%s", win.index, win.name)

			// color window label by best productive pane activity
			var bestProductive time.Time
			windowHasProductive := false
			for _, p := range win.panes {
				if isProductive(p.CurrentCommand) {
					if !windowHasProductive || p.LastActivity.After(bestProductive) {
						bestProductive = p.LastActivity
						windowHasProductive = true
					}
				}
			}

			var line strings.Builder
			line.WriteString(indent)
			if windowHasProductive {
				line.WriteString(stalenessStyle(bestProductive).Render(windowLabel))
			} else {
				line.WriteString(dimStyle.Render(windowLabel))
			}

			// panes inline after window label
			for _, p := range win.panes {
				style := dimStyle
				if isProductive(p.CurrentCommand) {
					style = stalenessStyle(p.LastActivity)
				}
				line.WriteString("  ")
				line.WriteString(style.Render("\u258e"))
				line.WriteString(" ")
				line.WriteString(style.Render(p.CurrentCommand))
				line.WriteString(" ")
				line.WriteString(dimStyle.Render(formatRelativeTime(p.LastActivity)))
			}
			tmuxLines = append(tmuxLines, line.String())
		}
	}

	if len(tmuxLines) > 0 {
		return mainLine + "\n" + strings.Join(tmuxLines, "\n")
	}
	return mainLine
}

// -- window rendering --

func renderWindows(windows []Window, maxTitleLen int, productiveActivity map[string]time.Time) string {
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

	// terminals: only colored when session has productive activity.
	// non-productive sessions render plain — their staleness is meaningless.
	for _, w := range terminals {
		rawTitle := strings.TrimSpace(w.Title)
		displayTitle := truncateStr(rawTitle, maxTitleLen)

		var entry string
		if displayTitle != "" {
			entry = fmt.Sprintf("%s: %s", w.App, displayTitle)
		} else {
			entry = w.App
		}

		if activity, ok := productiveActivity[rawTitle]; ok {
			parts = append(parts, stalenessStyle(activity).Render(entry))
		} else {
			parts = append(parts, entry)
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

// -- tmux staleness --

// bestProductiveActivity maps session name → most recent productive pane activity.
// only panes running a productive command (see config.go) contribute.
// used to color terminal entries and space index numbers — non-productive
// panes are ignored since their staleness is meaningless.
func bestProductiveActivity(panes []TmuxPane) map[string]time.Time {
	result := make(map[string]time.Time)
	for _, p := range panes {
		if !isProductive(p.CurrentCommand) {
			continue
		}
		if existing, ok := result[p.SessionName]; !ok || p.LastActivity.After(existing) {
			result[p.SessionName] = p.LastActivity
		}
	}
	return result
}

// -- tmux block rendering --

type tmuxWindowGroup struct {
	index int
	name  string
	panes []TmuxPane
}

type tmuxSessionGroup struct {
	name    string
	windows []tmuxWindowGroup
}

// groupPanesBySession groups panes into session → window → pane hierarchy,
// preserving tmux's natural ordering at each level
func groupPanesBySession(panes []TmuxPane) []tmuxSessionGroup {
	sessionMap := make(map[string][]TmuxPane)
	var sessionOrder []string
	for _, p := range panes {
		if _, exists := sessionMap[p.SessionName]; !exists {
			sessionOrder = append(sessionOrder, p.SessionName)
		}
		sessionMap[p.SessionName] = append(sessionMap[p.SessionName], p)
	}
	var groups []tmuxSessionGroup
	for _, name := range sessionOrder {
		groups = append(groups, tmuxSessionGroup{
			name:    name,
			windows: groupPanesByWindow(sessionMap[name]),
		})
	}
	return groups
}

// groupPanesByWindow splits a session's panes into per-window groups
func groupPanesByWindow(panes []TmuxPane) []tmuxWindowGroup {
	windowMap := make(map[int][]TmuxPane)
	windowNames := make(map[int]string)
	var windowOrder []int
	for _, p := range panes {
		if _, exists := windowMap[p.WindowIndex]; !exists {
			windowOrder = append(windowOrder, p.WindowIndex)
		}
		windowMap[p.WindowIndex] = append(windowMap[p.WindowIndex], p)
		windowNames[p.WindowIndex] = p.WindowName
	}
	var groups []tmuxWindowGroup
	for _, idx := range windowOrder {
		groups = append(groups, tmuxWindowGroup{
			index: idx,
			name:  windowNames[idx],
			panes: windowMap[idx],
		})
	}
	return groups
}

// stalenessStyle returns a color style reflecting how recently a pane had output.
// five tiers: green (<1m) → yellow (<5m) → orange (<15m) → dark orange (<1h) → red (1h+)
func stalenessStyle(lastActivity time.Time) lipgloss.Style {
	age := time.Since(lastActivity)
	if age < time.Minute {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	}
	if age < 5*time.Minute {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	}
	if age < 15*time.Minute {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	}
	if age < time.Hour {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("202"))
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
}

// formatRelativeTime renders a duration since last activity as a compact string
func formatRelativeTime(t time.Time) string {
	age := time.Since(t)
	if age < 5*time.Second {
		return "now"
	}
	if age < time.Minute {
		return fmt.Sprintf("%ds", int(age.Seconds()))
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age.Minutes()))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age.Hours()))
	}
	return fmt.Sprintf("%dd", int(age.Hours()/24))
}

// formatHistorySize renders scroll buffer line count compactly
func formatHistorySize(lines int) string {
	if lines < 1000 {
		return fmt.Sprintf("%d", lines)
	}
	if lines < 10000 {
		return fmt.Sprintf("%.1fk", float64(lines)/1000)
	}
	return fmt.Sprintf("%dk", lines/1000)
}

// renderTmuxSessions renders tmux panes grouped by session with staleness
// coloring, scroll buffer sizes, and time since last activity.
// header is the section label (e.g. "tmux" or "detached").
func renderTmuxSessions(panes []TmuxPane, header string) string {
	if len(panes) == 0 {
		return ""
	}
	sessions := groupPanesBySession(panes)

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(header))
	b.WriteString("\n")

	for _, session := range sessions {
		b.WriteString("  ")
		b.WriteString(session.name)
		b.WriteString("\n")

		for _, window := range session.windows {
			// window label — colored by best productive pane if any
			windowLabel := fmt.Sprintf("%d:%s", window.index, window.name)
			var bestProductive time.Time
			windowHasProductive := false
			for _, p := range window.panes {
				if isProductive(p.CurrentCommand) {
					if !windowHasProductive || p.LastActivity.After(bestProductive) {
						bestProductive = p.LastActivity
						windowHasProductive = true
					}
				}
			}

			b.WriteString("    ")
			if windowHasProductive {
				b.WriteString(stalenessStyle(bestProductive).Render(windowLabel))
			} else {
				b.WriteString(dimStyle.Render(windowLabel))
			}

			// panes inline on the same line as the window header
			for _, p := range window.panes {
				style := dimStyle
				if isProductive(p.CurrentCommand) {
					style = stalenessStyle(p.LastActivity)
				}
				timeStr := formatRelativeTime(p.LastActivity)
				b.WriteString("  ")
				b.WriteString(style.Render("\u258e"))
				b.WriteString(" ")
				b.WriteString(style.Render(p.CurrentCommand))
				b.WriteString(" ")
				b.WriteString(dimStyle.Render(timeStr))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

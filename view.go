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
	"github.com/mattn/go-runewidth"
)

// -- styles --

var (
	displayStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	dimStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	cursorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	freeStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warnStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	keyStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	helpStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	lyricActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	lyricNearStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	lyricFarStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
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
		col := renderDisplayColumn(dg, activeRow, colWidth, m.tmuxByDisplay[dg.index], productiveActivity, m.nvimBuffers)
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

	pad := strings.Repeat(" ", margin)

	// build the page in three slabs:
	//   top    = spaces grid + detached tmux + now-playing line
	//   middle = lyrics viewport (claims all remaining terminal height)
	//   bottom = keybinds help
	// computing lengths up front lets the lyrics block expand or shrink
	// to fill exactly the leftover vertical space.

	var top strings.Builder
	top.WriteString("\n")
	for _, line := range strings.Split(body, "\n") {
		top.WriteString(pad)
		top.WriteString(line)
		top.WriteString("\n")
	}

	if len(m.detachedTmux) > 0 {
		top.WriteString(renderTmuxSessions(m.detachedTmux, "detached", m.nvimBuffers))
	}

	nowPlayingDisplay := m.playingMeta.DisplayString()
	if nowPlayingDisplay != "" {
		top.WriteString("\n")
		top.WriteString(pad)
		top.WriteString(dimStyle.Render("\u266a "))
		top.WriteString(dimStyle.Render(nowPlayingDisplay))
		top.WriteString("\n")
	}

	bottom := "\n" + pad + renderHelp(numDisplays > 1) + "\n"

	topStr := top.String()

	// only render the lyrics viewport once we know a song is playing and
	// we have data (or are actively fetching it). avoids a permanent
	// empty band at the bottom of the screen when nothing is playing.
	artist, title := m.playingMeta.Artist, m.playingMeta.Title
	lyricsBlock := ""
	if artist != "" && title != "" {
		used := countLines(topStr) + countLines(bottom)
		// leave one breathing-room line above the lyrics block
		remaining := m.height - used - 1
		if remaining >= 3 {
			positionFrac := m.playingMeta.CurrentFraction(time.Now())
			lyricsBlock = "\n" + renderLyricsViewport(artist, title, positionFrac, m.width-2*margin, remaining, pad)
		}
	}

	return topStr + lyricsBlock + bottom
}

// countLines returns the number of '\n' separators in s. used to budget
// remaining vertical space for the lyrics viewport.
func countLines(s string) int {
	return strings.Count(s, "\n")
}

// -- column rendering --

func renderDisplayColumn(dg displayGroup, cursorRow int, colWidth int, tmuxPanes []TmuxPane, productiveActivity map[string]time.Time, nvimBuffers map[int][]NvimBuffer) string {
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
		b.WriteString(renderSpaceRow(row, relIdx, absIdx, isSelected, maxTitleLen, productiveActivity, tmuxBySession, nvimBuffers))
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

func renderSpaceRow(row spaceRow, relIdx, absIdx int, isSelected bool, maxTitleLen int, productiveActivity map[string]time.Time, tmuxBySession map[string][]TmuxPane, nvimBuffers map[int][]NvimBuffer) string {
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
				if p.CurrentCommand == "nvim" {
					if extra := nvimBufferLabel(nvimBuffers[p.PanePID]); extra != "" {
						line.WriteString(" ")
						line.WriteString(dimStyle.Render(extra))
					}
				}
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

// nvimBufferLabel renders the active buffer's basename plus a [N] suffix
// when more than one buffer is open. returns empty string when there's no
// active buffer (e.g. nvim just launched on a [No Name] scratch).
func nvimBufferLabel(buffers []NvimBuffer) string {
	if len(buffers) == 0 {
		return ""
	}
	var active *NvimBuffer
	for i := range buffers {
		if buffers[i].IsCurrent {
			active = &buffers[i]
			break
		}
	}
	if active == nil {
		return ""
	}
	name := filepathBase(active.Path)
	if active.IsModified {
		name += "[+]"
	}
	if len(buffers) > 1 {
		name += fmt.Sprintf(" [%d]", len(buffers))
	}
	return name
}

// filepathBase returns the final path segment, hand-rolled to avoid
// importing path/filepath just for this one call site.
func filepathBase(p string) string {
	if p == "" {
		return ""
	}
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
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
func renderTmuxSessions(panes []TmuxPane, header string, nvimBuffers map[int][]NvimBuffer) string {
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
				if p.CurrentCommand == "nvim" {
					if extra := nvimBufferLabel(nvimBuffers[p.PanePID]); extra != "" {
						b.WriteString(" ")
						b.WriteString(dimStyle.Render(extra))
					}
				}
				b.WriteString(" ")
				b.WriteString(dimStyle.Render(timeStr))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// -- lyrics viewport --

// renderLyricsViewport paints a full-width, height-fixed block of lyrics
// for the currently-playing song. when synced LRC data is available, the
// active line is held centered and surrounding lines fade with distance
// from the play head. positionFrac is a 0.0-1.0 seek decimal; -1 means
// the play head is unknown (renders from the song start).
//
// the function always returns exactly `height` newline-separated lines so
// the layout above and below stays stable as the viewport refreshes.
func renderLyricsViewport(artist, title string, positionFrac float64, width, height int, pad string) string {
	lyrics := getCachedLyrics(artist, title)

	label := " lyrics "
	if lyrics != nil && lyrics.Found && lyrics.Source != "" {
		label = " lyrics \u00b7 " + lyrics.Source + " "
	}
	header := dimStyle.Render("\u2500"+label) +
		dimStyle.Render(strings.Repeat("\u2500", maxInt(0, width-len(label)-1)))
	lines := []string{pad + header}

	bodyHeight := height - 1
	if bodyHeight < 1 {
		return padToHeight(lines, height, pad)
	}

	if lyrics == nil {
		if lyricsFetchInFlight(artist, title) {
			lines = append(lines, pad+dimStyle.Render("loading lyrics..."))
		}
		return padToHeight(lines, height, pad)
	}
	if !lyrics.Found {
		// chain tried all providers (lrclib + netease + qqmusic); none had
		// a match. message intentionally generic — naming any specific
		// provider would be misleading.
		lines = append(lines, pad+dimStyle.Render("no lyrics found"))
		return padToHeight(lines, height, pad)
	}

	// synced path — each LRC line expands into 1-3 visual rows:
	//   row 1: original text, left-aligned (with "> " marker on active LRC)
	//   row 2: romaji, right-aligned, when present
	//   row 3: english translation, right-aligned, when present
	// the viewport selects a window of visual rows centered on the active
	// LRC's first row. all rows belonging to the active LRC share the
	// active highlight so the eye snaps to the whole block at once.
	if len(lyrics.Synced) > 0 {
		havePosition := positionFrac >= 0
		songLen := lyrics.Duration
		if songLen == 0 {
			for _, l := range lyrics.Synced {
				if l.At > songLen {
					songLen = l.At
				}
			}
		}

		// active drives the bold/highlight cascade; scrollAnchor drives the
		// viewport's vertical centering. they're usually the same, but
		// diverge in two cases worth handling:
		//   1. position is known but before the first lyric — active stays
		//      -1 (no entry has actually fired), scrollAnchor points at
		//      the upcoming line so the viewport drifts towards it instead
		//      of sitting frozen at the top
		//   2. position unknown — both stay -1, viewport sits at top
		active := -1
		scrollAnchor := -1
		var elapsed time.Duration
		if havePosition && songLen > 0 {
			frac := positionFrac
			if frac > 1.0 {
				frac = 1.0
			}
			elapsed = time.Duration(float64(songLen) * frac)
			active = activeLineIndex(lyrics.Synced, elapsed)
			scrollAnchor = active
			if scrollAnchor < 0 {
				// no LRC line has fired yet — anchor to the upcoming one so
				// the viewport reflects progress through the pre-vocal
				// runway. fall back to position-fraction estimate when
				// even the upcoming line is past the end of the LRC array.
				for i := range lyrics.Synced {
					if lyrics.Synced[i].At > elapsed {
						scrollAnchor = i
						break
					}
				}
				if scrollAnchor < 0 && len(lyrics.Synced) > 0 {
					est := int(frac * float64(len(lyrics.Synced)))
					if est >= len(lyrics.Synced) {
						est = len(lyrics.Synced) - 1
					}
					scrollAnchor = est
				}
			}
		}

		visual := flattenSynced(lyrics.Synced)

		// find the visual index of the scroll anchor's first (text) row.
		// used to center the viewport. when scrollAnchor differs from
		// active (pre-vocal case), no row gets the active bold treatment
		// but the viewport still drifts towards the upcoming entry.
		activeVisual := -1
		if scrollAnchor >= 0 {
			for vi, v := range visual {
				if v.srcIdx == scrollAnchor && v.kind == lyricKindText {
					activeVisual = vi
					break
				}
			}
		}

		// when the active entry is a synthetic gap marker, render every
		// other row in far-style. the default near-style cascade (lines
		// within 2 of active) would lift the surrounding lyrics into a
		// semi-highlighted state and make the upcoming line look like it
		// shared focus with the ♪ marker — confusing during an
		// instrumental where the marker should be the only emphasized row.
		activeIsMarker := active >= 0 && lyrics.Synced[active].Text == gapMarkerText

		half := bodyHeight / 2
		start := 0
		if activeVisual >= 0 {
			start = activeVisual - half
			if start < 0 {
				start = 0
			}
		}
		end := start + bodyHeight
		if end > len(visual) {
			end = len(visual)
			start = end - bodyHeight
			if start < 0 {
				start = 0
			}
		}

		for vi := start; vi < end; vi++ {
			v := visual[vi]
			styled := renderLyricVisualRow(v, active, havePosition, activeIsMarker, width)
			lines = append(lines, pad+styled)
		}
		return padToHeight(lines, height, pad)
	}

	// plain text fallback — no scroll, just show whatever fits from the top.
	plainLines := strings.Split(lyrics.Plain, "\n")
	for i := 0; i < bodyHeight && i < len(plainLines); i++ {
		lines = append(lines, pad+lyricFarStyle.Render(truncateStr(plainLines[i], width-2)))
	}
	return padToHeight(lines, height, pad)
}

// padToHeight pads a list of rendered lines with blank rows so the
// resulting string has exactly `height` newline-terminated lines. keeps
// the rest of the TUI from jumping when the lyrics block has fewer
// lines than the available viewport.
func padToHeight(lines []string, height int, pad string) string {
	for len(lines) < height {
		lines = append(lines, pad)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n") + "\n"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// -- visual-row expansion for synced lyrics --

// lyricRowKind tags each visual row with what part of an LRC entry it
// represents. drives alignment + style picks downstream.
type lyricRowKind int

const (
	lyricKindText lyricRowKind = iota
	lyricKindRomaji
	lyricKindTranslation
)

// lyricVisualRow is one rendered line within the lyrics viewport. each
// source LRC entry expands into 1-3 of these depending on which optional
// fields are populated.
type lyricVisualRow struct {
	srcIdx int
	kind   lyricRowKind
	text   string
}

// flattenSynced expands an LRC stream into the per-visual-row stream the
// viewport consumes. blank-text entries are dropped upstream by
// dropBlankLines so every entry here contributes at least one row (the
// original text); romaji + translation rows only appear when populated.
func flattenSynced(synced []LRCLine) []lyricVisualRow {
	rows := make([]lyricVisualRow, 0, len(synced))
	for i, l := range synced {
		if l.Text == "" {
			continue // defensive: should be stripped by dropBlankLines already
		}
		rows = append(rows, lyricVisualRow{srcIdx: i, kind: lyricKindText, text: l.Text})
		if l.Romaji != "" {
			rows = append(rows, lyricVisualRow{srcIdx: i, kind: lyricKindRomaji, text: l.Romaji})
		}
		if l.Translation != "" {
			rows = append(rows, lyricVisualRow{srcIdx: i, kind: lyricKindTranslation, text: l.Translation})
		}
	}
	return rows
}

// renderLyricVisualRow paints one row from the expanded stream into the
// styled string the viewport joins. left-aligns text rows (with marker)
// and right-aligns romaji / translation rows. style cascades from the
// LRC index's distance to the active line so the whole 1-3 row block of
// the active LRC stays uniformly highlighted. activeIsMarker tells the
// style picker to suppress the near-style cascade so a music-note gap
// marker doesn't visually drag the adjacent lyric into its focus.
func renderLyricVisualRow(v lyricVisualRow, activeIdx int, havePosition, activeIsMarker bool, width int) string {
	style := pickLyricStyle(v.srcIdx, activeIdx, havePosition, activeIsMarker)

	// available content width after the 2-char left gutter (the gutter
	// holds either "> " for the active text row or "  " padding for
	// alignment with the marker column).
	inner := width - 2
	if inner < 4 {
		inner = 4
	}

	switch v.kind {
	case lyricKindText:
		// gap markers — synthetic ♪ between lyrics — render center-aligned
		// without the "> " marker so they read as filler rather than as
		// cryptic single-char lyrics. active styling still applies so the
		// user knows the play head is sitting in the instrumental.
		if v.text == gapMarkerText {
			body := v.text
			pad := (inner - runewidth.StringWidth(body)) / 2
			if pad < 0 {
				pad = 0
			}
			return style.Render("  " + strings.Repeat(" ", pad) + body)
		}
		gutter := "  "
		if havePosition && v.srcIdx == activeIdx {
			gutter = "> "
		}
		body := truncateForWidth(v.text, inner)
		return style.Render(gutter + body)
	default: // romaji or translation — right-aligned, no marker
		body := truncateForWidth(v.text, inner)
		bodyWidth := runewidth.StringWidth(body)
		padCount := inner - bodyWidth
		if padCount < 0 {
			padCount = 0
		}
		return style.Render("  " + strings.Repeat(" ", padCount) + body)
	}
}

// pickLyricStyle returns the style to use for a row belonging to LRC
// index srcIdx, given the currently active line. when no position is
// known we use the near style uniformly so nothing reads as "active".
// when activeIsMarker is true (instrumental ♪ playing), every non-active
// row drops to far-style so the marker is the only emphasized row.
func pickLyricStyle(srcIdx, activeIdx int, havePosition, activeIsMarker bool) lipgloss.Style {
	if !havePosition || activeIdx < 0 {
		return lyricNearStyle
	}
	if srcIdx == activeIdx {
		return lyricActiveStyle
	}
	if activeIsMarker {
		return lyricFarStyle
	}
	distance := srcIdx - activeIdx
	if distance < 0 {
		distance = -distance
	}
	if distance <= 2 {
		return lyricNearStyle
	}
	return lyricFarStyle
}

// truncateForWidth trims s so its on-screen width (per east-asian
// wide-char rules) fits within `width` cells, appending an ellipsis when
// truncation occurred. uses runewidth so CJK and other wide chars are
// counted accurately.
func truncateForWidth(s string, width int) string {
	if width < 2 {
		width = 2
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	const ellipsis = "..."
	target := width - len(ellipsis)
	if target < 1 {
		return s[:1]
	}
	var b strings.Builder
	consumed := 0
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if consumed+w > target {
			break
		}
		b.WriteRune(r)
		consumed += w
	}
	return b.String() + ellipsis
}

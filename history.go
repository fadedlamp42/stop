// history.go — discontinuity detection over snapshot history.
//
// looks for restarts, crashes, and sleeps in the snapshot timeline by
// detecting abrupt changes between consecutive snapshots: large wall-clock
// gaps (the snapshot loop wasn't running) or sharp drops in tmux pane count
// (tmux server restarted). for each event we print the last known state
// before the discontinuity so the previous workspace can be reconstructed.

package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// historyOptions controls the lookback window and discontinuity thresholds.
type historyOptions struct {
	since             time.Time     // ignore snapshots older than this
	gapThreshold      time.Duration // minimum wall-clock gap to flag
	collapseThreshold int           // minimum drop in tmux pane count to flag (0 disables)
	limit             int           // max number of discontinuities to print (0 = no limit)
}

// discontinuity describes an abrupt transition between two consecutive snapshots.
type discontinuity struct {
	kind        string // "gap", "collapse", or "gap+collapse"
	beforeID    int64
	afterID     int64
	beforeAt    time.Time
	afterAt     time.Time
	gap         time.Duration
	beforePanes int
	afterPanes  int
}

// historyCommand is the entry point for the `stop history` subcommand.
// opens the snapshot db, scans for discontinuities, and prints the last
// known state before each one (newest first).
func historyCommand(opts historyOptions) error {
	db, err := openSnapshotDB()
	if err != nil {
		return err
	}
	defer db.Close()

	events, err := findDiscontinuities(db, opts)
	if err != nil {
		return err
	}

	if len(events) == 0 {
		fmt.Println("no discontinuities found in the lookback window.")
		return nil
	}

	// newest-first so the most recent crash is the first thing visible
	printed := 0
	for i := len(events) - 1; i >= 0; i-- {
		printDiscontinuity(os.Stdout, db, events[i])
		fmt.Println()
		printed++
		if opts.limit > 0 && printed >= opts.limit {
			break
		}
	}
	return nil
}

// findDiscontinuities walks the snapshots table in chronological order and
// returns every transition where the wall-clock gap exceeds gapThreshold or
// the tmux pane count drops by at least collapseThreshold.
func findDiscontinuities(db *sql.DB, opts historyOptions) ([]discontinuity, error) {
	rows, err := db.Query(`
		SELECT s.id, s.captured_at, COALESCE(p.cnt, 0) AS pane_count
		FROM snapshots s
		LEFT JOIN (
			SELECT snapshot_id, COUNT(*) AS cnt
			FROM snapshot_tmux_panes
			GROUP BY snapshot_id
		) p ON p.snapshot_id = s.id
		WHERE s.captured_at >= ?
		ORDER BY s.id ASC
	`, opts.since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("querying snapshots: %w", err)
	}
	defer rows.Close()

	type rowState struct {
		id        int64
		at        time.Time
		paneCount int
	}
	var prev *rowState
	var found []discontinuity
	for rows.Next() {
		var r rowState
		var atStr string
		if err := rows.Scan(&r.id, &atStr, &r.paneCount); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, atStr)
		if err != nil {
			continue
		}
		r.at = t
		if prev != nil {
			gap := r.at.Sub(prev.at)
			drop := prev.paneCount - r.paneCount
			isGap := gap >= opts.gapThreshold
			isCollapse := opts.collapseThreshold > 0 && drop >= opts.collapseThreshold && prev.paneCount > 0
			if isGap || isCollapse {
				kind := "gap+collapse"
				if isGap && !isCollapse {
					kind = "gap"
				}
				if !isGap && isCollapse {
					kind = "collapse"
				}
				found = append(found, discontinuity{
					kind:        kind,
					beforeID:    prev.id,
					afterID:     r.id,
					beforeAt:    prev.at,
					afterAt:     r.at,
					gap:         gap,
					beforePanes: prev.paneCount,
					afterPanes:  r.paneCount,
				})
			}
		}
		copyRow := r
		prev = &copyRow
	}
	return found, rows.Err()
}

// printDiscontinuity writes a header describing the event followed by the
// full pre-discontinuity workspace state.
func printDiscontinuity(w io.Writer, db *sql.DB, ev discontinuity) {
	fmt.Fprintf(w, "── discontinuity (%s) ──\n", ev.kind)
	fmt.Fprintf(w, "  %s → %s\n", formatTime(ev.beforeAt), formatTime(ev.afterAt))
	fmt.Fprintf(w, "  gap %s, panes %d → %d\n\n", humanDuration(ev.gap), ev.beforePanes, ev.afterPanes)
	fmt.Fprintf(w, "last state before (snapshot %d):\n", ev.beforeID)
	if err := printSnapshotState(w, db, ev.beforeID); err != nil {
		fmt.Fprintf(w, "  (error reading snapshot %d: %v)\n", ev.beforeID, err)
	}
}

// printSnapshotState writes the spaces / visible windows / tmux session
// inventory for a single snapshot id in human-readable form.
func printSnapshotState(w io.Writer, db *sql.DB, snapshotID int64) error {
	if err := printSpacesAndWindows(w, db, snapshotID); err != nil {
		return err
	}
	return printTmuxInventory(w, db, snapshotID)
}

// printSpacesAndWindows summarizes yabai spaces (count, focus, visible)
// and lists the windows on currently-visible spaces.
func printSpacesAndWindows(w io.Writer, db *sql.DB, snapshotID int64) error {
	srows, err := db.Query(
		"SELECT space_index, display_index, has_focus, is_visible FROM snapshot_spaces WHERE snapshot_id = ? ORDER BY display_index, space_index",
		snapshotID,
	)
	if err != nil {
		return fmt.Errorf("querying spaces: %w", err)
	}
	defer srows.Close()

	var totalSpaces int
	displaySeen := map[int]bool{}
	var focused, visibleNoFocus []string
	for srows.Next() {
		var idx, display, hf, iv int
		if err := srows.Scan(&idx, &display, &hf, &iv); err != nil {
			continue
		}
		totalSpaces++
		displaySeen[display] = true
		if hf == 1 {
			focused = append(focused, fmt.Sprintf("display %d space %d", display, idx))
		}
		if hf == 0 && iv == 1 {
			visibleNoFocus = append(visibleNoFocus, fmt.Sprintf("display %d space %d", display, idx))
		}
	}

	fmt.Fprintf(w, "  spaces: %d total across %d displays\n", totalSpaces, len(displaySeen))
	if len(focused) > 0 {
		fmt.Fprintf(w, "  focus:  %s\n", strings.Join(focused, ", "))
	}
	if len(visibleNoFocus) > 0 {
		fmt.Fprintf(w, "  also visible: %s\n", strings.Join(visibleNoFocus, ", "))
	}

	wrows, err := db.Query(
		"SELECT space_index, app, title FROM snapshot_windows WHERE snapshot_id = ? AND is_visible = 1 ORDER BY space_index, app",
		snapshotID,
	)
	if err != nil {
		return fmt.Errorf("querying windows: %w", err)
	}
	defer wrows.Close()

	var windowLines []string
	for wrows.Next() {
		var spaceIdx int
		var app, title string
		if err := wrows.Scan(&spaceIdx, &app, &title); err == nil {
			windowLines = append(windowLines, fmt.Sprintf("    space %2d: %s — %q", spaceIdx, app, title))
		}
	}
	if len(windowLines) > 0 {
		fmt.Fprintln(w, "  visible windows:")
		for _, ln := range windowLines {
			fmt.Fprintln(w, ln)
		}
	}
	return nil
}

// printTmuxInventory groups all tmux panes by session → window and prints
// command, working dir, and resolved opencode session id per pane. nvim
// panes additionally show their open buffer list (if introspection succeeded
// at capture time).
func printTmuxInventory(w io.Writer, db *sql.DB, snapshotID int64) error {
	// preload all nvim buffers for this snapshot, keyed by pane_pid
	buffersByPane, err := loadNvimBuffersForSnapshot(db, snapshotID)
	if err != nil {
		return err
	}

	prows, err := db.Query(
		"SELECT session_name, window_index, window_name, pane_index, current_command, current_path, pane_pid, opencode_session_id FROM snapshot_tmux_panes WHERE snapshot_id = ? ORDER BY session_name, window_index, pane_index",
		snapshotID,
	)
	if err != nil {
		return fmt.Errorf("querying tmux panes: %w", err)
	}
	defer prows.Close()

	type pane struct {
		command, path, opencodeID string
		panePID                   int
	}
	type windowGroup struct {
		index int
		name  string
		panes []pane
	}
	type sessionGroup struct {
		name    string
		windows []windowGroup
	}

	var sessionOrder []string
	sessions := map[string]*sessionGroup{}
	for prows.Next() {
		var sessionName, windowName, command, path string
		var windowIdx, paneIdx, panePID int
		var oid sql.NullString
		if err := prows.Scan(&sessionName, &windowIdx, &windowName, &paneIdx, &command, &path, &panePID, &oid); err != nil {
			continue
		}

		sg, ok := sessions[sessionName]
		if !ok {
			sg = &sessionGroup{name: sessionName}
			sessions[sessionName] = sg
			sessionOrder = append(sessionOrder, sessionName)
		}

		var wg *windowGroup
		for i := range sg.windows {
			if sg.windows[i].index == windowIdx {
				wg = &sg.windows[i]
				break
			}
		}
		if wg == nil {
			sg.windows = append(sg.windows, windowGroup{index: windowIdx, name: windowName})
			wg = &sg.windows[len(sg.windows)-1]
		}

		opencodeID := ""
		if oid.Valid {
			opencodeID = oid.String
		}
		wg.panes = append(wg.panes, pane{command: command, path: path, opencodeID: opencodeID, panePID: panePID})
	}

	if len(sessionOrder) == 0 {
		fmt.Fprintln(w, "  tmux: (no panes)")
		return nil
	}

	fmt.Fprintln(w, "  tmux sessions:")
	for _, sn := range sessionOrder {
		sg := sessions[sn]
		paneTotal := 0
		for _, wg := range sg.windows {
			paneTotal += len(wg.panes)
		}
		fmt.Fprintf(w, "    [%s]  %d window(s), %d pane(s)\n", sg.name, len(sg.windows), paneTotal)
		for _, wg := range sg.windows {
			fmt.Fprintf(w, "      %d: %s\n", wg.index, wg.name)
			for _, p := range wg.panes {
				suffix := ""
				if p.opencodeID != "" {
					suffix = "  " + p.opencodeID
				}
				fmt.Fprintf(w, "        %-12s  %s%s\n", p.command, shortPath(p.path), suffix)
				for _, b := range buffersByPane[p.panePID] {
					marker := " "
					if b.IsCurrent {
						marker = "*"
					}
					mod := ""
					if b.IsModified {
						mod = " [+]"
					}
					fmt.Fprintf(w, "          %s %s%s\n", marker, shortPath(b.Path), mod)
				}
			}
		}
	}
	return nil
}

// loadNvimBuffersForSnapshot returns nvim buffers grouped by pane_pid for a
// given snapshot. current buffer is sorted first so the active file is
// immediately visible in the rendered output.
func loadNvimBuffersForSnapshot(db *sql.DB, snapshotID int64) (map[int][]NvimBuffer, error) {
	rows, err := db.Query(
		"SELECT pane_pid, nvim_pid, buffer_path, is_current, is_modified, last_used_ms, cursor_line, line_count, filetype, changedtick, gitsigns_added, gitsigns_changed, gitsigns_removed FROM snapshot_nvim_buffers WHERE snapshot_id = ? ORDER BY pane_pid, is_current DESC, last_used_ms DESC",
		snapshotID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying nvim buffers: %w", err)
	}
	defer rows.Close()

	out := map[int][]NvimBuffer{}
	for rows.Next() {
		var b NvimBuffer
		var isCurrent, isModified int
		var lastUsedMs int64
		if err := rows.Scan(
			&b.PanePID, &b.NvimPID, &b.Path, &isCurrent, &isModified, &lastUsedMs,
			&b.CursorLine, &b.LineCount, &b.Filetype, &b.ChangedTick,
			&b.GitsignsAdded, &b.GitsignsChanged, &b.GitsignsRemoved,
		); err != nil {
			continue
		}
		b.IsCurrent = isCurrent == 1
		b.IsModified = isModified == 1
		b.LastUsed = lastUsedMs / 1000
		out[b.PanePID] = append(out[b.PanePID], b)
	}
	return out, rows.Err()
}

// formatTime renders a UTC timestamp with the user's local time alongside.
func formatTime(t time.Time) string {
	return fmt.Sprintf("%s (%s)", t.UTC().Format(time.RFC3339), t.Local().Format("2006-01-02 15:04 MST"))
}

// humanDuration renders a duration in compact human form (e.g. 7h28m, 45s, 2m30s).
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", h, m)
}

// shortPath replaces the user's home directory prefix with ~ for compact display.
func shortPath(p string) string {
	if p == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

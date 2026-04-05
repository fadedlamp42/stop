// HTTP handlers for querying snapshot history.
//
// /snapshots?from=<ISO>&to=<ISO>&limit=N  — list snapshots in a time range
// /snapshots/latest                        — most recent snapshot with full detail

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// handleSnapshots returns snapshot summaries within a time range.
// query params: from (ISO 8601), to (ISO 8601), limit (int, default 100)
func handleSnapshots(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		limitStr := r.URL.Query().Get("limit")

		limit := 100
		if limitStr != "" {
			if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
				limit = parsed
			}
		}

		if from == "" {
			// default: last hour
			from = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		}
		if to == "" {
			to = time.Now().UTC().Format(time.RFC3339)
		}

		rows, err := db.Query(
			"SELECT id, captured_at FROM snapshots WHERE captured_at BETWEEN ? AND ? ORDER BY captured_at DESC LIMIT ?",
			from, to, limit,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var snapshots []map[string]any
		for rows.Next() {
			var id int64
			var capturedAt string
			if err := rows.Scan(&id, &capturedAt); err != nil {
				continue
			}

			// count child rows for summary
			spaceCount := countRows(db, "snapshot_spaces", id)
			windowCount := countRows(db, "snapshot_windows", id)
			paneCount := countRows(db, "snapshot_tmux_panes", id)

			snapshots = append(snapshots, map[string]any{
				"id":          id,
				"captured_at": capturedAt,
				"spaces":      spaceCount,
				"windows":     windowCount,
				"tmux_panes":  paneCount,
			})
		}

		writeJSON(w, snapshots)
	}
}

// handleLatestSnapshot returns the most recent snapshot with full detail:
// all spaces, windows, and tmux panes including resolved opencode session IDs.
func handleLatestSnapshot(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var snapshotID int64
		var capturedAt string
		err := db.QueryRow(
			"SELECT id, captured_at FROM snapshots ORDER BY id DESC LIMIT 1",
		).Scan(&snapshotID, &capturedAt)
		if err != nil {
			http.Error(w, "no snapshots available", http.StatusNotFound)
			return
		}

		response := buildSnapshotResponse(db, snapshotID, capturedAt)
		writeJSON(w, response)
	}
}

// buildSnapshotResponse assembles a full snapshot with all child data.
func buildSnapshotResponse(db *sql.DB, snapshotID int64, capturedAt string) map[string]any {
	// query spaces
	spaceRows, _ := db.Query(
		"SELECT yabai_id, space_index, display_index, label, has_focus, is_visible FROM snapshot_spaces WHERE snapshot_id = ? ORDER BY display_index, space_index",
		snapshotID,
	)
	var spaces []map[string]any
	if spaceRows != nil {
		defer spaceRows.Close()
		for spaceRows.Next() {
			var yabaiID, spaceIndex, displayIndex, hasFocus, isVisible int
			var label string
			if err := spaceRows.Scan(&yabaiID, &spaceIndex, &displayIndex, &label, &hasFocus, &isVisible); err != nil {
				continue
			}
			spaces = append(spaces, map[string]any{
				"yabai_id":      yabaiID,
				"space_index":   spaceIndex,
				"display_index": displayIndex,
				"label":         label,
				"has_focus":     hasFocus == 1,
				"is_visible":    isVisible == 1,
			})
		}
	}

	// query windows
	windowRows, _ := db.Query(
		"SELECT yabai_id, pid, app, title, space_index, is_visible, is_minimized, is_hidden FROM snapshot_windows WHERE snapshot_id = ? ORDER BY space_index",
		snapshotID,
	)
	var windows []map[string]any
	if windowRows != nil {
		defer windowRows.Close()
		for windowRows.Next() {
			var yabaiID, pid, spaceIndex, isVisible, isMinimized, isHidden int
			var app, title string
			if err := windowRows.Scan(&yabaiID, &pid, &app, &title, &spaceIndex, &isVisible, &isMinimized, &isHidden); err != nil {
				continue
			}
			windows = append(windows, map[string]any{
				"yabai_id":     yabaiID,
				"pid":          pid,
				"app":          app,
				"title":        title,
				"space_index":  spaceIndex,
				"is_visible":   isVisible == 1,
				"is_minimized": isMinimized == 1,
				"is_hidden":    isHidden == 1,
			})
		}
	}

	// query tmux panes
	paneRows, _ := db.Query(
		"SELECT session_name, window_index, window_name, pane_index, current_command, current_path, pane_pid, last_activity_ms, history_size, opencode_session_id FROM snapshot_tmux_panes WHERE snapshot_id = ? ORDER BY session_name, window_index, pane_index",
		snapshotID,
	)
	var tmuxPanes []map[string]any
	if paneRows != nil {
		defer paneRows.Close()
		for paneRows.Next() {
			var windowIndex, paneIndex, panePID, historySize int
			var lastActivityMS int64
			var sessionName, windowName, currentCommand, currentPath string
			var opencodeSessionID sql.NullString
			if err := paneRows.Scan(&sessionName, &windowIndex, &windowName, &paneIndex, &currentCommand, &currentPath, &panePID, &lastActivityMS, &historySize, &opencodeSessionID); err != nil {
				continue
			}
			pane := map[string]any{
				"session_name":     sessionName,
				"window_index":     windowIndex,
				"window_name":      windowName,
				"pane_index":       paneIndex,
				"current_command":  currentCommand,
				"current_path":     currentPath,
				"pane_pid":         panePID,
				"last_activity_ms": lastActivityMS,
				"history_size":     historySize,
			}
			if opencodeSessionID.Valid {
				pane["opencode_session_id"] = opencodeSessionID.String
			}
			tmuxPanes = append(tmuxPanes, pane)
		}
	}

	return map[string]any{
		"id":          snapshotID,
		"captured_at": capturedAt,
		"spaces":      spaces,
		"windows":     windows,
		"tmux_panes":  tmuxPanes,
	}
}

func countRows(db *sql.DB, table string, snapshotID int64) int {
	var count int
	// table name is hardcoded by callers, not user input
	db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE snapshot_id = ?", snapshotID).Scan(&count)
	return count
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(data)
}

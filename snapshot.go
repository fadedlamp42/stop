// snapshot persistence: periodic capture of system state to SQLite.
//
// every 30 seconds, captures yabai spaces/windows and tmux panes into
// snapshots.db. for panes running opencode, resolves the active session
// ID by querying opencode's own database. this gives a queryable history
// of workspace state — which sessions were open, where, and when.
//
// the db lives in the repo root following llm-usage conventions.
// schema uses IF NOT EXISTS so it's safe to run on every startup.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// schema for the snapshot database.
// one snapshot row per capture, with child rows for spaces, windows, and tmux panes.
const snapshotSchema = `
CREATE TABLE IF NOT EXISTS snapshots (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	captured_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_snapshots_captured_at
	ON snapshots (captured_at);

CREATE TABLE IF NOT EXISTS snapshot_spaces (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
	yabai_id INTEGER NOT NULL,
	space_index INTEGER NOT NULL,
	display_index INTEGER NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	has_focus INTEGER NOT NULL,
	is_visible INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_snapshot_spaces_snapshot_id
	ON snapshot_spaces (snapshot_id);

CREATE TABLE IF NOT EXISTS snapshot_windows (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
	yabai_id INTEGER NOT NULL,
	pid INTEGER NOT NULL,
	app TEXT NOT NULL,
	title TEXT NOT NULL,
	space_index INTEGER NOT NULL,
	is_visible INTEGER NOT NULL,
	is_minimized INTEGER NOT NULL,
	is_hidden INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_snapshot_windows_snapshot_id
	ON snapshot_windows (snapshot_id);

CREATE TABLE IF NOT EXISTS snapshot_tmux_panes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
	session_name TEXT NOT NULL,
	window_index INTEGER NOT NULL,
	window_name TEXT NOT NULL,
	pane_index INTEGER NOT NULL,
	current_command TEXT NOT NULL,
	current_path TEXT NOT NULL DEFAULT '',
	pane_pid INTEGER NOT NULL DEFAULT 0,
	last_activity_ms INTEGER NOT NULL,
	history_size INTEGER NOT NULL,
	opencode_session_id TEXT
);

CREATE INDEX IF NOT EXISTS idx_snapshot_tmux_panes_snapshot_id
	ON snapshot_tmux_panes (snapshot_id);

CREATE INDEX IF NOT EXISTS idx_snapshot_tmux_panes_opencode_session
	ON snapshot_tmux_panes (opencode_session_id);
`

// snapshotDBPath returns the path to snapshots.db in the executable's directory,
// falling back to the current working directory.
func snapshotDBPath() string {
	// try executable directory first (matches llm-usage convention of db in repo root)
	if execPath, err := os.Executable(); err == nil {
		dir := filepath.Dir(execPath)
		return filepath.Join(dir, "snapshots.db")
	}
	return "snapshots.db"
}

// openSnapshotDB opens (or creates) the snapshot database and ensures schema exists.
func openSnapshotDB() (*sql.DB, error) {
	dbPath := snapshotDBPath()
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening snapshot db at %s: %w", dbPath, err)
	}

	// apply schema (IF NOT EXISTS makes this idempotent)
	if _, err := db.Exec(snapshotSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying snapshot schema: %w", err)
	}

	log.Printf("snapshot db ready at %s", dbPath)
	return db, nil
}

// recordSnapshot captures the current system state into the database.
// fetches all data fresh, resolves opencode sessions via otop-serve, and writes atomically.
func recordSnapshot(db *sql.DB) error {
	result := fetchAll()
	if result.err != nil {
		return fmt.Errorf("fetching system state: %w", result.err)
	}

	// build pane_pid → opencode session ID map.
	// otop reports the opencode process PID, but tmux pane_pid is the shell
	// that spawned it. walk the process tree to connect them.
	panePIDToSession := resolveSessionsByAncestry(
		fetchOtopSessions(), result.tmuxPanes, result.processTree)

	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// insert snapshot row
	snapshotResult, err := tx.Exec("INSERT INTO snapshots (captured_at) VALUES (?)", now)
	if err != nil {
		return fmt.Errorf("inserting snapshot: %w", err)
	}
	snapshotID, _ := snapshotResult.LastInsertId()

	// insert spaces
	for _, space := range result.spaces {
		_, err := tx.Exec(
			"INSERT INTO snapshot_spaces (snapshot_id, yabai_id, space_index, display_index, label, has_focus, is_visible) VALUES (?, ?, ?, ?, ?, ?, ?)",
			snapshotID, space.ID, space.Index, space.Display, space.Label,
			boolToInt(space.HasFocus), boolToInt(space.IsVisible),
		)
		if err != nil {
			return fmt.Errorf("inserting space: %w", err)
		}
	}

	// insert windows
	for _, window := range result.windows {
		_, err := tx.Exec(
			"INSERT INTO snapshot_windows (snapshot_id, yabai_id, pid, app, title, space_index, is_visible, is_minimized, is_hidden) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			snapshotID, window.ID, window.PID, window.App, window.Title, window.Space,
			boolToInt(window.IsVisible), boolToInt(window.IsMinimized), boolToInt(window.IsHidden),
		)
		if err != nil {
			return fmt.Errorf("inserting window: %w", err)
		}
	}

	// insert tmux panes, resolving opencode sessions via otop PID mapping
	for _, pane := range result.tmuxPanes {
		var opencodeSessionID *string
		if isProductive(pane.CurrentCommand) && pane.PanePID > 0 {
			if sessionID, ok := panePIDToSession[pane.PanePID]; ok {
				opencodeSessionID = &sessionID
			}
		}

		_, err := tx.Exec(
			"INSERT INTO snapshot_tmux_panes (snapshot_id, session_name, window_index, window_name, pane_index, current_command, current_path, pane_pid, last_activity_ms, history_size, opencode_session_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			snapshotID, pane.SessionName, pane.WindowIndex, pane.WindowName,
			pane.PaneIndex, pane.CurrentCommand, pane.CurrentPath, pane.PanePID,
			pane.LastActivity.UnixMilli(), pane.HistorySize, opencodeSessionID,
		)
		if err != nil {
			return fmt.Errorf("inserting tmux pane: %w", err)
		}
	}

	return tx.Commit()
}

// resolveSessionsByAncestry maps tmux pane PIDs to opencode session IDs
// by walking the process tree. otop reports the opencode process PID, but
// tmux pane_pid is the shell that spawned it (parent or grandparent).
// for each otop session PID, walks up the ppid chain to find which tmux
// pane it belongs to.
func resolveSessionsByAncestry(
	otopSessions map[int]string,
	panes []TmuxPane,
	processTree map[int]int,
) map[int]string {
	result := make(map[int]string)

	if len(otopSessions) == 0 || len(panes) == 0 {
		return result
	}

	// build set of tmux pane PIDs for fast lookup
	panePIDs := make(map[int]bool, len(panes))
	for _, p := range panes {
		if p.PanePID > 0 {
			panePIDs[p.PanePID] = true
		}
	}

	// for each otop session, walk up the process tree to find the pane
	for opencodePID, sessionID := range otopSessions {
		pid := opencodePID
		for depth := 0; depth < 20; depth++ {
			if panePIDs[pid] {
				result[pid] = sessionID
				break
			}
			ppid, ok := processTree[pid]
			if !ok || ppid <= 1 {
				break
			}
			pid = ppid
		}
	}

	return result
}

// fetchOtopSessions queries otop-serve for running opencode sessions and
// returns a PID → session_id map. otop-serve already tracks which opencode
// process belongs to which session, so we just need to correlate by PID.
// returns empty map if otop-serve is unavailable.
func fetchOtopSessions() map[int]string {
	otopURL := os.Getenv("OTOP_URL")
	if otopURL == "" {
		otopURL = "http://localhost:8390"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", otopURL+"/sessions", nil)
	if err != nil {
		return nil
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var data struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			PID       int    `json:"pid"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}

	result := make(map[int]string, len(data.Sessions))
	for _, s := range data.Sessions {
		if s.PID > 0 && s.SessionID != "" {
			result[s.PID] = s.SessionID
		}
	}
	return result
}

// startSnapshotLoop runs the snapshot capture in a goroutine, writing to the
// database every interval. blocks until ctx is cancelled.
func startSnapshotLoop(ctx context.Context, db *sql.DB, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// capture one immediately on startup
	if err := recordSnapshot(db); err != nil {
		log.Printf("snapshot error: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := recordSnapshot(db); err != nil {
				log.Printf("snapshot error: %v", err)
			}
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

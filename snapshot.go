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
	"strings"
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

CREATE TABLE IF NOT EXISTS snapshot_nvim_buffers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
	pane_pid INTEGER NOT NULL,
	nvim_pid INTEGER NOT NULL DEFAULT 0,
	buffer_path TEXT NOT NULL,
	is_current INTEGER NOT NULL,
	is_modified INTEGER NOT NULL,
	last_used_ms INTEGER NOT NULL DEFAULT 0,
	cursor_line INTEGER NOT NULL DEFAULT 0,
	line_count INTEGER NOT NULL DEFAULT 0,
	filetype TEXT NOT NULL DEFAULT '',
	changedtick INTEGER NOT NULL DEFAULT 0,
	gitsigns_added INTEGER NOT NULL DEFAULT 0,
	gitsigns_changed INTEGER NOT NULL DEFAULT 0,
	gitsigns_removed INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_snapshot_nvim_buffers_snapshot_id
	ON snapshot_nvim_buffers (snapshot_id);

CREATE INDEX IF NOT EXISTS idx_snapshot_nvim_buffers_pane_pid
	ON snapshot_nvim_buffers (snapshot_id, pane_pid);

CREATE TABLE IF NOT EXISTS snapshot_nvim_windows (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
	pane_pid INTEGER NOT NULL,
	nvim_pid INTEGER NOT NULL,
	win_id INTEGER NOT NULL,
	tab_nr INTEGER NOT NULL,
	win_nr INTEGER NOT NULL,
	bufnr INTEGER NOT NULL,
	width INTEGER NOT NULL,
	height INTEGER NOT NULL,
	topline INTEGER NOT NULL,
	botline INTEGER NOT NULL,
	cursor_line INTEGER NOT NULL,
	cursor_col INTEGER NOT NULL,
	is_quickfix INTEGER NOT NULL,
	is_loclist INTEGER NOT NULL,
	is_terminal INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_snapshot_nvim_windows_snapshot_id
	ON snapshot_nvim_windows (snapshot_id);

CREATE INDEX IF NOT EXISTS idx_snapshot_nvim_windows_pane_pid
	ON snapshot_nvim_windows (snapshot_id, pane_pid);

CREATE TABLE IF NOT EXISTS snapshot_nvim_sessions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
	pane_pid INTEGER NOT NULL,
	nvim_pid INTEGER NOT NULL,
	cwd TEXT NOT NULL DEFAULT '',
	servername TEXT NOT NULL DEFAULT '',
	nvim_version TEXT NOT NULL DEFAULT '',
	argv_json TEXT NOT NULL DEFAULT '[]',
	cmd_history_json TEXT NOT NULL DEFAULT '[]',
	search_register TEXT NOT NULL DEFAULT '',
	jumplist_json TEXT NOT NULL DEFAULT '[]',
	quickfix_size INTEGER NOT NULL DEFAULT 0,
	loclist_size INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_snapshot_nvim_sessions_snapshot_id
	ON snapshot_nvim_sessions (snapshot_id);

CREATE INDEX IF NOT EXISTS idx_snapshot_nvim_sessions_pane_pid
	ON snapshot_nvim_sessions (snapshot_id, pane_pid);
`

// nvimBufferUpgrades lists columns added to snapshot_nvim_buffers after
// its initial release. fresh databases get them via the CREATE TABLE
// above; existing dbs need ALTER TABLE ADD COLUMN. SQLite has no
// "IF NOT EXISTS" for ADD COLUMN so we tolerate the duplicate-column
// error per call.
var nvimBufferUpgrades = []struct{ name, decl string }{
	{"nvim_pid", "INTEGER NOT NULL DEFAULT 0"},
	{"cursor_line", "INTEGER NOT NULL DEFAULT 0"},
	{"line_count", "INTEGER NOT NULL DEFAULT 0"},
	{"filetype", "TEXT NOT NULL DEFAULT ''"},
	{"changedtick", "INTEGER NOT NULL DEFAULT 0"},
	{"gitsigns_added", "INTEGER NOT NULL DEFAULT 0"},
	{"gitsigns_changed", "INTEGER NOT NULL DEFAULT 0"},
	{"gitsigns_removed", "INTEGER NOT NULL DEFAULT 0"},
}

// migrateSnapshotDB applies additive column upgrades to tables that
// shipped earlier without them. each ALTER is independent and idempotent
// via duplicate-column tolerance.
func migrateSnapshotDB(db *sql.DB) error {
	for _, c := range nvimBufferUpgrades {
		stmt := fmt.Sprintf("ALTER TABLE snapshot_nvim_buffers ADD COLUMN %s %s", c.name, c.decl)
		if _, err := db.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("migrating snapshot_nvim_buffers.%s: %w", c.name, err)
		}
	}
	return nil
}

// openSnapshotDB opens (or creates) the snapshot database and ensures schema exists.
// path comes from snapshotDBPath in config.go so the install location doesn't matter.
func openSnapshotDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", snapshotDBPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening snapshot db at %s: %w", snapshotDBPath, err)
	}

	// apply schema (IF NOT EXISTS makes this idempotent)
	if _, err := db.Exec(snapshotSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying snapshot schema: %w", err)
	}

	// run additive column migrations for tables that shipped earlier.
	if err := migrateSnapshotDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating snapshot db: %w", err)
	}

	log.Printf("snapshot db ready at %s", snapshotDBPath)
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

	// nvim state (buffers + windows + sessions) is already collected as
	// part of fetchAll's second phase.
	nvimBuffersByPane := result.nvimBuffers
	nvimWindows := result.nvimWindows
	nvimSessions := result.nvimSessions

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

	// insert nvim buffers per pane (only panes whose nvim we successfully reached)
	for panePID, buffers := range nvimBuffersByPane {
		for _, b := range buffers {
			_, err := tx.Exec(
				"INSERT INTO snapshot_nvim_buffers (snapshot_id, pane_pid, nvim_pid, buffer_path, is_current, is_modified, last_used_ms, cursor_line, line_count, filetype, changedtick, gitsigns_added, gitsigns_changed, gitsigns_removed) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				snapshotID, panePID, b.NvimPID, b.Path,
				boolToInt(b.IsCurrent), boolToInt(b.IsModified), b.LastUsed*1000,
				b.CursorLine, b.LineCount, b.Filetype, b.ChangedTick,
				b.GitsignsAdded, b.GitsignsChanged, b.GitsignsRemoved,
			)
			if err != nil {
				return fmt.Errorf("inserting nvim buffer: %w", err)
			}
		}
	}

	// insert nvim windows (one row per visible split across all reached nvims)
	for _, w := range nvimWindows {
		_, err := tx.Exec(
			"INSERT INTO snapshot_nvim_windows (snapshot_id, pane_pid, nvim_pid, win_id, tab_nr, win_nr, bufnr, width, height, topline, botline, cursor_line, cursor_col, is_quickfix, is_loclist, is_terminal) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			snapshotID, w.PanePID, w.NvimPID, w.WinID, w.TabNr, w.WinNr, w.BufNr,
			w.Width, w.Height, w.TopLine, w.BotLine, w.CursorLine, w.CursorCol,
			boolToInt(w.IsQuickfix), boolToInt(w.IsLoclist), boolToInt(w.IsTerminal),
		)
		if err != nil {
			return fmt.Errorf("inserting nvim window: %w", err)
		}
	}

	// insert nvim sessions (one row per reached nvim instance). list-shaped
	// fields are persisted as JSON so they round-trip without a schema change
	// when their semantics evolve.
	for _, s := range nvimSessions {
		argvJSON, _ := json.Marshal(s.Argv)
		cmdJSON, _ := json.Marshal(s.CmdHistory)
		jumpJSON, _ := json.Marshal(s.Jumplist)
		_, err := tx.Exec(
			"INSERT INTO snapshot_nvim_sessions (snapshot_id, pane_pid, nvim_pid, cwd, servername, nvim_version, argv_json, cmd_history_json, search_register, jumplist_json, quickfix_size, loclist_size) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			snapshotID, s.PanePID, s.NvimPID, s.Cwd, s.Servername, s.NvimVersion,
			string(argvJSON), string(cmdJSON), s.Search, string(jumpJSON),
			s.QuickfixSize, s.LoclistSize,
		)
		if err != nil {
			return fmt.Errorf("inserting nvim session: %w", err)
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

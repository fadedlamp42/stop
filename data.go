// data layer: yabai and tmux subprocess queries.
//
// all external data comes through here. queries run with context timeouts
// to avoid hanging if yabai or tmux are unresponsive. the fetchAll function
// runs all three queries (spaces, windows, tmux) concurrently via goroutines
// so total latency is max(query times) instead of sum.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// (queryNowPlaying removed: PlayingMeta carries artist/title now, so the
// `playing meta` call replaces `playing` + `playing position` entirely.)

// PlayingMeta captures a single sample of the player state. SampledAt is
// set at the time we received the data so consumers can interpolate the
// position forward between samples using wall-clock time, instead of
// paying the script's ~50ms latency on every render.
//
// State is "playing" / "paused" / "" (unknown / nothing). Position and
// Duration are seconds (floats); for a paused track Position stays fixed,
// for a playing track we interpolate Position += (now - SampledAt). when
// Duration <= 0 the consumer can't compute a fraction and should treat
// the play head as unknown.
//
// Artist and Title come from the same script call, so one osascript
// dispatch covers everything the renderer + lyrics fetcher need.
type PlayingMeta struct {
	State     string
	Position  float64
	Duration  float64
	Artist    string
	Title     string
	SampledAt time.Time
}

// queryPlayingMeta runs ~/scripts/playing meta and parses the
// tab-separated "state\tposition\tduration\tartist\ttitle" output.
// returns a zero meta (with empty State) when nothing is playing or the
// script is unavailable. SampledAt is stamped right after the read so
// interpolation tracks real wall-clock elapsed time from the read point,
// not the request point.
func queryPlayingMeta() PlayingMeta {
	out, ok := runPlaying("meta")
	if !ok {
		return PlayingMeta{}
	}
	line := strings.TrimRight(string(out), "\n\r")
	if line == "" {
		return PlayingMeta{}
	}
	parts := strings.Split(line, "\t")
	if len(parts) < 3 {
		return PlayingMeta{}
	}
	pos, errP := strconv.ParseFloat(parts[1], 64)
	dur, errD := strconv.ParseFloat(parts[2], 64)
	if errP != nil || errD != nil {
		return PlayingMeta{}
	}
	meta := PlayingMeta{
		State:     parts[0],
		Position:  pos,
		Duration:  dur,
		SampledAt: time.Now(),
	}
	if len(parts) >= 5 {
		meta.Artist = parts[3]
		meta.Title = parts[4]
	}
	return meta
}

// songDuration converts the float-seconds Duration field into a
// time.Duration. zero when unknown — that signals "skip duration-based
// match" to the lyrics fetcher.
func (m PlayingMeta) songDuration() time.Duration {
	if m.Duration <= 0 {
		return 0
	}
	return time.Duration(m.Duration * float64(time.Second))
}

// DisplayString renders the meta for the UI's now-playing line. matches
// the legacy /scripts/playing output: empty when nothing, "PAUSED" when
// paused, "ARTIST - TITLE" when playing.
func (m PlayingMeta) DisplayString() string {
	if m.State == "" {
		return ""
	}
	if m.State == "paused" {
		return "PAUSED"
	}
	if m.Artist == "" && m.Title == "" {
		return ""
	}
	return m.Artist + " - " + m.Title
}

// CurrentFraction interpolates the fraction 0.0-1.0 of the song's elapsed
// position at wall-clock time `now`. for a playing track it advances the
// last sampled position by (now - SampledAt); for paused it holds steady.
// returns -1 when the meta is empty or duration is unknown, which the
// renderer treats as "no play head" and suppresses the active-line marker.
func (m PlayingMeta) CurrentFraction(now time.Time) float64 {
	if m.Duration <= 0 || m.State == "" {
		return -1
	}
	pos := m.Position
	if m.State == "playing" {
		pos += now.Sub(m.SampledAt).Seconds()
	}
	if pos < 0 {
		pos = 0
	}
	frac := pos / m.Duration
	if frac > 1 {
		frac = 1
	}
	return frac
}

// runPlaying invokes ~/scripts/playing with the given args. centralized so
// path resolution and timeout policy live in one place. returns (output,
// ok) where ok=false means the script was missing or failed.
func runPlaying(args ...string) ([]byte, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false
	}
	script := filepath.Join(home, "scripts", "playing")
	if _, err := os.Stat(script); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, script, args...).Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

// terminal emulator app names as reported by macOS / yabai
var terminalApps = map[string]bool{
	"kitty":     true,
	"iTerm2":    true,
	"Terminal":  true,
	"Alacritty": true,
	"WezTerm":   true,
	"Hyper":     true,
	"Rio":       true,
	"Tabby":     true,
}

func isTerminal(app string) bool {
	return terminalApps[app]
}

// browser apps whose window titles contain useful page info
var browserApps = map[string]bool{
	"Firefox":        true,
	"Google Chrome":  true,
	"Safari":         true,
	"Arc":            true,
	"Brave Browser":  true,
	"Microsoft Edge": true,
	"Chromium":       true,
}

func isBrowser(app string) bool {
	return browserApps[app]
}

// cleanBrowserTitle strips the app name suffix browsers append to window titles.
// "GitHub — Mozilla Firefox" → "GitHub"
// "How to X - Stack Overflow - Google Chrome" → "How to X - Stack Overflow"
func cleanBrowserTitle(title string) string {
	// try em-dash first (firefox), then regular dash (chrome, safari, etc.)
	for _, sep := range []string{" — ", " - "} {
		if idx := strings.LastIndex(title, sep); idx > 0 {
			return title[:idx]
		}
	}
	return title
}

// -- yabai types --
// fields match yabai's JSON output (hyphenated keys)

// Space represents a macOS space/desktop as reported by yabai
type Space struct {
	ID        int    `json:"id"`
	Index     int    `json:"index"`
	Label     string `json:"label"`
	Display   int    `json:"display"`
	Windows   []int  `json:"windows"`
	HasFocus  bool   `json:"has-focus"`
	IsVisible bool   `json:"is-visible"`
}

// Window represents an application window as reported by yabai
type Window struct {
	ID          int    `json:"id"`
	PID         int    `json:"pid"`
	App         string `json:"app"`
	Title       string `json:"title"`
	Space       int    `json:"space"`
	IsVisible   bool   `json:"is-visible"`
	IsMinimized bool   `json:"is-minimized"`
	IsHidden    bool   `json:"is-hidden"`
}

// TmuxPane holds per-pane data from tmux including staleness and buffer info
type TmuxPane struct {
	SessionName    string
	WindowIndex    int
	WindowName     string
	PaneIndex      int
	CurrentCommand string
	CurrentPath    string // working directory of the pane's active process
	PanePID        int    // PID of the pane's active process
	LastActivity   time.Time
	HistorySize    int // lines in scroll buffer
}

// -- queries --

func queryYabai(domain string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "yabai", "-m", "query", "--"+domain).Output()
}

func querySpaces() ([]Space, error) {
	data, err := queryYabai("spaces")
	if err != nil {
		return nil, err
	}
	var spaces []Space
	return spaces, json.Unmarshal(data, &spaces)
}

func queryWindows() ([]Window, error) {
	data, err := queryYabai("windows")
	if err != nil {
		return nil, err
	}
	var windows []Window
	return windows, json.Unmarshal(data, &windows)
}

// queryTmuxPanes fetches per-pane data from all tmux sessions.
// returns nil if tmux is not running or has no sessions.
func queryTmuxPanes() []TmuxPane {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F",
		"#{session_name}\t#{window_index}\t#{window_name}\t#{pane_index}\t#{pane_current_command}\t#{window_activity}\t#{history_size}\t#{pane_current_path}\t#{pane_pid}").Output()
	if err != nil {
		return nil
	}
	var panes []TmuxPane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 9 {
			continue
		}
		var windowIndex, paneIndex, historySize, panePID int
		var activityEpoch int64
		fmt.Sscanf(parts[1], "%d", &windowIndex)
		fmt.Sscanf(parts[3], "%d", &paneIndex)
		fmt.Sscanf(parts[5], "%d", &activityEpoch)
		fmt.Sscanf(parts[6], "%d", &historySize)
		fmt.Sscanf(parts[8], "%d", &panePID)
		panes = append(panes, TmuxPane{
			SessionName:    parts[0],
			WindowIndex:    windowIndex,
			WindowName:     parts[2],
			PaneIndex:      paneIndex,
			CurrentCommand: parts[4],
			CurrentPath:    parts[7],
			PanePID:        panePID,
			LastActivity:   time.Unix(activityEpoch, 0),
			HistorySize:    historySize,
		})
	}
	return panes
}

// TmuxClient maps a tmux client process to its session.
// used to correlate tmux sessions with terminal windows via process tree.
type TmuxClient struct {
	PID         int
	SessionName string
}

// queryTmuxClients fetches the PID and session name for each attached tmux client.
// returns nil if tmux is not running or has no attached clients.
func queryTmuxClients() []TmuxClient {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-clients", "-F",
		"#{client_pid}\t#{session_name}").Output()
	if err != nil {
		return nil
	}
	var clients []TmuxClient
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		var pid int
		fmt.Sscanf(parts[0], "%d", &pid)
		clients = append(clients, TmuxClient{PID: pid, SessionName: parts[1]})
	}
	return clients
}

// queryProcessTree returns a pid → ppid map and pid → comm map for all
// running processes. used to walk from tmux client PIDs up to terminal
// emulator PIDs, and to detect productive process descendants.
func queryProcessTree() (map[int]int, map[int]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid,ppid,comm").Output()
	if err != nil {
		return nil, nil
	}
	tree := make(map[int]int)
	comm := make(map[int]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "PID") {
			continue
		}
		var pid, ppid int
		var cmd string
		if _, err := fmt.Sscanf(line, "%d %d %s", &pid, &ppid, &cmd); err == nil {
			tree[pid] = ppid
			comm[pid] = cmd
		}
	}
	return tree, comm
}

// focusSpace tells yabai to switch focus to a specific space index
func focusSpace(index int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "yabai", "-m", "space", "--focus", fmt.Sprintf("%d", index)).Run()
}

// resolveProductivePanePIDs walks up from every productive process in the
// system to find which tmux pane PIDs contain a productive descendant.
// productive is defined by config.go's productiveProcesses map.
// handles any nesting depth — a pane PID is marked productive if any
// descendant process (child, grandchild, etc.) has a productive command.
func resolveProductivePanePIDs(
	panes []TmuxPane,
	processTree map[int]int,
	processComm map[int]string,
) map[int]bool {
	// build set of pane PIDs for fast lookup while walking up
	panePIDs := make(map[int]bool, len(panes))
	for _, p := range panes {
		if p.PanePID > 0 {
			panePIDs[p.PanePID] = true
		}
	}

	// collect all pids whose command basename is productive
	productivePIDs := make(map[int]bool)
	for pid, cmd := range processComm {
		if isProductive(filepath.Base(cmd)) {
			productivePIDs[pid] = true
		}
	}

	// walk up from each productive pid until we hit a pane pid
	result := make(map[int]bool)
	for pid := range productivePIDs {
		cur := pid
		for depth := 0; depth < 50; depth++ {
			if panePIDs[cur] {
				result[cur] = true
				break
			}
			ppid, ok := processTree[cur]
			if !ok || ppid <= 1 {
				break
			}
			cur = ppid
		}
	}
	return result
}

// -- concurrent fetch --

// fetchResult holds the combined result of all concurrent queries
type fetchResult struct {
	spaces               []Space
	windows              []Window
	tmuxPanes            []TmuxPane
	tmuxClients          []TmuxClient
	processTree          map[int]int
	processComm          map[int]string
	productivePanePIDs   map[int]bool
	nvimBuffers          map[int][]NvimBuffer // pane_pid → buffers (only for nvim panes)
	nvimWindows          []NvimWindow         // flat list, each tagged with PanePID/NvimPID
	nvimSessions         []NvimSession        // one per reachable nvim instance
	playingMeta          PlayingMeta          // single sample of player state used for both UI + interpolation
	err                  error
}

// fetchAll queries yabai (spaces + windows) and tmux concurrently.
// spaces query is required; windows and tmux are best-effort.
func fetchAll() fetchResult {
	var (
		spaces              []Space
		windows             []Window
		tmuxPanes           []TmuxPane
		tmuxClients         []TmuxClient
		processTree         map[int]int
		processComm         map[int]string
		productivePanePIDs  map[int]bool
		spaceErr            error
		mu                  sync.Mutex
		wg                  sync.WaitGroup
	)

	var playingMeta PlayingMeta

	wg.Add(6)

	go func() {
		defer wg.Done()
		m := queryPlayingMeta()
		mu.Lock()
		playingMeta = m
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		s, err := querySpaces()
		mu.Lock()
		spaces, spaceErr = s, err
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		w, _ := queryWindows()
		mu.Lock()
		windows = w
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		t := queryTmuxPanes()
		mu.Lock()
		tmuxPanes = t
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		c := queryTmuxClients()
		mu.Lock()
		tmuxClients = c
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		t, c := queryProcessTree()
		mu.Lock()
		processTree = t
		processComm = c
		mu.Unlock()
	}()

	wg.Wait()

	// spaces are required — can't render anything without them
	if spaceErr != nil {
		return fetchResult{err: spaceErr}
	}

	// nvim introspection runs after the first phase since it needs both
	// tmuxPanes (to filter) and processTree (to map nvim → pane). queries
	// inside collectNvimState are themselves parallel per nvim instance
	// and one round-trip pulls buffers + windows + session state.
	capture := collectNvimState(tmuxPanes, processTree)

	// compute which pane PIDs have a productive process somewhere in
	// their descendant tree. this handles wrapper scripts and any
	// nesting depth — the fast check on pane_current_command alone
	// misses panes where the productive binary is a grandchild.
	if processTree != nil && processComm != nil {
		productivePanePIDs = resolveProductivePanePIDs(tmuxPanes, processTree, processComm)
	}

	return fetchResult{
		spaces:             spaces,
		windows:            windows,
		tmuxPanes:          tmuxPanes,
		tmuxClients:        tmuxClients,
		processTree:        processTree,
		processComm:        processComm,
		productivePanePIDs: productivePanePIDs,
		nvimBuffers:        capture.PerPaneBuffers,
		nvimWindows:        capture.Windows,
		nvimSessions:       capture.Sessions,
		playingMeta:        playingMeta,
	}
}

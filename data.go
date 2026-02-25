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
	"os/exec"
	"strings"
	"sync"
	"time"
)

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
		"#{session_name}\t#{window_index}\t#{window_name}\t#{pane_index}\t#{pane_current_command}\t#{window_activity}\t#{history_size}").Output()
	if err != nil {
		return nil
	}
	var panes []TmuxPane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		var windowIndex, paneIndex, historySize int
		var activityEpoch int64
		fmt.Sscanf(parts[1], "%d", &windowIndex)
		fmt.Sscanf(parts[3], "%d", &paneIndex)
		fmt.Sscanf(parts[5], "%d", &activityEpoch)
		fmt.Sscanf(parts[6], "%d", &historySize)
		panes = append(panes, TmuxPane{
			SessionName:    parts[0],
			WindowIndex:    windowIndex,
			WindowName:     parts[2],
			PaneIndex:      paneIndex,
			CurrentCommand: parts[4],
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

// queryProcessTree returns a pid → ppid map for all running processes.
// used to walk from tmux client PIDs up to terminal emulator PIDs.
func queryProcessTree() map[int]int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-eo", "pid,ppid").Output()
	if err != nil {
		return nil
	}
	tree := make(map[int]int)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "PID") {
			continue
		}
		var pid, ppid int
		if _, err := fmt.Sscanf(line, "%d %d", &pid, &ppid); err == nil {
			tree[pid] = ppid
		}
	}
	return tree
}

// focusSpace tells yabai to switch focus to a specific space index
func focusSpace(index int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "yabai", "-m", "space", "--focus", fmt.Sprintf("%d", index)).Run()
}

// -- concurrent fetch --

// fetchResult holds the combined result of all concurrent queries
type fetchResult struct {
	spaces      []Space
	windows     []Window
	tmuxPanes   []TmuxPane
	tmuxClients []TmuxClient
	processTree map[int]int
	err         error
}

// fetchAll queries yabai (spaces + windows) and tmux concurrently.
// spaces query is required; windows and tmux are best-effort.
func fetchAll() fetchResult {
	var (
		spaces      []Space
		windows     []Window
		tmuxPanes   []TmuxPane
		tmuxClients []TmuxClient
		processTree map[int]int
		spaceErr    error
		mu          sync.Mutex
		wg          sync.WaitGroup
	)

	wg.Add(5)

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
		t := queryProcessTree()
		mu.Lock()
		processTree = t
		mu.Unlock()
	}()

	wg.Wait()

	// spaces are required — can't render anything without them
	if spaceErr != nil {
		return fetchResult{err: spaceErr}
	}
	return fetchResult{
		spaces:      spaces,
		windows:     windows,
		tmuxPanes:   tmuxPanes,
		tmuxClients: tmuxClients,
		processTree: processTree,
	}
}

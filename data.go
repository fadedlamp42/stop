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

// TmuxSession holds a tmux session name and its window count
type TmuxSession struct {
	Name    string
	Windows int
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

func queryTmuxSessions() []TmuxSession {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-sessions", "-F", "#{session_name} #{session_windows}").Output()
	if err != nil {
		return nil
	}
	var sessions []TmuxSession
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		var windowCount int
		fmt.Sscanf(parts[1], "%d", &windowCount)
		sessions = append(sessions, TmuxSession{Name: parts[0], Windows: windowCount})
	}
	return sessions
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
	spaces  []Space
	windows []Window
	tmux    []TmuxSession
	err     error
}

// fetchAll queries yabai (spaces + windows) and tmux concurrently.
// spaces query is required; windows and tmux are best-effort.
func fetchAll() fetchResult {
	var (
		spaces   []Space
		windows  []Window
		tmux     []TmuxSession
		spaceErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)

	wg.Add(3)

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
		t := queryTmuxSessions()
		mu.Lock()
		tmux = t
		mu.Unlock()
	}()

	wg.Wait()

	// spaces are required — can't render anything without them
	if spaceErr != nil {
		return fetchResult{err: spaceErr}
	}
	return fetchResult{spaces: spaces, windows: windows, tmux: tmux}
}

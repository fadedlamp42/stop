// HTTP server mode for feeding data to the Rose companion app.
//
// serves yabai space/window data and tmux pane staleness as JSON
// so the phone can poll it via adb reverse port forwarding.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// serveCommand starts an HTTP server that exposes space/tmux data as JSON.
func serveCommand(port int) {
	http.HandleFunc("/spaces", handleSpaces)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("stop serve on %s\n", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Printf("error: %v\n", err)
	}
}

// handleSpaces returns the full yabai + tmux state as JSON.
func handleSpaces(w http.ResponseWriter, r *http.Request) {
	result := fetchAll()
	if result.err != nil {
		http.Error(w, result.err.Error(), http.StatusInternalServerError)
		return
	}

	nowMS := time.Now().UnixMilli()
	productiveActivity := bestProductiveActivity(result.tmuxPanes)
	groups := buildDisplayGroups(result.spaces, result.windows)

	// serialize displays
	var displays []map[string]any
	for _, dg := range groups {
		var spaces []map[string]any
		for i, row := range dg.spaces {
			var windows []map[string]any
			for _, w := range row.windows {
				windows = append(windows, map[string]any{
					"app":   w.App,
					"title": w.Title,
				})
			}

			// compute freshness for this space from productive sessions
			var freshestActivityMS int64
			for _, w := range row.windows {
				if !isTerminal(w.App) {
					continue
				}
				if activity, ok := productiveActivity[w.Title]; ok {
					actMS := activity.UnixMilli()
					if actMS > freshestActivityMS {
						freshestActivityMS = actMS
					}
				}
			}

			spaces = append(spaces, map[string]any{
				"index":                i + 1,
				"yabai_index":          row.space.Index,
				"label":                row.space.Label,
				"has_focus":            row.space.HasFocus,
				"is_visible":           row.space.IsVisible,
				"windows":              windows,
				"freshest_activity_ms": freshestActivityMS,
			})
		}

		displays = append(displays, map[string]any{
			"index":      dg.index,
			"spaces":     spaces,
			"free_count": dg.freeCount,
			"term_count": dg.termCount,
		})
	}

	// serialize tmux sessions with staleness
	var tmuxSessions []map[string]any
	sessionGroups := groupPanesBySession(result.tmuxPanes)
	for _, sg := range sessionGroups {
		var windows []map[string]any
		for _, wg := range sg.windows {
			var panes []map[string]any
			for _, p := range wg.panes {
				panes = append(panes, map[string]any{
					"command":          p.CurrentCommand,
					"last_activity_ms": p.LastActivity.UnixMilli(),
					"history_size":     p.HistorySize,
					"productive":       isProductive(p.CurrentCommand),
				})
			}
			windows = append(windows, map[string]any{
				"index": wg.index,
				"name":  wg.name,
				"panes": panes,
			})
		}
		tmuxSessions = append(tmuxSessions, map[string]any{
			"name":    sg.name,
			"windows": windows,
		})
	}

	response := map[string]any{
		"timestamp":     nowMS,
		"displays":      displays,
		"tmux_sessions": tmuxSessions,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(response)
}

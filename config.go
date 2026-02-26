// config: tunable lists and patterns.
//
// productive processes determine which tmux panes get staleness coloring.
// non-productive panes (bash, btop, etc.) render dim — it's not meaningful
// that they've been running a long time.

package main

// productiveProcesses are tmux pane commands that represent meaningful
// interactive work. only these get staleness coloring (green → red).
// everything else renders dim regardless of activity.
var productiveProcesses = map[string]bool{
	"opencode": true,
	"claude":   true,
	"codex":    true,
	"crush":    true,
	"gemini":   true,
}

// isProductive checks if a tmux pane command is considered productive work
func isProductive(command string) bool {
	return productiveProcesses[command]
}

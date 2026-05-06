package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	// `stop serve` subcommand — HTTP JSON server for Rose companion app
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		port := fs.Int("port", 8385, "port to listen on")
		fs.IntVar(port, "p", 8385, "port to listen on")
		_ = fs.Parse(os.Args[2:])
		serveCommand(*port)
		return
	}

	// `stop history` subcommand — find restarts/crashes/sleeps in the
	// snapshot timeline and print the last known state before each.
	if len(os.Args) > 1 && os.Args[1] == "history" {
		fs := flag.NewFlagSet("history", flag.ExitOnError)
		since := fs.Duration("since", 7*24*time.Hour, "lookback window (e.g. 24h, 168h)")
		gap := fs.Duration("gap", 5*time.Minute, "minimum wall-clock gap to flag as a discontinuity")
		collapse := fs.Int("collapse", 5, "minimum drop in tmux pane count to flag (0 disables)")
		limit := fs.Int("limit", 1, "max number of discontinuities to print (0 = no limit)")
		_ = fs.Parse(os.Args[2:])
		err := historyCommand(historyOptions{
			since:             time.Now().Add(-*since),
			gapThreshold:      *gap,
			collapseThreshold: *collapse,
			limit:             *limit,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// `stop show <snapshot_id>` — render a single snapshot by id (debug aid).
	if len(os.Args) > 2 && os.Args[1] == "show" {
		var id int64
		fmt.Sscanf(os.Args[2], "%d", &id)
		if id <= 0 {
			fmt.Fprintln(os.Stderr, "usage: stop show <snapshot_id>")
			os.Exit(1)
		}
		db, err := openSnapshotDB()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer db.Close()
		if err := printSnapshotState(os.Stdout, db, id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// default: launch TUI
	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

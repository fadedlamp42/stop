package main

import (
	"flag"
	"fmt"
	"os"

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

	// default: launch TUI
	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

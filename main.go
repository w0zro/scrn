// Command scrn is a terminal UI for working on projects at the command line.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	// The daemon is the same binary. It is started by the client when none is
	// listening, so nothing has to be installed or launched by hand.
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		if err := runDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "scrnd: %v\n", err)
			os.Exit(1)
		}
		return
	}

	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
}

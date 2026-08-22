// Command scrn is a terminal UI for working on projects at the command line.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
}

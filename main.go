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

	// The mouse is asked for so that it can be handed on. A program in the pane
	// that wants clicks and wheel turns cannot ask the real terminal for them —
	// it is talking to scrn's emulator, which has no window — so scrn asks on
	// its behalf and passes on what arrives.
	//
	// Cell motion rather than every movement: it covers clicking, dragging and
	// the wheel, which is what the programs that want a mouse are after, without
	// a message for every pixel the pointer crosses.
	p := tea.NewProgram(newModel(), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
}

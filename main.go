// Command scrn is a terminal UI for working on projects at the command line.
package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
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

	// The navigator's width is drawn from before the first paint, so it is the
	// one piece of config the client applies here rather than on a scan.
	if cfg, err := loadConfig(); err == nil {
		applyNavWidth(cfg.NavWidth)
	}

	// The alternate screen and the mouse are asked for on the view rather
	// than here: in Bubble Tea v2 they are facts about what is being drawn,
	// and the view carries them out with every frame.
	p := tea.NewProgram(newModel())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
}

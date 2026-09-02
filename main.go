// Command scrn is a terminal UI for working on projects at the command line.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// version is stamped by the release build. A build that came another way
// answers from the module system instead, which go install fills with the tag
// and a plain go build leaves as (devel).
var version string

func versionString() string {
	v := version
	if v == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			v = info.Main.Version
		}
	}
	if v == "" {
		v = "unknown"
	}
	return "scrn " + strings.TrimPrefix(v, "v")
}

// usage is the whole of scrn's command line. The detail — keys, config,
// the server's life — belongs to the manual, not here.
const usage = `scrn is a terminal UI for working on projects at the command line.

usage:
  scrn             open the window
  scrn ls          list the held shells: pid, directory, name
  scrn -h, --help  show this
  scrn --version   report the version

files:
  ~/.config/scrn/config.json  configuration
  ~/.local/state/scrn/        the socket of the tmux server holding the shells

environment:
  SCRN_SOCKET  where that server listens, instead of the state directory
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		case "--version", "version":
			fmt.Println(versionString())
			return
		case "ls":
			if err := runLS(os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
				os.Exit(1)
			}
			return
		// Anything else is refused rather than shrugged off: a mistyped
		// argument that silently opened the window would look like it worked.
		default:
			fmt.Fprintf(os.Stderr, "scrn: unknown argument %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}

	// The navigator's width is drawn from before the first paint, and the
	// transcript cap has to stand before the first shell brings the server
	// up, so both are applied here rather than on a scan.
	if cfg, err := loadConfig(); err == nil {
		applyNavWidth(cfg.NavWidth)
		applyScrollback(cfg.Scrollback)
		applyAgentConfig(cfg.Agent, cfg.AgentRuns)
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

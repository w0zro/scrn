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
  scrn             open the window: scrn's tmux server, with the navigator
  scrn ls          list the held shells: pid, directory, name
  scrn -h, --help  show this
  scrn --version   report the version

the chords run these; they are not for typing:
  scrn nav         the navigator, in the home window's left pane
  scrn keys [client] the keys, in a popup over client
  scrn page        the page inside that popup
  scrn home [key]  to the navigator, pressing key there
  scrn shell [dir] a shell in dir, shown beside the navigator
  scrn agent [dir] an agent in dir, shown beside the navigator
  scrn run [dir]   the plan of the place holding dir
  scrn jump        the next agent waiting on you
  scrn next, prev  the next and previous shell

files:
  ~/.config/scrn/config.json  configuration
  ~/.local/state/scrn/        the tmux server's socket and configuration

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
		case "nav":
			runNav()
			return
		case "page":
			runKeys()
			return
		case "home", "shell", "agent", "run", "jump", "next", "prev", "keys":
			arg := ""
			if len(os.Args) > 2 {
				arg = os.Args[2]
			}
			report(runChord(os.Args[1], arg))
			return
		// Anything else is refused rather than shrugged off: a mistyped
		// argument that silently opened the window would look like it worked.
		default:
			fmt.Fprintf(os.Stderr, "scrn: unknown argument %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}

	if cfg, err := loadConfig(); err == nil {
		applyNavWidth(cfg.NavWidth)
		applyScrollback(cfg.Scrollback)
		applyAgentConfig(cfg.Agent, cfg.AgentRuns)
	}
	if err := runLaunch(); err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
}

// runChord carries out one of the chords' commands.
func runChord(word, arg string) error {
	switch word {
	case "home":
		return runHome(arg)
	case "shell":
		return runShellAt(arg, "")
	case "agent":
		return runShellAt(arg, startAgent())
	case "run":
		return runPlanAt(arg)
	case "jump":
		return runJump()
	case "next":
		return runStep(1)
	case "prev":
		return runStep(-1)
	case "keys":
		return showKeys(tmuxCommand, scrnExe(), arg)
	}
	return nil
}

// runNav is the navigator: the program in the home window. The navigator's
// width is drawn from before the first paint and the agent kind before the
// first a, so both are applied here rather than on a scan.
func runNav() {
	if cfg, err := loadConfig(); err == nil {
		applyNavWidth(cfg.NavWidth)
		applyScrollback(cfg.Scrollback)
		applyAgentConfig(cfg.Agent, cfg.AgentRuns)
	}
	p := tea.NewProgram(newModel())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
}

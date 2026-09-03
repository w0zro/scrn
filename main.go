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
  scrn -h, --help  show this; help is the same word bare
  scrn --version   report the version; version, bare, too

the chords run these; they are not for typing:
  scrn nav         the navigator, in the home window's left pane
  scrn keys [c]    the keys, in a popup over the client c
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
	if err := needHome(); err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
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
		// Anything else is refused rather than shrugged off: a mistyped
		// argument that silently opened the window would look like it worked.
		default:
			chord, ok := chords[os.Args[1]]
			if !ok {
				fmt.Fprintf(os.Stderr, "scrn: unknown argument %q\n\n%s", os.Args[1], usage)
				os.Exit(2)
			}
			arg := ""
			if len(os.Args) > 2 {
				arg = os.Args[2]
			}
			// The chord's agent is the config's, the same as the a key's.
			// A config that cannot be read is the navigator's to report,
			// where it is in view; here the defaults stand.
			cfg, _ := loadConfig()
			cfg.apply()
			if !report(chord(arg)) {
				os.Exit(1)
			}
			return
		}
	}

	// A config that cannot be read is said here, before the terminal is
	// tmux's, and the defaults stand: a typo should not keep the window
	// from opening. The navigator says it again, in view.
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
	}
	cfg.apply()
	if err := runLaunch(); err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
}

// needHome refuses to run without a home directory to put the config and
// the socket under, unless the environment has placed both elsewhere.
// Without it the paths would be relative, and a socket made in whatever
// directory scrn was started from would look like it worked.
func needHome() error {
	placed := os.Getenv("XDG_CONFIG_HOME") != "" &&
		(os.Getenv("SCRN_SOCKET") != "" || os.Getenv("XDG_STATE_HOME") != "")
	if placed {
		return nil
	}
	if _, err := os.UserHomeDir(); err != nil {
		return fmt.Errorf("no home directory: %w", err)
	}
	return nil
}

// chords is every word the configuration binds, and what each does with
// the argument the binding passes.
var chords = map[string]func(arg string) error{
	"home":  runHome,
	"shell": func(dir string) error { return runShellAt(dir, "") },
	"agent": func(dir string) error { return runShellAt(dir, startAgent()) },
	"run":   runPlanAt,
	"jump":  func(string) error { return runJump() },
	"next":  func(string) error { return runStep(1) },
	"prev":  func(string) error { return runStep(-1) },
	"keys":  func(client string) error { return showKeys(tmuxCommand, scrnExe(), client) },
}

// runNav is the navigator: the program in the home window. The navigator's
// width is drawn from before the first paint and the agent kind before the
// first a, so both are applied here rather than on a scan. A config that
// cannot be read is reported on the status line by the scan that reads it
// again, so it is not said here, where stderr is the pane.
func runNav() {
	cfg, _ := loadConfig()
	cfg.apply()
	p := tea.NewProgram(newModel())
	final, err := p.Run()
	if m, ok := final.(model); ok && m.server != nil {
		// The control client is hung up rather than left to the pane's
		// end: the shells are the server's and stay.
		m.server.close()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		os.Exit(1)
	}
}

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// The launcher and the chords. `scrn` brings the server up under scrn's
// configuration, makes sure the home window exists with the navigator down
// its left, and hands the terminal to tmux. The chords the configuration
// binds run this same binary with a word — home, shell, agent, run, jump,
// next, prev — each a short command against the server that says nothing
// on success: run-shell would put anything printed in front of the user,
// so a failure is said through display-message.

// homeName is what the home window is called.
const homeName = "scrn"

// homeCommand is what the navigator's pane runs: this build, as the
// navigator. A variable so the tests can put something inert there.
var homeCommand = func() string {
	return shellQuote(scrnExe()) + " nav"
}

// shellQuote is s as one word to a POSIX shell, whatever is in it.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// scrnExe is the path of this build, for the configuration and the home
// window to run. When it cannot be known, the name on the PATH has to do.
func scrnExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "scrn"
	}
	return exe
}

// runLaunch is `scrn`: the server under its configuration, the home window,
// and this terminal attached. It returns only when it could not attach;
// attached, the process is tmux's.
func runLaunch() error {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return errors.New("tmux is not installed")
	}
	// The socket's directory is scrn's to make: tmux creates the socket but
	// not the directory around it, and a machine that has never run scrn
	// has no ~/.local/state/scrn to put it in.
	if err := os.MkdirAll(filepath.Dir(socketPath()), 0o700); err != nil {
		return err
	}
	conf := confPath()
	if err := os.WriteFile(conf, []byte(tmuxConf(scrnExe(), scrollbackLines, navWidth)), 0o600); err != nil {
		return err
	}

	if _, err := tmuxCommand("has-session", "-t", tmuxSession); err != nil {
		// The first launch brings the server up under the configuration,
		// with the home window as the session's first.
		out, err := tmuxCommand("-f", conf, "new-session", "-d", "-s", tmuxSession,
			"-n", homeName, "-c", "/", "-P", "-F", "#{window_id}\t#{pane_id}", homeCommand())
		if err != nil {
			return err
		}
		if f := strings.Split(out, "\t"); len(f) == 2 {
			markHome(f[0], f[1])
		}
	} else {
		// A server already running learns this build's bindings, the home
		// window comes back if it was closed, and a navigator from an
		// older build gives way to this one.
		if _, err := tmuxCommand("source-file", conf); err != nil {
			return err
		}
		h, err := ensureHome()
		if err != nil {
			return err
		}
		if err := refreshHome(h); err != nil {
			return err
		}
	}

	return syscall.Exec(tmux, []string{"tmux", "-S", socketPath(), "attach", "-t", tmuxSession}, os.Environ())
}

// home is the home window, and the pane in it the navigator runs in.
type home struct {
	win  string
	pane string
}

// markHome pins the options that tell the home window and the navigator's
// pane apart from the rest: the window's reaches every pane in it, which is
// how a shell shown beside the navigator is known to be shown.
func markHome(win, pane string) {
	_, _ = tmuxCommand("set", "-w", "-t", win, "@scrn_home", "1", ";",
		"set", "-p", "-t", pane, "@scrn_nav", "1")
}

// isNavCommand reports whether a pane's start command is the navigator's:
// this build's, or an older build's, which ran the same word. tmux reports
// a command with spaces in it quoted.
func isNavCommand(cmd string) bool {
	return strings.HasSuffix(strings.Trim(strings.TrimSpace(cmd), `"`), " nav")
}

// ensureHome finds the navigator, or makes it when it has gone: a new pane
// down the left of a home window that lost it, or a new home window. A
// navigator is known by its pane's mark, or — a server an older build
// started, whose navigator wears only the window's mark — by what the pane
// runs, and is marked then. A second navigator is scrn's own leftover,
// holding no work, and goes.
func ensureHome() (home, error) {
	out, err := tmuxCommand("list-panes", "-s", "-t", tmuxSession, "-F", "#{window_id}\t#{pane_id}\t#{@scrn_nav}\t#{@scrn_home}\t#{pane_start_command}")
	if err != nil {
		return home{}, err
	}
	homeWin := ""
	var navs []home
	marked := -1
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			continue
		}
		if f[3] == "1" {
			homeWin = f[0]
		}
		if f[2] == "1" || isNavCommand(f[4]) {
			if f[2] == "1" && marked < 0 {
				marked = len(navs)
			}
			navs = append(navs, home{win: f[0], pane: f[1]})
		}
	}
	if len(navs) > 0 {
		keep := max(marked, 0)
		for i, n := range navs {
			if i != keep {
				_, _ = tmuxCommand("kill-pane", "-t", n.pane)
			}
		}
		if marked < 0 {
			markHome(navs[keep].win, navs[keep].pane)
		}
		return navs[keep], nil
	}
	if homeWin != "" {
		// The window is there with a shell in it and no navigator: the
		// navigator goes back on the left, as the main pane of the layout.
		out, err = tmuxCommand("split-window", "-h", "-b", "-d", "-P", "-F", "#{pane_id}",
			"-t", homeWin, "-c", "/", homeCommand(), ";",
			"select-layout", "-t", homeWin, "main-vertical")
		if err != nil {
			return home{}, err
		}
		pane := strings.TrimSpace(out)
		markHome(homeWin, pane)
		return home{win: homeWin, pane: pane}, nil
	}
	out, err = tmuxCommand("new-window", "-d", "-P", "-t", tmuxSession+":", "-F", "#{window_id}\t#{pane_id}",
		"-n", homeName, "-c", "/", homeCommand())
	if err != nil {
		return home{}, err
	}
	f := strings.Split(out, "\t")
	if len(f) != 2 {
		return home{}, errors.New("tmux said " + out)
	}
	markHome(f[0], f[1])
	return home{win: f[0], pane: f[1]}, nil
}

// refreshHome restarts the navigator in its pane when it is not this
// build's: the configuration is re-read at every launch so the bindings
// are always the build's, and the navigator should be too, or a fix in
// it waits, unseen, behind a process started days ago. The pane stays —
// its place in the layout, its id — and the program in it is replaced.
// Only the launcher does this: a chord that restarted the navigator under
// the keys would be a surprise.
func refreshHome(h home) error {
	out, err := tmuxCommand("display", "-p", "-t", h.pane, "#{pane_start_command}")
	if err != nil {
		return err
	}
	if strings.Trim(strings.TrimSpace(out), `"`) == homeCommand() {
		return nil
	}
	_, err = tmuxCommand("respawn-pane", "-k", "-t", h.pane, homeCommand())
	return err
}

// runHome is `scrn home [key]`: to the navigator, and handed a key, that key
// pressed there — / to start looking, ? for the keys, A for the picker.
func runHome(key string) error {
	h, err := ensureHome()
	if err != nil {
		return err
	}
	if _, err := tmuxCommand("select-window", "-t", h.win, ";", "select-pane", "-t", h.pane); err != nil {
		return err
	}
	if key != "" {
		_, err = tmuxCommand("send-keys", "-t", h.pane, key)
	}
	return err
}

// tell presses a key at the navigator without going to it: the navigator
// acts on the key — showing a shell, taking the keys there — and the keys
// stay where they were unless that is where the navigator sends them. It
// is how a chord reaches what only the navigator knows: the order of the
// shells, and which agents are waiting.
func tell(key string) error {
	h, err := ensureHome()
	if err != nil {
		return err
	}
	_, err = tmuxCommand("send-keys", "-t", h.pane, key)
	return err
}

// runShellAt is `scrn shell [dir]` and `scrn agent [dir]`: a shell — or a
// command with a shell waiting behind it — in dir, opened in a window of
// its own and wanted, which the navigator answers by showing it beside
// itself and taking the keys there. The navigator is made sure of after,
// so a home window that was closed is back to answer.
func runShellAt(dir, command string) error {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if _, err := createWindow(tmuxCommand, dir, command, "", true); err != nil {
		return err
	}
	_, err := ensureHome()
	return err
}

// runPlanAt is `scrn run [dir]`: the plan of the place holding dir, started
// where it is not already running, each entry in a window of its own.
func runPlanAt(dir string) error {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m := model{subs: map[string][]Project{}}
	m.projects, m.groups, err = discoverAll(cfg.roots(), cfg.skipSet())
	if err != nil {
		return err
	}
	for _, p := range m.projects {
		if under(dir, p.Path) {
			m.subs[p.Path] = subProjects(p.Path)
		}
	}
	p, ok := m.placeAt(dir)
	if !ok {
		return errors.New("no project holds " + dir)
	}
	plan := readPlan(p.Path)
	if len(plan.Entries) == 0 {
		return errors.New(p.Name + " does not say what it needs")
	}

	out, err := tmuxCommand("list-panes", "-a", "-F", listFormat)
	if err != nil && !errors.Is(err, errNoServer) {
		return err
	}
	running := map[string]bool{}
	held, _ := parseListing(out)
	for _, pane := range held {
		if pane.name != "" && pane.dir == p.Path {
			running[pane.name] = true
		}
	}
	missing := plan.missing(running)
	if len(missing) == 0 {
		return errors.New("everything " + p.Name + " needs is running")
	}
	for _, e := range missing {
		if _, err := createWindow(tmuxCommand, p.Path, e.Run, e.Name, false); err != nil {
			return err
		}
	}
	return nil
}

// report says what a chord's command could not do, where the user is
// looking: the status line. The command itself stays silent on stdout, which
// run-shell would otherwise put in front of the user as a page. It reports
// whether the message reached anyone: a chord typed at a terminal with no
// server to show it is told on stderr instead, and that is a failure.
func report(err error) bool {
	if err == nil {
		return true
	}
	if _, told := tmuxCommand("display-message", "scrn: "+err.Error()); told != nil {
		fmt.Fprintf(os.Stderr, "scrn: %v\n", err)
		return false
	}
	return true
}

// runJump is `scrn jump`: the next agent waiting on you, which is the
// navigator's tab — it knows the marks, and it shows the shell and takes
// the keys there. With nothing waiting the navigator says so at its foot,
// which is in view from every shell.
func runJump() error {
	return tell("Tab")
}

// runStep is `scrn next` and `scrn prev`: the shell after or before the one
// shown, in the navigator's order, which is the navigator's J and K.
func runStep(delta int) error {
	if delta < 0 {
		return tell("K")
	}
	return tell("J")
}

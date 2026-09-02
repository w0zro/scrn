package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// The launcher and the chords. `scrn` brings the server up under scrn's
// configuration, makes sure the home window exists, and hands the terminal
// to tmux. The chords the configuration binds run this same binary with a
// word — home, shell, agent, run, jump — each a short command against the
// server that says nothing on success: run-shell would put anything printed
// in front of the user, so a failure is said through display-message.

// homeName is what the home window is called.
const homeName = "scrn"

// homeCommand is what the home window runs: this build, as the navigator.
// A variable so the tests can put something inert there.
var homeCommand = func() string {
	return "'" + strings.ReplaceAll(scrnExe(), "'", `'\''`) + "' nav"
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
	if err := os.WriteFile(conf, []byte(tmuxConf(scrnExe(), scrollbackLines)), 0o600); err != nil {
		return err
	}

	if _, err := tmuxCommand("has-session", "-t", tmuxSession); err != nil {
		// The first launch brings the server up under the configuration,
		// with the home window as the session's first.
		out, err := tmuxCommand("-f", conf, "new-session", "-d", "-s", tmuxSession,
			"-n", homeName, "-c", "/", "-P", "-F", "#{window_id}", homeCommand())
		if err != nil {
			return err
		}
		_, _ = tmuxCommand("set", "-w", "-t", out, "@scrn_home", "1")
	} else {
		// A server already running learns this build's bindings, and the
		// home window comes back if it was closed.
		if _, err := tmuxCommand("source-file", conf); err != nil {
			return err
		}
		if _, err := ensureHome(); err != nil {
			return err
		}
	}

	return syscall.Exec(tmux, []string{"tmux", "-S", socketPath(), "attach", "-t", tmuxSession}, os.Environ())
}

// home is the home window: the window and the pane the navigator runs in.
type home struct {
	win  string
	pane string
}

// ensureHome finds the home window, or makes one when it has been closed.
func ensureHome() (home, error) {
	out, err := tmuxCommand("list-windows", "-t", tmuxSession, "-F", "#{window_id}\t#{pane_id}\t#{@scrn_home}")
	if err != nil {
		return home{}, err
	}
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 && f[2] == "1" {
			return home{win: f[0], pane: f[1]}, nil
		}
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
	_, _ = tmuxCommand("set", "-w", "-t", f[0], "@scrn_home", "1")
	return home{win: f[0], pane: f[1]}, nil
}

// runHome is `scrn home [key]`: to the navigator, and handed a key, that key
// pressed there — / to start looking, ? for the keys, A for the picker.
func runHome(key string) error {
	h, err := ensureHome()
	if err != nil {
		return err
	}
	if _, err := tmuxCommand("select-window", "-t", h.win); err != nil {
		return err
	}
	if key != "" {
		_, err = tmuxCommand("send-keys", "-t", h.pane, key)
	}
	return err
}

// runShellAt is `scrn shell [dir]` and `scrn agent [dir]`: a new window
// holding a shell — or a command with a shell waiting behind it — in dir,
// and the client taken to it.
func runShellAt(dir, command string) error {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	b, err := createWindow(tmuxCommand, dir, command, "")
	if err != nil {
		return err
	}
	_, err = tmuxCommand("select-window", "-t", b.win)
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
	for _, pane := range parseListing(out) {
		if pane.name != "" && pane.dir == p.Path {
			running[pane.name] = true
		}
	}
	missing := plan.missing(running)
	if len(missing) == 0 {
		return errors.New("everything " + p.Name + " needs is running")
	}
	for _, e := range missing {
		if _, err := createWindow(tmuxCommand, p.Path, e.Run, e.Name); err != nil {
			return err
		}
	}
	return nil
}

// report says what a chord's command could not do, where the user is
// looking: the status line. The command itself stays silent on stdout, which
// run-shell would otherwise put in front of the user as a page.
func report(err error) {
	if err == nil {
		return
	}
	_, _ = tmuxCommand("display-message", "scrn: "+err.Error())
}

// runJump is `scrn jump`: the next agent waiting on you, in window order
// from where the client is, wrapping.
func runJump() error {
	return errors.New("jump is not wired yet")
}

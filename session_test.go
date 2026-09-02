package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the whole way: a real tmux server on a private socket, a
// real shell in a real pane, and the model reading the session's events. They
// are what stands where the old daemon's end-to-end tests stood.

// connected gives a model a session over a private tmux server.
func connected(t *testing.T, m model) model {
	t.Helper()
	tmuxOnSocket(t)
	// The session is let go before the server is: one left watching finds
	// the next test's server on its next probe and joins it as a second
	// client, which is a size arbitration nobody asked for.
	s := newSession()
	t.Cleanup(s.close)
	next, _ := m.Update(serverReadyMsg{session: s})
	return next.(model)
}

// pump feeds the session's messages into the model until want is satisfied.
func pump(t *testing.T, m model, want func(model) bool, d time.Duration) model {
	t.Helper()
	deadline := time.After(d)
	for !want(m) {
		select {
		case ev := <-m.server.events:
			next, _ := m.Update(ev)
			m = next.(model)
		case <-deadline:
			t.Fatalf("timed out; terms=%d", len(m.terms))
		}
	}
	return m
}

func hasShell(m model) bool { return len(m.terms) > 0 }

// onlyShell is the pid of the one shell the model holds.
func onlyShell(t *testing.T, m model) int {
	t.Helper()
	if len(m.terms) != 1 {
		t.Fatalf("terms = %d, want one shell", len(m.terms))
	}
	for pid := range m.terms {
		return pid
	}
	return 0
}

// openShellIn opens a shell through the server and waits for it to be held.
func openShellIn(t *testing.T, m model, dir string) model {
	t.Helper()
	m = connected(t, m)
	m.server.open(dir, "", "")
	return pump(t, m, hasShell, 10*time.Second)
}

func repoModel() model {
	return withProcList(90, 14, []Project{{Name: "tmp", Path: "/tmp"}}, nil)
}

func TestOpeningAShellHoldsItInAWindowOfItsOwn(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	pid := onlyShell(t, m)

	p := m.server.pane(pid)
	if p == nil || p.win == "" {
		t.Fatalf("pane = %+v, want the shell in a window the client can be taken to", p)
	}
	out, _ := tmuxCommand("display", "-p", "-t", p.id, "#{pane_current_path}")
	if out != "/tmp" && out != "/private/tmp" {
		t.Errorf("the shell works in %q, want /tmp", out)
	}
}

func TestTheShellIsShownBesideTheNavigatorAndParkedAgain(t *testing.T) {
	// The shell is a tmux pane. Shown, it sits in the home window beside the
	// navigator's pane, which keeps its column; parked, it is back in a
	// window of its own and the navigator has the home window to itself.
	m := connected(t, repoModel())
	out, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "-x", "120", "-y", "30",
		"-n", homeName, "-P", "-F", "#{window_id}\t#{pane_id}", "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Split(out, "\t")
	markHome(f[0], f[1])
	if _, err := tmuxCommand("set", "-g", "main-pane-width", "28"); err != nil {
		t.Fatal(err)
	}

	m.server.open("/tmp", "", "")
	m = pump(t, m, hasShell, 10*time.Second)
	pid := onlyShell(t, m)
	shown := func(m model) bool { p := m.server.pane(pid); return p != nil && p.shown }

	m.server.show(pid)
	m = pump(t, m, shown, 10*time.Second)
	out, _ = tmuxCommand("list-panes", "-t", f[0], "-F", "#{pane_id} #{pane_left} #{pane_width} #{pane_active}")
	if !strings.Contains(out, f[1]+" 0 28 0") || !strings.Contains(out, m.server.pane(pid).id+" 29 ") {
		t.Errorf("home window panes:\n%s\nwant the navigator 28 wide on the left and the shell active beside it", out)
	}

	// A second shell, previewed from the list: it trades places with the
	// first, and the keys stay at the navigator. tmux's swap makes the
	// pane swapped in the active one unless told not to.
	// Opened straight through tmux: a shell the navigator opens itself
	// is one it hands the keys to, and this one must not be.
	if _, err := createWindow(tmuxCommand, "/tmp", "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := tmuxCommand("select-pane", "-t", f[1]); err != nil {
		t.Fatal(err)
	}
	m.server.list()
	m = pump(t, m, func(m model) bool { return len(m.terms) == 2 }, 10*time.Second)
	other := 0
	for p := range m.terms {
		if p != pid {
			other = p
		}
	}
	m.server.preview(other)
	m = pump(t, m, func(m model) bool { p := m.server.pane(other); return p != nil && p.shown }, 10*time.Second)
	out, _ = tmuxCommand("display", "-p", "-t", f[0], "#{pane_id}")
	if out != f[1] {
		t.Errorf("active pane after a glance's swap = %s, want the navigator %s", out, f[1])
	}

	m.server.preview(0)
	m = pump(t, m, func(m model) bool {
		for p := range m.terms {
			if q := m.server.pane(p); q != nil && q.shown {
				return false
			}
		}
		return true
	}, 10*time.Second)
	out, _ = tmuxCommand("list-panes", "-t", f[0], "-F", "#{pane_id}")
	if out != f[1] {
		t.Errorf("home window panes = %q, want the navigator alone", out)
	}
	if nav := navColumn(m); len(nav) == 0 || !strings.Contains(nav[0], "tmp") {
		t.Errorf("the navigator should still be there, got %v", nav)
	}
}

func TestClosingTheShellEmptiesTheList(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	pid := onlyShell(t, m)

	m.server.closeTerm(pid)
	pump(t, m, func(m model) bool { return len(m.terms) == 0 }, 10*time.Second)
}

func TestANamedShellCarriesItsName(t *testing.T) {
	// A plan's entry opens under the name the plan gave it, and the name
	// survives the trip through the server's state.
	m := connected(t, repoModel())
	m.server.open("/tmp", "cat", "web")
	m = pump(t, m, func(m model) bool { return len(m.terms) == 1 }, 10*time.Second)

	for _, rt := range m.terms {
		if rt.name != "web" || rt.dir != "/tmp" {
			t.Errorf("term = %+v, want the plan's name and directory", rt)
		}
	}
}

func TestTheFirstShellMakesTheSocketsDirectory(t *testing.T) {
	// tmux creates the socket but not the directory around it, and a machine
	// that has never run scrn has no state directory. The first shell has to
	// make it, or it is the one shell that can never open.
	tmuxOnSocket(t)
	t.Setenv("SCRN_SOCKET", filepath.Join(filepath.Dir(os.Getenv("SCRN_SOCKET")), "state", "scrn", "t.sock"))
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })

	s := newSession()
	t.Cleanup(s.close)
	next, _ := repoModel().Update(serverReadyMsg{session: s})
	m := next.(model)
	m.server.open("/tmp", "", "")
	pump(t, m, hasShell, 10*time.Second)

	if _, err := os.Stat(os.Getenv("SCRN_SOCKET")); err != nil {
		t.Errorf("the socket should be where scrn said: %v", err)
	}
}

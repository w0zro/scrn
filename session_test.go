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

// screenHas reports whether a shell's previewed screen carries text.
func screenHas(pid int, text string) func(model) bool {
	return func(m model) bool {
		rt := m.terms[pid]
		return rt != nil && strings.Contains(strings.Join(rt.lines(30), "\n"), text)
	}
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

func TestTheShellIsRealAndItsScreenReachesThePreview(t *testing.T) {
	// The shell is a tmux pane the keys reach through tmux; what it draws
	// comes back to the navigator as the preview, and the navigator is
	// still there beside it.
	m := openShellIn(t, repoModel(), "/tmp")
	pid := onlyShell(t, m)
	p := m.server.pane(pid)
	if _, err := tmuxCommand("send-keys", "-t", p.id, "echo marker-in-the-pane", "Enter"); err != nil {
		t.Fatal(err)
	}
	m.server.attach(pid)
	m = pump(t, m, screenHas(pid, "marker-in-the-pane"), 10*time.Second)

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

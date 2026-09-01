package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// These tests run the whole way: a real tmux server on a private socket, a
// real shell in a real pane, and the model reading the session's events. They
// are what stands where the old daemon's end-to-end tests stood.

// connected gives a model a session over a private tmux server.
func connected(t *testing.T, m model) model {
	t.Helper()
	tmuxOnSocket(t)
	next, _ := m.Update(daemonReadyMsg{session: newSession()})
	return next.(model)
}

// pump feeds the session's messages into the model until want is satisfied.
func pump(t *testing.T, m model, want func(model) bool, d time.Duration) model {
	t.Helper()
	deadline := time.After(d)
	for !want(m) {
		select {
		case ev := <-m.daemon.events:
			next, _ := m.Update(ev)
			m = next.(model)
		case <-deadline:
			t.Fatalf("timed out; terms=%d focus=%d", len(m.terms), m.focus)
		}
	}
	return m
}

func hasShell(m model) bool { return len(m.terms) > 0 && m.focus != 0 }

func paneHas(text string) func(model) bool {
	return func(m model) bool {
		t := m.focused()
		return t != nil && strings.Contains(strings.Join(t.lines(30), "\n"), text)
	}
}

// openShellIn opens a shell through the server and waits for it to take the
// keys.
func openShellIn(t *testing.T, m model, dir string) model {
	t.Helper()
	m = connected(t, m)
	m.daemon.open(dir, "", "", 60, 12)
	return pump(t, m, hasShell, 10*time.Second)
}

// send types a line into the model — which forwards it to the focused
// shell — and presses enter.
func send(m model, s string) model {
	for _, r := range s {
		next, _ := m.Update(typed(string(r)))
		m = next.(model)
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return next.(model)
}

func repoModel() model {
	return withProcList(90, 14, []Project{{Name: "tmp", Path: "/tmp"}}, nil)
}

func TestEnterOnARepoOpensAShellAndFocusesIt(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")

	if m.focused() == nil {
		t.Fatal("a new shell should take the keystrokes")
	}
	if len(m.terms) != 1 {
		t.Errorf("terms = %d, want the one shell", len(m.terms))
	}
}

func TestTheShellRunsInThePaneAndTheNavigatorRemains(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	m = pump(t, send(m, "echo marker-in-the-pane"), paneHas("marker-in-the-pane"), 10*time.Second)

	if nav := navColumn(m); len(nav) == 0 || !strings.Contains(nav[0], "tmp") {
		t.Errorf("the navigator should still be there, got %v", nav)
	}
}

func TestCapitalsReachTheShell(t *testing.T) {
	// The bug that pushed scrn onto tmux: a shifted printable must arrive as
	// its text. The keystrokes cross as kitty-style events — base code,
	// shift, text — and the pane has to show what was typed.
	m := openShellIn(t, repoModel(), "/tmp")
	for _, r := range "echo " {
		next, _ := m.Update(typed(string(r)))
		m = next.(model)
	}
	for _, r := range "BIG" {
		next, _ := m.Update(tea.KeyPressMsg{Code: r + 32, Text: string(r), Mod: tea.ModShift})
		m = next.(model)
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = pump(t, next.(model), paneHas("BIG"), 10*time.Second)
}

func TestCtrlOIsSentOnToTheShell(t *testing.T) {
	// scrn reserves nothing but the prefix, so ctrl+o belongs to the program
	// in the pane, emacs and its kin included.
	m := openShellIn(t, repoModel(), "/tmp")
	next, _ := m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})

	if next.(model).focused() == nil {
		t.Error("ctrl+o should go to the shell, not step out of it")
	}
}

func TestClosingTheShellEmptiesTheList(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	pid := m.focus

	m.daemon.closeTerm(pid)
	m = pump(t, m, func(m model) bool { return len(m.terms) == 0 }, 10*time.Second)
	if m.focus != 0 {
		t.Errorf("focus = %d, want it released with the shell", m.focus)
	}
}

func TestANamedShellCarriesItsName(t *testing.T) {
	// A plan's entry opens under the name the plan gave it, and the name
	// survives the trip through the server's state.
	m := connected(t, repoModel())
	m.daemon.open("/tmp", "cat", "web", 60, 12)
	m = pump(t, m, func(m model) bool { return len(m.terms) == 1 }, 10*time.Second)

	for _, rt := range m.terms {
		if rt.name != "web" || rt.dir != "/tmp" {
			t.Errorf("term = %+v, want the plan's name and directory", rt)
		}
	}
}

func TestTheTranscriptComesBack(t *testing.T) {
	// What scrolls off the pane is not gone: the server keeps it, and the
	// reader gets it oldest first, above the screen as it stands.
	m := openShellIn(t, repoModel(), "/tmp")
	m = pump(t, send(m, "seq 1 40"), paneHas("40"), 10*time.Second)

	m.daemon.history(m.focus)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-m.daemon.events:
			if h, ok := ev.(historyMsg); ok {
				if !strings.Contains(h.history, "1") {
					t.Fatalf("history = %q, want the scrolled-off lines", h.history)
				}
				return
			}
		case <-deadline:
			t.Fatal("the transcript never came back")
		}
	}
}

func TestTheServerLearnsTheTerminalsColors(t *testing.T) {
	// tmux answers a pane's OSC 10 and 11 from window-style; the session
	// setting it is what lets programs in panes find out what color the
	// terminal is, the way the old emulator's answers did.
	m := connected(t, repoModel())
	m.daemon.open("/tmp", "", "", 60, 12)
	m = pump(t, m, hasShell, 10*time.Second)

	m.daemon.theme("#e6e6e6", "#1a1b26")
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := m.daemon.run("show", "-g", "window-style")
		if err == nil && strings.Contains(out, "fg=#e6e6e6,bg=#1a1b26") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("window-style = %q, want the terminal's colors", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAProgramsCopyReachesTheSystemClipboard(t *testing.T) {
	// OSC 52 from inside a pane: the server catches it into a buffer, the
	// session carries the bytes to the clipboard, and the buffer goes.
	copied := make(chan string, 1)
	old := writeClipboard
	writeClipboard = func(text string) error { copied <- text; return nil }
	t.Cleanup(func() { writeClipboard = old })

	m := openShellIn(t, repoModel(), "/tmp")
	// aGVsbG8tY29waWVk = "hello-copied"
	m = send(m, `printf '\033]52;c;aGVsbG8tY29waWVk\a'`)

	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-copied:
			if got != "hello-copied" {
				t.Fatalf("clipboard got %q, want the program's copy", got)
			}
			return
		case ev := <-m.daemon.events:
			next, _ := m.Update(ev)
			m = next.(model)
		case <-deadline:
			t.Fatal("the copy never reached the clipboard")
		}
	}
}

package main

import (
	"os"
	"path/filepath"
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
	m.server.open(dir, "", "", 60, 12)
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
	pump(t, next.(model), paneHas("BIG"), 10*time.Second)
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

	m.server.closeTerm(pid)
	m = pump(t, m, func(m model) bool { return len(m.terms) == 0 }, 10*time.Second)
	if m.focus != 0 {
		t.Errorf("focus = %d, want it released with the shell", m.focus)
	}
}

func TestANamedShellCarriesItsName(t *testing.T) {
	// A plan's entry opens under the name the plan gave it, and the name
	// survives the trip through the server's state.
	m := connected(t, repoModel())
	m.server.open("/tmp", "cat", "web", 60, 12)
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

	m.server.history(m.focus)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-m.server.events:
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
	m.server.open("/tmp", "", "", 60, 12)
	m = pump(t, m, hasShell, 10*time.Second)

	m.server.theme("#e6e6e6", "#1a1b26")
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := m.server.run("show", "-g", "window-style")
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
		case ev := <-m.server.events:
			next, _ := m.Update(ev)
			m = next.(model)
		case <-deadline:
			t.Fatal("the copy never reached the clipboard")
		}
	}
}

// TestEveryShellKeepsItsSizeWhenAnotherWindowLetsGo is the regression for a
// shell left drawing into a rectangle. tmux hands a client's size to the
// window that client has current; scrn has no current window, so a second,
// narrower scrn window takes every window down to its size and, on going,
// gets only the current one handed back. The others stayed narrow, and the
// shell being watched kept drawing at the narrow width for the rest of its
// life.
func TestEveryShellKeepsItsSizeWhenAnotherWindowLetsGo(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	// A second shell, so the server holds a window no client has current —
	// the one that used to be stranded.
	m.server.open("/tmp", "", "", 60, 12)
	m = pump(t, m, func(m model) bool { return len(m.terms) > 1 }, 10*time.Second)

	// The model is done being asked anything; its events are drained so
	// nothing blocks behind a full channel.
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-m.server.events:
			case <-done:
				return
			}
		}
	}()

	sizes := func() []string {
		out, err := m.server.run("list-windows", "-a", "-F", "#{window_width}x#{window_height}")
		if err != nil {
			t.Helper()
			t.Fatal(err)
		}
		return strings.Split(out, "\n")
	}
	all := func(want string) bool {
		for _, s := range sizes() {
			if s != want {
				return false
			}
		}
		return true
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: windows are %v", what, sizes())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	until("the shells never reached the size scrn asked for", func() bool { return all("60x12") })

	// A second scrn window, in a terminal half the size, takes every window
	// down with it — which is right, so both can draw what the shell drew.
	other, err := startCtl(func(ctlNote) {})
	if err != nil {
		t.Fatal(err)
	}
	other.say("refresh-client -C 30x6")
	until("the narrower window never took the shells down", func() bool { return all("30x6") })

	// And on its going, every shell is the size this window asks for again —
	// not just the one tmux happens to call current.
	other.close()
	until("a shell was left in the narrower window's rectangle", func() bool { return all("60x12") })
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
	m.server.open("/tmp", "", "", 60, 12)
	m = pump(t, m, hasShell, 10*time.Second)

	if m.focused() == nil {
		t.Fatal("the shell should be open and focused")
	}
	if _, err := os.Stat(os.Getenv("SCRN_SOCKET")); err != nil {
		t.Errorf("the socket should be where scrn said: %v", err)
	}
}

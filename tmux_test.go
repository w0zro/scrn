package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestNotesAreReadFromTheStream(t *testing.T) {
	cases := []struct {
		line string
		want ctlNote
	}{
		{"%output %3 hi there", ctlNote{kind: noteOutput, pane: "%3"}},
		{"%window-close @2", ctlNote{kind: noteWindows}},
		{"%unlinked-window-close @2", ctlNote{kind: noteWindows}},
		{"%window-add @4", ctlNote{kind: noteWindows}},
		{"%exit", ctlNote{kind: noteExit}},
		{"%exit detached", ctlNote{kind: noteExit}},
		{"%begin 123 4 1", ctlNote{}},
		{"%layout-change @1 whatever", ctlNote{}},
		{"just some pane text", ctlNote{}},
	}
	for _, c := range cases {
		if got := parseNote(c.line); got != c.want {
			t.Errorf("parseNote(%q) = %+v, want %+v", c.line, got, c.want)
		}
	}
}

func TestKeysBecomeTmuxCommands(t *testing.T) {
	cases := []struct {
		name string
		key  keyPress
		want string
	}{
		// Text is sent as bytes, in hex, so nothing ever meets quoting —
		// capitals included, which is the bug this path once fixed.
		{"a letter", keyPress{Code: 'l', Text: "l"}, "send-keys -t %1 -H 6c"},
		{"a capital", keyPress{Code: 'a', Text: "A", Mod: int(tea.ModShift)}, "send-keys -t %1 -H 41"},
		{"utf-8 text", keyPress{Code: 'é', Text: "é"}, "send-keys -t %1 -H c3 a9"},
		{"alt text is an escape prefix", keyPress{Code: 'b', Text: "b", Mod: int(tea.ModAlt)},
			"send-keys -t %1 -H 1b 62"},

		// Everything else goes by name, and tmux encodes it for the pane:
		// cursor mode, extended keys, all of it is tmux's business.
		{"enter", keyPress{Code: tea.KeyEnter}, "send-keys -t %1 Enter"},
		{"up", keyPress{Code: tea.KeyUp}, "send-keys -t %1 Up"},
		{"ctrl+c", keyPress{Code: 'c', Mod: int(tea.ModCtrl)}, "send-keys -t %1 C-c"},
		{"ctrl+arrow words its way", keyPress{Code: tea.KeyLeft, Mod: int(tea.ModCtrl)},
			"send-keys -t %1 C-Left"},
		{"shift+tab", keyPress{Code: tea.KeyTab, Mod: int(tea.ModShift)}, "send-keys -t %1 S-Tab"},
		{"alt+enter", keyPress{Code: tea.KeyEnter, Mod: int(tea.ModAlt)}, "send-keys -t %1 M-Enter"},
		{"f5", keyPress{Code: tea.KeyF5}, "send-keys -t %1 F5"},
		{"delete", keyPress{Code: tea.KeyDelete}, "send-keys -t %1 DC"},
		{"page up", keyPress{Code: tea.KeyPgUp}, "send-keys -t %1 PPage"},
	}
	for _, c := range cases {
		got := tmuxKeyLines("%1", &c.key)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: lines = %q, want [%q]", c.name, got, c.want)
		}
	}
}

func TestAPasteCrossesThroughABuffer(t *testing.T) {
	lines := tmuxPasteLines("%2", "a b\n\"quote\" $var ;")
	if len(lines) != 2 {
		t.Fatalf("lines = %q, want a set-buffer and a paste-buffer", lines)
	}
	if want := `set-buffer -b scrn-paste "a b\012\042quote\042 \044var \073"`; lines[0] != want {
		t.Errorf("set-buffer = %q, want %q", lines[0], want)
	}
	if want := "paste-buffer -p -d -b scrn-paste -t %2"; lines[1] != want {
		t.Errorf("paste-buffer = %q, want %q", lines[1], want)
	}
}

func TestAMouseClickBecomesSGRBytes(t *testing.T) {
	// Button 1 is the left button on both sides of the translation; SGR
	// numbers it 0, at 1-based coordinates.
	press := &mousePress{X: 4, Y: 2, Button: 1, Action: actPress}
	lines := tmuxMouseLines("%1", press, true)
	// \x1b[<0;5;3M
	if want := "send-keys -t %1 -H 1b 5b 3c 30 3b 35 3b 33 4d"; len(lines) != 1 || lines[0] != want {
		t.Fatalf("lines = %q, want [%q]", lines, want)
	}
	if lines := tmuxMouseLines("%1", press, false); lines != nil {
		t.Error("a pane not listening in SGR should be left unclicked, not garbled")
	}
}

// tmuxOnSocket points scrn at a private tmux server for one test, and tears
// the server down after. Skips when tmux is not installed. The socket lives
// under /tmp rather than the test's own directory: sun_path holds 104 bytes,
// and a test name is most of that on its own.
func tmuxOnSocket(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "scrn-tmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("SCRN_SOCKET", filepath.Join(dir, "t.sock"))
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })
}

func TestTheBridgeSpeaksToARealServer(t *testing.T) {
	tmuxOnSocket(t)

	// A session with one window running an inert command, so the test does
	// not depend on anyone's shell.
	out, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "-x", "50", "-y", "8",
		"-P", "-F", "#{pane_id} #{pane_pid}", "cat")
	if err != nil {
		t.Fatal(err)
	}
	pane, _, ok := strings.Cut(out, " ")
	if !ok {
		t.Fatalf("new-session said %q, want a pane and a pid", out)
	}

	notes := make(chan ctlNote, 64)
	ctl, err := startCtl(func(n ctlNote) { notes <- n })
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.close()

	// Typed text comes back out of a cat, which is the whole round trip:
	// control-mode write, pty, pane, %output notification, capture.
	for _, line := range tmuxKeyLines(pane, &keyPress{Code: 'h', Text: "Hi"}) {
		ctl.say(line)
	}
	for _, line := range tmuxKeyLines(pane, &keyPress{Code: tea.KeyEnter}) {
		ctl.say(line)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case n := <-notes:
			if n.kind != noteOutput || n.pane != pane {
				continue
			}
			screen, err := tmuxCommand("capture-pane", "-e", "-p", "-t", pane)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(screen, "Hi") {
				return
			}
		case <-deadline:
			t.Fatal("the typed text never showed on the pane")
		}
	}
}

func TestAnAbsentServerIsAnEmptyAnswer(t *testing.T) {
	tmuxOnSocket(t)
	_, err := tmuxCommand("list-panes", "-a")
	if err != errNoServer {
		t.Fatalf("err = %v, want errNoServer", err)
	}
}

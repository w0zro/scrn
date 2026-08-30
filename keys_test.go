package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// bytesFor runs a keystroke the whole way: through the translation the window
// does, into an emulator in whatever modes are given, and out as the bytes a
// shell would receive. Those bytes are the thing worth asserting — the
// translation on its own has no answer, which is the reason it stopped trying
// to have one.
func bytesFor(t *testing.T, msg tea.KeyMsg, modes ...string) string {
	t.Helper()

	e := vt.NewSafeEmulator(80, 24)
	defer e.Close()

	// The emulator blocks writing its answers until they are read, which is
	// what term.reply does in earnest.
	out := make(chan string, 8)
	go func() {
		buf := make([]byte, 128)
		for {
			n, err := e.Read(buf)
			if n > 0 {
				out <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	for _, mode := range modes {
		e.Write([]byte(mode))
	}

	k := keyEvent(msg)
	if k == nil {
		return ""
	}
	e.SendKey(uv.KeyPressEvent{Code: k.Code, Text: k.Text, Mod: uv.KeyMod(k.Mod)})

	select {
	case s := <-out:
		return s
	case <-time.After(2 * time.Second):
		return "<nothing>"
	}
}

func TestAKeystrokeReachesTheShellAsTheRightBytes(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyMsg
		want string
	}{
		{"letters", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")}, "l"},
		{"utf-8 survives", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("é")}, "é"},
		{"enter is a carriage return", tea.KeyMsg{Type: tea.KeyEnter}, "\r"},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}, "\t"},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, "\x7f"},
		{"ctrl+c interrupts", tea.KeyMsg{Type: tea.KeyCtrlC}, "\x03"},
		{"ctrl+d ends input", tea.KeyMsg{Type: tea.KeyCtrlD}, "\x04"},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}, "\x1b"},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, "\x1b[A"},
		{"left", tea.KeyMsg{Type: tea.KeyLeft}, "\x1b[D"},
		{"delete", tea.KeyMsg{Type: tea.KeyDelete}, "\x1b[3~"},
		{"space", tea.KeyMsg{Type: tea.KeySpace}, " "},
		{"alt is an escape prefix", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b"), Alt: true}, "\x1bb"},
	}
	for _, c := range cases {
		if got := bytesFor(t, c.msg); got != c.want {
			t.Errorf("%s: the shell got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAnArrowFollowsTheModeTheProgramAskedFor(t *testing.T) {
	// This is the whole reason a keystroke crosses as a keystroke. An up arrow
	// is one thing until a program asks for application cursor keys — which
	// vim, readline and less all do — and another thing afterwards. A window
	// deciding the bytes for itself has to be wrong in one of the two cases,
	// and it was wrong in the one that matters.
	const applicationCursorKeys = "\x1b[?1h"

	normal := bytesFor(t, tea.KeyMsg{Type: tea.KeyUp})
	application := bytesFor(t, tea.KeyMsg{Type: tea.KeyUp}, applicationCursorKeys)

	if normal != "\x1b[A" {
		t.Errorf("up in normal mode = %q, want %q", normal, "\x1b[A")
	}
	if application != "\x1bOA" {
		t.Errorf("up in application mode = %q, want %q", application, "\x1bOA")
	}
	if normal == application {
		t.Error("the arrow did not follow the mode, which is the point of sending the key")
	}
}

func TestCtrlOIsNotSentOnToTheShell(t *testing.T) {
	// It is scrn's one reserved key, so the shell must never see it.
	m := openShellIn(t, repoModel(), "/tmp")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})

	if next.(model).focused() != nil {
		t.Error("ctrl+o should be taken by scrn rather than passed through")
	}
}

func TestAMouseEventArrivesInThePanesOwnCoordinates(t *testing.T) {
	// The program in the pane believes it is drawing on a terminal that starts
	// at its own top left, so what it is told about has to be measured from
	// there rather than from the window's corner.
	click := tea.MouseMsg{X: navWidth + 1 + 4, Y: 7, Button: tea.MouseButtonLeft}
	got := mouseEvent(click, navWidth+1, 0)
	if got == nil {
		t.Fatal("a click inside the pane was dropped")
	}
	if got.X != 4 || got.Y != 7 {
		t.Errorf("click at (%d,%d), want (4,7)", got.X, got.Y)
	}

	// A click in the navigator is not the pane's to hear about.
	if got := mouseEvent(tea.MouseMsg{X: 3, Y: 2}, navWidth+1, 0); got != nil {
		t.Errorf("a click in the navigator reached the pane as %+v", got)
	}
}

package main

import (
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// bytesFor runs a keystroke the whole way: through the window's restatement,
// into an emulator in whatever modes are given, and out as the bytes a shell
// would receive. Those bytes are the thing worth asserting — the keystroke on
// its own has no answer, which is the reason nothing here writes bytes.
func bytesFor(t *testing.T, msg tea.KeyPressMsg, modes ...string) string {
	t.Helper()

	e := vt.NewSafeEmulator(80, 24)

	// The emulator blocks writing its answers until they are read, which is
	// what term.reply does in earnest. The teardown is term.close's too: the
	// emulator's own close races a concurrent read on its closed flag, so
	// the pipe goes first, the reader is waited out, and only then may the
	// emulator close beside nobody.
	out := make(chan string, 8)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
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
	defer e.Close()
	defer func() {
		if pw, ok := e.InputPipe().(*io.PipeWriter); ok {
			_ = pw.CloseWithError(io.EOF)
			<-readerDone
		}
	}()

	for _, mode := range modes {
		e.Write([]byte(mode))
	}

	for _, ev := range keyEvents(keyEvent(msg)) {
		e.SendKey(ev)
	}

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
		msg  tea.KeyPressMsg
		want string
	}{
		{"letters", tea.KeyPressMsg{Code: 'l', Text: "l"}, "l"},
		{"utf-8 survives", tea.KeyPressMsg{Code: 'é', Text: "é"}, "é"},
		{"enter is a carriage return", tea.KeyPressMsg{Code: tea.KeyEnter}, "\r"},
		{"tab", tea.KeyPressMsg{Code: tea.KeyTab}, "\t"},
		{"backspace", tea.KeyPressMsg{Code: tea.KeyBackspace}, "\x7f"},
		{"ctrl+c interrupts", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, "\x03"},
		{"ctrl+d ends input", tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}, "\x04"},
		{"esc", tea.KeyPressMsg{Code: tea.KeyEscape}, "\x1b"},
		{"up", tea.KeyPressMsg{Code: tea.KeyUp}, "\x1b[A"},
		{"left", tea.KeyPressMsg{Code: tea.KeyLeft}, "\x1b[D"},
		{"delete", tea.KeyPressMsg{Code: tea.KeyDelete}, "\x1b[3~"},
		{"space", tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}, " "},
		{"alt is an escape prefix", tea.KeyPressMsg{Code: 'b', Mod: tea.ModAlt}, "\x1bb"},

		// A key that typed under a modifier means its text. The kitty
		// protocol reports a capital as the base code plus a shift, which
		// the emulator's own tables drop on the floor; the legacy shape —
		// the capital as its own code, no modifier — never needed help.
		{"capitals under kitty's shift", tea.KeyPressMsg{Code: 'a', Text: "A", Mod: tea.ModShift}, "A"},
		{"capitals the legacy way", tea.KeyPressMsg{Code: 'A', Text: "A"}, "A"},
		{"capitals under caps lock", tea.KeyPressMsg{Code: 'a', Text: "A", Mod: tea.ModCapsLock}, "A"},
		{"shifted punctuation", tea.KeyPressMsg{Code: '1', Text: "!", Mod: tea.ModShift}, "!"},
		{"a shifted capital keeps alt's prefix",
			tea.KeyPressMsg{Code: 'a', Text: "A", Mod: tea.ModAlt | tea.ModShift}, "\x1bA"},
		{"shift+tab is still the emulator's", tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}, "\x1b[Z"},
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

	normal := bytesFor(t, tea.KeyPressMsg{Code: tea.KeyUp})
	application := bytesFor(t, tea.KeyPressMsg{Code: tea.KeyUp}, applicationCursorKeys)

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

func TestCtrlOIsSentOnToTheShell(t *testing.T) {
	// scrn reserves nothing but the prefix, so ctrl+o belongs to the program
	// in the pane, emacs and its kin included.
	m := openShellIn(t, repoModel(), "/tmp")
	next, _ := m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})

	if next.(model).focused() == nil {
		t.Error("ctrl+o should go to the shell, not step out of it")
	}
}

func TestAKeystrokeCrossesTheWireUnchanged(t *testing.T) {
	// Bubble Tea and the emulator share ultraviolet's vocabulary, so the wire
	// event is the keystroke restated, not translated: same code, same text,
	// same modifier bits.
	k := keyEvent(tea.KeyPressMsg{Code: 'g', Text: "g", Mod: tea.ModAlt})
	if k.Code != 'g' || k.Text != "g" || k.Mod != int(uv.ModAlt) {
		t.Errorf("event = %+v, want the keystroke as it was", k)
	}
}

func TestAMouseEventArrivesInThePanesOwnCoordinates(t *testing.T) {
	// The program in the pane believes it is drawing on a terminal that starts
	// at its own top left, so what it is told about has to be measured from
	// there rather than from the window's corner.
	click := tea.MouseClickMsg{X: navWidth + 1 + 4, Y: 7, Button: tea.MouseLeft}
	got := mouseEvent(click, navWidth+1, 0)
	if got == nil {
		t.Fatal("a click inside the pane was dropped")
	}
	if got.X != 4 || got.Y != 7 {
		t.Errorf("click at (%d,%d), want (4,7)", got.X, got.Y)
	}

	// A click in the navigator is not the pane's to hear about.
	if got := mouseEvent(tea.MouseClickMsg{X: 3, Y: 2}, navWidth+1, 0); got != nil {
		t.Errorf("a click in the navigator reached the pane as %+v", got)
	}
}

func TestAWheelTurnAndAReleaseKeepTheirActions(t *testing.T) {
	// The emulator reports a wheel turn onward as a press of a wheel button,
	// and a release as a release: the action has to survive the crossing.
	wheel := mouseEvent(tea.MouseWheelMsg{X: navWidth + 2, Y: 1, Button: tea.MouseWheelUp}, navWidth+1, 0)
	if wheel == nil || wheel.Action != actPress || wheel.Button != int(tea.MouseWheelUp) {
		t.Errorf("wheel = %+v, want a press of the wheel button", wheel)
	}
	up := mouseEvent(tea.MouseReleaseMsg{X: navWidth + 2, Y: 1, Button: tea.MouseLeft}, navWidth+1, 0)
	if up == nil || up.Action != actRelease {
		t.Errorf("release = %+v, want the release kept", up)
	}
}

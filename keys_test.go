package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

// How keys become bytes is tmux's business now, tested where the bridge is
// (tmux_test.go). What stays here is the client's half: the keystroke and
// the mouse crossing from Bubble Tea's vocabulary into the wire's unchanged.

func TestAKeystrokeCrossesTheWireUnchanged(t *testing.T) {
	// Bubble Tea and the bridge share ultraviolet's vocabulary, so the wire
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
	// A wheel turn crosses as a press of a wheel button, and a release as a
	// release: the action has to survive the crossing.
	wheel := mouseEvent(tea.MouseWheelMsg{X: navWidth + 2, Y: 1, Button: tea.MouseWheelUp}, navWidth+1, 0)
	if wheel == nil || wheel.Action != actPress || wheel.Button != int(tea.MouseWheelUp) {
		t.Errorf("wheel = %+v, want a press of the wheel button", wheel)
	}
	up := mouseEvent(tea.MouseReleaseMsg{X: navWidth + 2, Y: 1, Button: tea.MouseLeft}, navWidth+1, 0)
	if up == nil || up.Action != actRelease {
		t.Errorf("release = %+v, want its action kept", up)
	}
}

func TestThePrefixIsCtrlSpace(t *testing.T) {
	if !isPrefix(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl}) {
		t.Error("ctrl+space should be the prefix")
	}
	if isPrefix(tea.KeyPressMsg{Code: tea.KeySpace}) {
		t.Error("a bare space is not the prefix")
	}
}

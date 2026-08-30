package main

import (
	tea "charm.land/bubbletea/v2"
)

// A shell on the far side of a pty expects the bytes a terminal sends, and
// Bubble Tea has already read those bytes and turned them into a keystroke.
// What it has not kept is what the bytes were, and scrn cannot work them back
// out: an up arrow is "\x1b[A" until the program in the pane asks for
// application cursor keys, at which point it is "\x1bOA", and vim, readline
// and less all ask. The same key is different bytes depending on a mode that
// only the emulator is tracking.
//
// So nothing here writes bytes. A keystroke crosses to the daemon as the
// event it was, and the emulator, which knows what it has been asked for,
// writes the bytes at the other end. Bubble Tea and the emulator share
// ultraviolet's vocabulary — the same codes, the same modifier bits — so
// where this file once translated between two namings of every key, it now
// only restates one of them as the wire's struct.

// keyEvent turns a keystroke into the event the emulator will encode.
func keyEvent(msg tea.KeyPressMsg) *keyPress {
	return &keyPress{Code: msg.Code, Text: msg.Text, Mod: int(msg.Mod)}
}

// isPrefix reports whether a keystroke is scrn's prefix, ctrl+space. The
// terminal sends it as NUL, which ultraviolet reads back as the key it was.
func isPrefix(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeySpace && msg.Mod == tea.ModCtrl
}

// mouseEvent turns a mouse event into one in the pane's own coordinates, or
// nil when it happened somewhere the pane is not.
//
// The buttons cross by number, from the X11 codes every terminal has reported
// since: none, left, middle, right, then the four a wheel has. A wheel turn
// arrives as its own message type but is a press of a wheel button, which is
// how the emulator will report it onward.
func mouseEvent(msg tea.MouseMsg, left, top int) *mousePress {
	mo := msg.Mouse()
	x, y := mo.X-left, mo.Y-top
	if x < 0 || y < 0 {
		return nil
	}

	m := &mousePress{X: x, Y: y, Button: int(mo.Button), Mod: int(mo.Mod), Action: actPress}
	switch msg.(type) {
	case tea.MouseReleaseMsg:
		m.Action = actRelease
	case tea.MouseMotionMsg:
		m.Action = actMotion
	}
	return m
}

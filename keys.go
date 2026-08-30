package main

import (
	tea "github.com/charmbracelet/bubbletea"
	uv "github.com/charmbracelet/ultraviolet"
)

// A shell on the far side of a pty expects the bytes a terminal sends, and
// Bubble Tea has already read those bytes and turned them into a keystroke.
// What it has not kept is what the bytes were, and scrn cannot work them back
// out: an up arrow is "\x1b[A" until the program in the pane asks for
// application cursor keys, at which point it is "\x1bOA", and vim, readline
// and less all ask. The same key is different bytes depending on a mode that
// only the emulator is tracking.
//
// So nothing here writes bytes. It translates one vocabulary of keystrokes
// into another — Bubble Tea's into the emulator's — and the emulator, which
// knows what it has been asked for, writes the bytes at the other end.

// keyCodes are the keys the emulator names, against the way Bubble Tea names
// them. The ones that are missing are the ones that are not a key so much as a
// key with a modifier, and they are put back together below.
var keyCodes = map[tea.KeyType]rune{
	tea.KeyUp:        uv.KeyUp,
	tea.KeyDown:      uv.KeyDown,
	tea.KeyRight:     uv.KeyRight,
	tea.KeyLeft:      uv.KeyLeft,
	tea.KeyHome:      uv.KeyHome,
	tea.KeyEnd:       uv.KeyEnd,
	tea.KeyPgUp:      uv.KeyPgUp,
	tea.KeyPgDown:    uv.KeyPgDown,
	tea.KeyDelete:    uv.KeyDelete,
	tea.KeyInsert:    uv.KeyInsert,
	tea.KeyEnter:     uv.KeyEnter,
	tea.KeyTab:       uv.KeyTab,
	tea.KeyBackspace: uv.KeyBackspace,
	tea.KeyEsc:       uv.KeyEscape,
	tea.KeySpace:     uv.KeySpace,

	tea.KeyF1:  uv.KeyF1,
	tea.KeyF2:  uv.KeyF2,
	tea.KeyF3:  uv.KeyF3,
	tea.KeyF4:  uv.KeyF4,
	tea.KeyF5:  uv.KeyF5,
	tea.KeyF6:  uv.KeyF6,
	tea.KeyF7:  uv.KeyF7,
	tea.KeyF8:  uv.KeyF8,
	tea.KeyF9:  uv.KeyF9,
	tea.KeyF10: uv.KeyF10,
	tea.KeyF11: uv.KeyF11,
	tea.KeyF12: uv.KeyF12,
}

// modified are the keys Bubble Tea reports as a key of their own, which the
// emulator would rather have as an ordinary key held down with something.
var modified = map[tea.KeyType]keyPress{
	tea.KeyShiftTab: {Code: uv.KeyTab, Mod: int(uv.ModShift)},

	tea.KeyCtrlUp:     {Code: uv.KeyUp, Mod: int(uv.ModCtrl)},
	tea.KeyCtrlDown:   {Code: uv.KeyDown, Mod: int(uv.ModCtrl)},
	tea.KeyCtrlRight:  {Code: uv.KeyRight, Mod: int(uv.ModCtrl)},
	tea.KeyCtrlLeft:   {Code: uv.KeyLeft, Mod: int(uv.ModCtrl)},
	tea.KeyCtrlHome:   {Code: uv.KeyHome, Mod: int(uv.ModCtrl)},
	tea.KeyCtrlEnd:    {Code: uv.KeyEnd, Mod: int(uv.ModCtrl)},
	tea.KeyCtrlPgUp:   {Code: uv.KeyPgUp, Mod: int(uv.ModCtrl)},
	tea.KeyCtrlPgDown: {Code: uv.KeyPgDown, Mod: int(uv.ModCtrl)},

	tea.KeyShiftUp:    {Code: uv.KeyUp, Mod: int(uv.ModShift)},
	tea.KeyShiftDown:  {Code: uv.KeyDown, Mod: int(uv.ModShift)},
	tea.KeyShiftRight: {Code: uv.KeyRight, Mod: int(uv.ModShift)},
	tea.KeyShiftLeft:  {Code: uv.KeyLeft, Mod: int(uv.ModShift)},
	tea.KeyShiftHome:  {Code: uv.KeyHome, Mod: int(uv.ModShift)},
	tea.KeyShiftEnd:   {Code: uv.KeyEnd, Mod: int(uv.ModShift)},
}

// keyEvent turns a keystroke into the event the emulator will encode, or nil
// for one it has no idea what to do with.
func keyEvent(msg tea.KeyMsg) *keyPress {
	k := translate(msg)
	if k == nil {
		return nil
	}
	// Alt is a modifier here rather than the escape prefix it becomes on the
	// wire. Which of those it should be is the emulator's business.
	if msg.Alt {
		k.Mod |= int(uv.ModAlt)
	}
	return k
}

func translate(msg tea.KeyMsg) *keyPress {
	if k, ok := modified[msg.Type]; ok {
		return &k
	}
	if code, ok := keyCodes[msg.Type]; ok {
		k := keyPress{Code: code}
		// The keys that type something say so, so that the emulator can tell
		// a space from the idea of a space.
		if msg.Type == tea.KeySpace {
			k.Text = " "
		}
		return &k
	}

	if msg.Type == tea.KeyRunes {
		if len(msg.Runes) == 0 {
			return nil
		}
		return &keyPress{Code: msg.Runes[0], Text: string(msg.Runes)}
	}

	// What is left is the control keys, which Bubble Tea numbers by the byte
	// they stand for: ctrl+a is 1. The emulator wants the letter and the fact
	// that control was down, not the byte — the byte is one of the things it
	// is going to decide for itself.
	if msg.Type >= 1 && msg.Type <= 26 {
		return &keyPress{Code: rune('a' + msg.Type - 1), Mod: int(uv.ModCtrl)}
	}
	return nil
}

// mouseEvent turns a mouse event into one in the pane's own coordinates, or
// nil when it happened somewhere the pane is not.
//
// Bubble Tea and the emulator number the buttons the same way, both from the
// X11 codes every terminal has reported since: none, left, middle, right, then
// the four a wheel has.
func mouseEvent(msg tea.MouseMsg, left, top int) *mousePress {
	x, y := msg.X-left, msg.Y-top
	if x < 0 || y < 0 {
		return nil
	}

	m := &mousePress{X: x, Y: y, Button: int(msg.Button), Action: actPress}
	switch msg.Action {
	case tea.MouseActionRelease:
		m.Action = actRelease
	case tea.MouseActionMotion:
		m.Action = actMotion
	}

	for _, mod := range []struct {
		down bool
		bit  uv.KeyMod
	}{
		{msg.Shift, uv.ModShift},
		{msg.Alt, uv.ModAlt},
		{msg.Ctrl, uv.ModCtrl},
	} {
		if mod.down {
			m.Mod |= int(mod.bit)
		}
	}
	return m
}

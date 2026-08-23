package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// A shell on the far side of a pty expects the bytes a terminal sends, but
// Bubble Tea has already read those bytes and turned them into a keystroke.
// Getting them back is mostly bookkeeping: Bubble Tea numbers the control keys
// by the byte they stand for, so ctrl+c is 3 and enter is 13 and those need no
// table at all. Only the keys that arrive as escape sequences have to be
// written back out as escape sequences.

// escSeq is what a terminal sends for the keys that are not a single byte.
var escSeq = map[tea.KeyType]string{
	tea.KeyUp:    "\x1b[A",
	tea.KeyDown:  "\x1b[B",
	tea.KeyRight: "\x1b[C",
	tea.KeyLeft:  "\x1b[D",

	tea.KeyShiftTab: "\x1b[Z",
	tea.KeyHome:     "\x1b[H",
	tea.KeyEnd:      "\x1b[F",
	tea.KeyPgUp:     "\x1b[5~",
	tea.KeyPgDown:   "\x1b[6~",
	tea.KeyDelete:   "\x1b[3~",
	tea.KeyInsert:   "\x1b[2~",
	tea.KeySpace:    " ",

	tea.KeyCtrlUp:    "\x1b[1;5A",
	tea.KeyCtrlDown:  "\x1b[1;5B",
	tea.KeyCtrlRight: "\x1b[1;5C",
	tea.KeyCtrlLeft:  "\x1b[1;5D",
	tea.KeyCtrlHome:  "\x1b[1;5H",
	tea.KeyCtrlEnd:   "\x1b[1;5F",

	tea.KeyShiftUp:    "\x1b[1;2A",
	tea.KeyShiftDown:  "\x1b[1;2B",
	tea.KeyShiftRight: "\x1b[1;2C",
	tea.KeyShiftLeft:  "\x1b[1;2D",
	tea.KeyShiftHome:  "\x1b[1;2H",
	tea.KeyShiftEnd:   "\x1b[1;2F",
}

// keyBytes turns a keystroke back into what a terminal would have sent for it.
// Alt is the escape prefix, which is what it was on the way in.
func keyBytes(msg tea.KeyMsg) []byte {
	var b []byte
	switch {
	case msg.Type == tea.KeyRunes:
		b = []byte(string(msg.Runes))
	case escSeq[msg.Type] != "":
		b = []byte(escSeq[msg.Type])
	case msg.Type >= 0 && msg.Type <= 127:
		// A control key is numbered by the byte it stands for.
		b = []byte{byte(msg.Type)}
	default:
		return nil
	}

	if msg.Alt {
		return append([]byte{0x1b}, b...)
	}
	return b
}

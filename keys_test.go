package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyBytesRoundTripWhatATerminalSends(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyMsg
		want string
	}{
		{"letters", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ls")}, "ls"},
		{"utf-8 survives", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("é")}, "é"},
		{"enter is a carriage return", tea.KeyMsg{Type: tea.KeyEnter}, "\r"},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}, "\t"},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, "\x7f"},
		{"ctrl+c interrupts", tea.KeyMsg{Type: tea.KeyCtrlC}, "\x03"},
		{"ctrl+d ends input", tea.KeyMsg{Type: tea.KeyCtrlD}, "\x04"},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}, "\x1b"},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, "\x1b[A"},
		{"left", tea.KeyMsg{Type: tea.KeyLeft}, "\x1b[D"},
		{"shift+tab", tea.KeyMsg{Type: tea.KeyShiftTab}, "\x1b[Z"},
		{"delete", tea.KeyMsg{Type: tea.KeyDelete}, "\x1b[3~"},
		{"space", tea.KeyMsg{Type: tea.KeySpace}, " "},
		{"alt is an escape prefix", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b"), Alt: true}, "\x1bb"},
		{"alt on an escape sequence", tea.KeyMsg{Type: tea.KeyUp, Alt: true}, "\x1b\x1b[A"},
	}
	for _, c := range cases {
		if got := string(keyBytes(c.msg)); got != c.want {
			t.Errorf("%s: keyBytes = %q, want %q", c.name, got, c.want)
		}
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

package main

import (
	"errors"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestACaptureBecomesAScreen(t *testing.T) {
	// One command answers with the screen and, on its last line, the facts
	// about the pane; the session turns that into the message the model
	// draws from — the grid padded whole, the flags read as booleans, the
	// title kept with its spaces.
	s := newSession()
	s.closed = true
	s.panes[700] = &pane{id: "%1", pid: 700}
	s.byPane["%1"] = 700
	s.run = func(args ...string) (string, error) {
		if args[0] != "capture-pane" {
			t.Fatalf("asked %v, want one capture", args)
		}
		return "hello\nworld\n2 1 1 1 1 40 10 4 my title", nil
	}

	s.capture("%1")
	msg, ok := (<-s.events).(screenMsg)
	if !ok {
		t.Fatal("no screen came of the capture")
	}
	if msg.pid != 700 || msg.curX != 2 || msg.curY != 1 {
		t.Errorf("msg = %+v, want the cursor at (2,1) on pid 700", msg)
	}
	if !msg.alt || !msg.mouse || msg.sb != 40 || msg.title != "my title" {
		t.Errorf("msg = %+v, want the pane's facts read back", msg)
	}
	rows := strings.Split(msg.screen, "\n")
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want the pane's 4", len(rows))
	}
	for i, row := range rows {
		if lipgloss.Width(row) != 10 {
			t.Errorf("row %d is %d wide, want the pane's 10", i, lipgloss.Width(row))
		}
	}
	if !s.panes[700].sgr {
		t.Error("the SGR flag should be kept for the mouse to read")
	}
}

func TestTheListFallsBackToWhereThePaneWorks(t *testing.T) {
	// A pane scrn never dressed — opened through a bare tmux attach — has no
	// @scrn_dir; where it is working now is the honest answer.
	s := newSession()
	s.closed = true
	s.run = func(args ...string) (string, error) {
		return "%1\t@1\t700\t\t\t/somewhere/else", nil
	}

	s.refreshList()
	msg, ok := (<-s.events).(sessionsMsg)
	if !ok || len(msg.sessions) != 1 {
		t.Fatalf("msg = %+v, want the one shell", msg)
	}
	if msg.sessions[0].Dir != "/somewhere/else" {
		t.Errorf("dir = %q, want the fallback to the working directory", msg.sessions[0].Dir)
	}
}

func TestAColorThatSpansRowsIsRestatedOnEachOne(t *testing.T) {
	// capture-pane says an attribute once and lets it run across lines: a
	// full-width background says nothing at all on the lines after its
	// first. Every row has to stand alone, because scrn cuts, pads and
	// resets them one by one.
	rows := selfContained([]string{
		"\x1b[48;2;20;80;70mrow one",
		"row two",
		"\x1b[mrow three",
		"row four",
	})
	if rows[1] != "\x1b[48;2;20;80;70mrow two" {
		t.Errorf("row two = %q, want the background restated", rows[1])
	}
	if rows[3] != "row four" {
		t.Errorf("row four = %q, want nothing after the reset", rows[3])
	}
}

func TestAResetWithMoreToSayStartsAFreshPen(t *testing.T) {
	rows := selfContained([]string{
		"\x1b[42mgreen \x1b[0;31mthen red",
		"still red",
	})
	if rows[1] != "\x1b[31mstill red" {
		t.Errorf("row two = %q, want only the red carried", rows[1])
	}
}

func TestPartialStylingAccumulatesInThePen(t *testing.T) {
	rows := selfContained([]string{
		"\x1b[1m\x1b[42mbold on green",
		"carries both",
	})
	if rows[1] != "\x1b[1m\x1b[42mcarries both" {
		t.Errorf("row two = %q, want bold and background both restated", rows[1])
	}
}

func TestAListThatCouldNotBeReadLeavesTheShellsStanding(t *testing.T) {
	// A list-panes that failed for any reason but the server being gone — a
	// timeout under load — says nothing about the shells. Reporting them
	// gone would drop the focus and close the reader over a hiccup.
	s := newSession()
	s.closed = true
	s.panes[700] = &pane{id: "%1", pid: 700}
	s.byPane["%1"] = 700
	s.run = func(args ...string) (string, error) { return "", errors.New("signal: killed") }

	if !s.refreshList() {
		t.Error("a shell was held a moment ago, so a session was there to read")
	}
	if n := len(s.events); n != 0 {
		t.Errorf("%d events sent, want none: nothing is known to have changed", n)
	}
	if s.pane(700) == nil {
		t.Error("the shell should still be held")
	}
}

func TestNoServerIsEveryShellGone(t *testing.T) {
	s := newSession()
	s.closed = true
	s.panes[700] = &pane{id: "%1", pid: 700}
	s.byPane["%1"] = 700
	s.run = func(args ...string) (string, error) { return "", errNoServer }

	if s.refreshList() {
		t.Error("no server means no session to read")
	}
	if _, ok := (<-s.events).(termGoneMsg); !ok {
		t.Error("the shell should be reported gone")
	}
	if msg, ok := (<-s.events).(sessionsMsg); !ok || len(msg.sessions) != 0 {
		t.Errorf("msg = %+v, want an empty list", msg)
	}
}

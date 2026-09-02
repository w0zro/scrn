package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

func TestACaptureBecomesAScreen(t *testing.T) {
	// One command answers with the screen and, on its last line, the pane's
	// size; the session turns that into the message the preview draws from,
	// the grid padded whole.
	s := newSession()
	s.closed = true
	s.panes[700] = &pane{id: "%1", pid: 700}
	s.byPane["%1"] = 700
	s.run = func(args ...string) (string, error) {
		if args[0] != "capture-pane" {
			t.Fatalf("asked %v, want one capture", args)
		}
		return "hello\nworld\n10 4", nil
	}

	s.capture("%1")
	msg, ok := (<-s.events).(screenMsg)
	if !ok {
		t.Fatal("no screen came of the capture")
	}
	if msg.pid != 700 {
		t.Errorf("msg = %+v, want the screen for pid 700", msg)
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
}

func TestTheListFallsBackToWhereThePaneWorks(t *testing.T) {
	// A pane scrn never dressed — opened through a bare tmux attach — has no
	// @scrn_dir; where it is working now is the honest answer.
	s := newSession()
	s.closed = true
	s.run = func(args ...string) (string, error) {
		return "%1\t@1\t700\t\t\t/somewhere/else\t", nil
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

func TestAClientThatHangsUpAtOnceIsProbedForAgain(t *testing.T) {
	// The probe finds a session, attaches, and the server is gone by the
	// time the client speaks — %exit at once. The hangup asks for a probe
	// while the probe that attached is still finishing; the two must not
	// leave the session with nobody watching.
	tmuxOnSocket(t) // a socket nothing listens on: every attach hangs up
	s := newSession()
	s.probe = 10 * time.Millisecond
	t.Cleanup(s.close)
	var mu sync.Mutex
	probes := 0
	s.run = func(args ...string) (string, error) {
		if args[0] == "has-session" {
			mu.Lock()
			probes++
			mu.Unlock()
			return "", nil // a session was there when asked
		}
		return "", nil
	}
	go func() {
		for range s.events {
		}
	}()

	s.watchForServer()
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := probes
		mu.Unlock()
		if n >= 3 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("probed %d times; the hangup should have kept the probe going", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestTheSecondFirstShellJoinsTheSessionTheFirstMade(t *testing.T) {
	// Two windows open their first shells at once: both find no session,
	// both try to make one, and tmux refuses the second as a duplicate. The
	// second's shell opens in the session the first made.
	s := newSession()
	s.closed = true
	var asked [][]string
	s.run = func(args ...string) (string, error) {
		asked = append(asked, args)
		switch args[0] {
		case "has-session":
			return "", errNoServer
		case "new-session":
			return "", errors.New("duplicate session: scrn")
		case "new-window":
			return "%1 @1 700", nil
		case "list-panes":
			return "%1\t@1\t700\t/tmp\t\t/tmp\t", nil
		}
		return "", nil
	}

	s.open("/tmp", "", "")
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-s.events:
			if msg, ok := ev.(termOpenedMsg); ok {
				if msg.pid != 700 {
					t.Errorf("pid = %d, want the shell the new-window opened", msg.pid)
				}
				return
			}
			if msg, ok := ev.(serverErrorMsg); ok {
				t.Fatalf("the duplicate should have been retried, not reported: %v", msg.err)
			}
		case <-deadline:
			t.Fatalf("no shell opened; asked %v", asked)
		}
	}
}

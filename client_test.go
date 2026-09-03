package main

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTheListFallsBackToWhereThePaneWorks(t *testing.T) {
	// A pane scrn never dressed — opened through a bare tmux attach — has no
	// @scrn_dir; where it is working now is the honest answer.
	s := newSession()
	s.closed = true
	s.run = func(args ...string) (string, error) {
		return "%1\t700\t\t\t/somewhere/else\t\t\tshell", nil
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

func TestTheListingTellsTheNavigatorAndTheShownShellApart(t *testing.T) {
	// The navigator's pane is not a shell; a pane in the home window beside
	// it is the shell shown there; a window named for wanting is a chord's
	// ask to show the shell in it — unless it is shown already.
	held, nav := parseListing(strings.Join([]string{
		"%0\t100\t\t\t/\t1\t1\tscrn",
		"%1\t700\t/p/a\t\t/p/a\t\t1\tscrn",
		"%2\t701\t/p/b\tweb\t/p/b\t\t\tshell",
		"%3\t702\t/p/c\t\t/p/c\t\t\t" + wantName,
	}, "\n"))
	if nav != "%0" {
		t.Errorf("nav = %q, want the navigator's pane", nav)
	}
	if len(held) != 3 {
		t.Fatalf("held = %d shells, want the three that are not the navigator", len(held))
	}
	if !held[0].shown || held[1].shown || held[2].shown {
		t.Errorf("shown = %v %v %v, want only the pane beside the navigator", held[0].shown, held[1].shown, held[2].shown)
	}
	if held[0].wanted || held[1].wanted || !held[2].wanted {
		t.Errorf("wanted = %v %v %v, want only the window named for it", held[0].wanted, held[1].wanted, held[2].wanted)
	}
}

// idle says whether every ask of a latest has been done.
func (l *latest[T]) idle() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.busy
}

// homeWith is a fake tmux for showPane: a home window holding the navigator
// %0 and, when shown is not empty, that pane beside it. It records the one
// command that moves a pane.
func homeWith(shown string) (runner, *[]string) {
	var moved []string
	run := func(args ...string) (string, error) {
		switch args[0] {
		case "list-panes":
			out := "%0\t1\t120\t29"
			if shown != "" {
				out += "\n" + shown + "\t\t120\t29"
			}
			return out, nil
		case "rename-window":
			// Part of a move: the mover follows after the ;.
			for i, a := range args {
				if a == ";" {
					moved = append(moved, strings.Join(args[i+1:], " "))
					break
				}
			}
		default:
			moved = append(moved, strings.Join(args, " "))
		}
		return "", nil
	}
	return run, &moved
}

func TestShowingAShellJoinsSwapsOrParks(t *testing.T) {
	cases := []struct {
		shown, target string
		want          string
	}{
		// Nothing beside the navigator: the shell joins it, and the layout
		// gives the navigator its column.
		{"", "%5", "join-pane -h -d -l 91 -s %5 -t %0 ; select-layout -t %0 main-vertical"},
		// A shell there already: the two trade places, so the one leaving
		// takes the window the other came from, sized to the slot it left.
		{"%3", "%5", "swap-pane -d -s %5 -t %3 ; resize-window -t %3 -x 91 -y 29"},
		// No shell wanted: the one there goes back to a window of its own,
		// at the slot's size.
		{"%3", "", "break-pane -d -n shell -s %3 ; resize-window -t %3 -x 91 -y 29"},
	}
	for _, c := range cases {
		run, moved := homeWith(c.shown)
		if err := showPane(run, "%0", c.target, 28); err != nil {
			t.Fatal(err)
		}
		if len(*moved) != 1 || (*moved)[0] != c.want {
			t.Errorf("shown %q, target %q: moved %q, want %q", c.shown, c.target, *moved, c.want)
		}
	}
}

func TestShowingTheShellAlreadyShownMovesNothing(t *testing.T) {
	for _, shown := range []string{"", "%3"} {
		run, moved := homeWith(shown)
		if err := showPane(run, "%0", shown, 28); err != nil {
			t.Fatal(err)
		}
		if len(*moved) != 0 {
			t.Errorf("with %q shown and asked for, moved %q, want nothing", shown, *moved)
		}
	}
}

func TestArrangementsCoalesceToTheLastAsked(t *testing.T) {
	// A cursor flying down a column of shells asks for each in turn; while
	// one arrangement is being made the asks pile up, and only the last of
	// them is made after it. The pane ends on the shell the cursor stopped
	// on, having skipped the ones it crossed.
	s := newSession()
	s.closed = true
	s.nav = "%0"
	s.column = 28
	for pid := 1; pid <= 4; pid++ {
		id := "%" + strconv.Itoa(pid)
		s.panes[pid] = &pane{id: id, pid: pid}
		s.byPane[id] = pid
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var moved []string
	first := true
	s.run = func(args ...string) (string, error) {
		if args[0] == "list-panes" {
			if first {
				first = false
				close(started)
				<-release
			}
			return "%0\t1\t120\t29", nil
		}
		mu.Lock()
		moved = append(moved, strings.Join(args, " "))
		mu.Unlock()
		return "", nil
	}

	s.preview(1)
	<-started
	s.preview(2)
	s.preview(3)
	s.preview(4)
	close(release)
	deadline := time.After(time.Second)
	for !s.placing.idle() {
		select {
		case <-deadline:
			t.Fatal("the arrangements never finished")
		case <-time.After(time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(moved) != 2 || !strings.Contains(moved[0], "join-pane -h -d -l 91 -s %1 ") || !strings.Contains(moved[1], "join-pane -h -d -l 91 -s %4 ") {
		t.Errorf("moved %q, want the first ask and then only the last", moved)
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
			return "%1 700", nil
		case "list-panes":
			return "%1\t700\t/tmp\t\t/tmp\t", nil
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

func TestAShellIsToldTheTerminalAroundTmux(t *testing.T) {
	// The server's global environment keeps the launcher's TERM_PROGRAM,
	// and every shell's window carries it, so a program in the pane sees
	// the terminal it is really on rather than the tmux in between.
	var asked [][]string
	run := func(args ...string) (string, error) {
		asked = append(asked, args)
		switch args[0] {
		case "show-environment":
			switch args[2] {
			case "TERM_PROGRAM":
				return "TERM_PROGRAM=ghostty", nil
			case "TERM_PROGRAM_VERSION":
				return "TERM_PROGRAM_VERSION=1.3.1", nil
			}
		case "new-window":
			return "%1 700", nil
		}
		return "", nil
	}
	if _, err := createWindow(run, "/tmp", "", "", false); err != nil {
		t.Fatal(err)
	}
	var opened []string
	for _, a := range asked {
		if a[0] == "new-window" {
			opened = a
		}
	}
	want := []string{"-e", "TERM_PROGRAM=ghostty", "-e", "TERM_PROGRAM_VERSION=1.3.1"}
	if !strings.Contains(" "+strings.Join(opened, " ")+" ", " "+strings.Join(want, " ")+" ") {
		t.Errorf("new-window %v, want it carrying %v", opened, want)
	}
}

func TestAServerInsideAnotherTmuxHasNoTerminalToPassOn(t *testing.T) {
	var opened []string
	run := func(args ...string) (string, error) {
		switch args[0] {
		case "show-environment":
			if args[2] == "TERM_PROGRAM" {
				return "TERM_PROGRAM=tmux", nil
			}
			return "TERM_PROGRAM_VERSION=3.5a", nil
		case "new-window":
			opened = args
			return "%1 700", nil
		}
		return "", nil
	}
	if _, err := createWindow(run, "/tmp", "", "", false); err != nil {
		t.Fatal(err)
	}
	for _, a := range opened {
		if a == "-e" {
			t.Errorf("new-window %v carries an environment, but the terminal around is tmux's own", opened)
		}
	}
}

func TestAGoneSessionIsRemadeAroundAPlaceholderThatGoes(t *testing.T) {
	// The first shell of a remade session opens as a new window, so it
	// carries the terminal's name like every shell after it; the window
	// the session had to start with is gone once the shell is there.
	var asked [][]string
	run := func(args ...string) (string, error) {
		asked = append(asked, args)
		switch args[0] {
		case "has-session":
			return "", errNoServer
		case "show-environment":
			return "TERM_PROGRAM=ghostty", nil
		case "new-window":
			return "%1 700", nil
		}
		return "", nil
	}
	b, err := createWindow(run, "/tmp", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if b.pid != 700 {
		t.Errorf("pid = %d, want the shell the new-window opened", b.pid)
	}
	var order []string
	for _, a := range asked {
		switch a[0] {
		case "new-session", "new-window", "kill-window":
			order = append(order, a[0])
		case "show-environment":
			// Asked once the server is there to answer: before the
			// session, there is no server and nothing to ask.
			if len(order) == 0 || order[len(order)-1] != a[0] {
				order = append(order, a[0])
			}
		}
		if a[0] == "new-session" && a[len(a)-1] != "sleep 30" {
			t.Errorf("new-session %v, want a sleep as the placeholder rather than a shell", a)
		}
		if a[0] == "kill-window" && a[len(a)-1] != tmuxSession+":"+bornName {
			t.Errorf("kill-window %v, want the placeholder", a)
		}
	}
	if got := strings.Join(order, " "); got != "new-session show-environment new-window kill-window" {
		t.Errorf("order = %q, want the session, the terminal asked of it, the shell, then the placeholder gone", got)
	}
}

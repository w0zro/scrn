package main

import (
	"strings"
	"testing"
	"time"
)

func TestTheHomeWindowIsMadeOnceAndFoundAfter(t *testing.T) {
	tmuxOnSocket(t)
	old := homeCommand
	homeCommand = func() string { return "sleep 30" }
	t.Cleanup(func() { homeCommand = old })

	// A session with a shell window and no home, the way one looks after
	// the navigator was closed.
	if _, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "sleep 30"); err != nil {
		t.Fatal(err)
	}

	first, err := ensureHome()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ensureHome()
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("home = %+v then %+v, want the one window found again", first, again)
	}
	out, _ := tmuxCommand("list-windows", "-t", tmuxSession, "-F", "#{window_name}\t#{@scrn_home}")
	if !strings.Contains(out, homeName+"\t1") || strings.Count(out, "\n") != 1 {
		t.Errorf("windows = %q, want the shell and one home window", out)
	}
	out, _ = tmuxCommand("display", "-p", "-t", first.pane, "#{@scrn_nav}")
	if out != "1" {
		t.Errorf("the navigator's pane is not marked as it: @scrn_nav = %q", out)
	}
}

func TestTheNavigatorGoesBackOnTheLeftOfAHomeWindowThatLostIt(t *testing.T) {
	tmuxOnSocket(t)
	old := homeCommand
	homeCommand = func() string { return "sleep 30" }
	t.Cleanup(func() { homeCommand = old })

	// A home window holding only a shell: the navigator was in it and went,
	// and the shell that was beside it has the window.
	out, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "-x", "120", "-y", "30",
		"-n", homeName, "-P", "-F", "#{window_id}\t#{pane_id}", "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Split(out, "\t")
	if _, err := tmuxCommand("set", "-w", "-t", f[0], "@scrn_home", "1", ";",
		"set", "-g", "main-pane-width", "28"); err != nil {
		t.Fatal(err)
	}

	h, err := ensureHome()
	if err != nil {
		t.Fatal(err)
	}
	if h.win != f[0] {
		t.Errorf("home = %+v, want the navigator back in window %s", h, f[0])
	}
	out, _ = tmuxCommand("list-panes", "-t", f[0], "-F", "#{pane_id} #{pane_left} #{pane_width} #{@scrn_nav}")
	if !strings.Contains(out, h.pane+" 0 28 1") || !strings.Contains(out, f[1]+" 29 ") {
		t.Errorf("panes:\n%s\nwant the navigator 28 wide on the left and the shell beside it", out)
	}
}

func TestAChordPressesItsKeyAtTheNavigator(t *testing.T) {
	// The chords for what only the navigator knows — the next shell, the
	// next waiting agent — press a key at it, making it first if the home
	// window was closed. The key lands in the navigator's pane: an inert
	// program there lets the terminal echo it.
	tmuxOnSocket(t)
	old := homeCommand
	homeCommand = func() string { return "sleep 30" }
	t.Cleanup(func() { homeCommand = old })
	if _, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "sleep 30"); err != nil {
		t.Fatal(err)
	}

	if err := runStep(1); err != nil {
		t.Fatal(err)
	}
	h, err := ensureHome()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		out, _ := tmuxCommand("capture-pane", "-p", "-t", h.pane)
		if strings.Contains(out, "J") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the navigator's pane shows %q, want the J the chord pressed", out)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

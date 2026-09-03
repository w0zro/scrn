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

func TestAnOlderBuildsNavigatorIsAdoptedNotDoubled(t *testing.T) {
	// A server an older build started has a home window whose navigator
	// pane wears only the window's mark. It is known by what it runs, and
	// marked, rather than a second navigator being split in beside it.
	tmuxOnSocket(t)
	old := homeCommand
	homeCommand = func() string { return "sh -c 'sleep 30' nav" }
	t.Cleanup(func() { homeCommand = old })
	out, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "-n", homeName,
		"-P", "-F", "#{window_id}\t#{pane_id}", homeCommand())
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Split(out, "\t")
	if _, err := tmuxCommand("set", "-w", "-t", f[0], "@scrn_home", "1"); err != nil {
		t.Fatal(err)
	}

	h, err := ensureHome()
	if err != nil {
		t.Fatal(err)
	}
	if h.pane != f[1] {
		t.Errorf("home = %+v, want the navigator already there, %s", h, f[1])
	}
	out, _ = tmuxCommand("list-panes", "-s", "-t", tmuxSession, "-F", "#{pane_id} #{@scrn_nav}")
	if out != f[1]+" 1" {
		t.Errorf("panes = %q, want the one navigator, marked", out)
	}
}

func TestASecondNavigatorIsALeftoverAndGoes(t *testing.T) {
	tmuxOnSocket(t)
	old := homeCommand
	homeCommand = func() string { return "sh -c 'sleep 30' nav" }
	t.Cleanup(func() { homeCommand = old })
	out, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "-n", homeName,
		"-P", "-F", "#{window_id}\t#{pane_id}", homeCommand())
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Split(out, "\t")
	stray := strings.TrimSpace(f[1])
	// The marked navigator split in beside the older one, as happened.
	out, err = tmuxCommand("split-window", "-h", "-b", "-d", "-P", "-F", "#{pane_id}", "-t", f[0], homeCommand())
	if err != nil {
		t.Fatal(err)
	}
	markHome(f[0], out)

	h, err := ensureHome()
	if err != nil {
		t.Fatal(err)
	}
	if h.pane != out {
		t.Errorf("home = %+v, want the marked navigator %s", h, out)
	}
	panes, _ := tmuxCommand("list-panes", "-s", "-t", tmuxSession, "-F", "#{pane_id}")
	if strings.Contains(panes, stray) || panes != out {
		t.Errorf("panes = %q, want only the marked navigator; the stray %s should be gone", panes, stray)
	}
}

func TestALaunchRestartsANavigatorFromAnOlderBuild(t *testing.T) {
	// The navigator a launch finds may be an older build's; it is
	// replaced in its pane, and one that is this build's is left alone.
	tmuxOnSocket(t)
	old := homeCommand
	t.Cleanup(func() { homeCommand = old })
	homeCommand = func() string { return "sh -c 'sleep 30' nav" }
	out, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "-n", homeName,
		"-P", "-F", "#{window_id}\t#{pane_id}", homeCommand())
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Split(out, "\t")
	markHome(f[0], f[1])
	h, err := ensureHome()
	if err != nil {
		t.Fatal(err)
	}
	was, _ := tmuxCommand("display", "-p", "-t", h.pane, "#{pane_pid}")

	if err := refreshHome(h); err != nil {
		t.Fatal(err)
	}
	if now, _ := tmuxCommand("display", "-p", "-t", h.pane, "#{pane_pid}"); now != was {
		t.Errorf("a navigator that is this build's was restarted: pid %s, was %s", now, was)
	}

	// A newer build: the pane keeps its place, the program in it is new.
	homeCommand = func() string { return "sh -c 'sleep 31' nav" }
	if err := refreshHome(h); err != nil {
		t.Fatal(err)
	}
	now, _ := tmuxCommand("display", "-p", "-t", h.pane, "#{pane_pid}\t#{pane_start_command}\t#{@scrn_nav}")
	g := strings.Split(now, "\t")
	if len(g) != 3 || g[0] == was || !strings.Contains(g[1], "sleep 31") || g[2] != "1" {
		t.Errorf("after the newer build's launch the pane is %q, want a new process running the new command in the marked pane", now)
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

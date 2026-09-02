package main

import (
	"strings"
	"testing"
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
}

func TestJumpGoesToTheNextMarkedWindowAndWraps(t *testing.T) {
	tmuxOnSocket(t)
	old := homeCommand
	homeCommand = func() string { return "sleep 30" }
	t.Cleanup(func() { homeCommand = old })

	// Four windows: the first current, one unmarked, one blocked on a
	// question, one done and waiting.
	if _, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "sleep 30"); err != nil {
		t.Fatal(err)
	}
	for _, mark := range []string{"", glyphAsk, glyphOn} {
		if _, err := tmuxCommand("new-window", "-d", "-t", tmuxSession+":", "sleep 30", ";",
			"set", "-w", "-t", tmuxSession+":$", "@scrn_mark", mark); err != nil {
			t.Fatal(err)
		}
	}
	active := func() string {
		out, _ := tmuxCommand("display", "-p", "-t", tmuxSession+":", "#{window_index} #{@scrn_mark}")
		return out
	}

	want := []string{"2 " + glyphAsk, "3 " + glyphOn, "2 " + glyphAsk}
	for i, w := range want {
		if err := runJump(); err != nil {
			t.Fatal(err)
		}
		if got := active(); got != w {
			t.Errorf("jump %d landed on %q, want %q", i+1, got, w)
		}
	}
}

func TestJumpWithNothingMarkedGoesHome(t *testing.T) {
	tmuxOnSocket(t)
	old := homeCommand
	homeCommand = func() string { return "sleep 30" }
	t.Cleanup(func() { homeCommand = old })
	if _, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "sleep 30"); err != nil {
		t.Fatal(err)
	}

	if err := runJump(); err != nil {
		t.Fatal(err)
	}
	out, _ := tmuxCommand("display", "-p", "-t", tmuxSession+":", "#{@scrn_home}")
	if out != "1" {
		t.Errorf("with nothing marked the jump should end at the navigator; the current window is home=%q", out)
	}
}

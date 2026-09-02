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

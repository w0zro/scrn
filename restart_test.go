package main

import (
	"errors"
	"strings"
	"testing"
)

func TestRestartEndsTheServerAfterAsking(t *testing.T) {
	tmuxOnSocket(t)
	if _, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "cat"); err != nil {
		t.Fatal(err)
	}

	asked := -1
	err := endServer(func(held int) bool { asked = held; return true })
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("asked about %d shells, want the 1 the server held", asked)
	}
	if _, err := tmuxCommand("has-session", "-t", tmuxSession); !errors.Is(err, errNoServer) {
		t.Errorf("after ending, has-session = %v, want no server", err)
	}
}

func TestRestartKeepsTheServerWhenToldNo(t *testing.T) {
	tmuxOnSocket(t)
	if _, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "cat"); err != nil {
		t.Fatal(err)
	}

	err := endServer(func(int) bool { return false })
	if !errors.Is(err, errKept) {
		t.Fatalf("endServer = %v, want errKept", err)
	}
	if _, err := tmuxCommand("has-session", "-t", tmuxSession); err != nil {
		t.Errorf("after a no, has-session = %v, want the server still there", err)
	}
}

func TestRestartWithoutAServerAsksNothing(t *testing.T) {
	// Nothing held is nothing to ask about, and nothing to end: the launch
	// that follows starts the server the way any first launch does.
	tmuxOnSocket(t)

	err := endServer(func(int) bool { t.Fatal("asked with no server to end"); return false })
	if err != nil {
		t.Fatal(err)
	}
}

func TestTheQuestionTakesYesInAnyCase(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "Yes\n": true, "\n": false, "n\n": false, "": false} {
		var out strings.Builder
		got := askTerminal(strings.NewReader(in), &out)(3)
		if got != want {
			t.Errorf("answer %q = %v, want %v", in, got, want)
		}
		if !strings.Contains(out.String(), "the 3 shells it holds") {
			t.Errorf("question = %q, want it to count the shells", out.String())
		}
	}
	var out strings.Builder
	askTerminal(strings.NewReader("n\n"), &out)(1)
	if !strings.Contains(out.String(), "the 1 shell it holds") {
		t.Errorf("question = %q, want the singular", out.String())
	}
}

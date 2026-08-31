package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLsPrintsWhatTheServerHolds(t *testing.T) {
	m := connected(t, repoModel())
	m.daemon.open("/tmp", "cat", "web", 60, 12)
	m = pump(t, m, func(m model) bool { return len(m.terms) == 1 }, 10*time.Second)

	var pid int
	for p := range m.terms {
		pid = p
	}

	var out strings.Builder
	if err := runLS(&out); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d\t/tmp\tweb\n", pid)
	if out.String() != want {
		t.Errorf("ls = %q, want %q", out.String(), want)
	}
}

func TestLsWithoutAServerPrintsNothing(t *testing.T) {
	// No server means nothing is held: an empty list, not an error, and
	// certainly not a server started just to say so.
	tmuxOnSocket(t)

	var out strings.Builder
	if err := runLS(&out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("ls with no server = %q, want nothing", out.String())
	}
}

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLsPrintsWhatTheDaemonHolds(t *testing.T) {
	startDaemonFor(t)

	c, err := dialDaemon()
	if err != nil {
		t.Fatal(err)
	}
	conn := newConn(c)
	defer conn.close()
	conn.write(message{Kind: kindOpen, Dir: "/tmp", Width: 40, Height: 8})

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 {
		m, err := conn.readBy(deadline)
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind == kindOpened {
			pid = m.PID
		}
	}

	var out strings.Builder
	if err := runLS(&out); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d\t/tmp\t\n", pid)
	if out.String() != want {
		t.Errorf("ls = %q, want %q", out.String(), want)
	}
}

func TestLsWithoutADaemonPrintsNothing(t *testing.T) {
	// No daemon means nothing is held: an empty list, not an error, and
	// certainly not a daemon started just to say so.
	t.Setenv("SCRN_SOCKET", filepath.Join(t.TempDir(), "none.sock"))

	var out strings.Builder
	if err := runLS(&out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("ls with no daemon = %q, want nothing", out.String())
	}
}

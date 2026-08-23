package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitFor polls until cond holds, so tests do not race the daemon's goroutines.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitScreen reads from a raw connection until a shell's screen contains text.
func awaitScreen(t *testing.T, c *conn, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, err := c.readBy(deadline)
		if err != nil {
			t.Fatalf("reading from the daemon: %v", err)
		}
		if m.Kind == kindScreen && strings.Contains(m.Screen, want) {
			return m.Screen
		}
	}
	t.Fatalf("never saw %q on screen", want)
	return ""
}

func TestAShellOutlivesTheWindowThatOpenedIt(t *testing.T) {
	// This is the whole reason the daemon exists: closing a window detaches
	// from the work rather than ending it.
	d := startDaemonFor(t)

	first, err := dialDaemon()
	if err != nil {
		t.Fatal(err)
	}
	c1 := newConn(first)
	c1.write(message{Kind: kindOpen, Dir: "/tmp", Width: 40, Height: 8})

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 {
		m, err := c1.readBy(deadline)
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind == kindOpened {
			pid = m.PID
		}
	}

	// Leave a mark in the shell that a later window can look for.
	c1.write(message{Kind: kindInput, PID: pid, Data: []byte("PS1=; echo marker-survives\n")})
	awaitScreen(t, c1, "marker-survives")

	// The window goes away. The shell should not.
	c1.close()
	waitFor(t, "the client to be forgotten", func() bool { return len(d.allClients()) == 0 })

	if d.session(pid) == nil {
		t.Fatal("the shell died with the window that opened it")
	}

	// A new window attaches and finds the shell as it was left.
	second, err := dialDaemon()
	if err != nil {
		t.Fatal(err)
	}
	c2 := newConn(second)
	defer c2.close()

	c2.write(message{Kind: kindAttach, PID: pid, Width: 40, Height: 8})
	screen := awaitScreen(t, c2, "marker-survives")
	if !strings.Contains(screen, "marker-survives") {
		t.Errorf("reattached screen = %q, want what the first window left", screen)
	}
}

func TestASecondWindowSeesTheShellsAlreadyHeld(t *testing.T) {
	d := startDaemonFor(t)

	c1raw, _ := dialDaemon()
	c1 := newConn(c1raw)
	c1.write(message{Kind: kindOpen, Dir: "/tmp", Width: 40, Height: 8})
	waitFor(t, "the shell to open", func() bool { return len(d.list()) == 1 })
	c1.close()

	c2raw, err := dialDaemon()
	if err != nil {
		t.Fatal(err)
	}
	c2 := newConn(c2raw)
	defer c2.close()

	c2.write(message{Kind: kindList})
	deadline := time.Now().Add(5 * time.Second)
	for {
		m, err := c2.readBy(deadline)
		if err != nil {
			t.Fatal(err)
		}
		if m.Kind != kindSessions {
			continue
		}
		if len(m.Sessions) != 1 || m.Sessions[0].Dir != "/tmp" {
			t.Errorf("sessions = %+v, want the shell the first window left", m.Sessions)
		}
		return
	}
}

func TestOnlyWatchersAreSentAScreen(t *testing.T) {
	// A window that is not looking at a shell should not be woken by it, or
	// every window pays for every shell running anywhere.
	d := startDaemonFor(t)

	c1raw, _ := dialDaemon()
	c1 := newConn(c1raw)
	defer c1.close()
	c1.write(message{Kind: kindOpen, Dir: "/tmp", Width: 40, Height: 8})
	waitFor(t, "the shell to open", func() bool { return len(d.list()) == 1 })
	pid := d.list()[0].PID

	c2raw, _ := dialDaemon()
	c2 := newConn(c2raw)
	defer c2.close()
	waitFor(t, "the second window", func() bool { return len(d.allClients()) == 2 })

	if got := len(d.watchers(pid)); got != 1 {
		t.Errorf("watchers = %d, want only the window that opened it", got)
	}
}

func TestASecondDaemonStandsDown(t *testing.T) {
	// Two daemons on one socket would each hold half the shells.
	startDaemonFor(t)
	if _, err := listenDaemon(socketPath()); err == nil {
		t.Error("a second daemon claimed a socket that was already answered")
	}
}

func TestAStaleSocketIsCleared(t *testing.T) {
	// A daemon that was killed leaves its socket behind. Nothing answers it,
	// so the next daemon should take the path rather than refuse to start.
	dir, err := os.MkdirTemp("/tmp", "scrnd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "d.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := listenDaemon(path)
	if err != nil {
		t.Fatalf("a stale socket should not stop a daemon: %v", err)
	}
	d.stop()
}

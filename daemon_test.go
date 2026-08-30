package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
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

// typeInto types a string at a shell one keystroke at a time, which is how
// input crosses now: the bytes are the emulator's to decide, so a test cannot
// hand over a string of them either.
func typeInto(c *conn, pid int, s string) {
	for _, r := range s {
		k := &keyPress{Code: r, Text: string(r)}
		if r == '\n' {
			k = &keyPress{Code: uv.KeyEnter}
		}
		c.write(message{Kind: kindInput, PID: pid, Key: k})
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
	typeInto(c1, pid, "PS1=; echo marker-survives\n")
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

func TestClosingAShellTakesWhatItStartedWithIt(t *testing.T) {
	// A plan entry is a shell running something, and it is the something that
	// is the point of it. The hangup goes to the process group so that what
	// the shell started hears it too, rather than being left behind holding
	// the port the entry was started for.
	term, err := startTerm("/tmp", "sleep 41", "web", 40, 8)
	if err != nil {
		t.Fatal(err)
	}

	var inner int
	waitFor(t, "the shell to start what it was given", func() bool {
		inner = pidOf(t, "sleep 41")
		return inner != 0
	})

	term.close()
	waitFor(t, "the sleep to go with the shell that started it", func() bool {
		return !alive(inner)
	})
}

// pidOf finds a process by the command line it was run with, so a test can ask
// after something a shell started rather than the shell itself.
func pidOf(t *testing.T, command string) int {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// The shell's own "-c sleep 41; exec ..." names it too, and is not it.
		if !strings.HasSuffix(line, command) {
			continue
		}
		if pid, err := strconv.Atoi(strings.Fields(line)[0]); err == nil {
			return pid
		}
	}
	return 0
}

// alive reports whether a pid is still there. Signal 0 asks without sending.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestAnInteractiveShellIsHungUpRatherThanSignalled(t *testing.T) {
	// zsh and bash ignore SIGTERM when interactive, by design. Ending one
	// means taking its terminal away, which is the message the hangup carries.
	t.Setenv("SHELL", "/bin/zsh")
	term, err := startTerm("/tmp", "", "", 40, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer term.close()

	waitFor(t, "the shell to draw a prompt", func() bool {
		return strings.Contains(term.vt.Render(), "%") || strings.Contains(term.vt.Render(), "$")
	})

	// A signal first: it should be ignored, which is the whole problem.
	if err := signal(term.pid); err != nil {
		t.Fatalf("signalling: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	select {
	case <-term.done:
		t.Skip("this shell does exit on SIGTERM; the hangup path is still what scrn uses")
	default:
	}

	// Now hang it up. It has to go on the hangup rather than on the kill that
	// backs it up, so the wait is well inside the grace: a shell that only
	// goes once the grace has run out has not heard the hangup at all, which
	// is what closing the pty without sending one looked like.
	go term.close()
	select {
	case <-term.done:
	case <-time.After(hangupGrace / 2):
		t.Fatal("the shell survived the hangup and had to be killed")
	}
}

func TestClosingAShellTwiceIsHarmless(t *testing.T) {
	// A shell ended by hand is torn down again when its output stops.
	t.Setenv("SHELL", "/bin/sh")
	term, err := startTerm("/tmp", "", "", 40, 8)
	if err != nil {
		t.Fatal(err)
	}
	term.close()
	term.close()

	select {
	case <-term.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell did not go")
	}
}

func TestClosingThroughTheDaemonEndsTheShell(t *testing.T) {
	d := startDaemonFor(t)
	t.Setenv("SHELL", "/bin/zsh")

	c, err := dialDaemon()
	if err != nil {
		t.Fatal(err)
	}
	conn := newConn(c)
	defer conn.close()

	conn.write(message{Kind: kindOpen, Dir: "/tmp", Width: 40, Height: 8})
	waitFor(t, "the shell to open", func() bool { return len(d.list()) == 1 })
	pid := d.list()[0].PID

	conn.write(message{Kind: kindClose, PID: pid})
	waitFor(t, "the shell to go", func() bool { return d.session(pid) == nil })
}

func TestADaemonSaysWhenItStarted(t *testing.T) {
	// A client cannot tell a daemon older than its own build from a current
	// one without asking, and the difference is invisible until something it
	// holds behaves the way it used to.
	startDaemonFor(t)

	c, err := dialDaemon()
	if err != nil {
		t.Fatal(err)
	}
	conn := newConn(c)
	defer conn.close()

	conn.write(message{Kind: kindList})
	m, err := conn.readBy(time.Now().Add(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if m.Since == 0 {
		t.Fatalf("sessions = %+v, want it to say when the daemon started", m)
	}
	if since := time.UnixMilli(m.Since); time.Since(since) > time.Minute {
		t.Errorf("started = %v, want roughly now", since)
	}
}

func TestADaemonHoldingNothingStandsDown(t *testing.T) {
	d := startDaemonFor(t)

	c, _ := dialDaemon()
	conn := newConn(c)
	defer conn.close()

	conn.write(message{Kind: kindStand})
	waitFor(t, "the daemon to stop listening", func() bool {
		_, err := dialDaemon()
		return err != nil
	})
	_ = d
}

func TestADaemonHoldingShellsStaysUp(t *testing.T) {
	// The work in it is the reason it exists; a newer build does not outrank
	// a running shell.
	d := startDaemonFor(t)

	c, _ := dialDaemon()
	conn := newConn(c)
	defer conn.close()

	conn.write(message{Kind: kindOpen, Dir: "/tmp", Width: 40, Height: 8})
	waitFor(t, "the shell to open", func() bool { return len(d.list()) == 1 })

	conn.write(message{Kind: kindStand})
	time.Sleep(500 * time.Millisecond)

	if _, err := dialDaemon(); err != nil {
		t.Error("a daemon holding a shell should not stand down")
	}
}

func TestStaleIsAboutTheBuildNotTheClock(t *testing.T) {
	built := builtAt()
	if built.IsZero() {
		t.Skip("cannot tell when this binary was built")
	}
	if !stale(built.Add(-time.Hour)) {
		t.Error("a daemon that started before this build is stale")
	}
	if stale(built.Add(time.Hour)) {
		t.Error("a daemon started after this build is not stale")
	}
	if stale(time.Time{}) {
		t.Error("a daemon that did not say should not be called stale")
	}
}

func TestAToldDaemonGoesAndTakesItsShellsWithIt(t *testing.T) {
	// Being asked twice is the difference between "if you can" and "and take
	// what you are holding with you".
	d := startDaemonFor(t)

	c, _ := dialDaemon()
	conn := newConn(c)
	defer conn.close()

	conn.write(message{Kind: kindOpen, Dir: "/tmp", Width: 40, Height: 8})
	waitFor(t, "the shell to open", func() bool { return len(d.list()) == 1 })
	pid := d.list()[0].PID

	conn.write(message{Kind: kindStand, Force: true})
	waitFor(t, "the daemon to stop listening", func() bool {
		_, err := dialDaemon()
		return err != nil
	})
	waitFor(t, "the shell to go with it", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}

func TestTheTranscriptCrossesTheWireWhenAsked(t *testing.T) {
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

	// Enough lines that the first has scrolled off an 8-row pane.
	typeInto(conn, pid, "PS1=; for i in $(seq 1 20); do echo mark-$i; done\n")
	awaitScreen(t, conn, "mark-20")

	conn.write(message{Kind: kindHistory, PID: pid})
	for {
		m, err := conn.readBy(deadline)
		if err != nil {
			t.Fatalf("waiting for the transcript: %v", err)
		}
		if m.Kind != kindHistory {
			continue // screens keep arriving; they are not what was asked
		}
		if !strings.Contains(m.History, "mark-1") {
			t.Errorf("history = %q, want the lines that scrolled away", m.History)
		}
		return
	}
}

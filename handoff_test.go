package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// TestMain lets this binary answer as the daemon. An upgrading daemon execs
// the binary the ask names, or its own when the ask names none — which, under
// test, is the test binary either way — so the upgrade test exercises the
// real exec rather than a stand-in.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		if err := runDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestAHandoffCarriesTheScreenTheTranscriptAndTheModes(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	term, err := startTerm("/tmp", "", "", 40, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer term.close()

	// Leave marks in the pane and the scrollback, and have the program in the
	// shell turn a mode on, the way vim turns on application cursor keys.
	for _, r := range "PS1=; for i in $(seq 1 20); do echo mark-$i; done; printf '\\033[?1h'\n" {
		k := &keyPress{Code: r, Text: string(r)}
		if r == '\n' {
			k = &keyPress{Code: uv.KeyEnter}
		}
		term.send(message{Key: k})
	}
	waitFor(t, "the marks to be drawn", func() bool {
		return strings.Contains(term.vt.Render(), "mark-20")
	})
	waitFor(t, "the mode to be recorded", func() bool {
		term.modeMu.Lock()
		defer term.modeMu.Unlock()
		return term.modes[ansi.ModeCursorKeys]
	})

	h := term.handoff()
	if !h.Modes[int(ansi.ModeCursorKeys)] {
		t.Error("the mode the program set did not make it into the handoff")
	}

	// The replay should turn a fresh emulator into the one written down.
	emu := vt.NewSafeEmulator(h.Cols, h.Rows)
	if _, err := emu.Write(h.replay()); err != nil {
		t.Fatal(err)
	}

	want := strings.Split(h.Screen, "\n")
	got := strings.Split(emu.Render(), "\n")
	if len(got) != len(want) {
		t.Fatalf("replayed %d rows, wrote down %d", len(got), len(want))
	}
	for i := range want {
		w := strings.TrimRight(ansi.Strip(want[i]), " ")
		g := strings.TrimRight(ansi.Strip(got[i]), " ")
		if w != g {
			t.Errorf("row %d = %q, want %q", i, g, w)
		}
	}
	if p := emu.CursorPosition(); p.X != h.CursorX || p.Y != h.CursorY {
		t.Errorf("cursor = %d,%d, want %d,%d", p.X, p.Y, h.CursorX, h.CursorY)
	}

	// The lines that scrolled off the pane are in the replayed scrollback.
	sb := emu.Scrollback()
	var lines []string
	for i := 0; i < sb.Len(); i++ {
		lines = append(lines, sb.Line(i).Render())
	}
	if !strings.Contains(strings.Join(lines, "\n"), "mark-1") {
		t.Error("the transcript did not survive the replay")
	}
}

func TestAnUpgradedDaemonKeepsItsShells(t *testing.T) {
	// The whole point of the handoff: replacing the daemon — a real exec of a
	// new image over the old — loses neither the shell nor its screen.
	dir, err := os.MkdirTemp("/tmp", "scrnd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("SCRN_SOCKET", filepath.Join(dir, "d.sock"))
	t.Setenv("SHELL", "/bin/sh")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command(exe, "daemon")
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, err := dialDaemon(); err == nil {
			cc := newConn(c)
			cc.write(message{Kind: kindStand, Force: true})
			cc.close()
		}
		_ = daemon.Process.Kill()
		_, _ = daemon.Process.Wait()
	})

	c, err := waitForDaemon(daemonStartWait)
	if err != nil {
		t.Fatal(err)
	}
	conn := newConn(c)
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
	typeInto(conn, pid, "PS1=; echo survives-the-exec\n")
	awaitScreen(t, conn, "survives-the-exec")

	// Ask for the upgrade, naming the binary the way a real window does. The
	// exec drops this connection, which is what acting on it looks like from
	// here.
	conn.write(message{Kind: kindUpgrade, Exe: exe})
	for {
		if _, err := conn.readBy(deadline); err != nil {
			break
		}
	}
	conn.close()

	// The same socket answers — the listener crossed the exec — holding the
	// same shell, with the screen as the old image last showed it.
	c2, err := waitForDaemon(daemonStartWait)
	if err != nil {
		t.Fatalf("nothing took over the socket: %v", err)
	}
	conn2 := newConn(c2)
	defer conn2.close()

	conn2.write(message{Kind: kindList})
	for {
		m, err := conn2.readBy(deadline)
		if err != nil {
			t.Fatalf("waiting for the sessions: %v", err)
		}
		if m.Kind != kindSessions {
			continue
		}
		if len(m.Sessions) != 1 || m.Sessions[0].PID != pid {
			t.Fatalf("sessions = %+v, want the shell that crossed the exec (pid %d)", m.Sessions, pid)
		}
		break
	}

	conn2.write(message{Kind: kindAttach, PID: pid, Width: 40, Height: 8})
	awaitScreen(t, conn2, "survives-the-exec")

	// And the adopted pty still carries keystrokes both ways.
	typeInto(conn2, pid, "echo still-answers\n")
	awaitScreen(t, conn2, "still-answers")
}

func TestAStaleDaemonHoldingShellsIsAskedToUpgrade(t *testing.T) {
	// Replacing used to be the only offer, and it ends the work. The first
	// move now is the one that keeps it.
	ours, theirs := net.Pipe()
	defer ours.Close()
	defer theirs.Close()

	asked := make(chan message, 1)
	go func() {
		if m, err := newConn(ours).read(); err == nil {
			asked <- m
		}
	}()

	m := newModel()
	m.daemon = &session{conn: newConn(theirs), events: make(chan tea.Msg, 4)}
	held := []sessionInfo{{PID: 41, Dir: "/tmp"}}

	next, _ := m.Update(sessionsMsg{sessions: held, since: time.Now().Add(-24 * time.Hour)})
	m = next.(model)

	select {
	case got := <-asked:
		if got.Kind != kindUpgrade {
			t.Errorf("asked %q of the daemon, want %q", got.Kind, kindUpgrade)
		}
		if got.Exe == "" {
			t.Error("the ask does not say which binary to exec; the daemon's own may be gone")
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was asked of the stale daemon")
	}
	if !m.upgradeAsked {
		t.Error("the model does not remember asking")
	}

	// Asked once. A daemon still stale after that is one that cannot take the
	// upgrade, and the second word is the offer that costs the shells.
	next, _ = m.Update(sessionsMsg{sessions: held, since: time.Now().Add(-24 * time.Hour)})
	m = next.(model)
	if !strings.Contains(m.status, "R replaces it") {
		t.Errorf("status = %q, want the R fallback", m.status)
	}
}

func TestTheDaemonsOwnUpgradeErrorOutlivesTheLimboMessage(t *testing.T) {
	// An exec that returns comes with a reason, and the limbo message used to
	// pave over it with a guess. The reason stays; only the offer is added.
	m := newModel()
	m.daemon = &session{}
	m.upgradeAsked, m.daemonStale = true, true
	m.terms[41] = &remoteTerm{pid: 41}

	next, _ := m.Update(daemonErrorMsg{err: errors.New("upgrade: no such file or directory")})
	m = next.(model)
	next, _ = m.Update(upgradeLimboMsg{})
	m = next.(model)

	if !strings.Contains(m.status, "no such file or directory") {
		t.Errorf("status = %q, want the daemon's own reason kept", m.status)
	}
	if !strings.Contains(m.status, "R replaces it") {
		t.Errorf("status = %q, want the R offer alongside the reason", m.status)
	}
}

func TestTheWindowComesStraightBackAfterAnUpgrade(t *testing.T) {
	// An upgrading daemon drops every connection on its way through the exec.
	// That is it working, so the window reconnects rather than reporting it.
	m := newModel()
	m.upgradeAsked = true

	next, cmd := m.Update(daemonLostMsg{err: errors.New("EOF")})
	m = next.(model)

	if m.daemonErr != "" {
		t.Errorf("daemonErr = %q, want the drop taken as the upgrade working", m.daemonErr)
	}
	if cmd == nil {
		t.Error("nothing scheduled the reconnect")
	}
}

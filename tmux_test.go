package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotesAreReadFromTheStream(t *testing.T) {
	cases := []struct {
		line string
		want ctlNote
	}{
		{"%output %3 hi there", ctlNote{}},
		{"%window-close @2", ctlNote{kind: noteWindows}},
		{"%unlinked-window-close @2", ctlNote{kind: noteWindows}},
		{"%window-add @4", ctlNote{kind: noteWindows}},
		{"%exit", ctlNote{kind: noteExit}},
		{"%exit detached", ctlNote{kind: noteExit}},
		{"%begin 123 4 1", ctlNote{}},
		{"%layout-change @1 whatever", ctlNote{kind: noteWindows}},
		{"just some pane text", ctlNote{}},
	}
	for _, c := range cases {
		if got := parseNote(c.line); got != c.want {
			t.Errorf("parseNote(%q) = %+v, want %+v", c.line, got, c.want)
		}
	}
}

// tmuxOnSocket points scrn at a private tmux server for one test, and tears
// the server down after. Skips when tmux is not installed. The socket lives
// under /tmp rather than the test's own directory: sun_path holds 104 bytes,
// and a test name is most of that on its own.
func tmuxOnSocket(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "scrn-tmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("SCRN_SOCKET", filepath.Join(dir, "t.sock"))
	t.Cleanup(func() { _, _ = tmuxCommand("kill-server") })
}

func TestTheBridgeSpeaksToARealServer(t *testing.T) {
	tmuxOnSocket(t)

	// A session with one window running an inert command, so the test does
	// not depend on anyone's shell.
	out, err := tmuxCommand("new-session", "-d", "-s", tmuxSession, "-x", "50", "-y", "8",
		"-P", "-F", "#{pane_id} #{pane_pid}", "cat")
	if err != nil {
		t.Fatal(err)
	}
	pane, _, ok := strings.Cut(out, " ")
	if !ok {
		t.Fatalf("new-session said %q, want a pane and a pid", out)
	}

	notes := make(chan ctlNote, 64)
	ctl, err := startCtl(func(n ctlNote) { notes <- n })
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.close()

	// A window opened is the whole round trip: command, server, control
	// stream, note — and the pane it holds is there to be listed.
	if _, err := tmuxCommand("new-window", "-d", "-t", tmuxSession+":", "cat"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case n := <-notes:
			if n.kind != noteWindows {
				continue
			}
			out, err := tmuxCommand("list-panes", "-a", "-F", "#{pane_id}")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, pane) && strings.Count(out, "\n") == 1 {
				return
			}
		case <-deadline:
			t.Fatal("the window's opening never crossed the stream")
		}
	}
}

func TestAnAbsentServerIsAnEmptyAnswer(t *testing.T) {
	tmuxOnSocket(t)
	_, err := tmuxCommand("list-panes", "-a")
	if err != errNoServer {
		t.Fatalf("err = %v, want errNoServer", err)
	}
}

func TestARefusedCommandIsReportedByItsReason(t *testing.T) {
	// A reply is framed %begin, body, %end or %error — the reason a command
	// was refused comes before the %error that says it was. What follows the
	// frame is the ordinary stream again, and none of it is the error's.
	stream := strings.Join([]string{
		"%begin 1788307014 309 0",
		"%end 1788307014 309 0",
		"%session-changed $0 scrn",
		"%begin 1788307014 314 1",
		"parse error: unknown command: nosuchcommand",
		"%error 1788307014 314 1",
		"%output %0 hello",
		"%begin 1788307015 315 1",
		"%end 1788307015 315 1",
		"%exit",
	}, "\n") + "\n"

	var notes []ctlNote
	readCtl(strings.NewReader(stream), func(n ctlNote) { notes = append(notes, n) })

	want := []ctlNote{
		{kind: noteError, err: "parse error: unknown command: nosuchcommand"},
		{kind: noteExit},
	}
	if len(notes) != len(want) {
		t.Fatalf("notes = %+v, want %+v", notes, want)
	}
	for i := range want {
		if notes[i] != want[i] {
			t.Errorf("note %d = %+v, want %+v", i, notes[i], want[i])
		}
	}
}

func TestAFailureTmuxOnlyMentionsIsStillAFailure(t *testing.T) {
	// tmux exits zero after failing to create a socket, and says so only on
	// stderr. Silence on stdout beside a complaint is a refusal.
	tmuxOnSocket(t)
	t.Setenv("SCRN_SOCKET", filepath.Join(filepath.Dir(os.Getenv("SCRN_SOCKET")), "missing", "t.sock"))

	_, err := tmuxCommand("start-server")
	if err == nil || !strings.Contains(err.Error(), "error creating") {
		t.Fatalf("err = %v, want tmux's own complaint about the socket", err)
	}
}

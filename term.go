package main

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// scrn owns the shells it starts. A shell runs on a pty scrn allocated, its
// output is fed to a terminal emulator, and the emulator is drawn in the pane
// beside the navigator — so opening a shell does not take the screen away from
// the list of what else is running.
//
// The shell appears in the navigator by itself: it works in the repository it
// was started in, so the next process scan finds it like any other process and
// files it under that repository. Nothing here has to put it in the tree.
//
// A shell dies with scrn, because scrn holds the other end of its pty. Work
// that must outlive the window needs an owner that outlives it too, which is
// what the daemon is for.

// termReadSize is how much pty output is taken at a time.
const termReadSize = 32 * 1024

// terminal is one shell scrn started, and the screen it has drawn.
type terminal struct {
	pid  int
	repo string
	cmd  *exec.Cmd
	pty  *os.File
	vt   *vt.SafeEmulator

	// output carries a signal that the emulator has changed and the pane
	// should be redrawn. It is closed when the shell exits.
	output chan struct{}
}

// termOutputMsg says a terminal has drawn something new.
type termOutputMsg struct {
	pid int
}

// termExitedMsg says a shell has finished.
type termExitedMsg struct {
	pid int
}

// termStartedMsg carries a newly opened shell, or the reason there isn't one.
type termStartedMsg struct {
	term *terminal
	err  error
}

// openTerm starts a shell in dir on a pty of its own.
func openTerm(dir string, width, height int) tea.Cmd {
	return func() tea.Msg {
		t, err := startTerm(dir, width, height)
		return termStartedMsg{term: t, err: err}
	}
}

// termMinWidth and termMinHeight stand in when a shell is opened before the
// window size is known. A pty of no size makes the shell believe it is drawing
// on nothing, which it will act on; the next resize corrects it.
const (
	termMinWidth  = 80
	termMinHeight = 24
)

func startTerm(dir string, width, height int) (*terminal, error) {
	if width <= 0 {
		width = termMinWidth
	}
	if height <= 0 {
		height = termMinHeight
	}

	c := exec.Command(shellCommand())
	c.Dir = dir
	// TERM is what the shell and everything it runs will believe about the
	// screen they are drawing on, so it has to describe the emulator here
	// rather than whatever terminal scrn itself was started from.
	c.Env = append(os.Environ(), "TERM=xterm-256color")

	f, err := pty.StartWithSize(c, winsize(width, height))
	if err != nil {
		return nil, err
	}

	t := &terminal{
		pid:    c.Process.Pid,
		repo:   dir,
		cmd:    c,
		pty:    f,
		vt:     vt.NewSafeEmulator(width, height),
		output: make(chan struct{}, 1),
	}
	go t.pump()
	return t, nil
}

// pump feeds pty output into the emulator until the shell exits, waking the
// UI as it goes. The wake is a nudge rather than the output itself: a build
// can produce far more than the screen can show, and the pane draws whatever
// the emulator has arrived at rather than every byte on the way there.
func (t *terminal) pump() {
	defer close(t.output)

	buf := make([]byte, termReadSize)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			t.vt.Write(buf[:n])
			select {
			case t.output <- struct{}{}:
			default: // a wake is already pending; one is enough
			}
		}
		if err != nil {
			return
		}
	}
}

// waitForOutput turns the next wake into a message. Exactly one of these is in
// flight per terminal: each message schedules the next, the same discipline the
// refresh tick keeps.
func waitForOutput(t *terminal) tea.Cmd {
	pid := t.pid
	ch := t.output
	return func() tea.Msg {
		if _, ok := <-ch; !ok {
			return termExitedMsg{pid: pid}
		}
		return termOutputMsg{pid: pid}
	}
}

// write sends what the user typed to the shell. A write to a shell that has
// just exited fails harmlessly, which is the right answer for a keystroke that
// raced the exit.
func (t *terminal) write(b []byte) {
	if len(b) > 0 {
		_, _ = t.pty.Write(b)
	}
}

// resize tells both the shell and the emulator the pane has changed shape.
// The shell is told through the pty so that curses programs running in it
// redraw, which is the whole reason a pty is involved rather than a pipe.
func (t *terminal) resize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	_ = pty.Setsize(t.pty, winsize(width, height))
	t.vt.Resize(width, height)
}

// close ends the shell and releases the pty.
func (t *terminal) close() {
	_ = t.pty.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
}

// lines is the emulator's screen, one string per row, ready to be drawn.
func (t *terminal) lines(height int) []string {
	rows := strings.Split(t.vt.Render(), "\n")
	if len(rows) > height {
		rows = rows[:height]
	}
	return rows
}

// cursor is where the shell's cursor sits within the pane.
func (t *terminal) cursor() (x, y int) {
	p := t.vt.CursorPosition()
	return p.X, p.Y
}

func winsize(width, height int) *pty.Winsize {
	return &pty.Winsize{Cols: uint16(width), Rows: uint16(height)}
}

// shellCommand is the shell to open, honoring $SHELL so scrn opens the one the
// user actually uses.
func shellCommand() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

package main

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

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

	// window holds what the program running in here has asked of the terminal
	// window itself rather than of its screen: its title, and its progress.
	// Those mean nothing to an emulator drawn inside a pane, so they are kept
	// to be handed to the real terminal outside.
	windowMu sync.Mutex
	title    string
	progress string

	// done is closed once the process has been reaped, and closing guards the
	// teardown: a shell ended by hand is torn down again when its output stops.
	done    chan struct{}
	closing sync.Once
}

// hangupGrace is how long a shell is given to act on losing its terminal
// before it is killed outright.
const hangupGrace = 2 * time.Second

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

// termMinWidth and termMinHeight stand in when a shell is opened before the
// window size is known. A pty of no size makes the shell believe it is drawing
// on nothing, which it will act on; the next resize corrects it.
const (
	termMinWidth  = 80
	termMinHeight = 24
)

// startTerm runs command in dir on a pty of its own. An empty command means
// the user's shell, which is what most of them are.
func startTerm(dir, command string, width, height int) (*terminal, error) {
	if width <= 0 {
		width = termMinWidth
	}
	if height <= 0 {
		height = termMinHeight
	}

	// Whatever is asked for is run under a shell, so that it is found on the
	// PATH the user actually has rather than the one scrn inherited, and so a
	// command that exits leaves the shell behind rather than the row vanishing.
	c := exec.Command(shellCommand())
	if command != "" {
		c = exec.Command(shellCommand(), "-c", command+"; exec "+shellCommand())
	}
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
		done:   make(chan struct{}),
	}
	t.watchWindow()
	go func() {
		_ = c.Wait()
		close(t.done)
	}()
	go t.pump()
	go t.reply()
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

// reply carries the emulator's answers back to the shell.
//
// A terminal is asked questions as well as told things: what colour the
// background is, where the cursor got to, what the terminal claims to be.
// The emulator writes its answers into a pipe, and that write blocks while it
// still holds the lock on the screen — so with nothing draining the pipe, the
// first program to ask anything wedges the emulator, and the pane and the UI
// drawing it go with it. Every modern terminal program asks.
func (t *terminal) reply() {
	buf := make([]byte, 1024)
	for {
		n, err := t.vt.Read(buf)
		if n > 0 {
			if _, err := t.pty.Write(buf[:n]); err != nil {
				return
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

// Window-scoped sequences. A program addresses these to the terminal it is in,
// not to the grid it is drawing on, so an emulator has nothing to do with them
// and swallowing them loses what they were for: the title on the tab, and the
// progress the terminal shows while work is running.
const (
	oscTitleAndIcon = 0 // both at once
	oscIconName     = 1
	oscWindowTitle  = 2
	oscProgress     = 9 // 9;4;state;percent, as ConEmu defined it
)

// watchWindow catches what the program says to the window, so it can be passed
// out to the terminal that actually has one.
func (t *terminal) watchWindow() {
	for _, cmd := range []int{oscTitleAndIcon, oscIconName, oscWindowTitle} {
		t.vt.RegisterOscHandler(cmd, func(data []byte) bool {
			t.setWindow(&t.title, string(data))
			return false // the emulator still wants it for its own title
		})
	}
	t.vt.RegisterOscHandler(oscProgress, func(data []byte) bool {
		t.setWindow(&t.progress, string(data))
		return true
	})
}

func (t *terminal) setWindow(field *string, data string) {
	t.windowMu.Lock()
	*field = data
	t.windowMu.Unlock()

	// Wake the pane so what was just asked for goes out with the next screen.
	select {
	case t.output <- struct{}{}:
	default:
	}
}

// window returns what the program has asked of the terminal window.
func (t *terminal) window() (title, progress string) {
	t.windowMu.Lock()
	defer t.windowMu.Unlock()
	return t.title, t.progress
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
//
// Closing the pty is a hangup, which is what ending a shell actually looks
// like: an interactive shell ignores SIGTERM by design, so signalling one does
// nothing at all. Losing its terminal is the thing it listens to. Only a shell
// that will not go even then is killed outright.
//
// Closing the emulator is what lets the goroutine waiting on its answers
// finish, rather than sitting on a pipe nothing will ever write to again.
func (t *terminal) close() {
	t.closing.Do(func() {
		_ = t.vt.Close()
		_ = t.pty.Close()

		select {
		case <-t.done:
		case <-time.After(hangupGrace):
			if t.cmd.Process != nil {
				_ = t.cmd.Process.Kill()
			}
		}
	})
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

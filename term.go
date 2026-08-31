package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
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

// scrollbackLines is how many lines of transcript a shell keeps once they
// scroll off its pane. The daemon sets it from the config before any shell
// exists; every emulator made after that — opened or adopted — is given it.
var scrollbackLines = vt.DefaultScrollbackSize

// newEmulator is the emulator every shell draws on, holding as much
// transcript as the daemon was configured to keep.
func newEmulator(width, height int) *vt.SafeEmulator {
	e := vt.NewSafeEmulator(width, height)
	e.SetScrollbackSize(scrollbackLines)
	return e
}

// terminal is one shell scrn started, and the screen it has drawn.
type terminal struct {
	pid  int
	repo string
	name string // what the project calls it, if a project asked for it
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

	// sendMu is held while input is being handed to the emulator, and gone is
	// set under it before the emulator is closed. Together they are what stops
	// a keystroke arriving at an emulator whose answers nothing is draining
	// any more, which would block on the pipe while holding the screen.
	sendMu sync.RWMutex
	gone   bool

	// sizeMu guards cols. One client's resize can arrive while a screen is
	// being rendered for another, and the render needs the width the grid
	// actually has.
	sizeMu sync.Mutex
	cols   int

	// vtMu orders the pump's writes against a walk of the scrollback. The
	// emulator locks each of its own calls, but the scrollback is handed out
	// as the live buffer, and iterating it while the pump pushes lines onto
	// it is a race the emulator cannot see.
	vtMu sync.RWMutex

	// modeMu guards modes: every DEC private mode the program has set or
	// reset, as it last said it. The mouse-reporting ones decide whose a
	// wheel turn is, which the emulator does not answer — and all of them
	// together are what a handoff replays, because a fresh emulator starts
	// with none of them.
	modeMu sync.Mutex
	modes  map[ansi.DECMode]bool
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

	// The emulator allocates its whole grid up front, so a claimed size is
	// memory the daemon commits to on the spot. No real pane comes close to
	// these; a claim past them is a bug or a lie, and either way not worth
	// every shell the daemon holds.
	termMaxWidth  = 1024
	termMaxHeight = 512
)

// clampSize keeps a claimed pane within the sizes a pane can actually be.
func clampSize(width, height int) (int, int) {
	if width <= 0 {
		width = termMinWidth
	}
	if height <= 0 {
		height = termMinHeight
	}
	return min(width, termMaxWidth), min(height, termMaxHeight)
}

// startTerm runs command in dir on a pty of its own. An empty command means
// the user's shell, which is what most of them are.
func startTerm(dir, command, name string, width, height int) (*terminal, error) {
	width, height = clampSize(width, height)

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
		name:   name,
		cmd:    c,
		pty:    f,
		vt:     newEmulator(width, height),
		cols:   width,
		output: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	t.watchWindow()
	t.watchModes()
	t.start(func() { _ = c.Wait() })
	return t, nil
}

// start begins the goroutines that serve the shell: one waiting to reap it,
// one pumping its output into the emulator, one carrying the emulator's
// answers back. wait is how the exit is heard, because a shell this process
// forked is waited on through its cmd and one adopted across an exec has no
// cmd to wait through.
func (t *terminal) start(wait func()) {
	go func() {
		wait()
		close(t.done)
	}()
	go t.pump()
	go t.reply()
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
			t.vtMu.Lock()
			t.vt.Write(buf[:n])
			t.vtMu.Unlock()
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

// send hands what the user did to the emulator, which writes the bytes the
// program in the pane is expecting and hands them out through reply.
//
// This is the whole reason the input crosses as an event. The emulator is
// tracking what the program has asked for — application cursor keys, which
// mouse reporting mode, whether the kitty protocol is on — and those decide
// what an arrow key or a click is on the wire. Nothing on the window's side
// knows any of it.
//
// A keystroke that raced the close is dropped. The emulator answers into a
// pipe and blocks there while holding its screen, so handing it a key with
// reply no longer draining would wedge it and the shell's own goroutines with
// it. Losing the last keystroke of a shell being closed costs nothing.
func (t *terminal) send(m message) {
	t.sendMu.RLock()
	defer t.sendMu.RUnlock()
	if t.gone {
		return
	}

	switch {
	case m.Paste != "":
		// Paste rather than a run of keystrokes, so that a program with
		// bracketed paste on is told where the pasted text begins and ends
		// rather than being made to believe someone typed all of it.
		t.vt.Paste(m.Paste)
	case m.Key != nil:
		t.vt.SendKey(uv.KeyPressEvent{
			Code: m.Key.Code,
			Text: m.Key.Text,
			Mod:  uv.KeyMod(m.Key.Mod),
		})
	case m.Mouse != nil:
		// A wheel turned over the alternate screen, with the program not
		// listening for the mouse, becomes the arrow keys it would have
		// meant. It is how less and man scroll under any terminal that
		// implements alternate scroll, and here scrn is the terminal.
		if key, ok := wheelAsArrow(m.Mouse); ok && !t.mouseWanted() && t.vt.IsAltScreen() {
			for range wheelArrowCount {
				t.vt.SendKey(uv.KeyPressEvent{Code: key})
			}
			return
		}
		t.vt.SendMouse(m.Mouse.event())
	}
}

// wheelArrowCount is how many arrow presses one wheel notch stands for, which
// is the count xterm's alternate scroll settled on.
const wheelArrowCount = 3

// wheelAsArrow is the arrow key a vertical wheel turn stands for, when it is
// to be translated rather than reported.
func wheelAsArrow(m *mousePress) (rune, bool) {
	if m.Action != actPress {
		return 0, false
	}
	switch uv.MouseButton(m.Button) {
	case uv.MouseWheelUp:
		return uv.KeyUp, true
	case uv.MouseWheelDown:
		return uv.KeyDown, true
	}
	return 0, false
}

// event is the mouse event the emulator understands. The buttons are numbered
// the same on both sides, from the X11 codes.
func (m *mousePress) event() uv.MouseEvent {
	mouse := uv.Mouse{
		X:      m.X,
		Y:      m.Y,
		Button: uv.MouseButton(m.Button),
		Mod:    uv.KeyMod(m.Mod),
	}
	switch m.Action {
	case actRelease:
		return uv.MouseReleaseEvent(mouse)
	case actMotion:
		return uv.MouseMotionEvent(mouse)
	}
	// A wheel turn is reported as a press of a button only a wheel has, and
	// the emulator wants it as the wheel event it is.
	switch mouse.Button {
	case uv.MouseWheelUp, uv.MouseWheelDown, uv.MouseWheelLeft, uv.MouseWheelRight:
		return uv.MouseWheelEvent(mouse)
	}
	return uv.MouseClickEvent(mouse)
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

// mouseModes are the reporting modes a program can turn on, any of which
// makes the wheel and the buttons the program's rather than the terminal's.
var mouseModes = map[ansi.DECMode]bool{
	ansi.ModeMouseX10:         true,
	ansi.ModeMouseNormal:      true,
	ansi.ModeMouseHighlight:   true,
	ansi.ModeMouseButtonEvent: true,
	ansi.ModeMouseAnyEvent:    true,
}

// watchModes tracks the DEC modes as the program sets and resets them. The
// callback runs inside the emulator's own processing, so it touches nothing
// but the map.
func (t *terminal) watchModes() {
	t.vt.SetCallbacks(vt.Callbacks{
		EnableMode:  func(m ansi.Mode) { t.trackMode(m, true) },
		DisableMode: func(m ansi.Mode) { t.trackMode(m, false) },
	})
}

func (t *terminal) trackMode(mode ansi.Mode, on bool) {
	dec, ok := mode.(ansi.DECMode)
	if !ok {
		return
	}
	t.modeMu.Lock()
	defer t.modeMu.Unlock()
	if t.modes == nil {
		t.modes = map[ansi.DECMode]bool{}
	}
	t.modes[dec] = on
}

// mouseWanted reports whether the program has asked to hear about the mouse.
func (t *terminal) mouseWanted() bool {
	t.modeMu.Lock()
	defer t.modeMu.Unlock()
	for m := range mouseModes {
		if t.modes[m] {
			return true
		}
	}
	return false
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
	width, height = clampSize(width, height)
	// The size arbitration re-applies on every ask, so the same answer comes
	// through here again and again; unchanged is not a resize.
	if width == t.vt.Width() && height == t.vt.Height() {
		return
	}
	_ = pty.Setsize(t.pty, winsize(width, height))
	t.vtMu.Lock()
	t.vt.Resize(width, height)
	t.vtMu.Unlock()
	t.sizeMu.Lock()
	t.cols = width
	t.sizeMu.Unlock()
}

// close ends the shell and releases the pty.
//
// A hangup is what ending a shell looks like: an interactive shell ignores
// SIGTERM by design, so signalling one that way does nothing at all. Losing
// its terminal is the thing it listens to. Only a shell that will not go even
// then is killed outright.
//
// The hangup has to be sent rather than implied. Closing the master end of the
// pty reads like taking the terminal away, but no shell notices it — zsh, bash
// and sh all sit there indefinitely — so a close that only closed the pty
// waited out the whole grace and killed every shell it was trying to be gentle
// with. SIGHUP is the same message said out loud.
//
// It goes to the process group rather than the shell, which is what setsid put
// the shell at the head of. A plan entry is a shell running an npm running a
// node, and it is the node that has to hear about it; signalling the shell
// alone would leave what it started behind.
//
// Closing the emulator is what lets the goroutine waiting on its answers
// finish, rather than sitting on a pipe nothing will ever write to again.
func (t *terminal) close() {
	t.closing.Do(func() {
		// Stop taking input first, and wait out anything already inside send:
		// past this the emulator is going, and nothing may be left holding it.
		t.sendMu.Lock()
		t.gone = true
		t.sendMu.Unlock()

		// A shell already reaped has no group left to signal, and its pid is
		// free to have been given to something else — which must not be sent
		// a hangup meant for a process that has already gone.
		select {
		case <-t.done:
		default:
			_ = syscall.Kill(-t.pid, syscall.SIGHUP)
			select {
			case <-t.done:
			case <-time.After(hangupGrace):
				// By pid rather than through cmd, because an adopted shell
				// has no cmd: the exec kept the child but not the exec.Cmd
				// that once held it.
				_ = syscall.Kill(t.pid, syscall.SIGKILL)
			}
		}

		_ = t.vt.Close()
		_ = t.pty.Close()
	})
}

// screen is the emulator's pane, every row padded back out to the width of
// the grid it stands for. Render writes for a real terminal, which keeps a
// grid of its own and needs no trailing blanks, so it drops them. On the wire
// this string is the only grid there is: the client cuts columns out of it to
// mark the cursor cell, and a row that stops short of the edge puts that cell
// somewhere the cursor is not.
func (t *terminal) screen() string {
	rows := strings.Split(t.vt.Render(), "\n")
	t.sizeMu.Lock()
	cols := t.cols
	t.sizeMu.Unlock()
	for i, row := range rows {
		if pad := cols - ansi.StringWidth(row); pad > 0 {
			rows[i] = row + strings.Repeat(" ", pad)
		}
	}
	return strings.Join(rows, "\n")
}

// history is everything that has scrolled off the top of the pane, oldest
// first, styled the way it was drawn. It is the emulator's scrollback made
// into the kind of string a screen crosses the wire as — one row per line —
// but without the padding: no cursor is ever cut into these rows, so nothing
// leans on their width.
//
// Only the line headers are copied under the lock. A line is cloned as it is
// pushed and never written again — the buffer only shifts and appends around
// it — so the styling, which is the slow part, runs after the pump has been
// let go. A transcript rendered while a build pours output must not make the
// build wait.
func (t *terminal) history() string {
	t.vtMu.RLock()
	lines := slices.Clone(t.vt.Scrollback().Lines())
	t.vtMu.RUnlock()

	if len(lines) == 0 {
		return ""
	}
	rows := make([]string, len(lines))
	for i, line := range lines {
		rows[i] = line.Render()
	}
	return strings.Join(rows, "\n")
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

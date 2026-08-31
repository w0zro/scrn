package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A daemon is replaced by exec rather than by stopping. Exec keeps the
// process — its pid, its children, its open file descriptors — and swaps only
// the code, so the shells never notice the daemon holding them changed
// builds. What exec does not keep is memory: everything the emulators know is
// written down first, and the image that wakes up in this process reads it
// back and carries on.
//
// What is written down is what the emulators would show, not their every
// internal — the transcript, the screen, the cursor, and the modes the
// program asked for. State a program set without a mode — scroll margins, tab
// stops, kitty keyboard flags — starts over, and the programs that care
// re-assert it the next time they draw.

// handoffTerm is one shell as it crosses an exec: the fd its pty sits at, and
// everything a fresh emulator needs to take up where the old one left off.
type handoffTerm struct {
	PID  int    `json:"pid"`
	FD   int    `json:"fd"`
	Dir  string `json:"dir"`
	Name string `json:"name,omitempty"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`

	Title    string `json:"title,omitempty"`
	Progress string `json:"progress,omitempty"`

	// Modes is every DEC mode the program set or reset, as it last said it.
	Modes map[int]bool `json:"modes,omitempty"`
	Alt   bool         `json:"alt,omitempty"`

	History string `json:"history,omitempty"`
	Screen  string `json:"screen,omitempty"`
	CursorX int    `json:"cx"`
	CursorY int    `json:"cy"`
}

// handoffState is what one daemon leaves for the next image of itself.
type handoffState struct {
	ListenFD int           `json:"listenfd"`
	Terms    []handoffTerm `json:"terms"`
}

// handoffPath is where the state waits out the exec, beside the socket.
func handoffPath() string {
	return filepath.Join(filepath.Dir(socketPath()), "handoff.json")
}

// handoff is the shell written down for the image on the far side of the exec.
func (t *terminal) handoff() handoffTerm {
	x, y := t.cursor()
	title, progress := t.window()

	t.modeMu.Lock()
	modes := make(map[int]bool, len(t.modes))
	for m, on := range t.modes {
		modes[int(m)] = on
	}
	t.modeMu.Unlock()

	return handoffTerm{
		PID:      t.pid,
		FD:       int(t.pty.Fd()),
		Dir:      t.repo,
		Name:     t.name,
		Cols:     t.vt.Width(),
		Rows:     t.vt.Height(),
		Title:    title,
		Progress: progress,
		Modes:    modes,
		Alt:      t.vt.IsAltScreen(),
		History:  t.history(),
		Screen:   t.screen(),
		CursorX:  x,
		CursorY:  y,
	}
}

// altModes are the alternate-screen modes. The replay orders the switch by
// hand — the transcript belongs to the primary screen and has to be written
// there first — so wherever these fell in the program's own asking, they are
// not replayed in place.
var altModes = map[int]bool{47: true, 1047: true, 1048: true, 1049: true}

// replay is the byte stream that turns a fresh emulator into the one written
// down: the modes as the program left them, the transcript scrolled through
// the primary screen, the pane redrawn, the cursor put back. Styles are reset
// at every line's end so one row's colors do not bleed into the next.
func (h handoffTerm) replay() []byte {
	var b strings.Builder
	for m, on := range h.Modes {
		if altModes[m] {
			continue
		}
		flag := "l"
		if on {
			flag = "h"
		}
		fmt.Fprintf(&b, "\x1b[?%d%s", m, flag)
	}
	if h.History != "" {
		b.WriteString(strings.ReplaceAll(h.History, "\n", "\x1b[m\r\n"))
		b.WriteString("\x1b[m\r\n")
	}
	if h.Alt {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString(strings.ReplaceAll(h.Screen, "\n", "\x1b[m\r\n"))
	fmt.Fprintf(&b, "\x1b[m\x1b[%d;%dH", h.CursorY+1, h.CursorX+1)
	return []byte(b.String())
}

// adoptTerm rebuilds a shell around a pty fd that survived the exec. The
// process is already a child of this one — exec kept it — so it is reaped by
// pid, which is the one thing a freshly forked shell has that an adopted one
// lacks.
func adoptTerm(h handoffTerm) *terminal {
	// The fd crossed the exec only because keepOpen said so. Adopted, it goes
	// back to closing on exec, or every shell forked from here on inherits the
	// master — a stranger's copy that can read the session and keeps the
	// kernel from ever hanging the slave up.
	syscall.CloseOnExec(h.FD)
	t := &terminal{
		pid:    h.PID,
		repo:   h.Dir,
		name:   h.Name,
		pty:    os.NewFile(uintptr(h.FD), "pty"),
		vt:     newEmulator(h.Cols, h.Rows),
		cols:   h.Cols,
		output: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	// The watchers go in before the replay so that replaying the modes
	// records them again, the same way hearing them from the program did.
	t.watchWindow()
	t.watchModes()
	_, _ = t.vt.Write(h.replay())
	t.windowMu.Lock()
	t.title, t.progress = h.Title, h.Progress
	t.windowMu.Unlock()
	t.start(func() { waitChild(h.PID) })
	return t
}

// waitChild reaps a child adopted across an exec, the way cmd.Wait would have.
func waitChild(pid int) {
	for {
		if _, err := syscall.Wait4(pid, nil, 0, nil); err != syscall.EINTR {
			return
		}
	}
}

// keepOpen clears close-on-exec, which everything Go opens carries: an fd
// meant to survive the exec has to say so.
func keepOpen(fd uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, 0); errno != 0 {
		return errno
	}
	return nil
}

// execSelf replaces this daemon with the binary at exe — the one the asking
// window is running, since the daemon's own may be gone — carrying the
// listener and every shell across as inherited file descriptors and a state
// file. An ask without a path falls back to the daemon's own, for a window
// old enough not to send one. On success it does not return: the exec is the
// return. On failure the daemon carries on, so the one thing changed on the
// way — close-on-exec cleared from every pty — is put back, or the failed
// attempt leaves the masters leaking into every shell forked after it.
func (d *daemon) execSelf(exe string) error {
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}

	lf, err := d.listener.(*net.UnixListener).File()
	if err != nil {
		return err
	}
	defer lf.Close() // reached only when the exec did not happen

	if err := keepOpen(lf.Fd()); err != nil {
		return err
	}
	st := handoffState{ListenFD: int(lf.Fd())}
	reclose := func(err error) error {
		for _, h := range st.Terms {
			syscall.CloseOnExec(h.FD)
		}
		return err
	}
	for _, t := range d.held() {
		h := t.handoff()
		if err := keepOpen(uintptr(h.FD)); err != nil {
			return reclose(err)
		}
		st.Terms = append(st.Terms, h)
	}

	data, err := json.Marshal(st)
	if err != nil {
		return reclose(err)
	}
	path := handoffPath()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return reclose(err)
	}

	err = syscall.Exec(exe, []string{exe, "daemon"}, append(os.Environ(), "SCRN_HANDOFF="+path))
	_ = os.Remove(path) // the exec refused, so nothing will read it
	return reclose(err)
}

// resumeDaemon is the far side of the exec: the same process, running the new
// binary, reading back what the old image wrote down and carrying on serving.
func resumeDaemon(path string) (*daemon, error) {
	// Unset, or the variable rides along into the next exec and into every
	// shell this daemon starts.
	_ = os.Unsetenv("SCRN_HANDOFF")
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st handoffState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}

	lf := os.NewFile(uintptr(st.ListenFD), "scrnd.sock")
	l, err := net.FileListener(lf)
	lf.Close() // FileListener dups; the inherited number is done with
	if err != nil {
		return nil, err
	}

	d := &daemon{
		sessions: map[int]*terminal{},
		clients:  map[*client]bool{},
		listener: l,
		path:     socketPath(),
		started:  time.Now(),
		idleFrom: time.Now(),
	}
	for _, h := range st.Terms {
		t := adoptTerm(h)
		d.sessions[t.pid] = t
		go d.pump(t)
	}
	return d, nil
}

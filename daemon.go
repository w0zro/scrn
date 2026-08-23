package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// idleShutdown is how long the daemon waits, holding nothing and talking to
// nobody, before it stops. A daemon with shells in it never stops on its own:
// those shells are the whole reason it exists.
const idleShutdown = 60 * time.Second

// daemon holds the shells and serves the clients looking at them.
type daemon struct {
	mu       sync.Mutex
	sessions map[int]*terminal
	clients  map[*client]bool

	listener net.Listener
	path     string
	idleFrom time.Time
}

// client is one connected window, and the shells it is watching.
type client struct {
	conn *conn

	mu       sync.Mutex
	watching map[int]bool
	closed   bool
}

// runDaemon listens until nothing is left to hold.
func runDaemon() error {
	// The daemon inherits the working directory of the window that started it,
	// which would file it under that repository as work happening there — and
	// hang every shell it holds beneath itself in the tree. It belongs to no
	// project, so it stands in none of them.
	_ = os.Chdir("/")

	d, err := listenDaemon(socketPath())
	if err != nil {
		return err
	}
	go d.watchIdle()
	return d.accept()
}

// listenDaemon claims the socket. It is separate from accepting so that a test
// can hold a daemon of its own and stop it again.
func listenDaemon(path string) (*daemon, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	// A socket left by a daemon that died is not a daemon. Only clear it away
	// once nothing answers, so two daemons cannot fight over one path.
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, errors.New("a daemon is already running")
	}
	_ = os.Remove(path)

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return &daemon{
		sessions: map[int]*terminal{},
		clients:  map[*client]bool{},
		listener: l,
		path:     path,
		idleFrom: time.Now(),
	}, nil
}

// accept serves clients until the listener is closed.
func (d *daemon) accept() error {
	defer func() {
		d.listener.Close()
		_ = os.Remove(d.path)
	}()

	for {
		c, err := d.listener.Accept()
		if err != nil {
			return nil // the listener was closed, which is how this ends
		}
		go d.serve(newConn(c))
	}
}

// stop ends the daemon and every shell it holds. Only a test calls this: a
// real daemon stops when it has nothing left, and its shells are the point.
func (d *daemon) stop() {
	d.listener.Close()

	d.mu.Lock()
	held := make([]*terminal, 0, len(d.sessions))
	for _, t := range d.sessions {
		held = append(held, t)
	}
	d.sessions = map[int]*terminal{}
	d.mu.Unlock()

	for _, t := range held {
		t.close()
	}
}

// watchIdle stops a daemon that is holding nothing and serving nobody, so a
// machine does not collect one per experiment.
func (d *daemon) watchIdle() {
	for range time.Tick(5 * time.Second) {
		d.mu.Lock()
		busy := len(d.sessions) > 0 || len(d.clients) > 0
		if busy {
			d.idleFrom = time.Now()
		}
		done := !busy && time.Since(d.idleFrom) > idleShutdown
		d.mu.Unlock()

		if done {
			d.listener.Close()
			return
		}
	}
}

// serve handles one client until it goes away. Its shells are not touched on
// the way out: outliving the window is the point.
func (d *daemon) serve(c *conn) {
	cl := &client{conn: c, watching: map[int]bool{}}

	d.mu.Lock()
	d.clients[cl] = true
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		delete(d.clients, cl)
		d.idleFrom = time.Now()
		d.mu.Unlock()
		cl.markClosed()
		c.close()
	}()

	for {
		m, err := c.read()
		if err != nil {
			return
		}
		d.handle(cl, m)
	}
}

func (d *daemon) handle(cl *client, m message) {
	switch m.Kind {
	case kindOpen:
		d.open(cl, m)
	case kindList:
		cl.send(message{Kind: kindSessions, Sessions: d.list()})
	case kindAttach:
		d.attach(cl, m)
	case kindDetach:
		cl.unwatch(m.PID)
	case kindInput:
		if t := d.session(m.PID); t != nil {
			t.write(m.Data)
		}
	case kindResize:
		if t := d.session(m.PID); t != nil {
			t.resize(m.Width, m.Height)
			cl.send(t.screenMsg())
		}
	case kindClose:
		// Off this goroutine: a shell is given a moment to act on the hangup,
		// and this client has other things to say in the meantime.
		if t := d.session(m.PID); t != nil {
			go t.close()
		}
	}
}

func (d *daemon) open(cl *client, m message) {
	t, err := startTerm(m.Dir, m.Run, m.Width, m.Height)
	if err != nil {
		cl.send(message{Kind: kindError, Err: err.Error()})
		return
	}

	d.mu.Lock()
	d.sessions[t.pid] = t
	d.mu.Unlock()

	cl.watch(t.pid)
	go d.pump(t)
	// The client that asked is told which shell is the one it asked for. It
	// cannot tell from the list or the screen: by the time either arrives the
	// shell is just another one the daemon is holding.
	cl.send(message{Kind: kindOpened, PID: t.pid})
	cl.send(message{Kind: kindSessions, Sessions: d.list()})
	cl.send(t.screenMsg())
}

// attach starts sending a shell's screen to this client, beginning with the
// screen as it stands — which is why the emulator lives here and not there.
func (d *daemon) attach(cl *client, m message) {
	t := d.session(m.PID)
	if t == nil {
		cl.send(message{Kind: kindExited, PID: m.PID})
		return
	}
	if m.Width > 0 && m.Height > 0 {
		t.resize(m.Width, m.Height)
	}
	cl.watch(m.PID)
	cl.send(t.screenMsg())
}

// pump forwards a shell's screen to whoever is watching it, until it exits.
func (d *daemon) pump(t *terminal) {
	for range t.output {
		msg := t.screenMsg()
		for _, cl := range d.watchers(t.pid) {
			cl.send(msg)
		}
	}

	d.mu.Lock()
	delete(d.sessions, t.pid)
	d.idleFrom = time.Now()
	d.mu.Unlock()

	t.close()
	for _, cl := range d.allClients() {
		cl.send(message{Kind: kindExited, PID: t.pid})
	}
}

func (d *daemon) session(pid int) *terminal {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[pid]
}

func (d *daemon) list() []sessionInfo {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]sessionInfo, 0, len(d.sessions))
	for pid, t := range d.sessions {
		out = append(out, sessionInfo{PID: pid, Dir: t.repo})
	}
	return out
}

func (d *daemon) watchers(pid int) []*client {
	d.mu.Lock()
	defer d.mu.Unlock()

	var out []*client
	for cl := range d.clients {
		if cl.isWatching(pid) {
			out = append(out, cl)
		}
	}
	return out
}

func (d *daemon) allClients() []*client {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]*client, 0, len(d.clients))
	for cl := range d.clients {
		out = append(out, cl)
	}
	return out
}

// send writes to the client under its own lock, because screens arrive from a
// goroutine per shell and an encoder shared between them interleaves.
func (cl *client) send(m message) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.closed {
		return
	}
	if err := cl.conn.write(m); err != nil {
		cl.closed = true
	}
}

func (cl *client) watch(pid int) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.watching[pid] = true
}

func (cl *client) unwatch(pid int) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	delete(cl.watching, pid)
}

func (cl *client) isWatching(pid int) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.watching[pid]
}

func (cl *client) markClosed() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.closed = true
}

// screenMsg is the shell's pane as it now stands.
func (t *terminal) screenMsg() message {
	x, y := t.cursor()
	return message{
		Kind:    kindScreen,
		PID:     t.pid,
		Screen:  t.vt.Render(),
		CursorX: x,
		CursorY: y,
	}
}

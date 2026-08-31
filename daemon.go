package main

import (
	"errors"
	"net"
	"os"
	sig "os/signal"
	"path/filepath"
	"sync"
	"syscall"
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
	started  time.Time
	idleFrom time.Time

	// stopping holds the process open while a stand-down runs: closing the
	// listener is stop's first move, and accept returning on it must not
	// outrun the shells being hung up.
	stopping sync.WaitGroup
}

// client is one connected window, and the shells it is watching.
type client struct {
	conn *conn

	// out is the queue between everyone with something to tell this window
	// and the one goroutine that writes to its socket. done, closed when the
	// window is gone, is what lets the writer and any sender stop waiting.
	out  chan message
	done chan struct{}

	mu       sync.Mutex
	watching map[int]bool
	closed   bool

	// sizes is the pane this window gives each shell, by pid. No window
	// dictates a shell's size alone: the shell is sized for the smallest pane
	// watching it, and these are what that arbitration reads.
	sizes map[int][2]int
}

// runDaemon listens until nothing is left to hold.
func runDaemon() error {
	// The daemon inherits the working directory of the window that started it,
	// which would file it under that repository as work happening there — and
	// hang every shell it holds beneath itself in the tree. It belongs to no
	// project, so it stands in none of them.
	_ = os.Chdir("/")

	// The one piece of config the daemon owns: how much transcript to keep.
	// It is read before any shell exists — including the adopted ones, whose
	// replayed history has to fit in it.
	if cfg, _ := loadConfig(); cfg.Scrollback > 0 {
		scrollbackLines = cfg.Scrollback
	}

	// A daemon woken by its own exec has a state file waiting and shells to
	// take back up; one started fresh claims the socket instead.
	var d *daemon
	var err error
	if path := os.Getenv("SCRN_HANDOFF"); path != "" {
		d, err = resumeDaemon(path)
	} else {
		d, err = listenDaemon(socketPath())
	}
	if err != nil {
		return err
	}
	// SIGTERM is how an outsider ends the daemon — the replace flow sends
	// it. The shells still get the hangup, the grace, and the kill, rather
	// than being orphaned by a bare exit.
	term := make(chan os.Signal, 1)
	sig.Notify(term, syscall.SIGTERM)
	go func() {
		<-term
		d.stop()
		os.Exit(0)
	}()

	go d.watchIdle()
	return d.accept()
}

// listenDaemon claims the socket. It is separate from accepting so that a test
// can hold a daemon of its own and stop it again.
func listenDaemon(path string) (*daemon, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	// The claim runs under a lock beside the socket, so two daemons starting
	// at once cannot both hear silence and unlink each other's fresh socket:
	// one claims, and the other finds a daemon answering. A lock that cannot
	// be had is skipped, which is the old racier behaviour, not a refusal.
	if lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err == nil {
			defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
		}
		defer lock.Close()
	}

	// A socket left by a daemon that died is not a daemon. Only clear it away
	// once nothing answers.
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, errors.New("a daemon is already running")
	}
	_ = os.Remove(path)

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The 0700 directory is the guard; the socket's own mode is a second
	// refusal, honoured where the kernel checks it, for a path the user has
	// moved somewhere more open.
	_ = os.Chmod(path, 0o600)
	return &daemon{
		sessions: map[int]*terminal{},
		clients:  map[*client]bool{},
		listener: l,
		path:     path,
		started:  time.Now(),
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
			// The listener closing is how a stand-down begins, not how it
			// ends: the shells are still being hung up, and returning is what
			// lets the process exit. Wait the stand-down out.
			d.stopping.Wait()
			return nil
		}
		go d.serve(newConn(c))
	}
}

// stop ends the daemon and every shell it holds. Nothing reaches this by
// accident: a daemon stops on its own only when it has nothing left, and a
// client has to say outright that the work in it should go too.
func (d *daemon) stop() {
	d.listener.Close()

	held := d.held()
	d.mu.Lock()
	d.sessions = map[int]*terminal{}
	d.mu.Unlock()

	// Together rather than one after another. Each shell is given a grace to
	// go in, and taking them in turn spends that grace once per shell: a
	// daemon holding five of them took five graces to stand down, with the
	// window that asked waiting on all of it.
	var wg sync.WaitGroup
	for _, t := range held {
		wg.Go(func() {
			t.close()
		})
	}
	wg.Wait()
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
	cl := &client{
		conn:     c,
		out:      make(chan message, sendQueue),
		done:     make(chan struct{}),
		watching: map[int]bool{},
		sizes:    map[int][2]int{},
	}
	go cl.write()

	d.mu.Lock()
	d.clients[cl] = true
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		delete(d.clients, cl)
		d.idleFrom = time.Now()
		d.mu.Unlock()
		cl.markClosed()
		close(cl.done)
		c.close()
		// A window gone takes its pane out of the sizing, so a shell held
		// small on its account grows back for whoever is left watching.
		for _, pid := range cl.watched() {
			if t := d.session(pid); t != nil {
				d.applySize(t)
			}
		}
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
		cl.send(d.sessionsMsg())
	case kindAttach:
		d.attach(cl, m)
	case kindDetach:
		cl.unwatch(m.PID)
		// The leaving pane drops out of the sizing, so a shell held small on
		// its account grows back for whoever is still watching.
		if t := d.session(m.PID); t != nil {
			d.applySize(t)
		}
	case kindInput:
		if t := d.session(m.PID); t != nil {
			t.send(m)
		}
	case kindResize:
		if t := d.session(m.PID); t != nil {
			cl.setSize(m.PID, m.Width, m.Height)
			d.applySize(t)
			cl.send(t.screenMsg())
		}
	case kindStand:
		// A daemon holding nothing can be replaced for free. One holding
		// shells goes only when told outright, because the work in it is why
		// it exists and ending that is not the daemon's decision.
		if m.Force {
			d.stopping.Add(1)
			go func() {
				defer d.stopping.Done()
				d.stop()
			}()
			return
		}
		d.mu.Lock()
		empty := len(d.sessions) == 0
		d.mu.Unlock()
		if empty {
			d.listener.Close()
		}

	case kindUpgrade:
		// Exec never returns when it works, so reaching the send means it did
		// not: the daemon is whole, and says what went wrong.
		if err := d.execSelf(m.Exe); err != nil {
			cl.send(message{Kind: kindError, Err: "upgrade: " + err.Error()})
		}

	case kindClose:
		// Off this goroutine: a shell is given a moment to act on the hangup,
		// and this client has other things to say in the meantime.
		if t := d.session(m.PID); t != nil {
			go t.close()
		}

	case kindHistory:
		// The transcript is rendered fresh on every ask: it is wanted at the
		// moment of asking, not as it stood some earlier time.
		if t := d.session(m.PID); t != nil {
			cl.send(message{Kind: kindHistory, PID: m.PID, History: t.history()})
		}
	}
}

func (d *daemon) open(cl *client, m message) {
	t, err := startTerm(m.Dir, m.Run, m.Name, m.Width, m.Height)
	if err != nil {
		cl.send(message{Kind: kindError, Err: err.Error()})
		return
	}

	d.mu.Lock()
	d.sessions[t.pid] = t
	d.mu.Unlock()

	cl.setSize(t.pid, m.Width, m.Height)
	cl.watch(t.pid)
	go d.pump(t)
	// The client that asked is told which shell is the one it asked for. It
	// cannot tell from the list or the screen: by the time either arrives the
	// shell is just another one the daemon is holding.
	cl.send(message{Kind: kindOpened, PID: t.pid, Name: t.name, Dir: t.repo})
	cl.send(d.sessionsMsg())
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
	cl.setSize(m.PID, m.Width, m.Height)
	cl.watch(m.PID)
	d.applySize(t)
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
	// Only this shell's own entry: the pid is free for the kernel to reuse
	// the moment the shell is reaped, and a new shell could be standing at
	// this number by the time an old pump gets here.
	if d.sessions[t.pid] == t {
		delete(d.sessions, t.pid)
	}
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

// held is the shells the daemon is holding, as a slice a caller can walk
// without the lock.
func (d *daemon) held() []*terminal {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]*terminal, 0, len(d.sessions))
	for _, t := range d.sessions {
		out = append(out, t)
	}
	return out
}

// sessionsMsg is what the daemon is holding, and when it started holding it.
func (d *daemon) sessionsMsg() message {
	return message{
		Kind:     kindSessions,
		Sessions: d.list(),
		Since:    d.started.UnixMilli(),
	}
}

func (d *daemon) list() []sessionInfo {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]sessionInfo, 0, len(d.sessions))
	for pid, t := range d.sessions {
		out = append(out, sessionInfo{PID: pid, Dir: t.repo, Name: t.name})
	}
	return out
}

// applySize sizes a shell to the smallest pane among the windows watching it,
// each dimension on its own — which is how every window gets to hold the
// whole screen. One watcher means that window's pane exactly, so a shell of
// one window behaves as it always did; no watcher with a pane leaves the
// shell as it is.
func (d *daemon) applySize(t *terminal) {
	w, h := 0, 0
	for _, cl := range d.watchers(t.pid) {
		cw, ch, ok := cl.sizeFor(t.pid)
		if !ok {
			continue
		}
		if w == 0 || cw < w {
			w = cw
		}
		if h == 0 || ch < h {
			h = ch
		}
	}
	if w > 0 && h > 0 {
		t.resize(w, h)
	}
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

// sendQueue is how far one window may fall behind before its news starts
// going stale unread. With the kernel's own socket buffer under it, only a
// window that has stopped reading altogether ever gets here.
const sendQueue = 64

// send queues a message for the client's writer. It never blocks: a window
// that stopped reading — suspended, wedged — must not hold a shell's pump,
// and through it every other window, hostage. When the queue is full the
// oldest message goes instead, which costs nothing that lasts: screens are
// snapshots each superseding the last, and the sessions the window polls for
// retell whatever news a dropped exit carried.
func (cl *client) send(m message) {
	for {
		select {
		case cl.out <- m:
			return
		case <-cl.done:
			return
		default:
		}
		select {
		case <-cl.out: // full, and the oldest is the least true
		default:
		}
	}
}

// write carries queued messages to the window, one goroutine per client, so
// each window waits only on itself. A write that fails marks the client
// closed; the draining goes on so no sender is ever left standing.
func (cl *client) write() {
	for {
		select {
		case <-cl.done:
			return
		case m := <-cl.out:
			if cl.isClosed() {
				continue
			}
			if err := cl.conn.write(m); err != nil {
				cl.markClosed()
			}
		}
	}
}

func (cl *client) isClosed() bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.closed
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

// watched is the shells this window is watching, as a slice a caller can walk
// without the lock.
func (cl *client) watched() []int {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	out := make([]int, 0, len(cl.watching))
	for pid := range cl.watching {
		out = append(out, pid)
	}
	return out
}

// setSize records the pane this window gives a shell. A size without room in
// it is not a pane, so it is not recorded.
func (cl *client) setSize(pid, w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.sizes[pid] = [2]int{w, h}
}

// sizeFor is the pane this window last gave a shell, if it ever has.
func (cl *client) sizeFor(pid int) (w, h int, ok bool) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	s, ok := cl.sizes[pid]
	return s[0], s[1], ok
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

// screenMsg is the shell's pane as it now stands, with whatever the program in
// it has asked of the window it believes it is in.
func (t *terminal) screenMsg() message {
	x, y := t.cursor()
	title, progress := t.window()
	return message{
		Kind:       kindScreen,
		PID:        t.pid,
		Screen:     t.screen(),
		CursorX:    x,
		CursorY:    y,
		Title:      title,
		Progress:   progress,
		Scrollback: t.vt.ScrollbackLen(),
		MouseOn:    t.mouseWanted(),
		Alt:        t.vt.IsAltScreen(),
	}
}

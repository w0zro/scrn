package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The navigator's side of the server. It never touches a pty and never draws
// a shell: tmux does both, in the pane beside the navigator. This file is
// the session that talks to the server: asking what it holds, moving the
// shell under the cursor into the pane on the right and the last one back
// out, taking the keys to a shell, and hearing when shells come and go.

// socketPath is where conn's tmux server listens. It is per user and outside
// any project, because one server holds the shells for every repository. It
// honors XDG_STATE_HOME the way the config honors XDG_CONFIG_HOME.
func socketPath() string {
	if p := os.Getenv("CONN_SOCKET"); p != "" {
		return p
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, _ := os.UserHomeDir()
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "conn", "tmux-"+strconv.Itoa(os.Getuid())+".sock")
}

// sessionInfo is a shell the server is holding. Name is what the project
// that asked for it calls it, and empty for a shell opened by hand. Shown
// says it is the shell in the pane beside the navigator; Wanted that a chord
// opened it and asked for it to be shown.
type sessionInfo struct {
	PID    int
	Dir    string
	Name   string
	Shown  bool
	Wanted bool
}

// remoteTerm is a shell the server is holding, as the navigator sees it.
type remoteTerm struct {
	pid  int
	dir  string
	name string // what the project calls it, if a project asked for it
}

// Messages the session raises for the model.
type (
	// serverReadyMsg says the session is up, or explains why it is not.
	serverReadyMsg struct {
		session *session
		err     error
	}

	// termOpenedMsg is the shell this navigator just asked for.
	termOpenedMsg struct {
		pid  int
		dir  string
		name string
	}

	// sessionsMsg is the shells the server is holding.
	sessionsMsg struct {
		sessions []sessionInfo
	}

	// termGoneMsg says a shell has finished.
	termGoneMsg struct{ pid int }

	// serverLostMsg says the server hung this session up. With no error it is
	// the ordinary end of holding nothing — the last shell closed, the
	// session went, and the session object keeps watching for a new one.
	serverLostMsg struct{}

	// serverErrorMsg says the server could not do something it was asked to.
	// This is a report about one ask, not about the server.
	serverErrorMsg struct{ err error }
)

// reconnectMsg asks for another go at connecting, for the one failure the
// session cannot chase from inside: tmux itself missing.
type reconnectMsg struct{}

const (
	reconnectWait = 300 * time.Millisecond
	reconnectMax  = 5 * time.Second
)

// reconnect connects again after the wait.
func reconnect(after time.Duration) tea.Cmd {
	return tea.Tick(after, func(time.Time) tea.Msg { return reconnectMsg{} })
}

// probeEvery is how often a session with no server watches for one to
// appear — another window may hold shells this one cannot see yet.
const probeEvery = 2 * time.Second

// pane is one held shell as the session tracks it: which tmux pane it is,
// and what it is called.
type pane struct {
	id     string // "%3"
	pid    int
	dir    string
	name   string
	shown  bool // in the home window, beside the navigator
	wanted bool // opened by a chord that asked for it to be shown
}

// placement is one ask to arrange the home window: the shell to put beside
// the navigator — none, to leave the navigator the whole window — and
// whether the keys should go to it.
type placement struct {
	pid   int
	focus bool
}

// session is the navigator's connection to the tmux server holding the
// shells.
type session struct {
	events chan tea.Msg

	// run is one tmux command against conn's server; a seam for the tests,
	// which put a recorder here.
	run func(args ...string) (string, error)

	mu      sync.Mutex
	ctl     *ctlClient
	panes   map[int]*pane      // by the pid of the shell in the pane
	byPane  map[string]int     // pane id → that pid
	nav     string             // the navigator's own pane, "%0"
	column  int                // the navigator's width, read once: the goroutines never touch the global
	placing latest[placement]  // the arrangement asked for, made one at a time
	saying  latest[statusText] // what the status line is to read, said one at a time
	listing sync.Mutex         // one list is read and told at a time, so the newer is heard last
	probing bool
	closed  bool
	stopped chan struct{} // closed by close, so a watcher stops in its tracks

	// probe is how long the watch waits between looks for a server; the
	// tests set a pace that suits a test.
	probe time.Duration
}

func newSession() *session {
	return &session{
		events:  make(chan tea.Msg, 64),
		run:     tmuxCommand,
		panes:   map[int]*pane{},
		byPane:  map[string]int{},
		column:  navWidth,
		probe:   probeEvery,
		stopped: make(chan struct{}),
	}
}

// connectServer builds the session and has it find whatever is already held.
// The one unrecoverable failure is tmux not being installed: everything else
// the session chases on its own.
func connectServer() tea.Cmd {
	return func() tea.Msg {
		if _, err := exec.LookPath("tmux"); err != nil {
			return serverReadyMsg{err: errors.New("tmux is not installed")}
		}
		s := newSession()
		go s.connect()
		return serverReadyMsg{session: s}
	}
}

// connect attaches to a session that exists, or begins watching for one —
// shells opened by another window appear on their own.
func (s *session) connect() {
	if s.refreshList() {
		s.ensureCtl()
		return
	}
	s.watchForServer()
}

// watchForServer polls for a session appearing, one probe at a time. It is
// the quiet state of a navigator holding nothing.
func (s *session) watchForServer() {
	s.mu.Lock()
	if s.probing || s.closed {
		s.mu.Unlock()
		return
	}
	s.probing = true
	s.mu.Unlock()

	go func() {
		tick := time.NewTicker(s.probe)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
			case <-s.stopped:
			}
			// Whether the probe is over is decided in the same breath as
			// the probing flag is put down. A control client that hangs up
			// between the two — attached to a session that went in the
			// same instant — would ask for a probe while this one still
			// claimed to be running, and then nobody would be probing.
			s.mu.Lock()
			done := s.closed || s.ctl != nil
			if done {
				s.probing = false
			}
			s.mu.Unlock()
			if done {
				return
			}
			if _, err := s.run("has-session", "-t", tmuxSession); err == nil {
				s.refreshList()
				s.ensureCtl()
				// Not the end of the probe: the next pass is what sees
				// whether the client stayed attached.
			}
		}
	}()
}

// close lets go of the server. The shells stay held — this is a navigator
// finishing with them, not an end to them — and the session stops watching,
// stops probing, and will not attach again.
func (s *session) close() {
	s.mu.Lock()
	if !s.closed {
		close(s.stopped)
	}
	s.closed = true
	ctl := s.ctl
	s.ctl = nil
	s.mu.Unlock()
	if ctl != nil {
		ctl.close()
	}
}

// ensureCtl attaches the control-mode client if it is not already attached.
func (s *session) ensureCtl() {
	s.mu.Lock()
	if s.ctl != nil || s.closed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	ctl, err := startCtl(s.notify)
	if err != nil {
		s.watchForServer()
		return
	}
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.ctl = ctl
	}
	s.mu.Unlock()
	if closed {
		ctl.close()
	}
}

// notify is what the control stream tells the session.
func (s *session) notify(n ctlNote) {
	switch n.kind {
	case noteWindows:
		go s.refreshList()
	case noteError:
		s.events <- serverErrorMsg{err: errors.New(n.err)}
	case noteExit:
		s.mu.Lock()
		if s.ctl != nil {
			s.ctl.close()
			s.ctl = nil
		}
		s.mu.Unlock()
		s.events <- serverLostMsg{}
		s.watchForServer()
	}
}

// listFormat is the one line per pane the session reads the server's state
// through. The directory a shell was opened in is a pane option conn set;
// a pane conn never dressed — opened through a bare tmux attach — falls
// back to where it is working now. The navigator's own pane is told apart
// by its option, and a pane in the home window beside it is the shell
// shown there; the home window's option reaches its panes. A chord that
// opened a shell to be shown says so in the window's name, the one mark
// that is set in the same breath as the window is made — an option set
// after would race the refresh the new window sets off.
const listFormat = "#{pane_id}\t#{pane_pid}\t#{@conn_dir}\t#{@conn_name}\t#{pane_current_path}\t#{@conn_nav}\t#{@conn_home}\t#{window_name}"

// wantName is the window name that asks the navigator to show the shell
// in it; heldName is what the window is called once it has.
const (
	wantName = "conn-want"
	heldName = "shell"
)

// parseListing reads what list-panes said in listFormat: one record per
// shell, and the navigator's own pane, "" when there is none. Both readers
// of the server's state come through here, so the format is written once
// and read once.
func parseListing(out string) (held []*pane, nav string) {
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 8 {
			continue
		}
		if f[5] == "1" {
			nav = f[0]
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		// A pane conn never dressed reports no directory of its own, and
		// falls back to where it is working now.
		dir := f[2]
		if dir == "" {
			dir = f[4]
		}
		held = append(held, &pane{id: f[0], pid: pid, dir: dir, name: f[3],
			shown: f[6] == "1", wanted: f[6] != "1" && f[7] == wantName})
	}
	return held, nav
}

// info is the pane as the model hears about it.
func (p *pane) info() sessionInfo {
	return sessionInfo{PID: p.pid, Dir: p.dir, Name: p.name, Shown: p.shown, Wanted: p.wanted}
}

// refreshList reads what the server holds and tells the model, reporting
// whether a session was there to read. Shells that left are named going:
// the model clears its rows by the pid, not by the list.
func (s *session) refreshList() bool {
	// Held from the read to the telling: two reads in flight, each told
	// after its own unlock, could reach the model in the other's order,
	// and the model takes the list it hears last as the truth.
	s.listing.Lock()
	defer s.listing.Unlock()

	out, err := s.run("list-panes", "-a", "-F", listFormat)
	if err != nil && !errors.Is(err, errNoServer) {
		// A list that could not be read says nothing about the shells: a
		// tmux slow enough to time out under load is still holding them.
		// The last list stands, and the model hears nothing until the next
		// one is read.
		s.mu.Lock()
		held := len(s.panes) > 0
		s.mu.Unlock()
		return held
	}
	if err != nil {
		// No server is the one answer that means every shell is gone.
		s.mu.Lock()
		was := s.panes
		s.panes, s.byPane, s.nav = map[int]*pane{}, map[string]int{}, ""
		s.mu.Unlock()
		for pid := range was {
			s.events <- termGoneMsg{pid: pid}
		}
		s.events <- sessionsMsg{}
		return false
	}

	held := map[int]*pane{}
	byPane := map[string]int{}
	var infos []sessionInfo
	listed, nav := parseListing(out)
	for _, p := range listed {
		held[p.pid] = p
		byPane[p.id] = p.pid
		infos = append(infos, p.info())
	}

	// Which shells left is worked out under the lock and carried out as a
	// list of pids. Reading the new map after publishing it is reading a map
	// an open may be writing to in the same breath.
	var gone []int
	s.mu.Lock()
	for pid := range s.panes {
		if _, ok := held[pid]; !ok {
			gone = append(gone, pid)
		}
	}
	s.panes, s.byPane, s.nav = held, byPane, nav
	s.mu.Unlock()

	for _, pid := range gone {
		s.events <- termGoneMsg{pid: pid}
	}
	s.events <- sessionsMsg{sessions: infos}
	return true
}

// nextEvent waits for the session's next word. Exactly one of these is in
// flight, the same discipline the refresh tick keeps: each schedules the next.
func nextEvent(s *session) tea.Cmd {
	if s == nil {
		return nil
	}
	return func() tea.Msg {
		return <-s.events
	}
}

// paneBirth is what an open asks for back: the pane and the shell's pid.
const paneBirth = "#{pane_id} #{pane_pid}"

// runner is one tmux command against conn's server: a session's seam, or
// tmuxCommand itself for a one-shot caller.
type runner func(args ...string) (string, error)

// birth is a shell just opened: its pane, and the pid of the shell in it.
type birth struct {
	pane string
	pid  int
}

// createWindow opens a window in conn's session holding a shell — or, handed
// a command, that command with a shell waiting behind it — in dir, and pins
// the name and the opening directory on its pane so the list can tell a
// plan's web apart from a shell that wandered there. The window is where
// the shell waits until the navigator shows it; wanted asks the navigator
// to, as soon as it sees the window. The launcher is what brings the
// server up; a session that has gone in the meantime is made again around
// the first shell, without a home window, which `conn home` supplies when
// it is next asked for.
func createWindow(run runner, dir, command, name string, wanted bool) (birth, error) {
	cmd := ""
	if command != "" {
		// Under a shell, so the command is found on the user's own PATH,
		// and one that exits leaves the shell rather than the row
		// vanishing. The shell is the pane's own $SHELL, which tmux sets
		// to its default-shell whatever the asker's environment says: a
		// chord runs under run-shell, where SHELL is /bin/sh.
		cmd = command + `; exec "$SHELL"`
	}
	winName := heldName
	if wanted {
		winName = wantName
	}

	args := []string{"new-window", "-d", "-P", "-t", tmuxSession + ":",
		"-F", paneBirth, "-n", winName, "-c", dir}
	if _, err := run("has-session", "-t", tmuxSession); err != nil {
		// The session is made again around a placeholder, and the shell
		// opens in it the way every shell does: a session's first window
		// cannot carry the terminal's name, and a new window can. The
		// placeholder is a sleep, not a shell, and goes once the shell is
		// there. Two askers finding no session in the same instant make
		// one between them, and the other's shell still opens in it.
		_ = os.MkdirAll(filepath.Dir(socketPath()), 0o700)
		_, err := run("new-session", "-d", "-s", tmuxSession, "-n", bornName, "-c", "/", "sleep 30")
		if err == nil {
			defer func() { _, _ = run("kill-window", "-t", tmuxSession+":"+bornName) }()
		} else if !errors.Is(err, errDuplicateSession) {
			return birth{}, err
		}
	}
	// The pane is told which terminal is around tmux, for the programs
	// that speak to one. tmux sets the names itself, after the session's
	// environment, so they go with the window rather than the server —
	// which is asked once it is there to ask.
	for _, kv := range outerTerminal(run) {
		args = append(args, "-e", kv)
	}
	if cmd != "" {
		args = append(args, cmd)
	}

	out, err := run(args...)
	if err != nil {
		return birth{}, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return birth{}, errors.New("tmux said " + out)
	}
	pid, err := strconv.Atoi(f[1])
	if err != nil {
		return birth{}, errors.New("tmux said " + out)
	}
	_, _ = run("set", "-p", "-t", f[0], "@conn_dir", dir, ";",
		"set", "-p", "-t", f[0], "@conn_name", name)
	return birth{pane: f[0], pid: pid}, nil
}

// open starts a shell — or handed a command, that command with a shell
// waiting behind it — in dir, held by the server, and tells the model when
// it is there.
func (s *session) open(dir, run, name string) {
	if s == nil {
		return
	}
	go func() {
		b, err := createWindow(s.run, dir, run, name, false)
		if err != nil {
			s.events <- serverErrorMsg{err: err}
			return
		}

		s.mu.Lock()
		s.panes[b.pid] = &pane{id: b.pane, pid: b.pid, dir: dir, name: name}
		s.byPane[b.pane] = b.pid
		s.mu.Unlock()

		s.refreshList()
		s.ensureCtl()
		s.events <- termOpenedMsg{pid: b.pid, dir: dir, name: name}
	}()
}

// list asks for what the server holds, which comes back as a sessionsMsg.
func (s *session) list() {
	if s == nil {
		return
	}
	go s.refreshList()
}

// show puts a shell in the pane beside the navigator and takes the keys to
// it. tmux draws it there; the navigator keeps its column.
func (s *session) show(pid int) {
	if s == nil {
		return
	}
	s.place(placement{pid: pid, focus: true})
}

// preview puts a shell in the pane beside the navigator without taking the
// keys from wherever they are — the cursor has landed on its row. Zero puts
// the shown shell back in a window of its own and leaves the navigator the
// whole window: the cursor is on a row with no shell to show, or the
// navigator has something of its own to draw there.
func (s *session) preview(pid int) {
	if s == nil {
		return
	}
	s.place(placement{pid: pid})
}

// place asks for one arrangement of the home window, coalescing: one is made
// at a time, and an ask arriving while one is being made replaces any ask
// still waiting. A cursor moving down a column of shells becomes a handful
// of arrangements, each superseding the last, rather than one per row it
// crossed — and they are made in the order asked, so the pane ends up
// holding the shell the cursor stopped on.
func (s *session) place(p placement) {
	s.placing.ask(p, func(p placement) {
		if err := s.arrange(p); err != nil {
			s.events <- serverErrorMsg{err: err}
		}
	})
}

// latest holds the asks for one kind of work that is done one at a time
// and only ever needs its newest ask: one is done at a time, and an ask
// arriving while one is being done replaces any ask still waiting.
type latest[T any] struct {
	mu   sync.Mutex
	want *T   // the ask not yet done
	busy bool // an ask is being done
}

// ask queues v, superseding any ask still waiting, and sees to it that do
// is run — on its own goroutine, once per ask that was not superseded, in
// the order asked.
func (l *latest[T]) ask(v T, do func(T)) {
	l.mu.Lock()
	l.want = &v
	if l.busy {
		l.mu.Unlock()
		return
	}
	l.busy = true
	l.mu.Unlock()

	go func() {
		for {
			l.mu.Lock()
			next := l.want
			l.want = nil
			if next == nil {
				l.busy = false
				l.mu.Unlock()
				return
			}
			l.mu.Unlock()
			do(*next)
		}
	}()
}

// arrange makes one arrangement: the shell asked for moves into the pane
// beside the navigator, and whichever shell was there moves into the
// window it left. What the home window holds is asked of tmux as it
// stands rather than remembered, so an arrangement made by a chord or by
// a shell closing is built on, not fought.
func (s *session) arrange(p placement) error {
	s.mu.Lock()
	nav := s.nav
	target := ""
	if p.pid != 0 {
		if t := s.panes[p.pid]; t != nil {
			target = t.id
		}
	}
	s.mu.Unlock()
	if nav == "" {
		return errors.New("no navigator pane to arrange around")
	}
	if p.pid != 0 && target == "" {
		return nil // gone since it was asked for; the list will say so
	}
	if err := showPane(s.run, nav, target, s.column); err != nil {
		return err
	}
	if p.focus && target != "" {
		_, err := s.run("select-window", "-t", nav, ";", "select-pane", "-t", target)
		return err
	}
	return nil
}

// showPane puts the target pane beside the navigator's pane nav — or, with
// no target, puts whatever is there back in a window of its own. It reads
// the home window first, so it is right about what is there whoever last
// changed it. The layout is main-vertical with the navigator as the main
// pane, which is what holds the navigator's width when the window is
// resized: the configuration re-applies it on every resize.
//
// A shell keeps its size as it moves. It joins at the slot's width in one
// step rather than at half the window and then the slot, and the window it
// goes back to is sized to the slot as it goes — a window sized by hand
// stays that size whatever the client does — so a program in it sees no
// change in its terminal, and has nothing to repaint, as the cursor moves
// over its row and off again. A shell's first showing is the one resize:
// its window was made at the client's size. column is the navigator's
// width, which the slot is the rest of the window past.
func showPane(run runner, nav, target string, column int) error {
	out, err := run("list-panes", "-t", nav, "-F", "#{pane_id}\t#{@conn_nav}\t#{window_width}\t#{window_height}")
	if err != nil {
		return err
	}
	shown := ""
	var slot []string // -x width -y height of the slot, when known
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[0] == "" {
			continue
		}
		if f[1] != "1" && shown == "" {
			shown = f[0]
		}
		w, werr := strconv.Atoi(f[2])
		h, herr := strconv.Atoi(f[3])
		if werr == nil && herr == nil && w > column+1 && h > 0 {
			slot = []string{"-x", strconv.Itoa(w - column - 1), "-y", strconv.Itoa(h)}
		}
	}
	// park sizes the window a pane has just gone back to: the pane names
	// its window, and the window takes the slot's size.
	park := func(pane string) []string {
		if slot == nil {
			return nil
		}
		return append([]string{";", "resize-window", "-t", pane}, slot...)
	}
	switch {
	case target == shown:
		return nil
	case target == "":
		args := []string{"break-pane", "-d", "-n", heldName, "-s", shown}
		_, err = run(append(args, park(shown)...)...)
	case shown == "":
		// Shown is what a window named for wanting asked; the name goes
		// first, while the pane is still there to name the window by —
		// joined, its window closes behind it.
		args := []string{"rename-window", "-t", target, heldName, ";",
			"join-pane", "-h", "-d"}
		if slot != nil {
			args = append(args, "-l", slot[1])
		}
		args = append(args, "-s", target, "-t", nav, ";",
			"select-layout", "-t", nav, "main-vertical")
		_, err = run(args...)
	default:
		// -d: the keys stay where they are. Without it the pane swapped in
		// becomes the active one whatever was active before, and a glance
		// from the list would hand the next keystroke to the shell.
		args := []string{"rename-window", "-t", target, heldName, ";",
			"swap-pane", "-d", "-s", target, "-t", shown}
		_, err = run(append(args, park(shown)...)...)
	}
	return err
}

// help shows the keys in a popup over the window; the popup takes the
// next key and goes.
func (s *session) help() {
	if s == nil {
		return
	}
	go func() {
		if err := showKeys(s.run, connExe(), ""); err != nil {
			s.events <- serverErrorMsg{err: err}
		}
	}()
}

// leave detaches the client this navigator is drawn in. The shells keep
// running; `conn` attaches again.
func (s *session) leave() {
	if s == nil {
		return
	}
	go func() { _, _ = s.run("detach-client") }()
}

// closeTerm ends a shell the server holds. Killing the pane takes its
// terminal away, which is the hangup everything in it listens to.
func (s *session) closeTerm(pid int) {
	if s == nil {
		return
	}
	p := s.pane(pid)
	if p == nil {
		return
	}
	go func() { _, _ = s.run("kill-pane", "-t", p.id) }()
}

// replace ends the server and every shell it holds. Nothing reaches this by
// accident: it takes the same second key any other kill takes.
func (s *session) replace() {
	if s == nil {
		return
	}
	go func() { _, _ = s.run("kill-server") }()
}

// dress names a shell's pane — its place, what runs there, and its mark —
// for the terminal's title while the keys are in it.
func (s *session) dress(pid int, name string) {
	if s == nil {
		return
	}
	p := s.pane(pid)
	if p == nil {
		return
	}
	go func() { _, _ = s.run("set", "-p", "-t", p.id, "@conn_title", name) }()
}

// statusText is what the navigator has the status line read: the mode
// its keys are in, when it has one to name, and what it has to say.
type statusText struct {
	mode, msg string
}

// say hands tmux the navigator's part of the status line. Said one at a
// time and superseded while waiting, so a query being typed lands in the
// order it was typed and the line ends up reading the last of it; and the
// line is refreshed at once rather than on its next tick, because a mode
// that lags the keys is a mode that lies.
func (s *session) say(t statusText) {
	if s == nil {
		return
	}
	s.saying.ask(t, func(t statusText) {
		_, _ = s.run("set", "-g", "@conn_mode", t.mode, ";",
			"set", "-g", "@conn_msg", t.msg, ";",
			"refresh-client", "-S")
	})
}

func (s *session) pane(pid int) *pane {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.panes[pid]
}

// scrollbackLines is how many lines of transcript each shell keeps once they
// scroll off its pane. It is written into the server's configuration at
// launch, so raising it takes R — a fresh server — to reach anything.
var scrollbackLines = 10000

// applyScrollback sets the transcript cap from the config. Zero — unset —
// leaves the default standing.
func applyScrollback(n int) {
	if n > 0 {
		scrollbackLines = n
	}
}

// bornName is the placeholder window a session is remade around, gone
// as soon as the first shell is in.
const bornName = "conn-born"

// outerTerminal is the terminal around tmux, as the TERM_PROGRAM and
// TERM_PROGRAM_VERSION the server was started under: the launcher's, kept
// in the server's global environment. tmux tells every pane its terminal
// is tmux, and Claude Code takes it at its word — the progress it reports
// while it works, which Ghostty draws in the title bar, is offered only to
// a terminal it knows will. Told the outer name, it sees through tmux and
// wraps what it says for passing through, which the configuration allows.
// A server started inside another tmux has nothing to pass on.
func outerTerminal(run runner) []string {
	var kvs []string
	for _, name := range []string{"TERM_PROGRAM", "TERM_PROGRAM_VERSION"} {
		out, err := run("show-environment", "-g", name)
		if err != nil {
			continue
		}
		_, v, ok := strings.Cut(strings.TrimSpace(out), "=")
		if !ok || v == "" {
			continue
		}
		if name == "TERM_PROGRAM" && v == "tmux" {
			return nil
		}
		kvs = append(kvs, name+"="+v)
	}
	return kvs
}

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The navigator's side of the server. It never touches a pty and never draws
// a shell for real — tmux does both, in the window the shell lives in. This
// file is the session that talks to the server: asking for windows, taking
// the client to one, hearing which shells are held, and reading a shell's
// screen for the preview beside the list.

// socketPath is where scrn's tmux server listens. It is per user and outside
// any project, because one server holds the shells for every repository. It
// honors XDG_STATE_HOME the way the config honors XDG_CONFIG_HOME.
func socketPath() string {
	if p := os.Getenv("SCRN_SOCKET"); p != "" {
		return p
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, _ := os.UserHomeDir()
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "scrn", "tmux-"+strconv.Itoa(os.Getuid())+".sock")
}

// sessionInfo is a shell the server is holding. Name is what the project
// that asked for it calls it, and empty for a shell opened by hand.
type sessionInfo struct {
	PID  int
	Dir  string
	Name string
}

// remoteTerm is a shell the server is holding, as the navigator sees it: the
// last screen captured for the preview.
type remoteTerm struct {
	pid    int
	dir    string
	name   string // what the project calls it, if a project asked for it
	screen string
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

	// screenMsg is one shell's pane as it now stands.
	screenMsg struct {
		pid    int
		screen string
	}

	// termGoneMsg says a shell has finished.
	termGoneMsg struct{ pid int }

	// serverLostMsg says the server hung this session up. With no error it is
	// the ordinary end of holding nothing — the last shell closed, the
	// session went, and the session object keeps watching for a new one.
	serverLostMsg struct{ err error }

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

// pane is one held shell as the session tracks it: which tmux pane it is
// and which window holds it.
type pane struct {
	id   string // "%3"
	win  string // "@3", the window holding it — one pane to a window
	pid  int
	dir  string
	name string
}

// session is the navigator's connection to the tmux server holding the
// shells.
type session struct {
	events chan tea.Msg

	// run is one tmux command against scrn's server; a seam for the tests,
	// which put a recorder here.
	run func(args ...string) (string, error)

	mu       sync.Mutex
	ctl      *ctlClient
	panes    map[int]*pane   // by the pid of the shell in the pane
	byPane   map[string]int  // pane id → that pid
	watching map[int]bool    // pids whose screens the preview shows
	dirty    map[string]bool // panes that drew during their capture
	live     map[string]bool // captures in flight, by pane id
	probing  bool
	closed   bool

	// probe is how long the watch waits between looks for a server; the
	// tests set a pace that suits a test.
	probe time.Duration
}

func newSession() *session {
	return &session{
		events:   make(chan tea.Msg, 64),
		run:      tmuxCommand,
		panes:    map[int]*pane{},
		byPane:   map[string]int{},
		watching: map[int]bool{},
		dirty:    map[string]bool{},
		live:     map[string]bool{},
		probe:    probeEvery,
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
		for {
			time.Sleep(s.probe)
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
	case noteOutput:
		// Every pane's output rings here, watched or not: a build pouring
		// text in a shell nobody is previewing still crosses the control
		// stream, octal-escaped, to be dropped. tmux could be asked not to
		// send it — refresh-client -A pane:off — but once every client has
		// turned a pane off tmux stops reading the pane, and a program in
		// it blocks the moment its output buffer fills. That is a shell
		// stalling because nobody is looking at it, which is the one thing
		// held shells must never do; the traffic is the price.
		s.mu.Lock()
		pid, known := s.byPane[n.pane]
		watched := known && s.watching[pid]
		s.mu.Unlock()
		if watched {
			s.captureSoon(n.pane)
		}
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

// captureSoon schedules a capture of a pane, coalescing: one in flight per
// pane, and output during a capture means one more capture, not one per
// write. A build pouring text becomes a stream of snapshots, each
// superseding the last.
func (s *session) captureSoon(paneID string) {
	s.mu.Lock()
	if s.live[paneID] {
		s.dirty[paneID] = true
		s.mu.Unlock()
		return
	}
	s.live[paneID] = true
	s.mu.Unlock()

	go func() {
		for {
			s.capture(paneID)
			s.mu.Lock()
			again := s.dirty[paneID]
			delete(s.dirty, paneID)
			if !again {
				delete(s.live, paneID)
			}
			s.mu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// paneMeta is what one capture asks display for, beside the screen itself:
// the pane's size, which the screen is padded to.
const paneMeta = "#{pane_width} #{pane_height}"

// capture reads a pane's screen as it stands and sends it to the model for
// the preview. The screen and its size come back from one command so they
// describe the same moment; the size is last because the commands run in
// order.
func (s *session) capture(paneID string) {
	s.mu.Lock()
	pid, ok := s.byPane[paneID]
	s.mu.Unlock()
	if !ok {
		return
	}

	out, err := s.run("capture-pane", "-e", "-p", "-t", paneID,
		";", "display", "-p", "-t", paneID, "-F", paneMeta)
	if err != nil {
		return // the pane may be mid-close; the list refresh will say
	}
	lines := strings.Split(out, "\n")
	meta := lines[len(lines)-1]
	rows := selfContained(lines[:len(lines)-1])

	f := strings.Fields(meta)
	if len(f) != 2 {
		return
	}
	width, _ := strconv.Atoi(f[0])
	height, _ := strconv.Atoi(f[1])
	s.events <- screenMsg{pid: pid, screen: padScreen(rows, width, height)}
}

// sgrSeq matches one SGR sequence, which is the only styling capture-pane
// emits.
var sgrSeq = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// selfContained gives every row its whole styling. capture-pane writes a
// stream: an attribute is said once and runs until changed, across line
// breaks — a full-width background says nothing at all on the lines after
// its first. scrn cuts, pads, resets and recomposes rows one by one, so
// each must carry its own state: the pen left open at the end of one row
// is restated at the head of the next.
func selfContained(rows []string) []string {
	pen := ""
	for i, row := range rows {
		if pen != "" {
			rows[i] = pen + row
		}
		for _, seq := range sgrSeq.FindAllString(row, -1) {
			pen = writePen(pen, seq)
		}
	}
	return rows
}

// writePen is the pen after one more SGR sequence. A reset empties it;
// anything else is kept in order, because replaying the pen as it was said
// is what makes the restatement true. A leading zero is a reset with more
// to say, so the rest of it starts a fresh pen.
func writePen(pen, seq string) string {
	params := seq[2 : len(seq)-1]
	switch {
	case params == "" || params == "0":
		return ""
	case strings.HasPrefix(params, "0;"):
		return "\x1b[" + params[2:] + "m"
	}
	return pen + seq
}

// padScreen makes capture output into a grid: exactly height rows, every row
// exactly width columns. capture trims what a real terminal would not miss,
// and the preview cuts rows to the pane it has, so every row has to be as
// wide as the shell's.
func padScreen(rows []string, width, height int) string {
	if height > 0 && len(rows) > height {
		rows = rows[:height]
	}
	for height > 0 && len(rows) < height {
		rows = append(rows, "")
	}
	for i, row := range rows {
		if pad := width - ansi.StringWidth(row); pad > 0 {
			// Reset before padding: the padding is scrn's blank, not more of
			// whatever background the row's last cell left open.
			rows[i] = row + ansi.ResetStyle + strings.Repeat(" ", pad)
		}
	}
	return strings.Join(rows, "\n")
}

// listFormat is the one line per shell the session reads the server's state
// through. The directory a shell was opened in is a pane option scrn set;
// a pane scrn never dressed — opened through a bare tmux attach — falls
// back to where it is working now. The home window is not a shell and is
// left out by its option.
const listFormat = "#{pane_id}\t#{window_id}\t#{pane_pid}\t#{@scrn_dir}\t#{@scrn_name}\t#{pane_current_path}\t#{@scrn_home}"

// parseListing reads what list-panes said in listFormat, one record per
// shell. Both readers of the server's state come through here, so the format
// is written once and read once.
func parseListing(out string) []*pane {
	var held []*pane
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 7 || f[6] == "1" {
			continue
		}
		pid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		// A pane scrn never dressed reports no directory of its own, and
		// falls back to where it is working now.
		dir := f[3]
		if dir == "" {
			dir = f[5]
		}
		held = append(held, &pane{id: f[0], win: f[1], pid: pid, dir: dir, name: f[4]})
	}
	return held
}

// info is the pane as the model hears about it.
func (p *pane) info() sessionInfo {
	return sessionInfo{PID: p.pid, Dir: p.dir, Name: p.name}
}

// refreshList reads what the server holds and tells the model, reporting
// whether a session was there to read. Shells that left are named going:
// the model clears the preview by the pid, not by the list.
func (s *session) refreshList() bool {
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
		s.panes, s.byPane = map[int]*pane{}, map[string]int{}
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
	for _, p := range parseListing(out) {
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
	s.panes, s.byPane = held, byPane
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

// paneBirth is what an open asks for back: the pane, the window holding it,
// and the shell's pid.
const paneBirth = "#{pane_id} #{window_id} #{pane_pid}"

// runner is one tmux command against scrn's server: a session's seam, or
// tmuxCommand itself for a one-shot caller.
type runner func(args ...string) (string, error)

// birth is a shell just opened: its pane, the window holding it, and the
// pid of the shell in it.
type birth struct {
	pane string
	win  string
	pid  int
}

// createWindow opens a window in scrn's session holding a shell — or, handed
// a command, that command with a shell waiting behind it — in dir, and pins
// the name and the opening directory on its pane so the list can tell a
// plan's web apart from a shell that wandered there. The launcher is what
// brings the server up; a session that has gone in the meantime is made
// again around the first shell, without a home window, which `scrn home`
// supplies when it is next asked for.
func createWindow(run runner, dir, command, name string) (birth, error) {
	cmd := ""
	if command != "" {
		// Under a shell, so the command is found on the user's own PATH,
		// and one that exits leaves the shell rather than the row
		// vanishing.
		cmd = command + "; exec " + shellCommand()
	}

	another := []string{"new-window", "-d", "-P", "-t", tmuxSession + ":",
		"-F", paneBirth, "-c", dir}
	args := another
	if _, err := run("has-session", "-t", tmuxSession); err != nil {
		_ = os.MkdirAll(filepath.Dir(socketPath()), 0o700)
		args = []string{"new-session", "-d", "-s", tmuxSession, "-P", "-F", paneBirth, "-c", dir}
	}
	if cmd != "" {
		another = append(another, cmd)
		args = append(args, cmd)
	}

	out, err := run(args...)
	if err != nil && strings.Contains(err.Error(), "duplicate session") {
		// Two askers found no session in the same instant, and one of
		// them made it. The other's shell still opens — as every shell
		// after the first does.
		out, err = run(another...)
	}
	if err != nil {
		return birth{}, err
	}
	f := strings.Fields(out)
	if len(f) != 3 {
		return birth{}, errors.New("tmux said " + out)
	}
	pid, err := strconv.Atoi(f[2])
	if err != nil {
		return birth{}, errors.New("tmux said " + out)
	}
	_, _ = run("set", "-p", "-t", f[0], "@scrn_dir", dir, ";",
		"set", "-p", "-t", f[0], "@scrn_name", name)
	return birth{pane: f[0], win: f[1], pid: pid}, nil
}

// open starts a shell — or handed a command, that command with a shell
// waiting behind it — in dir, held by the server, and tells the model when
// it is there.
func (s *session) open(dir, run, name string) {
	if s == nil {
		return
	}
	go func() {
		b, err := createWindow(s.run, dir, run, name)
		if err != nil {
			s.events <- serverErrorMsg{err: err}
			return
		}

		s.mu.Lock()
		s.panes[b.pid] = &pane{id: b.pane, win: b.win, pid: b.pid, dir: dir, name: name}
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

// show takes the client to a shell's window. tmux draws it from there; the
// navigator keeps running in the home window behind it.
func (s *session) show(pid int) {
	if s == nil {
		return
	}
	p := s.pane(pid)
	if p == nil {
		return
	}
	go func() {
		if _, err := s.run("select-window", "-t", p.win); err != nil {
			s.events <- serverErrorMsg{err: err}
		}
	}()
}

// leave detaches the client this navigator is drawn in. The shells keep
// running; `scrn` attaches again.
func (s *session) leave() {
	if s == nil {
		return
	}
	go func() { _, _ = s.run("detach-client") }()
}

// attach starts previewing a shell's screen, beginning with the screen as it
// stands.
func (s *session) attach(pid int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.watching[pid] = true
	p := s.panes[pid]
	s.mu.Unlock()
	if p != nil {
		s.captureSoon(p.id)
	}
}

// detach stops previewing a shell without touching it.
func (s *session) detach(pid int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.watching, pid)
	s.mu.Unlock()
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

func (s *session) pane(pid int) *pane {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.panes[pid]
}

// lines is the screen the server last sent, one string per row.
func (t *remoteTerm) lines(height int) []string {
	rows := strings.Split(t.screen, "\n")
	if len(rows) > height {
		rows = rows[:height]
	}
	return rows
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

// shellCommand is the shell to open, honoring $SHELL so scrn opens the one
// the user actually uses.
func shellCommand() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

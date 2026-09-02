package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The client side of the split. It never touches a pty: the shells live in
// scrn's private tmux server, and this file is the session that talks to it —
// asking for shells, forwarding what the user did, and turning what tmux says
// into the messages the model reads.

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

// keyPress is a keystroke as the window saw it: which key, which modifiers,
// and what it types if it types anything. It is carried as the event it was
// rather than as bytes, because the bytes depend on modes only the far side
// of the pane knows — tmux's business now, reached through tmuxKeyLines.
type keyPress struct {
	Code rune
	Text string
	Mod  int
}

// mousePress is a mouse event in the pane's own coordinates, which is what
// the program drawing there believes it is being told about.
type mousePress struct {
	X      int
	Y      int
	Button int
	Mod    int
	Action string
}

// What a mouse can be doing.
const (
	actPress   = "press"
	actRelease = "release"
	actMotion  = "motion"
)

// sessionInfo is a shell the server is holding. Name is what the project
// that asked for it calls it, and empty for a shell opened by hand.
type sessionInfo struct {
	PID  int
	Dir  string
	Name string
}

// remoteTerm is a shell the server is holding, as the client sees it: the
// last screen it sent and where the cursor was.
type remoteTerm struct {
	pid    int
	dir    string
	name   string // what the project calls it, if a project asked for it
	screen string
	curX   int
	curY   int

	// What the last screen said about the pane: how much has scrolled off the
	// top, whether the program wants the mouse, and whether it is on the
	// alternate screen. They decide whether a wheel turn is the program's,
	// the transcript's, or — on the alternate screen — a pair of arrow keys.
	sb    int
	mouse bool
	alt   bool

	// What the program in it has asked of the terminal window. scrn is the
	// one with a window, so it is scrn that has to ask for it.
	title    string
	progress string
}

// Messages the client raises for the model.
type (
	// daemonReadyMsg says the session is up, or explains why it is not.
	daemonReadyMsg struct {
		session *session
		err     error
	}

	// termOpenedMsg is the shell this window just asked for.
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
		pid      int
		screen   string
		curX     int
		curY     int
		title    string
		progress string
		sb       int
		mouse    bool
		alt      bool
	}

	// historyMsg is a shell's transcript, asked for when someone starts
	// reading back through it.
	historyMsg struct {
		pid     int
		history string
	}

	// termGoneMsg says a shell has finished.
	termGoneMsg struct{ pid int }

	// daemonLostMsg says the server hung this window up. With no error it is
	// the ordinary end of holding nothing — the last shell closed, the
	// session went, and the session object keeps watching for a new one.
	daemonLostMsg struct{ err error }

	// daemonErrorMsg says the server could not do something it was asked to.
	// This is a report about one ask, not about the server.
	daemonErrorMsg struct{ err error }
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
// and what the last capture said about its modes.
type pane struct {
	id   string // "%3"
	win  string // "@3", the window holding it — one pane to a window
	pid  int
	dir  string
	name string
	sgr  bool // the program listens for the mouse in SGR
}

// session is the client's connection to the tmux server holding the shells.
type session struct {
	events chan tea.Msg

	// run is one tmux command against scrn's server; a seam for the tests,
	// which put a recorder here.
	run func(args ...string) (string, error)

	mu       sync.Mutex
	ctl      *ctlClient
	panes    map[int]*pane   // by the pid of the shell in the pane
	byPane   map[string]int  // pane id → that pid
	watching map[int]bool    // pids whose screens this window renders
	dirty    map[string]bool // panes that drew during their capture
	live     map[string]bool // captures in flight, by pane id
	width    int             // the pane size this window declared,
	height   int             // which is what every capture is padded to
	fg, bg   string          // the real terminal's colors, as "#rrggbb"
	probing  bool
	closed   bool
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
	}
}

// connectDaemon builds the session and has it find whatever is already held.
// The one unrecoverable failure is tmux not being installed: everything else
// the session chases on its own.
func connectDaemon() tea.Cmd {
	return func() tea.Msg {
		if _, err := exec.LookPath("tmux"); err != nil {
			return daemonReadyMsg{err: errors.New("tmux is not installed")}
		}
		s := newSession()
		go s.connect()
		return daemonReadyMsg{session: s}
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
// the quiet state of a scrn holding nothing.
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
			time.Sleep(probeEvery)
			s.mu.Lock()
			done := s.closed || s.ctl != nil
			s.mu.Unlock()
			if done {
				break
			}
			if _, err := s.run("has-session", "-t", tmuxSession); err == nil {
				s.refreshList()
				s.ensureCtl()
				break
			}
		}
		s.mu.Lock()
		s.probing = false
		s.mu.Unlock()
	}()
}

// close lets go of the server. The shells stay held — this is a window
// finishing with them, not an end to them — and the session stops watching,
// stops probing, and will not attach again. Without it a session outlives
// what made it and goes looking for whatever server it can find.
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
		return
	}
	s.declareSizes()
	// The server this window found may be another window's; it answers for
	// the terminal's colors either way, so it is told what this one sees —
	// and a program's OSC 52 must land in a buffer wherever the server
	// came from, so the clipboard option is said again, idempotently.
	s.applyTheme()
	go func() { _, _ = s.run("set", "-g", "set-clipboard", "on") }()
}

// theme records the real terminal's colors and carries them to the server.
// The server needs them to answer a pane asking OSC 10 or 11 — what color
// the terminal is — which is how programs pick their theming: the old
// emulator answered from its own defaults, and a server with no styles set
// answers nothing at all, which reads as a terminal with no opinion.
func (s *session) theme(fg, bg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	changed := fg != s.fg || bg != s.bg
	s.fg, s.bg = fg, bg
	s.mu.Unlock()
	if changed {
		go s.applyTheme()
	}
}

// applyTheme sets the server's window-style to the terminal's own colors.
// tmux answers OSC 10 and 11 from it, and a real client on the escape
// hatch paints panes the way scrn's terminal would. It never lands in the
// grid, so captures stay as they were. No server yet is fine: the colors
// ride into the creation chain when the first shell brings one up.
func (s *session) applyTheme() {
	style := s.themeStyle()
	if style == "" {
		return
	}
	_, _ = s.run("set", "-g", "window-style", style)
}

// themeStyle is the window-style the terminal's colors spell, or nothing
// while they are unknown.
func (s *session) themeStyle() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	if s.fg != "" {
		parts = append(parts, "fg="+s.fg)
	}
	if s.bg != "" {
		parts = append(parts, "bg="+s.bg)
	}
	return strings.Join(parts, ",")
}

// notify is what the control stream tells the session.
func (s *session) notify(n ctlNote) {
	switch n.kind {
	case noteOutput:
		s.mu.Lock()
		pid, known := s.byPane[n.pane]
		watched := known && s.watching[pid]
		s.mu.Unlock()
		if watched {
			s.captureSoon(n.pane)
		}
	case noteWindows:
		go s.refreshList()
	case noteClientGone:
		go s.declareSizes()
	case notePaste:
		// A program inside a pane copied — OSC 52, caught by the server —
		// and the copy belongs on the system clipboard, the same place it
		// would have landed with no scrn in the middle. scrn's own paste
		// buffer is the one changing that means nothing of the sort.
		if n.buffer != "scrn-paste" {
			go s.carryCopy(n.buffer)
		}
	case noteError:
		s.events <- daemonErrorMsg{err: errors.New(n.err)}
	case noteExit:
		s.mu.Lock()
		if s.ctl != nil {
			s.ctl.close()
			s.ctl = nil
		}
		s.mu.Unlock()
		s.events <- daemonLostMsg{}
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

// paneMeta is what one capture asks display for, beside the screen itself.
const paneMeta = "#{cursor_x} #{cursor_y} #{alternate_on} " +
	"#{mouse_any_flag} #{mouse_sgr_flag} #{history_size} " +
	"#{pane_width} #{pane_height} #{pane_title}"

// capture reads a pane's screen as it stands and sends it to the model. The
// screen and its facts come back from one command so they describe the same
// moment; the meta line is last because the commands run in order.
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
	if len(lines) < 1 {
		return
	}
	meta := lines[len(lines)-1]
	rows := selfContained(lines[:len(lines)-1])

	f := strings.SplitN(meta, " ", 9)
	if len(f) < 8 {
		return
	}
	curX, _ := strconv.Atoi(f[0])
	curY, _ := strconv.Atoi(f[1])
	alt := f[2] == "1"
	mouse := f[3] == "1"
	sgr := f[4] == "1"
	sb, _ := strconv.Atoi(f[5])
	width, _ := strconv.Atoi(f[6])
	height, _ := strconv.Atoi(f[7])
	title := ""
	if len(f) > 8 {
		title = f[8]
	}

	s.mu.Lock()
	if p := s.panes[pid]; p != nil {
		p.sgr = sgr
	}
	s.mu.Unlock()

	s.events <- screenMsg{
		pid:    pid,
		screen: padScreen(rows, width, height),
		curX:   curX, curY: curY,
		title: title,
		sb:    sb, mouse: mouse, alt: alt,
	}
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

// padScreen makes capture output into the grid the client's cursor-cutting
// leans on: exactly height rows, every row exactly width columns. capture
// trims what a real terminal would not miss, and the cursor cannot be cut
// into a cell a row does not carry.
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
// back to where it is working now.
const listFormat = "#{pane_id}\t#{window_id}\t#{pane_pid}\t#{@scrn_dir}\t#{@scrn_name}\t#{pane_current_path}"

// parseListing reads what list-panes said in listFormat, one record per
// shell. Both readers of the server's state come through here, so the format
// is written once and read once — the drift that costs is a field added to
// the format and counted in only one of the two places that count fields.
func parseListing(out string) []*pane {
	var held []*pane
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 6 {
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
// the model clears focus and scroll by the pid, not by the list.
func (s *session) refreshList() bool {
	out, err := s.run("list-panes", "-a", "-F", listFormat)
	if err != nil {
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
	for pid, old := range s.panes {
		if p, ok := held[pid]; ok {
			p.sgr = old.sgr
		} else {
			gone = append(gone, pid)
		}
	}
	s.panes, s.byPane = held, byPane
	s.mu.Unlock()

	// A shell this window had not seen — its own, just opened, or one
	// another window opened — has to be told what size to be.
	s.declareSizes()

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
// and the shell's pid. The window comes back with the rest because a shell
// has to be told its size before it draws, and the size is said per window.
const paneBirth = "#{pane_id} #{window_id} #{pane_pid}"

// open starts a shell — or handed a command, that command with a shell
// waiting behind it — in dir, held by the server. The first shell is what
// creates the session, and the session is what the control client attaches
// to, in that order.
func (s *session) open(dir, run, name string, w, h int) {
	if s == nil {
		return
	}
	s.setSize(w, h)
	go func() {
		cmd := ""
		if run != "" {
			// Under a shell, so the command is found on the user's own PATH,
			// and one that exits leaves the shell rather than the row
			// vanishing — the same wrapper the old daemon built.
			cmd = run + "; exec " + shellCommand()
		}

		args := []string{"new-window", "-d", "-P", "-t", tmuxSession + ":",
			"-F", paneBirth, "-c", dir}
		if _, err := s.run("has-session", "-t", tmuxSession); err != nil {
			// The first shell brings the server up around it. The options
			// ride in the same invocation: the transcript cap has to stand
			// before the first pane exists to keep any, and the terminal's
			// colors before the first program asks what color it is.
			//
			// The socket's directory is scrn's to make: tmux creates the
			// socket but not the directory around it, and on a machine that
			// has never run scrn there is no ~/.local/state/scrn to put it
			// in. A directory that cannot be made is left for the creation
			// to complain about, in tmux's words.
			_ = os.MkdirAll(filepath.Dir(socketPath()), 0o700)
			args = []string{"start-server", ";",
				"set", "-g", "history-limit", strconv.Itoa(scrollbackLines), ";",
				"set", "-g", "window-size", "smallest", ";",
				"set", "-g", "set-clipboard", "on", ";"}
			if style := s.themeStyle(); style != "" {
				args = append(args, "set", "-g", "window-style", style, ";")
			}
			args = append(args, "new-session", "-d", "-s", tmuxSession,
				"-x", strconv.Itoa(max(w, 1)), "-y", strconv.Itoa(max(h, 1)),
				"-P", "-F", paneBirth, "-c", dir)
		}
		if cmd != "" {
			args = append(args, cmd)
		}

		out, err := s.run(args...)
		if err != nil {
			s.events <- daemonErrorMsg{err: err}
			return
		}
		f := strings.Fields(out)
		if len(f) != 3 {
			s.events <- daemonErrorMsg{err: errors.New("tmux said " + out)}
			return
		}
		id, win := f[0], f[1]
		pid, aerr := strconv.Atoi(f[2])
		if aerr != nil {
			s.events <- daemonErrorMsg{err: errors.New("tmux said " + out)}
			return
		}

		// The name and the opening directory are pinned on the pane, so the
		// list can tell a plan's web apart from a shell that wandered there.
		_, _ = s.run("set", "-p", "-t", id, "@scrn_dir", dir, ";",
			"set", "-p", "-t", id, "@scrn_name", name)

		s.mu.Lock()
		s.panes[pid] = &pane{id: id, win: win, pid: pid, dir: dir, name: name}
		s.byPane[id] = pid
		s.watching[pid] = true
		s.mu.Unlock()

		// The control client is wired before the model is told: a keystroke
		// typed the moment the shell appears has to have somewhere to go.
		s.refreshList()
		s.ensureCtl()
		s.events <- termOpenedMsg{pid: pid, dir: dir, name: name}
		s.captureSoon(id)
	}()
}

// list asks for what the server holds, which comes back as a sessionsMsg.
func (s *session) list() {
	if s == nil {
		return
	}
	go s.refreshList()
}

// attach starts rendering a shell's screen in this window, beginning with
// the screen as it stands.
func (s *session) attach(pid, w, h int) {
	if s == nil {
		return
	}
	s.setSize(w, h)
	s.mu.Lock()
	s.watching[pid] = true
	p := s.panes[pid]
	s.mu.Unlock()
	if p != nil {
		s.captureSoon(p.id)
	}
}

// detach stops rendering a shell without touching it.
func (s *session) detach(pid int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.watching, pid)
	s.mu.Unlock()
}

// setSize records the pane this window gives its shells, and says so.
func (s *session) setSize(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	s.mu.Lock()
	changed := w != s.width || h != s.height
	s.width, s.height = w, h
	s.mu.Unlock()
	if changed {
		s.declareSizes()
	}
}

// declareSizes tells the server the size of this client and of every window
// it draws. Both, because they are different questions.
//
// A tmux client has one current window, and its size is the one the server
// hands out. scrn has no current window: it draws whichever shell is
// selected, out of a set that all have to be the size of the pane they are
// drawn into, because any of them can be selected next. A window no client
// has current is left at whatever size it was last given — a second scrn
// window, in a narrower terminal, takes every window down to its size, and
// when it goes only the current one is handed back. The rest stay narrow,
// and the shell that was drawing at 120 columns keeps drawing at 60.
//
// Said per window, tmux keeps the arbitration it always had: a shell two
// scrn windows are watching is the size of the smaller of them, and a larger
// size restated while the smaller window is still there moves nothing.
// Restating is how a stranded window is handed back, which is why nothing
// here is elided for being unchanged.
func (s *session) declareSizes() {
	s.mu.Lock()
	ctl, w, h := s.ctl, s.width, s.height
	wins := make([]string, 0, len(s.panes))
	for _, p := range s.panes {
		if p.win != "" {
			wins = append(wins, p.win)
		}
	}
	s.mu.Unlock()
	if ctl == nil || w <= 0 || h <= 0 {
		return
	}

	size := strconv.Itoa(w) + "x" + strconv.Itoa(h)
	ctl.say("refresh-client -C " + size)
	slices.Sort(wins)
	for _, win := range slices.Compact(wins) {
		ctl.say("refresh-client -C " + win + ":" + size)
	}
}

// resize tells the server this window's pane changed shape. The size is the
// window's, not any one shell's; the pid rides along only so the freshly
// sized screen comes back for the shell being looked at.
func (s *session) resize(pid, w, h int) {
	if s == nil {
		return
	}
	s.setSize(w, h)
	s.mu.Lock()
	p := s.panes[pid]
	watched := s.watching[pid]
	s.mu.Unlock()
	if p != nil && watched {
		s.captureSoon(p.id)
	}
}

// say hands a line to the control client, if one is attached. A shell can
// exist while the control client is between attachments; a keystroke then
// has nowhere to go, and goes nowhere.
func (s *session) say(lines []string) {
	s.mu.Lock()
	ctl := s.ctl
	s.mu.Unlock()
	if ctl == nil {
		return
	}
	for _, line := range lines {
		ctl.say(line)
	}
}

// key sends a keystroke to a shell, as the keystroke it was. The bytes are
// tmux's to decide, which is why it is not sent any.
func (s *session) key(pid int, k *keyPress) {
	if s == nil {
		return
	}
	if k == nil {
		return
	}
	if p := s.pane(pid); p != nil {
		s.say(tmuxKeyLines(p.id, k))
	}
}

// mouse sends a click, a drag or a wheel turn to a shell.
func (s *session) mouse(pid int, m *mousePress) {
	if s == nil {
		return
	}
	if m == nil {
		return
	}
	p := s.pane(pid)
	if p == nil {
		return
	}
	s.say(tmuxMouseLines(p.id, m, p.sgr))
}

// paste sends pasted text as a paste, so a program with bracketed paste on
// can tell it from someone typing very fast.
func (s *session) paste(pid int, text string) {
	if s == nil {
		return
	}
	if text == "" {
		return
	}
	if p := s.pane(pid); p != nil {
		s.say(tmuxPasteLines(p.id, text))
	}
}

// history asks for a shell's transcript — everything above the screen,
// oldest first — which comes back as a historyMsg.
func (s *session) history(pid int) {
	if s == nil {
		return
	}
	p := s.pane(pid)
	if p == nil {
		return
	}
	go func() {
		out, err := s.run("capture-pane", "-e", "-p", "-t", p.id, "-S", "-", "-E", "-1")
		if err != nil {
			out = ""
		}
		if out != "" {
			// The reader's window lands anywhere in this; every line has to
			// stand alone the same way a screen's rows do.
			out = strings.Join(selfContained(strings.Split(out, "\n")), "\n")
		}
		s.events <- historyMsg{pid: pid, history: out}
	}()
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

// carryCopy moves one buffer's bytes to the system clipboard and lets the
// buffer go. Through a file rather than a command's stdout, so the copy
// crosses byte-exact — a trailing newline included.
func (s *session) carryCopy(buffer string) {
	f, err := os.CreateTemp("", "scrn-clip-")
	if err != nil {
		return
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()

	if _, err := s.run("save-buffer", "-b", buffer, path); err != nil {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return
	}
	if writeClipboard(string(b)) == nil {
		_, _ = s.run("delete-buffer", "-b", buffer)
	}
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
// scroll off its pane. It is settled into the server when the first shell
// brings it up, so raising it takes R — a fresh server — to reach anything.
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

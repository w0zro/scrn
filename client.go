package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The client side of the split. It never touches a pty: it asks the daemon to
// open shells, sends keystrokes, and draws the screens that come back.

// daemonStartWait is how long to wait for a daemon that has just been started
// to begin listening.
const daemonStartWait = 3 * time.Second

// remoteTerm is a shell the daemon is holding, as the client sees it: the last
// screen it sent and where the cursor was.
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

	// What the program in it has asked of the terminal window. scrn is the one
	// with a window, so it is scrn that has to ask for it.
	title    string
	progress string
}

// session is the client's connection to the daemon.
type session struct {
	conn   *conn
	events chan tea.Msg
}

// Messages the client raises for the model.
type (
	// daemonReadyMsg says the connection is up, or explains why it is not.
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

	// sessionsMsg is the shells the daemon is holding, and when it started.
	sessionsMsg struct {
		sessions []sessionInfo
		since    time.Time
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

	// daemonLostMsg says the connection dropped. The shells are still running;
	// this window just cannot see them until it reconnects.
	daemonLostMsg struct{ err error }

	// daemonErrorMsg says the daemon could not do something it was asked to.
	// The connection is fine and the shells are where they were: this is a
	// report about one ask, not about the daemon.
	daemonErrorMsg struct{ err error }
)

// reconnectMsg asks for another go at the daemon, once the last one has had a
// moment to release the socket.
type reconnectMsg struct{}

// reconnectWait is how long a daemon that is deliberately going — an upgrade's
// exec, a stand-down — is given before connecting again. It is also the first
// wait of a chase after a daemon that went without being asked.
const reconnectWait = 300 * time.Millisecond

// reconnectMax caps the growing wait between attempts on a daemon that keeps
// going away. Retrying forever is the point — a window is no use without its
// daemon — but retrying fast would only hammer whatever is failing.
const reconnectMax = 5 * time.Second

// reconnect connects again after the wait, which starts a fresh daemon if
// nothing is listening by then.
func reconnect(after time.Duration) tea.Cmd {
	return tea.Tick(after, func(time.Time) tea.Msg { return reconnectMsg{} })
}

// stale reports whether a daemon started before this scrn was built.
func stale(since time.Time) bool {
	built := builtAt()
	return !built.IsZero() && !since.IsZero() && built.After(since)
}

// connectDaemon reaches the daemon, starting one if none is listening.
func connectDaemon() tea.Cmd {
	return func() tea.Msg {
		s, err := openSession()
		return daemonReadyMsg{session: s, err: err}
	}
}

func openSession() (*session, error) {
	c, err := dialDaemon()
	if err != nil {
		if err = startDaemon(); err != nil {
			return nil, err
		}
		if c, err = waitForDaemon(daemonStartWait); err != nil {
			return nil, err
		}
	}

	s := &session{conn: newConn(c), events: make(chan tea.Msg, 64)}
	go s.receive()
	return s, nil
}

// startDaemon launches a daemon that will outlive this window. Setsid is what
// makes that true: without its own session it would take the terminal's hangup
// along with the client and every shell would go with it.
func startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()

	cmd := exec.Command(exe, "daemon")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null

	// The daemon's stderr goes to a file rather than the bit bucket, so a
	// daemon that dies leaves its last words — a panic, a refused socket —
	// where they can be read. It is truncated here, on a fresh start, and
	// inherited across an upgrade's exec, so it only ever tells of the
	// daemon now running. Failing to open it costs the death rattle, not
	// the daemon.
	logPath := daemonLogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	if log, err := os.OpenFile(logPath,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
		defer log.Close()
		cmd.Stderr = log
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Wait on it from the side. Release only drops the handle — on unix it
	// reaps nothing — so a daemon that later died would sit as a zombie
	// under this window for as long as the window stayed open.
	go func() { _ = cmd.Wait() }()
	return nil
}

// waitForDaemon dials until the daemon is listening or the wait runs out.
func waitForDaemon(d time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(d)
	for {
		c, err := dialDaemon()
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("daemon did not start listening")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// receive turns everything the daemon says into messages for the model.
func (s *session) receive() {
	defer close(s.events)
	for {
		m, err := s.conn.read()
		if err != nil {
			s.events <- daemonLostMsg{err: err}
			return
		}
		switch m.Kind {
		case kindOpened:
			s.events <- termOpenedMsg{pid: m.PID, dir: m.Dir, name: m.Name}
		case kindSessions:
			s.events <- sessionsMsg{sessions: m.Sessions, since: time.UnixMilli(m.Since)}
		case kindScreen:
			s.events <- screenMsg{
				pid: m.PID, screen: m.Screen, curX: m.CursorX, curY: m.CursorY,
				title: m.Title, progress: m.Progress,
				sb: m.Scrollback, mouse: m.MouseOn, alt: m.Alt,
			}
		case kindHistory:
			s.events <- historyMsg{pid: m.PID, history: m.History}
		case kindExited:
			s.events <- termGoneMsg{pid: m.PID}
		case kindError:
			// An error is the daemon answering, which is the opposite of the
			// daemon being gone. Treating it as a loss dropped every shell this
			// window knew about because one ask failed.
			s.events <- daemonErrorMsg{err: errors.New(m.Err)}
		}
	}
}

// nextEvent waits for the daemon's next word. Exactly one of these is in
// flight, the same discipline the refresh tick keeps: each schedules the next.
func nextEvent(s *session) tea.Cmd {
	if s == nil {
		return nil
	}
	return func() tea.Msg {
		msg, ok := <-s.events
		if !ok {
			return daemonLostMsg{err: errors.New("daemon connection closed")}
		}
		return msg
	}
}

// The client's half of the conversation. Each is fire and forget: what comes
// back arrives as an event, not as a return value.

func (s *session) ask(m message) {
	if s == nil {
		return
	}
	_ = s.conn.write(m)
}

func (s *session) open(dir, run, name string, w, h int) {
	s.ask(message{Kind: kindOpen, Dir: dir, Run: run, Name: name, Width: w, Height: h})
}

func (s *session) list() { s.ask(message{Kind: kindList}) }

// detach stops a shell's screens without touching the shell: the pane has
// stopped showing it, so this window's pane should stop counting toward its
// size.
func (s *session) detach(pid int) { s.ask(message{Kind: kindDetach, PID: pid}) }

// standDown asks a daemon older than this build to stop, which it will only
// do if it is holding nothing.
func (s *session) standDown() { s.ask(message{Kind: kindStand}) }

// upgrade asks a daemon older than this build to exec the binary this window
// is running, carrying its shells across rather than ending them. The path
// travels with the ask because the daemon's own may no longer exist — a
// daemon started by `go run` came from a temp binary that went with the run.
// A daemon too old to know the word ignores it, which is what the R fallback
// is for.
func (s *session) upgrade() {
	exe, _ := os.Executable()
	s.ask(message{Kind: kindUpgrade, Exe: exe})
}

// upgradeLimboMsg says an upgrade that was asked for has had long enough: a
// daemon that acted dropped every connection well before this fires.
type upgradeLimboMsg struct{}

// awaitUpgrade gives the daemon a moment to act on the upgrade, because one
// too old to understand it ignores it silently, and silence should not hold
// the window forever.
func awaitUpgrade() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return upgradeLimboMsg{} })
}

// replace gets rid of a daemon and the shells keeping it alive.
//
// It asks first, which is how a daemon that understands the question goes: it
// ends its own shells and releases the socket. But the daemons most in need of
// replacing are the ones too old to have been taught the question, and they
// ignore it — so if the socket is still answered a moment later, the process
// holding it is signalled instead.
func replaceDaemon(s *session) tea.Cmd {
	if s != nil {
		s.replace()
	}
	return func() tea.Msg {
		if !answered(daemonStandDown) {
			return reconnectMsg{}
		}
		return replacedMsg{err: signalDaemon()}
	}
}

func (s *session) replace() { s.ask(message{Kind: kindStand, Force: true}) }

// replacedMsg reports a daemon that had to be signalled rather than asked.
type replacedMsg struct{ err error }

// daemonStandDown is how long a daemon gets to act on being asked to go before
// it is taken to be one that cannot.
const daemonStandDown = 700 * time.Millisecond

// answered reports whether anything is still listening after d.
func answered(d time.Duration) bool {
	time.Sleep(d)
	c, err := dialDaemon()
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// signalDaemon ends whatever is holding the socket. The shells go with it:
// they are its children and it holds the other end of their terminals.
//
// Every process named is considered rather than only the first, and this one
// is passed over. A client has the socket open as well as the daemon does, and
// which of them lsof names first is not something to stake the window on:
// signalling ourselves here would close the window and leave the daemon that
// was being replaced still holding everything. The kill path refuses the same
// thing for the same reason.
func signalDaemon() error {
	out, err := exec.Command("lsof", "-t", socketPath()).Output()
	if err != nil {
		return errors.New("could not find the daemon to end it")
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		return syscall.Kill(pid, syscall.SIGTERM)
	}
	return errors.New("could not find the daemon to end it")
}

// builtAt is when this scrn was built. A daemon that started before its own
// binary was built is running code the window talking to it does not have.
func builtAt() time.Time {
	exe, err := os.Executable()
	if err != nil {
		return time.Time{}
	}
	info, err := os.Stat(exe)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func (s *session) attach(pid, w, h int) {
	s.ask(message{Kind: kindAttach, PID: pid, Width: w, Height: h})
}

// history asks for a shell's transcript, which comes back as a historyMsg.
func (s *session) history(pid int) {
	s.ask(message{Kind: kindHistory, PID: pid})
}

// key sends a keystroke to a shell, as the keystroke it was. The bytes are
// the emulator's to decide, which is why it is not sent any.
func (s *session) key(pid int, k *keyPress) {
	if k != nil {
		s.ask(message{Kind: kindInput, PID: pid, Key: k})
	}
}

// mouse sends a click, a drag or a wheel turn to a shell.
func (s *session) mouse(pid int, m *mousePress) {
	if m != nil {
		s.ask(message{Kind: kindInput, PID: pid, Mouse: m})
	}
}

// paste sends pasted text as a paste, so that a program with bracketed paste
// on can tell it from someone typing very fast.
func (s *session) paste(pid int, text string) {
	if text != "" {
		s.ask(message{Kind: kindInput, PID: pid, Paste: text})
	}
}

// closeTerm ends a shell the daemon holds. The daemon hangs it up rather than
// signalling it, which is the only thing an interactive shell responds to.
func (s *session) closeTerm(pid int) {
	s.ask(message{Kind: kindClose, PID: pid})
}

func (s *session) resize(pid, w, h int) {
	s.ask(message{Kind: kindResize, PID: pid, Width: w, Height: h})
}

// lines is the screen the daemon last sent, one string per row.
func (t *remoteTerm) lines(height int) []string {
	rows := strings.Split(t.screen, "\n")
	if len(rows) > height {
		rows = rows[:height]
	}
	return rows
}

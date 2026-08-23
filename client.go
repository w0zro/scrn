package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	screen string
	curX   int
	curY   int
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
	termOpenedMsg struct{ pid int }

	// sessionsMsg is the shells the daemon is holding.
	sessionsMsg struct{ sessions []sessionInfo }

	// screenMsg is one shell's pane as it now stands.
	screenMsg struct {
		pid    int
		screen string
		curX   int
		curY   int
	}

	// termGoneMsg says a shell has finished.
	termGoneMsg struct{ pid int }

	// daemonLostMsg says the connection dropped. The shells are still running;
	// this window just cannot see them until it reconnects.
	daemonLostMsg struct{ err error }
)

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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Nothing waits on it, so let the process go rather than holding a zombie.
	go func() { _ = cmd.Process.Release() }()
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
			s.events <- termOpenedMsg{pid: m.PID}
		case kindSessions:
			s.events <- sessionsMsg{sessions: m.Sessions}
		case kindScreen:
			s.events <- screenMsg{pid: m.PID, screen: m.Screen, curX: m.CursorX, curY: m.CursorY}
		case kindExited:
			s.events <- termGoneMsg{pid: m.PID}
		case kindError:
			s.events <- daemonLostMsg{err: errors.New(m.Err)}
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

func (s *session) open(dir, run string, w, h int) {
	s.ask(message{Kind: kindOpen, Dir: dir, Run: run, Width: w, Height: h})
}

func (s *session) list() { s.ask(message{Kind: kindList}) }

func (s *session) attach(pid, w, h int) {
	s.ask(message{Kind: kindAttach, PID: pid, Width: w, Height: h})
}

func (s *session) input(pid int, b []byte) {
	if len(b) > 0 {
		s.ask(message{Kind: kindInput, PID: pid, Data: b})
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

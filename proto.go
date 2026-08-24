package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// scrn is split in two. A daemon owns every shell — the pty it runs on and the
// emulator holding what it has drawn — and outlives any window looking at it.
// The client draws the navigator and asks the daemon for screens. Closing the
// window detaches; the work carries on.
//
// The emulator lives on the daemon side rather than the client's so that
// attaching gives you the screen as it stands. A client that emulated for
// itself would come back to a blank pane and have to wait for the shell to say
// something before it had anything to show.
//
// Messages are JSON, one per line, in both directions. The traffic is screens
// and keystrokes, so the protocol is not where the time goes, and being able to
// read a session with nc is worth more than the bytes.

// message is everything either side can say. One struct rather than a union:
// the set is small, and a single decoder is easier to keep honest.
type message struct {
	Kind string `json:"kind"`

	PID    int    `json:"pid,omitempty"`
	Dir    string `json:"dir,omitempty"`
	Run    string `json:"run,omitempty"` // what to run instead of a shell
	Width  int    `json:"w,omitempty"`
	Height int    `json:"h,omitempty"`

	// Data is keystrokes going to a shell. Go encodes a byte slice as base64,
	// which keeps a line of JSON a line of JSON.
	Data []byte `json:"data,omitempty"`

	// Screen is a rendered pane, with the cursor the shell would be showing.
	Screen  string `json:"screen,omitempty"`
	CursorX int    `json:"cx,omitempty"`
	CursorY int    `json:"cy,omitempty"`

	// Title and Progress are what the program running in the pane has asked of
	// the terminal window. The daemon has no window, so they are carried out to
	// the client, which does.
	Title    string `json:"title,omitempty"`
	Progress string `json:"progress,omitempty"`

	Sessions []sessionInfo `json:"sessions,omitempty"`

	// Since is when the daemon started, so a client can tell whether it is
	// older than the build asking. A daemon outlives the window that started
	// it, which is the point of it, and it therefore also outlives rebuilds.
	Since int64 `json:"since,omitempty"`

	// Force turns a request to stand down into an instruction. A daemon holds
	// work, so being asked twice is the difference between "if you can" and
	// "and take what you are holding with you".
	Force bool   `json:"force,omitempty"`
	Err   string `json:"err,omitempty"`
}

// sessionInfo is a shell the daemon is holding.
type sessionInfo struct {
	PID int    `json:"pid"`
	Dir string `json:"dir"`
}

// What each side can say.
const (
	// Client to daemon.
	kindOpen   = "open"      // start a shell in Dir
	kindList   = "list"      // what shells are there
	kindAttach = "attach"    // send me this shell's screen, and keep sending
	kindDetach = "detach"    // stop sending, but keep it running
	kindInput  = "input"     // these keystrokes are for this shell
	kindResize = "resize"    // the pane changed shape
	kindClose  = "close"     // end this shell
	kindStand  = "standdown" // stop, if you are holding nothing

	// Daemon to client.
	kindOpened   = "opened"   // the shell you just asked for, and its pid
	kindSessions = "sessions" // the shells being held
	kindScreen   = "screen"   // a shell's pane as it now stands
	kindExited   = "exited"   // a shell has finished
	kindError    = "error"    // that did not work
)

// socketPath is where the daemon listens. It is per user and outside the
// project, because one daemon holds the shells for every repository.
func socketPath() string {
	if p := os.Getenv("SCRN_SOCKET"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "scrn", "scrnd-"+strconv.Itoa(os.Getuid())+".sock")
}

// dialDaemon connects to a daemon that is already running.
func dialDaemon() (net.Conn, error) {
	return net.Dial("unix", socketPath())
}

// conn is a message-framed connection to the other side.
type conn struct {
	net  net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	send chan message
}

func newConn(c net.Conn) *conn {
	return &conn{net: c, enc: json.NewEncoder(c), dec: json.NewDecoder(c)}
}

// write sends one message. Callers on the daemon side serialise through a
// single goroutine, because an encoder shared between goroutines interleaves.
func (c *conn) write(m message) error { return c.enc.Encode(m) }

// read blocks for the next message.
func (c *conn) read() (message, error) {
	var m message
	err := c.dec.Decode(&m)
	return m, err
}

// readBy is read with a deadline, so a caller waiting for a particular message
// gives up rather than hanging on one that never comes.
func (c *conn) readBy(t time.Time) (message, error) {
	if err := c.net.SetReadDeadline(t); err != nil {
		return message{}, err
	}
	defer c.net.SetReadDeadline(time.Time{})
	return c.read()
}

func (c *conn) close() error { return c.net.Close() }

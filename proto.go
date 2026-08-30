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
	Run    string `json:"run,omitempty"`  // what to run instead of a shell
	Name   string `json:"name,omitempty"` // what the project calls it
	Width  int    `json:"w,omitempty"`
	Height int    `json:"h,omitempty"`

	// What the user did, on its way to a shell. It travels as the event it was
	// rather than as the bytes it should become, because only the daemon's
	// emulator knows what those bytes are: see keyPress below.
	Key   *keyPress   `json:"key,omitempty"`
	Mouse *mousePress `json:"mouse,omitempty"`
	Paste string      `json:"paste,omitempty"`

	// Screen is a rendered pane, and its shape is part of the contract: every
	// row is exactly as wide as the pane, blanks and all. The cursor crosses
	// as a column in that grid, and the client cuts the row at the column to
	// mark it — geometry the rows do not carry cannot be cut to.
	Screen  string `json:"screen,omitempty"`
	CursorX int    `json:"cx,omitempty"`
	CursorY int    `json:"cy,omitempty"`

	// Facts about the pane a screen also carries: how many lines have scrolled
	// off the top, whether the program has asked to hear about the mouse, and
	// whether it is drawing on the alternate screen. Together they are what a
	// client needs to decide whose a wheel turn is.
	Scrollback int  `json:"sb,omitempty"`
	MouseOn    bool `json:"mouseon,omitempty"`
	Alt        bool `json:"alt,omitempty"`

	// History is the transcript that has scrolled off the top of a pane,
	// oldest line first. Unlike a screen its rows are not padded to the pane:
	// nothing cuts a cursor into what has already happened.
	History string `json:"history,omitempty"`

	// Title and Progress are what the program running in the pane has asked of
	// the terminal window. The daemon has no window, so they are carried out to
	// the client, which does.
	Title    string `json:"title,omitempty"`
	Progress string `json:"progress,omitempty"`

	Sessions []sessionInfo `json:"sessions,omitempty"`

	// Exe is the binary the asking window is running, sent with an upgrade.
	// The daemon cannot exec its own path: a daemon started by `go run` came
	// from a temp binary that was gone once that run ended. The window asking
	// for the upgrade is running the build it wants, so it says where.
	Exe string `json:"exe,omitempty"`

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

// keyPress is a keystroke as the window saw it: which key, which modifiers,
// and what it types if it types anything.
//
// It crosses as an event rather than as bytes because the bytes are not a
// property of the key. An up arrow is "\x1b[A" most of the time and "\x1bOA"
// to a program that has asked for application cursor keys, which is what vim,
// readline and less all do; ctrl+a is one byte under the usual encoding and a
// CSI sequence under the kitty protocol. Which of those is right is a fact
// about the emulator's current modes, and the emulator is on the far side of
// this connection. A client that decided for itself would be guessing at
// exactly the programs most likely to be running in the pane.
//
// Code is the key: a printable rune for the ones that type, and one of the
// emulator's own symbols for the ones that do not.
type keyPress struct {
	Code rune   `json:"code"`
	Text string `json:"text,omitempty"`
	Mod  int    `json:"mod,omitempty"`
}

// mousePress is a mouse event in the pane's own coordinates, which is what the
// program drawing there believes it is being told about.
//
// It crosses as an event for the same reason a keystroke does, and more so:
// what a mouse event looks like on the wire depends on which reporting mode
// the program asked for, and there are several. The emulator tracks that; the
// window has no way to know it.
type mousePress struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Button int    `json:"btn,omitempty"`
	Mod    int    `json:"mod,omitempty"`
	Action string `json:"act,omitempty"`
}

// What a mouse can be doing. A wheel turn is a press of a button that only a
// wheel has, which is how X11 numbered them and how every terminal has
// reported them since.
const (
	actPress   = "press"
	actRelease = "release"
	actMotion  = "motion"
)

// sessionInfo is a shell the daemon is holding.
//
// Name is what the project that asked for it calls it, and empty for a shell
// opened by hand. It is what lets scrn tell whether a project already has its
// web running without guessing from the command line of a process.
type sessionInfo struct {
	PID  int    `json:"pid"`
	Dir  string `json:"dir"`
	Name string `json:"name,omitempty"`
}

// What each side can say.
const (
	// Client to daemon.
	kindOpen    = "open"      // start a shell in Dir
	kindList    = "list"      // what shells are there
	kindAttach  = "attach"    // send me this shell's screen, and keep sending
	kindDetach  = "detach"    // stop sending, but keep it running
	kindInput   = "input"     // these keystrokes are for this shell
	kindResize  = "resize"    // the pane changed shape
	kindClose   = "close"     // end this shell
	kindStand   = "standdown" // stop, if you are holding nothing
	kindUpgrade = "upgrade"   // exec the binary at Exe, shells and all
	kindHistory = "history"   // asked: this shell's transcript; answered: here it is

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

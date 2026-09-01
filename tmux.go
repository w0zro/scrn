package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// scrn holds its shells in tmux. The shells outlive any window because the
// tmux server does, which is the same promise the old daemon made — kept now
// by twenty years of somebody else's hardening. scrn owns the experience; a
// private tmux server, on scrn's own socket where no .tmux.conf reaches,
// owns the ptys, the emulation, and the transcripts.
//
// The bridge speaks to it two ways, on purpose:
//
//   - One-shot commands (tmuxCommand) for anything whose answer is read:
//     capture-pane, list-panes, display. Their output crosses as a plain
//     subprocess's stdout, where a captured line that happens to begin with
//     "%end" is just a line. Inside control mode it would be a frame.
//   - A control-mode client (ctlClient) for the stream of notifications —
//     output arriving, windows closing, the server going — and for the
//     fire-and-forget writes on the typing path, where a subprocess per
//     keystroke would be felt.

// tmuxSession is the name of the one session scrn keeps its windows in.
const tmuxSession = "scrn"

// tmuxCommand runs one tmux command against scrn's server and returns its
// stdout. From the root directory: the server inherits the working directory
// of whoever starts it first, and a server filed under a repository would
// stand in that repository's process tree. Bounded like the scans, for the
// same reason the scans are.
func tmuxCommand(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-S", socketPath()}, args...)...)
	cmd.Dir = "/"
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if msg := firstLine(string(ee.Stderr)); msg != "" {
				// A server that is not running is an answer, not a failure:
				// nothing held means nothing to ask about. tmux says it one
				// way for a socket nothing ever claimed and another for one
				// it found stale.
				if strings.HasPrefix(msg, "no server running") ||
					strings.HasSuffix(msg, "(No such file or directory)") ||
					strings.HasSuffix(msg, "(Connection refused)") {
					return "", errNoServer
				}
				return "", errors.New(msg)
			}
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// errNoServer says no tmux server holds scrn's socket, which is how "no
// shells anywhere" looks from outside.
var errNoServer = errors.New("no server is running")

// ctlNote is one thing the control-mode stream said, reduced to what the bridge
// acts on.
type ctlNote struct {
	kind   noteKind
	pane   string // "%1", for output
	buffer string // for a paste buffer changing: which one
	err    string // what an %error block said
}

type noteKind int

const (
	noteNothing noteKind = iota // a line the bridge has no use for
	noteOutput                  // a pane drew something
	noteWindows                 // the set of windows changed
	noteError                   // a command was refused
	noteExit                    // the server hung up this client
	notePaste                   // a paste buffer changed: a program copied
)

// parseNote reads one control-mode line. The data of an %output is not kept:
// scrn renders panes by asking tmux for the screen as it stands, so the
// notification is a doorbell, not a delivery.
func parseNote(line string) ctlNote {
	switch {
	case strings.HasPrefix(line, "%output "):
		rest := strings.TrimPrefix(line, "%output ")
		pane, _, _ := strings.Cut(rest, " ")
		return ctlNote{kind: noteOutput, pane: pane}
	case strings.HasPrefix(line, "%window-close "),
		strings.HasPrefix(line, "%unlinked-window-close "),
		strings.HasPrefix(line, "%window-add "):
		// Either way the set of windows is not what it was. Which windows
		// remain is asked of tmux rather than tracked by arithmetic.
		return ctlNote{kind: noteWindows}
	case strings.HasPrefix(line, "%paste-buffer-changed "):
		// With set-clipboard on, a program's OSC 52 lands in a buffer and
		// this says so — the hop through which a copy inside a pane reaches
		// the system clipboard outside.
		return ctlNote{kind: notePaste, buffer: strings.TrimPrefix(line, "%paste-buffer-changed ")}
	case strings.HasPrefix(line, "%error"):
		return ctlNote{kind: noteError}
	case line == "%exit" || strings.HasPrefix(line, "%exit "):
		return ctlNote{kind: noteExit}
	}
	return ctlNote{}
}

// ctlClient is one control-mode connection: a tmux client that is a program.
// Writes go to its stdin under a lock; notifications come back on stdout.
type ctlClient struct {
	cmd *exec.Cmd

	mu sync.Mutex
	in io.WriteCloser
}

// startCtl attaches a control-mode client to scrn's session and begins
// feeding what it says to notify, one note per line, ending with noteExit
// when the stream does. It fails when there is nothing to attach to, which
// is not an error about tmux — a server with no session holds no shells.
func startCtl(notify func(ctlNote)) (*ctlClient, error) {
	cmd := exec.Command("tmux", "-S", socketPath(), "-C", "attach", "-t", tmuxSession)
	cmd.Dir = "/"
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	c := &ctlClient{cmd: cmd, in: in}
	go func() {
		// An %error block's text follows the %error line inside the reply
		// frame; the scanner folds the block into one note as it goes.
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		inError := false
		errText := ""
		for sc.Scan() {
			line := sc.Text()
			if inError {
				if strings.HasPrefix(line, "%end") || strings.HasPrefix(line, "%error") {
					inError = false
					notify(ctlNote{kind: noteError, err: errText})
					continue
				}
				if errText == "" {
					errText = line
				}
				continue
			}
			n := parseNote(line)
			if n.kind == noteError {
				inError = true
				errText = ""
				continue
			}
			if n.kind != noteNothing {
				notify(n)
			}
		}
		notify(ctlNote{kind: noteExit})
		_ = cmd.Wait()
	}()
	return c, nil
}

// say sends one command down the control stream, fire and forget: what comes
// of it arrives as notifications, or not at all.
func (c *ctlClient) say(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = io.WriteString(c.in, line+"\n")
}

// close hangs up the control client. The server and its shells stay.
func (c *ctlClient) close() {
	c.mu.Lock()
	_ = c.in.Close()
	c.mu.Unlock()
	_ = c.cmd.Process.Kill()
}

// tmuxKeyLines is a keystroke as control-mode send-keys commands.
//
// A key that typed text is sent as the text's bytes, in hex so that no
// character ever meets tmux's quoting — this is what fixed capitals once and
// keeps them fixed. Alt is a fact about the bytes (an ESC in front), so it
// stays on this path. Everything else goes by tmux's own key names, and tmux
// encodes them for the pane the way it always has: cursor mode, extended
// keys, all of it is tmux's business now.
func tmuxKeyLines(pane string, k *keyPress) []string {
	mod := tea.KeyMod(k.Mod)
	command := mod&(tea.ModCtrl|tea.ModMeta|tea.ModHyper|tea.ModSuper) != 0

	if k.Text != "" && !command {
		hex := ""
		if mod&tea.ModAlt != 0 {
			hex = " 1b"
		}
		for _, b := range []byte(k.Text) {
			hex += fmt.Sprintf(" %02x", b)
		}
		return []string{"send-keys -t " + pane + " -H" + hex}
	}

	// A super or hyper chord is the terminal's kind of command — cmd+w,
	// cmd+t — and one that leaks through to scrn types nothing. Falling
	// to the bare letter here is what once made cmd+v type a v.
	if mod&(tea.ModSuper|tea.ModHyper) != 0 {
		return nil
	}

	name, ok := tmuxKeyName(k.Code)
	if !ok {
		// A plain printable pressed under ctrl or meta.
		if k.Code > 0x20 && k.Code != 0x7f {
			name = string(k.Code)
		} else {
			return nil
		}
	}
	prefix := ""
	if mod&tea.ModCtrl != 0 {
		prefix += "C-"
	}
	if mod&(tea.ModAlt|tea.ModMeta) != 0 {
		prefix += "M-"
	}
	if mod&tea.ModShift != 0 {
		prefix += "S-"
	}
	return []string{"send-keys -t " + pane + " " + prefix + name}
}

// tmuxKeyName is the name tmux knows a special key by.
func tmuxKeyName(code rune) (string, bool) {
	names := map[rune]string{
		tea.KeyUp: "Up", tea.KeyDown: "Down",
		tea.KeyLeft: "Left", tea.KeyRight: "Right",
		tea.KeyHome: "Home", tea.KeyEnd: "End",
		tea.KeyPgUp: "PPage", tea.KeyPgDown: "NPage",
		tea.KeyEnter: "Enter", tea.KeyTab: "Tab",
		tea.KeyBackspace: "BSpace", tea.KeyEscape: "Escape",
		tea.KeyDelete: "DC", tea.KeyInsert: "IC",
		tea.KeySpace: "Space",
		tea.KeyF1:    "F1", tea.KeyF2: "F2", tea.KeyF3: "F3", tea.KeyF4: "F4",
		tea.KeyF5: "F5", tea.KeyF6: "F6", tea.KeyF7: "F7", tea.KeyF8: "F8",
		tea.KeyF9: "F9", tea.KeyF10: "F10", tea.KeyF11: "F11", tea.KeyF12: "F12",
	}
	name, ok := names[code]
	return name, ok
}

// tmuxPasteLines carries pasted text into a pane through a buffer, so that
// paste-buffer can wrap it in bracketed-paste markers when the program asked
// for them — the same courtesy the emulator's Paste used to extend.
func tmuxPasteLines(pane, text string) []string {
	return []string{
		"set-buffer -b scrn-paste \"" + escapeBuffer(text) + "\"",
		"paste-buffer -p -d -b scrn-paste -t " + pane,
	}
}

// escapeBuffer writes text as tmux's double-quoted strings read it: octal
// escapes for everything that could mean anything, so no byte of a paste is
// ever grammar.
func escapeBuffer(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		safe := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == ' ' || c == '.' || c == ',' ||
			c == '/' || c == ':' || c == '-' || c == '_' || c == '=' || c == '+'
		if safe {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "\\%03o", c)
	}
	return b.String()
}

// tmuxMouseLines is a mouse event as the SGR bytes the program in the pane
// asked to be told about, sent raw. tmux cannot be handed a mouse event over
// control mode, but it does say which reporting the program turned on, and
// SGR is the one every modern program uses; a pane listening only in an
// older encoding is left unclicked rather than garbled.
func tmuxMouseLines(pane string, m *mousePress, sgr bool) []string {
	if !sgr {
		return nil
	}
	mod := uv.KeyMod(m.Mod)
	b := ansi.EncodeMouseButton(ansi.MouseButton(m.Button), m.Action == actMotion,
		mod&uv.ModShift != 0, mod&uv.ModAlt != 0, mod&uv.ModCtrl != 0)
	seq := ansi.MouseSgr(b, m.X, m.Y, m.Action == actRelease)
	hex := ""
	for _, c := range []byte(seq) {
		hex += fmt.Sprintf(" %02x", c)
	}
	return []string{"send-keys -t " + pane + " -H" + hex}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

// conn holds its shells in tmux. The shells outlive any window because the
// tmux server does, a promise kept by twenty years of somebody else's
// hardening. conn owns the experience; a
// private tmux server, on conn's own socket where no .tmux.conf reaches,
// owns the ptys, the emulation, and the transcripts.
//
// The bridge speaks to it two ways, on purpose:
//
//   - One-shot commands (tmuxCommand) for anything whose answer is read —
//     list-panes, display — and for everything that moves a pane. Their
//     output crosses as a plain subprocess's stdout.
//   - A control-mode client (ctlClient) for the stream of notifications:
//     windows coming and going, panes moving, the server going. Nothing is
//     ever written down it; the keys are tmux's own.

// tmuxSession is the name of the one session conn keeps its windows in.
const tmuxSession = "conn"

// tmuxCommand runs one tmux command against conn's server and returns its
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	msg := firstLine(stderr.String())
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && msg != "" {
			// A server that is not running is an answer, not a failure:
			// nothing held means nothing to ask about. tmux says it one
			// way for a socket nothing ever claimed and another for one
			// it found stale.
			if strings.HasPrefix(msg, "no server running") ||
				strings.HasSuffix(msg, "(No such file or directory)") ||
				strings.HasSuffix(msg, "(Connection refused)") {
				return "", errNoServer
			}
			if strings.HasPrefix(msg, "duplicate session") {
				return "", errDuplicateSession
			}
			return "", errors.New(msg)
		}
		return "", err
	}
	if len(out) == 0 && msg != "" {
		// tmux can fail and still exit zero — a socket it could not create
		// is one such — and says so only on stderr. A command that answered
		// nothing and complained was refused, whatever the status says.
		return "", errors.New(msg)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// errNoServer says no tmux server holds conn's socket, which is how "no
// shells anywhere" looks from outside.
var errNoServer = errors.New("no server is running")

// errDuplicateSession says the session was made in the instant between
// asking after it and making it: someone else's making is as good as ours.
var errDuplicateSession = errors.New("duplicate session")

// ctlNote is one thing the control-mode stream said, reduced to what the bridge
// acts on.
type ctlNote struct {
	kind noteKind
	err  string // what an %error block said
}

type noteKind int

const (
	noteNothing noteKind = iota // a line the bridge has no use for
	noteWindows                 // the set of windows, or what they hold, changed
	noteError                   // a command was refused
	noteExit                    // the server hung up this client
)

// parseNote reads one control-mode line. Every pane's output crosses this
// stream too, octal-escaped, to be dropped here: tmux could be asked not to
// send it — refresh-client -A pane:off — but once every client has turned a
// pane off tmux stops reading the pane, and a program in it blocks the
// moment its output buffer fills. That is a shell stalling because nobody
// is looking at it, which is the one thing held shells must never do; the
// traffic is the price.
func parseNote(line string) ctlNote {
	switch {
	case strings.HasPrefix(line, "%window-close "),
		strings.HasPrefix(line, "%unlinked-window-close "),
		strings.HasPrefix(line, "%window-add "),
		strings.HasPrefix(line, "%layout-change "):
		// Either way the windows are not what they were: one came or went,
		// or a pane moved between two — a shell shown beside the navigator
		// or parked again. What holds what is asked of tmux rather than
		// tracked by arithmetic.
		return ctlNote{kind: noteWindows}
	case line == "%exit" || strings.HasPrefix(line, "%exit "):
		return ctlNote{kind: noteExit}
	}
	return ctlNote{}
}

// ctlClient is one control-mode connection: a tmux client that is a program.
// Notifications come back on stdout; stdin is held only so that closing it
// is a hangup.
type ctlClient struct {
	cmd *exec.Cmd
	in  io.WriteCloser
}

// startCtl attaches a control-mode client to conn's session and begins
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
		readCtl(out, notify)
		_ = cmd.Wait()
	}()
	return c, nil
}

// readCtl reads the control stream to its end, handing notify one note per
// line worth acting on and noteExit when the stream closes.
//
// A command's reply is a frame: %begin, the reply's body, then %end or
// %error closing it. The body of a refused command is the reason it was
// refused, and it comes before the %error that says so — so the body is
// held until its closing line says which of the two it was. Every command
// conn sends over this stream answers with an empty body when it succeeds,
// which is why a body is only ever read as an error's text.
func readCtl(r io.Reader, notify func(ctlNote)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inReply := false
	body := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "%begin"):
			inReply, body = true, ""
			continue
		case strings.HasPrefix(line, "%end"):
			inReply = false
			continue
		case strings.HasPrefix(line, "%error"):
			inReply = false
			notify(ctlNote{kind: noteError, err: body})
			continue
		}
		n := parseNote(line)
		if n.kind == noteExit {
			// Said once, below: the server closes the stream right after.
			break
		}
		if n.kind != noteNothing {
			notify(n)
			continue
		}
		if inReply && body == "" && strings.TrimSpace(line) != "" {
			body = line
		}
	}
	notify(ctlNote{kind: noteExit})
}

// close hangs up the control client. The server and its shells stay.
func (c *ctlClient) close() {
	_ = c.in.Close()
	_ = c.cmd.Process.Kill()
}

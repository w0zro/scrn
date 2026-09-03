package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// conn restart is R, then conn, as one command typed at a terminal: the
// server ended with every shell it holds, and brought up again under this
// build's configuration with this terminal attached. It is the way to a
// fresh server from outside the window — for a raised scrollback, or on
// the day something wedges and the keys are not reaching anyone.

// errKept says the user answered the question with anything but yes, and
// the server stands as it was.
var errKept = errors.New("the server is kept")

// runRestart is `conn restart`. The refusals come before anything is ended:
// from inside conn's own window the pane running this would go with the
// server, taking the command with it, and R is there for that; from inside
// another tmux the launch that follows would refuse to nest.
func runRestart() error {
	inside, err := attachable()
	if err != nil {
		return err
	}
	if inside {
		return errors.New("inside conn's window; ctrl-space R ends the server, and conn from another terminal brings it back")
	}
	if err := endServer(askTerminal(os.Stdin, os.Stdout)); err != nil {
		return err
	}
	return runLaunch()
}

// endServer ends the server and every shell it holds, after asking when it
// holds any — a question the navigator asks too, at R, for the same reason:
// nothing that ends a day's shells should go by without a second look. No
// server is nothing to end. It returns once the socket is quiet, so the
// launch that follows starts a server rather than finding the old one on
// its way out.
func endServer(confirm func(held int) bool) error {
	out, err := tmuxCommand("list-panes", "-a", "-F", listFormat)
	if errors.Is(err, errNoServer) {
		return nil
	}
	if err != nil {
		return err
	}
	held, _ := parseListing(out)
	if len(held) > 0 && !confirm(len(held)) {
		return errKept
	}
	if _, err := tmuxCommand("kill-server"); err != nil && !errors.Is(err, errNoServer) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := tmuxCommand("has-session", "-t", tmuxSession)
		if errors.Is(err, errNoServer) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the server did not go")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// askTerminal asks the question on the terminal and reads the answer: y or
// yes, in any case, is yes, and anything else — a stray enter, a closed
// stdin — keeps the server.
func askTerminal(in io.Reader, out io.Writer) func(held int) bool {
	return func(held int) bool {
		noun := "shells"
		if held == 1 {
			noun = "shell"
		}
		fmt.Fprintf(out, "end the server, and the %d %s it holds? (y/n) ", held, noun)
		line, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true
		}
		return false
	}
}

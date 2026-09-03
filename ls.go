package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
)

// conn ls is the faucet on the server's state: the shells it holds, one per
// line on stdout, for the reader that is not a person. The window shows the
// same list dressed up; scripts, prompts and grep get it plain.

// runLS asks the tmux server what it holds and writes one shell per line:
// pid, directory and name, tab-separated. A shell opened by hand has no
// name, and its last field is empty. No server listening means nothing is
// held, which is an empty list, not an error — and not a reason to start
// one, which would be an odd side effect of asking a question.
func runLS(w io.Writer) error {
	out, err := tmuxCommand("list-panes", "-a", "-F", listFormat)
	if err != nil {
		if errors.Is(err, errNoServer) {
			return nil
		}
		return err
	}

	var ss []sessionInfo
	held, _ := parseListing(out)
	for _, p := range held {
		ss = append(ss, p.info())
	}

	slices.SortFunc(ss, func(a, b sessionInfo) int {
		return cmp.Or(cmp.Compare(a.Dir, b.Dir), cmp.Compare(a.Name, b.Name), cmp.Compare(a.PID, b.PID))
	})
	for _, s := range ss {
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\n", s.PID, s.Dir, s.Name); err != nil {
			return err
		}
	}
	return nil
}

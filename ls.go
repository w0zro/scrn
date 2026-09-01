package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// scrn ls is the faucet on the server's state: the shells it holds, one per
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
		if err == errNoServer {
			return nil
		}
		return err
	}

	var ss []sessionInfo
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		dir := f[2]
		if dir == "" {
			dir = f[4]
		}
		ss = append(ss, sessionInfo{PID: pid, Dir: dir, Name: f[3]})
	}

	sort.Slice(ss, func(i, j int) bool {
		a, b := ss[i], ss[j]
		if a.Dir != b.Dir {
			return a.Dir < b.Dir
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.PID < b.PID
	})
	for _, s := range ss {
		if _, err := fmt.Fprintf(w, "%d\t%s\t%s\n", s.PID, s.Dir, s.Name); err != nil {
			return err
		}
	}
	return nil
}

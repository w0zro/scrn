package main

import (
	"fmt"
	"io"
	"sort"
	"time"
)

// scrn ls is the faucet on the daemon's state: the shells it holds, one per
// line on stdout, for the reader that is not a person. The window shows the
// same list dressed up; scripts, prompts and grep get it plain.

// runLS asks the daemon what it holds and writes one shell per line: pid,
// directory and name, tab-separated. A shell opened by hand has no name, and
// its last field is empty.
func runLS(w io.Writer) error {
	c, err := dialDaemon()
	if err != nil {
		// No daemon listening means no shells are held. That is an empty
		// list, not a failure — and not a reason to start a daemon, which
		// would be an odd side effect of asking a question.
		return nil
	}
	defer c.Close()

	conn := newConn(c)
	if err := conn.write(message{Kind: kindList}); err != nil {
		return err
	}

	// The daemon may say other things first; wait for the answer, but not on
	// one that never comes.
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, err := conn.readBy(deadline)
		if err != nil {
			return err
		}
		if m.Kind != kindSessions {
			continue
		}

		ss := m.Sessions
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
			fmt.Fprintf(w, "%d\t%s\t%s\n", s.PID, s.Dir, s.Name)
		}
		return nil
	}
}

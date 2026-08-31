package main

import (
	"fmt"
	"testing"
)

func TestReproFoldedPrefixS(t *testing.T) {
	// The drive's exact tree: shell1 (bare), shell2 (focused, folded with a
	// sleep it started), a foreign sleep. Cursor on the foreign row.
	procs := []Proc{
		{PID: 100, PPID: 1, Command: "zsh", Dir: "/p/repo"},                            // shell1
		{PID: 200, PPID: 1, Command: "zsh", Dir: "/p/repo"},                            // shell2, focused
		{PID: 201, PPID: 200, Command: "sleep", Argv: "sleep 400", Dir: "/p/repo"},     // its child
		{PID: 300, PPID: 999, Command: "sleep", Argv: "sleep 500", Dir: "/p/repo"},     // foreign
	}
	m := withProcList(96, 20, []Project{{Name: "repo", Path: "/p/repo"}}, procs)
	m.terms = map[int]*remoteTerm{100: {pid: 100, dir: "/p/repo"}, 200: {pid: 200, dir: "/p/repo"}}
	m.focus = 200
	m.rebuild()
	for i, r := range m.rows {
		if r.kind == rowProc && r.node.PID == 300 {
			m.cursor = i
		}
	}
	m, asked := pipeDaemon(t, m)

	m = chord(m, "s")
	got := askedFor(t, asked)
	fmt.Printf("open ask pid=%d\n", got.PID)
	for i := 0; i < 8; i++ {
		select {
		case ev := <-m.daemon.events:
			fmt.Printf("event %T\n", ev)
			next, _ := m.Update(ev)
			m = next.(model)
		default:
			i = 8
		}
	}
	fmt.Printf("after events: focus=%d wantCursor=%d cursor=%d\n", m.focus, m.wantCursor, m.cursor)

	next, _ := m.Update(procsMsg{procs: append(procs,
		Proc{PID: got.PID, PPID: 1, Command: "zsh", Dir: "/p/repo"})})
	m = next.(model)
	fmt.Printf("after scan: focus=%d wantCursor=%d cursor=%d\n", m.focus, m.wantCursor, m.cursor)
	for i, r := range m.rows {
		pid := 0
		if r.kind == rowProc {
			pid = r.node.PID
		}
		fmt.Printf("  row %d: pid=%d run=%d sel=%v\n", i, pid, len(r.run), i == m.cursor)
	}
	if r, ok := m.selected(); !ok || r.kind != rowProc || !r.holds(got.PID) {
		t.Errorf("cursor is not on the new shell")
	}
}

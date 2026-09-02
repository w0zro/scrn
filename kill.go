package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

// killRequest is a kill waiting on confirmation: the processes to signal, and
// what to call them while asking.
type killRequest struct {
	subject string
	nodes   []*ProcNode
}

// killResult is what became of one process a kill was aimed at. hungUp marks
// the ones scrn ended by taking their terminal away rather than by signalling
// them, so the report can say what actually happened.
type killResult struct {
	command string
	pid     int
	err     error
	hungUp  bool
}

// killedMsg reports the outcome of a kill. One process and a whole subtree are
// reported the same way, because a subtree routinely comes back mixed: it can
// span processes scrn is not allowed to signal, and ones that exited between
// the scan and the keystroke.
type killedMsg struct {
	subject string
	results []killResult
}

// spinRate is how often the marker beside a signalled process advances. It is
// the clock for the kill rather than for the list: it also paces the rescans
// that find out whether the process has gone.
const spinRate = 100 * time.Millisecond

// rescanFrames is how many frames pass between rescans while a kill is
// outstanding. An lsof sweep is far too expensive to run at the spinner's rate.
const rescanFrames = 4

// killLinger is how many frames a signalled process is given before scrn stops
// saying it is on its way out. A process that has not acted on SIGTERM in five
// seconds is not dying, it is refusing, and the marker should stop implying
// otherwise.
const killLinger = 50

// dyingProc is a process that has been signalled, counted in frames so one
// that ignores the signal can eventually be given up on.
type dyingProc struct {
	command string
	frames  int
}

// spinMsg advances the marker. Exactly one is ever in flight, the same
// discipline tickMsg keeps: a second kill joins the running chain instead of
// starting another that would double the frame rate.
type spinMsg struct{}

// spin schedules the next frame.
func spin() tea.Cmd {
	return tea.Tick(spinRate, func(time.Time) tea.Msg { return spinMsg{} })
}

// killTree sends SIGTERM to every process in the request. SIGTERM rather than
// SIGKILL: these are editors, shells and build tools, and they should get the
// chance to save, flush and tear down their own children.
//
// Parents are signalled before their children. A supervising process outliving
// the children it started will start them again — that is what a watcher is
// for — so the process that would do the restarting is stopped first.
// done carries the outcomes already settled before the command runs — the
// shells the server was asked to hang up, which needed no signal.
//
// Before anything is signalled, ps is asked when each pid's process began.
// The tree is a scan old, and on a busy machine a pid in it can have been
// freed and handed to a stranger in the meantime; a start time that no longer
// matches means the process the user aimed at is not the one holding the
// number. It is reported already gone — which, for the one aimed at, it is.
func killTree(req *killRequest, done []killResult) tea.Cmd {
	subject, nodes := req.subject, req.nodes
	return func() tea.Msg {
		msg := killedMsg{subject: subject, results: append([]killResult{}, done...)}
		started := startTimes(nodes)
		for _, n := range nodes {
			res := killResult{command: n.Command, pid: n.PID}
			if reused(n, started) {
				res.err = errGone
			} else {
				res.err = signal(n.PID)
			}
			msg.results = append(msg.results, res)
		}
		return msg
	}
}

// startTimes is when each of the given processes began, asked freshly of ps.
// nil when ps could not answer at all, which callers read as the check being
// unavailable rather than every process being gone.
func startTimes(nodes []*ProcNode) map[int]string {
	if len(nodes) == 0 {
		return nil
	}
	args := []string{"-o", "pid=,lstart="}
	for _, n := range nodes {
		args = append(args, "-p", strconv.Itoa(n.PID))
	}
	// ps exits nonzero when any asked-for pid is gone, while still listing
	// the rest; only nothing printed at all means it could not answer.
	out, err := listing(scanTimeout, "ps", args...)
	if err != nil && len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}

	table := map[int]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		pid, rest := cutField(line)
		n, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		table[n] = strings.Join(strings.Fields(rest), " ")
	}
	return table
}

// reused reports whether a pid now belongs to some other process than the
// scanned one: listed with a different start time, or missing from a table
// that answered. A scan that carried no start time has nothing to compare,
// and an empty table is ps failing — both let the signal proceed as it always
// did, where ESRCH still catches the truly gone.
func reused(n *ProcNode, started map[int]string) bool {
	if n.Started == "" || len(started) == 0 {
		return false
	}
	now, ok := started[n.PID]
	return !ok || now != n.Started
}

// subtree lists a node and everything below it, parents first. It reads the
// tree scrn last scanned, so a process started since is not in it — the kill
// covers what the user was looking at.
func subtree(n *ProcNode) []*ProcNode {
	out := []*ProcNode{n}
	for _, c := range n.Children {
		out = append(out, subtree(c)...)
	}
	return out
}

// procLabel names a process the way the navigator does.
func procLabel(n *ProcNode) string {
	return n.Command + " " + strconv.Itoa(n.PID)
}

// errGone says the process was not there to signal. It is not a failure: a
// kill asks for the process to be gone, and it is.
var errGone = errors.New("already gone")

// signal sends SIGTERM, translating the failures worth explaining.
func signal(pid int) error {
	switch {
	case pid <= 1:
		return errors.New("refusing to signal pid " + strconv.Itoa(pid))
	case pid == os.Getpid():
		return errors.New("refusing to signal scrn itself")
	}

	err := syscall.Kill(pid, syscall.SIGTERM)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return errGone
	case errors.Is(err, syscall.EPERM):
		return errors.New("not permitted")
	}
	return err
}

package main

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// killRequest is a kill waiting on confirmation: the processes to signal, and
// what to call them while asking.
type killRequest struct {
	subject string
	nodes   []*ProcNode
}

// killResult is what became of one process a kill was aimed at.
type killResult struct {
	command string
	pid     int
	err     error
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

// spinFrames is the marker itself, drawn in red beside a process that has been
// signalled but is still listed.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

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
func killTree(req *killRequest) tea.Cmd {
	subject, nodes := req.subject, req.nodes
	return func() tea.Msg {
		msg := killedMsg{subject: subject, results: make([]killResult, 0, len(nodes))}
		for _, n := range nodes {
			msg.results = append(msg.results, killResult{
				command: n.Command,
				pid:     n.PID,
				err:     signal(n.PID),
			})
		}
		return msg
	}
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
		return errors.New("already gone")
	case errors.Is(err, syscall.EPERM):
		return errors.New("not permitted")
	}
	return err
}

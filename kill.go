package main

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// killedMsg reports the outcome of a kill so the footer can say what happened.
type killedMsg struct {
	command string
	pid     int
	err     error
}

// killGrace is how long to wait before rescanning after a signal. A process
// given SIGTERM gets a moment to act on it, so the list does not redraw with
// the process still in it.
const killGrace = 400 * time.Millisecond

// killProc sends SIGTERM to a process. SIGTERM rather than SIGKILL: these are
// editors, shells and build tools, and they should get the chance to save,
// flush and tear down their own children.
func killProc(n *ProcNode) tea.Cmd {
	command, pid := n.Command, n.PID
	return func() tea.Msg {
		return killedMsg{command: command, pid: pid, err: signal(pid)}
	}
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

// rescanAfter refreshes the process list once the grace period has passed.
func rescanAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return scanProcs() })
}

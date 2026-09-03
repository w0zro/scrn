package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSignalRefusesDangerousTargets(t *testing.T) {
	for _, tc := range []struct {
		pid  int
		want string
	}{
		{0, "refusing"},
		{1, "refusing"},
		{os.Getpid(), "conn itself"},
	} {
		err := signal(tc.pid)
		if err == nil {
			t.Fatalf("signal(%d) succeeded, want a refusal", tc.pid)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("signal(%d) = %q, want it to mention %q", tc.pid, err, tc.want)
		}
	}
}

func TestSignalReportsAMissingProcess(t *testing.T) {
	// A process that has exited and been reaped.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	err := signal(cmd.Process.Pid)
	if err == nil || !strings.Contains(err.Error(), "already gone") {
		t.Errorf("signal on a dead pid = %v, want %q", err, "already gone")
	}
}

func TestSignalTerminatesARealProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	if err := signal(pid); err != nil {
		t.Fatalf("signal(%d) = %v, want it to terminate", pid, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if err == nil {
			t.Fatal("process exited cleanly, want termination by signal")
		}
		if !asExitError(err, &ee) {
			t.Fatalf("wait returned %v", err)
		}
		ws := ee.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
			t.Errorf("exit status = %v, want termination by SIGTERM", ws)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
}

func TestScanAndKillAgreeOnAStartTime(t *testing.T) {
	// The identity check only works if the token the scan stores and the one
	// the kill reads back are written the same way — ps pads single-digit
	// days, and both sides must normalize alike.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	scanned := psTable()[pid].started
	if len(strings.Fields(scanned)) != 5 {
		t.Fatalf("psTable started = %q, want the five fields of lstart", scanned)
	}
	fresh := startTimes([]*ProcNode{{Proc: Proc{PID: pid}}})[pid]
	if scanned != fresh {
		t.Errorf("scan says %q, kill check says %q; the same process must read the same", scanned, fresh)
	}
}

func TestKillPassesByARecycledPid(t *testing.T) {
	// The row is a scan old: a pid whose process no longer matches the start
	// time the scan saw belongs to a stranger, and must not be signalled.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	req := &killRequest{subject: "old", nodes: []*ProcNode{
		{Proc: Proc{PID: pid, Command: "gone-thing", Started: "Mon Jan 1 00:00:00 1990"}},
	}}
	msg := killTree(req, nil)().(killedMsg)

	if len(msg.results) != 1 || !errors.Is(msg.results[0].err, errGone) {
		t.Fatalf("results = %+v, want the stale pid reported already gone", msg.results)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Error("the process holding the recycled pid was signalled; it is not what was aimed at")
	}
}

func TestKillSignalsAPidStillItself(t *testing.T) {
	// The same check must not get in an honest kill's way.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	req := &killRequest{subject: "sleep", nodes: []*ProcNode{
		{Proc: Proc{PID: pid, Command: "sleep", Started: psTable()[pid].started}},
	}}
	msg := killTree(req, nil)().(killedMsg)
	if len(msg.results) != 1 || msg.results[0].err != nil {
		t.Fatalf("results = %+v, want the signal to have landed", msg.results)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

func TestAProcessAlreadyGoneIsNotAFailure(t *testing.T) {
	// Hanging up a shell can take what was running in it before the signal
	// aimed at that process lands. The outcome asked for is what happened.
	results := []killResult{
		{command: "zsh", pid: 700, hungUp: true},
		{command: "claude", pid: 701, err: errGone},
	}
	if got := describeFailures(results); got != "" {
		t.Errorf("failures = %q, want a process that is already gone not counted", got)
	}
	if got := ended(results); got != "closed " {
		t.Errorf("ended = %q, want the outcome read as done", got)
	}
}

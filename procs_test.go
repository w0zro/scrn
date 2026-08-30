package main

import (
	"os"
	"testing"
	"time"
)

func TestUnderMatchesRepoAndNested(t *testing.T) {
	for _, tc := range []struct {
		dir, path string
		want      bool
	}{
		{"/p/repo", "/p/repo", true},
		{"/p/repo/src/deep", "/p/repo", true},
		{"/p/repo-other", "/p/repo", false}, // prefix, but a different repo
		{"/p", "/p/repo", false},
		{"/other", "/p/repo", false},
	} {
		if got := under(tc.dir, tc.path); got != tc.want {
			t.Errorf("under(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}

func TestProcDirsAreDistinct(t *testing.T) {
	got := procDirs([]Proc{
		{PID: 1, Dir: "/a"}, {PID: 2, Dir: "/a"}, {PID: 3, Dir: "/b"},
	})
	if len(got) != 2 {
		t.Errorf("procDirs = %v, want 2 distinct dirs", got)
	}
}

func TestRunningProcsFindsThisTest(t *testing.T) {
	procs, err := runningProcs()
	if err != nil {
		t.Skipf("lsof unavailable: %v", err)
	}
	if len(procs) == 0 {
		t.Fatal("no processes found")
	}

	// The test binary runs in this package's directory, so that directory must
	// show up — and this process itself must not, since scrn excludes its own.
	cwd, _ := os.Getwd()
	var sawCwd bool
	for _, p := range procs {
		if p.PID == os.Getpid() {
			t.Errorf("runningProcs included our own pid %d", p.PID)
		}
		if p.Dir == cwd {
			sawCwd = true
		}
	}
	if !sawCwd {
		t.Errorf("no process reported %q as its cwd", cwd)
	}
}

func TestProcForestNestsChildren(t *testing.T) {
	roots := procForest([]Proc{
		{PID: 10, PPID: 1, Command: "zsh"},
		{PID: 20, PPID: 10, Command: "claude"},
		{PID: 30, PPID: 20, Command: "go"},
		{PID: 40, PPID: 10, Command: "vim"},
	})

	if len(roots) != 1 || roots[0].PID != 10 {
		t.Fatalf("roots = %v, want a single root pid 10", pids(roots))
	}
	if got := pids(roots[0].Children); len(got) != 2 || got[0] != 20 || got[1] != 40 {
		t.Errorf("children of 10 = %v, want [20 40] in name order", got)
	}
	if got := pids(roots[0].Children[0].Children); len(got) != 1 || got[0] != 30 {
		t.Errorf("children of 20 = %v, want [30]", got)
	}
}

func TestSiblingsOrderByNameSoARestartHoldsItsSlot(t *testing.T) {
	// pid 95 is a Node restarted long after its siblings; name order keeps it
	// in the N slot instead of dropping it to the bottom, case aside. The two
	// zsh rows fall back to pid order, which for same-named siblings is the
	// natural reading: oldest first.
	roots := procForest([]Proc{
		{PID: 10, PPID: 1, Command: "zsh"},
		{PID: 21, PPID: 10, Command: "vim"},
		{PID: 95, PPID: 10, Command: "Node"},
		{PID: 30, PPID: 10, Command: "go"},
		{PID: 40, PPID: 10, Command: "zsh"},
		{PID: 22, PPID: 10, Command: "zsh"},
	})

	want := []int{30, 95, 21, 22, 40}
	got := pids(roots[0].Children)
	if len(got) != len(want) {
		t.Fatalf("children of 10 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("children of 10 = %v, want %v: name order, ties by pid", got, want)
		}
	}
}

func TestProcForestRootsProcessesWhoseParentIsAbsent(t *testing.T) {
	// The parent runs outside the repo, so it is not in the set.
	roots := procForest([]Proc{
		{PID: 20, PPID: 999, Command: "claude"},
		{PID: 21, PPID: 998, Command: "vim"},
	})
	if got := pids(roots); len(got) != 2 {
		t.Errorf("roots = %v, want both processes rooted", got)
	}
}

func TestProcForestSurvivesCycles(t *testing.T) {
	done := make(chan []*ProcNode, 1)
	go func() {
		done <- procForest([]Proc{
			{PID: 10, PPID: 20}, {PID: 20, PPID: 10},
		})
	}()
	select {
	case roots := <-done:
		if len(roots) == 0 {
			t.Error("a cycle should still yield a root rather than nothing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("procForest hung on a parent cycle")
	}
}

func TestProcForestIgnoresSelfParent(t *testing.T) {
	roots := procForest([]Proc{{PID: 10, PPID: 10}})
	if len(roots) != 1 || len(roots[0].Children) != 0 {
		t.Error("a process parented to itself should be a childless root")
	}
}

func pids(ns []*ProcNode) []int {
	out := make([]int, len(ns))
	for i, n := range ns {
		out[i] = n.PID
	}
	return out
}

func pidOfSelf() int { return os.Getpid() }

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o644) }

func TestTheScanDoesNotReportItself(t *testing.T) {
	// scrn's own children — the lsof running the scan, the git and ps behind
	// the detail pane — inherit its working directory, so without filtering
	// they flicker through the tree of whatever repo scrn was started in.
	procs, err := runningProcs()
	if err != nil {
		t.Skipf("lsof unavailable: %v", err)
	}

	self := os.Getpid()
	for _, p := range procs {
		if p.PPID == self {
			t.Errorf("the scan reported a child of scrn's own: %+v", p)
		}
		if p.PID == self {
			t.Errorf("the scan reported scrn itself: %+v", p)
		}
	}
}

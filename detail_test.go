package main

import (
	"os/exec"
	"strings"
	"testing"
)

func fieldValue(fs []field, label string) (string, bool) {
	for _, f := range fs {
		if f.label == label {
			return f.value, true
		}
	}
	return "", false
}

func TestDescribeStatusCountsChanges(t *testing.T) {
	for _, tc := range []struct{ porcelain, want string }{
		{"", "clean"},
		{"?? new.go\n", "1 untracked"},
		{" M edited.go\n", "1 modified"},
		{"M  staged.go\n", "1 staged"},
		{"MM both.go\n", "1 staged, 1 modified"},
		{"M  a.go\n M b.go\n?? c.go\n", "1 staged, 1 modified, 1 untracked"},
	} {
		if got := describeStatus(tc.porcelain); got != tc.want {
			t.Errorf("describeStatus(%q) = %q, want %q", tc.porcelain, got, tc.want)
		}
	}
}

func TestDescribeStateNamesTheCode(t *testing.T) {
	if got := describeState("S+"); !strings.HasPrefix(got, "sleeping") {
		t.Errorf("describeState(\"S+\") = %q, want it to name the state", got)
	}
	if got := describeState("?"); got != "?" {
		t.Errorf("an unknown code should pass through, got %q", got)
	}
}

func TestCountTreeCountsDescendants(t *testing.T) {
	n := &ProcNode{Proc: Proc{PID: 1}, Children: []*ProcNode{
		{Proc: Proc{PID: 2}, Children: []*ProcNode{{Proc: Proc{PID: 3}}}},
		{Proc: Proc{PID: 4}},
	}}
	if got := countTree(n); got != 4 {
		t.Errorf("countTree = %d, want 4", got)
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "process", "processes"); got != "1 process" {
		t.Errorf("got %q", got)
	}
	if got := plural(0, "process", "processes"); got != "0 processes" {
		t.Errorf("got %q", got)
	}
}

func TestRepoFieldsDescribeARealRepo(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		// Ignore the developer's own git config: commit signing and hooks
		// would otherwise decide whether this test can run.
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := writeFile(dir+"/a.txt", "hi"); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-qm", "first commit")
	if err := writeFile(dir+"/dirty.txt", "x"); err != nil {
		t.Fatal(err)
	}

	fs := repoFields(Project{Name: "demo", Path: dir}, 2)

	if v, ok := fieldValue(fs, "branch"); !ok || v != "main" {
		t.Errorf("branch = %q (present=%v), want main", v, ok)
	}
	if v, ok := fieldValue(fs, "status"); !ok || !strings.Contains(v, "untracked") {
		t.Errorf("status = %q, want it to mention the untracked file", v)
	}
	if v, ok := fieldValue(fs, "last commit"); !ok || !strings.Contains(v, "first commit") {
		t.Errorf("last commit = %q, want the subject", v)
	}
	if v, ok := fieldValue(fs, "running"); !ok || v != "2 processes" {
		t.Errorf("running = %q, want %q", v, "2 processes")
	}
}

func TestRepoFieldsSurviveANonRepo(t *testing.T) {
	fs := repoFields(Project{Name: "nope", Path: t.TempDir()}, 0)

	if v, ok := fieldValue(fs, "path"); !ok || v == "" {
		t.Error("a non-repo should still report its path")
	}
	if _, ok := fieldValue(fs, "git"); !ok {
		t.Error("a directory git cannot read should say so rather than look clean")
	}
}

func TestProcFieldsDescribeThisProcess(t *testing.T) {
	self := &ProcNode{Proc: Proc{PID: pidOfSelf(), PPID: 1, Command: "test", Dir: "/tmp"}}
	fs := procFields(self, nil)

	for _, label := range []string{"command", "pid", "parent", "cwd", "argv", "uptime", "cpu"} {
		if _, ok := fieldValue(fs, label); !ok {
			t.Errorf("procFields is missing %q: %+v", label, fs)
		}
	}
}

func TestDetailKeyDistinguishesReposFromProcesses(t *testing.T) {
	repo := detailKey(navRow{kind: rowProject, project: Project{Path: "/p/a"}})
	proc := detailKey(navRow{kind: rowProc, node: &ProcNode{Proc: Proc{PID: 7}}})
	if repo == proc {
		t.Error("a repo and a process should not share a detail key")
	}
}

func TestRepoFieldsHandleARepoWithNoCommits(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q", "-b", "main")
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	fs := repoFields(Project{Name: "fresh", Path: dir}, 0)

	if v, ok := fieldValue(fs, "branch"); !ok || v != "main" {
		t.Errorf("branch = %q (present=%v), want main; a fresh repo still has one", v, ok)
	}
	if _, ok := fieldValue(fs, "git"); ok {
		t.Error("a fresh repo should not be reported as unreadable")
	}
	if v, ok := fieldValue(fs, "last commit"); !ok || v != "none yet" {
		t.Errorf("last commit = %q, want %q", v, "none yet")
	}
}

func TestGitErrorsSayWhatGitSaid(t *testing.T) {
	_, err := git(t.TempDir(), "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("expected an error outside a repository")
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Errorf("error = %q, want git's own message rather than an exit code", err)
	}
	if !strings.Contains(err.Error(), "repository") {
		t.Errorf("error = %q, want it to mention the missing repository", err)
	}
}

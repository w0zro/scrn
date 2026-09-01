package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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

	fs := repoFields(Project{Name: "demo", Path: dir}, 2, nil)

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
	fs := repoFields(Project{Name: "nope", Path: t.TempDir()}, 0, nil)

	if noteOf(fs) == "" {
		t.Error("a non-repo should still report its path")
	}
	if _, ok := fieldValue(fs, "git"); !ok {
		t.Error("a directory git cannot read should say so rather than look clean")
	}
}

func TestProcFieldsDescribeThisProcess(t *testing.T) {
	self := &ProcNode{Proc: Proc{PID: pidOfSelf(), PPID: 1, Command: "test", Dir: "/tmp"}}
	fs := procFields(self, nil, nil)

	// What it is and where it runs head the pane rather than sitting in the
	// list, because they are what the pane is about.
	if got := headingOf(fs); got != procLabel(self) {
		t.Errorf("heading = %q, want %q", got, procLabel(self))
	}
	if got := noteOf(fs); got != "/tmp" {
		t.Errorf("note = %q, want the working directory", got)
	}
	for _, label := range []string{"parent", "argv", "uptime", "cpu", "memory", "state"} {
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

	fs := repoFields(Project{Name: "fresh", Path: dir}, 0, nil)

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

// headingOf and noteOf are the lines a pane leads with.
func headingOf(fs []field) string { return firstOfKind(fs, headingField) }
func noteOf(fs []field) string    { return firstOfKind(fs, noteField) }

func firstOfKind(fs []field, kind fieldKind) string {
	for _, f := range fs {
		if f.kind == kind {
			return f.value
		}
	}
	return ""
}

// pairsOf is the labelled lines alone, without the breaks between groups.
func pairsOf(fs []field) []field {
	var out []field
	for _, f := range fs {
		if f.kind == pairField {
			out = append(out, f)
		}
	}
	return out
}

func TestAPaneLeadsWithWhatItIsAbout(t *testing.T) {
	fs := repoFields(Project{Name: "alpha", Path: "/p/alpha"}, 0, nil)
	if got := headingOf(fs); got != "alpha" {
		t.Errorf("heading = %q, want the repository's name", got)
	}
	if got := noteOf(fs); got != "/p/alpha" {
		t.Errorf("note = %q, want its path", got)
	}
	for _, f := range pairsOf(fs) {
		if f.label == "name" || f.label == "path" {
			t.Errorf("%q is still in the list as well as at the top", f.label)
		}
	}
}

func TestEachGroupSetsItsOwnValueColumn(t *testing.T) {
	// One long label should indent its own group and no others, or a single
	// "session id" pushes the whole pane across.
	fields := []field{
		{label: "a", value: "1"},
		gap(),
		{label: "a-very-long-label", value: "2"},
	}
	lines := []string{}
	for _, b := range blocks(fields) {
		lines = append(lines, renderBlock(b, 60)...)
	}

	if got := stripANSI(lines[0]); got != "  a  1" {
		t.Errorf("first group = %q, want it tight to its own widest label", got)
	}
	if got := stripANSI(lines[1]); got != "  a-very-long-label  2" {
		t.Errorf("second group = %q, want its own column", got)
	}
}

func TestAGroupWithNothingInItDrawsNothing(t *testing.T) {
	// A pane that skipped a whole group should not leave a hole where it
	// would have been.
	fields := []field{{label: "a", value: "1"}, gap(), gap(), {label: "b", value: "2"}}

	var lines []string
	for _, b := range blocks(fields) {
		if drawn := renderBlock(b, 60); len(drawn) > 0 {
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, drawn...)
		}
	}
	if len(lines) != 3 {
		t.Errorf("lines = %q, want two groups and one break between them", lines)
	}
}

func TestTheRunsPortsAreTheRowsPorts(t *testing.T) {
	// A dev server is a shell running an npm running a node, and it is the
	// node at the bottom that holds the port — the one the fold exists to
	// hide. Asking only the process the row is named for found nothing.
	c := exec.Command("python3", "-m", "http.server", "8932")
	c.Dir = "/tmp"
	if err := c.Start(); err != nil {
		t.Skip(err)
	}
	defer func() { _ = c.Process.Kill() }()
	time.Sleep(1500 * time.Millisecond)

	// The row is named for something above the listener, as a folded run is.
	named := &ProcNode{Proc: Proc{PID: os.Getpid(), Command: "npm", Dir: "/tmp"}}
	listener := &ProcNode{Proc: Proc{PID: c.Process.Pid, Command: "node", Dir: "/tmp"}}
	run := []*ProcNode{named, listener}

	got, ok := fieldValue(procFields(named, run, nil), "listening")
	if !ok {
		t.Fatalf("nothing reported, want the port the run is listening on")
	}
	if got != "8932" {
		t.Errorf("listening = %q, want the port held further down the run", got)
	}

	// And with no run, the row still speaks for itself.
	if _, ok := fieldValue(procFields(listener, nil, nil), "listening"); !ok {
		t.Error("a row that folded nothing should still report its own port")
	}
}

func TestTonesFollowTheFacts(t *testing.T) {
	// A value carries its state in color: alive is green, wrong is red, and
	// what is merely true recedes.
	if got := runningField(2).tone; got != toneGood {
		t.Errorf("2 running tone = %v, want good", got)
	}
	if got := runningField(0).tone; got != toneQuiet {
		t.Errorf("0 running tone = %v, want quiet", got)
	}
	if got := stateTone("R+"); got != toneGood {
		t.Errorf("running state tone = %v, want good", got)
	}
	if got := stateTone("Z"); got != toneBad {
		t.Errorf("zombie state tone = %v, want bad", got)
	}
	if got := stateTone("Ss"); got != tonePlain {
		t.Errorf("sleeping state tone = %v, want plain", got)
	}
}

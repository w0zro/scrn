package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// field is one line in the detail pane. Most are a label and a value, but a
// pane also needs to say what it is about and to group what it says: a flat
// list gives the same weight to what a session is doing as to its memory
// share, and leaves the reader to find the difference.
type field struct {
	label string
	value string
	kind  fieldKind
}

type fieldKind int

const (
	pairField    fieldKind = iota // a label and a value
	headingField                  // what the pane is about
	noteField                     // a quieter line under the heading
	gapField                      // a break between groups
)

func heading(s string) field { return field{value: s, kind: headingField} }
func note(s string) field    { return field{value: s, kind: noteField} }
func gap() field             { return field{kind: gapField} }

// detailMsg carries the inspection of whatever the cursor was on when it was
// requested. The key identifies the subject so a slow lookup that lands after
// the cursor has moved on can be discarded.
type detailMsg struct {
	key    string
	fields []field
}

// detailKey identifies the subject of a row, so details can be cached and
// stale results dropped.
func detailKey(r navRow) string {
	if r.kind == rowProc {
		return "proc:" + strconv.Itoa(r.node.PID)
	}
	return "repo:" + r.project.Path
}

// loadDetail inspects the selected row off the render path. Git and ps are
// fast, but they are still processes, and the UI should not wait on them.
func loadDetail(r navRow, procCount int, sess *claudeSession, running map[string]bool) tea.Cmd {
	key := detailKey(r)
	if r.kind == rowProc {
		node, run := r.node, r.run
		return func() tea.Msg {
			return detailMsg{key: key, fields: procFields(node, run, sess)}
		}
	}
	p := r.project
	return func() tea.Msg {
		return detailMsg{key: key, fields: repoFields(p, procCount, running)}
	}
}

// repoFields describes a repository: where it is, what state its checkout is
// in, and what is running in it.
func repoFields(p Project, procCount int, running map[string]bool) []field {
	fs := []field{
		heading(p.Name),
		note(p.Path),
		gap(),
	}

	// "branch --show-current" rather than "rev-parse HEAD": a repository with
	// no commits yet still has a branch, and rev-parse fails on it.
	branch, err := git(p.Path, "branch", "--show-current")
	switch {
	case err != nil:
		fs = append(fs, field{label: "git", value: "unavailable: " + err.Error()})
	case branch == "":
		if sha, e := git(p.Path, "rev-parse", "--short", "HEAD"); e == nil {
			fs = append(fs, field{label: "branch", value: "detached at " + sha})
		}
	default:
		fs = append(fs, field{label: "branch", value: branch})
	}

	if status, err := git(p.Path, "status", "--porcelain"); err == nil {
		fs = append(fs, field{label: "status", value: describeStatus(status)})
	}
	if upstream, err := git(p.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		fs = append(fs, field{label: "upstream", value: upstream + describeAheadBehind(p.Path)})
	}
	fs = append(fs, gap())
	if last, e := git(p.Path, "log", "-1", "--format=%h  %s"); e == nil {
		fs = append(fs, field{label: "last commit", value: last})
		if when, e := git(p.Path, "log", "-1", "--format=%cr  by %an"); e == nil {
			fs = append(fs, field{label: "", value: when})
		}
	} else if err == nil {
		// A repository git could read, with a branch but nothing on it yet.
		fs = append(fs, field{label: "last commit", value: "none yet"})
	}
	if origin, err := git(p.Path, "remote", "get-url", "origin"); err == nil {
		fs = append(fs, field{label: "origin", value: origin})
	}

	fs = append(fs, gap(), field{label: "running", value: plural(procCount, "process", "processes")})

	// What the project says it needs, and which of those are up. It is the
	// list u works from, so showing it is showing what u would do.
	if plan := readPlan(p.Path); len(plan.Entries) > 0 {
		fs = append(fs, gap())
		for i, e := range plan.Entries {
			label := "needs"
			if i > 0 {
				label = "" // the rest line up under the first
			}
			mark := "○ "
			if running[e.Name] {
				mark = "● "
			}
			fs = append(fs, field{label: label, value: mark + e.Name + "  " + e.Run})
		}
		fs = append(fs, field{label: "from", value: plan.Source})
	}
	return fs
}

// describeStatus turns porcelain output into a count of what changed.
func describeStatus(porcelain string) string {
	// Only trailing newlines may go: the first two columns are the status
	// itself, and a leading space is what distinguishes " M" (modified in the
	// worktree) from "M " (staged).
	lines := strings.Split(strings.Trim(porcelain, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return "clean"
	}
	var staged, unstaged, untracked int
	for _, ln := range lines {
		if len(ln) < 2 {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "??"):
			untracked++
		default:
			if ln[0] != ' ' {
				staged++
			}
			if ln[1] != ' ' {
				unstaged++
			}
		}
	}
	var parts []string
	if staged > 0 {
		parts = append(parts, fmt.Sprintf("%d staged", staged))
	}
	if unstaged > 0 {
		parts = append(parts, fmt.Sprintf("%d modified", unstaged))
	}
	if untracked > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", untracked))
	}
	if len(parts) == 0 {
		return "clean"
	}
	return strings.Join(parts, ", ")
}

// describeAheadBehind reports divergence from the upstream branch, as a
// suffix so it reads as part of the upstream line.
func describeAheadBehind(path string) string {
	counts, err := git(path, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if err != nil {
		return ""
	}
	f := strings.Fields(counts)
	if len(f) != 2 {
		return ""
	}
	ahead, behind := f[0], f[1]
	switch {
	case ahead == "0" && behind == "0":
		return "  (in sync)"
	case behind == "0":
		return "  (" + ahead + " ahead)"
	case ahead == "0":
		return "  (" + behind + " behind)"
	}
	return "  (" + ahead + " ahead, " + behind + " behind)"
}

// procFields describes a running process: what it is, where it runs, and how
// long it has been going.
func procFields(n *ProcNode, run []*ProcNode, sess *claudeSession) []field {
	fs := []field{
		heading(procLabel(n)),
		note(n.Dir),
	}

	// What a Claude Code instance is doing outranks the process table: it is
	// the reason the process is worth looking at, so it comes first.
	if sess != nil {
		readTranscript(transcriptPath(*sess), sess)
		fs = append(fs, claudeFields(*sess)...)
	}

	fs = append(fs, gap())
	// The navigator folds a run that never branches into the one row, so the
	// shell that started this and anything between them is not on screen
	// anywhere else. This is where it is said.
	if len(run) > 1 {
		fs = append(fs, field{label: "run", value: describeRun(run)})
	}
	fs = append(fs, field{label: "parent", value: strconv.Itoa(n.PPID)})

	if argv, err := ps(n.PID, "command="); err == nil && argv != "" {
		fs = append(fs, field{label: "argv", value: argv})
	}
	fs = append(fs, gap())
	if stats, err := ps(n.PID, "etime=,%cpu=,%mem="); err == nil {
		if f := strings.Fields(stats); len(f) == 3 {
			fs = append(fs,
				field{label: "uptime", value: f[0]},
				field{label: "cpu", value: f[1] + "%"},
				field{label: "memory", value: f[2] + "%"},
			)
		}
	}
	if started, err := ps(n.PID, "lstart="); err == nil && started != "" {
		fs = append(fs, field{label: "started", value: started})
	}
	if state, err := ps(n.PID, "stat="); err == nil && state != "" {
		fs = append(fs, field{label: "state", value: describeState(state)})
	}

	// Where it is, for the ones that are anywhere: a dev server's row says
	// what it is, and this says what to open.
	//
	// The whole run is asked, not just the process the row is named for. A
	// dev server is a shell running an npm running a node, and it is the node
	// at the bottom that holds the port — the one the fold exists to hide. The
	// row stands for the run, so the run's ports are the row's.
	if ports := runPorts(run, n); len(ports) > 0 {
		fs = append(fs, field{label: "listening", value: strings.Join(ports, ", ")})
	}

	if kids := countTree(n) - 1; kids > 0 {
		fs = append(fs, field{label: "children", value: plural(kids, "process", "processes")})
	}
	return fs
}

// runPorts is everything the processes a row stands for are listening on.
func runPorts(run []*ProcNode, n *ProcNode) []string {
	nodes := run
	if len(nodes) == 0 {
		nodes = []*ProcNode{n}
	}

	seen := map[string]bool{}
	var ports []string
	for _, node := range nodes {
		for _, p := range listeningPorts(node.PID) {
			if !seen[p] {
				seen[p] = true
				ports = append(ports, p)
			}
		}
	}
	sortPorts(ports)
	return ports
}

// describeRun names every process in a folded run, oldest first, so the shell
// the row was started from is the first thing in it.
func describeRun(run []*ProcNode) string {
	parts := make([]string, 0, len(run))
	for _, n := range run {
		parts = append(parts, procLabel(n))
	}
	return strings.Join(parts, " › ")
}

// describeState expands the leading character of a ps state code, which is the
// part that says whether the process is doing anything.
func describeState(stat string) string {
	if stat == "" {
		return stat
	}
	names := map[byte]string{
		'R': "running", 'S': "sleeping", 'I': "idle",
		'T': "stopped", 'U': "uninterruptible wait", 'Z': "zombie",
	}
	if name, ok := names[stat[0]]; ok {
		return name + "  (" + stat + ")"
	}
	return stat
}

// countTree counts a node and everything beneath it.
func countTree(n *ProcNode) int {
	total := 1
	for _, c := range n.Children {
		total += countTree(c)
	}
	return total
}

// git runs a git command, reporting what git printed rather than just an exit
// status: "not a git repository" is worth showing, "exit status 128" is not.
func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if msg := firstLine(string(ee.Stderr)); msg != "" {
				return "", errors.New(strings.TrimPrefix(msg, "fatal: "))
			}
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}

func ps(pid int, format string) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", format).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

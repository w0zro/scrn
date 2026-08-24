package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// field is one labelled line in the detail pane.
type field struct {
	label string
	value string
}

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
func loadDetail(r navRow, procCount int, sess *claudeSession) tea.Cmd {
	key := detailKey(r)
	if r.kind == rowProc {
		node, run := r.node, r.run()
		return func() tea.Msg {
			return detailMsg{key: key, fields: procFields(node, run, sess)}
		}
	}
	p := r.project
	return func() tea.Msg {
		return detailMsg{key: key, fields: repoFields(p, procCount)}
	}
}

// repoFields describes a repository: where it is, what state its checkout is
// in, and what is running in it.
func repoFields(p Project, procCount int) []field {
	fs := []field{
		{"name", p.Name},
		{"path", p.Path},
	}

	// "branch --show-current" rather than "rev-parse HEAD": a repository with
	// no commits yet still has a branch, and rev-parse fails on it.
	branch, err := git(p.Path, "branch", "--show-current")
	switch {
	case err != nil:
		fs = append(fs, field{"git", "unavailable: " + err.Error()})
	case branch == "":
		if sha, e := git(p.Path, "rev-parse", "--short", "HEAD"); e == nil {
			fs = append(fs, field{"branch", "detached at " + sha})
		}
	default:
		fs = append(fs, field{"branch", branch})
	}

	if status, err := git(p.Path, "status", "--porcelain"); err == nil {
		fs = append(fs, field{"status", describeStatus(status)})
	}
	if upstream, err := git(p.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		fs = append(fs, field{"upstream", upstream + describeAheadBehind(p.Path)})
	}
	if last, e := git(p.Path, "log", "-1", "--format=%h  %s"); e == nil {
		fs = append(fs, field{"last commit", last})
		if when, e := git(p.Path, "log", "-1", "--format=%cr  by %an"); e == nil {
			fs = append(fs, field{"", when})
		}
	} else if err == nil {
		// A repository git could read, with a branch but nothing on it yet.
		fs = append(fs, field{"last commit", "none yet"})
	}
	if origin, err := git(p.Path, "remote", "get-url", "origin"); err == nil {
		fs = append(fs, field{"origin", origin})
	}

	fs = append(fs, field{"running", plural(procCount, "process", "processes")})
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
		{"command", n.Command},
		{"pid", strconv.Itoa(n.PID)},
	}

	// The navigator folds a run that never branches into the one row, so the
	// shell that started this and anything between them is not on screen
	// anywhere else. This is where it is said.
	if len(run) > 1 {
		fs = append(fs, field{"run", describeRun(run)})
	}

	// What a Claude Code instance is doing outranks the process table: it is
	// the reason the process is worth looking at.
	if sess != nil {
		readTranscript(transcriptPath(*sess), sess)
		fs = append(fs, claudeFields(*sess)...)
	}

	fs = append(fs,
		field{"parent", strconv.Itoa(n.PPID)},
		field{"cwd", n.Dir},
	)

	if argv, err := ps(n.PID, "command="); err == nil && argv != "" {
		fs = append(fs, field{"argv", argv})
	}
	if stats, err := ps(n.PID, "etime=,%cpu=,%mem="); err == nil {
		if f := strings.Fields(stats); len(f) == 3 {
			fs = append(fs,
				field{"uptime", f[0]},
				field{"cpu", f[1] + "%"},
				field{"memory", f[2] + "%"},
			)
		}
	}
	if started, err := ps(n.PID, "lstart="); err == nil && started != "" {
		fs = append(fs, field{"started", started})
	}
	if state, err := ps(n.PID, "stat="); err == nil && state != "" {
		fs = append(fs, field{"state", describeState(state)})
	}

	if kids := countTree(n) - 1; kids > 0 {
		fs = append(fs, field{"children", plural(kids, "process", "processes")})
	}
	return fs
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

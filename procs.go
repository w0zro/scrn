package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// scanTimeout bounds every command a scan runs. lsof answers in tens of
// milliseconds on a healthy machine; the bound is for the machine with a dead
// network mount, where lsof hangs for minutes and a scan that waited would
// stand in the way of every scan after it.
const scanTimeout = 15 * time.Second

// listing runs one of the commands the scans read, bounded by timeout. What
// was written before a failure is still returned: lsof exits nonzero when any
// process refuses it, which says nothing about the ones that answered.
//
// WaitDelay is for the process the timeout's kill does not take on — an lsof
// stuck in uninterruptible disk wait cannot be killed, and the scan has to
// come back even when the process never will.
func listing(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	return cmd.Output()
}

// Proc is a running process, identified by the directory it is working in.
//
// Command is the name of the program, which is what lsof knows. Argv is what
// was actually run, which lsof does not know and ps does: "npm run dev" is a
// node, and being told it is a node is no help at all.
type Proc struct {
	PID     int
	PPID    int
	Command string
	Argv    string
	Dir     string

	// Started is when the process began, as ps prints it — an opaque token
	// that, together with the pid, identifies the process the way a pid
	// alone cannot: pids are recycled, start times are not. A kill compares
	// it before signalling, so a row from an old scan cannot aim at whatever
	// inherited its number.
	Started string
}

// ProcNode is a process together with the processes it started.
type ProcNode struct {
	Proc
	Children []*ProcNode
}

// runningProcs lists the processes visible to this user along with their
// working directories.
//
// lsof is the only way to read another process's cwd on macOS; there is no
// /proc to walk. Processes owned by other users are reported as permission
// errors on stderr and simply do not appear, which is the behavior we want.
func runningProcs() ([]Proc, error) {
	out, err := listing(scanTimeout, "lsof", "-a", "-d", "cwd", "-F", "pcRn")
	if err != nil && len(out) == 0 {
		return nil, err
	}

	self := os.Getpid()
	var procs []Proc
	var cur Proc

	// What each process was run with and when it began, in one call. Asking
	// per process is milliseconds each, which is fine for the one row being
	// inspected and far too slow for a list being redrawn.
	ps := psTable()

	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 2 {
			continue
		}
		field, value := line[0], line[1:]
		switch field {
		case 'p':
			pid, err := strconv.Atoi(value)
			if err != nil {
				cur = Proc{}
				continue
			}
			cur = Proc{PID: pid}
		case 'R':
			cur.PPID, _ = strconv.Atoi(value)
		case 'c':
			cur.Command = value
		case 'n':
			// Neither scrn nor anything it started for itself is work
			// happening in a repository. Its own children — the lsof that ran
			// this scan, the git and ps behind the detail pane — inherit the
			// directory scrn was started in, so without this they appear and
			// disappear in that repository's tree on every refresh.
			//
			// scrn has no children worth showing: the shells it opens belong
			// to the tmux server, which is a different process and keeps its own
			// working directory well away from any project.
			if cur.PID == 0 || cur.PID == self || cur.PPID == self || !strings.HasPrefix(value, "/") {
				continue
			}
			cur.Dir = value
			cur.Argv = ps[cur.PID].argv
			cur.Started = ps[cur.PID].started
			procs = append(procs, cur)
		}
	}
	return procs, sc.Err()
}

// psInfo is what ps says about one process: when it began, and what it was
// run with.
type psInfo struct {
	started string
	argv    string
}

// psTable is every process's start time and command line, keyed by pid. A
// failure leaves it empty; a process without an entry falls back to its name
// and goes without the start-time check.
func psTable() map[int]psInfo {
	out, err := listing(scanTimeout, "ps", "-axo", "pid=,lstart=,command=")
	if err != nil {
		return nil
	}

	lines := strings.Split(string(out), "\n")
	table := make(map[int]psInfo, len(lines))
	for _, line := range lines {
		pid, rest := cutField(line)
		n, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		// lstart is five fields — "Fri Aug 29 10:00:00 2026" — and the
		// command line is everything after them, its own spacing kept. The
		// fields are rejoined rather than sliced out whole, so a padded
		// single-digit day reads the same here as anywhere else ps prints it.
		fields := make([]string, 5)
		for i := range fields {
			fields[i], rest = cutField(rest)
		}
		table[n] = psInfo{started: strings.Join(fields, " "), argv: strings.TrimSpace(rest)}
	}
	return table
}

// cutField takes the next space-separated field, leaving the rest.
func cutField(s string) (string, string) {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// procForest arranges processes into parent/child trees. A process whose parent
// is not in the set becomes a root, so a repo's trees start at the outermost
// process actually working in it rather than at some ancestor outside it.
func procForest(procs []Proc) []*ProcNode {
	byPID := make(map[int]*ProcNode, len(procs))
	for _, p := range procs {
		byPID[p.PID] = &ProcNode{Proc: p}
	}

	var roots []*ProcNode
	for _, p := range procs {
		n := byPID[p.PID]
		parent, ok := byPID[p.PPID]
		if ok && parent != n && !descends(parent, n, byPID) {
			parent.Children = append(parent.Children, n)
			continue
		}
		roots = append(roots, n)
	}

	sortNodes(roots)
	return roots
}

// descends reports whether a is inside b's subtree, which would make attaching
// b under a a cycle. Process trees are acyclic in practice; this keeps a
// surprising ps table from hanging the renderer.
func descends(a, b *ProcNode, byPID map[int]*ProcNode) bool {
	for cur, seen := a, 0; cur != nil && seen < len(byPID); seen++ {
		if cur == b {
			return true
		}
		cur = byPID[cur.PPID]
	}
	return false
}

// sortNodes orders each level by the name its row will wear, ties broken by
// PID. The raw command is the wrong key: every shell scrn holds is a zsh
// underneath, whatever runs inside it, and sorting on that put a fresh shell
// between two agents — three zsh ties, settled by pid. A name keeps its slot
// when the process behind it is restarted under a new pid, which is when PID
// order would move the row; identically-named siblings fall back to creation
// order, the one place it is the natural reading.
func sortNodes(ns []*ProcNode) {
	sort.Slice(ns, func(i, j int) bool {
		a, b := sortName(ns[i]), sortName(ns[j])
		if a != b {
			return a < b
		}
		return ns[i].PID < ns[j].PID
	})
	for _, n := range ns {
		sortNodes(n.Children)
	}
}

// sortName is the name a node's row answers to: the navigator collapses a
// chain with nothing to choose between and names it for the first non-shell
// in it, so the sort walks the same chain the same way. The bound is for a
// process table that says a process started itself.
func sortName(n *ProcNode) string {
	run := []*ProcNode{n}
	for i := 0; len(n.Children) == 1 && i < 1024; i++ {
		n = n.Children[0]
		run = append(run, n)
	}
	return strings.ToLower(commandOf(nameOf(run)))
}

// indexNodes files a tree by pid, so a process can be reached from anywhere
// that knows only its number.
func indexNodes(n *ProcNode, into map[int]*ProcNode) {
	into[n.PID] = n
	for _, c := range n.Children {
		indexNodes(c, into)
	}
}

// listeningPorts is the TCP ports a process is accepting connections on.
//
// It is asked per process rather than for the whole machine, because it is
// only ever wanted for the one row being looked at, and asking about one
// process costs about as little as asking is ever going to.
//
// A port is worth showing because it is the thing you were about to go and
// look up: a dev server's row says what it is, and this says where it is.
func listeningPorts(pid int) []string {
	out, err := listing(scanTimeout, "lsof", "-nP", "-a", "-p", strconv.Itoa(pid),
		"-iTCP", "-sTCP:LISTEN", "-F", "n")
	if err != nil && len(out) == 0 {
		// No listeners is an error exit and not worth reporting. What was
		// written before a failure still counts — the same lesson the
		// process scan learned: lsof exits nonzero for reasons that say
		// nothing about the sockets it did list, and Linux's is freer with
		// those reasons than macOS's.
		return nil
	}

	seen := map[string]bool{}
	var ports []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		// The address is host:port, and the host may itself contain colons
		// when it is an IPv6 address.
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		port := line[i+1:]
		if port == "" || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	sortPorts(ports)
	return ports
}

// sortPorts orders ports by number, so 80 comes before 8080.
func sortPorts(ports []string) {
	sort.Slice(ports, func(i, j int) bool { return lessPort(ports[i], ports[j]) })
}

func lessPort(a, b string) bool {
	x, errA := strconv.Atoi(a)
	y, errB := strconv.Atoi(b)
	if errA != nil || errB != nil {
		return a < b
	}
	return x < y
}

// procDirs reduces a process list to the distinct directories they run in.
func procDirs(procs []Proc) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, p := range procs {
		if !seen[p.Dir] {
			seen[p.Dir] = true
			dirs = append(dirs, p.Dir)
		}
	}
	return dirs
}

// under reports whether dir is path itself or nested inside it.
func under(dir, path string) bool {
	return dir == path || strings.HasPrefix(dir, strings.TrimSuffix(path, "/")+"/")
}

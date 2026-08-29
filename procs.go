package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

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
	cmd := exec.Command("lsof", "-a", "-d", "cwd", "-F", "pcRn")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := cmd.Wait(); err != nil && out.Len() == 0 {
		return nil, err
	}

	self := os.Getpid()
	var procs []Proc
	var cur Proc

	// What each process was run with, in one call. Asking per process is
	// milliseconds each, which is fine for the one row being inspected and far
	// too slow for a list being redrawn.
	argv := commandLines()

	sc := bufio.NewScanner(bytes.NewReader(out.Bytes()))
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
			// to the daemon, which is a different process and keeps its own
			// working directory well away from any project.
			if cur.PID == 0 || cur.PID == self || cur.PPID == self || !strings.HasPrefix(value, "/") {
				continue
			}
			cur.Dir = value
			cur.Argv = argv[cur.PID]
			procs = append(procs, cur)
		}
	}
	return procs, sc.Err()
}

// commandLines is what every process was run with, keyed by pid. A failure
// leaves it empty, and a process without one falls back to its name.
func commandLines() map[int]string {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}

	lines := strings.Split(string(out), "\n")
	argv := make(map[int]string, len(lines))
	for _, line := range lines {
		pid, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		argv[n] = strings.TrimSpace(rest)
	}
	return argv
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

// sortNodes orders each level by PID so the tree is stable between scans.
func sortNodes(ns []*ProcNode) {
	sort.Slice(ns, func(i, j int) bool { return ns[i].PID < ns[j].PID })
	for _, n := range ns {
		sortNodes(n.Children)
	}
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
	out, err := exec.Command("lsof", "-nP", "-a", "-p", strconv.Itoa(pid),
		"-iTCP", "-sTCP:LISTEN", "-F", "n").Output()
	if err != nil {
		return nil // no listeners is an error exit, and is not worth reporting
	}

	seen := map[string]bool{}
	var ports []string
	for _, line := range strings.Split(string(out), "\n") {
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

package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// navWidth is the column the navigator occupies, divider excluded.
const navWidth = 28

// navMin is the total width below which there is no room for a detail pane
// beside the navigator, so the navigator takes the whole width.
const navMin = 60

// procPoll is how often the process list is refreshed. Processes come and go
// constantly, and an lsof sweep is cheap enough to repeat at this rate.
const procPoll = 2 * time.Second

// projectEvery is how many process polls pass between repository scans.
// Repositories appear and disappear far more slowly than processes do.
const projectEvery = 15

// tickMsg drives the refresh loop. Exactly one tick is ever in flight: each
// one schedules its successor, so a one-off rescan — after a kill, say — can
// never start a second chain that doubles the polling rate.
type tickMsg struct{}

// tick schedules the next refresh.
func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

// projectsMsg carries the result of the startup scan.
type projectsMsg struct {
	projects []Project
	err      error
}

// procsMsg carries the running processes. It arrives at startup and again
// whenever the view is narrowed, so the narrowed list reflects what is running
// now rather than at launch.
type procsMsg struct {
	procs []Proc
	err   error
}

// claudeMsg carries the Claude Code sessions found on disk.
type claudeMsg struct {
	sessions map[int]claudeSession
}

// rowKind distinguishes the two things the navigator lists.
type rowKind int

const (
	rowProject rowKind = iota
	rowProc
)

// navRow is one selectable line: a repository, or a process inside one.
type navRow struct {
	kind    rowKind
	project Project
	node    *ProcNode
	prefix  string // tree rules of the ancestors already drawn
	last    bool   // last child at its level, so it closes the branch
}

type model struct {
	width  int
	height int

	projects []Project
	err      error

	// procs are the running processes; byRepo groups them under the repository
	// they are working in, already arranged into parent/child trees.
	procs  []Proc
	byRepo map[string][]*ProcNode

	// showAll toggles the navigator between every repository and only those
	// with a process running in them. It starts off: the repositories with
	// something running in them are the ones worth opening scrn to see.
	showAll bool

	// rows is the flattened navigator, rebuilt whenever its inputs change;
	// cursor indexes into it and offset is the first row on screen.
	rows   []navRow
	cursor int
	offset int

	// collapsed holds the subjects whose children are folded away, keyed the
	// same way as details so the state survives a rescan.
	collapsed map[string]bool

	// pendingKill is the kill that has been asked for but not confirmed.
	// Killing cannot be undone, so it takes a second key.
	pendingKill *killRequest

	// dying holds the processes that have been signalled and are still listed.
	// They keep their place, marked, until a rescan finds them gone: a row that
	// vanished on the keystroke would claim an exit scrn has not seen yet.
	// spinning says whether the frame chain is running and frame is its count.
	dying    map[int]dyingProc
	spinning bool
	frame    int

	// status is a one-line report of the last action, cleared as soon as the
	// cursor moves on.
	status    string
	statusErr bool

	// ticks counts refresh cycles, so slower work can run every Nth one.
	ticks int

	// claude holds the Claude Code sessions currently advertised, keyed by pid.
	// It is refreshed with the process list so the navigator can mark the busy
	// instances without the cursor having to visit them.
	claude map[int]claudeSession

	// terms are the shells the daemon is holding, keyed by the pid running
	// each one. A repository can hold as many as you open; they tell themselves
	// apart in the navigator because each is its own process in that
	// repository's tree.
	//
	// The client owns none of them. It learns about them from the daemon, which
	// is what lets them still be here when a window that opened one has gone.
	terms map[int]*remoteTerm

	// daemon is the connection to the process holding the shells, and err says
	// so when there is not one.
	daemon    *session
	daemonErr string

	// wantCursor is a shell just opened, waiting for the scan that will put it
	// in the tree. The cursor moves to it when it lands, so leaving the shell
	// leaves the cursor on the row that shell belongs to.
	wantCursor int

	// focus is the pid of the terminal taking keystrokes, or 0 when the
	// navigator has them. A focused terminal shows in the pane whatever the
	// cursor is on, so that typing never goes somewhere you cannot see.
	focus int

	// details caches inspections by subject key, so revisiting a row is
	// instant and moving quickly through the list does not queue up work.
	details map[string][]field
}

func newModel() model {
	return model{
		collapsed: map[string]bool{},
		details:   map[string][]field{},
		dying:     map[int]dyingProc{},
		terms:     map[int]*remoteTerm{},
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scanProjects, scanProcs, scanClaude, connectDaemon(), tick(procPoll))
}

// scanProjects loads the config and walks the projects directory off the
// render path, so a slow disk cannot delay the first paint.
func scanProjects() tea.Msg {
	cfg, err := loadConfig()
	if err != nil {
		return projectsMsg{err: fmt.Errorf("config: %w", err)}
	}
	projects, err := discoverProjects(expandPath(cfg.ProjectsDir))
	if err != nil {
		return projectsMsg{err: err}
	}
	return projectsMsg{projects: projects}
}

// scanClaude reads what the running Claude Code instances say about
// themselves. Nothing is reported if Claude Code is not installed: the
// navigator simply has nothing extra to say about those processes.
func scanClaude() tea.Msg { return claudeMsg{sessions: claudeSessions()} }

// scanProcs reads the working directory of every visible process.
func scanProcs() tea.Msg {
	procs, err := runningProcs()
	if err != nil {
		return procsMsg{err: fmt.Errorf("processes: %w", err)}
	}
	return procsMsg{procs: procs}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.scrollToCursor()
		// The shells are drawing into the pane, so they are the ones that have
		// been resized, whatever the window did.
		for pid := range m.terms {
			m.daemon.resize(pid, m.detailWidth(), m.bodyHeight())
		}

	case projectsMsg:
		m.projects, m.err = msg.projects, msg.err
		m.rebuild()
		return m, m.detailCmd()

	case procsMsg:
		if msg.err != nil {
			// A failed process scan should not blank out the repo list; the
			// narrowed view just has nothing to show.
			m.procs = nil
		} else {
			m.procs = msg.procs
		}
		m.rebuild()
		return m, m.detailCmd()

	case daemonReadyMsg:
		if msg.err != nil {
			m.daemonErr = msg.err.Error()
			return m, nil
		}
		m.daemon, m.daemonErr = msg.session, ""
		// Ask what is already running: shells from a window that has since
		// been closed are still there, and this is where they come back.
		m.daemon.list()
		return m, nextEvent(m.daemon)

	case daemonLostMsg:
		m.daemon, m.daemonErr = nil, msg.err.Error()
		m.terms, m.focus = map[int]*remoteTerm{}, 0
		return m, nil

	case termOpenedMsg:
		// A shell this window opened takes the keys, and the cursor once the
		// scan puts it in the tree.
		if _, ok := m.terms[msg.pid]; !ok {
			m.terms[msg.pid] = &remoteTerm{pid: msg.pid}
		}
		m.focus, m.wantCursor = msg.pid, msg.pid
		return m, tea.Batch(nextEvent(m.daemon), scanProcs)

	case sessionsMsg:
		// The daemon is the authority on what it holds, so the client takes
		// the list rather than merging into what it thought it knew.
		held := make(map[int]*remoteTerm, len(msg.sessions))
		for _, s := range msg.sessions {
			if was, ok := m.terms[s.PID]; ok {
				held[s.PID] = was
				continue
			}
			held[s.PID] = &remoteTerm{pid: s.PID, dir: s.Dir}
		}
		m.terms = held
		if _, ok := m.terms[m.focus]; !ok {
			m.focus = 0
		}
		m.rebuild()
		return m, tea.Batch(nextEvent(m.daemon), scanProcs)

	case screenMsg:
		t, ok := m.terms[msg.pid]
		if !ok {
			// A screen can arrive for a shell this window has not been told
			// about yet; take it either way.
			t = &remoteTerm{pid: msg.pid}
			m.terms[msg.pid] = t
		}
		t.screen, t.curX, t.curY = msg.screen, msg.curX, msg.curY
		return m, nextEvent(m.daemon)

	case termGoneMsg:
		delete(m.terms, msg.pid)
		if m.focus == msg.pid {
			m.focus = 0
		}
		m.rebuild()
		return m, tea.Batch(nextEvent(m.daemon), scanProcs)

	case claudeMsg:
		m.claude = msg.sessions

	case detailMsg:
		m.details[msg.key] = msg.fields

	case tickMsg:
		m.ticks++
		cmds := []tea.Cmd{scanProcs, scanClaude, tick(procPoll)}
		if m.ticks%projectEvery == 0 {
			cmds = append(cmds, scanProjects)
		}
		// Keep the pane the user is looking at current too; the rest of the
		// cache is refreshed lazily when the cursor reaches it.
		if c := m.refreshDetailCmd(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case killedMsg:
		m.pendingKill = nil
		signalled := 0
		for _, r := range msg.results {
			if r.err != nil {
				continue
			}
			signalled++
			m.dying[r.pid] = dyingProc{command: r.command}
		}
		if signalled == 0 {
			m.status, m.statusErr = "could not kill "+msg.subject+": "+describeFailures(msg.results), true
			return m, nil
		}

		m.status, m.statusErr = "sent SIGTERM to "+msg.subject, false
		if failed := len(msg.results) - signalled; failed > 0 {
			// Part of a subtree going unsignalled is worth saying: the rest
			// spins down and the survivors just sit there unexplained.
			m.status += " — " + strconv.Itoa(failed) + " could not be killed: " + describeFailures(msg.results)
			m.statusErr = true
		}
		if m.spinning {
			return m, nil
		}
		m.spinning = true
		return m, spin()

	case spinMsg:
		m.frame++
		m.ageDying()
		if len(m.dying) == 0 {
			m.spinning = false
			return m, nil
		}
		cmds := []tea.Cmd{spin()}
		if m.frame%rescanFrames == 0 {
			cmds = append(cmds, scanProcs)
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		// A focused shell takes every keystroke except the one that leaves it.
		// That has to come before everything else: ctrl+c belongs to whatever
		// is running in the shell, not to scrn.
		if t := m.focused(); t != nil {
			if msg.Type == tea.KeyCtrlO {
				m.focus = 0
				return m, m.detailCmd()
			}
			m.daemon.input(t.pid, keyBytes(msg))
			return m, nil
		}

		// A pending kill takes the next key, whatever it is: no other binding
		// should fire while a confirmation is on screen.
		if m.pendingKill != nil {
			req := m.pendingKill
			switch msg.String() {
			case "x", "X", "y", "enter":
				m.pendingKill = nil
				return m, killTree(req)
			default:
				m.pendingKill = nil
				m.status, m.statusErr = "kill cancelled", false
				return m, nil
			}
		}

		m.status = ""
		switch msg.String() {
		case "enter":
			return m, m.openShell()
		case "n":
			return m, m.newShell()
		case "x":
			return m, m.askKill(false)
		case "X":
			return m, m.askKill(true)
		case "down", "j", "tab":
			return m, m.move(1)
		case "up", "k", "shift+tab":
			return m, m.move(-1)
		case " ", "space":
			m.toggleCollapse()
			return m, nil
		case "a":
			m.showAll = !m.showAll
			m.rebuild()
			if !m.showAll {
				// Narrowing is a question about right now, so ask again.
				return m, tea.Batch(scanProcs, m.detailCmd())
			}
			return m, m.detailCmd()
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

// move steps the cursor, wrapping at both ends so the list cycles.
func (m *model) move(delta int) tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	m.cursor = (m.cursor + delta + len(m.rows)) % len(m.rows)
	m.scrollToCursor()
	return m.detailCmd()
}

// focused returns the terminal taking keystrokes, if one is.
func (m model) focused() *remoteTerm {
	if m.focus == 0 {
		return nil
	}
	return m.terms[m.focus]
}

// paneTerm is the shell the pane should be showing: the focused one, or the
// one belonging to the row under the cursor.
func (m model) paneTerm() *remoteTerm {
	if t := m.focused(); t != nil {
		return t
	}
	r, ok := m.selected()
	if !ok || r.kind != rowProc {
		return nil
	}
	return m.terms[r.node.PID]
}

// attachable reports whether enter can step into a row: a repository opens a
// shell in itself, and a shell scrn started can be returned to. A process scrn
// did not start is on a terminal it does not own, and no amount of asking will
// change that.
func (m model) attachable(r navRow) bool {
	if r.kind == rowProject {
		return true
	}
	_, mine := m.terms[r.node.PID]
	return mine
}

// newShell starts a shell wherever the cursor is, whatever the cursor is on.
//
// This is the one way to put a process into the tree, so it cannot be reserved
// for the rows that happen to be enterable: standing on a process scrn does not
// own is a perfectly good reason to want a shell where that process is working.
// Attaching to a foreign process is impossible; opening a shell beside it is
// not, and the two are different questions.
func (m *model) newShell() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	if m.daemon == nil {
		m.status, m.statusErr = "no daemon to hold the shell: "+m.daemonErr, true
		return nil
	}
	m.daemon.open(m.shellDir(r), m.detailWidth(), m.bodyHeight())
	return nil
}

// shellDir is where a new shell on this row should start: the repository, or
// the directory the selected process is actually working in — which for a
// build or a test run is often further in than the repository root.
func (m model) shellDir(r navRow) string {
	if r.kind == rowProc && r.node.Dir != "" {
		return r.node.Dir
	}
	return r.project.Path
}

// openShell opens a shell on a repository row, or steps into one already open
// on the row under the cursor. Enter on a repository always opens another, so
// a repository can hold as many shells as the work needs.
func (m *model) openShell() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}

	if r.kind == rowProc {
		if !m.attachable(r) {
			// The row is already drawn dim to say so; this is the reminder for
			// pressing enter on it anyway, not a failure.
			m.status, m.statusErr = "scrn did not start "+procLabel(r.node), false
			return nil
		}
		m.focus = r.node.PID
		// The screen comes from the daemon, which is what makes a shell from
		// an earlier window come back with what it had drawn still on it.
		m.daemon.attach(r.node.PID, m.detailWidth(), m.bodyHeight())
		return nil
	}
	return m.newShell()
}

// askKill arms a kill for whatever the cursor is on. A plain kill takes the
// one selected process; a tree kill takes everything below it too, which for a
// repository row means everything running in that repository.
func (m *model) askKill(tree bool) tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}

	if r.kind == rowProject {
		// A repository is not itself killable, but the tree under it is.
		if !tree {
			m.status, m.statusErr = "select a process to kill, or X for the whole repository", true
			return nil
		}
		var nodes []*ProcNode
		for _, root := range m.byRepo[r.project.Path] {
			nodes = append(nodes, subtree(root)...)
		}
		if len(nodes) == 0 {
			m.status, m.statusErr = "nothing running in "+r.project.Name, true
			return nil
		}
		m.pendingKill = &killRequest{
			subject: plural(len(nodes), "process", "processes") + " in " + r.project.Name,
			nodes:   nodes,
		}
		return nil
	}

	if !tree {
		m.pendingKill = &killRequest{subject: procLabel(r.node), nodes: []*ProcNode{r.node}}
		return nil
	}

	// A leaf has nothing below it, so X on one is just a kill, and saying so
	// would only make the confirmation harder to read.
	nodes := subtree(r.node)
	subject := procLabel(r.node)
	if len(nodes) > 1 {
		subject += " and " + strconv.Itoa(len(nodes)-1) + " under it"
	}
	m.pendingKill = &killRequest{subject: subject, nodes: nodes}
	return nil
}

// describeFailures says why a kill did not land, naming the reasons rather
// than the processes: a subtree fails for the same handful of reasons over and
// over, and "not permitted" said once is the useful report.
func describeFailures(results []killResult) string {
	var reasons []string
	seen := map[string]bool{}
	for _, r := range results {
		if r.err == nil || seen[r.err.Error()] {
			continue
		}
		seen[r.err.Error()] = true
		reasons = append(reasons, r.err.Error())
	}
	sort.Strings(reasons)
	return strings.Join(reasons, ", ")
}

// toggleCollapse folds or unfolds the selected node. A row with nothing under
// it is left alone, so space never appears to do nothing at random.
func (m *model) toggleCollapse() {
	r, ok := m.selected()
	if !ok || m.childCount(r) == 0 {
		return
	}

	key := detailKey(r)
	if m.collapsed[key] {
		delete(m.collapsed, key)
	} else {
		m.collapsed[key] = true
	}

	// The rows below the cursor change, but the cursor keeps its subject.
	m.rows = m.flatten()
	m.scrollToCursor()
}

// childCount is how many processes a row hides when it is collapsed: every
// process in the repository, or every descendant of the process.
func (m model) childCount(r navRow) int {
	if r.kind == rowProject {
		total := 0
		for _, n := range m.byRepo[r.project.Path] {
			total += countTree(n)
		}
		return total
	}
	return countTree(r.node) - 1
}

// ageDying counts the frames each signalled process has lasted and gives up on
// the ones that are not going. Marking a process forever would both misreport
// it and keep rescanning on its behalf for the rest of the session.
func (m *model) ageDying() {
	var stuck []int
	for pid, d := range m.dying {
		d.frames++
		m.dying[pid] = d
		if d.frames > killLinger {
			stuck = append(stuck, pid)
		}
	}
	if len(stuck) == 0 {
		return
	}

	// Sorted, so what the footer says does not depend on map order.
	sort.Ints(stuck)
	names := make([]string, 0, len(stuck))
	for _, pid := range stuck {
		names = append(names, m.dying[pid].command+" "+strconv.Itoa(pid))
		delete(m.dying, pid)
	}
	m.status, m.statusErr = strings.Join(names, ", ")+" did not exit", true
}

// pruneDying forgets the processes that have gone. It reads the process list
// rather than the rows, because a dying process inside a folded subtree has no
// row and is not therefore gone.
func (m *model) pruneDying() {
	if len(m.dying) == 0 {
		return
	}
	live := make(map[int]bool, len(m.procs))
	for _, p := range m.procs {
		live[p.PID] = true
	}
	for pid := range m.dying {
		if !live[pid] {
			delete(m.dying, pid)
		}
	}
}

// bodyHeight is the number of rows between the header and the footer.
func (m model) bodyHeight() int {
	if h := m.height - 2; h > 0 {
		return h
	}
	return 0
}

// scrollToCursor moves the window the least amount that brings the cursor back
// into view, so scrolling follows the cursor instead of recentering on it.
func (m *model) scrollToCursor() {
	h := m.bodyHeight()
	if h == 0 {
		m.offset = 0
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	if max := len(m.rows) - h; m.offset > max {
		m.offset = max
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// rebuild regroups processes by repository and reflattens the navigator,
// keeping the cursor on the same subject where that subject still exists.
func (m *model) rebuild() {
	was := ""
	if r, ok := m.selected(); ok {
		was = detailKey(r)
	}

	m.groupProcs()
	m.rows = m.flatten()

	// Prefer the same subject; failing that — a process that just exited —
	// hold the same position in the list rather than jumping to the top.
	found := -1
	for i, r := range m.rows {
		if detailKey(r) == was {
			found = i
			break
		}
	}
	switch {
	case found >= 0:
		m.cursor = found
	case m.cursor >= len(m.rows):
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}

	// A shell just opened takes the cursor as soon as it is in the tree, so
	// that leaving it leaves the cursor somewhere that makes sense.
	if m.wantCursor != 0 {
		for i, r := range m.rows {
			if r.kind == rowProc && r.node.PID == m.wantCursor {
				m.cursor, m.wantCursor = i, 0
				break
			}
		}
	}

	m.pruneDetails()
	m.pruneDying()
	m.scrollToCursor()
}

// pruneDetails drops cached inspections for rows that are no longer listed, so
// a long session polling every couple of seconds does not accumulate details
// for every process that has ever run.
func (m *model) pruneDetails() {
	if len(m.details) == 0 {
		return
	}
	live := make(map[string]bool, len(m.rows))
	for _, r := range m.rows {
		live[detailKey(r)] = true
	}
	for k := range m.details {
		if !live[k] {
			delete(m.details, k)
		}
	}
}

// groupProcs files running processes under the repository they belong to.
//
// A process is attributed to the innermost repository containing it, so a
// process in a nested checkout is listed there and not under its parent repo.
func (m *model) groupProcs() {
	owner := make(map[string][]Proc, len(m.projects))
	for _, pr := range m.procs {
		best := ""
		for _, p := range m.projects {
			if under(pr.Dir, p.Path) && len(p.Path) > len(best) {
				best = p.Path
			}
		}
		if best != "" {
			owner[best] = append(owner[best], pr)
		}
	}

	m.byRepo = make(map[string][]*ProcNode, len(owner))
	for path, procs := range owner {
		m.byRepo[path] = procForest(procs)
	}
}

// flatten turns the visible repositories and their process trees into the flat
// list of selectable rows the navigator draws and the cursor walks.
func (m model) flatten() []navRow {
	var rows []navRow
	for _, p := range m.visible() {
		row := navRow{kind: rowProject, project: p}
		rows = append(rows, row)
		if m.collapsed[detailKey(row)] {
			continue
		}
		roots := m.byRepo[p.Path]
		for i, n := range roots {
			rows = append(rows, m.flattenProc(p, n, "", i == len(roots)-1)...)
		}
	}
	return rows
}

func (m model) flattenProc(p Project, n *ProcNode, prefix string, last bool) []navRow {
	row := navRow{kind: rowProc, project: p, node: n, prefix: prefix, last: last}
	rows := []navRow{row}
	if m.collapsed[detailKey(row)] {
		return rows
	}

	childPrefix := prefix + "│ "
	if last {
		childPrefix = prefix + "  "
	}
	for i, c := range n.Children {
		rows = append(rows, m.flattenProc(p, c, childPrefix, i == len(n.Children)-1)...)
	}
	return rows
}

// visible returns the repositories the navigator should list.
func (m model) visible() []Project {
	if m.showAll {
		return m.projects
	}
	out := make([]Project, 0, len(m.byRepo))
	for _, p := range m.projects {
		if len(m.byRepo[p.Path]) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// selected returns the row under the cursor.
func (m model) selected() (navRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return navRow{}, false
	}
	return m.rows[m.cursor], true
}

// refreshDetailCmd re-inspects the selected row even though it is cached, so
// what is on screen keeps up with the process it describes. The cached value
// stays until the new one lands, so the pane does not blink through "loading".
func (m model) refreshDetailCmd() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	return loadDetail(r, len(m.byRepo[r.project.Path]), m.claudeFor(r))
}

// claudeFor returns the Claude Code session a row is running, if it is one.
// The command name is checked as well as the session file, because a session
// file can outlive its process and a reused pid would otherwise be dressed up
// as a Claude instance.
func (m model) claudeFor(r navRow) *claudeSession {
	if r.kind != rowProc || r.node.Command != "claude" {
		return nil
	}
	s, ok := m.claude[r.node.PID]
	if !ok {
		return nil
	}
	return &s
}

// detailCmd inspects the selected row unless it has been inspected already.
func (m model) detailCmd() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	if _, cached := m.details[detailKey(r)]; cached {
		return nil
	}
	return loadDetail(r, len(m.byRepo[r.project.Path]), m.claudeFor(r))
}

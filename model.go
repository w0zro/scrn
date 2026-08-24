package main

import (
	"errors"
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

// claudePoll is how often the Claude sessions are re-read. It is far shorter
// than the process poll because it is a different kind of work: a handful of
// small files rather than an lsof sweep of every process on the machine, which
// is some three orders of magnitude apart. Tying the two together made a
// session that had started working wait up to a process poll to say so, which
// is exactly the moment the marker is for.
const claudePoll = 150 * time.Millisecond

// projectEvery is how many process polls pass between repository scans.
// Repositories appear and disappear far more slowly than processes do.
const projectEvery = 15

// claudeTickMsg drives the Claude session refresh, on a chain of its own for
// the same reason it has a rate of its own. Exactly one is in flight.
type claudeTickMsg struct{}

func claudeTick() tea.Cmd {
	return tea.Tick(claudePoll, func(time.Time) tea.Msg { return claudeTickMsg{} })
}

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
//
// A run of processes that never branches is one row, because it is one thing
// happening: a shell that started an editor is the editor, and an editor that
// forked itself is still the editor. node is the process the row is named for,
// the deepest of that run; chain is the top of it, which is what a tree kill
// has to cover. For a row that folded nothing the two are the same.
type navRow struct {
	kind    rowKind
	project Project
	run     []*ProcNode // the whole folded run, oldest first
	node    *ProcNode   // the one in it the row is named for
	prefix  string      // tree rules of the ancestors already drawn
	last    bool        // last child at its level, so it closes the branch
}

// chain is the top of the run, which is what a tree kill has to cover.
func (r navRow) chain() *ProcNode {
	if len(r.run) == 0 {
		return r.node
	}
	return r.run[0]
}

// leaf is the bottom of the run. Anything below it branches, so that is where
// the tree carries on.
func (r navRow) leaf() *ProcNode {
	if len(r.run) == 0 {
		return r.node
	}
	return r.run[len(r.run)-1]
}

type model struct {
	width  int
	height int

	projects []Project
	err      error

	// procs are the running processes; byRepo groups them under the repository
	// they are working in, already arranged into parent/child trees. parent
	// maps a pid to the one that started it, which is how a process is traced
	// back to the shell it is running inside.
	procs  []Proc
	byRepo map[string][]*ProcNode
	parent map[int]int
	nodes  map[int]*ProcNode

	// showHelp spells the keys out. They are worth a line to say they exist
	// and six to list, and the list is worth more to the navigator most of the
	// time than it is to the person reading it.
	showHelp bool

	// unfolded draws every process on a line of its own, including the ones a
	// run would otherwise fold away. The folded view is the reading view; this
	// is for when the shell in the middle is the thing you came to find.
	unfolded bool

	// filter narrows the navigator to the repositories whose name or path
	// matches it, searching every one rather than only those with something
	// running: the point of it is to reach a project you are not working in.
	// typing says the keys are going into the filter rather than at the list.
	filter string
	typing bool

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

	// daemonStale is set when the daemon is older than the build talking to
	// it and is being kept alive by the shells it holds. Those shells have to
	// go for it to be replaced, so that takes asking. pendingReplace is the
	// asking.
	daemonStale    bool
	pendingReplace bool

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
	return tea.Batch(scanProjects, scanProcs, scanClaude, connectDaemon(),
		tick(procPoll), claudeTick())
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
			m.daemon.resize(pid, m.detailWidth(), m.paneHeight())
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

	case replacedMsg:
		if msg.err != nil {
			m.status, m.statusErr = msg.err.Error(), true
			return m, connectDaemon()
		}
		return m, reconnect()

	case reconnectMsg:
		return m, connectDaemon()

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
		// A daemon outlives the window that started it, which is the point of
		// it — and means it also outlives rebuilds. One older than this build
		// is running code this window does not have, and the difference is
		// invisible until something it holds behaves the way it used to.
		m.daemonStale = false
		if stale(msg.since) {
			if len(msg.sessions) == 0 {
				// Holding nothing, so replacing it costs nothing.
				m.daemon.standDown()
				m.status, m.statusErr = "replacing a daemon older than this build", false
				return m, tea.Batch(nextEvent(m.daemon), reconnect())
			}
			m.daemonStale = true
			m.status, m.statusErr = "daemon predates this build; R replaces it, ending its "+
				plural(len(msg.sessions), "shell", "shells"), true
		}

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
		t.title, t.progress = msg.title, msg.progress

		// Only the shell being looked at speaks for the window. Another one
		// finishing a build should not retitle a tab showing something else.
		if m.focus == msg.pid && t.title != "" {
			return m, tea.Batch(nextEvent(m.daemon), tea.SetWindowTitle(oscTitleText(t.title)))
		}
		return m, nextEvent(m.daemon)

	case termGoneMsg:
		delete(m.terms, msg.pid)
		if m.focus == msg.pid {
			m.focus = 0
		}
		m.rebuild()
		// Asking again is what notices a daemon that has just become
		// replaceable: the shell keeping an out-of-date one alive was this.
		m.daemon.list()
		return m, tea.Batch(nextEvent(m.daemon), scanProcs)

	case claudeTickMsg:
		return m, tea.Batch(scanClaude, claudeTick())

	case claudeMsg:
		m.claude = msg.sessions
		// An instance that has started working sets the markers turning.
		if !m.spinning && m.spinNeeded() {
			m.spinning = true
			return m, spin()
		}

	case detailMsg:
		m.details[msg.key] = msg.fields

	case tickMsg:
		m.ticks++
		cmds := []tea.Cmd{scanProcs, tick(procPoll)}
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
			if errors.Is(r.err, errGone) {
				// Not there to signal, which is what was being asked for.
				// Nothing to mark: there is no row left to mark it on.
				signalled++
				continue
			}
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

		m.status, m.statusErr = ended(msg.results)+msg.subject, false
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
		if !m.spinNeeded() {
			m.spinning = false
			return m, nil
		}
		cmds := []tea.Cmd{spin()}
		// Only a kill needs the process list chased; a turning marker is
		// about a session file that the ordinary refresh already re-reads.
		if len(m.dying) > 0 && m.frame%rescanFrames == 0 {
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

		// The filter takes every key while it is being typed, so a repository
		// called "next" can be typed without n opening a shell halfway through.
		if m.typing {
			return m, m.filterKey(msg)
		}

		// Replacing the daemon ends the work it is holding, so it takes a
		// second key like any other kill.
		if m.pendingReplace {
			m.pendingReplace = false
			switch msg.String() {
			case "R", "y", "enter":
				old := m.daemon
				m.terms, m.focus, m.daemonStale = map[int]*remoteTerm{}, 0, false
				m.daemon, m.status, m.statusErr = nil, "replacing the daemon", false
				return m, replaceDaemon(old)
			}
			m.status, m.statusErr = "left the daemon alone", false
			return m, nil
		}

		// A pending kill takes the next key, whatever it is: no other binding
		// should fire while a confirmation is on screen.
		if m.pendingKill != nil {
			req := m.pendingKill
			switch msg.String() {
			case "x", "X", "y", "enter":
				m.pendingKill = nil
				return m, m.runKill(req)
			default:
				m.pendingKill = nil
				m.status, m.statusErr = "kill cancelled", false
				return m, nil
			}
		}

		m.status = ""
		switch msg.String() {
		case "?":
			m.showHelp = !m.showHelp
			m.scrollToCursor()
			return m, nil
		case "R":
			return m, m.askReplace()
		case "/":
			// The list becomes every project straight away, before a single
			// character is typed: half of looking one up is remembering which
			// ones there are.
			m.typing = true
			m.rebuild()
			m.cursor = 0
			m.scrollToCursor()
			return m, m.detailCmd()
		case "enter":
			return m, m.openShell()
		case "n":
			return m, m.start("")
		case "c":
			// A Claude instance scrn owns, so it survives the window and can
			// be stepped back into — unlike the ones it can only watch.
			return m, m.start(claudeCommand)
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
		case "-":
			m.unfolded = !m.unfolded
			m.rebuild()
			return m, m.detailCmd()
		case "a":
			m.showAll = !m.showAll
			m.rebuild()
			if !m.showAll {
				// Narrowing is a question about right now, so ask again.
				return m, tea.Batch(scanProcs, m.detailCmd())
			}
			return m, m.detailCmd()
		case "esc":
			// Whatever is open is what esc is most likely about, and only
			// once nothing is does it mean leave.
			if m.showHelp {
				m.showHelp = false
				m.scrollToCursor()
				return m, nil
			}
			if m.filter != "" {
				m.setFilter("")
				return m, m.detailCmd()
			}
			return m, tea.Quit
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

// filterKey handles a keystroke while the filter is being typed.
func (m *model) filterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEnter:
		// Accepting leaves the filter applied rather than clearing it, so the
		// repository just found does not drop back out of the list before
		// there has been a chance to start anything in it.
		m.typing = false
		return m.detailCmd()
	case tea.KeyEsc:
		m.typing = false
		m.setFilter("")
		return m.detailCmd()
	case tea.KeyBackspace:
		if r := []rune(m.filter); len(r) > 0 {
			m.setFilter(string(r[:len(r)-1]))
		}
		return m.detailCmd()
	case tea.KeyRunes:
		m.setFilter(m.filter + string(msg.Runes))
		return m.detailCmd()
	case tea.KeySpace:
		m.setFilter(m.filter + " ")
		return m.detailCmd()
	case tea.KeyUp:
		return m.move(-1)
	case tea.KeyDown:
		return m.move(1)
	}
	return nil
}

// setFilter narrows the list and starts again from the top, because the rows
// under the cursor are not the ones that were there a keystroke ago.
func (m *model) setFilter(s string) {
	m.filter = s
	m.rebuild()
	m.cursor = 0
	m.scrollToCursor()
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

// attachable reports whether enter takes you somewhere. A repository opens a
// shell in itself, and anything running inside a shell scrn holds can be
// reached by attaching to that shell. Only a process on a terminal scrn does
// not own is out of reach, and no amount of asking will change that.
func (m model) attachable(r navRow) bool {
	if r.kind == rowProject {
		return true
	}
	return m.owningTerm(r.node.PID) != nil
}

// owningTerm is the shell scrn holds that a process is running inside: itself,
// or the nearest ancestor that is one.
//
// A claude started with c is a child of the shell that ran it, so entering the
// claude row means entering that shell — which is where the claude is drawing.
// The walk is bounded because a process table that says a process is its own
// ancestor should not hang the navigator.
func (m model) owningTerm(pid int) *remoteTerm {
	for i := 0; pid > 1 && i <= len(m.procs); i++ {
		if t, ok := m.terms[pid]; ok {
			return t
		}
		pid = m.parent[pid]
	}
	return nil
}

// newShell starts a shell wherever the cursor is, whatever the cursor is on.
//
// This is the one way to put a process into the tree, so it cannot be reserved
// for the rows that happen to be enterable: standing on a process scrn does not
// own is a perfectly good reason to want a shell where that process is working.
// Attaching to a foreign process is impossible; opening a shell beside it is
// not, and the two are different questions.
func (m *model) start(command string) tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	if m.daemon == nil {
		m.status, m.statusErr = "no daemon to hold it: "+m.daemonErr, true
		return nil
	}
	m.daemon.open(m.shellDir(r), command, m.detailWidth(), m.paneHeight())
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
		t := m.owningTerm(r.node.PID)
		if t == nil {
			// The row is already drawn dim to say so; this is the reminder for
			// pressing enter on it anyway, not a failure.
			m.status, m.statusErr = "scrn did not start "+procLabel(r.node), false
			return nil
		}
		m.focus = t.pid
		// Stepping into something is acting on it, so a search that led here
		// is finished. Nothing has to be waited for: the project already holds
		// the shell being entered, so it stays listed without the filter.
		m.setFilter("")
		// The screen comes from the daemon, which is what makes a shell from
		// an earlier window come back with what it had drawn still on it.
		m.daemon.attach(t.pid, m.detailWidth(), m.paneHeight())
		return nil
	}
	return m.start("")
}

// runKill carries out a confirmed kill, splitting it by who owns the target.
//
// A shell scrn holds is hung up through the daemon rather than signalled: an
// interactive shell ignores SIGTERM, so signalling one leaves it sitting there
// and scrn reporting that it would not go. Everything else is somebody else's
// process, and a signal is all scrn has.
func (m *model) runKill(req *killRequest) tea.Cmd {
	var hungUp []killResult
	var signalled []*ProcNode

	for _, n := range req.nodes {
		if _, mine := m.terms[n.PID]; mine {
			m.daemon.closeTerm(n.PID)
			hungUp = append(hungUp, killResult{command: n.Command, pid: n.PID, hungUp: true})
			continue
		}
		signalled = append(signalled, n)
	}
	return killTree(&killRequest{subject: req.subject, nodes: signalled}, hungUp)
}

// shellAround is the shell scrn holds that a process is running inside, if it
// is not that shell itself.
func (m model) shellAround(n *ProcNode) *ProcNode {
	t := m.owningTerm(n.PID)
	if t == nil || t.pid == n.PID {
		return nil
	}
	return m.nodes[t.pid]
}

// askReplace arms the replacement of a daemon older than this build. It is
// only offered when there is one, because ending shells to swap a daemon that
// is already current would be destroying work for nothing.
func (m *model) askReplace() tea.Cmd {
	if !m.daemonStale {
		m.status, m.statusErr = "the daemon is the one this build expects", false
		return nil
	}
	m.pendingReplace = true
	return nil
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
		nodes := []*ProcNode{r.node}
		subject := procLabel(r.node)

		// A process running in a shell scrn holds takes the shell with it.
		// Quitting a Claude instance yourself leaves you at the prompt, which
		// is what the shell is there for; killing it from here means being
		// done with the whole thing, and leaving an empty shell behind would
		// only be something else to tidy up.
		if shell := m.shellAround(r.node); shell != nil {
			nodes = append(nodes, shell)
			subject += " and its shell"
		}
		m.pendingKill = &killRequest{subject: subject, nodes: nodes}
		return nil
	}

	// A tree kill covers the whole run the row stands for, not just the part
	// it is named after: the shell above an editor is part of that editor.
	nodes := subtree(r.chain())
	subject := procLabel(r.node)
	if len(nodes) > 1 {
		subject += " and " + strconv.Itoa(len(nodes)-1) + " under it"
	}
	m.pendingKill = &killRequest{subject: subject, nodes: nodes}
	return nil
}

// ended names what was actually done, because a kill is not one thing: a shell
// scrn holds is hung up and everything else is signalled, and a subtree can be
// both at once.
func ended(results []killResult) string {
	var hungUp, signalled int
	for _, r := range results {
		// A process already gone was not signalled and was not hung up;
		// nothing was done to it, so it names nothing.
		if r.err != nil {
			continue
		}
		if r.hungUp {
			hungUp++
			continue
		}
		signalled++
	}
	switch {
	case signalled == 0:
		return "closed "
	case hungUp == 0:
		return "sent SIGTERM to "
	default:
		return "ended "
	}
}

// describeFailures says why a kill did not land, naming the reasons rather
// than the processes: a subtree fails for the same handful of reasons over and
// over, and "not permitted" said once is the useful report.
func describeFailures(results []killResult) string {
	var reasons []string
	seen := map[string]bool{}
	for _, r := range results {
		if r.err == nil || errors.Is(r.err, errGone) || seen[r.err.Error()] {
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
	return countTree(r.leaf()) - 1
}

// spinNeeded reports whether anything on screen is moving: a process on its
// way out, or a Claude instance at work.
func (m model) spinNeeded() bool {
	if len(m.dying) > 0 {
		return true
	}
	for _, r := range m.rows {
		if s := m.claudeFor(r); s != nil && s.Status == busyStatus {
			return true
		}
	}
	return false
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

// bodyHeight is the number of navigator rows that fit between scrn's name and
// its keys, which is what the cursor scrolls within.
func (m model) bodyHeight() int {
	if h := m.height - 1 - len(m.hintLines(m.hintWidth(), m.height)); h > 0 {
		return h
	}
	return 0
}

// paneHeight is the room the attached process has, which is the whole window:
// scrn's own rows are in its column, not across the top and bottom.
func (m model) paneHeight() int {
	if m.height > 0 {
		return m.height
	}
	return 1
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

	// A shell that was just started has landed. The filter has done its job:
	// the project holds work now, so it stays in the list on its own merit and
	// the search that found it can go. Clearing here rather than when the key
	// was pressed is what stops the project blinking out and back while the
	// scan catches up.
	if m.wantCursor != 0 && m.running(m.wantCursor) {
		m.filter = ""
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
			// The shell may have folded into whatever it started, so the row
			// to land on is the one whose run begins with it.
			if r.kind == rowProc && r.holds(m.wantCursor) {
				m.cursor, m.wantCursor = i, 0
				break
			}
		}
	}

	m.pruneDetails()
	m.pruneDying()
	m.scrollToCursor()
}

// running reports whether a pid is in the process list as it now stands.
func (m model) running(pid int) bool {
	for _, p := range m.procs {
		if p.PID == pid {
			return true
		}
	}
	return false
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
	m.parent = make(map[int]int, len(m.procs))
	for _, pr := range m.procs {
		m.parent[pr.PID] = pr.PPID
	}

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
	m.nodes = make(map[int]*ProcNode, len(m.procs))
	for path, procs := range owner {
		m.byRepo[path] = procForest(procs)
		for _, root := range m.byRepo[path] {
			indexNodes(root, m.nodes)
		}
	}
}

// flatten turns the visible repositories and their process trees into the flat
// list of selectable rows the navigator draws and the cursor walks.
func (m model) flatten() []navRow {
	var rows []navRow
	for _, p := range m.visible() {
		row := navRow{kind: rowProject, project: p}
		rows = append(rows, row)
		// While a project is being looked up the list is of projects. What is
		// running inside them is not what is being chosen between, and it
		// would bury the names being scanned for.
		if m.typing || m.collapsed[detailKey(row)] {
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
	// Walk down while there is nothing to choose between. The bound is for a
	// process table that says a process started itself.
	run := []*ProcNode{n}
	for i := 0; !m.unfolded && len(n.Children) == 1 && i < len(m.procs); i++ {
		n = n.Children[0]
		run = append(run, n)
	}

	row := navRow{kind: rowProc, project: p, run: run, node: nameOf(run), prefix: prefix, last: last}
	rows := []navRow{row}
	if m.collapsed[detailKey(row)] {
		return rows
	}

	childPrefix := prefix + "│ "
	if last {
		childPrefix = prefix + "  "
	}
	for i, c := range row.leaf().Children {
		rows = append(rows, m.flattenProc(p, c, childPrefix, i == len(row.leaf().Children)-1)...)
	}
	return rows
}

// nameOf picks the process a run is named for: the first in it that is not a
// shell.
//
// The deepest is the wrong answer. A shell that started a claude that started
// a caffeinate is a claude, not a caffeinate — the last process in a run is
// as often something the interesting one reached for as it is the point of
// the run. It also moves: naming a run after its deepest process renames the
// row every time the process that matters starts a tool and finishes with it.
//
// A run that is shells all the way down is named for the last of them, which
// is the shell you would be typing into.
func nameOf(run []*ProcNode) *ProcNode {
	for _, n := range run {
		if !isShell(n.Command) {
			return n
		}
	}
	return run[len(run)-1]
}

// shells are the commands that stand in front of what was actually run.
var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true,
	"dash": true, "ksh": true, "csh": true, "tcsh": true,
	"login": true,
}

func isShell(command string) bool { return shells[strings.TrimPrefix(command, "-")] }

// visible returns the repositories the navigator should list.
func (m model) visible() []Project {
	if m.typing || m.filter != "" {
		// Every repository is searched, not just the ones already listed: a
		// filter is for reaching the projects that are not on screen. An empty
		// filter matches all of them, which is the list you get on pressing /.
		var out []Project
		for _, p := range m.projects {
			if matchesFilter(p, m.filter) {
				out = append(out, p)
			}
		}
		return out
	}
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

// matchesFilter reports whether a repository answers to what has been typed.
// The path is searched as well as the name, so a directory that is only in the
// name of a repository's parent still finds it.
func matchesFilter(p Project, filter string) bool {
	f := strings.ToLower(strings.TrimSpace(filter))
	if f == "" {
		return true
	}
	return strings.Contains(strings.ToLower(p.Name), f) ||
		strings.Contains(strings.ToLower(p.Path), f)
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

// holds reports whether a pid is anywhere in the run this row stands for.
func (r navRow) holds(pid int) bool {
	for _, n := range r.run {
		if n.PID == pid {
			return true
		}
	}
	return false
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

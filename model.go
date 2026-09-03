package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// navWidth is the column the navigator occupies, divider excluded. The
// default suits short home-project names; the config widens it for the long
// qualified paths a work checkout produces.
var navWidth = 30

// applyNavWidth sets the navigator's width from the config, within reason:
// below 16 columns no name survives, and past 60 the navigator is most of
// any screen. Zero — unset — leaves the default standing.
func applyNavWidth(w int) {
	if w > 0 {
		navWidth = min(max(w, 16), 60)
	}
}

// paneMin is the narrowest detail pane worth drawing beside the navigator;
// with less room than this the navigator takes the whole width. It measures
// the pane rather than the window, because the navigator's width is the
// user's to set: a wide navigator in a narrow window leaves the pane no
// room at all, and a pane of no room must not be drawn — negative widths
// walk straight into the renderer.
const paneMin = 31

// procPoll is how often the process list is refreshed. Processes come and go
// constantly, and an lsof sweep is cheap enough to repeat at this rate.
const procPoll = 2 * time.Second

// The agent poll lives in agent.go with the rest of the seam. It is far
// shorter than the process poll because it is a different kind of work: a
// handful of small files rather than an lsof sweep of every process on the
// machine, which is some three orders of magnitude apart. Tying the two
// together made a session that had started working wait up to a process poll
// to say so, which is exactly the moment the marker is for.

// projectEvery is how many process polls pass between repository scans.
// Repositories appear and disappear far more slowly than processes do.
const projectEvery = 15

// repoDetailEvery is how many process polls pass between refreshes of a
// selected repository's details. Those are half a dozen git spawns — git
// status among them, which in a large work checkout is real work — and what
// they report changes at the speed of a person committing, not of a process
// list. A process row keeps the every-poll rate: cpu, memory and ports are
// exactly the numbers that move.
const repoDetailEvery = 5

// tickMsg drives the refresh loop. Exactly one tick is ever in flight: each
// one schedules its successor, so a one-off rescan — after a kill, say — can
// never start a second chain that doubles the polling rate.
type tickMsg struct{}

// tick schedules the next refresh.
func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

// projectsMsg carries the result of the startup scan: the repositories, the
// groups that hold them, and the sub-projects found inside each repository,
// keyed by the repository's path.
type projectsMsg struct {
	projects []Project
	groups   []Project
	subs     map[string][]Project
	err      error
}

// procsMsg carries the running processes. It arrives at startup and again
// whenever the view is narrowed, so the narrowed list reflects what is running
// now rather than at launch.
type procsMsg struct {
	procs []Proc
	err   error
}

// rowKind distinguishes the two things the navigator lists.
type rowKind int

const (
	rowGroup   rowKind = iota // a folder of repositories that make one project
	rowProject                // a repository
	rowSub                    // a sub-project: a directory inside a repository with a manifest
	rowProc                   // a process
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

	// procs are the running processes; byPlace groups them under the place
	// they are working in — a repository, or a sub-project inside one — each
	// group already arranged into parent/child trees. parent maps a pid to
	// the one that started it, which is how a process is traced back to the
	// shell it is running inside.
	procs   []Proc
	byPlace map[string][]*ProcNode
	parent  map[int]int
	nodes   map[int]*ProcNode

	// subs are each repository's sub-projects, keyed by the repository's
	// path. They come from the repository scan, not the process scan: a
	// sub-project exists whether or not anything is running in it, which is
	// what lets the filter reach it cold.
	subs map[string][]Project

	// groups are the folders grouping repositories into one project, and
	// grouped files each group's repositories under its path. A project is
	// often several repositories in one directory, worked on at that level.
	groups  []Project
	grouped map[string][]Project

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

	// filterFrom is where the look began: the subject under the cursor.
	// Abandoning the filter with esc puts it back — acting on a result does
	// not, because acting is the point of having looked.
	filterFrom string

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

	// pendingG is a g waiting for the g that makes it mean the top.
	pendingG bool

	// resume is the picker over a place's suspended conversations, and nil
	// while it is closed. Open, it has the pane and the keys.
	resume *resumeView

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

	// blurred says the keys are in another pane, so nothing here should be
	// waiting on the next one.
	blurred bool

	// ticks counts refresh cycles, so slower work can run every Nth one.
	ticks int

	// scanning says a process scan is out, so another is never started behind
	// it: on a machine where lsof stalls, a poll that kept asking would pile a
	// stalled scan on top of every tick. rescan says one was wanted while one
	// was out — asked for by an event, not the poll — and is owed the moment
	// the answer lands, because that answer predates the event it was about.
	scanning bool
	rescan   bool

	// agents holds every live agent instance currently advertised, of every
	// kind, keyed by pid. It is refreshed on its own fast poll so the
	// navigator can mark the working and the waiting without the cursor
	// having to visit them.
	agents map[int]agent

	// worked is every agent pid that has been seen working. Waiting means a
	// finished turn — busy once, idle now — and an instance idle since it
	// was started has not finished anything and is not owed an answer.
	worked map[int]bool

	// terms are the shells the server is holding, keyed by the pid running
	// each one. A repository can hold as many as you open; they tell themselves
	// apart in the navigator because each is its own process in that
	// repository's tree. The navigator owns none of them: tmux holds them,
	// draws the one under the cursor in the pane beside the navigator, and
	// takes the keys to it.
	terms map[int]*remoteTerm

	// server is the connection to the tmux server holding the shells, and
	// err says so when there is not one.
	server    *session
	serverErr string

	// backoff is the wait before the next attempt to reach a server that went
	// away or could not be reached, doubled per consecutive failure and reset
	// once a server is talking.
	backoff time.Duration

	// pendingReplace is R waiting on its confirmation: ending the server
	// ends the work it holds, so it takes a second key like any other kill.
	pendingReplace bool

	// wantProject is a project whose processes were just started, holding the
	// cursor until they are in the tree.
	wantProject string

	// wantCursor is a shell just opened, waiting for the scan that will put it
	// in the tree. The cursor moves to it when it lands, so leaving the shell
	// leaves the cursor on the row that shell belongs to.
	wantCursor int

	// previewing is the shell the pane beside the navigator was last asked
	// to hold — the held shell under the cursor — and zero when the
	// navigator was asked for the window to itself. The pane is only asked
	// again when this changes.
	previewing int

	// previewKey is the row the pane was last arranged for. The pane
	// follows the cursor, so it is only rearranged when the cursor is on a
	// different row than it was — the world changing under a cursor that
	// has not moved leaves the pane alone. That is what lets a shell with
	// no row of its own, shown by J or a chord, stay shown until the cursor
	// moves on.
	previewKey string

	// synced says the first list from this connection has been read: the
	// one that tells a navigator starting beside a shell already shown
	// which shell that is, so it can begin on that row rather than move
	// the shell aside.
	synced bool

	// dressed is the name each shell's pane last wore, so tmux is only
	// told what changed.
	dressed map[int]string

	// said is the mode and message the status line last read, for the
	// same reason.
	said statusText

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
		worked:    map[int]bool{},
		dressed:   map[int]string{},
		// Init sends the first scan, and Init cannot write here to say so.
		scanning: true,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scanProjects, scanProcs, scanAgents, connectServer(),
		tick(procPoll), agentTick())
}

// scanProjects loads the config and walks the projects directory off the
// render path, so a slow disk cannot delay the first paint.
func scanProjects() tea.Msg {
	cfg, err := loadConfig()
	if err != nil {
		return projectsMsg{err: fmt.Errorf("config: %w", err)}
	}
	projects, groups, err := discoverAll(cfg.roots(), cfg.skipSet())
	if err != nil {
		return projectsMsg{err: err}
	}

	// Each repository's index is asked for its sub-projects, together rather
	// than in turn: fifty repositories at twenty milliseconds each would
	// otherwise hold the first paint for a second.
	subs := make(map[string][]Project, len(projects))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range projects {
		wg.Go(func() {
			if found := subProjects(p.Path); len(found) > 0 {
				mu.Lock()
				subs[p.Path] = found
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return projectsMsg{projects: projects, groups: groups, subs: subs}
}

// scanProcs reads the working directory of every visible process.
func scanProcs() tea.Msg {
	procs, err := runningProcs()
	if err != nil {
		return procsMsg{err: fmt.Errorf("processes: %w", err)}
	}
	return procsMsg{procs: procs}
}

// scanNow asks for a process scan on behalf of something that just happened —
// a shell opened, the view narrowed. A scan already out began before the
// event, so its answer cannot carry it; another is owed as soon as it lands.
func (m *model) scanNow() tea.Cmd {
	if m.scanning {
		m.rescan = true
		return nil
	}
	m.scanning = true
	return scanProcs
}

// scanPoll is the tick's ask: freshness only, so a scan already out is answer
// enough and nothing is owed.
func (m *model) scanPoll() tea.Cmd {
	if m.scanning {
		return nil
	}
	return m.scanNow()
}

// Update handles one message and then tells the status line what the
// navigator now has to say, whatever the message was: nearly anything can
// change it — a key, a report, a scan — and it is written once, from here,
// only when it changed.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	next.dressStatus()
	return next, cmd
}

func (m model) update(msg tea.Msg) (model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.FocusMsg:
		m.blurred = false

	case tea.BlurMsg:
		// The keys have gone to a shell. What was pending here was about
		// the next key, and the next key is not coming: a kill left armed
		// would fire on the first letter typed back into the list.
		m.blurred = true
		m.pendingKill, m.pendingReplace, m.pendingG = nil, false, false

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.keepColumn()
		m.scrollToCursor()

	case projectsMsg:
		m.projects, m.groups, m.subs, m.err = msg.projects, msg.groups, msg.subs, msg.err
		m.rebuild()
		return m, m.detailCmd()

	case procsMsg:
		m.scanning = false
		var owed tea.Cmd
		if m.rescan {
			m.rescan = false
			owed = m.scanNow()
		}
		if msg.err != nil {
			// A failed scan says nothing about what is running, so the last
			// list that succeeded stands: blanking the tree on every hiccup
			// of a loaded machine would be flicker, not information. The
			// failure is reported rather than shown as an empty machine.
			m.status, m.statusErr = msg.err.Error(), true
		} else {
			m.procs = msg.procs
		}
		m.rebuild()
		return m, tea.Batch(m.detailCmd(), owed)

	case reconnectMsg:
		return m, connectServer()

	case serverReadyMsg:
		if msg.err != nil {
			// Not reaching the server is not the end of it: nothing but a retry
			// will ever turn this window back into a useful one.
			m.serverErr = msg.err.Error()
			return m, m.retryConnect()
		}
		m.server, m.serverErr = msg.session, ""
		// A fresh connection knows nothing of the arrangement the last one
		// made; the server says what it holds, and the pane follows from
		// there.
		m.previewing, m.synced = 0, false
		m.said = statusText{}
		// Ask what is already running: shells from a window that has since
		// been closed are still there, and this is where they come back.
		m.server.list()
		return m, nextEvent(m.server)

	case serverErrorMsg:
		// One ask failed; the server and its shells are fine. Say what it said
		// and carry on listening.
		m.status, m.statusErr = msg.err.Error(), true
		return m, nextEvent(m.server)

	case serverLostMsg:
		// The server hung this window up — the last shell closed and the
		// session went with it, or something ended the server outright. The
		// bridge keeps watching for a new one on its own; here the window
		// only stops showing shells that are no longer held.
		m.terms = map[int]*remoteTerm{}
		m.dressed = map[int]string{}
		if msg.err != nil {
			m.status, m.statusErr = msg.err.Error(), true
		}
		m.rebuild()
		return m, nextEvent(m.server)

	case termOpenedMsg:
		if _, ok := m.terms[msg.pid]; !ok {
			m.terms[msg.pid] = &remoteTerm{pid: msg.pid, dir: msg.dir, name: msg.name}
		}
		// A shell asked for by name is one of several a project needed, and
		// none of them is more the one you meant than the others. Only a shell
		// opened on its own is shown, with the keys in it and the cursor on it.
		if msg.name == "" {
			m.showPID(msg.pid)
		}
		return m, tea.Batch(nextEvent(m.server), m.scanNow())

	case sessionsMsg:
		// The server is talking, so whatever chase was on is over.
		m.backoff = 0
		// The server is the authority on what it holds, so the client takes
		// the list rather than merging into what it thought it knew.
		held := make(map[int]*remoteTerm, len(msg.sessions))
		shown, wanted := 0, 0
		for _, s := range msg.sessions {
			if s.Shown {
				shown = s.PID
			}
			if s.Wanted {
				wanted = s.PID
			}
			if was, ok := m.terms[s.PID]; ok {
				held[s.PID] = was
				continue
			}
			held[s.PID] = &remoteTerm{pid: s.PID, dir: s.Dir, name: s.Name}
		}
		m.terms = held
		// A navigator starting beside a shell already shown — the last
		// navigator closed, or the server was found holding shells —
		// begins on that shell's row rather than moving it aside for
		// whatever row the cursor happened to start on.
		if !m.synced {
			m.synced = true
			if shown != 0 {
				m.previewing, m.wantCursor = shown, shown
				m.keepColumn()
			}
		}
		m.rebuild()
		// A shell a chord opened to be shown is shown the way one opened
		// here is: the keys go to it and the cursor follows.
		if wanted != 0 {
			m.showPID(wanted)
		}
		// The server may have just said what it holds while the cursor was
		// already standing on one of those shells; the pane should not wait
		// for the cursor to move before showing it.
		m.syncPreview()
		return m, tea.Batch(nextEvent(m.server), m.scanNow())

	case termGoneMsg:
		delete(m.terms, msg.pid)
		delete(m.dressed, msg.pid)
		if m.previewing == msg.pid {
			// Its pane went with it, and the navigator has the window.
			m.previewing = 0
		}
		m.rebuild()
		m.syncPreview()
		// Asking again is what notices a server that has just become
		// replaceable: the shell keeping an out-of-date one alive was this.
		m.server.list()
		return m, tea.Batch(nextEvent(m.server), m.scanNow())

	case agentTickMsg:
		return m, tea.Batch(scanAgents, agentTick())

	case agentsMsg:
		m.agents = msg.agents
		// The transitions are remembered here, because the scan is a snapshot
		// and cannot know them: an agent seen working that is idle now has
		// finished a turn. A pid that has left the table is forgotten, so a
		// recycled number does not inherit the old process's history.
		for pid, a := range msg.agents {
			if a.working() {
				m.worked[pid] = true
			}
		}
		for pid := range m.worked {
			if _, ok := msg.agents[pid]; !ok {
				delete(m.worked, pid)
			}
		}
		// The marks changed without the tree changing; the windows wear
		// the new ones.
		m.dressWindows()
		// An instance that has started working sets the markers turning.
		if !m.spinning && m.spinNeeded() {
			m.spinning = true
			return m, spin()
		}

	case detailMsg:
		m.details[msg.key] = msg.fields

	case convosMsg:
		// Only the picker that asked wants this; one opened on another place
		// since — or closed — has moved past the answer.
		if m.resume != nil && m.resume.place.Path == msg.place {
			m.resume.loaded = true
			m.resume.convos = msg.convos
		}

	case tickMsg:
		m.ticks++
		cmds := []tea.Cmd{m.scanPoll(), tick(procPoll)}
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
			cmds = append(cmds, m.scanPoll())
		}
		return m, tea.Batch(cmds...)

	case tea.PasteMsg:
		// Into the picker it is more of the query, the way it is for the
		// filter: pasting a phrase from a transcript is a fine way to look.
		if m.resume != nil {
			m.setResumeQuery(m.resume.query + msg.Content)
			return m, nil
		}
		// Into the filter it is just more of the query.
		if m.typing {
			m.status = ""
			m.setFilter(m.filter + msg.Content)
			return m, m.detailCmd()
		}
		// At the navigator, pasted text lands nowhere — and saying so beats
		// a paste that silently vanishes and reads as broken.
		m.status, m.statusErr = "nothing here to paste into", false
		return m, nil

	case tea.KeyPressMsg:
		// A pending kill takes the next key, whatever it is — before even
		// the prefix, or the warning lies: routed later, a prefix excursion
		// could carry the armed kill along for minutes and hand it to an
		// enter meant to open something.
		if m.pendingKill != nil {
			req := m.pendingKill
			m.pendingKill = nil
			switch msg.String() {
			case "x", "X", "y", "enter":
				return m, m.runKill(req)
			default:
				m.status, m.statusErr = "kill cancelled", false
				return m, nil
			}
		}

		// The resume picker takes every key while it is open: it is a look
		// through what could be continued, and its keys are the filter's.
		if m.resume != nil {
			return m, m.resumeKey(msg)
		}

		// The filter takes every key while it is being typed, so a repository
		// called "scrn" can be typed without s opening a shell halfway through.
		if m.typing {
			return m, m.filterKey(msg)
		}

		// Ending the server ends the work it is holding, so it takes a
		// second key like any other kill.
		if m.pendingReplace {
			m.pendingReplace = false
			switch msg.String() {
			case "R", "y", "enter":
				// The bridge notices the server going and says so; clearing
				// here as well just spares the window a beat of stale rows.
				m.server.replace()
				m.terms = map[int]*remoteTerm{}
				m.status, m.statusErr = "ending the server and its shells", false
				m.rebuild()
				return m, nil
			}
			m.status, m.statusErr = "left the server alone", false
			return m, nil
		}

		// gg is a pair, so the first g waits for the second. Anything else
		// cancels it and is swallowed, rather than being acted on as though
		// the g had not been typed.
		if m.pendingG {
			m.pendingG = false
			if msg.String() == "g" {
				return m, m.jump(0)
			}
			return m, nil
		}

		m.status = ""
		switch msg.String() {
		case "?":
			// The keys, in a popup over the whole window: tmux draws it,
			// and the next keystroke puts it away.
			m.server.help()
			return m, nil
		case "R":
			return m, m.askReplace()
		case "/":
			return m, m.openFilter()
		case "enter":
			return m, m.openShell()
		case "r":
			return m, m.run()
		case "s":
			return m, m.start("")
		case "a":
			// An agent scrn owns, so it survives the window and can be
			// stepped back into, unlike the ones it can only watch. Which
			// kind is the config's call; claude is the default.
			return m, m.start(startAgent())
		case "A":
			// The same verb, reaching back: a starts a fresh conversation,
			// A picks a suspended one back up.
			return m, m.openResume()
		case "x":
			return m, m.askKill(false)
		case "X":
			return m, m.askKill(true)
		case "g":
			m.pendingG = true
			return m, nil
		case "G":
			return m, m.jump(len(m.rows) - 1)
		case "down", "j":
			return m, m.move(1)
		case "up", "k":
			return m, m.move(-1)
		case "J":
			return m, m.stepShell(1)
		case "K":
			return m, m.stepShell(-1)
		case "tab":
			// The next agent waiting on you, and again around them in
			// turn: the summons the chord ctrl-space enter delivers from
			// any shell, at the list.
			return m, m.jumpWaiting()
		case "space":
			m.toggleCollapse()
			return m, nil
		case "-":
			m.unfolded = !m.unfolded
			m.rebuild()
			return m, m.detailCmd()
		case ".":
			// Dot shows the hidden, the way it does in a home directory.
			m.showAll = !m.showAll
			m.rebuild()
			if !m.showAll {
				// Narrowing is a question about right now, so ask again.
				return m, tea.Batch(m.scanNow(), m.detailCmd())
			}
			return m, m.detailCmd()
		case "esc":
			// Esc closes whatever is open — the filter here, and the modal
			// and the transcript where they take the keys — and it never
			// closes scrn. Leaving is q's word alone: one reflexive esc too
			// many, a beat after the filter it was meant for has already
			// gone, must not take the window with it.
			if m.filter != "" {
				m.setFilter("")
				return m, m.detailCmd()
			}
			return m, nil
		case "q", "ctrl+c":
			// Leaving the window, not the shells: the client detaches and
			// the navigator keeps its place in the home window for the
			// next `scrn`.
			m.server.leave()
			return m, nil
		}
	}
	return m, nil
}

// filterKey handles a keystroke while something is being looked up.
//
// Looking something up is a way of getting somewhere, so the keys that get
// you somewhere work while you are still typing: the list is narrowing under
// a cursor you can move, and enter, ctrl+r, ctrl+a or ctrl+x acts on
// whatever that cursor is on — a place, or a process that answered. Having
// to accept the filter first made finding a thing and doing something with
// it two separate acts, when it is one.
func (m *model) filterKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		// Enter is the one key that means both things. On a repository or a
		// sub-project it opens a shell there, which is the point of having
		// looked it up; on a process it steps into the shell holding it, the
		// way it does in the list. The filter is finished either way.
		m.typing = false
		if _, ok := m.selected(); ok {
			return m.openShell()
		}
		return m.detailCmd()

	// Moving through what is left, without leaving the typing.
	case "up", "ctrl+p":
		return m.move(-1)
	case "down", "ctrl+n":
		return m.move(1)

	case "esc":
		// Abandoning the look is not acting on anything, so it puts the
		// cursor back on the row it left.
		m.typing = false
		m.setFilter("")
		m.selectKey(m.filterFrom)
		return m.detailCmd()
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.setFilter(string(r[:len(r)-1]))
		}
		return m.detailCmd()
	case "ctrl+u":
		// The query is a line being typed, and ctrl+u is what clears a line
		// everywhere else a line is typed. The typing itself goes on.
		m.setFilter("")
		return m.detailCmd()
	case "tab":
		// The summons reaches through the look: the waiting agent lives in
		// the whole list, and going to it is the end of looking.
		return m.jumpWaiting()
	case "space":
		m.setFilter(m.filter + " ")
		return m.detailCmd()

	// The chords mean what their letters mean. Starting what a project needs
	// is the end of looking for it, so the search closes and leaves the cursor
	// on the project — which is where you would want to be watching it come
	// up, and where the keys mean what they usually mean again.
	case "ctrl+r":
		// Starting what a project needs is the end of looking for it, so the
		// typing stops. The filter itself is held until the processes land,
		// the same way it is for a shell: dropping it now would take the
		// project out of the narrowed list until the scan caught up, and the
		// cursor with it.
		m.typing = false
		return m.run()
	case "ctrl+a":
		// The end of looking, the same as ctrl+r: left set, the typing
		// outlives the filter and quietly takes the keys back the moment
		// the shell it opened is gone.
		m.typing = false
		return m.start(startAgent())
	case "ctrl+x":
		// Killing what you found is also the end of looking for it. The
		// typing stops so the confirmation's key is a confirmation, and the
		// filter holds so the subject stays where the cursor has it.
		m.typing = false
		return m.askKill(false)
	}

	// A letter is a letter: a project called "scrn" has to be typeable
	// without s doing something. The actions are on the chords, which no
	// name contains.
	if msg.Text != "" {
		m.status = "" // whatever was reported was about the last project
		m.setFilter(m.filter + msg.Text)
		return m.detailCmd()
	}
	return nil
}

// selectKey puts the cursor back on a remembered subject, where it is still
// listed. A subject that has gone leaves the cursor where the rebuild put
// it, which held the position rather than jumping to the top.
func (m *model) selectKey(key string) {
	if key == "" {
		return
	}
	for i, r := range m.rows {
		if detailKey(r) == key {
			m.cursor = i
			m.scrollToCursor()
			return
		}
	}
}

// selectProject puts the cursor on a repository, wherever it has ended up in
// the list. It is how an action that closes the search leaves you looking at
// what you acted on rather than back at the top of everything.
func (m *model) selectProject(path string) {
	for i, r := range m.rows {
		if r.kind != rowProc && r.project.Path == path {
			m.cursor = i
			m.scrollToCursor()
			return
		}
	}
}

// setFilter narrows the list and starts again from the top, because the rows
// under the cursor are not the ones that were there a keystroke ago.
//
// Unless they are. A filter is trimmed and folded before anything is matched
// against it, so a space does not narrow anything — and typing one in the
// middle of "vim pro" sent the selection back to the top of a list that had
// not moved, which from the typist's side is the cursor jumping for no reason
// at all. When the rows cannot have changed, neither does the cursor.
func (m *model) setFilter(s string) {
	narrowed := !strings.EqualFold(strings.TrimSpace(m.filter), strings.TrimSpace(s))

	m.filter = s
	// rebuild keeps the cursor on the subject it was on where that subject is
	// still listed, which is the whole of what is wanted when nothing changed.
	m.rebuild()
	if narrowed {
		m.cursor = m.firstAnswer()
	}
	m.scrollToCursor()
}

// firstAnswer is the row the narrowed cursor should land on: the first one
// that answers the filter by what it itself is — its name, its path, its
// command — rather than by standing above the answer. A repository listed
// for its child's sake is scaffolding, and the look should land on what was
// found.
func (m model) firstAnswer() int {
	f := strings.ToLower(strings.TrimSpace(m.filter))
	if f == "" {
		return 0
	}
	for i, r := range m.rows {
		if rowAnswers(r, f) {
			return i
		}
	}
	return 0
}

// rowAnswers reports whether a row itself answers the filter: a process by
// any command in its run, a place by its name or path.
func rowAnswers(r navRow, f string) bool {
	if r.kind == rowProc {
		for _, n := range r.run {
			if answers(f, n.Command) {
				return true
			}
		}
		return false
	}
	return matchesFilter(r.project, f)
}

// jump puts the cursor on a row and brings it into view. Out of range means
// the nearest end, so the top of an empty list is not a special case.
func (m *model) jump(i int) tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	switch {
	case i < 0:
		i = 0
	case i >= len(m.rows):
		i = len(m.rows) - 1
	}
	m.cursor = i
	m.scrollToCursor()
	return m.detailCmd()
}

// jumpWaiting goes to the next agent waiting on its user, in row order from
// the cursor, wrapping. Going to an agent scrn holds means taking the client
// to its window; one it can only watch gets the cursor instead, which is as
// far as enter could take it either.
func (m *model) jumpWaiting() tea.Cmd {
	// From the filter, the jump is the end of looking: the rows while typing
	// are the query's answers — places alone until a query lands — and the
	// waiting agent lives in the whole list.
	if m.typing {
		m.typing = false
		m.setFilter("")
	}
	for step := 1; step <= len(m.rows); step++ {
		i := (m.cursor + step) % len(m.rows)
		r := m.rows[i]
		if m.awaiting(r) == nil {
			continue
		}
		if t := m.owningTerm(r.node.PID); t != nil {
			m.show(t)
			return nil
		}
		m.cursor = i
		m.scrollToCursor()
		return m.detailCmd()
	}
	m.status, m.statusErr = "no agent is waiting", false
	return nil
}

// show takes the keys to a shell from anywhere: the cursor goes to its row,
// a filter that led here is finished, like enter's, and the shell is shown
// beside the navigator with the keys in it.
func (m *model) show(t *remoteTerm) {
	m.typing = false
	m.resume = nil
	m.setFilter("")
	for i, r := range m.rows {
		if r.kind == rowProc && m.owningTerm(r.node.PID) == t {
			m.cursor = i
			m.scrollToCursor()
			break
		}
	}
	m.previewing, m.previewKey = t.pid, m.cursorKey()
	m.keepColumn()
	m.server.show(t.pid)
}

// keepColumn holds the navigator to its column while a shell is shown
// beside it. The pane's size reaches the navigator late, as a message
// behind the resize: a frame drawn between a shell joining and that
// message is drawn at the whole window's width into a pane a column
// wide, and tmux writes the overflow wrapped into the column — the
// details, shifted, where the list should be, for a frame. The navigator
// knows when a shell is shown, so it does not wait to be told it is
// narrow: it narrows itself as it asks for the shell, and a size wider
// than its column while one is shown is a size from before the join.
func (m *model) keepColumn() {
	if m.previewing != 0 {
		m.width = min(m.width, navWidth)
	}
}

// showPID shows a shell that may not have a row yet — one just opened,
// before the process scan has seen it. The keys go to it now; the cursor
// follows as soon as its row lands.
func (m *model) showPID(pid int) {
	m.previewing, m.previewKey = pid, m.cursorKey()
	m.keepColumn()
	m.wantCursor = pid
	m.server.show(pid)
}

// cursorKey names what the pane beside the navigator is arranged for: the
// row under the cursor, and whether the picker is over it.
func (m model) cursorKey() string {
	key := ""
	if r, ok := m.selected(); ok {
		key = detailKey(r)
	}
	if m.resume != nil {
		key = "picker:" + key
	}
	return key
}

// stepShell shows the next or previous held shell in the navigator's order
// from the one shown, wrapping, and takes the keys to it: the chord
// ctrl-space j and k, from any shell.
func (m *model) stepShell(delta int) tea.Cmd {
	order := m.heldOrder()
	if len(order) == 0 {
		m.status, m.statusErr = "no shell is open", false
		return nil
	}
	at := -1
	for i, pid := range order {
		if pid == m.previewing {
			at = i
		}
	}
	next := order[(at+delta+len(order))%len(order)]
	m.show(m.terms[next])
	return nil
}

// heldOrder is every held shell in the order the navigator lists them: by
// place as the places are listed, then by age. It is the order J and K
// step through, and it does not depend on what is folded or filtered — a
// shell is still there when its row is not.
func (m model) heldOrder() []int {
	rank := map[string]int{}
	for i, p := range m.projects {
		rank[p.Path] = i
	}
	pids := make([]int, 0, len(m.terms))
	for pid := range m.terms {
		pids = append(pids, pid)
	}
	at := func(pid int) int {
		if p, ok := m.placeAt(m.terms[pid].dir); ok {
			if r, ok := rank[p.Path]; ok {
				return r
			}
		}
		return len(m.projects)
	}
	sort.Slice(pids, func(i, j int) bool {
		a, b := at(pids[i]), at(pids[j])
		if a != b {
			return a < b
		}
		return pids[i] < pids[j]
	})
	return pids
}

// openFilter starts typing a filter. The list becomes every project straight
// away, before a single character is typed: half of looking one up is
// remembering which ones there are.
func (m *model) openFilter() tea.Cmd {
	m.filterFrom = ""
	if r, ok := m.selected(); ok {
		m.filterFrom = detailKey(r)
	}
	m.resume = nil // one look at a time; the filter is the look now
	m.typing = true
	m.rebuild()
	m.cursor = 0
	m.scrollToCursor()
	return m.detailCmd()
}

// placeAt is the place a directory belongs to — the innermost repository or
// sub-project holding it, or failing those the group whose own level it is
// at — the same attribution rebuild gives a process working there.
func (m model) placeAt(dir string) (Project, bool) {
	var best Project
	found := false
	for _, p := range m.projects {
		if under(dir, p.Path) && len(p.Path) > len(best.Path) {
			best, found = p, true
		}
	}
	if !found {
		for _, g := range m.groups {
			if under(dir, g.Path) && len(g.Path) > len(best.Path) {
				best, found = g, true
			}
		}
		return best, found
	}
	for _, sp := range m.subs[best.Path] {
		if under(dir, sp.Path) && len(sp.Path) > len(best.Path) {
			best = sp
		}
	}
	return best, true
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

// paneTerm is the shell the pane should be previewing: the one belonging to
// the row under the cursor.
//
// A folded run is rarely a shell itself — the row is named for what the shell
// started — so the run is walked for the shell scrn holds in it. That shell's
// pane is where the thing the row is named for is drawing.
func (m model) paneTerm() *remoteTerm {
	r, ok := m.selected()
	if !ok || r.kind != rowProc {
		return nil
	}
	if t := m.terms[r.node.PID]; t != nil {
		return t
	}
	for _, n := range r.run {
		if t := m.terms[n.PID]; t != nil {
			return t
		}
	}
	return nil
}

// attachable reports whether enter takes you somewhere. A repository opens a
// shell in itself, and anything running inside a shell scrn holds can be
// reached by attaching to that shell. Only a process on a terminal scrn does
// not own is out of reach, and no amount of asking will change that.
func (m model) attachable(r navRow) bool {
	if r.kind != rowProc {
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
	if m.server == nil {
		m.status, m.statusErr = "no server to hold it: "+m.serverErr, true
		return nil
	}
	m.server.open(m.shellDir(r), command, "")
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

// openShell opens a shell on a repository row, or takes the client to the
// window of one already open on the row under the cursor. Enter on a
// repository always opens another, so a repository can hold as many shells
// as the work needs.
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
		m.show(t)
		return nil
	}
	return m.start("")
}

// runKill carries out a confirmed kill, splitting it by who owns the target.
//
// A shell scrn holds is hung up through the server rather than signalled: an
// interactive shell ignores SIGTERM, so signalling one leaves it sitting there
// and scrn reporting that it would not go. Everything else is somebody else's
// process, and a signal is all scrn has.
func (m *model) runKill(req *killRequest) tea.Cmd {
	var hungUp []killResult
	var signalled []*ProcNode

	for _, n := range req.nodes {
		if _, mine := m.terms[n.PID]; mine {
			m.server.closeTerm(n.PID)
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

// retryConnect schedules another attempt to reach the server, waiting twice
// as long as the last one up to a cap. The wait is reset by a server that
// talks, so a normal loss is recovered in well under a second and only a
// server that keeps failing is given room.
func (m *model) retryConnect() tea.Cmd {
	switch {
	case m.backoff <= 0:
		m.backoff = reconnectWait
	default:
		m.backoff *= 2
		if m.backoff > reconnectMax {
			m.backoff = reconnectMax
		}
	}
	return reconnect(m.backoff)
}

// askReplace arms R: ending the server outright, and the shells with it.
// There is no upgrade dance to gate it on any more — a tmux server never
// goes stale under a new build — so it is the blunt instrument, kept for
// the day something wedges, and it always asks first.
func (m *model) askReplace() tea.Cmd {
	if len(m.terms) == 0 && m.serverErr == "" {
		m.status, m.statusErr = "nothing is held; there is nothing to replace", false
		return nil
	}
	m.pendingReplace = true
	return nil
}

// run starts what the project the cursor is in says it needs, and is not
// already running. It is a list to run rather than a promise to keep, so
// running it again starts only what has since stopped.
func (m *model) run() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	return m.runPlace(r.project)
}

// runPlace starts what one place's plan says it needs and is not running.
func (m *model) runPlace(p Project) tea.Cmd {
	if m.server == nil {
		m.status, m.statusErr = "no server to hold them: "+m.serverErr, true
		return nil
	}

	plan := readPlan(p.Path)
	if len(plan.Entries) == 0 {
		m.status, m.statusErr = p.Name+" does not say what it needs", true
		return nil
	}

	missing := plan.missing(m.namesIn(p.Path))
	if len(missing) == 0 {
		m.status, m.statusErr = "everything "+p.Name+" needs is running", false
		return nil
	}

	for _, e := range missing {
		m.server.open(p.Path, e.Run, e.Name)
	}
	// Several things are starting and none of them is the one you meant, so
	// the cursor stays on the project rather than following any of them.
	m.wantCursor, m.wantProject = 0, p.Path
	// Started rather than entered: this is several things at once, and none of
	// them is more the one you meant than the others.
	m.status, m.statusErr = "started "+describeEntries(missing), false
	return m.scanNow()
}

// planned are the shells in a project that a plan started, which are the ones
// carrying the name the plan gave them.
func (m model) planned(path string) []*remoteTerm {
	var out []*remoteTerm
	for _, t := range m.terms {
		if t.name != "" && t.dir == path {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// namesIn is what a project already has running, by the names its plan uses.
func (m model) namesIn(path string) map[string]bool {
	running := map[string]bool{}
	for _, t := range m.planned(path) {
		running[t.name] = true
	}
	return running
}

// describeEntries names what was just started.
func describeEntries(entries []entry) string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return strings.Join(names, ", ")
}

// askKill arms a kill for whatever the cursor is on. A plain kill takes the
// one selected process; a tree kill takes everything below it too. On a
// repository row the two widths are the same width — everything running in
// it: a narrow kill that stopped only what the plan had started read as x
// ignoring half of what was on screen.
func (m *model) askKill(tree bool) tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}

	if r.kind != rowProc {
		// A place's kill covers everything beneath it: the sub-projects of a
		// repository, the repositories of a group. They are on screen under
		// the row, and an x that ignored them would be an x ignoring half of
		// what is shown.
		var roots []*ProcNode
		switch r.kind {
		case rowGroup:
			roots = m.groupTrees(r.project.Path)
		case rowProject:
			roots = m.repoTrees(r.project.Path)
		default:
			roots = m.byPlace[r.project.Path]
		}
		var nodes []*ProcNode
		for _, root := range roots {
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
	roots := m.byPlace[r.project.Path]
	switch r.kind {
	case rowGroup:
		roots = m.groupTrees(r.project.Path)
	case rowProject:
		roots = m.repoTrees(r.project.Path)
	case rowProc:
		return countTree(r.leaf()) - 1
	}
	total := 0
	for _, n := range roots {
		total += countTree(n)
	}
	return total
}

// spinNeeded reports whether anything on screen is moving: a process on its
// way out, or an agent at work.
func (m model) spinNeeded() bool {
	if len(m.dying) > 0 {
		return true
	}
	for _, r := range m.rows {
		if a := m.agentFor(r); a != nil && a.working() {
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
//
// It counts the keys as they are actually drawn, not as they were asked for. A
// hint block too tall for the window is cut down to leave the list a row, and
// measuring the untrimmed block instead made this say nought where the column
// still drew one — so the cursor scrolled within a list a row shorter than the
// one on screen, and could sit off the end of it.
func (m model) bodyHeight() int {
	// Two rows at the top — the masthead and the blank beneath it — though
	// the blank, being spacing, is the first thing a short window gives up.
	if h := m.height - 2; h > 0 {
		return h
	}
	if h := m.height - 1; h > 0 {
		return h
	}
	return 0
}

// paneHeight is the room the preview has, which is the whole window: scrn's
// own rows are in its column, not across the top and bottom.
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
	wasRoot := 0
	if r, ok := m.selected(); ok {
		was = detailKey(r)
		// The root of the run too: the name a run is keyed by is borrowed —
		// a transient child can name it for a single scan — but the top of
		// the run is owned, and it is what the row still is when the name
		// has moved on.
		if r.kind == rowProc {
			wasRoot = r.chain().PID
		}
	}

	// What was just started has landed. The filter has done its job: the
	// project holds work now, so it stays in the list on its own merit and the
	// search that found it can go. Clearing here rather than when the key was
	// pressed is what stops the project blinking out and back while the scan
	// catches up.
	if m.wantCursor != 0 && m.running(m.wantCursor) {
		m.filter = ""
	}
	// The server holding them is enough to know they exist; waiting for the
	// process scan as well would hold the search open for a poll longer, and
	// the server is the thing that was actually asked.
	if m.wantProject != "" && len(m.planned(m.wantProject)) > 0 {
		m.filter = ""
	}

	m.groupProcs()
	m.rows = m.flatten()

	// Prefer the same subject; failing that, the row that grew from the
	// same root — a run renames itself when what it is running comes or
	// goes, and the cursor should ride the rename rather than strand on
	// whatever slid into the old row's place. Only with both gone — a
	// process that exited — hold the position in the list rather than
	// jumping to the top.
	found, root := -1, -1
	for i, r := range m.rows {
		if detailKey(r) == was {
			found = i
			break
		}
		if root < 0 && wasRoot != 0 && r.kind == rowProc && r.chain().PID == wasRoot {
			root = i
		}
	}
	if found < 0 {
		found = root
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

	// A project whose processes were just started keeps the cursor, rather
	// than following any one of the several things that started. It holds
	// until the rows settle, then lets go.
	if m.wantProject != "" {
		m.selectProject(m.wantProject)
		if m.filter == "" && len(m.byPlace[m.wantProject]) > 0 {
			m.wantProject = ""
		}
	}

	// A shell just opened takes the cursor as soon as it is in the tree, so
	// that leaving it leaves the cursor somewhere that makes sense. One the
	// scan has seen and the tree has no row for — opened somewhere no
	// project holds — is not coming, and the cursor stops waiting for it.
	if m.wantCursor != 0 {
		landed := false
		for i, r := range m.rows {
			// The shell may have folded into whatever it started, so the row
			// to land on is the one whose run begins with it.
			if r.kind == rowProc && r.holds(m.wantCursor) {
				m.cursor, landed = i, true
				break
			}
		}
		if landed || m.running(m.wantCursor) {
			m.wantCursor = 0
		}
	}

	m.pruneDetails()
	m.pruneDying()
	m.scrollToCursor()
	m.dressWindows()
}

// dressWindows names each held shell's pane — its place, what is running
// there, and its mark — for the terminal's title while the keys are in
// it. Only what changed is said: saying the same thing again would be
// noise on the server.
func (m *model) dressWindows() {
	for pid, t := range m.terms {
		name, mark := m.shellLabel(pid, t)
		if mark != "" {
			name += " " + mark
		}
		if m.dressed[pid] != name {
			m.dressed[pid] = name
			m.server.dress(pid, name)
		}
	}
}

// dressStatus writes the navigator's part of the status line — the mode
// its keys are in and what it has to say — when either changed.
func (m *model) dressStatus() {
	t := m.statusLine()
	if t != m.said {
		m.said = t
		m.server.say(t)
	}
}

// statusLine is what the navigator has the status line read, in tmux's
// styling. The mode is the navigator's only when the keys are in something
// other than the list itself — a query being typed, a filter standing, a
// confirmation waiting on its second key — and tmux names the rest. The
// message is the last report, or the prompt a confirmation carries with
// it: while one is on screen it is the only thing the next keystroke is
// about. Most of the time both are empty, and the line is the strip.
func (m model) statusLine() statusText {
	var t statusText
	switch {
	case m.pendingReplace:
		t.mode = statusChip(tp.amber, "CONFIRM")
		t.msg = tmuxStyled(tp.amber, true, " end the server, and "+
			plural(len(m.terms), "shell", "shells")+"? · R confirms")
		return t

	case m.pendingKill != nil:
		t.mode = statusChip(tp.amber, "CONFIRM")
		t.msg = tmuxStyled(tp.amber, true, " kill "+m.pendingKill.subject+"? · x confirms")
		return t

	case m.resume != nil:
		// The picker wears the filter's face: it is the same kind of
		// typing, aimed at conversations instead of places.
		t.mode = statusChip(tp.fg, "CONTINUE /"+m.resume.query+"█")

	case m.typing:
		t.mode = statusChip(tp.fg, "/"+m.filter+"█")

	case m.filter != "":
		// A standing filter is the navigator's mode still, in its color.
		t.mode = statusChip(tp.cyan, "/"+m.filter)
	}

	// What was just reported stays beside the query being typed: acting
	// from the search is the point of it, and an action that says nothing
	// looks like one that did nothing.
	if m.status != "" {
		color := tp.fg
		if m.statusErr {
			color = tp.red
		}
		t.msg = tmuxStyled(color, false, " "+m.status)
	}
	return t
}

// windowLabelWidth is as much of a shell's label as its title gets: a
// whole command line there would run the terminal's title bar off the
// end.
const windowLabelWidth = 24

// shellLabel is what a held shell's window is called and how it is marked:
// the place it works in and the name its row would wear — the plan's name
// for it, unless what is running says more — and its agent's mark, if it
// is running one. It reads the process tree rather than the rows, because
// a row can be folded away or filtered out and the window is still there.
func (m model) shellLabel(pid int, t *remoteTerm) (string, string) {
	label := t.name
	mark := ""
	if n := m.nodes[pid]; n != nil {
		run := []*ProcNode{n}
		for i := 0; len(n.Children) == 1 && i < len(m.procs); i++ {
			n = n.Children[0]
			run = append(run, n)
		}
		r := navRow{kind: rowProc, run: run, node: nameOf(run)}
		if cmd := commandOf(r.node); label == "" || tellsMore(cmd, label) {
			label = cmd
		}
		if a := m.agentFor(r); a != nil {
			switch {
			case a.working():
				mark = glyphBusy
			case m.awaiting(r) != nil:
				if _, blocked := a.blocked(); blocked {
					mark = glyphAsk
				} else {
					mark = glyphOn
				}
			default:
				mark = glyphOff
			}
		}
	}
	if label == "" {
		label = "shell"
	}
	if p, ok := m.placeAt(t.dir); ok {
		label = p.Name + ": " + label
	}
	return truncateTail(label, windowLabelWidth), mark
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
	m.grouped = make(map[string][]Project, len(m.groups))
	for _, p := range m.projects {
		if p.Group != "" {
			m.grouped[p.Group] = append(m.grouped[p.Group], p)
		}
	}

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
		if best == "" {
			// Work at a group's own level — a shell opened on the group row —
			// is in none of its repositories, and belongs to the group.
			for _, g := range m.groups {
				if under(pr.Dir, g.Path) && len(g.Path) > len(best) {
					best = g.Path
				}
			}
			if best != "" {
				owner[best] = append(owner[best], pr)
			}
			continue
		}
		// Within the repository, the innermost sub-project containing the
		// process is its place; a process in none of them works at the root.
		place := best
		for _, sp := range m.subs[best] {
			if under(pr.Dir, sp.Path) && len(sp.Path) > len(place) {
				place = sp.Path
			}
		}
		owner[place] = append(owner[place], pr)
	}

	m.byPlace = make(map[string][]*ProcNode, len(owner))
	m.nodes = make(map[int]*ProcNode, len(m.procs))
	for path, procs := range owner {
		m.byPlace[path] = procForest(procs)
		for _, root := range m.byPlace[path] {
			indexNodes(root, m.nodes)
		}
	}
}

// repoTrees is every process tree in a repository: the ones at its root and
// the ones inside each of its sub-projects.
func (m model) repoTrees(repo string) []*ProcNode {
	out := append([]*ProcNode{}, m.byPlace[repo]...)
	for _, s := range m.subs[repo] {
		out = append(out, m.byPlace[s.Path]...)
	}
	return out
}

// placeHasWork reports whether a sub-project holds a process tree or a shell
// the server is holding there — the shell counting before the scan has seen
// it, for the same reason a repository's does.
func (m model) placeHasWork(path string) bool {
	if len(m.byPlace[path]) > 0 {
		return true
	}
	for _, t := range m.terms {
		if t.dir == path {
			return true
		}
	}
	return false
}

// workIn reports whether anything is running in a repository, its
// sub-projects included.
func (m model) workIn(repo string) bool {
	if len(m.repoTrees(repo)) > 0 {
		return true
	}
	for _, t := range m.terms {
		if under(t.dir, repo) {
			return true
		}
	}
	return false
}

// groupTrees is every process tree in a group: at its own level, and inside
// each of its repositories.
func (m model) groupTrees(g string) []*ProcNode {
	out := append([]*ProcNode{}, m.byPlace[g]...)
	for _, p := range m.grouped[g] {
		out = append(out, m.repoTrees(p.Path)...)
	}
	return out
}

// workInGroup reports whether anything is running anywhere in a group. The
// terms check covers a shell just opened at the group's own level, before
// the scan has seen it.
func (m model) workInGroup(g Project) bool {
	if len(m.byPlace[g.Path]) > 0 {
		return true
	}
	for _, t := range m.terms {
		if under(t.dir, g.Path) {
			return true
		}
	}
	return false
}

// flatten turns the visible places — groups, repositories, sub-projects —
// and their process trees into the flat list of selectable rows the
// navigator draws and the cursor walks.
func (m model) flatten() []navRow {
	var rows []navRow
	for _, top := range m.topPlaces() {
		if top.kind == rowProject {
			rows = append(rows, m.flattenRepo(top.project, "")...)
			continue
		}

		rows = append(rows, top)
		repos := m.visibleRepos(top.project)
		if m.typing {
			// Work at the group's own level answers a query the same way it
			// is listed without one: before the repositories it sits beside.
			if f := strings.ToLower(strings.TrimSpace(m.filter)); f != "" {
				roots := matchingProcs(m.byPlace[top.project.Path], f)
				for i, n := range roots {
					rows = append(rows, m.flattenProc(top.project, n, "  ", i == len(roots)-1)...)
				}
			}
			for _, p := range repos {
				rows = append(rows, m.flattenRepo(p, "  ")...)
			}
			continue
		}
		if m.collapsed[detailKey(top)] {
			continue
		}
		// Work at the group's own level — a shell opened on the group row —
		// comes before the repositories it sits beside.
		roots := m.byPlace[top.project.Path]
		for i, n := range roots {
			rows = append(rows, m.flattenProc(top.project, n, "  ", i == len(roots)-1)...)
		}
		for _, p := range repos {
			rows = append(rows, m.flattenRepo(p, "  ")...)
		}
	}
	return rows
}

// topPlaces is the top of the navigator: the groups and the repositories
// standing alone, in one alphabetical order, each listed when its own rule
// says so.
func (m model) topPlaces() []navRow {
	var out []navRow
	for _, g := range m.groups {
		if m.groupVisible(g) {
			out = append(out, navRow{kind: rowGroup, project: g})
		}
	}
	for _, p := range m.projects {
		if p.Group == "" && m.repoVisible(p) {
			out = append(out, navRow{kind: rowProject, project: p})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].project.Name), strings.ToLower(out[j].project.Name)
		if a != b {
			return a < b
		}
		return out[i].project.Path < out[j].project.Path
	})
	return out
}

// repoVisible is the repositories' listing rule. While a filter is at work,
// the ones that answer to it — by name or path, by a sub-project answering,
// or by a process running there whose command answers, so typing claude finds
// the repositories a claude is working in and not only the ones named for it.
// A repository whose sub-project answers is listed for the answer's sake, the
// way a directory holds the file you were looking for. Otherwise the ones
// with work in them, or all behind the dot. A shell the server is holding
// counts as work before the scan has seen it: it is running, and asking for
// it and then watching the project vanish for a poll would be a lie about
// what just happened.
func (m model) repoVisible(p Project) bool {
	if m.typing || m.filter != "" {
		return matchesFilter(p, m.filter) || len(m.matchingSubs(p)) > 0 ||
			m.procAnswers(p.Path, m.filter)
	}
	return m.showAll || m.workIn(p.Path)
}

// procAnswers reports whether something running at place answers the filter
// by its command. The whole tree is asked: a shell running a claude answers
// for claude, whichever of them the row happens to be named after.
func (m model) procAnswers(place, filter string) bool {
	f := strings.ToLower(strings.TrimSpace(filter))
	if f == "" {
		return false
	}
	return len(matchingProcs(m.byPlace[place], f)) > 0
}

// matchingProcs prunes process trees to what answers the filter, folded and
// lowered already: a process whose own command answers stays with its whole
// subtree, and a parent whose child answers stays as the trimmed copy that
// leads there. The copies carry the original pids, which is all a step-in or
// a kill reads.
func matchingProcs(ns []*ProcNode, f string) []*ProcNode {
	var out []*ProcNode
	for _, n := range ns {
		if answers(f, n.Command) {
			out = append(out, n)
			continue
		}
		if kept := matchingProcs(n.Children, f); len(kept) > 0 {
			c := *n
			c.Children = kept
			out = append(out, &c)
		}
	}
	return out
}

// groupVisible is the same rule at the group's altitude: its own name or a
// process running at its folder answering the filter, or any of its
// repositories answering; its own directory holding work, or any of its
// repositories holding some.
func (m model) groupVisible(g Project) bool {
	if m.typing || m.filter != "" {
		return matchesFilter(g, m.filter) || len(m.visibleRepos(g)) > 0 ||
			m.procAnswers(g.Path, m.filter)
	}
	if m.showAll || m.workInGroup(g) {
		return true
	}
	for _, p := range m.grouped[g.Path] {
		if m.workIn(p.Path) {
			return true
		}
	}
	return false
}

// visibleRepos is the repositories listed beneath a group, by the
// repositories' own rule.
func (m model) visibleRepos(g Project) []Project {
	var out []Project
	for _, p := range m.grouped[g.Path] {
		if m.repoVisible(p) {
			out = append(out, p)
		}
	}
	return out
}

// flattenRepo is one repository's section: its row, and beneath it the
// sub-projects and process trees, everything shifted right when the
// repository itself sits under a group.
func (m model) flattenRepo(p Project, indent string) []navRow {
	row := navRow{kind: rowProject, project: p, prefix: indent}
	rows := []navRow{row}
	subs := m.visibleSubs(p)
	// While a project is being looked up, an empty query lists places alone —
	// every process of every project would bury the names being scanned for.
	// But a query is a name, and a process that answers to it is as much a
	// thing being looked for as a project is: it is listed, pruned to the
	// branches that answer, so enter can step into it and ctrl+x can kill it.
	if m.typing {
		f := strings.ToLower(strings.TrimSpace(m.filter))
		var roots []*ProcNode
		if f != "" {
			roots = matchingProcs(m.byPlace[p.Path], f)
		}
		for i, n := range roots {
			rows = append(rows, m.flattenProc(p, n, indent, i == len(roots)-1 && len(subs) == 0)...)
		}
		for i, sp := range subs {
			srow := navRow{kind: rowSub, project: sp, prefix: indent,
				last: i == len(subs)-1}
			rows = append(rows, srow)
			if f == "" {
				continue
			}
			rail := indent + glyphRail + " "
			if srow.last {
				rail = indent + "  "
			}
			sroots := matchingProcs(m.byPlace[sp.Path], f)
			for j, n := range sroots {
				rows = append(rows, m.flattenProc(sp, n, rail, j == len(sroots)-1)...)
			}
		}
		return rows
	}
	if m.collapsed[detailKey(row)] {
		return rows
	}
	// Processes and sub-projects hang off the repository as one family of
	// siblings: a sub-project takes a branch the way a process does, and the
	// rail runs on past the last process when one is still to come.
	roots := m.byPlace[p.Path]
	for i, n := range roots {
		rows = append(rows, m.flattenProc(p, n, indent, i == len(roots)-1 && len(subs) == 0)...)
	}
	for i, sp := range subs {
		srow := navRow{kind: rowSub, project: sp, prefix: indent, last: i == len(subs)-1}
		rows = append(rows, srow)
		if m.collapsed[detailKey(srow)] {
			continue
		}
		rail := indent + glyphRail + " "
		if srow.last {
			rail = indent + "  "
		}
		sroots := m.byPlace[sp.Path]
		for i, n := range sroots {
			rows = append(rows, m.flattenProc(sp, n, rail, i == len(sroots)-1)...)
		}
	}
	return rows
}

// visibleSubs is the sub-projects listed beneath a repository. While a
// filter is at work they are the ones that answer to it — an empty query
// lists projects alone, or every sub-project of every repository would bury
// the list being remembered. Otherwise they follow the repositories' own
// rule: the ones with work in them, or all of them behind the dot.
func (m model) visibleSubs(p Project) []Project {
	if m.typing || m.filter != "" {
		return m.matchingSubs(p)
	}
	all := m.subs[p.Path]
	if m.showAll {
		return all
	}
	var out []Project
	for _, sp := range all {
		if m.placeHasWork(sp.Path) {
			out = append(out, sp)
		}
	}
	return out
}

// matchingSubs is the sub-projects of a repository that answer to the
// filter, by their own path or qualified by their repository's name — which
// is how "mono api" and "services/api" both find the same place — or by a
// process running in them whose command answers.
func (m model) matchingSubs(p Project) []Project {
	f := strings.ToLower(strings.TrimSpace(m.filter))
	if f == "" {
		return nil
	}
	var out []Project
	for _, sp := range m.subs[p.Path] {
		if answers(f, sp.Name) || answers(f, p.Name+"/"+sp.Name) ||
			m.procAnswers(sp.Path, f) {
			out = append(out, sp)
		}
	}
	return out
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

	childPrefix := prefix + glyphRail + " "
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

// matchesFilter reports whether a repository answers to what has been typed.
// The path is searched as well as the name, so a directory that is only in the
// name of a repository's parent still finds it.
func matchesFilter(p Project, filter string) bool {
	return answers(filter, p.Name) || answers(filter, p.Path)
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
//
// A repository is re-asked on its own, slower cadence: its answers cost git
// real work in a big checkout. Landing on a row still loads it at once —
// this is only the background refresh of an answer already on screen.
func (m model) refreshDetailCmd() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	if r.kind != rowProc && m.ticks%repoDetailEvery != 0 {
		return nil
	}
	return loadDetail(r, m.placeCount(r), len(m.grouped[r.project.Path]),
		m.agentFor(r), m.namesIn(r.project.Path))
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

// agentFor returns the agent instance a row is running, if it is one. The
// process is checked as well as what the agent advertises, because what it
// advertises can outlive its process and a reused pid would otherwise be
// dressed up as an agent.
func (m model) agentFor(r navRow) agent {
	if r.kind != rowProc {
		return nil
	}
	a, ok := m.agents[r.node.PID]
	if !ok || !runs(a, r.node) {
		return nil
	}
	return a
}

// awaiting returns the agent a row is running when it is waiting on its user:
// done with a turn it was seen working, or blocked mid-turn on a specific
// ask. An instance idle since it was started has not finished a turn and is
// not owed an answer — but a blocked one is owed its answer regardless of
// history, because the ask exists whether or not this window watched the
// work that raised it.
func (m model) awaiting(r navRow) agent {
	a := m.agentFor(r)
	if a == nil || a.working() {
		return nil
	}
	if _, ok := a.blocked(); ok {
		return a
	}
	if !m.worked[r.node.PID] {
		return nil
	}
	return a
}

// syncPreview keeps the pane beside the navigator holding the shell under
// the cursor: the cursor has moved, or the world has changed under it. A
// row with no held shell — a place, a process scrn only watches — and the
// picker both ask for the window whole, so the shown shell goes back to a
// window of its own.
func (m *model) syncPreview() {
	// A shell just opened is shown before its row exists; until the row
	// lands and the cursor is on it, the cursor stands somewhere else and
	// says nothing about what the pane should hold.
	if m.wantCursor != 0 || m.server == nil {
		return
	}
	key := m.cursorKey()
	if key == m.previewKey {
		return
	}
	m.previewKey = key
	want := 0
	if m.resume == nil {
		if t := m.paneTerm(); t != nil {
			want = t.pid
		}
	}
	if want == m.previewing {
		return
	}
	m.previewing = want
	m.keepColumn()
	m.server.preview(want)
	m.dressWindows()
}

// detailCmd inspects the selected row unless it has been inspected already.
// The cursor has just moved or the world just changed under it, so this is
// also where the pane's preview follows the selection.
func (m *model) detailCmd() tea.Cmd {
	m.syncPreview()
	r, ok := m.selected()
	if !ok {
		return nil
	}
	if _, cached := m.details[detailKey(r)]; cached {
		return nil
	}
	return loadDetail(r, m.placeCount(r), len(m.grouped[r.project.Path]),
		m.agentFor(r), m.namesIn(r.project.Path))
}

// placeCount is how many process trees a row's place holds — for a
// repository or a group, everything in it.
func (m model) placeCount(r navRow) int {
	switch r.kind {
	case rowGroup:
		return len(m.groupTrees(r.project.Path))
	case rowProject:
		return len(m.repoTrees(r.project.Path))
	}
	return len(m.byPlace[r.project.Path])
}

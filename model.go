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
	"github.com/charmbracelet/x/ansi"
)

// navWidth is the column the navigator occupies, divider excluded. The
// default suits short home-project names; the config widens it for the long
// qualified paths a work checkout produces.
var navWidth = 28

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

	// showHelp puts the keys modal over the window. The keys are worth a foot
	// line to say they exist and a modal to spell out; the next keystroke,
	// whatever it is, puts the modal away.
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

	// Where the look began: the subject under the cursor, and the shell the
	// keys were in if they were in one. Abandoning the filter with esc puts
	// both back — acting on a result does not, because acting is the point
	// of having looked.
	filterFrom      string
	filterFromFocus int

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

	// scroll is the focused pane's transcript being read back through, and
	// nil while the pane is live.
	scroll *scrollView

	// drag is a selection being swept across the pane with the mouse held,
	// and nil otherwise. It selects what the glass shows — whatever the
	// pane is drawing — and release carries it to the clipboard.
	drag *paneDrag

	// lastClick is where and when the pane was last pressed, for telling a
	// double-click — which copies the word under it — from two clicks that
	// merely happened.
	lastClick paneClick

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

	// pendingPrefix says ctrl+space has been pressed and the next key names
	// what is wanted of scrn; anything unbound cancels it.
	pendingPrefix bool

	// worked is every agent pid that has been seen working. Waiting means a
	// finished turn — busy once, idle now — and an instance idle since it
	// was started has not finished anything and is not owed an answer.
	worked map[int]bool

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

	// backoff is the wait before the next attempt to reach a daemon that went
	// away or could not be reached, doubled per consecutive failure and reset
	// once a daemon is talking.
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

	// focus is the pid of the terminal taking keystrokes, or 0 when the
	// navigator has them. A focused terminal shows in the pane whatever the
	// cursor is on, so that typing never goes somewhere you cannot see.
	focus int

	// lastFocus is the shell the keys were in before this one, which is what
	// ctrl+space ctrl+space steps back to.
	lastFocus int

	// windowTitle is what the window's tab should say: the last title a
	// focused shell asked for. It rides out on every view, because a title is
	// a standing fact about the window rather than a one-time ask.
	windowTitle string

	// termFG and termBG are the real terminal's colors, held for the server:
	// they can answer before the session is up, and the server needs them to
	// answer a pane asking what color the terminal is.
	termFG, termBG string

	// previewing is the shell this window watches only because the pane is
	// showing it — the held shell under the cursor, as opposed to the one
	// being typed into. Tracked so that leaving the row can detach it: a
	// shell merely glanced at should not keep this window's pane in its size
	// arbitration.
	previewing int

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
		// Init sends the first scan, and Init cannot write here to say so.
		scanning: true,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scanProjects, scanProcs, scanAgents, connectDaemon(),
		tick(procPoll), agentTick(),
		// The styles depend on the terminal's background, which lipgloss no
		// longer guesses at: scrn asks, and rebuilds them on the answer. The
		// foreground is asked for the server's sake — it answers panes that
		// ask what color the terminal is.
		func() tea.Msg { return tea.RequestBackgroundColor() },
		func() tea.Msg { return tea.RequestForegroundColor() })
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.scrollToCursor()
		// A transcript being read was cut to a pane that no longer exists.
		// Nothing rewraps; the reading ends and can be begun again. A sweep
		// mid-flight loses its geometry the same way.
		m.scroll = nil
		m.drag = nil
		// The shells are drawing into the pane, so they are the ones that have
		// been resized, whatever the window did.
		for pid := range m.terms {
			m.daemon.resize(pid, m.detailWidth(), m.paneHeight())
		}

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
		return m, connectDaemon()

	case daemonReadyMsg:
		if msg.err != nil {
			// Not reaching the daemon is not the end of it: nothing but a retry
			// will ever turn this window back into a useful one.
			m.daemonErr = msg.err.Error()
			return m, m.retryConnect()
		}
		m.daemon, m.daemonErr = msg.session, ""
		// The colors can have answered before the session was up; a session
		// that arrives second is told what the first answer said.
		m.daemon.theme(m.termFG, m.termBG)
		// A fresh connection holds no watches, whatever this window was
		// previewing over the last one; the preview is asked for again once
		// the daemon says what it holds.
		m.previewing = 0
		// Ask what is already running: shells from a window that has since
		// been closed are still there, and this is where they come back.
		m.daemon.list()
		return m, nextEvent(m.daemon)

	case daemonErrorMsg:
		// One ask failed; the daemon and its shells are fine. Say what it said
		// and carry on listening.
		m.status, m.statusErr = msg.err.Error(), true
		return m, nextEvent(m.daemon)

	case daemonLostMsg:
		// The server hung this window up — the last shell closed and the
		// session went with it, or something ended the server outright. The
		// bridge keeps watching for a new one on its own; here the window
		// only stops showing shells that are no longer held.
		m.terms, m.focus, m.lastFocus = map[int]*remoteTerm{}, 0, 0
		m.scroll = nil // the transcript went with the server holding it
		if msg.err != nil {
			m.status, m.statusErr = msg.err.Error(), true
		}
		m.rebuild()
		return m, nextEvent(m.daemon)

	case termOpenedMsg:
		if _, ok := m.terms[msg.pid]; !ok {
			m.terms[msg.pid] = &remoteTerm{pid: msg.pid, dir: msg.dir, name: msg.name}
		}
		// A shell asked for by name is one of several a project needed, and
		// none of them is more the one you meant than the others. Only a shell
		// opened on its own takes the keys and the cursor.
		if msg.name == "" {
			m.setFocus(msg.pid)
			m.wantCursor = msg.pid
		}
		return m, tea.Batch(nextEvent(m.daemon), m.scanNow())

	case sessionsMsg:
		// The server is talking, so whatever chase was on is over.
		m.backoff = 0
		// The server is the authority on what it holds, so the client takes
		// the list rather than merging into what it thought it knew.
		held := make(map[int]*remoteTerm, len(msg.sessions))
		for _, s := range msg.sessions {
			if was, ok := m.terms[s.PID]; ok {
				held[s.PID] = was
				continue
			}
			held[s.PID] = &remoteTerm{pid: s.PID, dir: s.Dir, name: s.Name}
		}
		m.terms = held
		if _, ok := m.terms[m.focus]; !ok {
			m.focus = 0
		}
		m.rebuild()
		// The daemon may have just said what it holds while the cursor was
		// already standing on one of those shells; the pane should not wait
		// for the cursor to move before showing it.
		m.syncPreview()
		return m, tea.Batch(nextEvent(m.daemon), m.scanNow())

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
		t.sb, t.mouse, t.alt = msg.sb, msg.mouse, msg.alt

		// Only the shell being looked at speaks for the window. Another one
		// finishing a build should not retitle a tab showing something else.
		if m.focus == msg.pid && t.title != "" {
			m.windowTitle = oscTitleText(t.title)
		}
		return m, nextEvent(m.daemon)

	case historyMsg:
		s := m.scroll
		t, ok := m.terms[msg.pid]
		if s == nil || s.pid != msg.pid || !ok {
			// Asked for and no longer wanted; the reading has already ended.
			return m, nextEvent(m.daemon)
		}
		// The transcript plus the screen as it stands is the whole document.
		// The screen is frozen into it on purpose: output only appends, so a
		// snapshot is a true prefix of the live transcript — never wrong,
		// merely not growing — and it holds still under the reader.
		hadDoc := s.doc != nil
		s.doc = strings.Split(t.screen, "\n")
		if msg.history != "" {
			grown := strings.Split(msg.history, "\n")
			s.doc = append(grown, s.doc...)
			if hadDoc {
				// The document grew a prefix; the cursor and the mark keep
				// their lines by shifting with it.
				s.cur += len(grown)
				if s.anchor >= 0 {
					s.anchor += len(grown)
				}
			}
		}
		if !hadDoc {
			// The wheel's way in: the cursor lands with the transcript, on
			// the bottom visible line.
			s.cur = max(len(s.doc)-1-s.above, 0)
		}
		if max := m.scrollMax(); s.above > max {
			s.above = max
		}
		m.clampCur()
		if s.above <= 0 && s.anchor < 0 {
			m.scroll = nil // nothing above after all
		}
		return m, nextEvent(m.daemon)

	case termGoneMsg:
		delete(m.terms, msg.pid)
		if m.focus == msg.pid {
			m.focus = 0
		}
		if m.scroll != nil && m.scroll.pid == msg.pid {
			m.scroll = nil
		}
		m.rebuild()
		// Asking again is what notices a daemon that has just become
		// replaceable: the shell keeping an out-of-date one alive was this.
		m.daemon.list()
		return m, tea.Batch(nextEvent(m.daemon), m.scanNow())

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
		// An instance that has started working sets the markers turning.
		if !m.spinning && m.spinNeeded() {
			m.spinning = true
			return m, spin()
		}

	case detailMsg:
		m.details[msg.key] = msg.fields

	case copiedMsg:
		switch {
		case msg.err != nil:
			m.status, m.statusErr = "could not copy: "+msg.err.Error(), true
		case msg.what != "":
			m.status, m.statusErr = "copied "+truncateRunes(msg.what, 40), false
		default:
			m.status, m.statusErr = "copied "+plural(msg.n, "line", "lines"), false
		}

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

	case tea.MouseMsg:
		// The keys modal leaves on any keystroke; a click is no different.
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}
		// An armed confirmation is about the next key alone; the mouse must
		// not move the subject out from under it.
		if m.pendingKill != nil || m.pendingReplace {
			return m, nil
		}

		// Left of the divider the mouse is the navigator's: the wheel walks
		// the list and a press puts the cursor on the row it landed on —
		// the pane previews it, and a click on the preview then steps in.
		// Not while reading or picking: those own the pane, and the list
		// should not slide beneath them.
		if mo := msg.Mouse(); mo.X < navWidth && m.scroll == nil && m.resume == nil {
			if delta := wheelDelta(msg); delta != 0 {
				return m, m.jump(m.cursor - delta/wheelLines*navWheelRows)
			}
			if _, press := msg.(tea.MouseClickMsg); press {
				if i, ok := m.rowAt(mo.Y); ok {
					cmd := m.jump(i)
					// A row that can be stepped into is stepped into:
					// clicking a process is switching to it, from wherever
					// the keys were. Anything else — a place, a process
					// scrn cannot reach — is selected, the pane previewing
					// it, and a click on the preview goes no further.
					if r := m.rows[i]; r.kind == rowProc {
						if t := m.owningTerm(r.node.PID); t != nil {
							m.attachTo(t)
							return m, nil
						}
					}
					return m, cmd
				}
			}
			return m, nil
		}

		// The mouse belongs to whatever the pane is showing — the focused
		// shell, a preview, the reader, the details. Every process gets its
		// wheel, not just the one being typed into.
		if !m.showDetail() {
			return m, nil
		}
		ev := mouseEvent(msg, m.paneLeft(), 0)
		if ev == nil {
			return m, nil
		}
		t := m.paneTerm()

		// The wheels first: a wheel never sweeps.
		if _, wheel := msg.(tea.MouseWheelMsg); wheel {
			// While the transcript is being read the wheel moves it, and
			// nothing reaches the shell.
			if m.scroll != nil {
				m.scrollBy(wheelDelta(msg))
				return m, nil
			}
			if t == nil {
				return m, nil
			}
			// A wheel turned up over a pane whose program is not listening
			// for the mouse starts reading the transcript — on the primary
			// screen, which is the one a transcript is above.
			if wheelDelta(msg) > 0 && !t.mouse && !t.alt && t.sb > 0 {
				m.scroll = &scrollView{pid: t.pid, above: wheelDelta(msg), anchor: -1}
				m.daemon.history(t.pid)
				return m, nil
			}
			// Over the alternate screen the wheel becomes the arrow keys it
			// would have meant — how less and man scroll under any terminal
			// that implements alternate scroll, and here scrn is the
			// terminal.
			if key, ok := wheelAsArrow(msg); ok && !t.mouse && t.alt {
				for range wheelArrowCount {
					m.daemon.key(t.pid, &keyPress{Code: key})
				}
				return m, nil
			}
			m.daemon.mouse(t.pid, ev)
			return m, nil
		}

		// The left button sweeps: press arms, motion extends, and what the
		// glass shows under the sweep is on the clipboard at release — the
		// way dragging copies in a terminal with nothing in the middle. A
		// press that never moved is still a click, delivered whole on
		// release: forwarded to a focused program, or stepping into a
		// preview the way clicking an unfocused window focuses it.
		if ev.Button == int(tea.MouseLeft) || (ev.Button == int(tea.MouseNone) && m.drag != nil) {
			switch ev.Action {
			case actPress:
				// The same cell pressed twice in a beat is a double-click:
				// the word under it goes to the clipboard, and this press
				// goes no further — the first click already said what a
				// click says.
				if ev.X == m.lastClick.x && ev.Y == m.lastClick.y &&
					time.Since(m.lastClick.at) < doubleClickWithin {
					m.lastClick = paneClick{}
					word := wordAt(m.paneLines(m.detailWidth(), m.paneHeight()), ev.X, ev.Y)
					if word == "" {
						return m, nil
					}
					return m, func() tea.Msg {
						return copiedMsg{what: word, err: writeClipboard(word)}
					}
				}
				m.lastClick = paneClick{x: ev.X, y: ev.Y, at: time.Now()}
				m.drag = &paneDrag{sx: ev.X, sy: ev.Y, x: ev.X, y: ev.Y, press: ev}
				return m, nil
			case actMotion:
				if m.drag != nil {
					m.drag.x, m.drag.y = ev.X, ev.Y
					// Only motion that went somewhere makes a sweep: a
					// wobble inside the pressed cell is still a click.
					if ev.X != m.drag.sx || ev.Y != m.drag.sy {
						m.drag.moved = true
					}
				}
				return m, nil
			case actRelease:
				d := m.drag
				if d == nil {
					return m, nil
				}
				m.drag = nil
				if d.moved {
					text, n := dragText(m.paneLines(m.detailWidth(), m.paneHeight()),
						m.detailWidth(), d.sx, d.sy, ev.X, ev.Y)
					return m, func() tea.Msg { return copiedMsg{n: n, err: writeClipboard(text)} }
				}
				if m.scroll != nil || t == nil {
					return m, nil // the reader has no click to give anything
				}
				if m.focused() != t {
					m.attachTo(t)
					return m, nil
				}
				m.daemon.mouse(t.pid, d.press)
				m.daemon.mouse(t.pid, ev)
				return m, nil
			}
		}

		// The other buttons are the program's, as they always were.
		if t != nil && m.focused() == t {
			m.daemon.mouse(t.pid, ev)
		}
		return m, nil

	case tea.BackgroundColorMsg:
		// The answer to Init's ask: now the styles can pick their side, and
		// the server can answer panes asking what color the terminal is.
		applyBackground(msg.IsDark())
		if msg.Color != nil {
			m.termBG = msg.String()
			m.daemon.theme(m.termFG, m.termBG)
		}
		return m, nil

	case tea.ForegroundColorMsg:
		if msg.Color != nil {
			m.termFG = msg.String()
			m.daemon.theme(m.termFG, m.termBG)
		}
		return m, nil

	case tea.PasteMsg:
		// A paste while the keys modal is up dismisses it, the way any
		// keystroke does, and goes no further.
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}
		// Reading the transcript swallows it: the reader is looking, and
		// typing lands nowhere.
		if m.scroll != nil {
			return m, nil
		}
		// Into the picker it is more of the query, the way it is for the
		// filter: pasting a phrase from a transcript is a fine way to look.
		if m.resume != nil {
			m.setResumeQuery(m.resume.query + msg.Content)
			return m, nil
		}
		// Pasted text goes to a focused shell as a paste rather than as the
		// keystrokes it would have taken to type, so a program that asked for
		// bracketed paste is told where it starts and stops.
		if t := m.focused(); t != nil {
			m.daemon.paste(t.pid, msg.Content)
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
		m.status, m.statusErr = "nothing is focused to paste into", false
		return m, nil

	case tea.KeyPressMsg:
		// The keys modal takes the next key, whatever it is: it was asked for
		// with a keystroke and leaves on one, and nothing under it should act
		// while it covers the window.
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}

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

		// ctrl+space is scrn's prefix, and it is taken everywhere — over the
		// navigator, the filter, a focused shell — because its point is to
		// reach scrn from wherever the keys are currently going. The chords
		// keep their letters' meanings: another ctrl+space toggles between
		// this shell and the one viewed before it, enter goes to the next
		// agent waiting on its user, / finds, j and k step through the
		// shells scrn holds, s a r act where the keys are, o steps out to
		// the navigator, q quits, ? shows the keys. Anything unbound cancels
		// it and is swallowed, the way a half-finished gg swallows.
		if m.pendingPrefix {
			m.pendingPrefix = false
			if isPrefix(msg) {
				return m, m.toggleFocus()
			}
			switch msg.String() {
			case "?":
				m.showHelp = true
			case "enter":
				return m, m.jumpWaiting()
			case "/":
				return m, m.openFilter()
			case "j":
				return m, m.attachStep(1)
			case "k":
				return m, m.attachStep(-1)
			case "s":
				return m, m.startHere("")
			case "a":
				return m, m.startHere(startAgent())
			case "A":
				return m, m.resumeHere()
			case "v":
				// Into the reader on purpose — the pager's screen included,
				// where the wheel cannot start it. v marks, y copies.
				return m, m.openReader()
			case "r":
				return m, m.runHere()
			case "n":
				// To the navigator from wherever the keys were — a shell,
				// the transcript, the filter mid-word. All the way out: the
				// letter is the destination, not the leaving.
				m.scroll = nil
				m.resume = nil
				m.typing = false
				m.setFocus(0)
				return m, m.detailCmd()
			case "q":
				// Leaving is q's word alone, and the prefix carries it out
				// of a focused shell the letter would otherwise type into.
				return m, tea.Quit
			}
			return m, nil
		}
		if isPrefix(msg) {
			m.pendingPrefix = true
			return m, nil
		}

		// cmd+v handed through by the terminal is the paste it meant. It is
		// restated as one — the clipboard read off the render path — and the
		// paste lands wherever typing goes, exactly as a translated cmd+v
		// would have.
		if isPasteChord(msg) {
			return m, pasteFromClipboard()
		}

		// The resume picker takes every key while it is open: it is a look
		// through what could be continued, and its keys are the filter's.
		if m.resume != nil {
			return m, m.resumeKey(msg)
		}

		// Reading the transcript takes every key: the reader is looking, not
		// typing, so none of them are the shell's. What is not a motion is
		// swallowed, the way a half-finished gg swallows.
		if m.scroll != nil {
			return m, m.scrollKey(msg)
		}

		// A focused shell takes every keystroke: ctrl+c, ctrl+o, all of it
		// belongs to whatever is running in the shell, not to scrn. The one
		// way out is the prefix, which was taken above.
		if t := m.focused(); t != nil {
			m.daemon.key(t.pid, keyEvent(msg))
			return m, nil
		}

		// The filter takes every key while it is being typed, so a repository
		// called "scrn" can be typed without s opening a shell halfway through.
		if m.typing {
			return m, m.filterKey(msg)
		}

		// Replacing the daemon ends the work it is holding, so it takes a
		// second key like any other kill.
		if m.pendingReplace {
			m.pendingReplace = false
			switch msg.String() {
			case "R", "y", "enter":
				// The bridge notices the server going and says so; clearing
				// here as well just spares the window a beat of stale rows.
				m.daemon.replace()
				m.terms, m.focus, m.lastFocus = map[int]*remoteTerm{}, 0, 0
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
			m.showHelp = true
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
		case "down", "j", "tab":
			return m, m.move(1)
		case "up", "k", "shift+tab":
			return m, m.move(-1)
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
			return m, tea.Quit
		}
	}
	return m, nil
}

// The modes the keys can be in, named for what they are aimed at. The foot
// wears the current one as a chip, the way vim says INSERT.
const (
	modeNavigate = "navigate"
	modeProc     = "proc"
	modePrefix   = "prefix"
)

// mode is where the keys are going right now: held by the prefix, into a
// process — a focused shell, or its transcript being read — or at the list.
func (m model) mode() string {
	switch {
	case m.pendingPrefix:
		return modePrefix
	case m.focused() != nil, m.scroll != nil:
		return modeProc
	}
	return modeNavigate
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
		// Abandoning the look is not acting on anything, so it puts things
		// back as they were: the keys in the shell they came from, or the
		// cursor on the row it left.
		m.typing = false
		m.setFilter("")
		if t := m.terms[m.filterFromFocus]; t != nil {
			m.attachTo(t)
			return nil
		}
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
			if strings.Contains(strings.ToLower(n.Command), f) {
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
// where the keys are, wrapping. Going to an agent scrn holds means stepping
// into its shell; one it can only watch gets the cursor instead, which is as
// far as enter could take it either.
func (m *model) jumpWaiting() tea.Cmd {
	// From the filter, the jump is the end of looking: the rows while typing
	// are the query's answers — places alone until a query lands — and the
	// waiting agent lives in the whole list, which the chord means to reach
	// from anywhere.
	if m.typing {
		m.typing = false
		m.setFilter("")
	}
	at := m.cursor
	if t := m.focused(); t != nil {
		for i, r := range m.rows {
			if r.kind == rowProc && m.owningTerm(r.node.PID) == t {
				at = i
				break
			}
		}
	}
	for step := 1; step <= len(m.rows); step++ {
		i := (at + step) % len(m.rows)
		r := m.rows[i]
		if m.awaiting(r) == nil {
			continue
		}
		if t := m.owningTerm(r.node.PID); t != nil {
			m.attachTo(t)
			return nil
		}
		m.scroll = nil
		m.typing = false // the cursor is the answer now, not the query
		m.setFocus(0)
		m.cursor = i
		m.scrollToCursor()
		return m.detailCmd()
	}
	m.status, m.statusErr = "no agent is waiting", false
	return nil
}

// toggleFocus steps between the shell being viewed and the one viewed before
// it: from inside a shell it is the other of the pair, from the navigator it
// is back into the one just left.
func (m *model) toggleFocus() tea.Cmd {
	t := m.terms[m.lastFocus]
	if t == nil {
		m.status, m.statusErr = "no shell to step back into", false
		return nil
	}
	m.attachTo(t)
	return nil
}

// setFocus moves the keys, remembering the shell they leave so the toggle
// can step back to it.
func (m *model) setFocus(pid int) {
	if pid != 0 {
		// The pane is the shell's now, wherever the keys came from; a picker
		// left open would be standing behind a screen it no longer draws.
		m.resume = nil
	}
	if m.focus != 0 && m.focus != pid {
		m.lastFocus = m.focus
	}
	m.focus = pid
}

// attachTo steps into a shell from anywhere: the keys go to the shell, the
// cursor to its row, and a filter that led here is finished, like enter's.
func (m *model) attachTo(t *remoteTerm) {
	m.scroll = nil
	m.typing = false
	m.setFocus(t.pid)
	m.setFilter("")
	for i, r := range m.rows {
		if r.kind == rowProc && m.owningTerm(r.node.PID) == t {
			m.cursor = i
			m.scrollToCursor()
			break
		}
	}
	m.daemon.attach(t.pid, m.detailWidth(), m.paneHeight())
}

// openFilter starts typing a filter. The list becomes every project straight
// away, before a single character is typed: half of looking one up is
// remembering which ones there are. As a chord it can arrive from inside a
// shell or a transcript, which it leaves the way jumpWaiting does.
func (m *model) openFilter() tea.Cmd {
	m.filterFromFocus = m.focus
	m.filterFrom = ""
	if r, ok := m.selected(); ok {
		m.filterFrom = detailKey(r)
	}
	m.setFocus(0)
	m.scroll = nil
	m.resume = nil // one look at a time; the filter is the look now
	m.typing = true
	m.rebuild()
	m.cursor = 0
	m.scrollToCursor()
	return m.detailCmd()
}

// attachStep attaches to the next or previous process that can be attached
// to, in row order, wrapping at the ends. From inside a shell it is how you
// move to the neighboring one without a trip through the navigator; from the
// navigator it steps from the cursor. Attaching is acting on what was found,
// so a filter that led here is finished, the way enter's is.
func (m *model) attachStep(delta int) tea.Cmd {
	at := m.cursor
	if t := m.focused(); t != nil {
		for i, r := range m.rows {
			if r.kind == rowProc && m.owningTerm(r.node.PID) == t {
				at = i
				break
			}
		}
	}
	for step := 1; step <= len(m.rows); step++ {
		i := (at + delta*step + len(m.rows)*step) % len(m.rows)
		r := m.rows[i]
		if r.kind != rowProc {
			continue
		}
		t := m.owningTerm(r.node.PID)
		if t == nil || t.pid == m.focus {
			continue
		}
		m.attachTo(t)
		return nil
	}
	m.status, m.statusErr = "nothing else to attach to", false
	return nil
}

// startHere opens a shell — or handed a command, an agent — where the keys
// are: beside the focused shell when one is taking them, else at the selected
// row, which is what the bare letter does.
func (m *model) startHere(command string) tea.Cmd {
	t := m.focused()
	if t == nil {
		return m.start(command)
	}
	if m.daemon == nil {
		m.status, m.statusErr = "no server to hold it: "+m.daemonErr, true
		return nil
	}
	m.daemon.open(t.dir, command, "", m.detailWidth(), m.paneHeight())
	return nil
}

// runHere runs the plan of the place the keys are in: the focused shell's
// place when one is taking them, else the selected row's.
func (m *model) runHere() tea.Cmd {
	t := m.focused()
	if t == nil {
		return m.run()
	}
	p, ok := m.placeAt(t.dir)
	if !ok {
		m.status, m.statusErr = "no project holds "+t.dir, true
		return nil
	}
	return m.runPlace(p)
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

// focused returns the terminal taking keystrokes, if one is.
func (m model) focused() *remoteTerm {
	if m.focus == 0 {
		return nil
	}
	return m.terms[m.focus]
}

// paneTerm is the shell the pane should be showing: the focused one, or the
// one belonging to the row under the cursor.
//
// A folded run is rarely a shell itself — the row is named for what the shell
// started — so the run is walked for the shell scrn holds in it. That shell's
// pane is where the thing the row is named for is drawing.
func (m model) paneTerm() *remoteTerm {
	if t := m.focused(); t != nil {
		return t
	}
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
	if m.daemon == nil {
		m.status, m.statusErr = "no server to hold it: "+m.daemonErr, true
		return nil
	}
	m.daemon.open(m.shellDir(r), command, "", m.detailWidth(), m.paneHeight())
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
		m.setFocus(t.pid)
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

// retryConnect schedules another attempt to reach the daemon, waiting twice
// as long as the last one up to a cap. The wait is reset by a daemon that
// talks, so a normal loss is recovered in well under a second and only a
// daemon that keeps failing is given room.
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
	if len(m.terms) == 0 && m.daemonErr == "" {
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
	if m.daemon == nil {
		m.status, m.statusErr = "no server to hold them: "+m.daemonErr, true
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
		m.daemon.open(p.Path, e.Run, e.Name, m.detailWidth(), m.paneHeight())
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
	hint := len(m.trimmedHint(m.height))
	if h := m.height - 2 - hint; h > 0 {
		return h
	}
	if h := m.height - 1 - hint; h > 0 {
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

// paneClick is one press, remembered long enough to recognize its double.
type paneClick struct {
	x, y int
	at   time.Time
}

// doubleClickWithin is how close together two presses on one cell have to
// land to be a double-click.
const doubleClickWithin = 400 * time.Millisecond

// paneDrag is the sweep: where the button went down, where it is now, and
// whether it has moved at all — a motionless press is still a click. The
// press is kept so a click can be delivered whole on release.
type paneDrag struct {
	sx, sy int
	x, y   int
	moved  bool
	press  *mousePress
}

// scrollView is a pane's transcript being read rather than followed: the
// lines that had scrolled away plus the screen as it stood when the reading
// began, and how far back up the reader has gone.
type scrollView struct {
	pid   int
	doc   []string // nil until the transcript arrives
	above int      // lines between the bottom of the viewport and the live tail

	// cur is the reader's own cursor: the line the motions move and the
	// mark is set at, drawn reversed. The viewport follows it.
	cur int

	// anchor is the line v marked, and -1 while nothing is selected. The
	// selection runs from it to the cursor, and y carries the span to the
	// system clipboard.
	anchor int
}

// wheelLines is how many lines one wheel notch moves the transcript, the same
// distance the notch would have scrolled anywhere else.
const wheelLines = 3

// wheelDelta is how far a mouse event asks the transcript to move: up for
// positive, and nothing for anything that is not a turn of the wheel.
func wheelDelta(msg tea.MouseMsg) int {
	wheel, ok := msg.(tea.MouseWheelMsg)
	if !ok {
		return 0
	}
	switch wheel.Button {
	case tea.MouseWheelUp:
		return wheelLines
	case tea.MouseWheelDown:
		return -wheelLines
	}
	return 0
}

// scrollBy moves the reading position, up for positive. Falling below the
// live tail ends the reading: rolling past the bottom is how a wheel says
// back to now.
func (m *model) scrollBy(delta int) {
	s := m.scroll
	if s == nil || delta == 0 {
		return
	}
	s.above += delta
	if s.above <= 0 {
		// Rolling past the bottom is back to now — unless a selection is
		// being made, which holds the reader at the live tail instead.
		if s.anchor >= 0 {
			s.above = 0
			m.clampCur()
			return
		}
		m.scroll = nil
		return
	}
	if max := m.scrollMax(); s.doc != nil && s.above > max {
		s.above = max
	}
	m.clampCur()
}

// scrollMax is as far up as the transcript goes: the lines that do not fit
// the pane.
func (m *model) scrollMax() int {
	if n := len(m.scroll.doc) - m.paneHeight(); n > 0 {
		return n
	}
	return 0
}

// openReader starts reading the pane's shell deliberately — the chord's way
// in, where the wheel's way needs a primary screen with lines above it. It
// works on the alternate screen too: the pager's screen becomes the
// document, which is exactly the page a selection wants.
func (m *model) openReader() tea.Cmd {
	t := m.paneTerm()
	if t == nil {
		m.status, m.statusErr = "nothing in the pane to read", false
		return nil
	}
	m.typing = false
	doc := strings.Split(t.screen, "\n")
	m.scroll = &scrollView{pid: t.pid, doc: doc, cur: len(doc) - 1, anchor: -1}
	if !t.alt {
		// The transcript above joins the document when it arrives; the
		// alternate screen has none, and is whole already.
		m.daemon.history(t.pid)
	}
	return nil
}

// selection is the span between the mark and the cursor, inclusive, in
// document order.
func (s *scrollView) selection(cur int) (int, int) {
	lo, hi := s.anchor, cur
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi
}

// curBy moves the reader's cursor, the viewport following. Moving down off
// the last line is back to live — unless a selection is being made, which
// pins the reader instead.
func (m *model) curBy(delta int) {
	s := m.scroll
	if s == nil || len(s.doc) == 0 || delta == 0 {
		return
	}
	if delta > 0 && s.cur >= len(s.doc)-1 && s.above <= 0 && s.anchor < 0 {
		m.scroll = nil
		return
	}
	s.cur = min(max(s.cur+delta, 0), len(s.doc)-1)
	m.followCur()
}

// followCur slides the viewport the least amount that keeps the cursor on
// screen, the discipline the navigator's own scrolling keeps.
func (m *model) followCur() {
	s := m.scroll
	page := m.paneHeight()
	bottom := len(s.doc) - 1 - s.above
	if s.cur > bottom {
		s.above = len(s.doc) - 1 - s.cur
	}
	if top := bottom - page + 1; s.cur < top {
		s.above = min(len(s.doc)-page-s.cur, m.scrollMax())
	}
	if s.above < 0 {
		s.above = 0
	}
}

// clampCur pulls the cursor back into the viewport after the viewport moved
// on its own — the wheel, a page — so the cursor is always a line on screen.
func (m *model) clampCur() {
	s := m.scroll
	if s == nil || len(s.doc) == 0 {
		return
	}
	bottom := max(len(s.doc)-1-s.above, 0)
	top := max(bottom-m.paneHeight()+1, 0)
	s.cur = min(max(s.cur, top), bottom)
}

// scrollKey is a keystroke while the transcript is being read: vim's motions
// moving the reader's own cursor, the mark and the yank at it, and the ways
// out.
func (m *model) scrollKey(msg tea.KeyPressMsg) tea.Cmd {
	s := m.scroll
	page := m.paneHeight()
	switch msg.String() {
	case "esc":
		// Layered, the way esc always is: first the selection, then the
		// reading.
		if s.anchor >= 0 {
			s.anchor = -1
			return nil
		}
		m.scroll = nil
	case "q":
		m.scroll = nil
	case "v":
		// v marks the cursor's line; moving extends the selection to
		// wherever the cursor goes. Pressed again, it marks afresh.
		s.anchor = s.cur
	case "y":
		if s.anchor < 0 {
			m.status, m.statusErr = "v marks before y copies", false
			return nil
		}
		lo, hi := s.selection(s.cur)
		lines := make([]string, 0, hi-lo+1)
		for _, row := range s.doc[lo : hi+1] {
			lines = append(lines, ansi.Strip(row))
		}
		m.scroll = nil // the yank is the end of the reading
		text := strings.Join(lines, "\n")
		return func() tea.Msg { return copiedMsg{n: len(lines), err: writeClipboard(text)} }
	case "up", "k":
		m.curBy(-1)
	case "down", "j":
		m.curBy(1)
	case "pgup", "ctrl+b":
		m.curBy(-page)
	case "pgdown", "ctrl+f":
		m.curBy(page)
	case "ctrl+u":
		m.curBy(-page / 2)
	case "ctrl+d":
		m.curBy(page / 2)
	case "g":
		m.curBy(-len(s.doc))
	case "G":
		// To the last line — the live tail — with the reading kept: the
		// bottom is where a mark on fresh output starts. Leaving stays
		// q's and esc's word.
		m.curBy(len(s.doc))
	}
	return nil
}

// navWheelRows is how many rows one wheel notch moves the navigator's
// cursor, the pace a notch moves anything else.
const navWheelRows = 3

// rowAt is the navigator row drawn on window line y, mirroring leftColumn:
// the masthead first, its blank while the window can spare one, then the
// list from the scrolled-to offset.
func (m model) rowAt(y int) (int, bool) {
	top := 1
	if m.height-2-len(m.trimmedHint(m.height)) > 0 {
		top++
	}
	i := m.offset + y - top
	if y < top || i >= len(m.rows) {
		return 0, false
	}
	return i, true
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
	// The daemon holding them is enough to know they exist; waiting for the
	// process scan as well would hold the search open for a poll longer, and
	// the daemon is the thing that was actually asked.
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
// the daemon is holding there — the shell counting before the scan has seen
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
// with work in them, or all behind the dot. A shell the daemon is holding
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
		if strings.Contains(strings.ToLower(n.Command), f) {
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
		if strings.Contains(strings.ToLower(sp.Name), f) ||
			strings.Contains(strings.ToLower(p.Name+"/"+sp.Name), f) ||
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
// command name is checked as well as what the agent advertises, because what
// it advertises can outlive its process and a reused pid would otherwise be
// dressed up as an agent.
func (m model) agentFor(r navRow) agent {
	if r.kind != rowProc {
		return nil
	}
	a, ok := m.agents[r.node.PID]
	if !ok || a.command() != r.node.Command {
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

// syncPreview keeps the daemon sending the screen the pane is showing. A
// window is attached to what it entered; the glance — the held shell under
// the cursor — has to be asked for too, or a shell this window never stepped
// into previews blank. What the pane stops showing is detached again.
func (m *model) syncPreview() {
	want := 0
	if m.focus == 0 {
		if t := m.paneTerm(); t != nil {
			want = t.pid
		}
	}
	if want == m.previewing {
		return
	}
	if m.previewing != 0 && m.previewing != m.focus {
		m.daemon.detach(m.previewing)
	}
	m.previewing = want
	if want != 0 {
		m.daemon.attach(want, m.detailWidth(), m.paneHeight())
	}
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

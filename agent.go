package main

import (
	"maps"
	"time"

	tea "charm.land/bubbletea/v2"
)

// An agent is an AI assistant working in a repository: a process like any
// other, plus what it advertises about itself. scrn reads agents through this
// seam — what any kind can say, not what Claude happens to write down — so
// that other models can be introduced as kinds of their own.

// agent is one running instance of some kind of agent.
type agent interface {
	// command is the process name that runs this kind of agent. It is the
	// guard against a reused pid: what an agent advertises can outlive its
	// process, so nothing is believed unless the process table agrees.
	command() string

	// id names the conversation the instance is carrying — for Claude, its
	// session id. It is what keeps a conversation that is running from also
	// being offered as one at rest.
	id() string

	// working reports the instance is doing something. Not working means
	// waiting on its user, which is the state worth being taken to.
	working() bool

	// blocked reports the instance is stopped mid-turn on a specific ask —
	// a permission prompt, a question — and names it. Distinct from merely
	// not working: an idle instance's turn is over and it can wait, but a
	// blocked one is holding unfinished work until it is answered.
	blocked() (string, bool)

	// waitingFor is how long the instance has been waiting on its user, so
	// the one waiting longest — the answer owed first — can be found.
	waitingFor() time.Duration

	// describe is the detail pane's description of the instance. It may read
	// deeper sources than the scan does, so it runs off the render path, for
	// the selected row only.
	describe() []field
}

// agentKind is one kind of agent scrn knows: how to spot its processes, what
// to run to start one, and how to read what its instances say.
type agentKind struct {
	command string // the process command that marks a row as this kind
	run     string // what starting one runs; the a key uses the first kind's
	scan    func() map[int]agent

	// suspended lists the kind's conversations at rest in the given
	// directories — had there once, with no live instance carrying them.
	// live is the ids of the conversations the model believes are running.
	suspended func(dirs []string, live map[string]bool) []conversation

	// resume is the command that picks one of them back up.
	resume func(id string) string
}

// conversation is a talk an agent had in a directory and could pick back up:
// its transcript is on disk, and no live instance is carrying it.
type conversation struct {
	ID      string
	Dir     string    // where it was had, which is where resuming belongs
	When    time.Time // when it last moved
	Branch  string
	Prompt  string // the last thing its user asked of it
	Summary string // what it said it was doing, when it said
}

// agentKinds is every kind of agent scrn knows how to read.
var agentKinds = []agentKind{claudeKind}

// agentPoll is how often the agents' own state is re-read. It is file reads,
// not process scans, which is what lets it run this much faster than lsof.
const agentPoll = 150 * time.Millisecond

// agentTickMsg says it is time to re-read what the agents advertise.
type agentTickMsg struct{}

func agentTick() tea.Cmd {
	return tea.Tick(agentPoll, func(time.Time) tea.Msg { return agentTickMsg{} })
}

// agentsMsg carries every live agent instance, keyed by pid.
type agentsMsg struct {
	agents map[int]agent
}

// scanAgents reads what every kind's instances say about themselves.
func scanAgents() tea.Msg {
	agents := map[int]agent{}
	for _, k := range agentKinds {
		maps.Copy(agents, k.scan())
	}
	return agentsMsg{agents: agents}
}

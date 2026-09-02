package main

import (
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// An agent is an AI assistant working in a repository: a process like any
// other, plus what it advertises about itself. scrn reads agents through this
// seam — what any kind can say, not what Claude happens to write down — so
// that other tools slot in as kinds of their own.
//
// Kinds are unequal on purpose. Claude Code advertises its status and keeps
// transcripts worth resuming; an ollama REPL advertises nothing and keeps
// nothing. A kind says what it can do by which hooks it fills in, and every
// caller degrades gracefully around the holes: a kind without a scan is a
// process like any other, a kind without suspended conversations simply
// offers none to the picker.

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

// runs reports whether a process is an instance of an agent: the kind's own
// command, or an interpreter running a script that is named for it. Claude
// Code installed through npm is a node running claude-code/cli.js, and a
// row that says node is still a claude — what the session file advertises
// is believed when the process table can agree with it either way.
func runs(a agent, n *ProcNode) bool {
	name := a.command()
	if n.Command == name {
		return true
	}
	fields := strings.Fields(n.Argv)
	if len(fields) < 2 || !interpreters[filepath.Base(fields[0])] {
		return false
	}
	for seg := range strings.SplitSeq(filepath.ToSlash(fields[1]), "/") {
		if seg == name || strings.HasPrefix(seg, name+"-") {
			return true
		}
	}
	return false
}

// agentKind is one kind of agent scrn knows: how to spot its processes, what
// to run to start one, and — where the kind has anything to say — how to
// read what its instances advertise and pick their conversations back up.
type agentKind struct {
	name    string // what the config calls it
	command string // the process command that marks a row as this kind

	// run is the command that starts an instance, resolved when asked: a
	// kind may have to look around first — ollama, for which model. The
	// config's agentRuns override is applied outside, in startAgent.
	run func() string

	// scan reads what the kind's live instances advertise, keyed by pid.
	// nil: instances advertise nothing, and their rows are plain processes.
	scan func() map[int]agent

	// suspended lists the kind's conversations at rest in the given
	// directories — had there once, with no live instance carrying them.
	// live is the ids of the conversations the model believes are running.
	// nil: conversations do not survive their instances.
	suspended func(dirs []string, live map[string]bool) []conversation

	// resume is the command that picks one of them back up. nil when
	// suspended is.
	resume func(id string) string
}

// conversation is a talk an agent had in a directory and could pick back up:
// its transcript is on disk, and no live instance is carrying it.
type conversation struct {
	Kind    string // the agent kind that had it, and can resume it
	ID      string
	Dir     string    // where it was had, which is where resuming belongs
	When    time.Time // when it last moved
	Branch  string
	Prompt  string // the last thing its user asked of it
	Summary string // what it said it was doing, when it said
}

// agentKinds is every kind of agent scrn knows how to read. The first is the
// default the a key starts when the config names none.
var agentKinds = []agentKind{claudeKind, ollamaKind}

// The config's say over the kinds: which one a starts, and what starting
// one runs when the kind's own answer is not the wanted one. Applied at
// startup beside the navigator's width.
var (
	defaultAgent = ""
	agentRuns    map[string]string
)

// applyAgentConfig records the config's agent choices.
func applyAgentConfig(agent string, runs map[string]string) {
	defaultAgent, agentRuns = agent, runs
}

// defaultKind is the kind the a key starts: the config's, or the first one.
// A name the registry does not know falls back rather than failing — the
// keystroke should start something, and the status will say what it started.
func defaultKind() agentKind {
	if k, ok := kindNamed(defaultAgent); ok {
		return k
	}
	return agentKinds[0]
}

// kindNamed finds a kind by the name the config uses.
func kindNamed(name string) (agentKind, bool) {
	for _, k := range agentKinds {
		if k.name == name && name != "" {
			return k, true
		}
	}
	return agentKind{}, false
}

// startAgent is the command the a key runs: the config's override for the
// default kind, or the kind's own resolution.
func startAgent() string {
	k := defaultKind()
	if run := agentRuns[k.name]; run != "" {
		return run
	}
	return k.run()
}

// suspendedConversations is every kind's conversations at rest under the
// given directories, newest first across all of them. Each is stamped with
// the kind that had it, which is the kind that knows how to resume it.
func suspendedConversations(dirs []string, live map[string]bool) []conversation {
	var out []conversation
	for _, k := range agentKinds {
		if k.suspended == nil {
			continue
		}
		found := k.suspended(dirs, live)
		for i := range found {
			found[i].Kind = k.name
		}
		out = append(out, found...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].When.Equal(out[j].When) {
			return out[i].When.After(out[j].When)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// resumeCommand is what continuing a conversation runs, by the kind that
// had it. A conversation whose kind has gone from the registry has nothing
// to run, which the empty answer says.
func resumeCommand(c conversation) string {
	k, ok := kindNamed(c.Kind)
	if !ok || k.resume == nil {
		return ""
	}
	return k.resume(c.ID)
}

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

// scanAgents reads what every kind's instances say about themselves. A kind
// with no scan has nothing to say, which is not the same as nothing running.
func scanAgents() tea.Msg {
	agents := map[int]agent{}
	for _, k := range agentKinds {
		if k.scan != nil {
			maps.Copy(agents, k.scan())
		}
	}
	return agentsMsg{agents: agents}
}

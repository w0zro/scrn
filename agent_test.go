package main

import (
	"testing"
	"time"
)

// withKinds swaps the kind registry and the config's say for one test.
func withKinds(t *testing.T, kinds []agentKind, agent string, runs map[string]string) {
	t.Helper()
	oldKinds, oldAgent, oldRuns := agentKinds, defaultAgent, agentRuns
	agentKinds = kinds
	applyAgentConfig(agent, runs)
	t.Cleanup(func() {
		agentKinds = oldKinds
		applyAgentConfig(oldAgent, oldRuns)
	})
}

func TestTheConfigPicksTheKindTheAKeyStarts(t *testing.T) {
	kinds := []agentKind{
		{name: "claude", run: func() string { return "claude" }},
		{name: "ollama", run: func() string { return "ollama run something" }},
	}

	withKinds(t, kinds, "ollama", nil)
	if got := startAgent(); got != "ollama run something" {
		t.Errorf("startAgent = %q, want the config's kind", got)
	}

	// A name scrn does not know starts something rather than nothing.
	withKinds(t, kinds, "cursor", nil)
	if got := startAgent(); got != "claude" {
		t.Errorf("startAgent = %q, want the fallback to the first kind", got)
	}
}

func TestTheConfigOverridesWhatStartingAKindRuns(t *testing.T) {
	withKinds(t, []agentKind{
		{name: "ollama", run: func() string { return "ollama run guessed" }},
	}, "ollama", map[string]string{"ollama": "ollama run mistral"})

	if got := startAgent(); got != "ollama run mistral" {
		t.Errorf("startAgent = %q, want the override", got)
	}
}

func TestConversationsMergeAcrossKindsNewestFirst(t *testing.T) {
	now := time.Now()
	withKinds(t, []agentKind{
		{name: "one", suspended: func([]string, map[string]bool) []conversation {
			return []conversation{{ID: "old", When: now.Add(-2 * time.Hour)}}
		}},
		{name: "quiet"}, // keeps nothing; the picker just never hears from it
		{name: "two", suspended: func([]string, map[string]bool) []conversation {
			return []conversation{{ID: "new", When: now}}
		}},
	}, "", nil)

	got := suspendedConversations(nil, nil)
	if len(got) != 2 || got[0].ID != "new" || got[1].ID != "old" {
		t.Fatalf("conversations = %+v, want both kinds merged newest first", got)
	}
	if got[0].Kind != "two" || got[1].Kind != "one" {
		t.Errorf("kinds = %s, %s; want each stamped with who had it", got[0].Kind, got[1].Kind)
	}
}

func TestResumeGoesThroughTheKindThatHadIt(t *testing.T) {
	withKinds(t, []agentKind{
		{name: "one", resume: func(id string) string { return "one --resume " + id }},
		{name: "quiet"},
	}, "", nil)

	if got := resumeCommand(conversation{Kind: "one", ID: "x"}); got != "one --resume x" {
		t.Errorf("resume = %q, want the kind's own command", got)
	}
	if got := resumeCommand(conversation{Kind: "quiet", ID: "x"}); got != "" {
		t.Errorf("resume = %q, want nothing for a kind that cannot", got)
	}
	if got := resumeCommand(conversation{Kind: "gone", ID: "x"}); got != "" {
		t.Errorf("resume = %q, want nothing for a kind not registered", got)
	}
}

func TestAScanlessKindScansAsNothing(t *testing.T) {
	// ollama advertises nothing; its presence in the registry must not
	// break the poll that reads what the others say.
	withKinds(t, []agentKind{{name: "quiet"}}, "", nil)
	msg := scanAgents().(agentsMsg)
	if len(msg.agents) != 0 {
		t.Errorf("agents = %d, want a quiet registry to say nothing", len(msg.agents))
	}
}

func TestTheFirstOllamaModelIsRead(t *testing.T) {
	out := "NAME            ID          SIZE      MODIFIED\n" +
		"llama3.2:3b     a80c4f17acd5    2.0 GB    3 weeks ago\n" +
		"mistral:latest  61e88e884507    4.1 GB    5 weeks ago\n"
	if got := firstOllamaModel(out); got != "llama3.2:3b" {
		t.Errorf("model = %q, want the first listed", got)
	}
	if got := firstOllamaModel("NAME ID\n"); got != "" {
		t.Errorf("model = %q, want nothing from an empty list", got)
	}
	if got := firstOllamaModel(""); got != "" {
		t.Errorf("model = %q, want nothing from no answer", got)
	}
}

func TestAnInterpreterRunningTheAgentIsTheAgent(t *testing.T) {
	// Claude Code installed through npm is a node running claude-code's
	// cli.js. The row says node; the session file says claude; the process
	// table can agree with it by what the node was given to run.
	a := claudeSession{PID: 700, SessionID: "s"}
	cases := []struct {
		name    string
		proc    Proc
		isAgent bool
	}{
		{"the binary", Proc{Command: "claude", Argv: "claude"}, true},
		{"npm's node", Proc{Command: "node", Argv: "node /opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, true},
		{"a bin shim", Proc{Command: "node", Argv: "/usr/bin/node /home/me/.local/bin/claude --resume x"}, true},
		{"another node", Proc{Command: "node", Argv: "node server.js"}, false},
		{"a reused pid", Proc{Command: "vim", Argv: "vim claude.go"}, false},
		{"a bare node", Proc{Command: "node", Argv: "node"}, false},
	}
	for _, c := range cases {
		if got := runs(a, &ProcNode{Proc: c.proc}); got != c.isAgent {
			t.Errorf("%s: runs = %v, want %v", c.name, got, c.isAgent)
		}
	}
}

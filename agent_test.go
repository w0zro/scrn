package main

import (
	"os"
	"path/filepath"
	"strings"
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
	if got := startAgent(nil); got != "ollama run something" {
		t.Errorf("startAgent = %q, want the config's kind", got)
	}

	// A name conn does not know starts something rather than nothing.
	withKinds(t, kinds, "cursor", nil)
	if got := startAgent(nil); got != "claude" {
		t.Errorf("startAgent = %q, want the fallback to the first kind", got)
	}
}

// serverSaying is a runner standing in for a server whose agent option
// reads as answer: the show that asks for it gets that, and every other
// command — the set that chooses — is recorded and succeeds.
func serverSaying(answer string, asked *[][]string) runner {
	return func(args ...string) (string, error) {
		if asked != nil {
			*asked = append(*asked, args)
		}
		if len(args) > 0 && args[0] == "show" {
			return answer + "\n", nil
		}
		return "", nil
	}
}

func TestTheWindowsChoiceOfKindOutranksTheConfigs(t *testing.T) {
	kinds := []agentKind{
		{name: "claude", run: func() string { return "claude" }},
		{name: "ollama", run: func() string { return "ollama run something" }},
	}
	withKinds(t, kinds, "claude", map[string]string{"ollama": "ollama run mistral"})

	// The server says ollama: a starts it, through the config's override
	// for that kind, the same as if the config had named it.
	if got := startAgent(serverSaying("ollama", nil)); got != "ollama run mistral" {
		t.Errorf("startAgent = %q, want the kind the server was told, as the config runs it", got)
	}
	// The server says nothing, or something conn does not know: the
	// config's kind stands.
	for _, said := range []string{"", "cursor"} {
		if got := startAgent(serverSaying(said, nil)); got != "claude" {
			t.Errorf("server saying %q: startAgent = %q, want the config's kind", said, got)
		}
	}
	// No server at all is the config's answer too, not a crash.
	if got := startAgent(nil); got != "claude" {
		t.Errorf("startAgent = %q with no server, want the config's kind", got)
	}
}

func TestTheNextKindGoesAroundTheRegistry(t *testing.T) {
	kinds := []agentKind{{name: "claude"}, {name: "ollama"}}
	withKinds(t, kinds, "", nil)

	if got := nextKind(kinds[0]).name; got != "ollama" {
		t.Errorf("after claude = %q, want ollama", got)
	}
	if got := nextKind(kinds[1]).name; got != "claude" {
		t.Errorf("after ollama = %q, want claude, around the end", got)
	}
	if got := nextKind(agentKind{name: "gone"}).name; got != "claude" {
		t.Errorf("after an unregistered kind = %q, want the first", got)
	}
}

func TestChoosingAKindTellsTheServer(t *testing.T) {
	var asked [][]string
	if err := chooseKind(serverSaying("", &asked), agentKind{name: "ollama"}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("asked = %v, want one command", asked)
	}
	got := strings.Join(asked[0], " ")
	if !strings.HasPrefix(got, "set -g "+agentOption+" ollama") {
		t.Errorf("asked %q, want the option set to the kind", got)
	}
	if !strings.Contains(got, "refresh-client -S") {
		t.Errorf("asked %q, want the status line refreshed with it", got)
	}
}

func TestTheConfigOverridesWhatStartingAKindRuns(t *testing.T) {
	withKinds(t, []agentKind{
		{name: "ollama", run: func() string { return "ollama run guessed" }},
	}, "ollama", map[string]string{"ollama": "ollama run mistral"})

	if got := startAgent(nil); got != "ollama run mistral" {
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

func TestOllamasModelsAreReadOffTheDiskNewestFirst(t *testing.T) {
	// A pulled model is a manifest file under manifests/<registry>/
	// <namespace>/<model>/<tag>; the names are shortened the way ollama
	// list shortens them, and the newest manifest comes first, which is
	// the model most recently pulled or run.
	dir := t.TempDir()
	manifests := filepath.Join(dir, "manifests")
	write := func(rel string, when time.Time) {
		t.Helper()
		path := filepath.Join(manifests, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write("registry.ollama.ai/library/qwen2.5-coder/7b", now.Add(-2*time.Hour))
	write("registry.ollama.ai/library/llama3.2/latest", now)
	write("registry.ollama.ai/someone/finetune/v2", now.Add(-time.Hour))
	write("hub.example.com/library/other/1b", now.Add(-3*time.Hour))
	write("registry.ollama.ai/library/stray", now) // not a manifest: too shallow

	got := ollamaModelsOnDisk(dir)
	want := []string{"llama3.2:latest", "someone/finetune:v2", "qwen2.5-coder:7b", "hub.example.com/library/other:1b"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("models = %q, want %q", got, want)
	}

	// Nothing pulled, or no models directory at all, is nothing — not an
	// error, and not a crash.
	if got := ollamaModelsOnDisk(t.TempDir()); len(got) != 0 {
		t.Errorf("models = %q from an empty directory, want none", got)
	}
	if got := ollamaModelsOnDisk(""); len(got) != 0 {
		t.Errorf("models = %q from no directory, want none", got)
	}
}

func TestOllamaStartsTheNewestModelOnDiskWithoutTheDaemon(t *testing.T) {
	// The daemon is not asked when the disk answers: it is often not up
	// until something needs it, and asking it would name nothing exactly
	// when a session is starting fresh.
	dir := t.TempDir()
	path := filepath.Join(dir, "manifests", "registry.ollama.ai", "library", "qwen2.5-coder", "7b")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_MODELS", dir)
	if got := ollamaRun(); got != "ollama run qwen2.5-coder:7b" {
		t.Errorf("ollamaRun = %q, want the model on disk", got)
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

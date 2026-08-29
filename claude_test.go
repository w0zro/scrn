package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// claudeHome lays out a fake Claude Code state directory and points scrn at it.
func claudeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	for _, sub := range []string{"sessions", "projects"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeSession(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := writeFile(filepath.Join(dir, "sessions", name), body); err != nil {
		t.Fatal(err)
	}
}

// writeTranscript files a transcript the way Claude Code does, under the
// encoded working directory.
func writeTranscript(t *testing.T, dir, cwd, id string, records ...string) string {
	t.Helper()
	pdir := filepath.Join(dir, "projects", encodePath(cwd))
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pdir, id+".jsonl")
	if err := writeFile(path, strings.Join(records, "\n")+"\n"); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEncodePathMatchesClaudesLayout(t *testing.T) {
	cases := map[string]string{
		"/Users/w0zro/projects/w0zro/scrn":       "-Users-w0zro-projects-w0zro-scrn",
		"/Users/w0zro/projects/SkellyLabs, Inc.": "-Users-w0zro-projects-SkellyLabs--Inc-",
		"/private/tmp/claude-501/x":              "-private-tmp-claude-501-x",
	}
	for cwd, want := range cases {
		if got := encodePath(cwd); got != want {
			t.Errorf("encodePath(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestSessionsAreKeyedByPID(t *testing.T) {
	dir := claudeHome(t)
	writeSession(t, dir, "4242.json", `{"pid":4242,"sessionId":"abc","cwd":"/p/scrn",
		"name":"scrn-1f","status":"busy","statusUpdatedAt":`+recentMillis(90*time.Second)+`}`)

	got := claudeSessions()
	s, ok := got[4242]
	if !ok {
		t.Fatalf("sessions = %v, want one keyed by pid 4242", got)
	}
	if s.Name != "scrn-1f" || s.Status != "busy" || s.SessionID != "abc" {
		t.Errorf("session = %+v, want the name, status and id read", s)
	}
	if s.StatusFor < time.Minute || s.StatusFor > 5*time.Minute {
		t.Errorf("StatusFor = %v, want roughly 90s", s.StatusFor)
	}
}

func TestKeyFilesAreNotSessions(t *testing.T) {
	// The sessions directory also holds per-session key files.
	dir := claudeHome(t)
	writeSession(t, dir, "4242.deadbeef.key", "not json")
	writeSession(t, dir, "notapid.json", `{"pid":7,"sessionId":"x"}`)

	if got := claudeSessions(); len(got) != 0 {
		t.Errorf("sessions = %v, want only <pid>.json files read", got)
	}
}

func TestMissingClaudeIsNotAnError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
	if got := claudeSessions(); len(got) != 0 {
		t.Errorf("sessions = %v, want nothing when Claude Code is not installed", got)
	}
}

func TestMalformedSessionFileIsSkipped(t *testing.T) {
	dir := claudeHome(t)
	writeSession(t, dir, "1.json", "{ this is not json")
	writeSession(t, dir, "2.json", `{"pid":2,"name":"fine"}`)

	got := claudeSessions()
	if len(got) != 1 || got[2].Name != "fine" {
		t.Errorf("sessions = %v, want the sound file read and the broken one skipped", got)
	}
}

const (
	asstRec = `{"type":"assistant","gitBranch":"main","message":{"model":"claude-opus-5",` +
		`"usage":{"input_tokens":12,"cache_read_input_tokens":177000,"cache_creation_input_tokens":652}}}`
	userRec = `{"type":"user","message":{"content":"make the spinner red"}}`
)

func TestTranscriptFillsInWhatTheSessionFileCannot(t *testing.T) {
	dir := claudeHome(t)
	writeTranscript(t, dir, "/p/scrn", "abc", userRec, asstRec)

	s := claudeSession{SessionID: "abc", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)

	if s.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want the model of the last turn", s.Model)
	}
	if s.Branch != "main" {
		t.Errorf("Branch = %q, want the branch the session is on", s.Branch)
	}
	if s.Prompt != "make the spinner red" {
		t.Errorf("Prompt = %q, want the last thing the user asked", s.Prompt)
	}
	if s.Context != 177664 {
		t.Errorf("Context = %d, want the whole context of the last request", s.Context)
	}
}

func TestTranscriptIsFoundAfterARename(t *testing.T) {
	// The transcript stays filed under the directory's old name.
	dir := claudeHome(t)
	writeTranscript(t, dir, "/p/old-name", "abc", asstRec)

	s := claudeSession{SessionID: "abc", Cwd: "/p/new-name"}
	if got := transcriptPath(s); got == "" {
		t.Error("a renamed project should still find its transcript by session id")
	}
}

func TestOnlyWhatTheUserTypedCountsAsAPrompt(t *testing.T) {
	dir := claudeHome(t)
	writeTranscript(t, dir, "/p/scrn", "abc",
		`{"type":"user","message":{"content":"the real question"}}`,
		// A tool result: content is a list, not a string.
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		// Harness chatter the user never typed.
		`{"type":"user","message":{"content":"<system-reminder>be good</system-reminder>"}}`,
		`{"type":"user","isMeta":true,"message":{"content":"session resumed"}}`,
		// A subagent's conversation, not this session's.
		`{"type":"user","isSidechain":true,"message":{"content":"subagent instructions"}}`,
	)

	s := claudeSession{SessionID: "abc", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)

	if s.Prompt != "the real question" {
		t.Errorf("Prompt = %q, want only what the user actually typed", s.Prompt)
	}
}

func TestALongPromptIsCutDown(t *testing.T) {
	dir := claudeHome(t)
	long := strings.Repeat("word ", 200)
	writeTranscript(t, dir, "/p/scrn", "abc",
		`{"type":"user","message":{"content":"`+long+`"}}`)

	s := claudeSession{SessionID: "abc", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)

	if n := len([]rune(s.Prompt)); n > promptLimit+1 {
		t.Errorf("prompt is %d runes, want it cut to about %d", n, promptLimit)
	}
	if !strings.HasSuffix(s.Prompt, "…") {
		t.Errorf("Prompt = %q, want the cut marked", s.Prompt)
	}
}

func TestOnlyTheTailOfAHugeTranscriptIsRead(t *testing.T) {
	// The prompt is at the very start, past the tail window, so it should not
	// be found — but the recent records still must parse cleanly.
	dir := claudeHome(t)
	filler := `{"type":"assistant","message":{"content":"` + strings.Repeat("x", 4000) + `"}}`
	records := []string{`{"type":"user","message":{"content":"ancient history"}}`}
	for i := 0; i < transcriptTail/4000+10; i++ {
		records = append(records, filler)
	}
	records = append(records, asstRec)
	writeTranscript(t, dir, "/p/scrn", "abc", records...)

	s := claudeSession{SessionID: "abc", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)

	if s.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want the tail parsed", s.Model)
	}
	if s.Prompt != "" {
		t.Errorf("Prompt = %q, want nothing found beyond the tail window", s.Prompt)
	}
}

func TestAPartialFirstLineIsDiscarded(t *testing.T) {
	dir := claudeHome(t)
	path := writeTranscript(t, dir, "/p/scrn", "abc", asstRec)

	// Read a window that starts mid-record; the broken line must not be used.
	lines, err := tailLines(path, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range lines {
		if strings.HasPrefix(string(ln), "{") {
			t.Errorf("kept a line the seek landed inside: %q", ln)
		}
	}
}

func TestMissingTranscriptLeavesTheSessionAlone(t *testing.T) {
	claudeHome(t)
	s := claudeSession{SessionID: "gone", Cwd: "/p/scrn", Name: "scrn-1f"}
	readTranscript(transcriptPath(s), &s)

	if s.Name != "scrn-1f" || s.Model != "" {
		t.Errorf("session = %+v, want it untouched when there is no transcript", s)
	}
}

func TestClaudeFieldsLeadWithTheSession(t *testing.T) {
	fs := claudeFields(claudeSession{
		Name: "scrn-1f", Status: "busy", StatusFor: 3 * time.Minute,
		Summary: "building scrn", Model: "claude-opus-5", Context: 177664,
		Prompt: "make the spinner red",
		Branch: "main", SessionID: "abc",
	})

	// What it is, then what it is doing, then what it is doing it with.
	pairs := pairsOf(fs)
	want := []string{"session", "status", "branch", "summary", "asked", "model", "context", "session id"}
	for i, label := range want {
		if i >= len(pairs) || pairs[i].label != label {
			t.Fatalf("field %d = %q, want %q\nall: %+v", i, labelAt(pairs, i), label, fs)
		}
	}
	if len(blocks(fs)) < 3 {
		t.Errorf("fields fall into %d groups, want them grouped rather than one list", len(blocks(fs)))
	}
	if v, _ := fieldValue(fs, "status"); v != "busy  (3m)" {
		t.Errorf("status = %q, want it to say how long it has been that way", v)
	}
	if v, _ := fieldValue(fs, "context"); v != "177k tokens" {
		t.Errorf("context = %q, want a readable token count", v)
	}
}

func TestClaudeFieldsSkipWhatIsUnknown(t *testing.T) {
	fs := pairsOf(claudeFields(claudeSession{Name: "scrn-1f", Status: "idle"}))
	for _, f := range fs {
		if f.value == "" {
			t.Errorf("field %q has no value; empty fields should be left out", f.label)
		}
	}
	if len(fs) != 2 {
		t.Errorf("fields = %+v, want only the two that are known", fs)
	}
}

func TestShortDuration(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Second: "0s", 45 * time.Second: "45s", 3 * time.Minute: "3m",
		90 * time.Minute: "1h30m", 2 * time.Hour: "2h",
	}
	for d, want := range cases {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestShortTokens(t *testing.T) {
	cases := map[int]string{950: "950", 177664: "177k", 2_400_000: "2.4M"}
	for n, want := range cases {
		if got := shortTokens(n); got != want {
			t.Errorf("shortTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func labelAt(fs []field, i int) string {
	if i < len(fs) {
		return fs[i].label
	}
	return "<missing>"
}

func recentMillis(ago time.Duration) string {
	return strconv.FormatInt(time.Now().Add(-ago).UnixMilli(), 10)
}

func TestTheSessionSaysWhatItIsDoing(t *testing.T) {
	dir := claudeHome(t)
	writeTranscript(t, dir, "/p/scrn", "abc",
		`{"type":"system","subtype":"away_summary","content":"an older summary"}`,
		`{"type":"system","subtype":"compact_boundary","content":"not a summary"}`,
		`{"type":"system","subtype":"away_summary","content":"We're building scrn.  Just committed."}`,
		asstRec)

	s := claudeSession{SessionID: "abc", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)

	if s.Summary != "We're building scrn. Just committed." {
		t.Errorf("Summary = %q, want the most recent one, flattened onto a line", s.Summary)
	}
}

func TestTheRecordedLastPromptWins(t *testing.T) {
	// Claude Code records the last prompt itself; that beats reading back
	// through the conversation for something that looks like one.
	dir := claudeHome(t)
	writeTranscript(t, dir, "/p/scrn", "abc",
		`{"type":"user","message":{"content":"an older question"}}`,
		`{"type":"last-prompt","lastPrompt":"the recorded one"}`,
		asstRec)

	s := claudeSession{SessionID: "abc", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)

	if s.Prompt != "the recorded one" {
		t.Errorf("Prompt = %q, want the recorded last prompt", s.Prompt)
	}
}

func TestReadingBackIsStillTheFallbackForAPrompt(t *testing.T) {
	dir := claudeHome(t)
	writeTranscript(t, dir, "/p/scrn", "abc",
		`{"type":"user","message":{"content":"no record of this one"}}`, asstRec)

	s := claudeSession{SessionID: "abc", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)

	if s.Prompt != "no record of this one" {
		t.Errorf("Prompt = %q, want the conversation read back when nothing was recorded", s.Prompt)
	}
}

func TestALongSummaryIsCutDown(t *testing.T) {
	dir := claudeHome(t)
	writeTranscript(t, dir, "/p/scrn", "abc",
		`{"type":"system","subtype":"away_summary","content":"`+strings.Repeat("word ", 200)+`"}`)

	s := claudeSession{SessionID: "abc", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)

	if n := len([]rune(s.Summary)); n > summaryLimit+1 {
		t.Errorf("summary is %d runes, want it cut to about %d", n, summaryLimit)
	}
}

// --- subagents -----------------------------------------------------------

// withAgent writes a subagent the way Claude Code does: a transcript of its
// own and a small file saying what it was started to do.
func withAgent(t *testing.T, dir, cwd, session, id, desc, kind string, age time.Duration) {
	t.Helper()
	sub := filepath.Join(dir, "projects", encodePath(cwd), session, "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"agentType":%q,"description":%q,"toolUseId":%q,"spawnDepth":1}`,
		kind, desc, "toolu_"+id)
	if err := writeFile(filepath.Join(sub, "agent-"+id+".meta.json"), meta); err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(sub, "agent-"+id+".jsonl")
	if err := writeFile(body, "{}\n"); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(body, when, when); err != nil {
		t.Fatal(err)
	}
}

func agentsFor(t *testing.T, dir string, records ...string) []agentRun {
	t.Helper()
	writeTranscript(t, dir, "/p/scrn", "sess", records...)
	s := claudeSession{SessionID: "sess", Cwd: "/p/scrn"}
	readTranscript(transcriptPath(s), &s)
	return s.Agents
}

const (
	fgCall  = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_fg","name":"Agent","input":{"description":"a","subagent_type":"Explore"}}]}}`
	fgDone  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_fg"}]}}`
	bgCall  = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_bg","name":"Agent","input":{"description":"b","subagent_type":"general-purpose","run_in_background":true}}]}}`
	bgStart = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_bg"}]}}`
	bgDone  = `{"type":"queue-operation","content":"<task-notification><tool-use-id>toolu_bg</tool-use-id><status>completed</status></task-notification>"}`
)

func TestAWaitedForAgentIsRunningUntilItAnswers(t *testing.T) {
	dir := claudeHome(t)
	withAgent(t, dir, "/p/scrn", "sess", "fg", "a", "Explore", 0)

	if got := agentsFor(t, dir, fgCall); len(got) != 1 || got[0].Description != "a" {
		t.Errorf("agents = %+v, want the one still being waited for", got)
	}
	if got := agentsFor(t, dir, fgCall, fgDone); len(got) != 0 {
		t.Errorf("agents = %+v, want none once its result came back", got)
	}
}

func TestABackgroundAgentIsNotFinishedByBeingStarted(t *testing.T) {
	// It is answered the moment it starts, so its result says nothing about
	// whether it is done. Reading that as finished hid every one of them.
	dir := claudeHome(t)
	withAgent(t, dir, "/p/scrn", "sess", "bg", "b", "general-purpose", 0)

	if got := agentsFor(t, dir, bgCall, bgStart); len(got) != 1 || got[0].Description != "b" {
		t.Errorf("agents = %+v, want it still running after being launched", got)
	}
	if got := agentsFor(t, dir, bgCall, bgStart, bgDone); len(got) != 0 {
		t.Errorf("agents = %+v, want none once its finishing was recorded", got)
	}
}

func TestAnAgentThatStoppedWritingLongAgoIsNotRunning(t *testing.T) {
	// The transcript is only read in a window, so a finish old enough to have
	// fallen out of it would otherwise leave the agent listed for good.
	dir := claudeHome(t)
	withAgent(t, dir, "/p/scrn", "sess", "old", "c", "Explore", 2*agentStale)

	if got := agentsFor(t, dir, `{"type":"assistant","message":{"content":[]}}`); len(got) != 0 {
		t.Errorf("agents = %+v, want nothing that went quiet long ago", got)
	}
}

func TestASessionWithNoSubagentsSaysNothingAboutThem(t *testing.T) {
	dir := claudeHome(t)
	if got := agentsFor(t, dir, asstRec); len(got) != 0 {
		t.Errorf("agents = %+v, want none", got)
	}
	fs := claudeFields(claudeSession{Name: "x", Status: "idle"})
	for _, f := range fs {
		if f.label == "agents" {
			t.Error("a session with no subagents should not have a line about them")
		}
	}
}

func TestTheAgentsAreListedUnderOneLabel(t *testing.T) {
	fs := claudeFields(claudeSession{
		Name: "x",
		Agents: []agentRun{
			{Description: "first", Type: "Explore"},
			{Description: "second", Type: "general-purpose"},
		},
	})
	var labels, values []string
	for _, f := range pairsOf(fs) {
		if f.label == "agents" || (f.label == "" && strings.Contains(f.value, "second")) {
			labels = append(labels, f.label)
			values = append(values, f.value)
		}
	}
	if len(values) != 2 {
		t.Fatalf("values = %v, want both agents listed", values)
	}
	if labels[0] != "agents" || labels[1] != "" {
		t.Errorf("labels = %v, want the rest to line up under the first", labels)
	}
	if !strings.Contains(values[0], "first  (Explore)") {
		t.Errorf("value = %q, want the description and what kind it is", values[0])
	}
}

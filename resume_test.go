package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// agedTranscript files a transcript the way claude_test's helper does, then
// ages it to when — the recency the picker orders by.
func agedTranscript(t *testing.T, claude, dir, id string, when time.Time, records ...string) {
	t.Helper()
	path := writeTranscript(t, claude, dir, id, records...)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestSuspendedConversationsAreNewestFirst(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	now := time.Now()

	agedTranscript(t, claude, "/p/scrn", "aaaa-1111", now.Add(-2*time.Hour),
		`{"type":"last-prompt","lastPrompt":"fix the resize race","gitBranch":"main"}`)
	agedTranscript(t, claude, "/p/scrn", "bbbb-2222", now.Add(-time.Minute),
		`{"type":"last-prompt","lastPrompt":"polish the picker","gitBranch":"picker"}`,
		`{"type":"system","subtype":"away_summary","content":"laying out the pane"}`)

	got := claudeSuspended([]string{"/p/scrn"}, nil)
	if len(got) != 2 {
		t.Fatalf("listed %d conversations, want 2", len(got))
	}
	if got[0].ID != "bbbb-2222" || got[1].ID != "aaaa-1111" {
		t.Fatalf("order = %s, %s; want the newest first", got[0].ID, got[1].ID)
	}
	if got[0].Prompt != "polish the picker" || got[0].Branch != "picker" {
		t.Errorf("meta = %q on %q, want the prompt and the branch read back", got[0].Prompt, got[0].Branch)
	}
	if got[0].Summary != "laying out the pane" {
		t.Errorf("summary = %q, want the away summary read back", got[0].Summary)
	}
	if got[0].Dir != "/p/scrn" {
		t.Errorf("dir = %q, want the directory it was filed under", got[0].Dir)
	}
}

func TestARunningConversationIsNotSuspended(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)

	agedTranscript(t, claude, "/p/scrn", "aaaa-1111", time.Now(),
		`{"type":"last-prompt","lastPrompt":"still going"}`)

	got := claudeSuspended([]string{"/p/scrn"}, map[string]bool{"aaaa-1111": true})
	if len(got) != 0 {
		t.Fatalf("listed %d conversations, want the live one excluded", len(got))
	}
}

func TestOnlyIdShapedFilesAreConversations(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)

	agedTranscript(t, claude, "/p/scrn", "aaaa-1111", time.Now(),
		`{"type":"last-prompt","lastPrompt":"the real one"}`)
	pdir := filepath.Join(claude, "projects", encodePath("/p/scrn"))
	// A name that is not an id would end up on a shell command line; it is
	// not a session, whatever it holds.
	for _, name := range []string{"notes.txt", "evil;rm.jsonl", "aaaa-1111"} {
		if err := os.WriteFile(filepath.Join(pdir, name), []byte("{}"), 0o600); err != nil {
			// aaaa-1111 also exists as a directory name in real layouts; a
			// file is close enough for the shape check.
			t.Fatal(err)
		}
	}

	got := claudeSuspended([]string{"/p/scrn"}, nil)
	if len(got) != 1 || got[0].ID != "aaaa-1111" {
		t.Fatalf("listed %+v, want only the id-shaped transcript", got)
	}
}

func TestAConversationIsTakenOnceAcrossDirs(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)

	// Claude's encoding is lossy: both of these file under -p-a-b.
	agedTranscript(t, claude, "/p/a/b", "aaaa-1111", time.Now(),
		`{"type":"last-prompt","lastPrompt":"once"}`)

	got := claudeSuspended([]string{"/p/a/b", "/p/a-b"}, nil)
	if len(got) != 1 {
		t.Fatalf("listed %d conversations, want the collision taken once", len(got))
	}
}

func TestConvoMetaFallsBackToAUserRecord(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)

	// An old transcript predates the last-prompt records; what the user
	// typed is still in it.
	agedTranscript(t, claude, "/p/scrn", "aaaa-1111", time.Now(),
		`{"type":"user","message":{"content":"make the tests pass"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`)

	got := claudeSuspended([]string{"/p/scrn"}, nil)
	if len(got) != 1 || got[0].Prompt != "make the tests pass" {
		t.Fatalf("prompt = %+v, want the user record read back", got)
	}
}

func TestClaudeResumeNamesTheSession(t *testing.T) {
	if got := claudeResume("aaaa-1111"); got != "claude --resume aaaa-1111" {
		t.Fatalf("resume command = %q", got)
	}
}

func TestShortAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		when time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "now"},
		{now.Add(-5 * time.Minute), "5m"},
		{now.Add(-3 * time.Hour), "3h"},
		{now.Add(-50 * time.Hour), "2d"},
		{now.Add(-40 * 24 * time.Hour), "5w"},
	}
	for _, c := range cases {
		if got := shortAge(c.when); got != c.want {
			t.Errorf("shortAge(%v ago) = %q, want %q", time.Since(c.when), got, c.want)
		}
	}
}

// pickerOn is a model standing on a repository with the picker open and a
// listing already landed.
func pickerOn(convos ...conversation) model {
	m := withProcs(96, 14, []Project{{Name: "scrn", Path: "/p/scrn"}}, nil)
	for i := range convos {
		if convos[i].Kind == "" {
			convos[i].Kind = "claude" // stamped by the layer in earnest
		}
	}
	m.resume = &resumeView{place: Project{Name: "scrn", Path: "/p/scrn"}, loaded: true, convos: convos}
	return m
}

func TestAOpensThePickerOnThePlace(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
	m := withProcs(96, 14, []Project{{Name: "scrn", Path: "/p/scrn"}}, nil)

	next, cmd := m.Update(typed("A"))
	m = next.(model)
	if m.resume == nil || m.resume.place.Path != "/p/scrn" {
		t.Fatalf("picker = %+v, want it open on the selected place", m.resume)
	}
	if cmd == nil {
		t.Fatal("no listing was asked for")
	}
	msg, ok := cmd().(convosMsg)
	if !ok || msg.place != "/p/scrn" {
		t.Fatalf("listing = %+v, want convosMsg for the place", msg)
	}
	next, _ = m.Update(msg)
	m = next.(model)
	if !m.resume.loaded {
		t.Fatal("the landed listing did not mark the picker loaded")
	}
}

func TestPickerTypingNarrowsAndEnterContinues(t *testing.T) {
	m := pickerOn(
		conversation{ID: "aaaa-1111", Dir: "/p/scrn", Prompt: "fix the resize race"},
		conversation{ID: "bbbb-2222", Dir: "/p/scrn/docs", Prompt: "polish the site"},
	)
	m, asked := pipeDaemon(t, m)

	for _, k := range []string{"s", "i", "t", "e"} {
		m = press(m, k)
	}
	if got := m.resume.matches(); len(got) != 1 || got[0].ID != "bbbb-2222" {
		t.Fatalf("matches = %+v, want the query to narrow to the site work", got)
	}

	m = press(m, "enter")
	got := askedFor(t, asked)
	if got.Kind != kindOpen || got.Dir != "/p/scrn/docs" || got.Run != "claude --resume bbbb-2222" {
		t.Fatalf("asked %+v, want the conversation continued where it was had", got)
	}
	if m.resume != nil {
		t.Error("the picker is still open after acting; picking is the end of looking")
	}
}

func TestPickerEscReturnsToTheNavigator(t *testing.T) {
	m := pickerOn(conversation{ID: "aaaa-1111", Prompt: "anything"})
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if next.(model).resume != nil {
		t.Fatal("esc left the picker open")
	}
}

func TestPickerCursorMovesAndWraps(t *testing.T) {
	m := pickerOn(
		conversation{ID: "aaaa-1111", Prompt: "one"},
		conversation{ID: "bbbb-2222", Prompt: "two"},
	)
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = next.(model)
	if m.resume.cursor != 1 {
		t.Fatalf("cursor = %d after down, want 1", m.resume.cursor)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := next.(model).resume.cursor; got != 0 {
		t.Fatalf("cursor = %d, want the wrap back to the top", got)
	}
}

func TestAStaleListingIsDropped(t *testing.T) {
	m := pickerOn(conversation{ID: "aaaa-1111", Prompt: "current"})
	next, _ := m.Update(convosMsg{place: "/p/other", convos: []conversation{{ID: "cccc-3333"}}})
	if got := next.(model).resume.convos; len(got) != 1 || got[0].ID != "aaaa-1111" {
		t.Fatalf("convos = %+v, want another place's listing dropped", got)
	}
}

func TestFocusTakesThePickerDown(t *testing.T) {
	m := pickerOn(conversation{ID: "aaaa-1111", Prompt: "anything"})
	m.terms[700] = &remoteTerm{pid: 700, dir: "/p/scrn"}

	// A shell opened by hand takes the keys, which is the pane's now.
	next, _ := m.Update(termOpenedMsg{pid: 700})
	if next.(model).resume != nil {
		t.Fatal("the picker outlived the focus moving into a shell")
	}
}

func TestPrefixAReachesFromAFocusedShell(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
	m := twoShells(700)
	m.terms[700].dir = "/p/a"

	m = chord(m, "A")
	if m.resume == nil || m.resume.place.Path != "/p/a" {
		t.Fatalf("picker = %+v, want it open on the focused shell's place", m.resume)
	}
	if m.focus != 0 {
		t.Errorf("focus = %d, want the keys given to the picker", m.focus)
	}
}

func TestConvoDirsCoverThePlace(t *testing.T) {
	m := withProcs(96, 14, []Project{
		{Name: "a", Path: "/g/one/a", Group: "/g/one"},
		{Name: "b", Path: "/g/one/b", Group: "/g/one"},
	}, nil)
	m.groups = []Project{{Name: "one", Path: "/g/one"}}
	m.subs = map[string][]Project{"/g/one/a": {{Name: "docs", Path: "/g/one/a/docs"}}}
	m.rebuild()

	got := m.convoDirs(Project{Path: "/g/one"})
	want := []string{"/g/one", "/g/one/a", "/g/one/a/docs", "/g/one/b"}
	if len(got) != len(want) {
		t.Fatalf("dirs = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, d := range got {
		seen[d] = true
	}
	for _, d := range want {
		if !seen[d] {
			t.Errorf("dirs = %v, missing %s", got, d)
		}
	}
}

func TestLiveConversationsAreVettedAgainstTheTable(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, SessionID: "live-1111"},
	})
	if live := m.liveConversations(); !live["live-1111"] {
		t.Fatal("a running instance's conversation was not counted live")
	}

	// The same advertisement over a pid whose process is something else is a
	// leftover file, and its conversation is suspended.
	m = withClaude("vim", map[int]claudeSession{
		700: {PID: 700, SessionID: "left-2222"},
	})
	if live := m.liveConversations(); live["left-2222"] {
		t.Fatal("a stale session file hid the conversation it left behind")
	}
}

func TestPickerListsWhatCouldBeContinued(t *testing.T) {
	m := pickerOn(
		conversation{ID: "aaaa-1111", When: time.Now().Add(-2 * time.Hour),
			Branch: "main", Prompt: "fix the resize race"},
	)
	pane := stripANSI(strings.Join(m.resumeLines(60, 12), "\n"))
	for _, want := range []string{"scrn", "suspended conversations", "2h", "main", "fix the resize race"} {
		if !strings.Contains(pane, want) {
			t.Errorf("pane = %q, missing %q", pane, want)
		}
	}
}

func TestPickerSaysWhenNothingAnswers(t *testing.T) {
	m := pickerOn(conversation{ID: "aaaa-1111", Prompt: "one thing"})
	m = press(m, "z")
	pane := stripANSI(strings.Join(m.resumeLines(60, 12), "\n"))
	if !strings.Contains(pane, "nothing answers z") {
		t.Errorf("pane = %q, want it to say nothing answers", pane)
	}

	m.resume = &resumeView{place: Project{Name: "scrn"}, loaded: true}
	pane = stripANSI(strings.Join(m.resumeLines(60, 12), "\n"))
	if !strings.Contains(pane, "none to continue") {
		t.Errorf("pane = %q, want it to say there is nothing suspended", pane)
	}
}

func TestPickerFootWearsTheQuery(t *testing.T) {
	m := pickerOn(conversation{ID: "aaaa-1111", Prompt: "one thing"})
	m = press(m, "o")
	if f := footer(m); !strings.Contains(f, "/o█") {
		t.Errorf("footer = %q, want the query being typed", f)
	}
}

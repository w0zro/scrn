package main

// The pane, the rows, and the keys, tested without a server: everything here
// once lived beside the daemon's end-to-end tests and never needed the
// daemon. What does need one lives in session_test.go.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// insideShell puts a process under a shell the server holds, the way a claude
// started with a sits under the shell that ran it.
func insideShell(t *testing.T, shellPID int) model {
	t.Helper()
	m := withProcList(90, 14,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{
			{PID: shellPID, PPID: 1, Command: "zsh", Dir: "/tmp"},
			{PID: shellPID + 1, PPID: shellPID, Command: "claude", Dir: "/tmp"},
			// Two children, so the claude is a row of its own rather than
			// folding into the one thing it started.
			{PID: shellPID + 2, PPID: shellPID + 1, Command: "rg", Dir: "/tmp"},
			{PID: shellPID + 3, PPID: shellPID + 1, Command: "sed", Dir: "/tmp"},
		})
	m.terms = map[int]*remoteTerm{shellPID: {pid: shellPID, dir: "/tmp"}}
	m.rebuild()
	return m
}

// dimmed reports whether a row draws in the quiet gray.
func dimmed(m model, r navRow, selected bool) bool {
	return m.rowStyle(r, selected).GetForeground() == faintStyle.GetForeground()
}

func TestEnterOnAProcessScrnDidNotStartSaysSo(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 900, PPID: 1, Command: "vim", Dir: "/p/scrn"}})
	m.cursor = 1

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)

	if f := footer(m); !strings.Contains(f, "did not start vim 900") {
		t.Errorf("footer = %q, want it to explain why nothing happened", f)
	}
}

func TestAProcessScrnDidNotStartIsDimmed(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 900, PPID: 1, Command: "vim", Dir: "/p/scrn"}})

	if !dimmed(m, m.rows[1], false) {
		t.Error("a process scrn did not start should be drawn dim")
	}
	if !dimmed(m, m.rows[1], true) {
		t.Error("selecting it should not make it look available")
	}
}

func TestARepositoryIsNotDimmed(t *testing.T) {
	// Enter on a repository opens a shell, so it is somewhere you can go.
	m := withProcList(90, 14, []Project{{Name: "scrn", Path: "/p/scrn"}}, nil)

	if dimmed(m, m.rows[0], false) {
		t.Error("a repository can be stepped into and should read that way")
	}
}

func TestTheSelectedRowIsStillFindableWhenDimmed(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 900, PPID: 1, Command: "vim", Dir: "/p/scrn"}})
	m.cursor = 1

	row := stripANSI(m.renderRow(m.rows[1], true))
	if !strings.HasPrefix(row, "▸") {
		t.Errorf("row = %q, want the cursor marker to still say where you are", row)
	}
	if !m.rowStyle(m.rows[1], true).GetBold() {
		t.Error("the selected row should stand out from the other dim ones")
	}
}

func TestRefusingToAttachIsNotAnError(t *testing.T) {
	// The dim row already says it cannot be entered; pressing enter anyway is
	// a reminder, not a failure.
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 900, PPID: 1, Command: "vim", Dir: "/p/scrn"}})
	m.cursor = 1

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := next.(model)

	if got.statusErr {
		t.Error("declining to attach should not be reported as an error")
	}
	if !strings.Contains(footer(got), "did not start vim 900") {
		t.Errorf("footer = %q, want it to say why", footer(got))
	}
}

func TestANewShellStartsWhereTheWorkIs(t *testing.T) {
	// A build running in a subdirectory should give you a shell there, not at
	// the top of the repository.
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 900, PPID: 1, Command: "go", Dir: "/p/scrn/internal/deep"}})

	if got := m.shellDir(m.rows[1]); got != "/p/scrn/internal/deep" {
		t.Errorf("shellDir = %q, want the directory the process works in", got)
	}
	if got := m.shellDir(m.rows[0]); got != "/p/scrn" {
		t.Errorf("shellDir on a repo = %q, want the repository", got)
	}
}

func TestFooterAdvertisesTheNewShellKey(t *testing.T) {
	if f := keysOf(sized(160, 24)); !strings.Contains(f, "s shell") {
		t.Errorf("footer = %q, want the one way to make a process advertised", f)
	}
}

func TestFooterAdvertisesTheAgentKey(t *testing.T) {
	if f := keysOf(sized(160, 24)); !strings.Contains(f, "a agent") {
		t.Errorf("footer = %q, want the agent key advertised", f)
	}
}

func TestAnythingInsideAShellScrnHoldsIsBright(t *testing.T) {
	m := insideShell(t, 500)

	for _, r := range m.rows {
		if dimmed(m, r, false) {
			t.Errorf("row %q is dim, but enter reaches it through the shell", stripANSI(m.renderRow(r, false)))
		}
	}
}

func TestAProcessOutsideAnyOwnedShellIsStillOutOfReach(t *testing.T) {
	m := insideShell(t, 500)
	next, _ := m.Update(procsMsg{procs: append(m.procs,
		Proc{PID: 900, PPID: 1, Command: "vim", Dir: "/tmp"})})
	m = next.(model)

	var vim navRow
	for _, r := range m.rows {
		if r.kind == rowProc && r.node.PID == 900 {
			vim = r
		}
	}
	if vim.node == nil {
		t.Fatal("setup: the unrelated process is not listed")
	}
	if !dimmed(m, vim, false) {
		t.Error("a process in no shell scrn holds should still be dim")
	}
	if m.attachable(vim) {
		t.Error("enter should not claim to reach it")
	}
}

func TestAProcessTableThatLoopsDoesNotHangTheWalk(t *testing.T) {
	// Two processes each claiming to be the other's parent.
	m := withProcList(90, 14,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{
			{PID: 10, PPID: 11, Command: "a", Dir: "/tmp"},
			{PID: 11, PPID: 10, Command: "b", Dir: "/tmp"},
		})
	if got := m.owningTerm(10); got != nil {
		t.Errorf("owningTerm = %+v, want nothing found", got)
	}
}

func TestAProcessInNobodysShellIsJustItself(t *testing.T) {
	// scrn cannot end a shell it does not hold, so there is nothing to take.
	m := withProcList(90, 14,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{
			{PID: 800, PPID: 1, Command: "zsh", Dir: "/tmp"},
			{PID: 801, PPID: 800, Command: "vim", Dir: "/tmp"},
		})
	m.cursor = 1

	next, _ := m.Update(typed("x"))
	if got := targets(next.(model).pendingKill); !sameInts(got, []int{801}) {
		t.Errorf("targets = %v, want just the process", got)
	}
}

func TestAShellIsNotItsOwnShell(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/tmp"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/tmp"}}
	m.rebuild()
	m.cursor = 1

	next, _ := m.Update(typed("x"))
	m = next.(model)

	if got := targets(m.pendingKill); !sameInts(got, []int{700}) {
		t.Errorf("targets = %v, want the shell once", got)
	}
	if f := footer(m); strings.Contains(f, "and its shell") {
		t.Errorf("footer = %q, want no mention of a shell around the shell", f)
	}
}

func TestTheReportSaysWhatWasActuallyDone(t *testing.T) {
	// A kill is not one thing: a shell scrn holds is hung up, everything else
	// is signalled, and a subtree can be both at once.
	cases := []struct {
		name    string
		results []killResult
		want    string
	}{
		{"only signalled", []killResult{{pid: 1}}, "sent SIGTERM to "},
		{"only hung up", []killResult{{pid: 1, hungUp: true}}, "closed "},
		{"both", []killResult{{pid: 1}, {pid: 2, hungUp: true}}, "ended "},
		{"failures do not count", []killResult{
			{pid: 1, hungUp: true}, {pid: 2, err: errors.New("nope")},
		}, "closed "},
	}
	for _, c := range cases {
		if got := ended(c.results); got != c.want {
			t.Errorf("%s: ended = %q, want %q", c.name, got, c.want)
		}
	}
}

// replaceableServer is a model holding one shell over a recording session,
// which is what R is for ending.
func replaceableServer(t *testing.T) model {
	t.Helper()
	m := repoModel()
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/tmp"}}
	m, _ = pipeServer(t, m)
	m.rebuild()
	return m
}

func TestRAsksBeforeEndingTheWorkItHolds(t *testing.T) {
	m := replaceableServer(t)
	next, _ := m.Update(typed("R"))
	m = next.(model)

	if !m.pendingReplace {
		t.Fatal("R should ask before ending the shells the server holds")
	}
	if f := footer(m); !strings.Contains(f, "end the server, and 1 shell?") {
		t.Errorf("footer = %q, want it to say what it is about to end", f)
	}
}

func TestConfirmingEndsTheServer(t *testing.T) {
	m := replaceableServer(t)
	next, _ := m.Update(typed("R"))
	next, _ = next.(model).Update(typed("R"))
	m = next.(model)

	if len(m.terms) != 0 {
		t.Error("the shells went with the server; the navigator should not still hold them")
	}
	if !strings.Contains(footer(m), "ending the server") {
		t.Errorf("footer = %q, want it to say what is happening", footer(m))
	}
}

func TestAnyOtherKeyLeavesTheServerAlone(t *testing.T) {
	m := replaceableServer(t)
	next, _ := m.Update(typed("R"))
	next, _ = next.(model).Update(typed("j"))
	m = next.(model)

	if m.pendingReplace {
		t.Error("the question should be over")
	}
	if len(m.terms) != 1 {
		t.Error("cancelling should not have ended anything")
	}
	if !strings.Contains(footer(m), "left the server alone") {
		t.Errorf("footer = %q, want it to say nothing happened", footer(m))
	}
}

func TestRWithNothingHeldSaysSo(t *testing.T) {
	m := repoModel()
	next, _ := m.Update(typed("R"))
	m = next.(model)

	if m.pendingReplace {
		t.Error("there is nothing to end; R should not arm")
	}
	if !strings.Contains(footer(m), "nothing is held") {
		t.Errorf("footer = %q, want it to explain itself", footer(m))
	}
}

// projectNeeding is a project with the given plan, over a recording session.
func projectNeeding(t *testing.T, plan string) (model, string) {
	t.Helper()
	dir := t.TempDir()
	if plan != "" {
		if err := os.WriteFile(filepath.Join(dir, planFile), []byte(plan), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := withProcList(90, 20, []Project{{Name: "proj", Path: dir}}, nil)
	m, _ = pipeServer(t, m)
	return m, dir
}

func TestROnAProjectThatSaysNothingExplainsItself(t *testing.T) {
	m, _ := projectNeeding(t, "")
	next, _ := m.Update(typed("r"))
	m = next.(model)

	if len(m.terms) != 0 {
		t.Error("nothing should have been started")
	}
	if !strings.Contains(footer(m), "does not say what it needs") {
		t.Errorf("footer = %q, want it to explain why nothing happened", footer(m))
	}
}

func TestXOnAProjectWithNothingRunningSaysSo(t *testing.T) {
	// A plan is a list to run, not something running: until r is pressed
	// there is nothing for a kill to act on.
	m, _ := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(typed("x"))
	m = next.(model)

	if m.pendingKill != nil {
		t.Error("there is nothing to kill, so nothing to confirm")
	}
	if !strings.Contains(footer(m), "nothing running in proj") {
		t.Errorf("footer = %q, want it to explain", footer(m))
	}
}

func TestTheKeysListTheRun(t *testing.T) {
	if f := keysOf(sized(160, 24)); !strings.Contains(f, "r run") {
		t.Errorf("keys = %q, want the run key listed", f)
	}
}

func TestAShellSittingAtAPromptIsCalledWhatTheProjectCallsIt(t *testing.T) {
	// A shell running nothing in particular has nothing better to offer than
	// the name its project gave it.
	m := withProcList(90, 14,
		[]Project{{Name: "proj", Path: "/p/proj"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Argv: "/bin/zsh", Dir: "/p/proj"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/p/proj", name: "web"}}
	m.rebuild()

	if got := navColumn(m)[1]; !strings.Contains(got, "web") {
		t.Errorf("row = %q, want it named for what the project calls it", got)
	}
}

func TestWhatIsRunningWinsOverThePlansNameForIt(t *testing.T) {
	// "dev" is a fine name for a plan entry and a poor one for a row: "npm run
	// dev" is the same thing said usefully, and it is what the row would show
	// had the shell been started by hand.
	m := withProcList(90, 14,
		[]Project{{Name: "proj", Path: "/p/proj"}},
		[]Proc{
			{PID: 700, PPID: 1, Command: "zsh", Argv: "/bin/zsh", Dir: "/p/proj"},
			{PID: 701, PPID: 700, Command: "node", Argv: "node /opt/npm run dev", Dir: "/p/proj"},
		})
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/p/proj", name: "dev"}}
	m.rebuild()

	if got := navColumn(m)[1]; !strings.Contains(got, "npm run dev") {
		t.Errorf("row = %q, want what is actually running", got)
	}
}

func TestANameIsNotRepeatedBackAtYou(t *testing.T) {
	// A plan entry whose command is its own name says nothing twice.
	m := withProcList(90, 14,
		[]Project{{Name: "proj", Path: "/p/proj"}},
		[]Proc{
			{PID: 700, PPID: 1, Command: "zsh", Argv: "/bin/zsh", Dir: "/p/proj"},
			{PID: 701, PPID: 700, Command: "claude", Argv: "claude", Dir: "/p/proj"},
		})
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/p/proj", name: "claude"}}
	m.rebuild()

	if got := strings.TrimSpace(navColumn(m)[1]); got != "└─ claude" {
		t.Errorf("row = %q, want the name once", got)
	}
}

func TestAShellOpenedByHandKeepsItsCommandName(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "proj", Path: "/p/proj"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/p/proj"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/p/proj"}}
	m.rebuild()

	if got := navColumn(m)[1]; !strings.Contains(got, "zsh") {
		t.Errorf("row = %q, want the command when no project named it", got)
	}
}

// lookingUp is two projects over a recording session, mid-search.
func lookingUp(t *testing.T, filter string) model {
	t.Helper()
	dir := t.TempDir()
	m := withProcList(90, 20, []Project{
		{Name: "alpha", Path: dir},
		{Name: "beta", Path: t.TempDir()},
	}, nil)
	m, _ = pipeServer(t, m)
	m.showAll = false
	m = press(m, "/")
	for _, r := range filter {
		next, _ := m.Update(typed(string(r)))
		m = next.(model)
	}
	return m
}

func TestCtrlROnAProjectThatNeedsNothingSaysSo(t *testing.T) {
	m := lookingUp(t, "alpha")

	next, _ := m.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = next.(model)

	if len(m.terms) != 0 {
		t.Error("nothing should have been started")
	}
	// The search still closes: it was answered, even if the answer was that
	// there was nothing to do.
	if m.typing {
		t.Error("the search should be over")
	}
	if !strings.Contains(footer(m), "does not say what it needs") {
		t.Errorf("footer = %q, want it to explain why nothing happened", footer(m))
	}
}

func TestCtrlUClearsTheQueryAndKeepsTyping(t *testing.T) {
	// The query is a line being typed, and ctrl+u is what clears one.
	m := lookingUp(t, "alpha")

	next, _ := m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m = next.(model)

	if m.filter != "" {
		t.Errorf("filter = %q, want ctrl+u to have cleared it", m.filter)
	}
	if !m.typing {
		t.Error("clearing the query should not end the search")
	}
	if len(m.rows) < 2 {
		t.Errorf("rows = %d, want the list back to every project", len(m.rows))
	}
}

func TestTypingOnClearsWhatWasSaidAboutTheLastProject(t *testing.T) {
	// Typing a letter clears the last report and carries the search on.
	m := lookingUp(t, "alpha")
	m.status, m.statusErr = "something about the last one", false

	m = typeFilter(m, "x")
	if m.status != "" {
		t.Errorf("status = %q, want it cleared once the search moved on", m.status)
	}
	if !m.typing {
		t.Error("typing a letter should not have ended the search")
	}
}

func TestStartingAnAgentFromTheFilterEndsTheTyping(t *testing.T) {
	// ctrl+r and ctrl+x end the looking; ctrl+a used to leave it on. The
	// shell it opened took the keys — and handed them back to the filter the
	// moment it was gone, with q typing rather than quitting.
	m := lookingUp(t, "alpha")

	next, _ := m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	m = next.(model)

	if m.typing {
		t.Error("starting an agent should be the end of looking for the place to start it")
	}
}

func TestAHeldShellsRowDrawsItsFactsNotItsScreen(t *testing.T) {
	// The shell itself is tmux's pane beside the navigator, drawn live. When
	// the navigator has the window — before the shell has joined it, or in
	// a window too narrow to share — its own pane says what the row is,
	// and never tries to draw what the shell is showing.
	m := withProcList(90, 24,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/tmp"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700}}
	m.cursor = 1
	m.details[detailKey(m.rows[1])] = []field{heading("zsh 700"), note("/tmp")}

	pane := stripANSI(strings.Join(m.paneLines(60, 24), "\n"))
	if !strings.Contains(pane, "zsh 700") {
		t.Errorf("pane lacks the row's facts:\n%s", pane)
	}
}

func TestBesideAShownShellTheNavigatorIsOnlyItsColumn(t *testing.T) {
	// With a shell shown, the navigator's pane is exactly its column wide,
	// and it draws no pane of its own: the border and everything right of
	// it are tmux's.
	m := withProcList(navWidth, 24,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/tmp"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700}}
	m.cursor = 1

	for i, line := range strings.Split(stripANSI(m.layout()), "\n") {
		if strings.Contains(line, glyphDivider) || lipgloss.Width(line) > navWidth {
			t.Fatalf("line %d = %q, want the navigator's column and nothing beside it", i, line)
		}
	}
}

func TestAPasteAtTheNavigatorSaysWhereItWent(t *testing.T) {
	// Nowhere, that is. A paste with no filter open lands on nothing, and
	// the foot says so rather than letting a paste read as broken.
	m := repoModel()
	next, _ := m.Update(tea.PasteMsg{Content: "some text"})
	if f := footer(next.(model)); !strings.Contains(f, "nothing here to paste") {
		t.Errorf("footer = %q, want the paste explained", f)
	}
}

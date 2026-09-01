package main

// The pane, the rows, and the keys, tested without a server: everything here
// once lived beside the daemon's end-to-end tests and never needed the
// daemon. What does need one lives in session_test.go.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// insideShell puts a process under a shell the daemon holds, the way a claude
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

func TestAPreviewWearsGrayInPlaceOfTheProgramsColors(t *testing.T) {
	// A glance at the pane should answer whether the keys are going there:
	// full color is attached, gray is a preview.
	m := withProcList(90, 14,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{{PID: 700, PPID: 1, Command: "claude", Dir: "/tmp"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700, screen: "\x1b[31mred alert\x1b[m"}}
	m.cursor = 1 // the shell's row, so the pane previews it

	preview := strings.Join(m.paneLines(40, 5), "\n")
	if strings.Contains(preview, "\x1b[31m") {
		t.Error("a preview kept the program's own colors")
	}
	if !strings.Contains(stripANSI(preview), "red alert") {
		t.Error("the preview lost the screen's text")
	}

	m.focus = 700
	attached := strings.Join(m.paneLines(40, 5), "\n")
	if !strings.Contains(attached, "\x1b[31m") {
		t.Error("the attached shell should keep the program's own colors")
	}
}

func TestEnterOnAProcessScrnDidNotStartSaysSo(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 900, PPID: 1, Command: "vim", Dir: "/p/scrn"}})
	m.cursor = 1

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)

	if m.focus != 0 {
		t.Error("scrn cannot attach to a process it did not start")
	}
	if f := footer(m); !strings.Contains(f, "did not start vim 900") {
		t.Errorf("footer = %q, want it to explain why nothing happened", f)
	}
}

func TestEveryScreenRowIsAsWideAsThePane(t *testing.T) {
	// The client cuts columns out of a row to mark the cursor cell, so the
	// grid a capture becomes has to be whole: every row as wide as the pane,
	// exactly as many rows as the pane is tall.
	got := padScreen([]string{"ab"}, 12, 3)
	rows := strings.Split(got, "\n")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the pane's 3", len(rows))
	}
	for i, row := range rows {
		if lipgloss.Width(row) != 12 {
			t.Errorf("row %d is %d columns wide, want 12", i, lipgloss.Width(row))
		}
	}
}

func TestTheCursorColumnIsACellTheRowActuallyHas(t *testing.T) {
	// A cursor sitting past the text — at the prompt's end — must land on a
	// cell the padded row actually carries.
	row := strings.Split(padScreen([]string{"ab "}, 12, 1), "\n")[0]
	if got := lipgloss.Width(row); got <= 3 {
		t.Errorf("the row is %d columns wide with the cursor at 3 — the cell under the cursor is not in it", got)
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

func TestOnlyTheAttachedProcessSpeaksForTheWindow(t *testing.T) {
	// A build finishing in a shell you are not looking at should not put its
	// progress on the tab of the one you are.
	m := repoModel()
	m.terms = map[int]*remoteTerm{
		700: {pid: 700, progress: "9;4;3;50"},
		800: {pid: 800, progress: "9;4;1;10"},
	}

	m.focus = 700
	if got := m.progressBar(); got == nil || got.State != tea.ProgressBarIndeterminate {
		t.Errorf("bar = %+v, want the attached shell's progress", got)
	}
	m.focus = 800
	if got := m.progressBar(); got == nil || got.State != tea.ProgressBarDefault || got.Value != 10 {
		t.Errorf("bar = %+v, want the newly attached shell's progress", got)
	}
}

func TestProgressIsClearedWhenNothingIsAttached(t *testing.T) {
	// Leaving a shell that was reporting progress must take the indicator with
	// it, or the terminal keeps showing work that is no longer being watched.
	m := repoModel()
	m.terms = map[int]*remoteTerm{700: {pid: 700, progress: "9;4;3;50"}}

	m.focus = 0
	if got := m.progressBar(); got != nil {
		t.Errorf("bar = %+v, want none: a nil bar is how the renderer clears it", got)
	}
}

func TestOnlyTheAttachedProcessRetitlesTheWindow(t *testing.T) {
	// The title rides out on the view, and only the shell being looked at
	// speaks for it: another one finishing a build should not retitle a tab
	// showing something else.
	m := repoModel()
	m.terms = map[int]*remoteTerm{700: {pid: 700}, 800: {pid: 800}}
	m.focus = 700

	next, _ := m.Update(screenMsg{pid: 800, title: "0;not yours"})
	m = next.(model)
	if got := m.View().WindowTitle; got != "" {
		t.Errorf("title = %q, want an unfocused shell kept off the window", got)
	}

	next, _ = m.Update(screenMsg{pid: 700, title: "0;vim README.md"})
	m = next.(model)
	if got := m.View().WindowTitle; got != "vim README.md" {
		t.Errorf("title = %q, want the focused shell's, without its command number", got)
	}
}

func TestTheTitlePayloadDropsItsCommandNumber(t *testing.T) {
	cases := map[string]string{
		"0;✳ Claude Code": "✳ Claude Code",
		"2;vim README.md": "vim README.md",
		"no semicolon":    "no semicolon",
	}
	for in, want := range cases {
		if got := oscTitleText(in); got != want {
			t.Errorf("oscTitleText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheProgressPayloadIsReadAsStateAndValue(t *testing.T) {
	// The payload the emulator hands over is the OSC 9;4 it heard, and its
	// states are numbered the way the renderer's are.
	m := repoModel()
	m.terms = map[int]*remoteTerm{700: {pid: 700, progress: "9;4;2;77"}}
	m.focus = 700

	got := m.progressBar()
	if got == nil || got.State != tea.ProgressBarError || got.Value != 77 {
		t.Errorf("bar = %+v, want the error state at 77", got)
	}

	// A payload that is not a progress report sets nothing.
	m.terms[700].progress = "0;some title"
	if got := m.progressBar(); got != nil {
		t.Errorf("bar = %+v, want none for a payload that is not 9;4", got)
	}
}

// replaceableServer is a model holding one shell over a recording session,
// which is what R is for ending.
func replaceableServer(t *testing.T) model {
	t.Helper()
	m := repoModel()
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/tmp"}}
	m, _ = pipeDaemon(t, m)
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

	if len(m.terms) != 0 || m.focus != 0 {
		t.Error("the shells went with the server; the window should not still hold them")
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
	m, _ = pipeDaemon(t, m)
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
	m, _ = pipeDaemon(t, m)
	m.showAll = false
	m = press(m, "/")
	for _, r := range filter {
		next, _ := m.Update(typed(string(r)))
		m = next.(model)
	}
	return m
}

func paneText(m model) string {
	return strings.Join(detailColumn(m), "\n")
}

func wheelMsg(btn tea.MouseButton) tea.MouseMsg {
	return tea.MouseWheelMsg{X: navWidth + 2, Y: 2, Button: btn}
}

// watchingTail is a model focused on a shell with forty lines of transcript
// above a full screen, not yet reading back.
func watchingTail(t *testing.T) model {
	t.Helper()
	m := withProcList(90, 14, []Project{{Name: "tmp", Path: "/tmp"}}, nil)
	rows := make([]string, m.paneHeight())
	for i := range rows {
		rows[i] = "srow"
	}
	rows[len(rows)-1] = "live-tail-marker"
	m.terms = map[int]*remoteTerm{700: {pid: 700, screen: strings.Join(rows, "\n"), sb: 40}}
	m.focus = 700
	return m
}

// readingBack is watchingTail with the wheel turned and the transcript
// arrived: h-1 oldest through h-40 newest, three lines up.
func readingBack(t *testing.T) model {
	t.Helper()
	m := watchingTail(t)
	next, _ := m.Update(wheelMsg(tea.MouseWheelUp))
	m = next.(model)
	if m.scroll == nil {
		t.Fatal("a wheel up at the prompt should start reading back")
	}

	hist := make([]string, 40)
	for i := range hist {
		hist[i] = "h-" + strconv.Itoa(i+1)
	}
	next, _ = m.Update(historyMsg{pid: 700, history: strings.Join(hist, "\n")})
	return next.(model)
}

func TestAWheelOnTheAlternateScreenBecomesArrows(t *testing.T) {
	// How less and man scroll: the program never asked for the mouse, the
	// alternate screen has no transcript, so a notch is three arrow presses.
	m := watchingTail(t)
	m.terms[700].alt = true
	m, asked := pipeDaemon(t, m)

	next, _ := m.Update(wheelMsg(tea.MouseWheelUp))
	if next.(model).scroll != nil {
		t.Fatal("the alternate screen has no transcript above it to read")
	}
	for i := 0; i < wheelArrowCount; i++ {
		got := askedFor(t, asked)
		if got.Kind != kindInput || !strings.Contains(got.Run, "Up") {
			t.Fatalf("ask %d = %+v, want an Up arrow", i, got)
		}
	}
}

func TestAWheelStaysAWheelWhenTheProgramAskedForIt(t *testing.T) {
	// A program listening for the mouse gets the wheel as the SGR bytes it
	// asked to be told about, not as arrows.
	m := watchingTail(t)
	m.terms[700].mouse = true
	m, asked := pipeDaemon(t, m)

	m.Update(wheelMsg(tea.MouseWheelUp))
	got := askedFor(t, asked)
	// ESC [ < is 1b 5b 3c: the SGR mouse prelude, sent as hex.
	if got.Kind != kindInput || !strings.Contains(got.Run, "-H 1b 5b 3c") {
		t.Fatalf("ask = %+v, want the wheel as SGR bytes", got)
	}
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

func TestAWheelUpAtThePromptReadsTheTranscript(t *testing.T) {
	m := readingBack(t)

	if m.scroll == nil || m.scroll.above != wheelLines {
		t.Fatalf("scroll = %+v, want the reader a notch up", m.scroll)
	}
	pane := paneText(m)
	if !strings.Contains(pane, "h-40") {
		t.Errorf("pane should show the transcript above the screen:\n%s", pane)
	}
	if strings.Contains(pane, "live-tail-marker") {
		t.Errorf("the live tail should be below the viewport:\n%s", pane)
	}
}

func TestTheMotionsMoveTheReading(t *testing.T) {
	m := readingBack(t)

	m = press(m, "k")
	if m.scroll.above != wheelLines+1 {
		t.Errorf("above = %d, want k to have gone a line up", m.scroll.above)
	}
	m = press(m, "j")
	if m.scroll.above != wheelLines {
		t.Errorf("above = %d, want j to have come a line back", m.scroll.above)
	}

	m = press(m, "g")
	if !strings.Contains(paneText(m), "h-1") {
		t.Errorf("g should reach the oldest line:\n%s", paneText(m))
	}
	m = press(m, "G")
	if m.scroll != nil {
		t.Error("G should end at the live screen, which is where leaving goes")
	}
}

func TestRollingPastTheBottomReturnsToLive(t *testing.T) {
	m := readingBack(t)

	next, _ := m.Update(wheelMsg(tea.MouseWheelDown))
	m = next.(model)

	if m.scroll != nil {
		t.Fatal("rolling past the tail should return to the live pane")
	}
	if !strings.Contains(paneText(m), "live-tail-marker") {
		t.Errorf("the pane should be live again:\n%s", paneText(m))
	}
}

func TestReadingSwallowsWhatIsNotAMotion(t *testing.T) {
	m := readingBack(t)

	m = press(m, "x")
	if m.pendingKill != nil {
		t.Error("x while reading should not arm a kill")
	}
	if m.scroll == nil {
		t.Error("a swallowed key should not end the reading")
	}

	m = press(m, "esc")
	if m.scroll != nil {
		t.Error("esc should end the reading")
	}
}

func TestPrefixNWhileReadingGoesAllTheWayOut(t *testing.T) {
	m := chord(readingBack(t), "n")

	if m.scroll != nil || m.focus != 0 {
		t.Errorf("scroll = %v focus = %d, want the reading and the shell both left", m.scroll, m.focus)
	}
}

func TestTheWheelIsTheProgramsWhenItAskedForIt(t *testing.T) {
	m := watchingTail(t)
	m.terms[700].mouse = true

	next, _ := m.Update(wheelMsg(tea.MouseWheelUp))
	if next.(model).scroll != nil {
		t.Error("a program listening for the mouse should keep its wheel")
	}
}

func TestTheWheelDoesNotReadOverTheAlternateScreen(t *testing.T) {
	m := watchingTail(t)
	m.terms[700].alt = true

	next, _ := m.Update(wheelMsg(tea.MouseWheelUp))
	if next.(model).scroll != nil {
		t.Error("the alternate screen has no transcript above it to read")
	}
}

func TestTheHintSaysTheTranscriptIsBeingRead(t *testing.T) {
	m := readingBack(t)
	if f := footer(m); !strings.Contains(f, "scrollback") {
		t.Errorf("footer = %q, want it to say where the keys went", f)
	}
}

func TestABareHeldShellGetsItsFactsAboveItsScreen(t *testing.T) {
	// A screen dump with no name over it says what the shell is showing but
	// not what it is. A shell scrn holds previews like a folded run: facts in
	// a banner, the live screen beneath.
	m := withProcList(90, 24,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/tmp"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700, screen: "marker-on-screen"}}
	m.cursor = 1
	m.details[detailKey(m.rows[1])] = []field{heading("zsh 700"), note("/tmp")}

	pane := stripANSI(strings.Join(m.paneLines(60, 24), "\n"))
	if !strings.Contains(pane, "zsh 700") {
		t.Errorf("pane lacks the banner facts:\n%s", pane)
	}
	if !strings.Contains(pane, "marker-on-screen") {
		t.Errorf("pane lacks the screen below the banner:\n%s", pane)
	}

	// Focused, the banner yields the whole pane to the shell.
	m.focus = 700
	pane = stripANSI(strings.Join(m.paneLines(60, 24), "\n"))
	if strings.Contains(pane, "zsh 700") {
		t.Error("a focused shell should have the pane whole, without the banner")
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

func TestAPaneRowCannotBleedIntoTheNextLine(t *testing.T) {
	// A captured row can end with a background still open — a full-width
	// prompt bar does. Left unreset it would run across the newline and
	// paint the next line's navigator, so every composed line ends reset.
	m := watchingTail(t)
	rows := strings.Split(m.terms[700].screen, "\n")
	rows[2] = "\x1b[42mpainted to the edge"
	m.terms[700].screen = strings.Join(rows, "\n")

	for i, line := range strings.Split(m.layout(), "\n") {
		if !strings.HasSuffix(line, "\x1b[m") {
			t.Fatalf("line %d = %q, want it to end with the styles reset", i, line)
		}
	}
}

func TestPaddingIsNotPaintedWithTheRowsLeftovers(t *testing.T) {
	got := padScreen([]string{"\x1b[42mgreen"}, 10, 1)
	if !strings.Contains(got, "\x1b[42mgreen\x1b[m ") {
		t.Errorf("row = %q, want the padding to start reset", got)
	}
}

func TestAPasteAtTheNavigatorSaysWhereItWent(t *testing.T) {
	// Nowhere, that is. A paste with no shell focused and no filter open
	// lands on nothing, and the foot says so rather than letting cmd+v
	// read as broken.
	m := repoModel()
	next, _ := m.Update(tea.PasteMsg{Content: "some text"})
	if f := footer(next.(model)); !strings.Contains(f, "nothing is focused") {
		t.Errorf("footer = %q, want the paste explained", f)
	}
}

func TestCmdVReachesTheShellAsAPaste(t *testing.T) {
	// The terminal handed the chord through instead of pasting; scrn reads
	// the clipboard and the content crosses as the paste it was meant to
	// be — bracketed for the programs that asked, exactly as a translated
	// cmd+v would have arrived.
	old := readClipboard
	readClipboard = func() string { return "from the clipboard" }
	t.Cleanup(func() { readClipboard = old })

	m := watchingTail(t)
	m, asked := pipeDaemon(t, m)

	next, cmd := m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModSuper})
	m = next.(model)
	if cmd == nil {
		t.Fatal("no clipboard read was scheduled")
	}
	next, _ = m.Update(cmd())
	_ = next

	got := askedFor(t, asked)
	if got.Kind != kindInput || !strings.Contains(got.Run, "set-buffer") {
		t.Fatalf("ask = %+v, want the clipboard crossing as a buffer", got)
	}
}

// previewedShell is a model standing on a held shell's row without having
// entered it: the pane is a preview, the keys are the navigator's.
func previewedShell(t *testing.T) model {
	th := t.Helper
	th()
	m := withProcList(90, 14, []Project{{Name: "tmp", Path: "/tmp"}}, []Proc{
		{PID: 700, PPID: 1, Command: "zsh", Dir: "/tmp"},
	})
	rows := make([]string, m.paneHeight())
	for i := range rows {
		rows[i] = "srow"
	}
	m.terms = map[int]*remoteTerm{700: {pid: 700, screen: strings.Join(rows, "\n"), sb: 40}}
	m.rebuild()
	for i, r := range m.rows {
		if r.kind == rowProc && r.holds(700) {
			m.cursor = i
		}
	}
	return m
}

func TestAPreviewsWheelReadsItsTranscript(t *testing.T) {
	// Scrolling is looking, and a preview is exactly the pane being looked
	// at: the wheel reads back without stepping in.
	m := previewedShell(t)
	next, _ := m.Update(wheelMsg(tea.MouseWheelUp))
	m = next.(model)
	if m.scroll == nil || m.scroll.pid != 700 {
		t.Fatalf("scroll = %+v, want the preview's transcript being read", m.scroll)
	}
	if m.focus != 0 {
		t.Error("the wheel should not have stepped into the shell")
	}
}

func TestAPreviewsWheelReachesAProgramThatAskedForIt(t *testing.T) {
	m := previewedShell(t)
	m.terms[700].mouse = true
	m, asked := pipeDaemon(t, m)

	m.Update(wheelMsg(tea.MouseWheelUp))
	got := askedFor(t, asked)
	if got.Kind != kindInput || !strings.Contains(got.Run, "-H 1b 5b 3c") {
		t.Fatalf("ask = %+v, want the wheel as SGR bytes to the previewed pane", got)
	}
}

func TestClickingAPreviewStepsIn(t *testing.T) {
	// A press is more than looking: like clicking an unfocused window, it
	// focuses what was clicked.
	m := previewedShell(t)
	m, _ = pipeDaemon(t, m)

	next, _ := m.Update(tea.MouseClickMsg{X: navWidth + 5, Y: 3, Button: tea.MouseLeft})
	if got := next.(model).focus; got != 700 {
		t.Errorf("focus = %d, want the click to step into the shell", got)
	}
}

func TestAClickInTheNavigatorTouchesNoPane(t *testing.T) {
	m := previewedShell(t)
	m, _ = pipeDaemon(t, m)
	next, _ := m.Update(tea.MouseClickMsg{X: 3, Y: 3, Button: tea.MouseLeft})
	if got := next.(model).focus; got != 0 {
		t.Errorf("focus = %d, want a navigator click to enter nothing", got)
	}
}

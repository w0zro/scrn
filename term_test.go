package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// openShellIn starts a real shell in dir and drains its first output.
func openShellIn(t *testing.T, m model, dir string) model {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")

	msg := openTerm(dir, 40, 8)()
	started, ok := msg.(termStartedMsg)
	if !ok || started.err != nil {
		t.Fatalf("openTerm = %+v, want a shell", msg)
	}
	next, _ := m.Update(started)
	m = next.(model)
	t.Cleanup(func() {
		for _, term := range m.terms {
			term.close()
		}
	})
	return drain(m, 2*time.Second)
}

// drain pumps terminal output into the model until the shell goes quiet.
func drain(m model, d time.Duration) model {
	deadline := time.After(d)
	for {
		t := m.terms[m.focus]
		if t == nil {
			return m
		}
		select {
		case <-deadline:
			return m
		case _, ok := <-t.output:
			if !ok {
				return m
			}
			next, _ := m.Update(termOutputMsg{pid: t.pid})
			m = next.(model)
		case <-time.After(300 * time.Millisecond):
			return m
		}
	}
}

func send(m model, s string) model {
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return next.(model)
}

func paneText(m model) string {
	return strings.Join(detailColumn(m), "\n")
}

func repoModel() model {
	return withProcList(90, 14, []Project{{Name: "scrn", Path: "/tmp"}}, nil)
}

func TestEnterOnARepoOpensAShellAndFocusesIt(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")

	if m.focused() == nil {
		t.Fatal("a new shell should take the keystrokes")
	}
	if len(m.terms) != 1 {
		t.Errorf("terms = %d, want the one shell", len(m.terms))
	}
}

func TestTheShellRunsInThePaneAndTheNavigatorRemains(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	m = drain(send(m, "echo marker-in-the-pane"), 2*time.Second)

	if !strings.Contains(paneText(m), "marker-in-the-pane") {
		t.Errorf("pane should show the shell's output:\n%s", paneText(m))
	}
	if nav := navColumn(m); len(nav) == 0 || !strings.Contains(nav[0], "scrn") {
		t.Errorf("the navigator should still be there, got %v", nav)
	}
}

func TestCtrlOLeavesTheShellRunning(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = next.(model)

	if m.focused() != nil {
		t.Error("ctrl+o should hand the keys back to the navigator")
	}
	if len(m.terms) != 1 {
		t.Error("ctrl+o should leave the shell running, not end it")
	}
}

func TestAFocusedShellTakesEveryOtherKey(t *testing.T) {
	// q, x and ctrl+c are scrn's keys on the list and the shell's here.
	m := openShellIn(t, repoModel(), "/tmp")

	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyRunes, Runes: []rune("x")},
		{Type: tea.KeyCtrlC},
	} {
		next, cmd := m.Update(msg)
		m = next.(model)
		if cmd != nil {
			t.Errorf("%v should go to the shell, not act on the list", msg)
		}
		if m.pendingKill != nil {
			t.Errorf("%v armed a kill instead of reaching the shell", msg)
		}
	}
}

func TestTheShellTakesItsPlaceInTheTree(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	pid := m.focus

	// The scan that follows finds the shell working in the repository.
	next, _ := m.Update(procsMsg{procs: []Proc{
		{PID: pid, PPID: 1, Command: "sh", Dir: "/tmp"},
	}})
	m = next.(model)

	if r, ok := m.selected(); !ok || r.kind != rowProc || r.node.PID != pid {
		t.Fatalf("selected = %+v, want the cursor moved onto the new shell", r)
	}
	if m.paneTerm() == nil {
		t.Error("the pane should show the shell the cursor is on")
	}
}

func TestEnterOnAShellStepsBackIntoIt(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	pid := m.focus
	next, _ := m.Update(procsMsg{procs: []Proc{{PID: pid, PPID: 1, Command: "sh", Dir: "/tmp"}}})
	m = next.(model)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)

	if m.focus != pid {
		t.Errorf("focus = %d, want enter to step back into shell %d", m.focus, pid)
	}
}

func TestEnterOnAProcessScrnDidNotStartSaysSo(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 900, PPID: 1, Command: "vim", Dir: "/p/scrn"}})
	m.cursor = 1

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)

	if m.focus != 0 {
		t.Error("scrn cannot attach to a process it did not start")
	}
	if f := footer(m); !strings.Contains(f, "did not start vim 900") {
		t.Errorf("footer = %q, want it to explain why nothing happened", f)
	}
}

func TestAnUnfocusedShellStillShowsInThePane(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	m = drain(send(m, "echo still-visible"), 2*time.Second)
	pid := m.focus

	next, _ := m.Update(procsMsg{procs: []Proc{{PID: pid, PPID: 1, Command: "sh", Dir: "/tmp"}}})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = next.(model)

	if !strings.Contains(paneText(m), "still-visible") {
		t.Errorf("the pane should still show the shell under the cursor:\n%s", paneText(m))
	}
}

func TestAShellThatExitsIsForgotten(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	pid := m.focus

	next, _ := m.Update(termExitedMsg{pid: pid})
	m = next.(model)

	if len(m.terms) != 0 {
		t.Error("a shell that exited should not be held on to")
	}
	if m.focus != 0 {
		t.Error("focus should return to the navigator when the shell goes")
	}
}

func TestManyShellsCanRunInOneRepository(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	first := m.focus
	m = openShellIn(t, m, "/tmp")

	if len(m.terms) != 2 {
		t.Fatalf("terms = %d, want a repository to hold more than one shell", len(m.terms))
	}
	if m.focus == first {
		t.Error("a second shell should take the focus")
	}
}

func TestResizingReachesTheShell(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")

	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(model)

	if got := len(m.terms[m.focus].lines(40)); got == 0 {
		t.Error("the shell should still render after a resize")
	}
}

// --- what can be stepped into -------------------------------------------

func dimmed(m model, r navRow, selected bool) bool {
	return m.rowStyle(r, selected).GetForeground() == faintStyle.GetForeground()
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

func TestAShellScrnStartedIsNotDimmed(t *testing.T) {
	m := openShellIn(t, repoModel(), "/tmp")
	pid := m.focus
	next, _ := m.Update(procsMsg{procs: []Proc{{PID: pid, PPID: 1, Command: "sh", Dir: "/tmp"}}})
	m = next.(model)

	row := m.rows[1]
	if row.kind != rowProc || row.node.PID != pid {
		t.Fatalf("row = %+v, want the shell scrn started", row)
	}
	if dimmed(m, row, false) {
		t.Error("a shell scrn started can be returned to and should read that way")
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

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := next.(model)

	if got.statusErr {
		t.Error("declining to attach should not be reported as an error")
	}
	if !strings.Contains(footer(got), "did not start vim 900") {
		t.Errorf("footer = %q, want it to say why", footer(got))
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// startDaemonFor runs a daemon on a socket of this test's own, so tests never
// touch the one holding the user's real shells.
func startDaemonFor(t *testing.T) *daemon {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	// A unix socket path is capped near 104 bytes, and the per-test temp
	// directory alone is longer than that.
	dir, err := os.MkdirTemp("/tmp", "scrnd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	t.Setenv("SCRN_SOCKET", sock)

	d, err := listenDaemon(sock)
	if err != nil {
		t.Fatal(err)
	}
	go d.accept()
	t.Cleanup(d.stop)
	return d
}

// connected returns a model already talking to a daemon of this test's own.
func connected(t *testing.T, m model) model {
	t.Helper()
	startDaemonFor(t)

	s, err := openSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.conn.close() })

	next, _ := m.Update(daemonReadyMsg{session: s})
	return next.(model)
}

// pump feeds the daemon's messages into the model until want is satisfied.
func pump(t *testing.T, m model, want func(model) bool, d time.Duration) model {
	t.Helper()
	deadline := time.After(d)
	for !want(m) {
		select {
		case ev, ok := <-m.daemon.events:
			if !ok {
				t.Fatal("the daemon connection closed")
			}
			next, _ := m.Update(ev)
			m = next.(model)
		case <-deadline:
			t.Fatalf("timed out; terms=%d focus=%d", len(m.terms), m.focus)
		}
	}
	return m
}

func hasShell(m model) bool { return len(m.terms) > 0 && m.focus != 0 }
func paneHas(text string) func(model) bool {
	return func(m model) bool { return strings.Contains(strings.Join(m.terms[m.focus].lines(20), "\n"), text) }
}

// openShellIn opens a shell through the daemon and waits for its first screen.
func openShellIn(t *testing.T, m model, dir string) model {
	t.Helper()
	m = connected(t, m)
	m.daemon.open(dir, 40, 8)
	return pump(t, m, hasShell, 5*time.Second)
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
	return withProcList(90, 14, []Project{{Name: "tmp", Path: "/tmp"}}, nil)
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
	m = pump(t, send(m, "echo marker-in-the-pane"), paneHas("marker-in-the-pane"), 5*time.Second)

	if !strings.Contains(paneText(m), "marker-in-the-pane") {
		t.Errorf("pane should show the shell's output:\n%s", paneText(m))
	}
	if nav := navColumn(m); len(nav) == 0 || !strings.Contains(nav[0], "tmp") {
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
	m = pump(t, send(m, "echo still-visible"), paneHas("still-visible"), 5*time.Second)
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

	next, _ := m.Update(termGoneMsg{pid: pid})
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
	m.daemon.open("/tmp", 40, 8)
	m = pump(t, m, func(m model) bool { return len(m.terms) == 2 }, 5*time.Second)

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
	if m.daemon == nil {
		t.Error("resizing should have gone to the daemon holding the shell")
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

func TestNStartsAShellOnAnyRow(t *testing.T) {
	// Standing on a process scrn does not own is a fine reason to want a shell
	// where that process is working; it is only attaching that is impossible.
	m := withProcList(90, 14,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{{PID: 900, PPID: 1, Command: "vim", Dir: "/tmp"}})
	m.cursor = 1
	if m.attachable(m.rows[1]) {
		t.Fatal("setup: expected a row that cannot be attached to")
	}
	m = connected(t, m)
	m.cursor = 1

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = pump(t, next.(model), hasShell, 5*time.Second)

	if len(m.terms) != 1 || m.focused() == nil {
		t.Errorf("terms = %d, want one new shell with the keys", len(m.terms))
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

func TestNIsTheShellsOwnKeyOnceFocused(t *testing.T) {
	// Inside a shell, n is just a letter.
	m := openShellIn(t, repoModel(), "/tmp")
	before := len(m.terms)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if got := len(next.(model).terms); got != before {
		t.Errorf("terms = %d, want n typed into the shell rather than opening another", got)
	}
}

func TestFooterAdvertisesTheNewShellKey(t *testing.T) {
	if f := footer(sized(120, 8)); !strings.Contains(f, "n new shell") {
		t.Errorf("footer = %q, want the one way to make a process advertised", f)
	}
}

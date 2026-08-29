package main

import (
	"errors"
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
	m.daemon.open(dir, "", "", 40, 8)
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
	m.daemon.open("/tmp", "", "", 40, 8)
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
	if f := keysOf(sized(160, 24)); !strings.Contains(f, "n shell") {
		t.Errorf("footer = %q, want the one way to make a process advertised", f)
	}
}

func TestCStartsAClaudeInstanceScrnOwns(t *testing.T) {
	// The Claude instances already running are somebody else's; this is how
	// you get one that outlives the window and can be stepped back into.
	m := connected(t, repoModel())

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m = pump(t, next.(model), hasShell, 5*time.Second)

	if len(m.terms) != 1 {
		t.Fatalf("terms = %d, want the one instance", len(m.terms))
	}
}

func TestWhatIsStartedOutlivesItsCommand(t *testing.T) {
	// A command that exits should leave the shell behind rather than taking
	// the row with it, so a claude that quits does not close the pane.
	term, err := startTerm("/tmp", "echo the-command-ran", "", 40, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer term.close()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-term.output:
			if !ok {
				t.Fatal("the shell exited with the command it ran")
			}
		case <-deadline:
			t.Fatal("never saw the command run")
		}
		if strings.Contains(term.vt.Render(), "the-command-ran") {
			break
		}
	}

	// Still alive and still taking input once the command is done.
	term.write([]byte("echo still-here\n"))
	after := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-term.output:
			if !ok {
				t.Fatal("the shell exited after its command finished")
			}
		case <-after:
			t.Fatal("the shell stopped responding once its command finished")
		}
		if strings.Contains(term.vt.Render(), "still-here") {
			return
		}
	}
}

func TestFooterAdvertisesTheClaudeKey(t *testing.T) {
	if f := keysOf(sized(160, 24)); !strings.Contains(f, "c claude") {
		t.Errorf("footer = %q, want the claude key advertised", f)
	}
}

// --- what enter can reach ------------------------------------------------

// insideShell puts a process under a shell the daemon holds, the way a claude
// started with c sits under the shell that ran it.
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

func TestAnythingInsideAShellScrnHoldsIsBright(t *testing.T) {
	m := insideShell(t, 500)

	for _, r := range m.rows {
		if dimmed(m, r, false) {
			t.Errorf("row %q is dim, but enter reaches it through the shell", stripANSI(m.renderRow(r, false)))
		}
	}
}

func TestEnteringSomethingInsideAShellEntersTheShell(t *testing.T) {
	// The claude is drawing on the shell's terminal, so that is what attaching
	// to it means.
	m := connected(t, insideShell(t, 500))
	m.cursor = 1 // the claude row: the shell folded into it

	if r, _ := m.selected(); r.node.Command != "claude" {
		t.Fatalf("setup: selected %+v, want the claude row", r)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if got := next.(model).focus; got != 500 {
		t.Errorf("focus = %d, want the shell %d the claude is running inside", got, 500)
	}
}

func TestEnteringAGrandchildStillFindsTheShell(t *testing.T) {
	m := connected(t, insideShell(t, 500))
	m.cursor = 2 // rg, under the claude, under the shell

	if r, _ := m.selected(); r.node.Command != "rg" {
		t.Fatalf("setup: selected %+v, want the deepest row", r)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if got := next.(model).focus; got != 500 {
		t.Errorf("focus = %d, want the shell at the top of its tree", got)
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

func TestKillingAShellScrnHoldsGoesThroughTheDaemon(t *testing.T) {
	// Signalling it would do nothing: an interactive shell ignores SIGTERM.
	m := connected(t, repoModel())
	m.daemon.open("/tmp", "", "", 40, 8)
	m = pump(t, m, hasShell, 5*time.Second)
	pid := m.focus

	next, _ := m.Update(procsMsg{procs: []Proc{{PID: pid, PPID: 1, Command: "zsh", Dir: "/tmp"}}})
	m = next.(model)

	// Leave the shell first, or X is just a letter typed into it.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = next.(model)
	m.cursor = 1

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	m = next.(model)
	if m.pendingKill == nil {
		t.Fatal("X should arm a kill on a shell scrn holds")
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	m = next.(model)
	if cmd == nil {
		t.Fatal("confirming should report the outcome")
	}

	// The outcome is settled without a signal, because the daemon hung it up.
	out, ok := cmd().(killedMsg)
	if !ok || len(out.results) != 1 {
		t.Fatalf("outcome = %+v, want the one shell accounted for", out)
	}
	if out.results[0].err != nil {
		t.Errorf("result = %+v, want the hangup to have settled it", out.results[0])
	}
	m = pump(t, m, func(m model) bool { return len(m.terms) == 0 }, 5*time.Second)
}

func TestKillingSomethingInAShellTakesTheShellToo(t *testing.T) {
	// Quitting a Claude instance yourself leaves you at the prompt, which is
	// what the shell is there for. Killing it from here means being done with
	// the whole thing.
	m := connected(t, insideShell(t, 500))
	m.cursor = 1 // the claude row, which folded the shell into it

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)

	if got := targets(m.pendingKill); !sameInts(got, []int{501, 500}) {
		t.Errorf("targets = %v, want the claude and the shell it runs in", got)
	}
	if f := footer(m); !strings.Contains(f, "kill claude 501 and its shell?") {
		t.Errorf("footer = %q, want it to say the shell goes too", f)
	}
}

func TestTheShellIsHungUpWhileTheProcessIsSignalled(t *testing.T) {
	// scrn holds the shell, so it goes by having its terminal taken away. It
	// does not hold the claude, so that is signalled.
	m := connected(t, insideShell(t, 500))
	m.cursor = 1
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)

	var hungUp, signalled []int
	for _, n := range m.pendingKill.nodes {
		if _, mine := m.terms[n.PID]; mine {
			hungUp = append(hungUp, n.PID)
			continue
		}
		signalled = append(signalled, n.PID)
	}
	if !sameInts(hungUp, []int{500}) {
		t.Errorf("hung up %v, want the shell", hungUp)
	}
	if !sameInts(signalled, []int{501}) {
		t.Errorf("signalled %v, want the claude", signalled)
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

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
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

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
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

// --- what the process asks of the window ---------------------------------

func TestWhatTheProcessAsksOfTheWindowIsCarriedOut(t *testing.T) {
	// A program addresses its title and progress to the terminal it believes
	// it is in. That is scrn, and scrn is the one with a real window.
	t.Setenv("SHELL", "/bin/sh")
	term, err := startTerm("/tmp", "", "", 40, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer term.close()

	term.write([]byte("printf '\\033]0;a title\\007\\033]9;4;3;50\\007'\n"))

	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-term.output:
			if !ok {
				t.Fatal("the shell exited")
			}
		case <-deadline:
			title, progress := term.window()
			t.Fatalf("never saw them: title=%q progress=%q", title, progress)
		}
		if title, progress := term.window(); title == "0;a title" && progress == "9;4;3;50" {
			return
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
	if got := m.windowRequests(); !strings.Contains(got, "9;4;3;50") {
		t.Errorf("requests = %q, want the attached shell's progress", got)
	}
	m.focus = 800
	if got := m.windowRequests(); !strings.Contains(got, "9;4;1;10") {
		t.Errorf("requests = %q, want the newly attached shell's progress", got)
	}
}

func TestProgressIsClearedWhenNothingIsAttached(t *testing.T) {
	// Leaving a shell that was reporting progress must take the indicator with
	// it, or the terminal keeps showing work that is no longer being watched.
	m := repoModel()
	m.terms = map[int]*remoteTerm{700: {pid: 700, progress: "9;4;3;50"}}

	m.focus = 0
	if got := m.windowRequests(); got != "\x1b]9;4;0;\x07" {
		t.Errorf("requests = %q, want the progress cleared", got)
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

func TestTheProgressGoesOutAsItCameIn(t *testing.T) {
	// The payload already carries its own command number; prefixing another
	// makes a sequence no terminal will act on.
	m := repoModel()
	m.terms = map[int]*remoteTerm{700: {pid: 700, progress: "9;4;3;77"}}
	m.focus = 700

	if got := m.windowRequests(); got != "\x1b]9;4;3;77\x07" {
		t.Errorf("requests = %q, want the payload passed through unchanged", got)
	}
}

// --- replacing a daemon older than the build -----------------------------

// staleDaemon is a model told it is talking to an out-of-date daemon holding
// one shell, which is the only state R is offered in.
func staleDaemon(t *testing.T) model {
	t.Helper()
	m := connected(t, repoModel())
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/tmp"}}
	m.daemonStale = true
	return m
}

func TestRAsksBeforeEndingTheWorkItReplaces(t *testing.T) {
	m := staleDaemon(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = next.(model)

	if !m.pendingReplace {
		t.Fatal("R should ask before ending the shells keeping a daemon alive")
	}
	if f := footer(m); !strings.Contains(f, "replace the daemon, ending 1 shell?") {
		t.Errorf("footer = %q, want it to say what it is about to end", f)
	}
}

func TestConfirmingReplacesTheDaemon(t *testing.T) {
	m := staleDaemon(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	next, cmd := next.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = next.(model)

	if cmd == nil {
		t.Error("confirming should reconnect once the old daemon has gone")
	}
	if len(m.terms) != 0 || m.focus != 0 {
		t.Error("the shells went with the daemon; the window should not still hold them")
	}
	if m.daemonStale {
		t.Error("the daemon being replaced is no longer the stale one")
	}
}

func TestAnyOtherKeyLeavesTheDaemonAlone(t *testing.T) {
	m := staleDaemon(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	next, _ = next.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)

	if m.pendingReplace {
		t.Error("the question should be over")
	}
	if len(m.terms) != 1 {
		t.Error("cancelling should not have ended anything")
	}
	if !strings.Contains(footer(m), "left the daemon alone") {
		t.Errorf("footer = %q, want it to say nothing happened", footer(m))
	}
}

func TestROnACurrentDaemonDoesNothing(t *testing.T) {
	// Ending shells to swap a daemon that is already the right one would be
	// destroying work for nothing.
	m := connected(t, repoModel())
	m.terms = map[int]*remoteTerm{700: {pid: 700}}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = next.(model)

	if m.pendingReplace {
		t.Error("R should only be offered when there is an out-of-date daemon")
	}
	if !strings.Contains(footer(m), "the one this build expects") {
		t.Errorf("footer = %q, want it to say why nothing happened", footer(m))
	}
}

// --- starting what a project needs ---------------------------------------

// projectNeeding is a model over a real directory whose plan says what it
// needs, connected to a daemon of the test's own.
func projectNeeding(t *testing.T, plan string) (model, string) {
	t.Helper()
	dir := t.TempDir()
	if plan != "" {
		if err := writeFile(filepath.Join(dir, planFile), plan); err != nil {
			t.Fatal(err)
		}
	}
	m := withProcList(90, 20, []Project{{Name: "proj", Path: dir}}, nil)
	return connected(t, m), dir
}

func TestUStartsWhatTheProjectSaysItNeeds(t *testing.T) {
	m, _ := projectNeeding(t, "one: sleep 30\ntwo: sleep 30\n")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 2 }, 5*time.Second)

	got := map[string]bool{}
	for _, t := range m.terms {
		got[t.name] = true
	}
	if !got["one"] || !got["two"] {
		t.Errorf("started %v, want both entries by name", got)
	}
	if !strings.Contains(footer(m), "started one, two") {
		t.Errorf("footer = %q, want it to say what it started", footer(m))
	}
}

func TestUStartsOnlyWhatIsMissing(t *testing.T) {
	// It is a list to run, not a promise to keep, so running it again starts
	// only what has since stopped.
	m, dir := projectNeeding(t, "one: sleep 30\ntwo: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 2 }, 5*time.Second)

	// One of them stops, the way a dev server dies.
	for pid, term := range m.terms {
		if term.name == "one" {
			next, _ := m.Update(termGoneMsg{pid: pid})
			m = next.(model)
			break
		}
	}
	if len(m.terms) != 1 {
		t.Fatalf("terms = %d, want the one that is left", len(m.terms))
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 2 }, 5*time.Second)

	names := map[string]int{}
	for _, term := range m.terms {
		names[term.name]++
	}
	if names["one"] != 1 || names["two"] != 1 {
		t.Errorf("terms = %v, want one of each rather than a duplicate", names)
		_ = dir
	}
}

func TestUSaysSoWhenEverythingIsAlreadyRunning(t *testing.T) {
	m, _ := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = next.(model)
	if !strings.Contains(footer(m), "everything proj needs is running") {
		t.Errorf("footer = %q, want it to say there was nothing to do", footer(m))
	}
	if len(m.terms) != 1 {
		t.Errorf("terms = %d, want nothing started twice", len(m.terms))
	}
}

func TestUOnAProjectThatSaysNothingExplainsItself(t *testing.T) {
	m, _ := projectNeeding(t, "")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = next.(model)

	if len(m.terms) != 0 {
		t.Error("nothing should have been started")
	}
	if !strings.Contains(footer(m), "does not say what it needs") {
		t.Errorf("footer = %q, want it to explain why nothing happened", footer(m))
	}
}

func TestDStopsOnlyWhatThePlanStarted(t *testing.T) {
	// A shell opened by hand was not part of the list and is not swept up.
	m, dir := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	m.daemon.open(dir, "", "", 40, 8) // by hand, so unnamed
	m = pump(t, m, func(m model) bool { return len(m.terms) == 2 }, 5*time.Second)

	// Opening one by hand steps into it, so come back out before pressing a
	// key meant for the list.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = next.(model)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = next.(model)
	if m.pendingDown == nil {
		t.Fatal("d should ask before stopping")
	}
	if f := footer(m); !strings.Contains(f, "stop what proj started?") {
		t.Errorf("footer = %q, want it to say what it is about to stop", f)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	for _, term := range m.terms {
		if term.name != "" {
			t.Errorf("term %q survived, want only the one opened by hand", term.name)
		}
	}
}

func TestAnyOtherKeyLeavesThemRunning(t *testing.T) {
	m, _ := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	next, _ = next.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)

	if len(m.terms) != 1 {
		t.Error("cancelling should not have stopped anything")
	}
	if !strings.Contains(footer(m), "left them running") {
		t.Errorf("footer = %q, want it to say nothing happened", footer(m))
	}
}

func TestDOnAProjectWithNothingOfItsOwnSaysSo(t *testing.T) {
	m, _ := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = next.(model)

	if m.pendingDown != nil {
		t.Error("there is nothing to stop, so nothing to confirm")
	}
	if !strings.Contains(footer(m), "nothing in proj was started from its plan") {
		t.Errorf("footer = %q, want it to explain", footer(m))
	}
}

func TestTheRepoPaneShowsThePlanAsAChecklist(t *testing.T) {
	m, dir := projectNeeding(t, "one: sleep 30\ntwo: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 2 }, 5*time.Second)

	fs := repoFields(Project{Name: "proj", Path: dir}, 2, m.namesIn(dir))
	var needs []string
	for _, f := range fs {
		if f.label == "needs" || (f.label == "" && strings.Contains(f.value, "two")) {
			needs = append(needs, f.value)
		}
	}
	if len(needs) != 2 {
		t.Fatalf("needs = %v, want both entries listed", needs)
	}
	for _, n := range needs {
		if !strings.HasPrefix(n, "● ") {
			t.Errorf("entry %q should be marked as running", n)
		}
	}

	// And unmarked when nothing is up.
	fs = repoFields(Project{Name: "proj", Path: dir}, 0, nil)
	for _, f := range fs {
		if f.label == "needs" && !strings.HasPrefix(f.value, "○ ") {
			t.Errorf("entry %q should be marked as not running", f.value)
		}
	}
}

func TestTheKeysListUpAndDown(t *testing.T) {
	f := keysOf(sized(160, 24))
	for _, key := range []string{"u up", "d down"} {
		if !strings.Contains(f, key) {
			t.Errorf("keys = %q, want %q listed", f, key)
		}
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

// --- acting on a project while still looking it up -----------------------

// lookingUp is a model connected to a daemon, with a project being searched
// for and found.
func lookingUp(t *testing.T, filter string) model {
	t.Helper()
	dir := t.TempDir()
	m := connected(t, withProcList(90, 20, []Project{
		{Name: "alpha", Path: dir},
		{Name: "beta", Path: t.TempDir()},
	}, nil))
	m.showAll = false
	m = press(m, "/")
	for _, r := range filter {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	return m
}

func TestEnterOpensAShellInWhatTheFilterFound(t *testing.T) {
	// Finding a project and starting work in it is one act, not two.
	m := lookingUp(t, "alpha")
	if len(m.rows) != 1 {
		t.Fatalf("rows = %d, want the one match", len(m.rows))
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = pump(t, next.(model), hasShell, 5*time.Second)

	if m.typing {
		t.Error("the looking up should be over")
	}
	if len(m.terms) != 1 {
		t.Errorf("terms = %d, want a shell in what was found", len(m.terms))
	}
}

func TestCtrlRStartsAClaudeInWhatTheFilterFound(t *testing.T) {
	m := lookingUp(t, "alpha")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = pump(t, next.(model), hasShell, 5*time.Second)

	if len(m.terms) != 1 {
		t.Errorf("terms = %d, want one started", len(m.terms))
	}
}

func TestCtrlUStartsWhatAProjectNeedsAndLandsOnIt(t *testing.T) {
	// Starting what a project needs is the end of looking for it.
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, planFile), "one: sleep 30\ntwo: sleep 30\n"); err != nil {
		t.Fatal(err)
	}
	m := connected(t, withProcList(90, 20, []Project{{Name: "alpha", Path: dir}}, nil))
	m.showAll = false
	m = press(m, "/")
	m = typeFilter(m, "alpha")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 2 }, 5*time.Second)

	names := map[string]bool{}
	for _, term := range m.terms {
		names[term.name] = true
	}
	if !names["one"] || !names["two"] {
		t.Errorf("started %v, want everything the project needs", names)
	}
	if m.typing {
		t.Errorf("typing=%v, want the typing over", m.typing)
	}
	// The filter is held until the processes are in the tree, so that the
	// project does not drop out of the narrowed list before they arrive.
	m = pump(t, m, func(m model) bool { return m.filter == "" }, 5*time.Second)
	if r, ok := m.selected(); !ok || r.project.Path != dir {
		t.Errorf("selected %+v, want the cursor left on the project", r.project)
	}
}

func TestCtrlUOnAProjectThatNeedsNothingSaysSo(t *testing.T) {
	m := lookingUp(t, "alpha")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
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

func TestWhatAnActionReportsIsVisibleWhileTyping(t *testing.T) {
	// Acting from the search is the point of it, and an action that says
	// nothing looks like one that did nothing.
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, planFile), "one: sleep 30\n"); err != nil {
		t.Fatal(err)
	}
	m := connected(t, withProcList(160, 24, []Project{{Name: "alpha", Path: dir}}, nil))
	m.showAll = false
	m = typeFilter(press(m, "/"), "alpha")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	if f := footer(m); !strings.Contains(f, "started one") {
		t.Errorf("footer = %q, want what it just did", f)
	}
}

func TestTypingOnClearsWhatWasSaidAboutTheLastProject(t *testing.T) {
	// ctrl+r reports and leaves the typing alone, so the search carries on.
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

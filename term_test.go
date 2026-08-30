package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
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

func TestADaemonErrorDoesNotCostTheConnection(t *testing.T) {
	// An error is the daemon answering, which is the opposite of it being
	// gone. A failed open must arrive as a report, with the connection and
	// every shell this window can see intact.
	m := openShellIn(t, repoModel(), "/tmp")
	m.daemon.open(filepath.Join(t.TempDir(), "gone"), "", "", 40, 8)
	m = pump(t, m, func(m model) bool { return m.statusErr }, 5*time.Second)

	if m.daemon == nil {
		t.Fatal("daemon = nil, want the connection kept through a failed ask")
	}
	if len(m.terms) != 1 {
		t.Errorf("terms = %d, want the shell already open to survive the report", len(m.terms))
	}
	if m.status == "" {
		t.Error("status is empty, want the daemon's error shown")
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

// --- the screen keeps its grid ------------------------------------------

// paddedTerm is an emulator with no shell behind it, fed by hand: the grid
// contract is the emulator's and the padding's, and a real pty would only add
// timing to a test about geometry.
func paddedTerm(width, height int) *terminal {
	return &terminal{vt: vt.NewSafeEmulator(width, height), cols: width}
}

func TestEveryScreenRowIsAsWideAsThePane(t *testing.T) {
	term := paddedTerm(12, 3)
	term.vt.Write([]byte("ab "))

	for i, row := range strings.Split(term.screen(), "\n") {
		if got := lipgloss.Width(row); got != 12 {
			t.Errorf("row %d is %d columns wide, want 12", i, got)
		}
	}
}

func TestTheCursorColumnIsACellTheRowActuallyHas(t *testing.T) {
	// A trailing space is the whole point: it moves the cursor without
	// leaving anything visible behind it, which is exactly what a render
	// that trims trailing blanks loses.
	term := paddedTerm(12, 3)
	term.vt.Write([]byte("ab "))

	msg := term.screenMsg()
	if msg.CursorX != 3 {
		t.Fatalf("cursor at column %d after typing three cells, want 3", msg.CursorX)
	}
	row := strings.Split(msg.Screen, "\n")[msg.CursorY]
	if got := lipgloss.Width(row); got <= msg.CursorX {
		t.Errorf("the row is %d columns wide with the cursor at %d — the cell under the cursor is not in it", got, msg.CursorX)
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

func TestSStartsAShellOnAnyRow(t *testing.T) {
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

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
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

func TestSIsTheShellsOwnKeyOnceFocused(t *testing.T) {
	// Inside a shell, s is just a letter.
	m := openShellIn(t, repoModel(), "/tmp")
	before := len(m.terms)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if got := len(next.(model).terms); got != before {
		t.Errorf("terms = %d, want s typed into the shell rather than opening another", got)
	}
}

func TestFooterAdvertisesTheNewShellKey(t *testing.T) {
	if f := keysOf(sized(160, 24)); !strings.Contains(f, "s shell") {
		t.Errorf("footer = %q, want the one way to make a process advertised", f)
	}
}

func TestAStartsAnAgentScrnOwns(t *testing.T) {
	// The Claude instances already running are somebody else's; this is how
	// you get one that outlives the window and can be stepped back into.
	m := connected(t, repoModel())

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
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
	typeAt(term, "echo still-here\n")
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

func TestFooterAdvertisesTheAgentKey(t *testing.T) {
	if f := keysOf(sized(160, 24)); !strings.Contains(f, "a agent") {
		t.Errorf("footer = %q, want the agent key advertised", f)
	}
}

// --- what enter can reach ------------------------------------------------

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

	typeAt(term, "printf '\\033]0;a title\\007\\033]9;4;3;50\\007'\n")

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

func TestRStartsWhatTheProjectSaysItNeeds(t *testing.T) {
	m, _ := projectNeeding(t, "one: sleep 30\ntwo: sleep 30\n")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
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

func TestRStartsOnlyWhatIsMissing(t *testing.T) {
	// It is a list to run, not a promise to keep, so running it again starts
	// only what has since stopped.
	m, dir := projectNeeding(t, "one: sleep 30\ntwo: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
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

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
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

func TestRSaysSoWhenEverythingIsAlreadyRunning(t *testing.T) {
	m, _ := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(model)
	if !strings.Contains(footer(m), "everything proj needs is running") {
		t.Errorf("footer = %q, want it to say there was nothing to do", footer(m))
	}
	if len(m.terms) != 1 {
		t.Errorf("terms = %d, want nothing started twice", len(m.terms))
	}
}

func TestROnAProjectThatSaysNothingExplainsItself(t *testing.T) {
	m, _ := projectNeeding(t, "")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(model)

	if len(m.terms) != 0 {
		t.Error("nothing should have been started")
	}
	if !strings.Contains(footer(m), "does not say what it needs") {
		t.Errorf("footer = %q, want it to explain why nothing happened", footer(m))
	}
}

func TestXOnARepoStopsEverythingRunningInIt(t *testing.T) {
	// The shell opened by hand goes too: x on a repository means being done
	// with the repository, not with the part of it a plan happened to start.
	m, dir := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	m.daemon.open(dir, "", "", 40, 8) // by hand, so unnamed
	m = pump(t, m, func(m model) bool { return len(m.terms) == 2 }, 5*time.Second)

	// Opening one by hand steps into it, so come back out before pressing a
	// key meant for the list.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = next.(model)

	// The kill covers what the scan has seen, so let it see both shells.
	procs := make([]Proc, 0, 2)
	for pid := range m.terms {
		procs = append(procs, Proc{PID: pid, PPID: 1, Command: "zsh", Dir: dir})
	}
	next, _ = m.Update(procsMsg{procs: procs})
	m = next.(model)
	m.cursor = 0 // back onto the repo row

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)
	if m.pendingKill == nil {
		t.Fatal("x should ask before killing")
	}
	if f := footer(m); !strings.Contains(f, "kill 2 processes in proj?") {
		t.Errorf("footer = %q, want it to say what it is about to clear out", f)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 0 }, 5*time.Second)
}

func TestAnyOtherKeyLeavesThemRunning(t *testing.T) {
	m, dir := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	procs := make([]Proc, 0, 1)
	for pid := range m.terms {
		procs = append(procs, Proc{PID: pid, PPID: 1, Command: "zsh", Dir: dir})
	}
	next, _ = m.Update(procsMsg{procs: procs})
	m = next.(model)
	m.cursor = 0

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	next, _ = next.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)

	if len(m.terms) != 1 {
		t.Error("cancelling should not have stopped anything")
	}
	if !strings.Contains(footer(m), "cancelled") {
		t.Errorf("footer = %q, want it to say nothing happened", footer(m))
	}
}

func TestXOnAProjectWithNothingRunningSaysSo(t *testing.T) {
	// A plan is a list to run, not something running: until r is pressed
	// there is nothing for a kill to act on.
	m, _ := projectNeeding(t, "one: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)

	if m.pendingKill != nil {
		t.Error("there is nothing to kill, so nothing to confirm")
	}
	if !strings.Contains(footer(m), "nothing running in proj") {
		t.Errorf("footer = %q, want it to explain", footer(m))
	}
}

func TestTheRepoPaneShowsThePlanAsAChecklist(t *testing.T) {
	m, dir := projectNeeding(t, "one: sleep 30\ntwo: sleep 30\n")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
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

func TestCtrlAStartsAnAgentInWhatTheFilterFound(t *testing.T) {
	m := lookingUp(t, "alpha")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = pump(t, next.(model), hasShell, 5*time.Second)

	if len(m.terms) != 1 {
		t.Errorf("terms = %d, want one started", len(m.terms))
	}
}

func TestCtrlRStartsWhatAProjectNeedsAndLandsOnIt(t *testing.T) {
	// Starting what a project needs is the end of looking for it.
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, planFile), "one: sleep 30\ntwo: sleep 30\n"); err != nil {
		t.Fatal(err)
	}
	m := connected(t, withProcList(90, 20, []Project{{Name: "alpha", Path: dir}}, nil))
	m.showAll = false
	m = press(m, "/")
	m = typeFilter(m, "alpha")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
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

func TestCtrlROnAProjectThatNeedsNothingSaysSo(t *testing.T) {
	m := lookingUp(t, "alpha")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
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

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
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

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = pump(t, next.(model), func(m model) bool { return len(m.terms) == 1 }, 5*time.Second)

	if f := footer(m); !strings.Contains(f, "started one") {
		t.Errorf("footer = %q, want what it just did", f)
	}
}

func TestTypingOnClearsWhatWasSaidAboutTheLastProject(t *testing.T) {
	// ctrl+a reports and leaves the typing alone, so the search carries on.
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

// typeAt types a string at a shell the way a window does: one keystroke at a
// time, handed to the emulator, which decides the bytes.
func typeAt(t *terminal, s string) {
	for _, r := range s {
		k := &keyPress{Code: r, Text: string(r)}
		if r == '\n' {
			k = &keyPress{Code: uv.KeyEnter}
		}
		t.send(message{Key: k})
	}
}

func TestWhatTheShellReceivesFollowsTheModesItAskedFor(t *testing.T) {
	// The whole journey: a keystroke in the window, translated, handed to the
	// emulator, out of the pty, and shown by a program that prints the bytes it
	// was given. What those bytes are depends on what the program has asked
	// for, which is why the window sends the key rather than the bytes.
	term, err := startTerm("/tmp", "", "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer term.close()

	await := func(what, when string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(term.vt.Render(), what) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: never saw %q; the screen was:\n%s", when, what, term.vt.Render())
	}

	// cat -v prints what it is given rather than acting on it.
	typeAt(term, "PS1=; cat -v\n")
	waitFor(t, "cat to be reading", func() bool {
		return strings.Contains(term.vt.Render(), "cat -v")
	})

	term.send(message{Key: keyEvent(tea.KeyMsg{Type: tea.KeyUp})})
	await("^[[A", "an arrow with nothing asked for")

	// Application cursor keys, which vim, readline and less all turn on.
	term.vt.Write([]byte("\x1b[?1h"))
	term.send(message{Key: keyEvent(tea.KeyMsg{Type: tea.KeyUp})})
	await("^[OA", "an arrow under application cursor keys")

	// And a click, with the program asking to hear about the mouse at all.
	term.vt.Write([]byte("\x1b[?1000h\x1b[?1006h"))
	term.send(message{Mouse: mouseEvent(
		tea.MouseMsg{X: navWidth + 1 + 9, Y: 4, Button: tea.MouseButtonLeft},
		navWidth+1, 0)})
	await("^[[<0;10;5M", "a left click nine columns into the pane")
}

// --- the transcript --------------------------------------------------------

func TestHistoryIsWhatScrolledOffTheTop(t *testing.T) {
	// Five rows of screen, twelve lines printed: the first seven are in the
	// scrollback and the rest are still the screen's business.
	term := &terminal{vt: vt.NewSafeEmulator(20, 5)}
	for i := 1; i <= 12; i++ {
		term.vt.Write([]byte("line-" + strconv.Itoa(i) + "\r\n"))
	}

	h := term.history()
	if !strings.Contains(h, "line-1") || !strings.Contains(h, "line-7") {
		t.Errorf("history = %q, want the lines that scrolled away", h)
	}
	if strings.Contains(h, "line-12") {
		t.Errorf("history = %q, want nothing the screen still shows", h)
	}
}

func TestHistoryOfAQuietShellIsEmpty(t *testing.T) {
	term := &terminal{vt: vt.NewSafeEmulator(20, 5)}
	term.vt.Write([]byte("just a prompt"))

	if h := term.history(); h != "" {
		t.Errorf("history = %q, want nothing before anything scrolls", h)
	}
}

func TestAScreenSaysWhatKindOfPaneItIs(t *testing.T) {
	term := &terminal{vt: vt.NewSafeEmulator(20, 5), cols: 20}
	term.watchModes()

	if m := term.screenMsg(); m.Scrollback != 0 || m.MouseOn || m.Alt {
		t.Errorf("a fresh pane should have nothing scrolled and nothing asked for, got %+v", m)
	}

	for i := 1; i <= 12; i++ {
		term.vt.Write([]byte("line-" + strconv.Itoa(i) + "\r\n"))
	}
	if m := term.screenMsg(); m.Scrollback == 0 {
		t.Error("a pane that has scrolled should say so")
	}

	term.vt.Write([]byte("\x1b[?1000h\x1b[?1006h"))
	if m := term.screenMsg(); !m.MouseOn {
		t.Error("a program that asked for the mouse should be reported")
	}
	term.vt.Write([]byte("\x1b[?1000l"))
	if m := term.screenMsg(); m.MouseOn {
		t.Error("a program that let the mouse go should be reported too")
	}

	term.vt.Write([]byte("\x1b[?1049h"))
	if m := term.screenMsg(); !m.Alt {
		t.Error("the alternate screen should be reported")
	}
}

// wheelUp is one upward wheel notch in pane coordinates.
func wheelUp() *mousePress {
	return &mousePress{X: 1, Y: 1, Button: int(uv.MouseWheelUp), Action: actPress}
}

// drainVT collects what the emulator writes back until want appears or the
// wait runs out, because the emulator's answers block until they are read.
func drainVT(t *testing.T, term *terminal, want string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		var out []byte
		for {
			n, err := term.vt.Read(buf)
			out = append(out, buf[:n]...)
			if strings.Contains(string(out), want) || err != nil {
				got <- string(out)
				return
			}
		}
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(2 * time.Second):
		t.Fatalf("never saw %q from the emulator", want)
		return ""
	}
}

func TestAWheelOnTheAlternateScreenBecomesArrows(t *testing.T) {
	// less and man scroll under alternate scroll without ever asking for the
	// mouse, and scrn is the terminal that has to provide it.
	term := &terminal{vt: vt.NewSafeEmulator(20, 5)}
	term.watchModes()
	term.vt.Write([]byte("\x1b[?1049h"))

	go term.send(message{Mouse: wheelUp()})
	if out := drainVT(t, term, "\x1b[A\x1b[A\x1b[A"); !strings.Contains(out, "\x1b[A\x1b[A\x1b[A") {
		t.Errorf("emulator wrote %q, want three arrow presses", out)
	}
}

func TestAWheelStaysAWheelWhenTheProgramAskedForIt(t *testing.T) {
	term := &terminal{vt: vt.NewSafeEmulator(20, 5)}
	term.watchModes()
	term.vt.Write([]byte("\x1b[?1049h\x1b[?1000h\x1b[?1006h"))

	go term.send(message{Mouse: wheelUp()})
	// SGR reports a wheel-up as button 64.
	if out := drainVT(t, term, "\x1b[<64;"); !strings.Contains(out, "\x1b[<64;") {
		t.Errorf("emulator wrote %q, want the wheel reported to the program", out)
	}
}

// wheelMsg is a wheel notch over the pane, in window coordinates.
func wheelMsg(btn tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{X: navWidth + 2, Y: 2, Button: btn, Action: tea.MouseActionPress}
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
	next, _ := m.Update(wheelMsg(tea.MouseButtonWheelUp))
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

	next, _ := m.Update(wheelMsg(tea.MouseButtonWheelDown))
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

func TestCtrlOWhileReadingGoesAllTheWayOut(t *testing.T) {
	m := readingBack(t)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = next.(model)

	if m.scroll != nil || m.focus != 0 {
		t.Errorf("scroll = %v focus = %d, want the reading and the shell both left", m.scroll, m.focus)
	}
}

func TestTheWheelIsTheProgramsWhenItAskedForIt(t *testing.T) {
	m := watchingTail(t)
	m.terms[700].mouse = true

	next, _ := m.Update(wheelMsg(tea.MouseButtonWheelUp))
	if next.(model).scroll != nil {
		t.Error("a program listening for the mouse should keep its wheel")
	}
}

func TestTheWheelDoesNotReadOverTheAlternateScreen(t *testing.T) {
	m := watchingTail(t)
	m.terms[700].alt = true

	next, _ := m.Update(wheelMsg(tea.MouseButtonWheelUp))
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

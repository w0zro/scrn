package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// sized returns a model laid out for the given terminal dimensions. The
// startup scan is taken to have answered, which is the state every test that
// seeds procs directly is standing in.
func sized(w, h int) model {
	m, _ := newModel().Update(tea.WindowSizeMsg{Width: w, Height: h})
	s := m.(model)
	s.scanning = false
	return s
}

// withProcs builds a model from repos and one process per given directory.
func withProcs(w, h int, projects []Project, dirs []string) model {
	procs := make([]Proc, len(dirs))
	for i, d := range dirs {
		procs[i] = Proc{PID: 100 + i, PPID: 1, Command: "proc", Dir: d}
	}
	return withProcList(w, h, projects, procs)
}

// withProcList builds a model showing every repository. The narrowed view is
// the default in the app, so tests that want it call narrowed().
func withProcList(w, h int, projects []Project, procs []Proc) model {
	m := sized(w, h)
	m.showAll = true
	m.projects, m.procs = projects, procs
	m.rebuild()
	return m
}

// narrowed flips the model to the running-only view, as Update does.
func narrowed(m model) model {
	m.showAll = false
	m.rebuild()
	return m
}

// typed is a printable keystroke as the terminal would deliver it.
func typed(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: []rune(s)[0], Text: s}
}

// press sends a key and returns the resulting model.
func press(m model, key string) model {
	var msg tea.KeyPressMsg
	switch key {
	case "up":
		msg = tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		msg = tea.KeyPressMsg{Code: tea.KeyDown}
	default:
		msg = typed(key)
	}
	next, _ := m.Update(msg)
	return next.(model)
}

// killed and killFailed are the outcome of signalling one process, shaped the
// way killTree reports it.
func killed(command string, pid int) killedMsg {
	return killedMsg{
		subject: command + " " + strconv.Itoa(pid),
		results: []killResult{{command: command, pid: pid}},
	}
}

func killFailed(command string, pid int, err error) killedMsg {
	msg := killed(command, pid)
	msg.results[0].err = err
	return msg
}

// targets lists what a pending kill would signal, in the order it would.
func targets(req *killRequest) []int {
	if req == nil {
		return nil
	}
	return pids(req.nodes)
}

// splitRow cuts a body row into its navigator and detail halves at the pane
// divider, which sits at a fixed column. It cannot search for "│": the process
// tree draws the same rune as a continuation rule.
func splitRow(row string) (nav, detail string) {
	r := []rune(row)
	if len(r) <= navWidth {
		return row, ""
	}
	return string(r[:navWidth]), string(r[navWidth+1:])
}

// bodyRows returns every row of the view.
func bodyRows(m model) []string {
	all := strings.Split(m.View().Content, "\n")
	out := make([]string, 0, len(all))
	for _, ln := range all {
		out = append(out, stripANSI(ln))
	}
	return out
}

// navColumn returns the non-blank navigator rows: scrn's own name and keys
// bracket them in that column and are not part of the list.
func navColumn(m model) []string {
	rows := bodyRows(m)
	end := len(rows) - len(m.trimmedHint(m.height))
	if end < 1 {
		return nil
	}

	var out []string
	for _, row := range rows[1:end] {
		nav, _ := splitRow(row)
		if nav = strings.TrimRight(nav, " "); strings.TrimSpace(nav) != "" {
			out = append(out, nav)
		}
	}
	return out
}

// detailColumn returns the non-blank pane rows. The pane runs the whole height
// of the window, so none of them are skipped.
func detailColumn(m model) []string {
	var out []string
	for _, row := range bodyRows(m) {
		_, detail := splitRow(row)
		if detail = strings.TrimRight(detail, " "); strings.TrimSpace(detail) != "" {
			out = append(out, detail)
		}
	}
	return out
}

func wantRows(t *testing.T, got, want []string) {
	t.Helper()
	for i, w := range want {
		if i >= len(got) || !strings.HasPrefix(got[i], w) {
			t.Fatalf("row %d = %q, want prefix %q\nfull:\n%s", i, lineAt(got, i), w, strings.Join(got, "\n"))
		}
	}
}

func lineAt(ls []string, i int) string {
	if i < len(ls) {
		return ls[i]
	}
	return "<missing>"
}

// --- layout ---------------------------------------------------------------

func TestViewPutsScrnInTopLeft(t *testing.T) {
	lines := strings.Split(sized(80, 24).View().Content, "\n")
	if got := len(lines); got != 24 {
		t.Fatalf("view height = %d lines, want 24", got)
	}
	if !strings.HasPrefix(stripANSI(lines[0]), " scrn") {
		t.Errorf("first line = %q, want the masthead in the gutter every row wears", lines[0])
	}
	if nav, _ := splitRow(stripANSI(lines[1])); strings.TrimSpace(nav) != "" {
		t.Errorf("second line = %q, want the blank row under the masthead", lines[1])
	}
}

func TestNavPaneOccupiesItsColumn(t *testing.T) {
	lines := strings.Split(sized(80, 24).View().Content, "\n")
	for i := 1; i < len(lines)-1; i++ {
		row := []rune(stripANSI(lines[i]))
		if len(row) <= navWidth || row[navWidth] != '│' {
			t.Fatalf("row %d: no divider at column %d: %q", i, navWidth, string(row))
		}
	}
}

func TestDetailPaneDroppedWhenTooNarrow(t *testing.T) {
	view := stripANSI(sized(navWidth+paneMin, 24).View().Content)
	if strings.Contains(view, "│") {
		t.Errorf("detail pane drawn with less than %d columns of pane:\n%s", paneMin, view)
	}
}

func TestViewFitsShortTerminals(t *testing.T) {
	for _, h := range []int{0, 1, 2, 3} {
		got := len(strings.Split(sized(80, h).View().Content, "\n"))
		if got > 3 && got > h {
			t.Errorf("height %d: view = %d lines, overflows", h, got)
		}
	}
}

// --- navigator contents ---------------------------------------------------

func TestNavListsRepoNames(t *testing.T) {
	m := withProcList(80, 8, []Project{{Name: "alpha"}, {Name: "beta"}}, nil)
	wantRows(t, navColumn(m), []string{"▸alpha", " beta"})
}

func TestNavTruncatesLongNames(t *testing.T) {
	m := withProcList(80, 8, []Project{{Name: strings.Repeat("x", 100)}}, nil)
	if got := len([]rune(navColumn(m)[0])); got > navWidth {
		t.Errorf("row is %d columns wide, want at most %d", got, navWidth)
	}
}

func TestQualifiedNamesKeepTheirRepoName(t *testing.T) {
	m := withProcList(80, 8,
		[]Project{{Name: "w0zro/archive/checklists.org/checklists-api"}}, nil)

	row := navColumn(m)[0]
	if !strings.Contains(row, "checklists-api") {
		t.Errorf("row = %q, want the repo name to survive truncation", row)
	}
	if got := len([]rune(row)); got > navWidth {
		t.Errorf("row is %d columns, want at most %d: %q", got, navWidth, row)
	}
}

func TestNavShowsScanError(t *testing.T) {
	m := sized(80, 6)
	m.err = errors.New("boom")
	if !strings.Contains(strings.Join(navColumn(m), " "), "boom") {
		t.Errorf("scan errors should be visible:\n%s", strings.Join(navColumn(m), "\n"))
	}
}

func TestNavShowsEmptyProjectsDir(t *testing.T) {
	m := withProcList(80, 6, []Project{}, nil)
	if !strings.Contains(strings.Join(navColumn(m), " "), "no repositories") {
		t.Error("an empty projects dir should say so")
	}
}

// --- process trees --------------------------------------------------------

func TestARunThatNeverBranchesIsOneRow(t *testing.T) {
	// A shell that started a claude that started a go build is one thing
	// happening, and the row is named for the claude: the shell is what got
	// there and the go is what it reached for.
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "claude", Dir: "/p/scrn"},
			{PID: 30, PPID: 20, Command: "go", Dir: "/p/scrn/cmd"},
		},
	)
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ claude"})

	if r, _ := m.rows[1], 0; r.chain().PID != 10 {
		t.Errorf("chain starts at %d, want the shell at the top of the run", r.chain().PID)
	}
}

func TestASameCommandForkingItselfIsStillOneRow(t *testing.T) {
	// nvim starts a second nvim; that is one editor, not two.
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
			{PID: 21, PPID: 20, Command: "nvim", Dir: "/p/scrn"},
		},
	)
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ nvim"})
}

func TestARunStopsFoldingWhereItBranches(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
			{PID: 30, PPID: 10, Command: "zig", Dir: "/p/scrn"},
		},
	)
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ zsh", "   ├─ nvim", "   └─ zig"})
}

func TestNavDrawsSiblingsWithContinuationRules(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "vim", Dir: "/p/scrn"},
			{PID: 30, PPID: 10, Command: "zig", Dir: "/p/scrn"},
			{PID: 40, PPID: 20, Command: "fmt", Dir: "/p/scrn"},
			{PID: 50, PPID: 20, Command: "lint", Dir: "/p/scrn"},
		},
	)
	wantRows(t, navColumn(m), []string{
		"▸scrn", " └─ zsh", "   ├─ vim", "   │ ├─ fmt", "   │ └─ lint", "   └─ zig",
	})
}

func TestProcessesGoUnderTheInnermostRepo(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "outer", Path: "/p/outer"}, {Name: "inner", Path: "/p/outer/inner"}},
		[]Proc{{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/outer/inner/src"}},
	)
	if n := len(m.byPlace["/p/outer"]); n != 0 {
		t.Errorf("outer repo got %d processes, want 0; the nested repo owns it", n)
	}
	if n := len(m.byPlace["/p/outer/inner"]); n != 1 {
		t.Errorf("inner repo got %d processes, want 1", n)
	}
}

// --- the "." toggle -------------------------------------------------------

func TestNavStartsNarrowedToRunningRepos(t *testing.T) {
	if newModel().showAll {
		t.Error("scrn should open on the repositories with something running")
	}

	m := sized(80, 10)
	m.projects = []Project{{Name: "busy", Path: "/p/busy"}, {Name: "idle", Path: "/p/idle"}}
	m.procs = []Proc{{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/busy"}}
	m.rebuild()

	col := strings.Join(navColumn(m), "\n")
	if !strings.Contains(col, "busy") {
		t.Errorf("a repo with a process should be listed at startup:\n%s", col)
	}
	if strings.Contains(col, "idle") {
		t.Errorf("an idle repo should not be listed at startup:\n%s", col)
	}
}

func TestNarrowedShowsOnlyReposWithProcesses(t *testing.T) {
	m := narrowed(withProcs(80, 10,
		[]Project{
			{Name: "busy", Path: "/p/busy"},
			{Name: "idle", Path: "/p/idle"},
			{Name: "nested", Path: "/p/nested"},
		},
		[]string{"/p/busy", "/p/nested/cmd/x", "/elsewhere"},
	))

	col := strings.Join(navColumn(m), "\n")
	if !strings.Contains(col, "busy") || !strings.Contains(col, "nested") {
		t.Errorf("repos with processes should be listed:\n%s", col)
	}
	if strings.Contains(col, "idle") {
		t.Errorf("a repo with nothing running should be hidden:\n%s", col)
	}
}

func TestShowAllIncludesIdleRepos(t *testing.T) {
	m := withProcs(80, 8,
		[]Project{{Name: "busy", Path: "/p/busy"}, {Name: "idle", Path: "/p/idle"}},
		[]string{"/p/busy"},
	)
	if !strings.Contains(strings.Join(navColumn(m), "\n"), "idle") {
		t.Error("showing all should include idle repos")
	}
}

func TestNarrowedWithNothingRunningExplainsItself(t *testing.T) {
	m := narrowed(withProcs(80, 8, []Project{{Name: "idle", Path: "/p/idle"}}, nil))

	col := strings.Join(navColumn(m), "\n")
	if !strings.Contains(col, "nothing running") {
		t.Errorf("an empty narrowed list should say why:\n%s", col)
	}
	if !strings.Contains(col, "show all") {
		t.Errorf("an empty narrowed list should say how to get back:\n%s", col)
	}
}

func TestAToggleRoundTrips(t *testing.T) {
	// Starts narrowed, as the app does.
	m := narrowed(withProcs(80, 8,
		[]Project{{Name: "busy", Path: "/p/busy"}, {Name: "idle", Path: "/p/idle"}},
		[]string{"/p/busy"},
	))

	m = press(m, ".")
	if !strings.Contains(strings.Join(navColumn(m), "\n"), "idle") {
		t.Error(". should bring every repo into the list")
	}
	m = press(m, ".")
	if strings.Contains(strings.Join(navColumn(m), "\n"), "idle") {
		t.Error(". should narrow back to running repos")
	}
}

func TestNarrowingRescansProcesses(t *testing.T) {
	wide := sized(80, 8)
	wide.showAll = true

	_, cmd := wide.Update(typed("."))
	if cmd == nil {
		t.Error("narrowing should rescan processes so the list is current")
	}
}

func TestTheKeysListTheToggle(t *testing.T) {
	// Both sides at once: the modal describes the pair rather than tracking
	// which view the next press would show.
	if !strings.Contains(keysOf(sized(160, 24)), ". all · running") {
		t.Error("the keys should mention the all/running toggle")
	}
}

func TestProcScanFailureKeepsRepoList(t *testing.T) {
	m := withProcs(80, 8, []Project{{Name: "alpha", Path: "/p/alpha"}}, []string{"/p/alpha"})
	next, _ := m.Update(procsMsg{err: errors.New("lsof exploded")})
	if !strings.Contains(strings.Join(navColumn(next.(model)), "\n"), "alpha") {
		t.Error("a failed process scan should not blank the repo list")
	}
}

// --- cursor ---------------------------------------------------------------

func threeRepos(h int) model {
	return withProcList(80, h, []Project{
		{Name: "a", Path: "/p/a"}, {Name: "b", Path: "/p/b"}, {Name: "c", Path: "/p/c"},
	}, nil)
}

func TestCursorStartsOnTheFirstRow(t *testing.T) {
	if c := threeRepos(10).cursor; c != 0 {
		t.Errorf("cursor = %d, want 0", c)
	}
}

func TestCursorMovesWithArrowsAndJK(t *testing.T) {
	for _, key := range []string{"down", "j", "tab"} {
		if c := press(threeRepos(10), key).cursor; c != 1 {
			t.Errorf("%q moved cursor to %d, want 1", key, c)
		}
	}
	for _, key := range []string{"up", "k"} {
		m := press(press(threeRepos(10), "down"), key)
		if m.cursor != 0 {
			t.Errorf("%q moved cursor to %d, want 0", key, m.cursor)
		}
	}
}

func TestCursorCyclesAtBothEnds(t *testing.T) {
	m := threeRepos(10)
	if c := press(m, "up").cursor; c != 2 {
		t.Errorf("up from the top went to %d, want 2 (wraps to the end)", c)
	}

	m = press(press(press(m, "down"), "down"), "down")
	if m.cursor != 0 {
		t.Errorf("down past the end went to %d, want 0 (wraps to the top)", m.cursor)
	}
}

func TestCursorWalksProcessesToo(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"}},
	)
	m = press(m, "down")

	r, ok := m.selected()
	if !ok || r.kind != rowProc || r.node.PID != 10 {
		t.Errorf("selected = %+v, want the process row", r)
	}
	wantRows(t, navColumn(m), []string{" scrn", "▸└─ zsh"})
}

func TestCursorOnEmptyListDoesNotPanic(t *testing.T) {
	m := withProcList(80, 8, nil, nil)
	press(press(m, "down"), "up") // must not panic
}

// --- scrolling ------------------------------------------------------------

func manyRepos(n, h int) model {
	ps := make([]Project, n)
	for i := range ps {
		ps[i] = Project{Name: string(rune('a' + i)), Path: "/p/" + string(rune('a'+i))}
	}
	return withProcList(80, h, ps, nil)
}

func TestScrollFollowsCursorPastTheBottom(t *testing.T) {
	m := manyRepos(10, 6) // 3 body rows, under the masthead, blank and chip
	for range 3 {
		m = press(m, "down")
	}

	if m.offset != 1 {
		t.Errorf("offset = %d, want 1; the window should follow the cursor by one row", m.offset)
	}
	wantRows(t, navColumn(m), []string{" b", " c", "▸d"})
}

func TestScrollKeepsCursorVisibleAfterWrap(t *testing.T) {
	m := press(manyRepos(10, 5), "up") // wraps to the last row

	col := navColumn(m)
	if !strings.HasPrefix(col[len(col)-1], "▸j") {
		t.Errorf("after wrapping to the end the cursor should be on screen:\n%s", strings.Join(col, "\n"))
	}
}

func TestScrollStopsAtTheLastRow(t *testing.T) {
	m := manyRepos(10, 5)
	for range 9 {
		m = press(m, "down")
	}
	if want := len(m.rows) - m.bodyHeight(); m.offset != want {
		t.Errorf("offset = %d, want %d; the window should not scroll past the end", m.offset, want)
	}
}

// --- detail pane ----------------------------------------------------------

func TestDetailPaneDescribesTheSelectedRepo(t *testing.T) {
	m := withProcList(80, 12, []Project{{Name: "alpha", Path: "/p/alpha"}}, nil)
	m.details[detailKey(m.rows[0])] = []field{{label: "name", value: "alpha"}, {label: "path", value: "/p/alpha"}}

	col := strings.Join(detailColumn(m), "\n")
	if !strings.Contains(col, "alpha") || !strings.Contains(col, "/p/alpha") {
		t.Errorf("detail pane should describe the selection:\n%s", col)
	}
}

func TestDetailPaneFollowsTheCursor(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"}},
	)
	m.details[detailKey(m.rows[0])] = []field{{label: "name", value: "scrn"}}
	m.details[detailKey(m.rows[1])] = []field{{label: "command", value: "zsh"}}

	if !strings.Contains(strings.Join(detailColumn(m), "\n"), "scrn") {
		t.Error("detail should describe the repo while the repo is selected")
	}
	m = press(m, "down")
	if !strings.Contains(strings.Join(detailColumn(m), "\n"), "zsh") {
		t.Error("detail should describe the process once the cursor moves onto it")
	}
}

func TestDetailPaneSaysWhenItIsStillLoading(t *testing.T) {
	m := withProcList(80, 12, []Project{{Name: "alpha", Path: "/p/alpha"}}, nil)
	if !strings.Contains(strings.Join(detailColumn(m), "\n"), "loading") {
		t.Error("an uninspected row should say it is loading rather than look empty")
	}
}

func TestMovingRequestsDetailForTheNewRow(t *testing.T) {
	m := threeRepos(10)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if cmd == nil {
		t.Error("moving the cursor should request details for the newly selected row")
	}
}

func TestDetailIsNotRefetchedWhenCached(t *testing.T) {
	m := threeRepos(10)
	m.details[detailKey(m.rows[1])] = []field{{label: "name", value: "b"}}

	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyDown}); cmd != nil {
		t.Error("a row already inspected should not be inspected again")
	}
}

func TestStaleDetailKeysAreIgnored(t *testing.T) {
	m := threeRepos(10)
	next, _ := m.Update(detailMsg{key: "repo:/p/gone", fields: []field{{label: "name", value: "gone"}}})

	if strings.Contains(strings.Join(detailColumn(next.(model)), "\n"), "gone") {
		t.Error("a detail result for another row should not be shown for this one")
	}
}

func TestCursorKeepsItsSubjectAcrossRescans(t *testing.T) {
	m := threeRepos(10)
	m = press(press(m, "down"), "down") // on "c"

	// A rescan that reorders the list should keep the cursor on "c".
	next, _ := m.Update(projectsMsg{projects: []Project{
		{Name: "new", Path: "/p/new"},
		{Name: "a", Path: "/p/a"},
		{Name: "b", Path: "/p/b"},
		{Name: "c", Path: "/p/c"},
	}})

	r, _ := next.(model).selected()
	if r.project.Path != "/p/c" {
		t.Errorf("cursor landed on %q after a rescan, want /p/c", r.project.Path)
	}
}

// --- helpers --------------------------------------------------------------

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}

		// An OSC runs to a bell or a string terminator, not to the first
		// letter — its payload is words, and stopping at one would eat what
		// came after it.
		if i+1 < len(s) && s[i+1] == ']' {
			for i += 2; i < len(s); i++ {
				if s[i] == 0x07 {
					break
				}
				if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
					i++
					break
				}
			}
			continue
		}
		for i < len(s) && !isANSITerm(s[i]) {
			i++
		}
	}
	return b.String()
}

func isANSITerm(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// --- collapsing -----------------------------------------------------------

// nestedTree is one repo with zsh → (vim → fmt, zig).
func nestedTree(h int) model {
	return withProcList(80, h,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "vim", Dir: "/p/scrn"},
			{PID: 30, PPID: 10, Command: "zig", Dir: "/p/scrn"},
			{PID: 40, PPID: 20, Command: "fmt", Dir: "/p/scrn"},
			{PID: 50, PPID: 20, Command: "lint", Dir: "/p/scrn"},
		},
	)
}

func TestSpaceCollapsesAProcessNode(t *testing.T) {
	m := nestedTree(12)
	wantRows(t, navColumn(m), []string{
		"▸scrn", " └─ zsh", "   ├─ vim", "   │ ├─ fmt", "   │ └─ lint", "   └─ zig",
	})

	// Move onto vim and fold it.
	m = press(press(press(m, "down"), "down"), " ")
	wantRows(t, navColumn(m), []string{
		" scrn", " └─ zsh", "▸  ├─ vim +2", "   └─ zig",
	})
}

func TestSpaceCollapsesARepo(t *testing.T) {
	m := press(nestedTree(12), " ")

	col := navColumn(m)
	wantRows(t, col, []string{"▸scrn +5"})
	if len(col) != 1 {
		t.Errorf("a collapsed repo should hide its whole tree, got:\n%s", strings.Join(col, "\n"))
	}
}

func TestSpaceUnfoldsAgain(t *testing.T) {
	m := nestedTree(12)
	folded := press(m, " ")
	unfolded := press(folded, " ")

	if len(navColumn(unfolded)) != len(navColumn(m)) {
		t.Errorf("space should restore the tree:\n%s", strings.Join(navColumn(unfolded), "\n"))
	}
}

func TestCollapsedNodeReportsWhatItHides(t *testing.T) {
	// zsh hides vim, zig and fmt.
	m := press(press(nestedTree(12), "down"), " ")
	wantRows(t, navColumn(m), []string{" scrn", "▸└─ zsh +4"})
}

func TestSpaceOnALeafDoesNothing(t *testing.T) {
	m := nestedTree(12)
	for range 4 {
		m = press(m, "down") // onto "lint 50", a leaf
	}
	before := navColumn(m)

	m = press(m, " ")
	if got := navColumn(m); len(got) != len(before) {
		t.Errorf("space on a leaf changed the tree:\n%s", strings.Join(got, "\n"))
	}
	if strings.Contains(strings.Join(navColumn(m), ""), "+0") {
		t.Error("a leaf should not be marked as hiding anything")
	}
}

func TestSpaceOnARepoWithNoProcessesDoesNothing(t *testing.T) {
	m := withProcList(80, 8, []Project{{Name: "idle", Path: "/p/idle"}}, nil)
	m = press(m, " ")
	wantRows(t, navColumn(m), []string{"▸idle"})
	if strings.Contains(navColumn(m)[0], "+") {
		t.Error("an idle repo should not be marked as hiding anything")
	}
}

func TestCursorSkipsFoldedChildren(t *testing.T) {
	// Fold zsh, then step down: the next row is the next repo, not a child.
	m := withProcList(80, 12,
		[]Project{{Name: "a", Path: "/p/a"}, {Name: "b", Path: "/p/b"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/a"},
			{PID: 20, PPID: 10, Command: "vim", Dir: "/p/a"},
		},
	)
	m = press(press(m, "down"), " ") // on zsh, folded
	m = press(m, "down")

	r, _ := m.selected()
	if r.kind != rowProject || r.project.Name != "b" {
		t.Errorf("cursor landed on %+v, want the next repo", r)
	}
}

func TestCollapseSurvivesARescan(t *testing.T) {
	m := press(nestedTree(12), " ") // repo folded
	next, _ := m.Update(procsMsg{procs: m.procs})

	if got := navColumn(next.(model)); len(got) != 1 {
		t.Errorf("a rescan should not unfold the tree:\n%s", strings.Join(got, "\n"))
	}
}

func TestTheKeysListTheFolds(t *testing.T) {
	// Both directions at once: the modal describes the pair rather than
	// tracking which way the next press would go.
	if !strings.Contains(keysOf(sized(160, 24)), "space · - fold · unfold all") {
		t.Error("the keys should mention folding")
	}
}

func TestCollapsedRowStaysInItsColumn(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: strings.Repeat("x", 60), Path: "/p/x"}},
		[]Proc{{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/x"}},
	)
	m = press(m, " ")

	if got := len([]rune(navColumn(m)[0])); got > navWidth {
		t.Errorf("collapsed row is %d columns, want at most %d: %q", got, navWidth, navColumn(m)[0])
	}
}

// --- killing --------------------------------------------------------------

// keysOf opens the list of keys and returns it, since it is only a line until
// somebody asks.
// keysOf opens the keys modal and flattens the window to one string, so a
// test can ask whether a key is listed.
func keysOf(m model) string {
	return strings.Join(strings.Fields(stripANSI(press(m, "?").View().Content)), " ")
}

// footer is scrn's own block at the foot of its column, flattened to one
// string so a test can ask whether something is in it.
func footer(m model) string {
	lines := strings.Split(m.View().Content, "\n")
	n := min(len(m.hintLines(m.hintWidth())), len(lines))

	out := make([]string, 0, n)
	for _, ln := range lines[len(lines)-n:] {
		nav, _ := splitRow(stripANSI(ln))
		out = append(out, strings.Join(strings.Fields(nav), " "))
	}
	return strings.Join(out, " ")
}

func TestXAsksBeforeKilling(t *testing.T) {
	m := press(nestedTree(12), "down") // onto zsh 10
	m = press(m, "x")

	if m.pendingKill == nil || len(m.pendingKill.nodes) != 1 || m.pendingKill.nodes[0].PID != 10 {
		t.Fatalf("pendingKill = %v, want just the selected process", m.pendingKill)
	}
	if f := footer(m); !strings.Contains(f, "kill zsh 10?") {
		t.Errorf("footer = %q, want it to ask before killing", f)
	}
}

func TestXDoesNotKillOnItsOwn(t *testing.T) {
	m := press(nestedTree(12), "down")
	_, cmd := m.Update(typed("x"))
	if cmd != nil {
		t.Error("the first x should only arm the confirmation, not signal anything")
	}
}

func TestConfirmingRunsTheKill(t *testing.T) {
	m := press(press(nestedTree(12), "down"), "x")

	for _, key := range []string{"x", "y", "enter"} {
		var msg tea.KeyPressMsg
		if key == "enter" {
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		} else {
			msg = typed(key)
		}
		next, cmd := m.Update(msg)
		if cmd == nil {
			t.Errorf("%q should confirm the kill", key)
		}
		if next.(model).pendingKill != nil {
			t.Errorf("%q should clear the pending kill", key)
		}
	}
}

func TestAnyOtherKeyCancelsTheKill(t *testing.T) {
	armed := press(press(nestedTree(12), "down"), "x")

	for _, key := range []string{"s", "esc", "j", "a", " "} {
		var msg tea.KeyPressMsg
		switch key {
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		default:
			msg = typed(key)
		}
		next, cmd := armed.Update(msg)
		m := next.(model)

		if m.pendingKill != nil {
			t.Errorf("%q left the kill armed", key)
		}
		if cmd != nil {
			t.Errorf("%q signalled something instead of cancelling", key)
		}
		if !strings.Contains(footer(m), "cancelled") {
			t.Errorf("%q should say the kill was cancelled, footer = %q", key, footer(m))
		}
	}
}

func TestCancellingKeysDoNotAlsoActOnTheList(t *testing.T) {
	armed := press(press(nestedTree(12), "down"), "x")
	cursorWas := armed.cursor

	next, _ := armed.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if next.(model).cursor != cursorWas {
		t.Error("the key that cancels a kill should not also move the cursor")
	}
}

func TestQuitStillWorksWhileArmed(t *testing.T) {
	// Cancelling is the priority, but the user must not be trapped: the next
	// key after cancelling quits as usual.
	armed := press(press(nestedTree(12), "down"), "x")
	cancelled := press(armed, "q")
	if _, cmd := cancelled.Update(typed("q")); cmd == nil {
		t.Error("q should quit once the confirmation is cleared")
	}
}

func TestKillFailureIsReported(t *testing.T) {
	m := nestedTree(12)
	next, cmd := m.Update(killFailed("zsh", 10, errors.New("not permitted")))

	if f := footer(next.(model)); !strings.Contains(f, "not permitted") {
		t.Errorf("footer = %q, want the failure reported", f)
	}
	if cmd != nil {
		t.Error("a failed kill should not schedule a rescan")
	}
}

func TestKillSuccessStartsTheMarker(t *testing.T) {
	m := nestedTree(12)
	next, cmd := m.Update(killed("zsh", 10))
	got := next.(model)

	if f := footer(got); !strings.Contains(f, "SIGTERM") {
		t.Errorf("footer = %q, want it to report the signal", f)
	}
	if cmd == nil || !got.spinning {
		t.Error("a successful kill should start the frame chain that watches for the exit")
	}
	if _, dying := got.dying[10]; !dying {
		t.Error("a successful kill should mark the process")
	}
}

func TestStatusClearsOnTheNextKey(t *testing.T) {
	m := press(nestedTree(12), "r") // leaves a status about the missing server
	if !strings.Contains(footer(m), "no server") {
		t.Fatal("expected a status to clear")
	}

	m = press(m, "down")
	if strings.Contains(footer(m), "no server") {
		t.Errorf("status should clear once the cursor moves, footer = %q", footer(m))
	}
}

func TestTheKeysListTheKill(t *testing.T) {
	if !strings.Contains(keysOf(sized(80, 24)), "x · X kill") {
		t.Error("the keys should mention the kill key")
	}
}

// --- automatic refresh ----------------------------------------------------

func TestTickRefreshesAndReschedulesItself(t *testing.T) {
	m := nestedTree(12)
	next, cmd := m.Update(tickMsg{})

	if cmd == nil {
		t.Fatal("a tick should rescan and schedule the next tick")
	}
	if next.(model).ticks != 1 {
		t.Errorf("ticks = %d, want 1", next.(model).ticks)
	}
}

func TestTickKeepsTicking(t *testing.T) {
	var m tea.Model = nestedTree(12)
	for i := range 3 {
		var cmd tea.Cmd
		m, cmd = m.Update(tickMsg{})
		if cmd == nil {
			t.Fatalf("tick %d did not schedule a successor", i)
		}
	}
	if got := m.(model).ticks; got != 3 {
		t.Errorf("ticks = %d, want 3", got)
	}
}

func TestProjectsAreRescannedLessOftenThanProcesses(t *testing.T) {
	m := nestedTree(12)
	m.ticks = projectEvery - 1

	next, _ := m.Update(tickMsg{})
	if next.(model).ticks%projectEvery != 0 {
		t.Fatalf("expected this tick to be a project-scanning one, ticks = %d", next.(model).ticks)
	}
}

func TestKilledProcessDisappearsOnTheNextScan(t *testing.T) {
	m := nestedTree(12)
	if len(m.rows) != 6 {
		t.Fatalf("rows = %d, want 6 to start", len(m.rows))
	}

	// The same scan without pid 40, as if it had exited.
	next, _ := m.Update(procsMsg{procs: []Proc{
		{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
		{PID: 20, PPID: 10, Command: "vim", Dir: "/p/scrn"},
		{PID: 30, PPID: 10, Command: "zig", Dir: "/p/scrn"},
	}})

	col := strings.Join(navColumn(next.(model)), "\n")
	if strings.Contains(col, "fmt") {
		t.Errorf("an exited process should leave the tree:\n%s", col)
	}
}

func TestCursorHoldsItsPlaceWhenTheSelectionExits(t *testing.T) {
	m := nestedTree(12)
	m = press(press(press(m, "down"), "down"), "down") // onto fmt 40, index 3

	if r, _ := m.selected(); r.node == nil || r.node.PID != 40 {
		t.Fatalf("setup: selected %+v, want pid 40", r)
	}

	next, _ := m.Update(procsMsg{procs: []Proc{
		{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
		{PID: 20, PPID: 10, Command: "vim", Dir: "/p/scrn"},
		{PID: 30, PPID: 10, Command: "zig", Dir: "/p/scrn"},
	}})

	if c := next.(model).cursor; c == 0 {
		t.Error("the cursor jumped to the top when its process exited; it should hold its place")
	}
}

func TestCursorClampsWhenTheListShrinksPastIt(t *testing.T) {
	m := nestedTree(12)
	for range 4 {
		m = press(m, "down") // last row
	}

	next, _ := m.Update(procsMsg{procs: nil}) // every process gone
	got := next.(model)

	if got.cursor >= len(got.rows) {
		t.Errorf("cursor %d is past the end of %d rows", got.cursor, len(got.rows))
	}
}

func TestRefreshKeepsTheVisibleDetailCurrent(t *testing.T) {
	m := nestedTree(12)
	m.details[detailKey(m.rows[0])] = []field{{label: "name", value: "stale"}}

	if cmd := m.refreshDetailCmd(); cmd == nil {
		t.Error("the selected row should be re-inspected even when cached")
	}
}

func TestRefreshDoesNotBlankTheDetailPane(t *testing.T) {
	m := nestedTree(12)
	m.details[detailKey(m.rows[0])] = []field{{label: "name", value: "scrn"}}

	next, _ := m.Update(tickMsg{})
	if strings.Contains(strings.Join(detailColumn(next.(model)), "\n"), "loading") {
		t.Error("a refresh should keep showing the old value until the new one lands")
	}
}

func TestStaleDetailsArePruned(t *testing.T) {
	m := nestedTree(12)
	m.details["proc:99999"] = []field{{label: "command", value: "long gone"}}
	m.rebuild()

	if _, ok := m.details["proc:99999"]; ok {
		t.Error("details for rows no longer listed should be dropped")
	}
	if _, ok := m.details[detailKey(m.rows[0])]; !ok && len(m.details) > 0 {
		t.Error("pruning should keep details for rows still listed")
	}
}

func TestRefreshPreservesCollapsedNodes(t *testing.T) {
	m := press(nestedTree(12), " ") // repo folded
	next, _ := m.Update(tickMsg{})
	next, _ = next.Update(procsMsg{procs: m.procs})

	if got := navColumn(next.(model)); len(got) != 1 {
		t.Errorf("an automatic refresh should not unfold the tree:\n%s", strings.Join(got, "\n"))
	}
}

func TestRefreshDoesNotClearAStatusMessage(t *testing.T) {
	m := nestedTree(12)
	next, _ := m.Update(killed("zsh", 10))
	next, _ = next.Update(tickMsg{})

	if !strings.Contains(footer(next.(model)), "SIGTERM") {
		t.Errorf("a background refresh should not wipe the last report, footer = %q", footer(next.(model)))
	}
}

func TestRefreshDoesNotDisturbAPendingKill(t *testing.T) {
	armed := press(press(nestedTree(12), "down"), "x")
	next, _ := armed.Update(tickMsg{})

	if next.(model).pendingKill == nil {
		t.Error("a background refresh should not cancel a pending confirmation")
	}
	if !strings.Contains(footer(next.(model)), "kill zsh 10?") {
		t.Error("the confirmation should stay on screen through a refresh")
	}
}

func TestASignalledProcessKeepsItsRowAndIsMarked(t *testing.T) {
	m := nestedTree(12)
	next, _ := m.Update(killed("fmt", 40))
	got := next.(model)

	row := navColumn(got)[3]
	if !strings.Contains(row, "fmt") {
		t.Fatalf("row = %q, want the process still listed until it is seen gone", row)
	}
	if !strings.Contains(row, spinFrames[got.frame%len(spinFrames)]) {
		t.Errorf("row = %q, want a marker on the signalled process", row)
	}
}

func TestOnlyTheSignalledProcessIsMarked(t *testing.T) {
	next, _ := nestedTree(12).Update(killed("fmt", 40))

	for i, row := range navColumn(next.(model)) {
		if strings.Contains(row, "fmt") {
			continue
		}
		for _, f := range spinFrames {
			if strings.Contains(row, f) {
				t.Errorf("row %d = %q carries a marker but was not signalled", i, row)
			}
		}
	}
}

func TestAFailedKillMarksNothing(t *testing.T) {
	next, _ := nestedTree(12).Update(killFailed("fmt", 40, errors.New("not permitted")))

	if got := next.(model); len(got.dying) != 0 {
		t.Errorf("dying = %v, want nothing marked when the signal did not land", got.dying)
	}
}

func TestTheMarkerAdvances(t *testing.T) {
	m := nestedTree(12)
	next, _ := m.Update(killed("fmt", 40))
	first := navColumn(next.(model))[3]

	next, _ = next.(model).Update(spinMsg{})
	if second := navColumn(next.(model))[3]; second == first {
		t.Errorf("the marker did not advance: %q twice", second)
	}
}

func TestTheMarkerGoesWhenTheProcessDoes(t *testing.T) {
	m := nestedTree(12)
	next, _ := m.Update(killed("fmt", 40))

	// The next scan without pid 40, as if it had acted on the signal.
	next, _ = next.(model).Update(procsMsg{procs: []Proc{
		{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
		{PID: 20, PPID: 10, Command: "vim", Dir: "/p/scrn"},
		{PID: 30, PPID: 10, Command: "zig", Dir: "/p/scrn"},
	}})
	got := next.(model)

	if col := strings.Join(navColumn(got), "\n"); strings.Contains(col, "fmt") {
		t.Errorf("an exited process should leave the tree:\n%s", col)
	}
	if len(got.dying) != 0 {
		t.Errorf("dying = %v, want it forgotten once the process is gone", got.dying)
	}
}

func TestAProcessDyingInAFoldedSubtreeIsNotForgotten(t *testing.T) {
	// fmt 40 hangs under vim 20. Folding vim takes its row away, which is not
	// the same as the process having exited.
	m := press(press(nestedTree(12), "down"), "down") // onto vim 20
	next, _ := m.Update(killed("fmt", 40))
	folded := press(next.(model), " ")

	if _, dying := folded.dying[40]; !dying {
		t.Error("folding a subtree should not count its processes as gone")
	}
}

func TestTheFrameChainRescansButNotEveryFrame(t *testing.T) {
	// The chain always schedules the next frame; on a rescanning frame it also
	// asks for a scan, which is what eventually finds the process gone. A batch
	// of two is that second command; a lone command is the next frame alone.
	m := nestedTree(12)
	next, _ := m.Update(killed("fmt", 40))
	got := next.(model)

	scans := 0
	for i := range 2 * rescanFrames {
		n, cmd := got.Update(spinMsg{})
		got = n.(model)
		if cmd == nil {
			t.Fatalf("frame %d ended the chain while a process was still dying", i)
		}
		if got.frame%rescanFrames != 0 {
			// Only the next frame, which is a real timer: running it would
			// make the test wait out the frame rate.
			continue
		}
		scans++
		batch, ok := cmd().(tea.BatchMsg)
		if !ok {
			t.Errorf("frame %d scheduled one command, want the next frame and a scan", got.frame)
			continue
		}
		if len(batch) != 2 {
			t.Errorf("frame %d batched %d commands, want the next frame and a scan", got.frame, len(batch))
		}
		// The scan answers well before the next rescanning frame; without the
		// answer, that frame would rightly decline to stack a second scan
		// behind a first still out.
		n, _ = got.Update(procsMsg{procs: got.procs})
		got = n.(model)
	}

	if scans != 2 {
		t.Errorf("scanned %d times in %d frames, want one every %d", scans, 2*rescanFrames, rescanFrames)
	}
}

func TestASecondKillJoinsTheRunningChain(t *testing.T) {
	m := nestedTree(12)
	next, cmd := m.Update(killed("fmt", 40))
	if cmd == nil {
		t.Fatal("the first kill should start the frame chain")
	}

	next, cmd = next.(model).Update(killed("zig", 30))
	if cmd != nil {
		t.Error("a second kill started its own chain, doubling the frame rate")
	}
	if got := next.(model); len(got.dying) != 2 {
		t.Errorf("dying = %v, want both signalled processes marked", got.dying)
	}
}

func TestTheChainStopsWhenNothingIsDying(t *testing.T) {
	m := nestedTree(12)
	m.spinning = true

	next, cmd := m.Update(spinMsg{})
	if cmd != nil {
		t.Error("the frame chain should stop once nothing is marked")
	}
	if next.(model).spinning {
		t.Error("spinning should be cleared so the next kill can start a chain")
	}
}

func TestAProcessThatIgnoresTheSignalIsGivenUpOn(t *testing.T) {
	m := nestedTree(12)
	next, _ := m.Update(killed("fmt", 40))
	got := next.(model)

	for i := 0; i <= killLinger; i++ {
		n, _ := got.Update(spinMsg{})
		got = n.(model)
	}

	if _, dying := got.dying[40]; dying {
		t.Error("a process that never acted on SIGTERM is still marked as dying")
	}
	if f := footer(got); !strings.Contains(f, "fmt 40 did not exit") {
		t.Errorf("footer = %q, want it to say the process did not go", f)
	}
	if col := strings.Join(navColumn(got), "\n"); !strings.Contains(col, "fmt") {
		t.Errorf("the process is still running and should still be listed:\n%s", col)
	}
}

// --- killing a whole tree ------------------------------------------------

func TestXKillsTheSubtreeParentsFirst(t *testing.T) {
	// zsh 10 holds vim 20 (holding fmt 40) and zig 30.
	m := press(press(nestedTree(12), "down"), "X") // onto zsh 10

	if got, want := targets(m.pendingKill), []int{10, 20, 40, 50, 30}; !sameInts(got, want) {
		t.Errorf("targets = %v, want the subtree parents first %v", got, want)
	}
	if f := footer(m); !strings.Contains(f, "kill zsh 10 and 4 under it?") {
		t.Errorf("footer = %q, want it to say how much it is about to kill", f)
	}
}

func TestLowercaseXTakesOnlyTheOneProcess(t *testing.T) {
	m := press(press(nestedTree(12), "down"), "x") // onto zsh 10

	if got := targets(m.pendingKill); !sameInts(got, []int{10}) {
		t.Errorf("targets = %v, want only the selected process", got)
	}
	if f := footer(m); !strings.Contains(f, "kill zsh 10?") {
		t.Errorf("footer = %q, want a plain kill to read as one", f)
	}
}

func TestXOnALeafReadsAsAPlainKill(t *testing.T) {
	m := nestedTree(12)
	for range 3 {
		m = press(m, "down") // onto fmt 40, which has nothing below it
	}
	m = press(m, "X")

	if got := targets(m.pendingKill); !sameInts(got, []int{40}) {
		t.Errorf("targets = %v, want just the leaf", got)
	}
	if f := footer(m); !strings.Contains(f, "kill fmt 40?") {
		t.Errorf("footer = %q, want no count when there is nothing below it", f)
	}
}

func TestXOnARepoTakesEverythingInIt(t *testing.T) {
	m := press(nestedTree(12), "X") // cursor is on the repo row

	if got, want := targets(m.pendingKill), []int{10, 20, 40, 50, 30}; !sameInts(got, want) {
		t.Errorf("targets = %v, want every process in the repo %v", got, want)
	}
	if f := footer(m); !strings.Contains(f, "kill 5 processes in scrn?") {
		t.Errorf("footer = %q, want it to say what it is about to clear out", f)
	}
}

func TestLowercaseXOnARepoTakesEverythingInItToo(t *testing.T) {
	// On a repository both widths are the same width. A narrow kill that
	// stopped only what the plan had started read as x ignoring half of what
	// was on screen.
	m := press(nestedTree(12), "x") // cursor is on the repo row

	if got, want := targets(m.pendingKill), []int{10, 20, 40, 50, 30}; !sameInts(got, want) {
		t.Errorf("targets = %v, want every process in the repo %v", got, want)
	}
}

func TestXOnAnIdleRepoSaysThereIsNothingToKill(t *testing.T) {
	for _, key := range []string{"x", "X"} {
		m := withProcList(80, 12, []Project{{Name: "scrn", Path: "/p/scrn"}}, nil)
		m = press(m, key)

		if m.pendingKill != nil {
			t.Errorf("%q on an idle repository should not arm a kill", key)
		}
		if f := footer(m); !strings.Contains(f, "nothing running in scrn") {
			t.Errorf("%q footer = %q, want it to explain why nothing happened", key, f)
		}
	}
}

func TestXCollapsedStillKillsWhatIsFoldedAway(t *testing.T) {
	// Folding is a display state; it should not narrow what a kill covers.
	m := press(press(nestedTree(12), "down"), " ") // fold zsh 10
	m = press(m, "X")

	if got, want := targets(m.pendingKill), []int{10, 20, 40, 50, 30}; !sameInts(got, want) {
		t.Errorf("targets = %v, want the folded subtree too %v", got, want)
	}
}

func TestConfirmingATreeKillSignalsEveryProcess(t *testing.T) {
	m := press(press(nestedTree(12), "down"), "X")
	next, cmd := m.Update(typed("X"))
	if cmd == nil {
		t.Fatal("X should confirm a tree kill it armed")
	}
	if next.(model).pendingKill != nil {
		t.Error("confirming should clear the pending kill")
	}
}

func TestEveryProcessInATreeKillIsMarked(t *testing.T) {
	m := nestedTree(12)
	next, _ := m.Update(killedMsg{
		subject: "zsh 10 and 3 under it",
		results: []killResult{
			{command: "zsh", pid: 10}, {command: "vim", pid: 20},
			{command: "fmt", pid: 40}, {command: "zig", pid: 30},
		},
	})
	got := next.(model)

	for _, pid := range []int{10, 20, 30, 40} {
		if _, dying := got.dying[pid]; !dying {
			t.Errorf("pid %d was signalled but is not marked", pid)
		}
	}
	if f := footer(got); !strings.Contains(f, "sent SIGTERM to zsh 10 and 3 under it") {
		t.Errorf("footer = %q, want the whole kill reported once", f)
	}
}

func TestAPartlyRefusedTreeKillSaysSo(t *testing.T) {
	m := nestedTree(12)
	next, _ := m.Update(killedMsg{
		subject: "zsh 10 and 1 under it",
		results: []killResult{
			{command: "zsh", pid: 10},
			{command: "vim", pid: 20, err: errors.New("not permitted")},
		},
	})
	got := next.(model)

	f := footer(got)
	if !strings.Contains(f, "1 could not be killed") || !strings.Contains(f, "not permitted") {
		t.Errorf("footer = %q, want the survivors accounted for", f)
	}
	if _, dying := got.dying[20]; dying {
		t.Error("a process that refused the signal should not be marked as dying")
	}
	if _, dying := got.dying[10]; !dying {
		t.Error("the processes that did take the signal should still be marked")
	}
}

func TestAWhollyRefusedTreeKillReportsEachReasonOnce(t *testing.T) {
	m := nestedTree(12)
	next, _ := m.Update(killedMsg{
		subject: "3 processes in scrn",
		results: []killResult{
			{command: "zsh", pid: 10, err: errors.New("not permitted")},
			{command: "vim", pid: 20, err: errors.New("not permitted")},
			{command: "zig", pid: 30, err: errors.New("already gone")},
		},
	})
	got := next.(model)

	if f := footer(got); !strings.Contains(f, "could not kill 3 processes in scrn: already gone, not permitted") {
		t.Errorf("footer = %q, want each reason named once", f)
	}
	if got.spinning {
		t.Error("nothing was signalled, so nothing should be spinning")
	}
}

func TestTheKeysListTheTreeKill(t *testing.T) {
	if f := keysOf(sized(160, 24)); !strings.Contains(f, "kill the tree") {
		t.Errorf("keys = %q, want the tree kill listed", f)
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- claude instances -----------------------------------------------------

// withClaude builds a repo holding one process, plus the sessions Claude Code
// would have advertised for it.
func withClaude(command string, sessions map[int]claudeSession) model {
	m := withProcList(96, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 700, PPID: 1, Command: command, Dir: "/p/scrn"}})
	m.agents = asAgents(sessions)
	return m
}

func TestABusyClaudeTurns(t *testing.T) {
	// The difference between an instance thinking and one waiting on you is
	// the thing worth crossing the room for, so it moves.
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: busyStatus},
	})

	first := navColumn(m)[1]
	if !strings.Contains(first, "claude "+spinFrames[m.frame%len(spinFrames)]) {
		t.Fatalf("row = %q, want a turning marker on a working instance", first)
	}

	next, _ := m.Update(spinMsg{})
	if second := navColumn(next.(model))[1]; second == first {
		t.Errorf("the marker did not turn: %q twice", second)
	}
}

func TestAWorkingInstanceSetsTheMarkersTurning(t *testing.T) {
	m := withClaude("claude", nil)
	if m.spinning {
		t.Fatal("setup: nothing should be turning yet")
	}

	next, cmd := m.Update(agentsMsg{agents: asAgents(map[int]claudeSession{
		700: {PID: 700, Status: busyStatus},
	})})
	if cmd == nil || !next.(model).spinning {
		t.Error("an instance that has started working should set the markers turning")
	}
}

func TestTheMarkersStopWhenNothingIsWorking(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{700: {PID: 700, Status: busyStatus}})
	m.spinning = true

	next, cmd := m.Update(agentsMsg{agents: asAgents(map[int]claudeSession{
		700: {PID: 700, Status: "idle"},
	})})
	m = next.(model)

	next, cmd = m.Update(spinMsg{})
	if cmd != nil {
		t.Error("the frame chain should stop once nothing is working")
	}
	if next.(model).spinning {
		t.Error("spinning should be cleared so the next one can start it again")
	}
}

func TestATurningMarkerDoesNotChaseTheProcessList(t *testing.T) {
	// A kill needs the list re-read to notice the exit. A working instance is
	// a session file the ordinary refresh already picks up, and an lsof sweep
	// every few frames for the length of a Claude turn is not free.
	m := withClaude("claude", map[int]claudeSession{700: {PID: 700, Status: busyStatus}})
	m.spinning = true
	m.frame = rescanFrames - 1

	_, cmd := m.Update(spinMsg{})
	if cmd == nil {
		t.Fatal("the chain should keep running")
	}
	if _, batched := cmd().(tea.BatchMsg); batched {
		t.Error("a turning marker should schedule the next frame and nothing else")
	}
}

func TestAFinishedTurnLightsItsRow(t *testing.T) {
	// Done-and-waiting is the state that most wants to be seen: once an
	// instance seen working goes idle, the marker fills and the row itself
	// takes the attention color rather than leaving a stopped spinner to
	// whisper it.
	m := withClaude("claude", nil)
	next, _ := m.Update(agentsMsg{agents: asAgents(map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: busyStatus},
	})})
	next, _ = next.(model).Update(agentsMsg{agents: asAgents(map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: "idle"},
	})})
	m = next.(model)

	row := navColumn(m)[1]
	if !strings.Contains(row, "claude ●") {
		t.Errorf("row = %q, want a filled marker on a finished turn", row)
	}
	styled := false
	for raw := range strings.SplitSeq(m.View().Content, "\n") {
		if strings.Contains(raw, attnStyle.Render("claude")) {
			styled = true
		}
	}
	if !styled {
		t.Error("no row renders the label in the attention style")
	}
}

func TestABlockedInstanceHoldsTheBrightDiamond(t *testing.T) {
	// Stopped mid-turn on a specific ask is brighter than done-and-waiting:
	// that answer resumes work already in flight. It is owed even from an
	// instance this window never saw working — the ask exists either way —
	// so no working turn precedes it here.
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: waitingStatus, WaitingFor: "permission prompt"},
	})

	row := navColumn(m)[1]
	if !strings.Contains(row, "claude ◆") {
		t.Errorf("row = %q, want a diamond on a blocked instance", row)
	}
	styled := false
	for raw := range strings.SplitSeq(m.View().Content, "\n") {
		if strings.Contains(raw, blockedStyle.Render("claude")) {
			styled = true
		}
	}
	if !styled {
		t.Error("no row renders the label in the blocked style")
	}

	// The chord counts it among the waiting, unlike an instance merely idle
	// since launch.
	if got := chordKey(m, tea.KeyEnter).status; got == "no agent is waiting" {
		t.Error("the chord passed over a blocked instance")
	}
}

func TestAnInstanceIdleSinceLaunchStaysQuiet(t *testing.T) {
	// A fresh instance at its prompt is waiting, but it has finished nothing
	// and is owed nothing: hollow marker, no highlight, and the chord passes
	// it by.
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: "idle"},
	})

	row := navColumn(m)[1]
	if !strings.Contains(row, "claude ○") {
		t.Errorf("row = %q, want a hollow marker on an instance idle since launch", row)
	}

	if got := chordKey(m, tea.KeyEnter).status; got != "no agent is waiting" {
		t.Errorf("status = %q, want the chord to find nothing owed", got)
	}
}

func TestARecycledPidDoesNotInheritAFinishedTurn(t *testing.T) {
	// The pid leaving the table forgets its history, so whatever takes the
	// number next does not light up on someone else's turn.
	m := withClaude("claude", nil)
	next, _ := m.Update(agentsMsg{agents: asAgents(map[int]claudeSession{
		700: {PID: 700, Status: busyStatus},
	})})
	next, _ = next.(model).Update(agentsMsg{agents: map[int]agent{}})
	next, _ = next.(model).Update(agentsMsg{agents: asAgents(map[int]claudeSession{
		700: {PID: 700, Status: "idle"},
	})})
	m = next.(model)

	if row := navColumn(m)[1]; !strings.Contains(row, "claude ○") {
		t.Errorf("row = %q, want the new holder of the pid quiet", row)
	}
}

func TestPrefixEnterCyclesTheWaitingAgents(t *testing.T) {
	// ctrl+space enter is the summons: it goes to the next agent waiting on
	// its user, and pressing it again continues around them in turn.
	m := withProcList(96, 14,
		[]Project{{Name: "a", Path: "/p/a"}, {Name: "b", Path: "/p/b"}},
		[]Proc{
			{PID: 700, PPID: 1, Command: "claude", Dir: "/p/a"},
			{PID: 701, PPID: 1, Command: "claude", Dir: "/p/b"},
		})
	m.agents = asAgents(map[int]claudeSession{
		700: {PID: 700, Status: "idle", StatusFor: time.Minute},
		701: {PID: 701, Status: "idle", StatusFor: time.Hour},
	})
	m.worked = map[int]bool{700: true, 701: true}

	m = chordKey(m, tea.KeyEnter)
	if r, ok := m.selected(); !ok || r.kind != rowProc || r.node.PID != 700 {
		t.Errorf("cursor on %+v, want the first waiting agent, pid 700", r)
	}

	m = chordKey(m, tea.KeyEnter)
	if r, ok := m.selected(); !ok || r.kind != rowProc || r.node.PID != 701 {
		t.Errorf("cursor on %+v, want the next waiting agent, pid 701", r)
	}

	m = chordKey(m, tea.KeyEnter)
	if r, ok := m.selected(); !ok || r.kind != rowProc || r.node.PID != 700 {
		t.Errorf("cursor on %+v, want the cycle to wrap back to pid 700", r)
	}
}

func TestPrefixReachesOutOfAFocusedShell(t *testing.T) {
	// The prefix's point is to reach scrn from wherever the keys are going:
	// a working agent focused elsewhere must not swallow the summons.
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Status: "idle", StatusFor: time.Minute},
	})
	m.worked = map[int]bool{700: true}
	m.terms = map[int]*remoteTerm{900: {pid: 900}}
	m.focus = 900

	m = chordKey(m, tea.KeyEnter)

	if m.focus != 0 {
		t.Error("the chord should leave the shell for the navigator")
	}
	if r, ok := m.selected(); !ok || r.kind != rowProc || r.node.PID != 700 {
		t.Errorf("cursor on %+v, want the waiting agent", r)
	}
}

func TestAnUnboundChordCancelsThePrefix(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Status: "idle", StatusFor: time.Minute},
	})
	m.worked = map[int]bool{700: true}

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	next, _ = next.(model).Update(typed("z"))
	m = next.(model)

	if m.pendingPrefix {
		t.Error("an unbound key should cancel the prefix")
	}
	if m.cursor != 0 {
		t.Error("the cancelling key should be swallowed, not acted on")
	}
}

// chord presses the prefix and then one key.
func chord(m model, key string) model {
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	next, _ = next.(model).Update(typed(key))
	return next.(model)
}

// chordKey presses the prefix and then one non-printable key.
func chordKey(m model, code rune) model {
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	next, _ = next.(model).Update(tea.KeyPressMsg{Code: code})
	return next.(model)
}

// chordPrefix presses the prefix twice.
func chordPrefix(m model) model {
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	next, _ = next.(model).Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	return next.(model)
}

// twoShells is two repos, each with a shell scrn holds, plus a process it
// does not own, focused on the first shell when pid is not zero.
func twoShells(focus int) model {
	m := withProcList(96, 14,
		[]Project{{Name: "a", Path: "/p/a"}, {Name: "b", Path: "/p/b"}},
		[]Proc{
			{PID: 700, PPID: 1, Command: "zsh", Dir: "/p/a"},
			{PID: 800, PPID: 1, Command: "cargo", Dir: "/p/a"},
			{PID: 701, PPID: 1, Command: "zsh", Dir: "/p/b"},
		})
	m.terms = map[int]*remoteTerm{700: {pid: 700}, 701: {pid: 701}}
	m.focus = focus
	return m
}

func TestPrefixJStepsToTheNextShell(t *testing.T) {
	m := chord(twoShells(700), "j")

	if m.focus != 701 {
		t.Errorf("focus = %d, want the next shell 701", m.focus)
	}
	if r, ok := m.selected(); !ok || r.kind != rowProc || r.node.PID != 701 {
		t.Errorf("cursor on %+v, want it moved with the attachment", r)
	}

	// The step skips the foreign process and wraps at the end.
	if m = chord(m, "j"); m.focus != 700 {
		t.Errorf("focus = %d, want the step to wrap back to 700", m.focus)
	}
}

func TestPrefixKStepsBack(t *testing.T) {
	if m := chord(twoShells(701), "k"); m.focus != 700 {
		t.Errorf("focus = %d, want the previous shell 700", m.focus)
	}
}

func TestPrefixJFromTheNavigatorAttaches(t *testing.T) {
	// Not focused anywhere, cursor on the first repo row: the chord still
	// means motion between shells, so it enters the nearest one ahead.
	if m := chord(twoShells(0), "j"); m.focus != 700 {
		t.Errorf("focus = %d, want the first shell 700", m.focus)
	}
}

func TestPrefixJWithNoOtherShellSaysSo(t *testing.T) {
	m := withProcList(96, 14,
		[]Project{{Name: "a", Path: "/p/a"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/p/a"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700}}
	m.focus = 700

	m = chord(m, "j")
	if m.focus != 700 {
		t.Errorf("focus = %d, want it left where it was", m.focus)
	}
	if m.status != "nothing else to attach to" {
		t.Errorf("status = %q, want it to say there is nowhere to go", m.status)
	}
}

// message is a daemon-era ask, reconstructed from the tmux commands the
// session runs, so the tests keep asserting intent rather than plumbing.
type message struct {
	Kind string
	PID  int
	Dir  string
	Run  string
	Name string
}

const (
	kindOpen   = "open"
	kindAttach = "attach"
	kindClose  = "close"
	kindInput  = "input"
)

// sayCollector stands in for the control client's stdin: every send-keys or
// paste line the session says becomes a kindInput ask, with the line itself
// riding in Run.
type sayCollector struct {
	asked chan message
}

func (c sayCollector) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if strings.HasPrefix(line, "send-keys ") || strings.HasPrefix(line, "set-buffer ") ||
		strings.HasPrefix(line, "paste-buffer ") {
		pid := 0
		f := strings.Fields(line)
		for i, a := range f {
			if a == "-t" && i+1 < len(f) && strings.HasPrefix(f[i+1], "%") {
				pid, _ = strconv.Atoi(strings.TrimPrefix(f[i+1], "%"))
			}
		}
		c.asked <- message{Kind: kindInput, PID: pid, Run: line}
	}
	return len(p), nil
}

func (c sayCollector) Close() error { return nil }

// pipeDaemon gives a model a session whose asks land on the returned
// channel. The session runs over a fake tmux: panes are seeded from the
// terms the test built, commands are answered plausibly, and the control
// side never starts, so no real server is touched.
func pipeDaemon(t *testing.T, m model) (model, chan message) {
	t.Helper()
	s, asked := recordingSession(m.terms)
	m.daemon = s
	return m, asked
}

func recordingSession(terms map[int]*remoteTerm) (*session, chan message) {
	s := newSession()
	s.closed = true // never attach a control client or probe for a server

	asked := make(chan message, 16)
	s.ctl = &ctlClient{in: sayCollector{asked: asked}}
	var mu sync.Mutex
	var opening *message
	var listing []string
	nextPID := 900

	for pid, rt := range terms {
		id := "%" + strconv.Itoa(pid)
		s.panes[pid] = &pane{id: id, pid: pid, dir: rt.dir, name: rt.name, sgr: true}
		s.byPane[id] = pid
		listing = append(listing, fmt.Sprintf("%s\t%d\t%s\t%s\t%s", id, pid, rt.dir, rt.name, rt.dir))
	}

	target := func(args []string) int {
		for i, a := range args {
			if a == "-t" && i+1 < len(args) && strings.HasPrefix(args[i+1], "%") {
				pid, _ := strconv.Atoi(strings.TrimPrefix(args[i+1], "%"))
				return pid
			}
		}
		return 0
	}

	s.run = func(args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "has-session":
			return "", nil
		case "new-window", "start-server":
			// An open: the directory rides behind -c, the command — when
			// there is one — is the last argument, wearing the shell wrapper.
			dir, run := "", ""
			for i, a := range args {
				if a == "-c" && i+1 < len(args) {
					dir = args[i+1]
				}
			}
			if last := args[len(args)-1]; strings.Contains(last, "; exec ") {
				run, _, _ = strings.Cut(last, "; exec ")
			}
			nextPID++
			opening = &message{Kind: kindOpen, PID: nextPID, Dir: dir, Run: run}
			id := "%" + strconv.Itoa(nextPID)
			listing = append(listing, fmt.Sprintf("%s\t%d\t%s\t\t%s", id, nextPID, dir, dir))
			return fmt.Sprintf("%s %d", id, nextPID), nil
		case "set":
			// The name lands on the pane right after the open; the ask is
			// whole once it does.
			if opening != nil {
				opening.Name = args[len(args)-1]
				asked <- *opening
				opening = nil
			}
			return "", nil
		case "list-panes":
			return strings.Join(listing, "\n"), nil
		case "capture-pane":
			// A screen ask means the pane is being watched; a history read
			// (-S) is the transcript, not a watch.
			if !slices.Contains(args, "-S") {
				asked <- message{Kind: kindAttach, PID: target(args)}
			}
			return "", nil
		case "kill-pane":
			asked <- message{Kind: kindClose, PID: target(args)}
			return "", nil
		}
		return "", nil
	}
	return s, asked
}

// askedFor waits for the daemon to be asked something, or fails the test.
func askedFor(t *testing.T, asked chan message) message {
	t.Helper()
	select {
	case got := <-asked:
		return got
	case <-time.After(time.Second):
		t.Fatal("the daemon was asked for nothing")
		return message{}
	}
}

func TestPrefixSOpensAShellBesideTheFocusedOne(t *testing.T) {
	m := twoShells(700)
	m.terms[700].dir = "/p/a"
	m, asked := pipeDaemon(t, m)

	chord(m, "s")
	if got := askedFor(t, asked); got.Kind != kindOpen || got.Dir != "/p/a" || got.Run != "" {
		t.Fatalf("asked %+v, want a bare shell opened at /p/a", got)
	}
}

func TestPrefixAStartsAnAgentBesideTheFocusedShell(t *testing.T) {
	m := twoShells(700)
	m.terms[700].dir = "/p/a"
	m, asked := pipeDaemon(t, m)

	chord(m, "a")
	if got := askedFor(t, asked); got.Kind != kindOpen || got.Dir != "/p/a" || got.Run != agentKinds[0].run {
		t.Fatalf("asked %+v, want an agent started at /p/a", got)
	}
}

func TestPrefixRRunsThePlanOfTheFocusedShellsPlace(t *testing.T) {
	// The shell sits below the project root; the plan lives at the root, so
	// the chord has to walk the dir up to its place before reading it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".scrn"), []byte("web: npm run dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := withProcList(96, 14, []Project{{Name: "app", Path: dir}}, nil)
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: filepath.Join(dir, "src")}}
	m.focus = 700
	m, asked := pipeDaemon(t, m)

	chord(m, "r")
	if got := askedFor(t, asked); got.Kind != kindOpen || got.Dir != dir || got.Name != "web" {
		t.Fatalf("asked %+v, want the plan's web entry at the project root", got)
	}
}

func TestPlaceAtPrefersTheInnermostPlace(t *testing.T) {
	m := model{
		projects: []Project{{Name: "mono", Path: "/p/mono"}},
		subs:     map[string][]Project{"/p/mono": {{Name: "api", Path: "/p/mono/api"}}},
		groups:   []Project{{Name: "p", Path: "/p"}},
	}

	if p, ok := m.placeAt("/p/mono/api/src"); !ok || p.Path != "/p/mono/api" {
		t.Errorf("placeAt = %+v %v, want the sub-project", p, ok)
	}
	if p, ok := m.placeAt("/p/mono/cmd"); !ok || p.Path != "/p/mono" {
		t.Errorf("placeAt = %+v %v, want the repository", p, ok)
	}
	if p, ok := m.placeAt("/p"); !ok || p.Path != "/p" {
		t.Errorf("placeAt = %+v %v, want work at the group's own level to belong to the group", p, ok)
	}
	if p, ok := m.placeAt("/elsewhere"); ok {
		t.Errorf("placeAt = %+v, want nothing outside every place", p)
	}
}

func TestPrefixQQuitsFromAFocusedShell(t *testing.T) {
	// Leaving is q's word, and the prefix carries it out of a shell the
	// letter would otherwise type into.
	m := twoShells(700)
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	_, cmd := next.(model).Update(typed("q"))

	if cmd == nil {
		t.Fatal("^space q should quit, got no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("^space q produced %T, want tea.QuitMsg", cmd())
	}
}

func TestPrefixSlashOpensTheFilterFromAShell(t *testing.T) {
	m := chord(twoShells(700), "/")

	if !m.typing {
		t.Error("^space / should start the filter")
	}
	if m.focus != 0 {
		t.Error("the filter is typed in the navigator, so the shell should be left")
	}
}

func TestPrefixEnterWithNothingWaitingSaysSo(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Status: busyStatus},
	})

	if m = chordKey(m, tea.KeyEnter); m.status != "no agent is waiting" {
		t.Errorf("status = %q, want it to say nothing is waiting", m.status)
	}
}

func TestPrefixPrefixTogglesBetweenTheLastTwoShells(t *testing.T) {
	m := chord(twoShells(700), "j") // over to 701; 700 is now the one before

	if m = chordPrefix(m); m.focus != 700 {
		t.Errorf("focus = %d, want the toggle back to 700", m.focus)
	}
	if m = chordPrefix(m); m.focus != 701 {
		t.Errorf("focus = %d, want the toggle forth to 701", m.focus)
	}
}

func TestTheToggleResumesAShellLeftWithPrefixO(t *testing.T) {
	m := chord(twoShells(700), "o")
	if m.focus != 0 {
		t.Fatalf("focus = %d, want ^space o to step out first", m.focus)
	}

	if m = chordPrefix(m); m.focus != 700 {
		t.Errorf("focus = %d, want the toggle to resume the shell just left", m.focus)
	}
}

func TestTheToggleWithNothingBeforeSaysSo(t *testing.T) {
	m := chordPrefix(twoShells(700))

	if m.focus != 700 {
		t.Errorf("focus = %d, want the toggle to stay put with nowhere to go", m.focus)
	}
	if m.status != "no shell to step back into" {
		t.Errorf("status = %q, want it to explain", m.status)
	}
}

func TestAReusedPIDIsNotDressedUpAsClaude(t *testing.T) {
	// A session file can outlive its process; the command name settles it.
	m := withClaude("vim", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: "busy"},
	})

	row := navColumn(m)[1]
	if strings.ContainsAny(row, "●○") {
		t.Errorf("row = %q, want no marker on a process that is not claude", row)
	}
}

func TestAClaudeWithNoSessionFileIsJustAProcess(t *testing.T) {
	// The daemon is called claude too, and advertises no session.
	m := withClaude("claude", nil)

	row := navColumn(m)[1]
	if strings.ContainsAny(row, "●○") {
		t.Errorf("row = %q, want no marker without a session to report", row)
	}
}

func TestTheMarkerLeavesRoomForTheName(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Status: "busy"},
	})

	for i, row := range bodyRows(m) {
		nav, _ := splitRow(row)
		if w := len([]rune(nav)); w != navWidth {
			t.Fatalf("row %d is %d columns wide, want %d: %q", i, w, navWidth, nav)
		}
	}
}

func TestAgentDetailIsAskedForOnlyOnAnAgentRow(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: "busy"},
	})

	repo, _ := m.rows[0], m.rows[1]
	if got := m.agentFor(repo); got != nil {
		t.Errorf("agentFor(repo row) = %+v, want nothing", got)
	}
	m.cursor = 1
	proc, _ := m.selected()
	got, ok := m.agentFor(proc).(claudeSession)
	if !ok || got.Name != "scrn-1f" {
		t.Errorf("agentFor(claude row) = %+v, want the session", m.agentFor(proc))
	}
}

func TestTheKeysNeverOutgrowTheirColumn(t *testing.T) {
	// A line that overflows the column would push the divider out of true.
	for _, w := range []int{40, 60, 80, 100, 140} {
		m := sized(w, 24)
		for i, ln := range m.hintLines(m.hintWidth()) {
			if got := lipgloss.Width(ln); got > m.hintWidth() {
				t.Errorf("width %d: key line %d is %d columns, want at most %d: %q",
					w, i, got, m.hintWidth(), stripANSI(ln))
			}
		}
	}
}

func TestTheKeysNeverCrowdOutTheList(t *testing.T) {
	// However short the window, the list keeps a row of its own.
	for _, h := range []int{3, 4, 6, 8, 12, 24} {
		m := withProcList(80, h, []Project{{Name: "alpha"}, {Name: "beta"}}, nil)
		if got := len(strings.Split(m.View().Content, "\n")); got != h {
			t.Errorf("height %d: view is %d lines", h, got)
		}
		if h >= 3 && len(navColumn(m)) == 0 {
			t.Errorf("height %d: the keys left no room for the list", h)
		}
	}
}

// --- the folded run, and unfolding it ------------------------------------

func TestTheDetailPaneNamesTheWholeRun(t *testing.T) {
	// The shell that started this is not on screen anywhere else once the run
	// is folded, so the detail pane is where it has to be said.
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
			{PID: 21, PPID: 20, Command: "nvim", Dir: "/p/scrn"},
		})
	m.cursor = 1

	r, _ := m.selected()
	fs := procFields(r.node, r.run, nil)

	got, ok := fieldValue(fs, "run")
	if !ok {
		t.Fatalf("no run field: %+v", fs)
	}
	if got != "zsh 10 › nvim 20 › nvim 21" {
		t.Errorf("run = %q, want the whole run oldest first", got)
	}
}

func TestARowThatFoldedNothingHasNoRun(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
			{PID: 30, PPID: 10, Command: "zig", Dir: "/p/scrn"},
		})
	m.cursor = 2 // nvim, which folded nothing because zsh branches

	r, _ := m.selected()
	if _, ok := fieldValue(procFields(r.node, r.run, nil), "run"); ok {
		t.Error("a row that stands for one process should not describe a run")
	}
}

func TestDashShowsEveryProcess(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
			{PID: 21, PPID: 20, Command: "nvim", Dir: "/p/scrn"},
		})
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ nvim"})

	m = press(m, "-")
	wantRows(t, navColumn(m), []string{
		"▸scrn", " └─ zsh", "   └─ nvim", "     └─ nvim",
	})
}

func TestDashFoldsThemBackAgain(t *testing.T) {
	m := press(press(nestedTree(12), "-"), "-")
	if m.unfolded {
		t.Error("- should toggle rather than only unfold")
	}
}

func TestUnfoldingKeepsTheCursorOnItsProcess(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
			{PID: 21, PPID: 20, Command: "nvim", Dir: "/p/scrn"},
		})
	m.cursor = 1 // the folded row, named for nvim 20

	m = press(m, "-")
	if r, _ := m.selected(); r.node.PID != 20 {
		t.Errorf("selected pid %d, want to still be on nvim 20 after unfolding", r.node.PID)
	}
}

// runWithShell is a model standing on a folded run — a shell scrn holds that
// started a claude — with the shell's screen and the row's details in hand.
func runWithShell(screen string) model {
	m := withProcList(80, 24,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "claude", Dir: "/p/scrn"},
		})
	m.terms[10] = &remoteTerm{pid: 10, screen: screen}
	m.details["proc:20"] = []field{heading("claude"), {label: "run", value: "zsh 10 › claude 20"}}
	m.cursor = 1
	return m
}

func TestAFoldedRunSplitsThePaneIntoBannerAndScreen(t *testing.T) {
	// The shell's screen shows what the run is doing; the banner says what it
	// is. Standing on the row shows both, the facts above the live pane.
	pane := detailColumn(runWithShell("hello-from-the-shell"))

	rule, screen := -1, -1
	for i, row := range pane {
		if strings.HasPrefix(strings.TrimSpace(row), "──") {
			rule = i
		}
		if strings.Contains(row, "hello-from-the-shell") {
			screen = i
		}
	}
	if len(pane) == 0 || !strings.Contains(pane[0], "claude") {
		t.Fatalf("pane = %q, want the detail heading across the top", pane)
	}
	if rule < 0 || screen < 0 || rule > screen {
		t.Fatalf("pane = %q, want a rule between the banner and the screen", pane)
	}
}

func TestTheSplitPaneKeepsTheBottomOfTheScreen(t *testing.T) {
	// The shell is sized for the whole pane, and the banner leaves less than
	// that. What goes is blank padding first and the oldest rows second: the
	// bottom of a screen is the part a glance is after.
	term := &remoteTerm{screen: "old\nnewer\nnewest\n\n\n"}
	got := screenTail(term, 2)
	if len(got) != 2 || got[0] != "newer" || got[1] != "newest" {
		t.Errorf("screenTail = %q, want the last things said", got)
	}
}

func TestAFocusedShellHasNoBanner(t *testing.T) {
	// Focused, the pane is the shell: the banner belongs to looking, not to
	// working in it.
	m := runWithShell("hello-from-the-shell")
	m.focus = 10

	for _, row := range detailColumn(m) {
		if strings.HasPrefix(strings.TrimSpace(row), "──") {
			t.Error("a focused pane should be the whole screen")
		}
	}
}

// --- finding a project ---------------------------------------------------

// manyProjects is a set wide enough that finding one matters, with nothing
// running in any of them.
func manyProjects(w, h int) model {
	return withProcList(w, h, []Project{
		{Name: "scrn", Path: "/p/w0zro/scrn"},
		{Name: "hsg", Path: "/p/hsg/hsg"},
		{Name: "brand", Path: "/p/hsg/brand"},
		{Name: "tressle-api", Path: "/p/node/tressle-api"},
		{Name: "flocking-pixi", Path: "/p/flocking-pixi"},
	}, nil)
}

func TestSlashSearchesEveryProjectNotJustTheRunningOnes(t *testing.T) {
	// The point of the filter is to reach a project you are not working in,
	// so it looks past the narrowed view rather than within it.
	m := narrowed(manyProjects(90, 14))
	if len(m.rows) != 0 {
		t.Fatalf("setup: rows = %d, want the narrowed view to be empty", len(m.rows))
	}

	m = press(m, "/")
	m = typeFilter(m, "brand")

	wantRows(t, navColumn(m), []string{"▸brand"})
}

// typeFilter sends each rune to the filter.
func typeFilter(m model, s string) model {
	for _, r := range s {
		next, _ := m.Update(typed(string(r)))
		m = next.(model)
	}
	return m
}

func TestFilterReachesProcessesByCommand(t *testing.T) {
	// Typing claude finds the repositories a claude is working in, not only
	// the ones named for it — at work the question is as often "where is that
	// running" as "where is that checked out".
	m := withProcList(90, 14, []Project{
		{Name: "scrn", Path: "/p/scrn"},
		{Name: "brand", Path: "/p/brand"},
	}, []Proc{{PID: 100, PPID: 1, Command: "claude", Dir: "/p/brand"}})

	m = typeFilter(press(narrowed(m), "/"), "claude")
	wantRows(t, navColumn(m), []string{" brand", "▸└─ claude"})
}

func TestFilterReachesAChildProcessCommand(t *testing.T) {
	// The whole tree answers: a shell running an npm running a node is found
	// by any of their names, whichever the row happens to be named after.
	m := withProcList(90, 14, []Project{{Name: "brand", Path: "/p/brand"}},
		[]Proc{
			{PID: 100, PPID: 1, Command: "zsh", Dir: "/p/brand"},
			{PID: 101, PPID: 100, Command: "node", Dir: "/p/brand"},
		})

	m = typeFilter(press(narrowed(m), "/"), "node")
	wantRows(t, navColumn(m), []string{" brand", "▸└─ node"})
}

func TestTypingListsTheProcessesThatAnswer(t *testing.T) {
	// A query is a name, and a process that answers to it is as much the
	// thing being looked for as a project is: it is listed under its place,
	// pruned to the branches that answer, so it can be acted on straight
	// from the look.
	m := withProcList(90, 14, []Project{{Name: "brand", Path: "/p/brand"}},
		[]Proc{
			{PID: 100, PPID: 1, Command: "zsh", Dir: "/p/brand"},
			{PID: 101, PPID: 100, Command: "node", Dir: "/p/brand"},
			{PID: 102, PPID: 1, Command: "vim", Dir: "/p/brand"},
		})

	m = typeFilter(press(narrowed(m), "/"), "node")
	rows := navColumn(m)
	wantRows(t, rows, []string{" brand", "▸└─ node"})
	if len(rows) != 2 {
		t.Fatalf("rows = %q, want the vim pruned away", rows)
	}
}

func TestTypingAnEmptyQueryListsPlacesAlone(t *testing.T) {
	// Every process of every project would bury the names being scanned for;
	// the processes join the list only once a query gives them a reason to.
	m := withProcList(90, 14, []Project{{Name: "brand", Path: "/p/brand"}},
		[]Proc{{PID: 100, PPID: 1, Command: "vim", Dir: "/p/brand"}})

	m = press(narrowed(m), "/")
	for _, row := range navColumn(m) {
		if strings.Contains(row, "─") {
			t.Fatalf("row %q lists a process before anything was typed", row)
		}
	}
}

func TestEnterOnAFoundProcessStepsIn(t *testing.T) {
	m := withProcList(90, 14, []Project{{Name: "brand", Path: "/p/brand"}},
		[]Proc{{PID: 100, PPID: 1, Command: "zsh", Dir: "/p/brand"}})
	m.terms[100] = &remoteTerm{pid: 100}
	m, asked := pipeDaemon(t, m)

	// The cursor lands on the matching shell by itself; enter needs no move.
	m = typeFilter(press(narrowed(m), "/"), "zsh")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)

	if m.focus != 100 {
		t.Fatalf("focus = %d, want the found shell, pid 100", m.focus)
	}
	if got := askedFor(t, asked); got.Kind != kindAttach || got.PID != 100 {
		t.Fatalf("asked %+v, want an attach to pid 100", got)
	}
}

func TestCtrlXWhileTypingAsksToKillWhatWasFound(t *testing.T) {
	m := withProcList(90, 14, []Project{{Name: "brand", Path: "/p/brand"}},
		[]Proc{{PID: 100, PPID: 1, Command: "vim", Dir: "/p/brand"}})

	// The cursor lands on the matching process by itself.
	m = typeFilter(press(narrowed(m), "/"), "vim")
	next, _ := m.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	m = next.(model)

	if m.typing {
		t.Error("ctrl+x should end the typing, so the confirmation's key confirms")
	}
	if m.filter != "vim" {
		t.Errorf("filter = %q, want it held so the subject stays listed", m.filter)
	}
	if got := targets(m.pendingKill); len(got) != 1 || got[0] != 100 {
		t.Fatalf("pending kill on %v, want the vim, pid 100", got)
	}
}

func TestNavWidthComesFromConfigWithinReason(t *testing.T) {
	defer func(w int) { navWidth = w }(navWidth)

	for _, tc := range []struct{ in, want int }{
		{0, 28},   // unset leaves the default
		{40, 40},  // a chosen width holds
		{5, 16},   // too narrow for any name
		{200, 60}, // most of any screen
	} {
		navWidth = 28
		applyNavWidth(tc.in)
		if navWidth != tc.want {
			t.Errorf("applyNavWidth(%d): navWidth = %d, want %d", tc.in, navWidth, tc.want)
		}
	}
}

func TestFilterMatchesThePathAsWellAsTheName(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "node")
	wantRows(t, navColumn(m), []string{"▸tressle-api"})
}

func TestFilterIgnoresCase(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "SCRN")
	wantRows(t, navColumn(m), []string{"▸scrn"})
}

func TestKeysGoToTheFilterWhileItIsBeingTyped(t *testing.T) {
	// A project called "scrn" must be typeable without s opening a shell.
	m := press(narrowed(manyProjects(90, 14)), "/")
	m = typeFilter(m, "s")

	if m.filter != "s" {
		t.Errorf("filter = %q, want the keystroke to have gone into it", m.filter)
	}
	if len(m.terms) != 0 {
		t.Error("s while typing a filter should not open a shell")
	}
}

func TestBackspaceWidensTheFilter(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brandx")
	if len(m.rows) != 0 {
		t.Fatalf("setup: rows = %d, want no match", len(m.rows))
	}

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	wantRows(t, navColumn(next.(model)), []string{"▸brand"})
}

func TestEnterKeepsTheFilterSoTheProjectStays(t *testing.T) {
	// Clearing it on accept would drop an idle project straight back out of
	// the narrowed list, before anything could be started in it.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)

	if m.typing {
		t.Error("enter should stop the typing")
	}
	if m.filter != "brand" {
		t.Errorf("filter = %q, want it still applied", m.filter)
	}
	wantRows(t, navColumn(m), []string{"▸brand"})
}

func TestOnceAcceptedTheOrdinaryKeysWorkAgain(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)

	// s now means open a shell rather than a letter of the filter.
	next, _ = m.Update(typed("s"))
	if got := next.(model); got.filter != "brand" {
		t.Errorf("filter = %q, want s to have been taken as a key", got.filter)
	}
}

func TestEscapeClearsTheFilterRatherThanQuitting(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)

	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil {
		t.Error("esc with a filter applied should clear it, not quit")
	}
	if got := next.(model); got.filter != "" {
		t.Errorf("filter = %q, want it cleared", got.filter)
	}
}

func TestEscapeNeverQuits(t *testing.T) {
	// Esc closes what is open, and leaving is q's word alone: the esc that
	// closed the filter is often followed by a reflexive second one, and
	// that beat must not take the window with it.
	if _, cmd := manyProjects(90, 14).Update(tea.KeyPressMsg{Code: tea.KeyEscape}); cmd != nil {
		t.Error("esc with nothing open should do nothing, not quit")
	}
	if _, cmd := manyProjects(90, 14).Update(typed("q")); cmd == nil {
		t.Error("q should still quit")
	}
}

func TestEscapeWhileTypingAbandonsTheFilter(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(model)

	if m.typing || m.filter != "" {
		t.Errorf("typing=%v filter=%q, want the filter abandoned", m.typing, m.filter)
	}
}

func TestEscWhileTypingPutsTheCursorBack(t *testing.T) {
	// Abandoning the look is not acting on anything, so it puts the cursor
	// back on the row it left when / was pressed.
	m := manyProjects(90, 14)
	m = press(press(m, "down"), "down")
	r, ok := m.selected()
	if !ok {
		t.Fatal("setup: nothing selected")
	}
	was := detailKey(r)

	m = typeFilter(press(m, "/"), "scrn")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(model)

	if r, ok = m.selected(); !ok || detailKey(r) != was {
		t.Fatalf("cursor on %q, want back on %q", detailKey(r), was)
	}
}

func TestEscWhileTypingStepsBackIntoTheShell(t *testing.T) {
	// A filter opened from inside a shell was a look out of it; abandoning
	// the look means the keys go back where they came from.
	m := twoShells(700)
	m, asked := pipeDaemon(t, m)
	m = chord(m, "/")
	if !m.typing || m.focus != 0 {
		t.Fatalf("setup: typing=%v focus=%d, want the filter to have the keys", m.typing, m.focus)
	}

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(model)

	if m.focus != 700 {
		t.Fatalf("focus = %d, want back in the shell, pid 700", m.focus)
	}
	if got := askedFor(t, asked); got.Kind != kindAttach || got.PID != 700 {
		t.Fatalf("asked %+v, want a re-attach to pid 700", got)
	}
}

func TestAFilterThatMatchesNothingSaysSo(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "zzz")
	wantRows(t, navColumn(m), []string{" no project matches"})
}

func TestTheFooterShowsWhatIsBeingTyped(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(160, 14)), "/"), "bra")
	if f := footer(m); !strings.Contains(f, "/bra") {
		t.Errorf("footer = %q, want it to show the filter being typed", f)
	}

	// Enter opens a shell in what is under the cursor, so the filter is over
	// and what it says next is about that rather than about the search.
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if next.(model).typing {
		t.Error("enter should finish the looking up")
	}
}

func TestTheEmptyListPointsAtTheFilter(t *testing.T) {
	m := narrowed(manyProjects(90, 14))
	if col := strings.Join(navColumn(m), "\n"); !strings.Contains(col, "/  find a project") {
		t.Errorf("empty list = %q, want it to point at the way out", col)
	}
}

func TestStartingSomethingClearsTheSearchThatFoundIt(t *testing.T) {
	// Once there is work in the project it stays listed on its own merit, so
	// the filter has nothing left to do.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)

	// A shell opened in it, and then the scan that finds it.
	m.wantCursor = 700
	next, _ = m.Update(procsMsg{procs: []Proc{
		{PID: 700, PPID: 1, Command: "zsh", Dir: "/p/hsg/brand"},
	}})
	m = next.(model)

	if m.filter != "" {
		t.Errorf("filter = %q, want it gone once the shell landed", m.filter)
	}
	// The cursor followed the shell it just started.
	wantRows(t, navColumn(m), []string{" brand", "▸└─ zsh"})
}

func TestTheSearchHoldsUntilTheShellActuallyLands(t *testing.T) {
	// Clearing on the keystroke would drop the project out of the list until
	// the scan caught up, which is a flicker for no reason.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)
	m.wantCursor = 700 // asked for, not yet running

	next, _ = m.Update(procsMsg{procs: nil})
	m = next.(model)

	if m.filter != "brand" {
		t.Errorf("filter = %q, want it held until there is something to show", m.filter)
	}
	wantRows(t, navColumn(m), []string{"▸brand"})
}

func TestEnteringSomethingClearsTheSearchAtOnce(t *testing.T) {
	// Nothing has to be waited for: the project already holds the shell.
	m := withProcList(90, 14,
		[]Project{{Name: "brand", Path: "/p/hsg/brand"}, {Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/p/hsg/brand"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/p/hsg/brand"}}
	m, _ = pipeDaemon(t, m)
	m.showAll = false
	m.filter = "brand"
	m.rebuild()
	m.cursor = 1

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := next.(model); got.filter != "" {
		t.Errorf("filter = %q, want stepping in to have finished the search", got.filter)
	}
}

func TestKillingDoesNotClearTheSearch(t *testing.T) {
	// Clearing out several projects is one job; the list should not move
	// underneath it after each one.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)

	next, _ = m.Update(typed("X"))
	if got := next.(model); got.filter != "brand" {
		t.Errorf("filter = %q, want a kill to leave the search alone", got.filter)
	}
}

func TestSlashListsEveryProjectBeforeAnythingIsTyped(t *testing.T) {
	// Half of looking a project up is remembering which ones there are.
	m := narrowed(manyProjects(90, 14))
	if len(m.rows) != 0 {
		t.Fatalf("setup: rows = %d, want the narrowed view empty", len(m.rows))
	}

	m = press(m, "/")
	// Alphabetical, the order the scan delivers and topPlaces keeps.
	wantRows(t, navColumn(m), []string{
		"▸brand", " flocking-pixi", " hsg", " scrn", " tressle-api",
	})
}

func TestThePickerShowsProjectsWithoutTheirProcesses(t *testing.T) {
	// The names are what is being scanned; what is running would bury them.
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}, {Name: "hsg", Path: "/p/hsg"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
		})

	m = press(m, "/")
	wantRows(t, navColumn(m), []string{"▸hsg", " scrn"})
	if len(m.rows) != 2 {
		t.Errorf("rows = %d, want only the two projects", len(m.rows))
	}
}

func TestThePickerDimsEverythingButTheCursor(t *testing.T) {
	m := press(narrowed(manyProjects(90, 14)), "/")

	for i, r := range m.rows {
		selected := i == m.cursor
		if selected && dimmed(m, r, true) {
			t.Error("the row under the cursor should stand out from the rest")
		}
		if !selected && !dimmed(m, r, false) {
			t.Errorf("row %d is lit; nothing is chosen yet", i)
		}
	}
}

func TestTypingNarrowsThePicker(t *testing.T) {
	m := press(narrowed(manyProjects(90, 14)), "/")
	if len(m.rows) != 5 {
		t.Fatalf("rows = %d, want every project to start", len(m.rows))
	}

	m = typeFilter(m, "h")
	wantRows(t, navColumn(m), []string{"▸brand", " hsg"}) // both are under /p/hsg
	m = typeFilter(m, "sg/h")
	wantRows(t, navColumn(m), []string{"▸hsg"})
}

func TestLeavingThePickerBringsTheProcessesBack(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"}})

	m = press(m, "/")
	wantRows(t, navColumn(m), []string{"▸scrn"})

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	wantRows(t, navColumn(next.(model)), []string{"▸scrn", " └─ zsh"})
}

func TestARunIsNamedForTheProcessThatMatters(t *testing.T) {
	// claude keeps the machine awake while it works, so a caffeinate hangs
	// below it. Naming the run after its deepest process would call this a
	// caffeinate and hide the claude entirely.
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "claude", Dir: "/p/scrn"},
			{PID: 30, PPID: 20, Command: "caffeinate", Dir: "/p/scrn"},
		})
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ claude"})
}

func TestATransientChildDoesNotRenameTheRow(t *testing.T) {
	// A claude reaching for a tool and finishing with it should not rename the
	// row it is on, twice, while you are looking at it.
	procs := []Proc{
		{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
		{PID: 20, PPID: 10, Command: "claude", Dir: "/p/scrn"},
	}
	m := withProcList(80, 12, []Project{{Name: "scrn", Path: "/p/scrn"}}, procs)
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ claude"})

	next, _ := m.Update(procsMsg{procs: append(procs,
		Proc{PID: 40, PPID: 20, Command: "rg", Dir: "/p/scrn"})})
	wantRows(t, navColumn(next.(model)), []string{"▸scrn", " └─ claude"})
}

func TestARunOfNothingButShellsIsNamedForTheLast(t *testing.T) {
	// That is the one you would be typing into.
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "bash", Dir: "/p/scrn"},
		})
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ bash"})
}

func TestALoginShellIsStillAShell(t *testing.T) {
	if !isShell("-zsh") {
		t.Error("a login shell is written with a leading dash and is still a shell")
	}
	if isShell("claude") {
		t.Error("claude is not a shell")
	}
}

// --- the keys, on request ------------------------------------------------

func TestTheFootWearsTheMode(t *testing.T) {
	// navigate at the list, proc while the keys are inside one, prefix while
	// the chord hangs — the way vim says INSERT.
	if f := footer(twoShells(0)); !strings.HasPrefix(f, "navigate") {
		t.Errorf("footer = %q, want it to open with navigate", f)
	}

	m := twoShells(700)
	if f := footer(m); !strings.HasPrefix(f, "proc") {
		t.Errorf("footer = %q, want proc while the keys are in a shell", f)
	}

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	if f := footer(next.(model)); !strings.HasPrefix(f, "prefix") {
		t.Errorf("footer = %q, want prefix while the chord hangs", f)
	}
}

func TestTheFootIsOnlyTheModeWhenQuiet(t *testing.T) {
	// The keys live behind ?; the foot wears the mode and nothing else
	// until something has to be said.
	m := manyProjects(90, 14)
	if got := footer(m); got != "navigate" {
		t.Errorf("footer = %q, want the mode alone", got)
	}
}

func TestQuestionMarkOpensTheKeysModal(t *testing.T) {
	m := press(manyProjects(160, 24), "?")
	view := stripANSI(m.View().Content)
	for _, key := range []string{"╭─ keys", "s", "shell", "kill the tree", "the next waiting agent", "quit"} {
		if !strings.Contains(view, key) {
			t.Errorf("view does not show %q with the modal open", key)
		}
	}
	if got := footer(m); got != "navigate" {
		t.Errorf("footer = %q, want the foot untouched by the modal", got)
	}
}

func TestTheKeysModalNeverWidensTheWindow(t *testing.T) {
	// The compositor draws to the union of its layers, so a box wider than a
	// narrow window would widen every row and the terminal would wrap the
	// whole frame.
	m := press(manyProjects(30, 14), "?")
	for i, ln := range strings.Split(m.View().Content, "\n") {
		if w := lipgloss.Width(ln); w > 30 {
			t.Errorf("row %d is %d columns in a 30-column window", i, w)
		}
	}
}

func TestTheKeysModalIsARectangle(t *testing.T) {
	// The overlay reserves the first row's width for every row, so a row
	// wider than the top border would spill past the box's right edge.
	m := manyProjects(120, 30)
	box := m.keysModal(30)
	if len(box) == 0 {
		t.Fatal("no modal to measure")
	}
	w := lipgloss.Width(box[0])
	for i, ln := range box {
		if got := lipgloss.Width(ln); got != w {
			t.Errorf("modal row %d is %d columns, want %d: %q", i, got, w, stripANSI(ln))
		}
	}
}

func TestAnyKeyPutsTheKeysAway(t *testing.T) {
	// The modal was asked for with a keystroke and leaves on one — and the
	// leaving key is swallowed, not acted on.
	m := press(press(manyProjects(90, 14), "?"), "j")
	if m.showHelp {
		t.Error("a key with the modal open should close it")
	}
	if m.cursor != 0 {
		t.Error("the closing key should be swallowed, not acted on")
	}
}

func TestEscapeClosesTheKeysFirst(t *testing.T) {
	// Whatever is open is what esc is most likely about.
	m := press(manyProjects(90, 14), "?")
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(model)

	if cmd != nil {
		t.Error("esc with the keys open should close them, not quit")
	}
	if m.showHelp {
		t.Error("esc should close the keys")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); cmd != nil {
		t.Error("a second esc, with nothing left open, should do nothing")
	}
}

func TestPrefixQuestionMarkShowsTheKeysFromAShell(t *testing.T) {
	// The modal rides the prefix so it can be asked for from wherever the
	// keys are going, including inside a focused shell — and the key that
	// closes it must not fall through into the shell.
	m := manyProjects(90, 14)
	m.terms = map[int]*remoteTerm{900: {pid: 900}}
	m.focus = 900

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl})
	m = press(next.(model), "?")
	if !m.showHelp {
		t.Fatal("^spc ? should show the keys over a focused shell")
	}
	m = press(m, "x") // would reach the shell, or panic on the nil daemon
	if m.showHelp {
		t.Error("the next key should close the modal")
	}
	if m.focus != 900 {
		t.Error("the modal should leave the shell focused")
	}
}

func TestTheKeysModalTakesNoRoomFromTheList(t *testing.T) {
	m := manyProjects(90, 14)
	closed := m.bodyHeight()
	open := press(m, "?").bodyHeight()

	if open != closed {
		t.Errorf("rows for the list: %d with the modal open, %d closed; a modal covers, it does not squeeze", open, closed)
	}
	if closed != m.height-3 {
		t.Errorf("the list has %d rows, want all but the masthead, its blank and the chip", closed)
	}
}

func TestSomethingBeingSaidStillTakesTheFoot(t *testing.T) {
	// A confirmation or a report is about the next keystroke, so it is shown
	// whether or not the keys have been asked for.
	m := press(nestedTree(24), "r") // no server, so it explains itself
	if !strings.Contains(footer(m), "no server") {
		t.Errorf("footer = %q, want what was just said", footer(m))
	}
}

func TestTheSessionsAreReadOnAChainOfTheirOwn(t *testing.T) {
	// Reading them costs a few small files; the process scan costs an lsof
	// sweep of the machine. Tying them together made a session that had just
	// started working wait up to a process poll to say so.
	m := nestedTree(12)
	next, cmd := m.Update(agentTickMsg{})
	if cmd == nil {
		t.Fatal("the session chain should schedule its own next read")
	}
	if _, ok := cmd().(tea.BatchMsg); !ok {
		t.Error("a session tick should both read and schedule the next")
	}
	_ = next
}

func TestTheProcessTickNoLongerCarriesTheSessions(t *testing.T) {
	// Two chains reading them would double the rate for no reason.
	m := nestedTree(12)
	// On the repo-detail cadence, so the selected row's refresh rides along.
	m.ticks = repoDetailEvery - 1
	_, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("a tick should still refresh")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("a tick should schedule more than one thing")
	}
	// The process scan, the next tick, and re-inspecting the selected row.
	// If this becomes four, check that the sessions have not been put back on
	// this chain as well as their own.
	if len(batch) != 3 {
		t.Errorf("tick batched %d commands, want 3", len(batch))
	}
}

// --- the ends of the list ------------------------------------------------

func TestGGoesToTheBottom(t *testing.T) {
	m := nestedTree(24)
	m = press(m, "G")

	if got, want := m.cursor, len(m.rows)-1; got != want {
		t.Errorf("cursor = %d, want the last row %d", got, want)
	}
}

func TestGGGoesToTheTop(t *testing.T) {
	m := press(nestedTree(24), "G")
	if m.cursor == 0 {
		t.Fatal("setup: expected to be somewhere other than the top")
	}

	m = press(m, "g")
	if m.cursor == 0 {
		t.Error("one g should wait for the second rather than moving")
	}
	m = press(m, "g")
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want the top", m.cursor)
	}
}

func TestOneGFollowedByAnythingElseDoesNothing(t *testing.T) {
	m := press(nestedTree(24), "G")
	at := m.cursor

	m = press(press(m, "g"), "j")
	if m.cursor != at {
		t.Errorf("cursor = %d, want %d: the key that cancels a g should not also move", m.cursor, at)
	}
	if m.pendingG {
		t.Error("the g should be over")
	}
}

func TestTheEndsScrollTheWindow(t *testing.T) {
	// A list longer than the window has to be scrolled to, not just pointed at.
	m := nestedTree(6)
	m = press(m, "G")

	if m.cursor < m.offset || m.cursor >= m.offset+m.bodyHeight() {
		t.Errorf("cursor %d is outside the window at %d..%d", m.cursor, m.offset, m.offset+m.bodyHeight())
	}
	m = press(press(m, "g"), "g")
	if m.offset != 0 {
		t.Errorf("offset = %d, want the window back at the top", m.offset)
	}
}

func TestTheEndsOfAnEmptyListAreHarmless(t *testing.T) {
	m := narrowed(withProcList(80, 12, []Project{{Name: "scrn", Path: "/p/scrn"}}, nil))
	if len(m.rows) != 0 {
		t.Fatalf("setup: rows = %d, want none", len(m.rows))
	}
	press(press(press(m, "G"), "g"), "g")
}

func TestTheEndsWorkWithinAFilter(t *testing.T) {
	// The list they move through is the one on screen.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "s")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(model)
	if len(m.rows) < 2 {
		t.Fatalf("setup: rows = %d, want a few matches", len(m.rows))
	}

	m = press(m, "G")
	if m.cursor != len(m.rows)-1 {
		t.Errorf("cursor = %d, want the last match", m.cursor)
	}
}

func TestGIsALetterWhileAFilterIsBeingTyped(t *testing.T) {
	m := press(narrowed(manyProjects(90, 14)), "/")
	m = typeFilter(m, "g")

	if m.filter != "g" {
		t.Errorf("filter = %q, want the g typed into it", m.filter)
	}
	if m.pendingG {
		t.Error("a g in a filter is a letter, not the start of a motion")
	}
}

func TestTheKeysListTheEnds(t *testing.T) {
	f := keysOf(sized(160, 24))
	for _, key := range []string{"gg · G", "top · bottom"} {
		if !strings.Contains(f, key) {
			t.Errorf("keys = %q, want %q listed", f, key)
		}
	}
}

// --- acting on what the filter found -------------------------------------

func TestCtrlNAndCtrlPMoveWhileTyping(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "s")
	if len(m.rows) < 2 {
		t.Fatalf("setup: rows = %d, want a few matches", len(m.rows))
	}
	if m.cursor != 0 {
		t.Fatal("setup: expected to start at the top")
	}

	next, _ := m.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	m = next.(model)
	if m.cursor != 1 {
		t.Errorf("cursor = %d, want ctrl+n to move down", m.cursor)
	}
	if !m.typing {
		t.Error("moving should not end the looking up")
	}

	next, _ = m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if got := next.(model).cursor; got != 0 {
		t.Errorf("cursor = %d, want ctrl+p to move back up", got)
	}
}

func TestTypingStillNarrowsAfterMoving(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "s")
	next, _ := m.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	m = typeFilter(next.(model), "crn")

	if m.filter != "scrn" {
		t.Errorf("filter = %q, want the letters to have gone on narrowing it", m.filter)
	}
	wantRows(t, navColumn(m), []string{"▸scrn"})
}

func TestLettersAreStillLettersWhileTyping(t *testing.T) {
	// The actions are on chords because a project called "scrn" has to be
	// typeable without s doing something.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "sarx")
	if m.filter != "sarx" {
		t.Errorf("filter = %q, want every letter typed into it", m.filter)
	}
	if len(m.terms) != 0 {
		t.Error("no letter should have started anything")
	}
}

func TestTheFilterPromptSitsAboveTheMode(t *testing.T) {
	// The prompt is messaging, so it stacks above the chip; the chip keeps
	// the bottom line to itself.
	m := typeFilter(press(narrowed(manyProjects(160, 24)), "/"), "b")
	if got := footer(m); got != "/b█ navigate" {
		t.Errorf("footer = %q, want the prompt above the mode alone", got)
	}
}

// --- the pid, and where a process is listening ---------------------------

func TestThePidIsOnlyShownWhenEveryProcessIs(t *testing.T) {
	// Folded, the list is about what is happening and the pid is a number
	// beside every row that never helps you read it.
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
		})
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ nvim"})

	m = press(m, "-")
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ zsh 10", "   └─ nvim 20"})
}

func TestTwoOfTheSameCommandAreStillToldApartUnfolded(t *testing.T) {
	// Which is the point of the pid: unfolded it is what tells them apart.
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "nvim", Dir: "/p/scrn"},
			{PID: 11, PPID: 1, Command: "nvim", Dir: "/p/scrn"},
		})
	m = press(m, "-")
	wantRows(t, navColumn(m), []string{"▸scrn", " ├─ nvim 10", " └─ nvim 11"})
}

func TestAProcessListeningNowhereSaysNothingAboutPorts(t *testing.T) {
	// Most processes are not listening on anything, and a line saying so on
	// every one of them would be noise.
	self := &ProcNode{Proc: Proc{PID: pidOfSelf(), PPID: 1, Command: "test", Dir: "/tmp"}}
	for _, f := range procFields(self, nil, nil) {
		if f.label == "listening" {
			t.Errorf("this process is not a server, yet the pane says %q", f.value)
		}
	}
}

func TestPortsAreOrderedByNumber(t *testing.T) {
	got := []string{"8080", "80", "443"}
	sortPorts(got)
	want := []string{"80", "443", "8080"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ports = %v, want %v", got, want)
		}
	}
}

func TestASpaceInTheSearchDoesNotMoveTheCursor(t *testing.T) {
	// A filter is trimmed before it is matched against, so a space narrows
	// nothing. Sending the selection back to the top for one is the cursor
	// jumping in the middle of a name being typed.
	m := withProcList(90, 20, []Project{
		{Name: "alpha", Path: "/p/alpha"},
		{Name: "vim pro", Path: "/p/vim pro"},
		{Name: "vim proxy", Path: "/p/vim proxy"},
	}, nil)

	m = press(m, "/")
	for _, r := range "vim" {
		next, _ := m.Update(typed(string(r)))
		m = next.(model)
	}
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = next.(model)

	was, _ := m.selected()
	rows := len(m.rows)

	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	m = next.(model)

	if len(m.rows) != rows {
		t.Fatalf("the space changed the list from %d rows to %d", rows, len(m.rows))
	}
	if got, _ := m.selected(); got.project.Path != was.project.Path {
		t.Errorf("the space moved the cursor from %q to %q",
			was.project.Name, got.project.Name)
	}

	// A letter still does start again from the top: those rows really have
	// changed under the cursor.
	next, _ = m.Update(typed("p"))
	if m = next.(model); m.cursor != 0 {
		t.Errorf("cursor = %d after narrowing the list, want the top", m.cursor)
	}
}

func TestALostServerClearsTheShells(t *testing.T) {
	// The server hanging up is the ordinary end of holding nothing: the last
	// shell closed and the session went with it. The window only stops
	// showing what is no longer held; the bridge watches for a new server on
	// its own.
	m := sized(90, 14)
	m.terms = map[int]*remoteTerm{700: {pid: 700}}
	m.focus = 700
	next, _ := m.Update(daemonLostMsg{})
	got := next.(model)
	if len(got.terms) != 0 || got.focus != 0 {
		t.Errorf("terms = %d, focus = %d; want the held shells cleared", len(got.terms), got.focus)
	}
	if got.status != "" {
		t.Errorf("status = %q, want a clean loss to say nothing", got.status)
	}
}

func TestAFailedConnectIsRetried(t *testing.T) {
	// Not reaching the daemon leaves nothing to wait on but the retry itself.
	_, cmd := sized(90, 14).Update(daemonReadyMsg{err: errors.New("no daemon")})
	if cmd == nil {
		t.Fatal("no command, want another attempt scheduled")
	}
}

func TestTheChaseBacksOffToACap(t *testing.T) {
	// The one failure retried from the model is tmux itself missing; the
	// retries slow down rather than hammering.
	m := sized(90, 14)
	for range 12 {
		next, _ := m.Update(daemonReadyMsg{err: errors.New("tmux is not installed")})
		m = next.(model)
	}
	if m.backoff != reconnectMax {
		t.Errorf("backoff = %v after many failures, want capped at %v", m.backoff, reconnectMax)
	}
}

func TestADaemonTalkingResetsTheChase(t *testing.T) {
	m := sized(90, 14)
	m.backoff = reconnectMax
	next, _ := m.Update(sessionsMsg{})
	if got := next.(model).backoff; got != 0 {
		t.Errorf("backoff = %v after the daemon spoke, want the chase forgotten", got)
	}
}

func TestOnlyOneScanIsEverOut(t *testing.T) {
	// On a machine where lsof stalls, a poll that kept asking would pile a
	// stalled scan on top of every tick.
	m := sized(80, 8)
	if m.scanPoll() == nil {
		t.Fatal("an idle model should scan on the tick")
	}
	if m.scanPoll() != nil {
		t.Error("a second poll started a scan behind the first")
	}
	if m.rescan {
		t.Error("the poll owes nothing: a scan already out is answer enough")
	}
}

func TestAScanAskedForDuringOneIsOwed(t *testing.T) {
	// The scan already out began before the event that is asking, so its
	// answer cannot carry it.
	m := sized(80, 8)
	if m.scanNow() == nil {
		t.Fatal("an idle model should scan at once")
	}
	if m.scanNow() != nil {
		t.Error("a second ask started a scan behind the first")
	}
	if !m.rescan {
		t.Fatal("an event's ask during a scan should be owed")
	}

	next, _ := m.Update(procsMsg{})
	got := next.(model)
	if !got.scanning {
		t.Error("the owed scan should have gone out with the answer's arrival")
	}
	if got.rescan {
		t.Error("the debt should be settled by paying it")
	}
}

func TestAScanFailureKeepsTheLastList(t *testing.T) {
	// A failed scan says nothing about what is running; blanking the tree on
	// every hiccup of a loaded machine would be flicker, not information.
	m := withProcs(80, 8, []Project{{Name: "alpha", Path: "/p/alpha"}}, []string{"/p/alpha"})
	next, _ := m.Update(procsMsg{err: errors.New("lsof timed out")})
	got := next.(model)

	if len(got.procs) != 1 {
		t.Errorf("procs = %d, want the last good list kept", len(got.procs))
	}
	if !got.statusErr || got.status == "" {
		t.Error("a scan failure should be reported, not shown as an empty machine")
	}
}

func TestASlowListingIsCutOff(t *testing.T) {
	if _, err := listing(50*time.Millisecond, "sleep", "5"); err == nil {
		t.Error("a command that outlives the timeout should come back an error")
	}
}

func TestARepoIsReAskedOnItsOwnSlowerCadence(t *testing.T) {
	// A repository's details are half a dozen git spawns, and git status in a
	// large checkout is real work; what they say changes at the speed of a
	// person committing. Only the background refresh slows — landing on a row
	// still loads it at once, through the cache-miss path.
	m := threeRepos(8)
	m.ticks = 1
	if m.refreshDetailCmd() != nil {
		t.Error("a repo's details were re-asked on an off-cadence poll")
	}
	m.ticks = repoDetailEvery
	if m.refreshDetailCmd() == nil {
		t.Error("the cadence tick should refresh the repo's details")
	}
}

func TestAProcessIsReAskedOnEveryPoll(t *testing.T) {
	// cpu, memory and ports are exactly the numbers that move.
	m := withProcs(80, 8, []Project{{Name: "a", Path: "/p/a"}}, []string{"/p/a"})
	m.cursor = 1
	m.ticks = 1
	if m.refreshDetailCmd() == nil {
		t.Error("a process's details should refresh on every poll")
	}
}

// subbed is a model whose one repository holds two sub-projects, showing all.
func subbed(procs ...Proc) model {
	m := sized(90, 20)
	m.showAll = true
	m.projects = []Project{{Name: "mono", Path: "/p/mono"}}
	m.subs = map[string][]Project{"/p/mono": {
		{Name: "services/api", Path: "/p/mono/services/api"},
		{Name: "web", Path: "/p/mono/web"},
	}}
	m.procs = procs
	m.rebuild()
	return m
}

func TestProcessesFileUnderTheirSubProject(t *testing.T) {
	// A monorepo is a projects directory that happens to be one repository:
	// work inside a sub-project is listed there, not in a heap at the root.
	m := subbed(
		Proc{PID: 100, PPID: 1, Command: "node", Dir: "/p/mono/services/api"},
		Proc{PID: 101, PPID: 1, Command: "make", Dir: "/p/mono"},
	)
	wantRows(t, navColumn(m), []string{
		"▸mono",
		" ├─ make",
		" ├─ services/api",
		" │ └─ node",
		" └─ web",
	})
}

func TestIdleSubProjectsAreBehindTheDot(t *testing.T) {
	// The repositories' own rule: what has work shows, the rest waits.
	m := narrowed(subbed(Proc{PID: 100, PPID: 1, Command: "node", Dir: "/p/mono/services/api"}))
	wantRows(t, navColumn(m), []string{
		"▸mono",
		" └─ services/api",
		"   └─ node",
	})
	for _, row := range navColumn(m) {
		if strings.Contains(row, "web") {
			t.Errorf("row %q shows an idle sub-project in the narrowed view", row)
		}
	}
}

func TestTheFilterReachesAnIdleSubProject(t *testing.T) {
	// The cold start at work: nothing is running, and /api still lands you
	// somewhere you can press s or r.
	m := typeFilter(press(subbed(), "/"), "api")
	wantRows(t, navColumn(m), []string{
		" mono",
		"▸└─ services/api",
	})
	for _, row := range navColumn(m) {
		if strings.Contains(row, "web") {
			t.Errorf("row %q does not answer to the query", row)
		}
	}
}

func TestAnEmptyQueryListsProjectsAlone(t *testing.T) {
	// Every sub-project of every repository would bury the list of names
	// being remembered.
	m := press(subbed(), "/")
	for _, row := range navColumn(m) {
		if strings.Contains(row, "services") || strings.Contains(row, "web") {
			t.Errorf("row %q lists a sub-project before anything was typed", row)
		}
	}
}

func TestAShellOnASubProjectStartsThere(t *testing.T) {
	r := navRow{kind: rowSub, project: Project{Name: "services/api", Path: "/p/mono/services/api"}}
	if got := (model{}).shellDir(r); got != "/p/mono/services/api" {
		t.Errorf("shellDir = %q, want the sub-project's own directory", got)
	}
}

// cursorOn puts the cursor on the row for a path, failing if there is none.
func cursorOn(t *testing.T, m model, path string) model {
	t.Helper()
	for i, r := range m.rows {
		if r.kind != rowProc && r.project.Path == path {
			m.cursor = i
			return m
		}
	}
	t.Fatalf("no row for %q", path)
	return m
}

func TestXOnASubProjectTakesOnlyItsProcesses(t *testing.T) {
	m := subbed(
		Proc{PID: 100, PPID: 1, Command: "node", Dir: "/p/mono/services/api"},
		Proc{PID: 101, PPID: 1, Command: "make", Dir: "/p/mono"},
	)
	m = press(cursorOn(t, m, "/p/mono/services/api"), "x")

	if m.pendingKill == nil {
		t.Fatal("x on a sub-project should arm a kill")
	}
	if len(m.pendingKill.nodes) != 1 || m.pendingKill.nodes[0].PID != 100 {
		t.Errorf("nodes = %+v, want the sub-project's process alone", m.pendingKill.nodes)
	}
	if !strings.Contains(m.pendingKill.subject, "services/api") {
		t.Errorf("subject = %q, want it named for the sub-project", m.pendingKill.subject)
	}
}

func TestXOnTheRepositoryTakesSubProcessesToo(t *testing.T) {
	// The sub-projects are on screen beneath it; an x that ignored them
	// would be an x ignoring half of what is shown.
	m := subbed(
		Proc{PID: 100, PPID: 1, Command: "node", Dir: "/p/mono/services/api"},
		Proc{PID: 101, PPID: 1, Command: "make", Dir: "/p/mono"},
	)
	m = press(cursorOn(t, m, "/p/mono"), "x")

	if m.pendingKill == nil || len(m.pendingKill.nodes) != 2 {
		t.Fatalf("pendingKill = %+v, want both processes", m.pendingKill)
	}
}

func TestWorkInASubProjectKeepsItsRepositoryListed(t *testing.T) {
	m := narrowed(subbed(Proc{PID: 100, PPID: 1, Command: "node", Dir: "/p/mono/services/api"}))
	if len(m.rows) == 0 || m.rows[0].project.Name != "mono" {
		t.Fatalf("rows = %d, want the repository listed for its sub's work", len(m.rows))
	}
}

// groupedModel is a model with one group of two repositories and one
// repository standing alone, showing all.
func groupedModel(procs ...Proc) model {
	m := sized(90, 20)
	m.showAll = true
	m.projects = []Project{
		{Name: "api", Path: "/p/checklists.org/api", Group: "/p/checklists.org"},
		{Name: "web", Path: "/p/checklists.org/web", Group: "/p/checklists.org"},
		{Name: "scrn", Path: "/p/scrn"},
	}
	m.groups = []Project{{Name: "checklists.org", Path: "/p/checklists.org"}}
	m.procs = procs
	m.rebuild()
	return m
}

func TestAGroupHoldsItsRepositories(t *testing.T) {
	// A project is often several repositories in one folder, worked on at
	// that level; the folder gets the row and its repositories sit under it.
	m := groupedModel()
	wantRows(t, navColumn(m), []string{
		"▸checklists.org",
		"   api",
		"   web",
		" scrn",
	})
}

func TestWorkInARepositoryLiftsItsGroupIntoView(t *testing.T) {
	m := narrowed(groupedModel(Proc{PID: 100, PPID: 1, Command: "node", Dir: "/p/checklists.org/api"}))
	wantRows(t, navColumn(m), []string{
		"▸checklists.org",
		"   api",
		"   └─ node",
	})
	for _, row := range navColumn(m) {
		if strings.Contains(row, "web") || strings.Contains(row, "scrn") {
			t.Errorf("row %q has no work and should not be listed", row)
		}
	}
}

func TestAShellAtTheGroupLevelBelongsToTheGroup(t *testing.T) {
	// Working at that level means a shell opened there, in none of the
	// repositories; it belongs to the group row.
	m := narrowed(groupedModel(Proc{PID: 100, PPID: 1, Command: "zsh", Dir: "/p/checklists.org"}))
	wantRows(t, navColumn(m), []string{
		"▸checklists.org",
		"   └─ zsh",
	})
}

func TestTheFilterFindsTheGroupByName(t *testing.T) {
	m := typeFilter(press(groupedModel(), "/"), "check")
	rows := navColumn(m)
	if len(rows) == 0 || !strings.Contains(rows[0], "checklists.org") {
		t.Fatalf("rows = %v, want the group found by its name", rows)
	}
	for _, row := range rows {
		if strings.Contains(row, "scrn") {
			t.Errorf("row %q does not answer to the query", row)
		}
	}
}

func TestXOnAGroupTakesEverythingInIt(t *testing.T) {
	m := groupedModel(
		Proc{PID: 100, PPID: 1, Command: "node", Dir: "/p/checklists.org/api"},
		Proc{PID: 101, PPID: 1, Command: "vite", Dir: "/p/checklists.org/web"},
		Proc{PID: 102, PPID: 1, Command: "zsh", Dir: "/p/checklists.org"},
	)
	m = press(cursorOn(t, m, "/p/checklists.org"), "x")

	if m.pendingKill == nil || len(m.pendingKill.nodes) != 3 {
		t.Fatalf("pendingKill = %+v, want all three processes in the group", m.pendingKill)
	}
	if !strings.Contains(m.pendingKill.subject, "checklists.org") {
		t.Errorf("subject = %q, want it named for the group", m.pendingKill.subject)
	}
}

func TestAShellOnAGroupRowStartsAtTheGroup(t *testing.T) {
	r := navRow{kind: rowGroup, project: Project{Name: "checklists.org", Path: "/p/checklists.org"}}
	if got := (model{}).shellDir(r); got != "/p/checklists.org" {
		t.Errorf("shellDir = %q, want the group's own directory", got)
	}
}

func TestTheGlanceAttachesAndTheLeavingDetaches(t *testing.T) {
	// A shell this window never stepped into previews blank unless the pane
	// asks for its screens: landing on the row attaches, and moving off
	// detaches, so a glance does not keep a say in the shell's size forever.
	m := withProcList(90, 14,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/tmp"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700}}
	m, asked := pipeDaemon(t, m)

	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // onto the shell's row
	m = next.(model)
	if got := askedFor(t, asked); got.Kind != kindAttach || got.PID != 700 {
		t.Fatalf("asked %q for pid %d, want the shell's screen for 700", got.Kind, got.PID)
	}

	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp}) // back to the repo row
	m = next.(model)
	if m.previewing != 0 {
		t.Errorf("previewing = %d, want nothing", m.previewing)
	}
	m.daemon.mu.Lock()
	watching := m.daemon.watching[700]
	m.daemon.mu.Unlock()
	if watching {
		t.Error("leaving the row let the watch stand")
	}
}

func TestAWideNavigatorInANarrowWindowDoesNotPanic(t *testing.T) {
	// navWidth is the user's to set, up to 60 — and the window is the
	// terminal's to size. At 60 columns each, the pane came out a column
	// short of nothing, and the negative width walked into the renderer.
	old := navWidth
	t.Cleanup(func() { navWidth = old })
	applyNavWidth(60)

	m := withProcList(60, 24,
		[]Project{{Name: "tmp", Path: "/tmp"}},
		[]Proc{{PID: 700, PPID: 1, Command: "zsh", Dir: "/tmp"}})
	m.terms = map[int]*remoteTerm{700: {pid: 700, screen: "held"}}
	m.cursor = 1 // the shell's row, where the banner and its rule would draw

	view := stripANSI(m.View().Content)
	if strings.Contains(view, "│") {
		t.Errorf("a pane with no room was drawn anyway:\n%s", view)
	}
}

func TestTheJumpToAWaitingAgentLeavesTheFilter(t *testing.T) {
	// The chord means "from anywhere". While typing, the rows are the
	// query's answers — places alone until a query lands — and a waiting
	// agent used to be invisible to it: the chord said nothing was owed
	// while a diamond stood in plain sight.
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: waitingStatus, WaitingFor: "permission prompt"},
	})
	m = press(m, "/")

	m = chordKey(m, tea.KeyEnter)
	if m.status == "no agent is waiting" {
		t.Error("the chord searched only the filter's answers")
	}
	if m.typing {
		t.Error("the jump should be the end of looking")
	}
}

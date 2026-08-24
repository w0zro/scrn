package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// sized returns a model laid out for the given terminal dimensions.
func sized(w, h int) model {
	m, _ := newModel().Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m.(model)
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

// press sends a key and returns the resulting model.
func press(m model, key string) model {
	var msg tea.KeyMsg
	switch key {
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
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

// bodyRows returns the rows between the header and the footer.
func bodyRows(m model) []string {
	all := strings.Split(m.View(), "\n")
	if len(all) < 3 {
		return nil
	}
	out := make([]string, 0, len(all)-2)
	for _, ln := range all[1 : len(all)-1] {
		out = append(out, stripANSI(ln))
	}
	return out
}

// navColumn returns the non-blank navigator rows alone.
func navColumn(m model) []string {
	var out []string
	for _, row := range bodyRows(m) {
		nav, _ := splitRow(row)
		if nav = strings.TrimRight(nav, " "); strings.TrimSpace(nav) != "" {
			out = append(out, nav)
		}
	}
	return out
}

// detailColumn returns the non-blank detail rows alone.
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
	lines := strings.Split(sized(80, 24).View(), "\n")
	if got := len(lines); got != 24 {
		t.Fatalf("view height = %d lines, want 24", got)
	}
	if !strings.HasPrefix(stripANSI(lines[0]), "scrn") {
		t.Errorf("first line = %q, want it to start with %q", lines[0], "scrn")
	}
}

func TestNavPaneOccupiesItsColumn(t *testing.T) {
	lines := strings.Split(sized(80, 24).View(), "\n")
	for i := 1; i < len(lines)-1; i++ {
		row := stripANSI(lines[i])
		if got := strings.Index(row, "│"); got != navWidth {
			t.Fatalf("row %d: divider at column %d, want %d (row %q)", i, got, navWidth, row)
		}
	}
}

func TestDetailPaneDroppedWhenTooNarrow(t *testing.T) {
	view := stripANSI(sized(navMin-1, 24).View())
	if strings.Contains(view, "│") {
		t.Errorf("detail pane drawn below %d columns:\n%s", navMin, view)
	}
}

func TestViewFitsShortTerminals(t *testing.T) {
	for _, h := range []int{0, 1, 2, 3} {
		got := len(strings.Split(sized(80, h).View(), "\n"))
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
	// happening, and the row is named for what it ends in.
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "claude", Dir: "/p/scrn"},
			{PID: 30, PPID: 20, Command: "go", Dir: "/p/scrn/cmd"},
		},
	)
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ go 30"})

	if r, _ := m.rows[1], 0; r.chain.PID != 10 {
		t.Errorf("chain starts at %d, want the shell at the top of the run", r.chain.PID)
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
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ nvim 21"})
}

func TestARunStopsFoldingWhereItBranches(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "nvim", Dir: "/p/scrn"},
			{PID: 30, PPID: 10, Command: "go", Dir: "/p/scrn"},
		},
	)
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ zsh 10", "   ├─ nvim 20", "   └─ go 30"})
}

func TestNavDrawsSiblingsWithContinuationRules(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "vim", Dir: "/p/scrn"},
			{PID: 30, PPID: 10, Command: "go", Dir: "/p/scrn"},
			{PID: 40, PPID: 20, Command: "fmt", Dir: "/p/scrn"},
			{PID: 50, PPID: 20, Command: "lint", Dir: "/p/scrn"},
		},
	)
	wantRows(t, navColumn(m), []string{
		"▸scrn", " └─ zsh 10", "   ├─ vim 20", "   │ ├─ fmt 40", "   │ └─ lint 50", "   └─ go 30",
	})
}

func TestProcessesGoUnderTheInnermostRepo(t *testing.T) {
	m := withProcList(80, 12,
		[]Project{{Name: "outer", Path: "/p/outer"}, {Name: "inner", Path: "/p/outer/inner"}},
		[]Proc{{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/outer/inner/src"}},
	)
	if n := len(m.byRepo["/p/outer"]); n != 0 {
		t.Errorf("outer repo got %d processes, want 0; the nested repo owns it", n)
	}
	if n := len(m.byRepo["/p/outer/inner"]); n != 1 {
		t.Errorf("inner repo got %d processes, want 1", n)
	}
}

// --- the "a" toggle -------------------------------------------------------

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

	m = press(m, "a")
	if !strings.Contains(strings.Join(navColumn(m), "\n"), "idle") {
		t.Error("a should bring every repo into the list")
	}
	m = press(m, "a")
	if strings.Contains(strings.Join(navColumn(m), "\n"), "idle") {
		t.Error("a should narrow back to running repos")
	}
}

func TestNarrowingRescansProcesses(t *testing.T) {
	wide := sized(80, 8)
	wide.showAll = true

	_, cmd := wide.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if cmd == nil {
		t.Error("narrowing should rescan processes so the list is current")
	}
}

func TestFooterAdvertisesTheToggle(t *testing.T) {
	m := sized(160, 8)
	if !strings.Contains(stripANSI(m.View()), "a all") {
		t.Error("footer should offer to show all while narrowed, which is the default")
	}
	if !strings.Contains(stripANSI(press(m, "a").View()), "a running") {
		t.Error("footer should offer the running-only view once showing all")
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
	wantRows(t, navColumn(m), []string{" scrn", "▸└─ zsh 10"})
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
	m := manyRepos(10, 5) // 3 body rows
	for i := 0; i < 3; i++ {
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
	for i := 0; i < 9; i++ {
		m = press(m, "down")
	}
	if want := len(m.rows) - m.bodyHeight(); m.offset != want {
		t.Errorf("offset = %d, want %d; the window should not scroll past the end", m.offset, want)
	}
}

// --- detail pane ----------------------------------------------------------

func TestDetailPaneDescribesTheSelectedRepo(t *testing.T) {
	m := withProcList(80, 12, []Project{{Name: "alpha", Path: "/p/alpha"}}, nil)
	m.details[detailKey(m.rows[0])] = []field{{"name", "alpha"}, {"path", "/p/alpha"}}

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
	m.details[detailKey(m.rows[0])] = []field{{"name", "scrn"}}
	m.details[detailKey(m.rows[1])] = []field{{"command", "zsh"}}

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
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if cmd == nil {
		t.Error("moving the cursor should request details for the newly selected row")
	}
}

func TestDetailIsNotRefetchedWhenCached(t *testing.T) {
	m := threeRepos(10)
	m.details[detailKey(m.rows[1])] = []field{{"name", "b"}}

	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown}); cmd != nil {
		t.Error("a row already inspected should not be inspected again")
	}
}

func TestStaleDetailKeysAreIgnored(t *testing.T) {
	m := threeRepos(10)
	next, _ := m.Update(detailMsg{key: "repo:/p/gone", fields: []field{{"name", "gone"}}})

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
		if s[i] == 0x1b {
			for i < len(s) && !isANSITerm(s[i]) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isANSITerm(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// --- collapsing -----------------------------------------------------------

// nestedTree is one repo with zsh → (vim → fmt, go).
func nestedTree(h int) model {
	return withProcList(80, h,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{
			{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"},
			{PID: 20, PPID: 10, Command: "vim", Dir: "/p/scrn"},
			{PID: 30, PPID: 10, Command: "go", Dir: "/p/scrn"},
			{PID: 40, PPID: 20, Command: "fmt", Dir: "/p/scrn"},
			{PID: 50, PPID: 20, Command: "lint", Dir: "/p/scrn"},
		},
	)
}

func TestSpaceCollapsesAProcessNode(t *testing.T) {
	m := nestedTree(12)
	wantRows(t, navColumn(m), []string{
		"▸scrn", " └─ zsh 10", "   ├─ vim 20", "   │ ├─ fmt 40", "   │ └─ lint 50", "   └─ go 30",
	})

	// Move onto vim and fold it.
	m = press(press(press(m, "down"), "down"), " ")
	wantRows(t, navColumn(m), []string{
		" scrn", " └─ zsh 10", "▸  ├─ vim 20 +2", "   └─ go 30",
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
	// zsh hides vim, go and fmt.
	m := press(press(nestedTree(12), "down"), " ")
	wantRows(t, navColumn(m), []string{" scrn", "▸└─ zsh 10 +4"})
}

func TestSpaceOnALeafDoesNothing(t *testing.T) {
	m := nestedTree(12)
	for i := 0; i < 4; i++ {
		m = press(m, "down") // onto "go 30", a leaf
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

func TestFooterAdvertisesCollapse(t *testing.T) {
	if !strings.Contains(stripANSI(sized(160, 8).View()), "space collapse") {
		t.Error("footer should mention the collapse key")
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

func footer(m model) string {
	all := strings.Split(m.View(), "\n")
	return stripANSI(all[len(all)-1])
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
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd != nil {
		t.Error("the first x should only arm the confirmation, not signal anything")
	}
}

func TestConfirmingRunsTheKill(t *testing.T) {
	m := press(press(nestedTree(12), "down"), "x")

	for _, key := range []string{"x", "y", "enter"} {
		var msg tea.KeyMsg
		if key == "enter" {
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		} else {
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
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

	for _, key := range []string{"n", "esc", "j", "a", " "} {
		var msg tea.KeyMsg
		switch key {
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
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

	next, _ := armed.Update(tea.KeyMsg{Type: tea.KeyDown})
	if next.(model).cursor != cursorWas {
		t.Error("the key that cancels a kill should not also move the cursor")
	}
}

func TestQuitStillWorksWhileArmed(t *testing.T) {
	// Cancelling is the priority, but the user must not be trapped: the next
	// key after cancelling quits as usual.
	armed := press(press(nestedTree(12), "down"), "x")
	cancelled := press(armed, "q")
	if _, cmd := cancelled.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Error("q should quit once the confirmation is cleared")
	}
}

func TestXOnARepoSaysItIsNotKillable(t *testing.T) {
	m := press(nestedTree(12), "x") // cursor is on the repo row

	if m.pendingKill != nil {
		t.Error("a repository should not arm a kill")
	}
	if f := footer(m); !strings.Contains(f, "select a process") {
		t.Errorf("footer = %q, want it to explain why nothing happened", f)
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
	m := press(nestedTree(12), "x") // leaves the "select a process" status
	if !strings.Contains(footer(m), "select a process") {
		t.Fatal("expected a status to clear")
	}

	m = press(m, "down")
	if strings.Contains(footer(m), "select a process") {
		t.Errorf("status should clear once the cursor moves, footer = %q", footer(m))
	}
}

func TestFooterAdvertisesKill(t *testing.T) {
	if !strings.Contains(footer(sized(80, 8)), "x kill") {
		t.Error("footer should mention the kill key")
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
	for i := 0; i < 3; i++ {
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
		{PID: 30, PPID: 10, Command: "go", Dir: "/p/scrn"},
	}})

	col := strings.Join(navColumn(next.(model)), "\n")
	if strings.Contains(col, "fmt 40") {
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
		{PID: 30, PPID: 10, Command: "go", Dir: "/p/scrn"},
	}})

	if c := next.(model).cursor; c == 0 {
		t.Error("the cursor jumped to the top when its process exited; it should hold its place")
	}
}

func TestCursorClampsWhenTheListShrinksPastIt(t *testing.T) {
	m := nestedTree(12)
	for i := 0; i < 4; i++ {
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
	m.details[detailKey(m.rows[0])] = []field{{"name", "stale"}}

	if cmd := m.refreshDetailCmd(); cmd == nil {
		t.Error("the selected row should be re-inspected even when cached")
	}
}

func TestRefreshDoesNotBlankTheDetailPane(t *testing.T) {
	m := nestedTree(12)
	m.details[detailKey(m.rows[0])] = []field{{"name", "scrn"}}

	next, _ := m.Update(tickMsg{})
	if strings.Contains(strings.Join(detailColumn(next.(model)), "\n"), "loading") {
		t.Error("a refresh should keep showing the old value until the new one lands")
	}
}

func TestStaleDetailsArePruned(t *testing.T) {
	m := nestedTree(12)
	m.details["proc:99999"] = []field{{"command", "long gone"}}
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
	if !strings.Contains(row, "fmt 40") {
		t.Fatalf("row = %q, want the process still listed until it is seen gone", row)
	}
	if !strings.Contains(row, spinFrames[got.frame%len(spinFrames)]) {
		t.Errorf("row = %q, want a marker on the signalled process", row)
	}
}

func TestOnlyTheSignalledProcessIsMarked(t *testing.T) {
	next, _ := nestedTree(12).Update(killed("fmt", 40))

	for i, row := range navColumn(next.(model)) {
		if strings.Contains(row, "fmt 40") {
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
		{PID: 30, PPID: 10, Command: "go", Dir: "/p/scrn"},
	}})
	got := next.(model)

	if col := strings.Join(navColumn(got), "\n"); strings.Contains(col, "fmt 40") {
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
	for i := 0; i < 2*rescanFrames; i++ {
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

	next, cmd = next.(model).Update(killed("go", 30))
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
	if col := strings.Join(navColumn(got), "\n"); !strings.Contains(col, "fmt 40") {
		t.Errorf("the process is still running and should still be listed:\n%s", col)
	}
}

// --- killing a whole tree ------------------------------------------------

func TestXKillsTheSubtreeParentsFirst(t *testing.T) {
	// zsh 10 holds vim 20 (holding fmt 40) and go 30.
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
	for i := 0; i < 3; i++ {
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

func TestXOnAnIdleRepoSaysThereIsNothingToKill(t *testing.T) {
	m := withProcList(80, 12, []Project{{Name: "scrn", Path: "/p/scrn"}}, nil)
	m = press(m, "X")

	if m.pendingKill != nil {
		t.Error("an idle repository should not arm a kill")
	}
	if f := footer(m); !strings.Contains(f, "nothing running in scrn") {
		t.Errorf("footer = %q, want it to explain why nothing happened", f)
	}
}

func TestXOnARepoPointsAtTheTreeKill(t *testing.T) {
	if f := footer(press(nestedTree(12), "x")); !strings.Contains(f, "X for the whole repository") {
		t.Errorf("footer = %q, want it to point at the key that does work here", f)
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
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
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
			{command: "fmt", pid: 40}, {command: "go", pid: 30},
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
			{command: "go", pid: 30, err: errors.New("already gone")},
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

func TestFooterAdvertisesTheTreeKill(t *testing.T) {
	if f := footer(sized(160, 8)); !strings.Contains(f, "X kill tree") {
		t.Errorf("footer = %q, want the tree kill advertised", f)
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
	m.claude = sessions
	return m
}

func TestABusyClaudeIsMarkedInTheNavigator(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: "busy"},
	})

	row := navColumn(m)[1]
	if !strings.Contains(row, "claude 700 ●") {
		t.Errorf("row = %q, want a filled marker on a working instance", row)
	}
}

func TestAWaitingClaudeIsMarkedDifferently(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: "idle"},
	})

	row := navColumn(m)[1]
	if !strings.Contains(row, "claude 700 ○") {
		t.Errorf("row = %q, want a hollow marker on an idle instance", row)
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

func TestClaudeDetailIsAskedForOnlyOnAClaudeRow(t *testing.T) {
	m := withClaude("claude", map[int]claudeSession{
		700: {PID: 700, Name: "scrn-1f", Status: "busy"},
	})

	repo, _ := m.rows[0], m.rows[1]
	if got := m.claudeFor(repo); got != nil {
		t.Errorf("claudeFor(repo row) = %+v, want nothing", got)
	}
	m.cursor = 1
	proc, _ := m.selected()
	if got := m.claudeFor(proc); got == nil || got.Name != "scrn-1f" {
		t.Errorf("claudeFor(claude row) = %+v, want the session", got)
	}
}

func TestTheFooterNeverOutgrowsTheWindow(t *testing.T) {
	// A hint that wraps costs a line the body was drawing on.
	for _, w := range []int{40, 60, 80, 100, 140} {
		if got := lipgloss.Width(footer(sized(w, 10))); got > w {
			t.Errorf("at width %d the footer is %d columns: %q", w, got, footer(sized(w, 10)))
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
	fs := procFields(r.node, r.run(), nil)

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
			{PID: 30, PPID: 10, Command: "go", Dir: "/p/scrn"},
		})
	m.cursor = 2 // nvim, which folded nothing because zsh branches

	r, _ := m.selected()
	if _, ok := fieldValue(procFields(r.node, r.run(), nil), "run"); ok {
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
	wantRows(t, navColumn(m), []string{"▸scrn", " └─ nvim 21"})

	m = press(m, "-")
	wantRows(t, navColumn(m), []string{
		"▸scrn", " └─ zsh 10", "   └─ nvim 20", "     └─ nvim 21",
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
	m.cursor = 1 // the folded row, named for nvim 21

	m = press(m, "-")
	if r, _ := m.selected(); r.node.PID != 21 {
		t.Errorf("selected pid %d, want to still be on nvim 21 after unfolding", r.node.PID)
	}
}

func TestTheFooterSaysWhichWayTheFoldGoes(t *testing.T) {
	m := sized(160, 8)
	if f := footer(m); !strings.Contains(f, "- every process") {
		t.Errorf("footer = %q, want it to offer the full tree", f)
	}
	m = press(m, "-")
	if f := footer(m); !strings.Contains(f, "- fold runs") {
		t.Errorf("footer = %q, want it to offer folding back", f)
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
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	return m
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
	// A project called "next" must be typeable without n opening a shell.
	m := press(narrowed(manyProjects(90, 14)), "/")
	m = typeFilter(m, "n")

	if m.filter != "n" {
		t.Errorf("filter = %q, want the keystroke to have gone into it", m.filter)
	}
	if len(m.terms) != 0 {
		t.Error("n while typing a filter should not open a shell")
	}
}

func TestBackspaceWidensTheFilter(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brandx")
	if len(m.rows) != 0 {
		t.Fatalf("setup: rows = %d, want no match", len(m.rows))
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	wantRows(t, navColumn(next.(model)), []string{"▸brand"})
}

func TestEnterKeepsTheFilterSoTheProjectStays(t *testing.T) {
	// Clearing it on accept would drop an idle project straight back out of
	// the narrowed list, before anything could be started in it.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
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
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)

	// n now means open a shell rather than a letter of the filter.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if got := next.(model); got.filter != "brand" {
		t.Errorf("filter = %q, want n to have been taken as a key", got.filter)
	}
}

func TestEscapeClearsTheFilterRatherThanQuitting(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("esc with a filter applied should clear it, not quit")
	}
	if got := next.(model); got.filter != "" {
		t.Errorf("filter = %q, want it cleared", got.filter)
	}
}

func TestEscapeStillQuitsWithNoFilter(t *testing.T) {
	if _, cmd := manyProjects(90, 14).Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd == nil {
		t.Error("esc should still quit when there is no filter to clear")
	}
}

func TestEscapeWhileTypingAbandonsTheFilter(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)

	if m.typing || m.filter != "" {
		t.Errorf("typing=%v filter=%q, want the filter abandoned", m.typing, m.filter)
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

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if f := footer(next.(model)); !strings.Contains(f, "filter bra") {
		t.Errorf("footer = %q, want it to show the filter still applied", f)
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
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
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
	wantRows(t, navColumn(m), []string{" brand", "▸└─ zsh 700"})
}

func TestTheSearchHoldsUntilTheShellActuallyLands(t *testing.T) {
	// Clearing on the keystroke would drop the project out of the list until
	// the scan caught up, which is a flicker for no reason.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
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
	m = connected(t, m)
	m.terms = map[int]*remoteTerm{700: {pid: 700, dir: "/p/hsg/brand"}}
	m.showAll = false
	m.filter = "brand"
	m.rebuild()
	m.cursor = 1

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := next.(model); got.filter != "" {
		t.Errorf("filter = %q, want stepping in to have finished the search", got.filter)
	}
}

func TestKillingDoesNotClearTheSearch(t *testing.T) {
	// Clearing out several projects is one job; the list should not move
	// underneath it after each one.
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "brand")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
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
	wantRows(t, navColumn(m), []string{
		"▸scrn", " hsg", " brand", " tressle-api", " flocking-pixi",
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
	wantRows(t, navColumn(m), []string{"▸scrn", " hsg"})
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
	wantRows(t, navColumn(m), []string{"▸hsg", " brand"}) // both are under /p/hsg
	m = typeFilter(m, "sg/h")
	wantRows(t, navColumn(m), []string{"▸hsg"})
}

func TestLeavingThePickerBringsTheProcessesBack(t *testing.T) {
	m := withProcList(90, 14,
		[]Project{{Name: "scrn", Path: "/p/scrn"}},
		[]Proc{{PID: 10, PPID: 1, Command: "zsh", Dir: "/p/scrn"}})

	m = press(m, "/")
	wantRows(t, navColumn(m), []string{"▸scrn"})

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	wantRows(t, navColumn(next.(model)), []string{"▸scrn", " └─ zsh 10"})
}

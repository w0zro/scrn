package main

import (
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// agentMark is the glyph beside an agent's row. A working one turns, one
// stopped mid-turn on a specific ask holds a bright diamond, one that has
// finished a turn and waits on its user holds a filled marker in the
// attention color, and one idle since it started — owed nothing — sits
// hollow and quiet. The diamond is the one worth crossing the room for:
// that answer resumes work already in flight.
func (m model) agentMark(r navRow, a agent) (string, lipgloss.Style) {
	if _, ok := a.blocked(); ok {
		return glyphAsk, blockedStyle
	}
	switch {
	case a.working():
		return spinFrames[m.frame%len(spinFrames)], busyStyle
	case m.awaiting(r) != nil:
		return glyphOn, attnStyle
	}
	return glyphOff, faintStyle
}

// View lays the window out as two full-height columns.
//
// scrn's own name and keys sit at the top and bottom of the left one rather
// than spanning the window, so the pane on the right is the attached process
// and nothing else. A terminal made to give up its first and last rows to a
// header and a footer is a terminal drawing something other than what it was
// told it had room for.
//
// What the attached process asked of the terminal window — its title, its
// progress — rides out on the view too. A program in the pane addresses those
// to the terminal it believes it is in, which is scrn; scrn is inside a real
// one, and the view is how it hands them on.
func (m model) View() tea.View {
	v := tea.NewView(m.layout())
	v.AltScreen = true
	v.MouseMode = m.mouseMode()
	v.WindowTitle = m.windowTitle
	v.ProgressBar = m.progressBar()
	return v
}

// mouseMode is whether scrn asks the terminal for the mouse at all — and
// mostly it does not. Tracking is all-or-nothing for the whole window, and
// holding it costs the terminal's own selection: the drag, the double-click,
// the copy-on-select, the links. So scrn holds the mouse only while the
// focused program asked for it — vim, htop — and forwards every event raw;
// the rest of the time the mouse is the terminal's, native everywhere, and
// the wheel still moves things as the terminal's alternate scroll arrows.
func (m model) mouseMode() tea.MouseMode {
	if t := m.focused(); t != nil && t.mouse {
		return tea.MouseModeCellMotion
	}
	return tea.MouseModeNone
}

// progressBar is the attached process's progress, restated for the window.
// Only a focused shell speaks for it: a build finishing in a pane being
// merely looked at should not set a bar on a tab showing something else.
func (m model) progressBar() *tea.ProgressBar {
	t := m.focused()
	if t == nil || t.progress == "" {
		// Nothing attached, or nothing running: a nil bar is the renderer's
		// cue to clear whatever the last shell had put up.
		return nil
	}
	// The payload the emulator hands over is the OSC 9;4 it heard —
	// "9;4;<state>;<value>" — and the states are numbered the same on both
	// sides of this restatement.
	parts := strings.Split(t.progress, ";")
	if len(parts) < 3 || parts[0] != "9" || parts[1] != "4" {
		return nil
	}
	state, err := strconv.Atoi(parts[2])
	if err != nil || state < int(tea.ProgressBarNone) || state > int(tea.ProgressBarWarning) {
		return nil
	}
	value := 0
	if len(parts) > 3 {
		value, _ = strconv.Atoi(parts[3])
	}
	return tea.NewProgressBar(tea.ProgressBarState(state), value)
}

// oscTitleText is the title out of an OSC 0, 1 or 2 payload, which is the part
// after the leading command number the emulator hands over.
func oscTitleText(data string) string {
	if _, rest, ok := strings.Cut(data, ";"); ok {
		return rest
	}
	return data
}

func (m model) layout() string {
	rows := m.height
	if rows <= 0 {
		rows = 1
	}

	left := m.leftColumn(rows)
	lines := padTo(left, rows)
	if m.showDetail() {
		right := m.paneLines(m.detailWidth(), rows)
		divider := ruleStyle.Render(glyphDivider)

		lines = make([]string, 0, rows)
		for i := 0; i < rows; i++ {
			// The pane's rows are somebody else's styling, captured as it
			// stood — a background left open at a row's end would run across
			// the newline and paint the next line's navigator. Every line
			// ends reset, so nothing a program set can outlive its row.
			lines = append(lines, pad(at(left, i), navWidth)+divider+at(right, i)+ansi.ResetStyle)
		}
	}
	if m.showHelp {
		lines = m.overlayKeys(lines)
	}
	return strings.Join(lines, "\n")
}

// leftColumn is scrn's own column: its name, the navigator, and its keys held
// down at the bottom.
func (m model) leftColumn(rows int) []string {
	hint := m.trimmedHint(rows)
	body := m.bodyHeight()

	// The name wears the gutter every row wears and takes a blank row after
	// it: a masthead, not the first item of the list. The blank is spacing,
	// and spacing is the first thing a short window gives up — bodyHeight
	// makes the same call, so the list and the layout agree.
	lines := make([]string, 0, rows)
	lines = append(lines, " "+titleStyle.Render("scrn"))
	if rows-2-len(hint) > 0 {
		lines = append(lines, "")
	}

	nav := m.navLines(body)
	if len(nav) > body {
		nav = nav[:body]
	}
	lines = append(lines, nav...)

	// Blank rows push the keys to the bottom rather than leaving them under
	// the last project.
	for len(lines) < rows-len(hint) {
		lines = append(lines, "")
	}
	return append(lines, hint...)
}

// trimmedHint is what scrn has to say at the foot of its column, cut to what
// the window can spare for it. Whatever is being said, the list keeps a row:
// a confirmation that wrapped over a short window would otherwise take the
// whole column, leaving nothing to say what is being confirmed about.
func (m model) trimmedHint(rows int) []string {
	hint := m.hintLines(m.hintWidth())
	if max := rows - 2; max > 0 && len(hint) > max {
		// The chip holds the bottom whatever else has to go, so the cut
		// takes the oldest lines from the top.
		hint = hint[len(hint)-max:]
	}
	return hint
}

// hintWidth is the room scrn's keys have: its own column, or the whole window
// when there is no pane beside it.
func (m model) hintWidth() int {
	if m.showDetail() {
		return navWidth
	}
	return m.width
}

// padTo lengthens lines to exactly n.
func padTo(lines []string, n int) []string {
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines[:n]
}

// hintLines is what scrn says at the foot of its column. The last line is
// the mode chip — where the keys are, worn the way vim wears INSERT — and
// it is the only thing there most of the time: the foot is beside the list
// all day, and busyness there is paid for on every frame. What has to be
// said — a confirmation, a report, the filter being typed — is said above
// the chip. The keys themselves live behind ?.
func (m model) hintLines(width int) []string {
	md := m.mode()
	look := modeLooks[md]
	bar := look.chip.Render(" " + md + " ")
	if fill := width - lipgloss.Width(bar); fill > 0 {
		bar += look.bar.Render(strings.Repeat(" ", fill))
	}
	return append(m.footLines(width), bar)
}

// footLines is the messaging above the chip, empty when there is nothing to
// say. A pending confirmation carries its answer with it: while it is on
// screen it is the only thing the next keystroke is about.
func (m model) footLines(width int) []string {
	switch {
	case m.pendingReplace:
		return hintBlock("end the server, and "+
			plural(len(m.terms), "shell", "shells")+"? · R confirms", width, warnStyle)

	case m.pendingKill != nil:
		return hintBlock("kill "+m.pendingKill.subject+"? · x confirms", width, warnStyle)

	case m.resume != nil:
		// The picker's look wears the filter's face: it is the same kind of
		// typing, aimed at conversations instead of places. What was just
		// reported — a missing daemon — takes the line above it.
		lines := hintBlock("/"+m.resume.query+"█", width, itemStyle)
		if m.status != "" {
			style := itemStyle
			if m.statusErr {
				style = errStyle
			}
			lines = append(hintBlock(m.status, width, style), lines...)
		}
		return lines

	case m.typing:
		// The prompt stays, because the typing has not stopped. What was just
		// reported takes the line above it: acting from the search is the
		// point of it, and an action that says nothing looks like one that
		// did nothing.
		lines := hintBlock("/"+m.filter+"█", width, itemStyle)
		if m.status != "" {
			style := itemStyle
			if m.statusErr {
				style = errStyle
			}
			lines = append(hintBlock(m.status, width, style), lines...)
		}
		return lines

	case m.scroll != nil && m.scroll.doc != nil:
		word := "scrollback · " + strconv.Itoa(m.scroll.above) + " up"
		if m.scroll.anchor >= 0 {
			lo, hi := m.scroll.selection(m.scroll.cur)
			word = plural(hi-lo+1, "line", "lines") + " marked · y copies"
		}
		lines := hintBlock(word, width, warnStyle)
		if m.status != "" {
			// A report mid-reading — an unmarked y's hint — takes the line
			// above, the way it does while typing.
			lines = append(hintBlock(m.status, width, itemStyle), lines...)
		}
		return lines

	case m.status != "":
		style := itemStyle
		if m.statusErr {
			style = errStyle
		}
		return hintBlock(m.status, width, style)

	case m.filter != "" && m.focused() == nil:
		return hintBlock("filter "+m.filter, width, selStyle)
	}
	return nil
}

// keysModal is every key, spelled out in a box for the middle of the window,
// cut down to what the window can hold. The list is ordered so that what a
// short window drops is what is least missed.
func (m model) keysModal(rows int) []string {
	keys := [][2]string{
		{"↑↓ j k", "move"},
		{"enter", "open"},
		{"s", "shell"},
		{"a", "agent"},
		{"A", "continue a conversation"},
		{"r", "run"},
		{"x · X", "kill · kill the tree"},
		{"/", "find a project · a process"},
		{"space · -", "fold · unfold all"},
		{".", "all · running"},
		{"gg · G", "top · bottom"},
		{"^spc n", "out to the navigator"},
		{"^spc j k", "next · previous shell"},
		{"^spc /", "find from anywhere"},
		{"^spc s a r A", "shell · agent · run · continue, here"},
		{"^spc v", "read the pane · v marks · y copies"},
		{"^spc ^spc", "back to the last process"},
		{"^spc enter", "the next waiting agent"},
		{"^spc q", "quit, even from a shell"},
		{"^spc ?", "these keys"},
		{"R", "end the server, shells and all"},
		{"q", "quit"},
	}
	// Four rows around the list: the borders, and a blank inside each — the
	// blanks going first when the window cannot spare them.
	air := rows-4 >= len(keys)
	max := rows - 2
	if air {
		max = rows - 4
	}
	if max < len(keys) {
		if max < 1 {
			return nil
		}
		keys = keys[:max]
	}

	var keyw, descw int
	for _, k := range keys {
		if w := lipgloss.Width(k[0]); w > keyw {
			keyw = w
		}
		if w := lipgloss.Width(k[1]); w > descw {
			descw = w
		}
	}
	inner := 1 + keyw + 2 + descw + 1

	// "─ keys " is seven columns, so the dashes make the row up to the same
	// inner width the content rows have.
	top := ruleStyle.Render("╭─ ") + headingStyle.Render("keys") +
		ruleStyle.Render(" "+strings.Repeat("─", inner-7)+"╮")
	edge := ruleStyle.Render("│")
	blank := edge + strings.Repeat(" ", inner) + edge
	lines := make([]string, 0, len(keys)+4)
	lines = append(lines, top)
	if air {
		lines = append(lines, blank)
	}
	for _, k := range keys {
		lines = append(lines, edge+" "+pad(itemStyle.Render(k[0]), keyw)+
			"  "+pad(hintStyle.Render(k[1]), descw)+" "+edge)
	}
	if air {
		lines = append(lines, blank)
	}
	return append(lines, ruleStyle.Render("╰"+strings.Repeat("─", inner)+"╯"))
}

// overlayKeys lays the keys modal over the composed frame, through the
// compositor: the frame is one layer and the box a layer above it, centered,
// so the window shows around the box rather than being replaced by it.
func (m model) overlayKeys(lines []string) []string {
	box := m.keysModal(len(lines))
	if len(box) == 0 || m.width <= 0 {
		return lines
	}
	x := max((m.width-lipgloss.Width(box[0]))/2, 0)
	y := (len(lines) - len(box)) / 2

	comp := lipgloss.NewCompositor(
		lipgloss.NewLayer(strings.Join(lines, "\n")).Z(0),
		lipgloss.NewLayer(strings.Join(box, "\n")).X(x).Y(y).Z(1),
	)
	// The compositor draws to the union of its layers, so a box wider than
	// a narrow window would widen every row and wrap the whole frame. The
	// window's width is the law; the box loses its right edge before the
	// layout loses its shape.
	out := strings.Split(comp.Render(), "\n")
	for i, ln := range out {
		out[i] = ansi.Truncate(ln, m.width, "")
	}
	return out
}

// hintBlock wraps a line of scrn's own words to the column it has.
func hintBlock(s string, width int, style lipgloss.Style) []string {
	chunks := wrapValue(s, width-1)
	if len(chunks) == 0 {
		return nil
	}
	lines := make([]string, 0, len(chunks))
	for _, c := range chunks {
		lines = append(lines, " "+style.Render(c))
	}
	return lines
}

// navLines renders the visible window of the navigator: repositories, each
// followed by the processes running in them, nested the way they started one
// another.
func (m model) navLines(rows int) []string {
	if m.err != nil {
		return wrapText(m.err.Error(), navWidth-1, rows, errStyle)
	}
	if m.projects == nil {
		return nil // still scanning
	}
	switch {
	case len(m.projects) == 0:
		return []string{" " + noteStyle.Render("no repositories")}
	case len(m.rows) == 0 && m.filter != "":
		return []string{" " + noteStyle.Render("no project matches")}
	case len(m.rows) == 0:
		return []string{
			" " + noteStyle.Render("nothing running"),
			"",
			" " + faintStyle.Render(".  show all"),
			" " + faintStyle.Render("/  find a project"),
		}
	}

	end := min(m.offset+rows, len(m.rows))

	lines := make([]string, 0, rows)
	for i := m.offset; i < end; i++ {
		lines = append(lines, m.renderRow(m.rows[i], i == m.cursor))
	}
	return lines
}

// renderRow draws one navigator row. The cursor is a marker in the gutter
// rather than a highlight, so it survives the tree rules beside it.
//
// A collapsed node carries the count of what it is hiding. That count is what
// distinguishes a folded node from a leaf, which the tree rules alone cannot
// show once the children are gone.
//
// A signalled process keeps its row and gains a red marker until a rescan finds
// it gone, so the list never claims an exit that has not been observed.
func (m model) renderRow(r navRow, selected bool) string {
	marker := " "
	if selected {
		marker = glyphSelected
	}
	style := m.rowStyle(r, selected)

	fold := ""
	if m.collapsed[detailKey(r)] {
		if n := m.childCount(r); n > 0 {
			fold = " +" + strconv.Itoa(n)
		}
	}

	// An agent's row says which of you the other is waiting on: a working
	// instance turns beside its name, and one that has finished a turn
	// lights the whole row, because done-and-waiting is the state that most
	// wants to be seen and the one a stopped spinner used to whisper.
	mark, markStyle := "", faintStyle
	if a := m.agentFor(r); a != nil {
		glyph, mstyle := m.agentMark(r, a)
		mark, markStyle = " "+glyph, mstyle
		if _, ok := a.blocked(); ok && !selected {
			style = blockedStyle
		} else if m.awaiting(r) != nil && !selected {
			style = attnStyle
		}
	}

	spinner := ""
	if r.kind == rowProc {
		if _, dying := m.dying[r.node.PID]; dying {
			spinner = " " + spinFrames[m.frame%len(spinFrames)]
		}
	}

	// A group or a repository sits on indent alone, naming a place the rows
	// beneath are inside. What hangs off a repository — its processes and its
	// sub-projects — is one family of siblings, and the tree rules say so.
	rules := r.prefix
	if r.kind == rowProc || r.kind == rowSub {
		branch := glyphBranch
		if r.last {
			branch = glyphLast
		}
		rules = r.prefix + branch + " "
	}
	// A repository is cut from the left and a command from the right, because
	// what identifies each is at that end: the repo name after its parents,
	// and the program before its arguments.
	label := r.project.Name
	fromLeft := strings.Contains(label, "/")
	if r.kind == rowProc {
		label = m.rowLabel(r)
		fromLeft = false
	}

	room := navWidth - 2 - lipgloss.Width(rules) - lipgloss.Width(fold) -
		lipgloss.Width(spinner) - lipgloss.Width(mark)

	// While a query is at work the matched letters are lit, so the narrowed
	// list always shows why it narrowed. The styled label is cut ansi-aware;
	// the plain path stays the plain cut.
	var seg string
	if q := strings.TrimSpace(m.filter); q != "" && (m.typing || m.filter != "") {
		seg = truncateStyled(highlight(label, matchSpans(q, label), style), room, fromLeft)
	} else if fromLeft {
		seg = style.Render(truncate(label, room))
	} else {
		seg = style.Render(truncateTail(label, room))
	}
	return marker + faintStyle.Render(rules) + seg +
		markStyle.Render(mark) + errStyle.Render(spinner) + faintStyle.Render(fold)
}

// rowLabel names a process row. A shell a project asked for by name is called
// that: "web" is what the project calls it and what you would say out loud,
// where "sleep 35228" is only true.
//
// The name belongs to the shell, so it stands for whatever is running in it —
// a run folded into one row is named for the shell that was asked for, not for
// the command that shell happens to be running now.
// The pid is only shown while every process is on a line of its own. Folded,
// the list is about what is happening and the pid is a number beside every row
// that never helps you read it; unfolded, it is what tells two nvim apart and
// what you would type at another window.
func (m model) rowLabel(r navRow) string {
	name := commandOf(r.node)

	// A shell a project asked for is called what the project calls it, unless
	// what is running in it says more. "dev" is a fine name for a plan entry
	// and a poor one for a row: "npm run dev" is the same thing said usefully,
	// and it is what a shell started by hand would show. A shell sitting at a
	// prompt has nothing better to offer, so there the name stands.
	if planned := m.plannedName(r); planned != "" && !tellsMore(name, planned) {
		name = planned
	}

	if m.unfolded {
		return name + " " + strconv.Itoa(r.node.PID)
	}
	return name
}

// plannedName is what a project called the shell a row stands for.
func (m model) plannedName(r navRow) string {
	for _, n := range r.run {
		if t, ok := m.terms[n.PID]; ok && t.name != "" {
			return t.name
		}
	}
	return ""
}

// tellsMore reports whether a command line says more than the name a plan gave
// it. It does when it is more than the name over again: "npm run dev" against
// "dev" is worth the width, "dev" against "dev" is not, and a bare shell is
// the plan's entry doing nothing in particular.
func tellsMore(command, planned string) bool {
	if command == "" || command == planned {
		return false
	}
	return !shells[strings.TrimPrefix(command, "-")]
}

// commandOf is what a process was run with, cut down to what identifies it.
//
// The name alone is often not the answer: "npm run dev" reports itself as a
// node, and a row saying node tells you nothing you did not know. What was
// typed is what you would call it.
func commandOf(n *ProcNode) string {
	if short := shortArgv(n.Argv); short != "" {
		return short
	}
	return n.Command
}

// shortArgv trims a command line to the part that says what it is, or returns
// nothing when the arguments would say less than the name alone.
//
// A path is cut to its last element, because /opt/homebrew/bin/npm and npm are
// the same thing to read. The arguments are kept, since they are the whole
// difference between one npm run and another.
func shortArgv(argv string) string {
	fields := strings.Fields(argv)
	if len(fields) == 0 {
		return ""
	}

	// A command run through an interpreter names itself in its arguments, so
	// the interpreter is worth dropping when something follows it.
	first := filepath.Base(fields[0])
	if len(fields) > 1 && interpreters[strings.TrimSuffix(first, ".exe")] {
		if next := fields[1]; !strings.HasPrefix(next, "-") {
			fields, first = fields[1:], filepath.Base(fields[1])
		}
	}

	// A shell given a script to run is the wrong thing to name a row after.
	// The script is somebody's idea of a command line, not a command: it is
	// long, it starts with whatever setup it needs, and by the time it has
	// been cut to fit, what is left is a fragment of a path. A bare shell says
	// less but is at least true.
	if shells[strings.TrimPrefix(first, "-")] {
		return ""
	}

	return strings.Join(append([]string{first}, fields[1:]...), " ")
}

// interpreters run something else, which is the thing worth naming.
var interpreters = map[string]bool{
	"node": true, "python": true, "python3": true, "ruby": true, "perl": true,
}

// rowStyle decides how brightly a row is drawn. Brightness in this list means
// the row can be stepped into: a repository opens a shell, and a shell scrn
// started can be returned to. Everything else is somebody else's process on
// somebody else's terminal, which scrn cannot attach to, so it is drawn dim
// rather than offered and then refused.
func (m model) rowStyle(r navRow, selected bool) lipgloss.Style {
	// While a project is being looked up the list is a reference rather than
	// the working view. Every row is a candidate and none of them has been
	// chosen, so nothing is lit but the one under the cursor.
	if m.typing {
		if selected {
			return selStyle
		}
		return faintStyle
	}
	if !m.attachable(r) {
		if selected {
			return offSelStyle
		}
		return faintStyle
	}
	if selected {
		return selStyle
	}
	return itemStyle
}

// paneLeft is the pane's first column in the window: past the navigator and
// the divider. The layout and the mouse map both read it here, so a change
// that moves the pane cannot leave the clicks landing where it used to be.
func (m model) paneLeft() int { return navWidth + 1 }

// detailWidth is the room left for the detail pane beside the navigator.
func (m model) detailWidth() int { return m.width - m.paneLeft() }

// showDetail reports whether the terminal is wide enough to carry a detail
// pane beside the navigator.
func (m model) showDetail() bool { return m.detailWidth() >= paneMin }

// paneLines renders the pane beside the navigator: the shell it should be
// showing, or, when there is none, what is known about the selected row.
func (m model) paneLines(width, rows int) []string {
	if m.resume != nil {
		return m.resumeLines(width, rows)
	}
	t := m.paneTerm()
	if t == nil {
		return m.detailLines(width, rows)
	}

	if s := m.scroll; s != nil && s.doc != nil && s.pid == t.pid {
		return scrollWindow(s, width, rows)
	}

	// A row that folded a run into one line has more to say than its shell's
	// screen: what the run is, its ports, what its agent is doing. The pane
	// splits — those facts in a banner across the top, the live screen under
	// them — so standing on the row shows both. The keys do not change with
	// the look: the screen below is still a preview.
	banner := m.runBanner(width, rows)
	var screen []string
	if len(banner) > 0 {
		screen = screenTail(t, rows-len(banner))
	} else {
		screen = t.lines(rows)
	}

	// A screen can arrive wider than this pane: the shell is sized by the
	// windows watching it, and this window may not be one of them. A row
	// wider than the pane would wrap and take the layout with it, so each is
	// cut to fit; rows at the pane's width — the usual case — pass whole.
	for i, row := range screen {
		screen[i] = ansi.Truncate(row, width, "")
	}

	// A preview wears the quiet gray in place of the program's own colors,
	// the way an unfocused window is grayed everywhere else: a glance at the
	// pane answers whether the keys are going there. The banner keeps its
	// colors either way — it is scrn's report about the row, not the screen.
	if m.focused() != t {
		for i, row := range screen {
			screen[i] = previewStyle.Render(ansi.Strip(row))
		}
		return append(banner, screen...)
	}

	lines := append(banner, screen...)
	// The cursor is only drawn where the keystrokes are going. On an unfocused
	// shell it would say the typing lands there, which it does not.
	if t.curY >= 0 && t.curY < len(lines) {
		lines[t.curY] = withCursor(lines[t.curY], t.curX, width)
	}
	return lines
}

// resumeLines is the picker: a place's suspended conversations, newest
// first. Each row is when the conversation last moved, the branch it was on,
// and the last thing asked of it — the things a reader recognizes one by.
// The cursor's row is lit the way the navigator's is.
func (m model) resumeLines(width, rows int) []string {
	v := m.resume
	lines := []string{
		paneGutter + headingStyle.Render(v.place.Name),
		paneGutter + noteStyle.Render("suspended conversations"),
		"",
	}
	switch {
	case !v.loaded:
		return append(lines, paneGutter+noteStyle.Render("looking…"))
	case len(v.convos) == 0:
		return append(lines, paneGutter+noteStyle.Render("none to continue"))
	}
	list := v.matches()
	if len(list) == 0 {
		return append(lines, paneGutter+noteStyle.Render("nothing answers "+strings.TrimSpace(v.query)))
	}

	// The columns are sized to this listing: the age is short by construction,
	// and a branch keeps enough to be told apart without owning the row.
	agew, bw := 0, 0
	for _, c := range list {
		agew = max(agew, lipgloss.Width(shortAge(c.When)))
		bw = max(bw, lipgloss.Width(c.Branch))
	}
	bw = min(bw, 12)

	// The window slides the least amount that keeps the cursor on screen,
	// derived from the cursor alone so drawing moves nothing.
	sel := min(v.cursor, len(list)-1)
	detail := resumeDetail(list[sel], width, rows)
	body := max(rows-len(lines)-len(detail), 1)
	off := max(sel-body+1, 0)

	lead := 3 + agew + 2
	if bw > 0 {
		lead += bw + 2
	}
	for i := off; i < min(off+body, len(list)); i++ {
		c := list[i]
		marker, style := " ", itemStyle
		if i == sel {
			marker, style = glyphSelected, selStyle
		}
		row := " " + marker + " " + faintStyle.Render(pad(shortAge(c.When), agew)) + "  "
		if bw > 0 {
			row += faintStyle.Render(pad(truncate(c.Branch, bw), bw)) + "  "
		}
		// The prompt is the recognizer; a conversation that never got one is
		// named by what it said it was doing, or failing that by its id.
		text := c.Prompt
		if text == "" {
			text = c.Summary
		}
		if text == "" {
			text = c.ID
		}
		seg := style.Render(truncateTail(text, width-lead))
		if q := strings.TrimSpace(v.query); q != "" {
			seg = truncateStyled(highlight(text, matchSpans(q, text), style), width-lead, false)
		}
		lines = append(lines, row+seg)
	}
	for len(lines) < rows-len(detail) {
		lines = append(lines, "")
	}
	return append(lines, detail...)
}

// resumeDetail is the selected conversation whole, under the list: the full
// prompt a row could only truncate, what the session said it was doing, and
// where and on what branch it was had. A short pane keeps the list instead —
// the names are the scanning surface, and the depth can wait for room.
func resumeDetail(c conversation, width, rows int) []string {
	if rows < 14 {
		return nil
	}
	var fs []field
	add := func(label, value string, t tone) {
		if value != "" {
			fs = append(fs, field{label: label, value: value, tone: t})
		}
	}
	add("asked", c.Prompt, tonePlain)
	add("said", c.Summary, toneQuiet)
	add("branch", c.Branch, toneAccent)
	add("where", c.Dir, toneQuiet)
	if len(fs) == 0 {
		return nil
	}
	out := []string{ruleStyle.Render(strings.Repeat("─", width))}
	return append(out, renderBlock(fs, width)...)
}

// runBanner is the detail summary drawn across the top of the pane when the
// row under the cursor stands for a shell scrn holds — a folded run or a
// bare shell alike: a screen dump with no name over it says what the shell
// is showing but not what it is, and every other row gets its facts. The
// banner takes at most a third of the pane: it is there to say what the row
// is, and the screen below to show what it is doing, and of the two it is
// the screen that is live. Focused, there is no banner — the pane is the
// shell then, whole.
func (m model) runBanner(width, rows int) []string {
	if m.focused() != nil {
		return nil
	}
	r, ok := m.selected()
	if !ok || r.kind != rowProc {
		return nil
	}
	if len(r.run) < 2 && m.terms[r.node.PID] == nil {
		return nil
	}
	room := rows / 3
	if room < 1 {
		return nil
	}
	lines := m.detailLines(width, room)
	return append(lines, ruleStyle.Render(strings.Repeat("─", width)))
}

// screenTail is the bottom of a shell's screen, trailing blank rows dropped.
// Under a banner the pane is shorter than the shell was sized for, so some of
// the screen has to go; what goes is blank padding first and the oldest rows
// second, because the bottom of a screen — the prompt, the last thing said —
// is the part a glance is after.
func screenTail(t *remoteTerm, rows int) []string {
	if rows <= 0 {
		return nil
	}
	lines := strings.Split(t.screen, "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	return lines
}

// scrollWindow is the transcript as the reader has it: the rows that fit the
// pane, stopping above lines short of the live tail. No cursor is drawn —
// typing lands nowhere while reading — and rows older than a narrowing
// resize can be wider than the pane, so each is cut to it. A selection
// wears reverse video, plain: marked lines are on their way to the
// clipboard, and the clipboard gets text, not styling.
func scrollWindow(s *scrollView, width, rows int) []string {
	top := max(len(s.doc)-rows-s.above, 0)
	end := min(top+rows, len(s.doc))
	lo, hi := s.cur, s.cur
	if s.anchor >= 0 {
		lo, hi = s.selection(s.cur)
	}
	out := make([]string, 0, rows)
	for i, row := range s.doc[top:end] {
		line := ansi.Truncate(row, width, "")
		if at := top + i; at >= lo && at <= hi {
			line = cursorStyle.Render(ansi.Strip(line))
		}
		out = append(out, line)
	}
	return out
}

// withCursor marks the cell the shell's cursor is on. The line already carries
// the shell's own styling, so it is cut around the cell rather than indexed
// into: a byte offset would land in the middle of an escape sequence. The cut
// leans on the protocol's shape — every row arrives as wide as the pane — so
// column x is always a cell the row actually has.
func withCursor(line string, x, width int) string {
	if x < 0 || x >= width {
		return line
	}
	under := ansi.Cut(line, x, x+1)
	if strings.TrimSpace(under) == "" {
		under = " "
	}
	return ansi.Cut(line, 0, x) + cursorStyle.Render(ansi.Strip(under)) + ansi.Cut(line, x+1, width)
}

// detailLines renders everything known about the selected row.
func (m model) detailLines(width, rows int) []string {
	r, ok := m.selected()
	if !ok {
		return []string{paneGutter + noteStyle.Render("nothing selected")}
	}

	fields, loaded := m.details[detailKey(r)]
	if !loaded {
		return []string{paneGutter + noteStyle.Render("loading…")}
	}

	var lines []string
	for _, block := range blocks(fields) {
		drawn := renderBlock(block, width)
		if len(drawn) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, drawn...)
	}
	if len(lines) > rows {
		lines = lines[:rows]
	}
	return lines
}

// blocks splits the fields at the breaks between groups. A group sets its own
// value column, so one long label does not indent a pane that has nothing else
// like it in it.
func blocks(fields []field) [][]field {
	var out [][]field
	var cur []field
	for _, f := range fields {
		if f.kind == gapField {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, f)
	}
	return append(out, cur)
}

// renderBlock draws one group, preceded by the blank line that separates it
// from the last. A group with nothing in it draws nothing at all, so a pane
// that skipped a whole group does not leave a hole where it would have been.
func renderBlock(block []field, width int) []string {
	if len(block) == 0 {
		return nil
	}

	// The widest label in this group sets its value column, so values line up.
	labelW := 0
	for _, f := range block {
		if f.kind == pairField {
			if w := lipgloss.Width(f.label); w > labelW {
				labelW = w
			}
		}
	}

	var lines []string
	for _, f := range block {
		switch f.kind {
		case headingField:
			lines = append(lines, paneGutter+headingStyle.Render(f.value))
		case noteField:
			for _, c := range wrapValue(f.value, width-len(paneGutter)-1) {
				lines = append(lines, paneGutter+noteStyle.Render(c))
			}
		default:
			lines = append(lines, wrapField(f, labelW, width)...)
		}
	}
	return lines
}

// wrapField draws one label and its value, wrapping a long value under the
// value column rather than letting it run off the pane.
func wrapField(f field, labelW, width int) []string {
	label := pad(labelStyle.Render(f.label), labelW)
	gutter := paneGutter
	valueW := max(width-labelW-2*len(gutter), 8)

	chunks := wrapValue(f.value, valueW)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	style := toneStyles[f.tone]
	lines := make([]string, 0, len(chunks))
	for i, c := range chunks {
		if i == 0 {
			lines = append(lines, gutter+label+gutter+style.Render(c))
			continue
		}
		lines = append(lines, gutter+strings.Repeat(" ", labelW)+gutter+style.Render(c))
	}
	return lines
}

// wrapValue breaks a value at spaces where it can, and mid-token when a single
// token is longer than the pane — paths and command lines usually are.
//
// Everything is measured in the columns a terminal will give it, the way the
// rest of this file measures. Counting bytes instead wraps a line of accented
// text a third of the way early, and cutting at a byte offset lands inside a
// character, leaving half of it on each of two lines where it draws as neither
// — which the ellipsis on a truncated prompt and the › between the processes
// of a run are both enough to trigger.
func wrapValue(s string, width int) []string {
	if width <= 0 {
		return nil
	}
	var lines []string
	for word := range strings.FieldsSeq(s) {
		switch {
		case len(lines) == 0:
			lines = append(lines, word)
		case lipgloss.Width(lines[len(lines)-1])+1+lipgloss.Width(word) <= width:
			lines[len(lines)-1] += " " + word
		default:
			lines = append(lines, word)
		}
		// Split anything still too wide for the pane.
		for lipgloss.Width(lines[len(lines)-1]) > width {
			head, tail := cutColumns(lines[len(lines)-1], width)
			lines[len(lines)-1] = head
			if tail == "" {
				break // a single character wider than the whole pane
			}
			lines = append(lines, tail)
		}
	}
	return lines
}

// cutColumns splits s after the last character that still fits in width
// columns. A character too wide for the pane on its own is kept whole and
// overflows, because the alternative is to cut it into bytes that are not a
// character at all — and returning it uncut is what lets the caller stop
// rather than ask again for the same string.
func cutColumns(s string, width int) (head, tail string) {
	col := 0
	for i, r := range s {
		w := lipgloss.Width(string(r))
		if i > 0 && col+w > width {
			return s[:i], s[i:]
		}
		col += w
	}
	return s, ""
}

// at returns the line at i, or blank past the end.
func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

// pad right-fills a rendered line to width columns, measuring display width so
// styling does not count toward it.
func pad(s string, width int) string {
	if gap := width - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// truncate shortens s to width columns, marking the cut with an ellipsis.
// Columns, not runes — a name in wide characters is as wide as the terminal
// will draw it, which is the measure everything else in this file uses.
//
// A qualified name like "w0zro/archive/scrn" is cut from the left, because the
// repo name at the end is the part that identifies it; the parent directories
// are only there to break a tie. An unqualified name is cut from the right.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	total := lipgloss.Width(s)
	if total <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	if strings.Contains(s, "/") {
		// The widest tail that still fits beside the ellipsis.
		col := 0
		for i, r := range s {
			if total-col <= width-1 {
				return "…" + s[i:]
			}
			col += lipgloss.Width(string(r))
		}
		return "…"
	}
	head, _ := cutColumns(s, width-1)
	return head + "…"
}

// truncateTail cuts from the right, keeping the start. A command line says
// what it is first and how it was run afterwards, so the front is the part
// worth keeping — the opposite of a repository's name.
func truncateTail(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	head, _ := cutColumns(s, width-1)
	return head + "…"
}

// wrapText breaks a message across at most rows navigator lines, measured in
// columns like everything else here.
func wrapText(s string, width, rows int, style lipgloss.Style) []string {
	if width <= 0 || rows <= 0 {
		return nil
	}
	var lines []string
	for rest := s; rest != "" && len(lines) < rows; {
		var head string
		head, rest = cutColumns(rest, width)
		lines = append(lines, " "+style.Render(head))
	}
	return lines
}

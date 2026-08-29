package main

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#5A3FD9", Dark: "#B9A7FF"})

	hintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#8A8A8A", Dark: "#6C6C6C"})

	ruleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#D8DEE4", Dark: "#30363D"})

	itemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#1F2328", Dark: "#E6E6E6"})

	selStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#0550AE", Dark: "#79C0FF"})

	faintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#98A0A8", Dark: "#5C6570"})

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#6A737D", Dark: "#8B949E"})

	warnStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#D29922"})

	errStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#F85149"})

	busyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"})

	cursorStyle = lipgloss.NewStyle().Reverse(true)

	// headingStyle names what the detail pane is about, so that what a row is
	// does not read at the same weight as its memory share.
	headingStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#1F2328", Dark: "#E6E6E6"})

	// offSelStyle marks the selected row when that row is one scrn cannot step
	// into: bold enough to find, dim enough to still read as unavailable.
	offSelStyle = faintStyle.Bold(true)
)

// claudeMark is the glyph beside a Claude instance. A working one turns, so
// that a glance tells you which instances are thinking and which are waiting
// on you — the difference a still marker leaves you to guess at, and the one
// worth crossing the room for.
func (m model) claudeMark(status string) (string, lipgloss.Style) {
	if status == busyStatus {
		return spinFrames[m.frame%len(spinFrames)], busyStyle
	}
	return "○", faintStyle
}

// View lays the window out as two full-height columns.
//
// scrn's own name and keys sit at the top and bottom of the left one rather
// than spanning the window, so the pane on the right is the attached process
// and nothing else. A terminal made to give up its first and last rows to a
// header and a footer is a terminal drawing something other than what it was
// told it had room for.
func (m model) View() string {
	return m.windowRequests() + m.layout()
}

// windowRequests passes on what the attached process asked of the terminal
// window. A program in the pane addresses these to the terminal it believes it
// is in, which is scrn; scrn is inside a real one, and it is the only thing
// that can hand them on.
//
// They ride out with the frame rather than being written straight to the
// terminal, because the renderer holds the output while it draws and a write
// from anywhere else would land in the middle of one. Neither sequence moves
// the cursor or changes a colour, so carrying them along costs the frame
// nothing, and repeating them every frame is how a terminal expects to be told.
func (m model) windowRequests() string {
	t := m.focused()
	if t == nil || t.progress == "" {
		// Nothing attached, or nothing running: say so once the shell that was
		// reporting progress is no longer the one being watched.
		return "\x1b]9;4;0;\x07"
	}
	// The payload the emulator hands over already carries its own command
	// number, so it goes out as it came in.
	return "\x1b]" + t.progress + "\x07"
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
	if !m.showDetail() {
		return strings.Join(padTo(left, rows), "\n")
	}

	right := m.paneLines(m.detailWidth(), rows)
	divider := ruleStyle.Render("│")

	lines := make([]string, 0, rows)
	for i := 0; i < rows; i++ {
		lines = append(lines, pad(at(left, i), navWidth)+divider+at(right, i))
	}
	return strings.Join(lines, "\n")
}

// leftColumn is scrn's own column: its name, the navigator, and its keys held
// down at the bottom.
func (m model) leftColumn(rows int) []string {
	hint := m.hintLines(m.hintWidth(), rows)
	// Whatever is being said, the list keeps a row.
	if max := rows - 2; max > 0 && len(hint) > max {
		hint = hint[:max]
	}
	body := rows - 1 - len(hint)
	if body < 0 {
		body = 0
	}

	lines := make([]string, 0, rows)
	lines = append(lines, titleStyle.Render("scrn"))

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

// hintLines draws scrn's own keys at the foot of its column. It is a block
// rather than a line because that column is narrow, and knowing the keys is
// worth more than the horizontal room it would take to list them across.
//
// A pending confirmation or the report of the last action takes the whole
// block: while either is on screen it is the only thing the next keystroke
// is about.
func (m model) hintLines(width, rows int) []string {
	switch {
	case m.pendingReplace:
		return append(
			hintBlock("replace the daemon, ending "+
				plural(len(m.terms), "shell", "shells")+"?", width, warnStyle),
			hintBlock("R confirm · any other key cancels", width, hintStyle)...)

	case m.pendingKill != nil:
		return append(
			hintBlock("kill "+m.pendingKill.subject+"?", width, warnStyle),
			hintBlock("x confirm · any other key cancels", width, hintStyle)...)

	case m.typing:
		return append(
			hintBlock("/"+m.filter+"█", width, itemStyle),
			hintBlock("enter keep · esc clear", width, hintStyle)...)

	case m.focused() != nil:
		return append(
			hintBlock("shell", width, warnStyle),
			hintBlock("ctrl+o back to the list", width, hintStyle)...)

	case m.status != "":
		style := itemStyle
		if m.statusErr {
			style = errStyle
		}
		return hintBlock(m.status, width, style)

	case m.filter != "":
		return append(
			hintBlock("filter "+m.filter, width, selStyle),
			hintBlock("n shell · c claude · / edit · esc clear", width, hintStyle)...)
	}
	if !m.showHelp {
		// One line to say the keys exist. The list of them is worth less to
		// the reader, most of the time, than the rows it would cover up.
		return []string{" " + hintStyle.Render("? keys")}
	}
	return m.keyLines(width, rows)
}

// keyLines is the standing list of keys, in two columns.
func (m model) keyLines(width, rows int) []string {
	all := "a all"
	if m.showAll {
		all = "a running"
	}
	folds := "- unfold"
	if m.unfolded {
		folds = "- fold"
	}

	pairs := [][2]string{
		{"↑↓ move", "gg top"},
		{"G bottom", "/ find"},
		{"n shell", "c claude"},
		{"enter open", "space fold"},
		{folds, all},
		{"x kill", "X kill tree"},
		{"q quit", ""},
	}

	// The keys must not crowd out the list they are about, so a short window
	// gets the first of them rather than all of them. They are ordered so that
	// what goes first is what is least missed.
	if max := rows / 3; max < len(pairs) {
		if max < 1 {
			max = 1
		}
		pairs = pairs[:max]
	}

	col := (width - 1) / 2
	lines := make([]string, 0, len(pairs))
	for _, p := range pairs {
		lines = append(lines, " "+pad(hintStyle.Render(p[0]), col)+hintStyle.Render(p[1]))
	}
	return lines
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
		return []string{" " + faintStyle.Render("no repositories")}
	case len(m.rows) == 0 && m.filter != "":
		return []string{" " + faintStyle.Render("no project matches")}
	case len(m.rows) == 0:
		return []string{
			" " + faintStyle.Render("nothing running"),
			"",
			" " + faintStyle.Render("a  show all"),
			" " + faintStyle.Render("/  find a project"),
		}
	}

	end := m.offset + rows
	if end > len(m.rows) {
		end = len(m.rows)
	}

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
		marker = "▸"
	}
	style := m.rowStyle(r, selected)

	fold := ""
	if m.collapsed[detailKey(r)] {
		if n := m.childCount(r); n > 0 {
			fold = " +" + strconv.Itoa(n)
		}
	}

	// A busy Claude instance is marked in the gutter beside its name, so the
	// repositories with work happening in them read at a glance.
	mark, markStyle := "", faintStyle
	if s := m.claudeFor(r); s != nil {
		glyph, style := m.claudeMark(s.Status)
		mark, markStyle = " "+glyph, style
	}

	spinner := ""
	if r.kind == rowProc {
		if _, dying := m.dying[r.node.PID]; dying {
			spinner = " " + spinFrames[m.frame%len(spinFrames)]
		}
	}

	rules := ""
	label := r.project.Name
	if r.kind == rowProc {
		branch := "├─"
		if r.last {
			branch = "└─"
		}
		rules = r.prefix + branch + " "
		label = procLabel(r.node)
	}

	room := navWidth - 2 - lipgloss.Width(rules) - lipgloss.Width(fold) -
		lipgloss.Width(spinner) - lipgloss.Width(mark)
	return marker + faintStyle.Render(rules) + style.Render(truncate(label, room)) +
		markStyle.Render(mark) + errStyle.Render(spinner) + faintStyle.Render(fold)
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

// detailWidth is the room left for the detail pane beside the navigator.
func (m model) detailWidth() int { return m.width - navWidth - 1 }

// showDetail reports whether the terminal is wide enough to carry a detail
// pane beside the navigator.
func (m model) showDetail() bool { return m.width >= navMin }

// paneLines renders the pane beside the navigator: the shell it should be
// showing, or, when there is none, what is known about the selected row.
func (m model) paneLines(width, rows int) []string {
	t := m.paneTerm()
	if t == nil {
		return m.detailLines(width, rows)
	}

	lines := t.lines(rows)

	// The cursor is only drawn where the keystrokes are going. On an unfocused
	// shell it would say the typing lands there, which it does not.
	if m.focused() == t && t.curY >= 0 && t.curY < len(lines) {
		lines[t.curY] = withCursor(lines[t.curY], t.curX, width)
	}
	return lines
}

// withCursor marks the cell the shell's cursor is on. The line already carries
// the shell's own styling, so it is cut around the cell rather than indexed
// into: a byte offset would land in the middle of an escape sequence.
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
		return []string{" " + faintStyle.Render("nothing selected")}
	}

	fields, loaded := m.details[detailKey(r)]
	if !loaded {
		return []string{" " + faintStyle.Render("loading…")}
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
		if f.kind == pairField && len(f.label) > labelW {
			labelW = len(f.label)
		}
	}

	var lines []string
	for _, f := range block {
		switch f.kind {
		case headingField:
			lines = append(lines, " "+headingStyle.Render(f.value))
		case noteField:
			for _, c := range wrapValue(f.value, width-2) {
				lines = append(lines, " "+faintStyle.Render(c))
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
	gutter := " "
	valueW := width - labelW - 2*len(gutter)
	if valueW < 8 {
		valueW = 8
	}

	chunks := wrapValue(f.value, valueW)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	lines := make([]string, 0, len(chunks))
	for i, c := range chunks {
		if i == 0 {
			lines = append(lines, gutter+label+gutter+itemStyle.Render(c))
			continue
		}
		lines = append(lines, gutter+strings.Repeat(" ", labelW)+gutter+itemStyle.Render(c))
	}
	return lines
}

// wrapValue breaks a value at spaces where it can, and mid-token when a single
// token is longer than the pane — paths and command lines usually are.
func wrapValue(s string, width int) []string {
	if width <= 0 {
		return nil
	}
	var lines []string
	for _, word := range strings.Fields(s) {
		switch {
		case len(lines) == 0:
			lines = append(lines, word)
		case len(lines[len(lines)-1])+1+len(word) <= width:
			lines[len(lines)-1] += " " + word
		default:
			lines = append(lines, word)
		}
		// Split anything still too wide for the pane.
		for len(lines[len(lines)-1]) > width {
			cur := lines[len(lines)-1]
			lines[len(lines)-1] = cur[:width]
			lines = append(lines, cur[width:])
		}
	}
	return lines
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
//
// A qualified name like "w0zro/archive/scrn" is cut from the left, because the
// repo name at the end is the part that identifies it; the parent directories
// are only there to break a tie. An unqualified name is cut from the right.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	if strings.Contains(s, "/") {
		return "…" + string(r[len(r)-(width-1):])
	}
	return string(r[:width-1]) + "…"
}

// wrapText breaks a message across at most rows navigator lines.
func wrapText(s string, width, rows int, style lipgloss.Style) []string {
	if width <= 0 || rows <= 0 {
		return nil
	}
	var lines []string
	for r := []rune(s); len(r) > 0 && len(lines) < rows; r = r[min(width, len(r)):] {
		lines = append(lines, " "+style.Render(string(r[:min(width, len(r))])))
	}
	return lines
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

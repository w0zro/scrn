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

	// offSelStyle marks the selected row when that row is one scrn cannot step
	// into: bold enough to find, dim enough to still read as unavailable.
	offSelStyle = faintStyle.Bold(true)
)

// claudeMark is the glyph beside a Claude instance: filled while it is
// working, hollow while it is waiting on its user.
func claudeMark(status string) string {
	if status == "busy" {
		return "●"
	}
	return "○"
}

func claudeMarkStyle(status string) lipgloss.Style {
	if status == "busy" {
		return busyStyle
	}
	return faintStyle
}

func (m model) View() string {
	header := titleStyle.Render("scrn")
	hint := m.renderHint()
	return header + "\n" + m.renderBody(m.bodyHeight()) + hint
}

// renderHint draws the footer. A pending confirmation or the report of the
// last action takes the whole line: while either is on screen it is the only
// thing the next keystroke is about.
func (m model) renderHint() string {
	if req := m.pendingKill; req != nil {
		return warnStyle.Render("kill "+req.subject+"?") +
			hintStyle.Render("   x confirm  ·  any other key cancels")
	}
	if m.status != "" {
		if m.statusErr {
			return errStyle.Render(m.status)
		}
		return itemStyle.Render(m.status)
	}

	// A focused shell has its own vocabulary: every other key is the shell's,
	// so listing scrn's would be a lie about what they do.
	if m.focused() != nil {
		return warnStyle.Render("shell") +
			hintStyle.Render("  ·  ctrl+o back to the list")
	}

	all := "a all"
	if m.showAll {
		all = "a running"
	}
	// The keys matter more than the words for them, so a narrow window loses
	// the wording rather than the last few bindings off the end.
	full := "↑↓ move · n shell · c claude · enter open · space collapse · x kill · X kill tree · " + all + " · q quit"
	if lipgloss.Width(full) <= m.width {
		return hintStyle.Render(full)
	}
	short := "↑↓ move · n shell · c claude · enter open · space fold · x kill · X tree · " + all + " · q quit"
	return hintStyle.Render(truncate(short, m.width))
}

// renderBody draws the navigator beside the detail pane, each row ending in a
// newline so the footer starts on its own line.
func (m model) renderBody(rows int) string {
	nav := m.navLines(rows)
	if !m.showDetail() {
		return joinRows(nav, rows)
	}

	detail := m.paneLines(m.detailWidth(), rows)
	divider := ruleStyle.Render("│")

	var b strings.Builder
	for i := 0; i < rows; i++ {
		b.WriteString(pad(at(nav, i), navWidth))
		b.WriteString(divider)
		b.WriteString(at(detail, i))
		b.WriteString("\n")
	}
	return b.String()
}

func joinRows(lines []string, rows int) string {
	var b strings.Builder
	for i := 0; i < rows; i++ {
		b.WriteString(at(lines, i))
		b.WriteString("\n")
	}
	return b.String()
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
	case len(m.rows) == 0:
		return []string{
			" " + faintStyle.Render("nothing running"),
			"",
			" " + faintStyle.Render("a  show all"),
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
		mark, markStyle = " "+claudeMark(s.Status), claudeMarkStyle(s.Status)
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

	// The widest label sets the value column, so values line up.
	labelW := 0
	for _, f := range fields {
		if n := len(f.label); n > labelW {
			labelW = n
		}
	}

	var lines []string
	for _, f := range fields {
		lines = append(lines, wrapField(f, labelW, width)...)
	}
	if len(lines) > rows {
		lines = lines[:rows]
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

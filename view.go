package main

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
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
)

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

	all := "a all"
	if m.showAll {
		all = "a running"
	}
	return hintStyle.Render("↑↓ move  ·  space collapse  ·  x kill  ·  X kill tree  ·  " + all + "  ·  q quit")
}

// renderBody draws the navigator beside the detail pane, each row ending in a
// newline so the footer starts on its own line.
func (m model) renderBody(rows int) string {
	nav := m.navLines(rows)
	if !m.showDetail() {
		return joinRows(nav, rows)
	}

	detail := m.detailLines(m.detailWidth(), rows)
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
	marker, style := " ", itemStyle
	if selected {
		marker, style = "▸", selStyle
	}

	fold := ""
	if m.collapsed[detailKey(r)] {
		if n := m.childCount(r); n > 0 {
			fold = " +" + strconv.Itoa(n)
		}
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

	room := navWidth - 2 - lipgloss.Width(rules) - lipgloss.Width(fold) - lipgloss.Width(spinner)
	return marker + faintStyle.Render(rules) + style.Render(truncate(label, room)) +
		errStyle.Render(spinner) + faintStyle.Render(fold)
}

// detailWidth is the room left for the detail pane beside the navigator.
func (m model) detailWidth() int { return m.width - navWidth - 1 }

// showDetail reports whether the terminal is wide enough to carry a detail
// pane beside the navigator.
func (m model) showDetail() bool { return m.width >= navMin }

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

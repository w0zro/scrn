package main

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// How scrn looks, in one place: the palette names every color by its job, the
// glyphs are the marks drawn beside and between the words, and the styles the
// rest of the code reads are derived from them. An appearance change is a
// change here; the render code asks for roles, not colors.
//
// scrn carries no colors of its own. It draws with the terminal's sixteen
// slots, so the list wears whatever the terminal wears — datum, in light or
// dark — and the claude in the pane beside it, the tmux around both and the
// list are one palette by construction. Hue is spent on state: amber for
// working, green for finished and waiting on you, the orange-red for blocked
// and for dying — and on one thing more: where you are, in the slot the
// terminal's own cursor wears, so scrn's cursor and the terminal's are the
// same color. When nothing needs you, the list is ink and the cursor.

// The slots, by the job each does here. The terminal's theme says what
// color a slot is; scrn says what it means.
const (
	slotRed    = "1"  // blocked on an ask, dying, failed, destructive
	slotGreen  = "2"  // alive and well; finished and waiting on you
	slotSelf   = "4"  // scrn itself, where it shows up as a process
	slotCursor = "5"  // here: the cursor, and scrn's own name — the terminal's cursor color
	slotCyan   = "6"  // found: the letters a query matched
	slotGray   = "8"  // quiet: idle rows, labels, scrn's own asides
	slotAmber  = "9"  // working, and an answer owed
	slotPlace  = "12" // a place — a group, a repository, a sub-project: the frequent names, in a pastel
)

// The styles are package-wide because everything drawing reads them.
var (
	titleStyle, hintStyle, ruleStyle, itemStyle, selStyle lipgloss.Style
	faintStyle, labelStyle, errStyle, busyStyle           lipgloss.Style
	attnStyle, blockedStyle, headingStyle                 lipgloss.Style
	offSelStyle, noteStyle, matchStyle, selfStyle         lipgloss.Style
	placeStyle                                            lipgloss.Style
)

func init() { applyStyles() }

// applyStyles builds every style from the slots.
func applyStyles() {
	slot := func(s string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(s)) }
	ink := lipgloss.NewStyle()

	// The masthead and the cursor row are the two bold things on screen,
	// and the two in the cursor's color: scrn's name, and where you are.
	titleStyle = slot(slotCursor).Bold(true)
	selStyle = slot(slotCursor).Bold(true)
	// selfStyle tags a process that is scrn itself, in a color nothing
	// else wears: not the cursor's, not a state's.
	selfStyle = slot(slotSelf)

	// placeStyle names the places — the groups, repositories and
	// sub-projects the processes sit under — in the pastel datum keeps for
	// the names that are everywhere: they structure the list without
	// competing with the states.
	placeStyle = slot(slotPlace)
	itemStyle = ink
	headingStyle = ink.Bold(true)

	// One quiet gray, worn by content that is idle (faint), by the names of
	// facts (label) and by scrn's own asides (hint) alike; asides in italic,
	// the way an editor's aside is set apart from the text.
	faintStyle = slot(slotGray)
	labelStyle = slot(slotGray)
	hintStyle = slot(slotGray)
	noteStyle = faintStyle.Italic(true)

	// The rules that separate, never speak: a hairline, one step off the
	// ground in whatever the ground is.
	ruleStyle = ink.Faint(true)

	// offSelStyle marks the selected row when that row is one scrn cannot
	// step into: bold enough to find, dim enough to still read as unavailable.
	offSelStyle = faintStyle.Bold(true)

	// The states. busyStyle turns beside work in progress in claude's own
	// amber; attnStyle marks an agent that is done and waiting on its user;
	// blockedStyle one stopped mid-turn on a specific ask, holding up work
	// already in flight.
	busyStyle = slot(slotAmber)
	attnStyle = slot(slotGreen).Bold(true)
	blockedStyle = slot(slotRed).Bold(true)
	errStyle = slot(slotRed)

	// matchStyle lights the letters a query matched, inside whatever style
	// the row is otherwise wearing: a narrowed list always shows why it
	// narrowed. Cyan is found, and nothing else.
	matchStyle = slot(slotCyan).Bold(true)

	toneStyles = map[tone]lipgloss.Style{
		tonePlain:  itemStyle,
		toneGood:   slot(slotGreen),
		toneAttn:   slot(slotAmber),
		toneUrgent: slot(slotRed),
		toneBad:    slot(slotRed),
		toneAccent: ink.Bold(true),
		toneQuiet:  faintStyle,
	}
}

// tone is how a value in the detail pane reads. Most facts are plain; the
// few that carry a state carry it in the same colors the navigator's marks
// wear, and the ones that are true but secondary recede.
type tone int

const (
	tonePlain  tone = iota // content, read at full weight
	toneGood               // alive and well: running, working
	toneAttn               // worth a glance: a dirty tree, a diverged branch
	toneUrgent             // holding up work: blocked on an ask
	toneBad                // wrong: a zombie, a failure
	toneAccent             // identity worth picking out: a branch, a port
	toneQuiet              // true but secondary: ids, urls, empty counts
)

// toneStyles is the color each tone reads in. Values stay unbolded whatever
// their tone: the pane is a page, and color is enough of a voice on a page.
var toneStyles map[tone]lipgloss.Style

// tmuxPalette is what tmux draws with: the status line, the borders, the
// popups. tmux takes hex, and is configured once for every terminal rather
// than asked which ground it found, so the config names the side and the
// values are datum's for it — the same colors the slots resolve to in a
// terminal wearing datum.
type tmuxPalette struct {
	bg1, bg2                string // one and two steps off the ground: the wash, the chip
	fg, gray                string
	green, amber, red, cyan string
}

var tmuxDark = tmuxPalette{
	bg1: "#1A1E24", bg2: "#2B2F35", fg: "#DBE0E8", gray: "#8F98A3",
	green: "#54DCAA", amber: "#F8BD5F", red: "#FE9864", cyan: "#6AE5EC",
}

var tmuxLight = tmuxPalette{
	bg1: "#E7ECF2", bg2: "#CED3D9", fg: "#292E35", gray: "#616A76",
	green: "#007553", amber: "#976700", red: "#A24500", cyan: "#0D7A7F",
}

// tp is the palette tmux draws with: dark unless the config says light.
var tp = tmuxDark

// applyTheme picks tmux's side from the config. Anything but "light" is
// dark, the more common terminal.
func applyTheme(theme string) {
	if theme == "light" {
		tp = tmuxLight
	} else {
		tp = tmuxDark
	}
}

// statusChip is a mode on the status line: the word, bold in its color on
// the chip's ground, and after it the rest of the line washed one step off
// the terminal's ground — one tone for every mode, the chip alone carrying
// the color. The word's # are doubled: tmux expands them otherwise.
func statusChip(color, word string) string {
	return "#[fg=" + color + ",bg=" + tp.bg2 + ",bold] " +
		strings.ReplaceAll(word, "#", "##") +
		" #[fg=default,bg=" + tp.bg1 + ",fill=" + tp.bg1 + "]"
}

// tmuxStyled wraps text for tmux's status line: its color and weight in
// tmux's own style syntax, reset after. A # is tmux's to expand in a
// format, so the text's are doubled.
func tmuxStyled(fg string, bold bool, text string) string {
	style := "#[fg=" + fg
	if bold {
		style += ",bold"
	}
	return style + "]" + strings.ReplaceAll(text, "#", "##") + "#[default]"
}

// The glyphs. A filled mark is something lit — an answer waiting, a plan
// entry up — and a hollow one is the same thing quiet; the diamond is an ask.
const (
	glyphSelected = "▸"  // the cursor, in the navigator's gutter
	glyphIndent   = "  " // a child sits on indent alone: the tree's rules were the loudest thing on screen
	glyphDivider  = "│"  // between the navigator and its own pane; tmux's border stands where a shell's is
	glyphOn       = "●"
	glyphOff      = "○"
	glyphAsk      = "◆"
	glyphBusy     = "⋯" // the spinner, standing still: for a status line that is not redrawn per frame
	glyphJoin     = "›" // between the processes of a run
)

// paneGutter is the room the detail pane's prose keeps off the divider. The
// navigator's single-column gutter is part of the tree; the pane is a page,
// and a page gets a margin.
const paneGutter = "  "

// spinFrames is the turning marker beside work in progress: a process being
// killed, an agent mid-turn.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

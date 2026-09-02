package main

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// How scrn looks, in one place: the palette names every color by its job, the
// glyphs are the marks drawn beside and between the words, and the styles the
// rest of the code reads are derived from them. An appearance change is a
// change here; the render code asks for roles, not colors.

// palette is every color scrn draws with. The values are GitHub Primer's,
// plus a brand purple: a palette built for reading interfaces, in a light and
// a dark answer for each role.
type palette struct {
	brand  color.Color // scrn's own name
	fg     color.Color // what is being read
	muted  color.Color // quiet: tree rules, idle rows, scrn's own asides
	label  color.Color // the name of a fact, beside its value
	border color.Color // lines that separate, never speak
	accent color.Color // the selection

	success   color.Color // something alive and well
	attention color.Color // an answer owed, worth a glance
	urgent    color.Color // an answer holding up work, worth crossing the room
	danger    color.Color // dying, failed, destructive

	// The washes: three of the same answers, pale enough to paint a field
	// under text rather than speak. The mode bar rides on them.
	accentWash    color.Color
	successWash   color.Color
	attentionWash color.Color
}

// newPalette picks each role's color for the background the terminal
// reported.
func newPalette(dark bool) palette {
	pick := lipgloss.LightDark(dark)
	c := func(light, dark string) color.Color {
		return pick(lipgloss.Color(light), lipgloss.Color(dark))
	}
	return palette{
		brand:  c("#5A3FD9", "#B9A7FF"),
		fg:     c("#1F2328", "#E6E6E6"),
		muted:  c("#98A0A8", "#5C6570"),
		label:  c("#6A737D", "#8B949E"),
		border: c("#D8DEE4", "#30363D"),
		accent: c("#0550AE", "#79C0FF"),

		success:   c("#1A7F37", "#3FB950"),
		attention: c("#9A6700", "#D29922"),
		urgent:    c("#BF3989", "#F778BA"),
		danger:    c("#CF222E", "#F85149"),

		accentWash:    c("#DDF4FF", "#15294A"),
		successWash:   c("#DAFBE1", "#12351F"),
		attentionWash: c("#FFF8C5", "#372E12"),
	}
}

// The glyphs. A filled mark is something lit — an answer waiting, a plan
// entry up — and a hollow one is the same thing quiet; the diamond is an ask.
const (
	glyphSelected = "▸" // the cursor, in the navigator's gutter
	glyphBranch   = "├─"
	glyphLast     = "└─"
	glyphRail     = "│" // the tree's vertical, continuing past a branch
	glyphDivider  = "│" // between the navigator and its own pane; tmux's border stands where a shell's is
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

// The styles are package-wide because everything drawing reads them, and they
// are variables because they depend on a fact that arrives late: which
// background the terminal has. Lipgloss no longer guesses at it, so scrn asks
// (Init) and rebuilds on the answer (Update). Until it comes, dark — the more
// common terminal, and a wrong guess lasts one frame.
var (
	titleStyle, hintStyle, ruleStyle, itemStyle, selStyle  lipgloss.Style
	faintStyle, labelStyle, warnStyle, errStyle, busyStyle lipgloss.Style
	attnStyle, blockedStyle, headingStyle                  lipgloss.Style
	offSelStyle, noteStyle, matchStyle                     lipgloss.Style
)

func init() { applyBackground(true) }

// applyBackground rebuilds every style for the background the terminal
// reported.
func applyBackground(dark bool) {
	p := newPalette(dark)

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(p.brand)
	ruleStyle = lipgloss.NewStyle().Foreground(p.border)
	itemStyle = lipgloss.NewStyle().Foreground(p.fg)
	selStyle = lipgloss.NewStyle().Bold(true).Foreground(p.accent)
	labelStyle = lipgloss.NewStyle().Foreground(p.label)

	// One quiet gray, worn by content that is idle (faint) and by scrn's own
	// asides (hint) alike. They were two barely different grays, and read as
	// inconsistency rather than hierarchy.
	faintStyle = lipgloss.NewStyle().Foreground(p.muted)
	hintStyle = lipgloss.NewStyle().Foreground(p.muted)
	errStyle = lipgloss.NewStyle().Foreground(p.danger)
	busyStyle = lipgloss.NewStyle().Foreground(p.success)

	// warnStyle asks before something irreversible; attnStyle marks an agent
	// that is done and waiting on its user. Different moments, one color:
	// both are an answer owed, bright enough to catch from across the room.
	warnStyle = lipgloss.NewStyle().Bold(true).Foreground(p.attention)
	attnStyle = lipgloss.NewStyle().Bold(true).Foreground(p.attention)

	// blockedStyle marks an agent stopped mid-turn on a specific ask — a
	// permission prompt, a question. Brighter than the amber of done-and-
	// waiting, because this answer is holding up work already in flight.
	blockedStyle = lipgloss.NewStyle().Bold(true).Foreground(p.urgent)

	// headingStyle names what the detail pane is about, so that what a row is
	// does not read at the same weight as its memory share.
	headingStyle = itemStyle.Bold(true)

	// offSelStyle marks the selected row when that row is one scrn cannot
	// step into: bold enough to find, dim enough to still read as unavailable.
	offSelStyle = faintStyle.Bold(true)

	// matchStyle lights the letters a query matched, inside whatever style
	// the row is otherwise wearing: a narrowed list always shows why it
	// narrowed.
	matchStyle = lipgloss.NewStyle().Bold(true).Foreground(p.accent)

	// noteStyle is scrn speaking for itself — the path under a heading, an
	// empty list saying why it is empty. Italic sets the voice apart from
	// content the same way an editor's aside is set apart from the text.
	noteStyle = faintStyle.Italic(true)

	toneStyles = map[tone]lipgloss.Style{
		tonePlain:  itemStyle,
		toneGood:   lipgloss.NewStyle().Foreground(p.success),
		toneAttn:   lipgloss.NewStyle().Foreground(p.attention),
		toneUrgent: lipgloss.NewStyle().Foreground(p.urgent),
		toneBad:    lipgloss.NewStyle().Foreground(p.danger),
		toneAccent: lipgloss.NewStyle().Foreground(p.accent),
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

// toneStyles is the color each tone reads in, rebuilt with the rest of the
// styles. Values stay unbolded whatever their tone: the pane is a page, and
// color is enough of a voice on a page.
var toneStyles map[tone]lipgloss.Style

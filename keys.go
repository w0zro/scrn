package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The keys, spelled out. They are asked for with ? — at the navigator, or
// with the prefix from any shell — and answer in a tmux popup over the
// whole window, whichever pane the keys were in: the navigator's own pane
// is a column, and a page needs the width. The popup runs this build as
// `conn page`, which draws the page, waits for a keystroke and goes.

// keyList is every key, in the order a reader wants them: the navigator's
// first, then the chords.
var keyList = [][2]string{
	{"↑↓ j k", "move"},
	{"J K", "next · previous shell"},
	{"enter", "open"},
	{"tab", "the next waiting agent"},
	{"s", "shell"},
	{"a", "agent"},
	{"A", "continue a conversation"},
	{"r", "run"},
	{"x · X", "kill · kill the tree"},
	{"/", "find a project · a process"},
	{"esc", "clear the filter"},
	{"space · -", "fold · unfold all"},
	{".", "all · running"},
	{"gg · G", "top · bottom"},
	{"R", "end the server, shells and all"},
	{"q", "leave; the shells keep running"},
	{"^spc -", "here, from any shell"},
	{"^spc j k", "next · previous shell"},
	{"^spc ^spc", "between the list and the shell"},
	{"^spc enter", "the next waiting agent"},
	{"^spc s a r A", "shell · agent · run, here · continue"},
	{"^spc v", "read back; v marks, y copies"},
	{"^spc /", "find from anywhere"},
	{"^spc q", "leave from anywhere"},
	{"^spc R", "end the server from anywhere"},
	{"^spc ?", "this"},
}

// keysPage is the page: a blank row, the keys under each other with their
// meanings aligned, a blank row. tmux draws the border around it.
func keysPage() []string {
	var keyw, descw int
	for _, k := range keyList {
		keyw = max(keyw, lipgloss.Width(k[0]))
		descw = max(descw, lipgloss.Width(k[1]))
	}
	lines := []string{""}
	for _, k := range keyList {
		lines = append(lines, " "+pad(itemStyle.Render(k[0]), keyw)+"  "+pad(hintStyle.Render(k[1]), descw)+" ")
	}
	return append(lines, "")
}

// showKeys shows the page in a popup over the client: sized to the page,
// or to the client when the client is smaller — tmux refuses a popup it
// cannot fit rather than cutting it — titled, and closing when the page
// does. The client has to be named: a command from outside tmux has none,
// and a popup with no client has no size to fit. A chord names the client
// that pressed it; the navigator, given none, takes the one that spoke
// last. exe is this build, quoted for the shell tmux runs the page under.
func showKeys(run runner, exe, client string) error {
	if client == "" {
		out, err := run("list-clients", "-t", tmuxSession, "-F", "#{client_activity}\t#{client_name}")
		if err != nil {
			return err
		}
		latest := -1
		for line := range strings.SplitSeq(out, "\n") {
			when, name, ok := strings.Cut(line, "\t")
			if t, err := strconv.Atoi(when); ok && err == nil && t > latest {
				latest, client = t, name
			}
		}
		if client == "" {
			return errors.New("no client to show the keys on")
		}
	}

	page := keysPage()
	width := 0
	for _, l := range page {
		width = max(width, lipgloss.Width(l))
	}
	width, height := width+2, len(page)+2
	if out, err := run("display-message", "-p", "-c", client, "#{client_width} #{client_height}"); err == nil {
		if f := strings.Fields(out); len(f) == 2 {
			if cw, err := strconv.Atoi(f[0]); err == nil && cw > 0 {
				width = min(width, cw)
			}
			if ch, err := strconv.Atoi(f[1]); err == nil && ch > 0 {
				height = min(height, ch)
			}
		}
	}
	_, err := run("display-popup", "-E", "-c", client, "-T", " keys ",
		"-w", strconv.Itoa(width), "-h", strconv.Itoa(height),
		shellQuote(exe)+" page")
	return err
}

// keysModel is the page as a program: drawn once, gone on the first key.
type keysModel struct{}

func (keysModel) Init() tea.Cmd { return nil }

func (k keysModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg:
		return k, tea.Quit
	}
	return k, nil
}

func (keysModel) View() tea.View {
	v := tea.NewView(strings.Join(keysPage(), "\n"))
	v.AltScreen = true
	return v
}

// runKeys is `conn page`: the page, in the popup.
func runKeys() {
	if _, err := tea.NewProgram(keysModel{}).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "conn: %v\n", err)
		os.Exit(1)
	}
}

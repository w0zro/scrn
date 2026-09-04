package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// conn is a tmux client. The terminal attaches to conn's private server the
// way any tmux client does, and tmux draws the shells, holds the prefix and
// keeps the status line; conn is the program in the home window's left
// pane — the navigator — and the commands the prefix's chords run. The
// shell under the navigator's cursor is the pane on its right; the rest
// wait in windows of their own. What makes the server conn's rather than a
// stock tmux is this configuration, written at every launch and sourced
// into a server already running, so the bindings are always the build's.

// confPath is where the configuration is written: beside the socket, in
// the state directory.
func confPath() string {
	return filepath.Join(filepath.Dir(socketPath()), "tmux.conf")
}

// tmuxConf is the configuration for conn's server. conn is the path of this
// build, which the chords run; the path is quoted so a directory with a
// space in its name still finds it. navWidth is the navigator's column,
// which the layout holds through every resize.
func tmuxConf(conn string, scrollback, navWidth int) string {
	exe := shellQuote(conn)
	run := func(args string) string {
		return `run-shell "` + exe + ` ` + args + `"`
	}
	var b strings.Builder
	w := func(lines ...string) {
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}

	w("# Written by conn at every launch; edits do not survive one.",
		"",
		"# The keys. ctrl-space is the prefix, and each chord keeps its letter's",
		"# meaning: - is the navigator, ctrl-space again is back where the keys",
		"# were, j and k the next and previous shell, enter",
		"# the next agent waiting on you, s a r a shell, an agent, the plan where",
		"# the keys are, and , the next kind of agent for a to start. Every",
		"# chord tmux would otherwise bind is unbound first;",
		"# the root table is left as tmux has it, which is the mouse.",
		"set -g prefix C-Space",
		"unbind -a",
		"bind C-Space "+run("back"),
		"bind - "+run("home"),
		"bind j "+run("next"),
		"bind k "+run("prev"),
		"bind Enter "+run("jump"),
		"bind / "+run("home /"),
		"bind ? "+run("keys '#{client_name}'"),
		"bind s "+run("shell '#{pane_current_path}'"),
		"bind a "+run("agent '#{pane_current_path}'"),
		"bind A "+run("home A"),
		"bind , "+run("kind"),
		"bind r "+run("run '#{pane_current_path}'"),
		"bind v copy-mode",
		"bind q detach-client",
		`bind R confirm-before -p "end the server, and every shell it holds? (y/n)" kill-server`,
		"",
		"# The server. The transcript cap is the config's; windows follow the",
		"# client that last spoke; a program's copy reaches the clipboard; the",
		"# terminal's title is the pane the keys are in. The mouse is tmux's,",
		"# with tmux's own bindings: the wheel scrolls a pane's transcript, a",
		"# drag selects and copies, a click takes the keys to the pane under",
		"# it, and a program that speaks mouse gets its own events.",
		"set -g mouse on",
		"set -g history-limit "+strconv.Itoa(scrollback),
		"set -g window-size latest",
		"set -g set-clipboard on",
		"set -g mode-keys vi",
		"set -g automatic-rename off",
		"set -g allow-rename off",
		"set -g set-titles on",
		`set -g set-titles-string "#{?#{@conn_nav},conn,#{?#{@conn_title},#{@conn_title},#{pane_current_command}}}"`,
		"set -g escape-time 10",
		"set -g focus-events on",
		"set -g default-terminal tmux-256color",
		`set -as terminal-features ",*:RGB"`,
		// Claude Code caps itself at 256 colors wherever TMUX is set, whatever
		// TERM and COLORTERM say, and paints datum's near-black grounds as the
		// nearest cube color, a saturated teal. This variable is its own way
		// out of the cap; every pane inherits it.
		"set-environment -g CLAUDE_CODE_TMUX_TRUECOLOR 1",
		// What a program says to the terminal around tmux — the progress
		// Claude Code reports while it works, its notification when it is
		// done — reaches it only wrapped for passing through, and only
		// where the server allows the wrapping. Every pane is told the
		// outer terminal's name when it opens (outerTerminal), so a
		// program knows there is one to speak to.
		"set -g allow-passthrough on",
		"set -g display-time 3000",
		// The clock the status line reads to know whether a chord's note
		// is still current: T: expands an option's strftime specifiers,
		// and this option is the one specifier the format needs.
		`set -g `+nowOption+` "%s"`,
		"",
		"# The home window: the navigator down the left at its width, the shell",
		"# under its cursor filling the right. The layout is re-applied on every",
		"# resize, so the window's growth is the shell's.",
		"set -g main-pane-width "+strconv.Itoa(navWidth),
		`set-hook -g window-resized 'if -F "#{@conn_home}" "select-layout main-vertical"'`,
		`set -g pane-border-style "fg=`+tp.bg1+`"`,
		`set -g pane-active-border-style "fg=`+tp.bg1+`"`,
		"set -g pane-border-indicators off",
		"set -g popup-border-lines rounded",
		`set -g popup-border-style "fg=`+tp.bg2+`"`,
		"",
		"# The status line: conn's name, then the mode the keys are in — the",
		"# prefix while a chord hangs, copy mode, the navigator's own when it",
		"# has one to name, else which pane the keys are in — then what the",
		"# navigator says, or — for a few seconds, over it — what a chord said.",
		"# The right corner names the kind of agent a starts:",
		"# the one the window chose, else the config's. The window list tmux",
		"# would draw is turned off: the windows are where shells wait, and",
		"# the navigator is the list of them.",
		"set -g status on",
		"set -g status-position bottom",
		"set -g status-interval 1",
		"set -g status-justify left",
		"set -g status-left-length 400",
		`set -g status-style "bg=`+tp.bg1+`,fg=`+tp.gray+`"`,
		`set -g status-left "`+statusLeft()+`"`,
		`set -g status-right "`+statusRight()+`"`,
		`set -g window-status-separator ""`,
		`set -g window-status-format ""`,
		`set -g window-status-current-format ""`,
		`set -g message-style "bg=`+tp.bg1+`,fg=`+tp.amber+`,bold"`,
		`set -g message-command-style "bg=`+tp.bg1+`,fg=`+tp.amber+`"`,
		`set -g mode-style "bg=`+tp.bg2+`,fg=`+tp.fg+`"`,
	)
	return b.String()
}

// The status line's message slot is shared. The navigator writes msgOption
// and keeps it until its next key; a chord — a different process, done in
// a moment — writes noteOption with untilOption beside it, the second the
// note is stale, and the format shows the note over the message while the
// clock has not reached it. Nothing has to clear the note, and the
// navigator's message is back under it when it goes.
const (
	msgOption   = "@conn_msg"
	noteOption  = "@conn_note"
	untilOption = "@conn_until"
	nowOption   = "@conn_now" // holds %s, so #{T:@conn_now} is the time
)

// noteFor is how long a chord's note stands: display-time's three seconds
// and one more, since the line ticks once a second and a note set late in
// one loses most of it.
const noteFor = 4 * time.Second

// messageSlot is the status line's message slot: a chord's note while one is
// current, else what the navigator said.
func messageSlot() string {
	return "#{?#{e|<:#{T:" + nowOption + "},#{" + untilOption + "}},#{" + noteOption + "},#{" + msgOption + "}}"
}

// statusRight is the status line's corner: the kind of agent a starts,
// dim. The server's option when the window has chosen one — the format
// reads it live, so a choice shows the moment it is made — else the
// config's kind, which the launcher knows when it writes this.
func statusRight() string {
	return "#[fg=" + tp.gray + "]#{?" + agentOption + ",#{" + agentOption + "}," + defaultKind().name + "} "
}

// statusLeft is the status line's format: conn's name, the mode, then the
// message. The name is first and always there — the line is where conn
// says its own, now that the column is the list alone. tmux knows most of the modes itself — the prefix, copy mode, which pane
// the keys are in — and the navigator names its own in @conn_mode, which
// counts only while the keys are with it: a filter half-typed is not the
// mode of a shell. What the navigator has to say is in @conn_msg. Each
// mode is a chip in its color with the line washed one tone after it:
// amber for the prefix, green for a process, cyan for copy mode, and the
// navigator in ink — home is not a state. The message after the mode is
// the navigator's, or a chord's note over it while the note is fresh.
func statusLeft() string {
	// A chip stands inside a conditional, where a comma would split the
	// alternatives, so the styles' commas are escaped.
	chip := func(color, word string) string {
		return strings.ReplaceAll(statusChip(color, word), ",", "#,")
	}
	mode := "#{?client_prefix," + chip(tp.amber, "PREFIX") +
		",#{?pane_in_mode," + chip(tp.cyan, "COPY") +
		",#{?@conn_nav,#{?@conn_mode,#{@conn_mode}," + chip(tp.fg, "NAV") + "}," +
		chip(tp.green, "PROC") + "}}}"
	return brandChip() + mode + messageSlot()
}

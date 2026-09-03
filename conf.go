package main

import (
	"path/filepath"
	"strconv"
	"strings"
)

// scrn is a tmux client. The terminal attaches to scrn's private server the
// way any tmux client does, and tmux draws the shells, holds the prefix and
// keeps the status line; scrn is the program in the home window's left
// pane — the navigator — and the commands the prefix's chords run. The
// shell under the navigator's cursor is the pane on its right; the rest
// wait in windows of their own. What makes the server scrn's rather than a
// stock tmux is this configuration, written at every launch and sourced
// into a server already running, so the bindings are always the build's.

// confPath is where the configuration is written: beside the socket, in
// the state directory.
func confPath() string {
	return filepath.Join(filepath.Dir(socketPath()), "tmux.conf")
}

// tmuxConf is the configuration for scrn's server. scrn is the path of this
// build, which the chords run; the path is quoted so a directory with a
// space in its name still finds it. navWidth is the navigator's column,
// which the layout holds through every resize.
func tmuxConf(scrn string, scrollback, navWidth int) string {
	exe := "'" + strings.ReplaceAll(scrn, "'", `'\''`) + "'"
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

	w("# Written by scrn at every launch; edits do not survive one.",
		"",
		"# The keys. ctrl-space is the prefix, and each chord keeps its letter's",
		"# meaning: n is the navigator, j and k the next and previous shell, enter",
		"# the next agent waiting on you, s a r a shell, an agent, the plan where",
		"# the keys are. Every chord tmux would otherwise bind is unbound first;",
		"# the root table is left as tmux has it, which is the mouse.",
		"set -g prefix C-Space",
		"unbind -a",
		"bind C-Space last-pane",
		"bind n "+run("home"),
		"bind j "+run("next"),
		"bind k "+run("prev"),
		"bind Enter "+run("jump"),
		"bind / "+run("home /"),
		"bind ? "+run("keys '#{client_name}'"),
		"bind s "+run("shell '#{pane_current_path}'"),
		"bind a "+run("agent '#{pane_current_path}'"),
		"bind A "+run("home A"),
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
		`set -g set-titles-string "#{?#{@scrn_nav},scrn,#{?#{@scrn_tab},#{@scrn_tab},#{pane_current_command}}}"`,
		"set -g escape-time 10",
		"set -g focus-events on",
		"set -g default-terminal tmux-256color",
		`set -as terminal-features ",*:RGB"`,
		"set -g display-time 3000",
		"",
		"# The home window: the navigator down the left at its width, the shell",
		"# under its cursor filling the right. The layout is re-applied on every",
		"# resize, so the window's growth is the shell's.",
		"set -g main-pane-width "+strconv.Itoa(navWidth),
		`set-hook -g window-resized 'if -F "#{@scrn_home}" "select-layout main-vertical"'`,
		`set -g pane-border-style "fg=`+darkBorder+`"`,
		`set -g pane-active-border-style "fg=`+darkBorder+`"`,
		"set -g pane-border-indicators off",
		"set -g popup-border-lines rounded",
		`set -g popup-border-style "fg=`+darkBorder+`"`,
		"",
		"# The status line: the mode the keys are in — the prefix while a",
		"# chord hangs, copy mode, the navigator's own when it has one to",
		"# name, else which pane the keys are in — then what the navigator",
		"# says. The window list tmux would draw is turned off: the windows",
		"# are where shells wait, and the navigator is the list of them.",
		"set -g status on",
		"set -g status-position bottom",
		"set -g status-interval 1",
		"set -g status-justify left",
		"set -g status-left-length 400",
		`set -g status-style "bg=default,fg=`+darkLabel+`"`,
		`set -g status-left "`+statusLeft()+`"`,
		`set -g status-right ""`,
		`set -g window-status-separator ""`,
		`set -g window-status-format ""`,
		`set -g window-status-current-format ""`,
		`set -g message-style "bg=default,fg=`+darkAttention+`,bold"`,
		`set -g message-command-style "bg=default,fg=`+darkAttention+`"`,
		`set -g mode-style "bg=`+darkAccentWash+`,fg=`+darkFg+`"`,
	)
	return b.String()
}

// statusLeft is the status line's format: the mode, then the message.
// tmux knows most of the modes itself — the prefix, copy mode, which pane
// the keys are in — and the navigator names its own in @scrn_mode, which
// counts only while the keys are with it: a filter half-typed is not the
// mode of a shell. What the navigator has to say is in @scrn_msg. Each
// mode is a chip in its color with the line washed after it: amber for
// the prefix, the navigator's purple, the accent blue of a process.
func statusLeft() string {
	// A chip stands inside a conditional, where a comma would split the
	// alternatives, so the styles' commas are escaped.
	chip := func(color, word string) string {
		return strings.ReplaceAll(statusChip(color, word), ",", "#,")
	}
	mode := "#{?client_prefix," + chip(darkAttention, "PREFIX") +
		",#{?pane_in_mode," + chip(darkLabel, "COPY") +
		",#{?@scrn_nav,#{?@scrn_mode,#{@scrn_mode}," + chip(darkBrand, "NAV") + "}," +
		chip(darkAccent, "PROC") + "}}}"
	return mode + "#{@scrn_msg}"
}

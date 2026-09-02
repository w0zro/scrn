package main

import (
	"path/filepath"
	"strconv"
	"strings"
)

// scrn is a tmux client. The terminal attaches to scrn's private server the
// way any tmux client does, and tmux draws the shells, holds the prefix and
// keeps the status line; scrn is the program in the home window — the
// navigator — and the commands the prefix's chords run. What makes the
// server scrn's rather than a stock tmux is this configuration, written at
// every launch and sourced into a server already running, so the bindings
// are always the build's.

// confPath is where the configuration is written: beside the socket, in
// the state directory.
func confPath() string {
	return filepath.Join(filepath.Dir(socketPath()), "tmux.conf")
}

// tmuxConf is the configuration for scrn's server. scrn is the path of this
// build, which the chords run; the path is quoted so a directory with a
// space in its name still finds it.
func tmuxConf(scrn string, scrollback int) string {
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
		"# the keys are. Everything tmux would otherwise bind is unbound first.",
		"set -g prefix C-Space",
		"unbind -a",
		"unbind -a -T root",
		"bind C-Space last-window",
		"bind n "+run("home"),
		"bind j next-window",
		"bind k previous-window",
		"bind Enter "+run("jump"),
		"bind / "+run("home /"),
		"bind ? "+run("home ?"),
		"bind s "+run("shell '#{pane_current_path}'"),
		"bind a "+run("agent '#{pane_current_path}'"),
		"bind A "+run("home A"),
		"bind r "+run("run '#{pane_current_path}'"),
		"bind v copy-mode",
		"bind q detach-client",
		`bind R confirm-before -p "end the server, and every shell it holds? (y/n)" kill-server`,
		"",
		"# The server. The transcript cap is the config's; windows follow the",
		"# client that last spoke; a program's copy reaches the clipboard; names",
		"# are scrn's to set and nobody else's.",
		"set -g history-limit "+strconv.Itoa(scrollback),
		"set -g window-size latest",
		"set -g set-clipboard on",
		"set -g mode-keys vi",
		"set -g automatic-rename off",
		"set -g allow-rename off",
		"set -g set-titles on",
		`set -g set-titles-string "#W"`,
		"set -g escape-time 10",
		"set -g focus-events on",
		"set -g default-terminal tmux-256color",
		`set -as terminal-features ",*:RGB"`,
		"set -g display-time 3000",
		"",
		"# The status line: scrn's name, every shell with its mark, and the",
		"# prefix while a chord hangs — the foot the navigator used to wear.",
		"set -g status on",
		"set -g status-position bottom",
		"set -g status-interval 1",
		"set -g status-justify left",
		"set -g status-left-length 12",
		`set -g status-style "bg=default,fg=#8B949E"`,
		`set -g status-left "#[fg=#B9A7FF,bold] scrn #[default] "`,
		`set -g status-right "#{?client_prefix,#[fg=#D29922#,bold] prefix ,}"`,
		`set -g window-status-separator ""`,
		`set -g window-status-format " #W "`,
		`set -g window-status-current-format "#[fg=#79C0FF,bold] #W #[default]"`,
		`set -g message-style "bg=default,fg=#D29922,bold"`,
		`set -g message-command-style "bg=default,fg=#D29922"`,
		`set -g mode-style "bg=#15294A,fg=#E6E6E6"`,
	)
	return b.String()
}

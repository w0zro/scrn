package main

import (
	"strings"
	"testing"
)

func TestTheConfigurationBindsTheChordsToThisBuild(t *testing.T) {
	// The chords run the binary the launcher was, by its full path, quoted
	// so a directory with a space or a quote in its name still finds it.
	conf := tmuxConf("/opt/my tools/it's/conn", 4242, 33)
	for _, want := range []string{
		"set -g prefix C-Space",
		"unbind -a",
		"set -g mouse on",
		`bind - run-shell "'/opt/my tools/it'\''s/conn' home"`,
		`bind s run-shell "'/opt/my tools/it'\''s/conn' shell '#{pane_current_path}'"`,
		`bind j run-shell "'/opt/my tools/it'\''s/conn' next"`,
		"bind C-Space last-pane",
		`bind ? run-shell "'/opt/my tools/it'\''s/conn' keys '#{client_name}'"`,
		"bind v copy-mode",
		"bind q detach-client",
		"set -g history-limit 4242",
		"set -g automatic-rename off",
		"set -g main-pane-width 33",
		`set-hook -g window-resized 'if -F "#{@conn_home}" "select-layout main-vertical"'`,
		`set -g status-left "` + statusLeft() + `"`,
		// The status line begins with conn's name, in its purple, before
		// any mode: it is the one chip that stays.
		`set -g status-left "#[fg=` + tp.bg1 + `,bg=` + tp.purple + `,bold] CONN #{?client_prefix,`,
		// Then the mode — tmux's own, then the navigator's — and the
		// navigator's message.
		"#{?client_prefix,", "#{?pane_in_mode,", "#{?@conn_nav,",
		"#{?@conn_mode,#{@conn_mode},", "#{@conn_msg}",
		// Each mode is a chip on a dark ground of its color with the rest of
		// the line washed in it, the commas escaped for the conditional.
		"#[fg=" + tp.green + "#,bg=" + tp.bg2 + "#,bold] PROC #[fg=default#,bg=" + tp.bg1 + "#,fill=" + tp.bg1 + "]",
		"#[fg=" + tp.fg + "#,bg=" + tp.bg2 + "#,bold] NAV ",
		"#[fg=" + tp.amber + "#,bg=" + tp.bg2 + "#,bold] PREFIX ",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("the configuration lacks %q", want)
		}
	}
	// The root table is tmux's: that is where its mouse bindings live.
	if strings.Contains(conf, "-T root") {
		t.Error("the configuration touches the root table")
	}
}

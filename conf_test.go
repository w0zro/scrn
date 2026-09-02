package main

import (
	"strings"
	"testing"
)

func TestTheConfigurationBindsTheChordsToThisBuild(t *testing.T) {
	// The chords run the binary the launcher was, by its full path, quoted
	// so a directory with a space or a quote in its name still finds it.
	conf := tmuxConf("/opt/my tools/it's/scrn", 4242, 33)
	for _, want := range []string{
		"set -g prefix C-Space",
		"unbind -a",
		"set -g mouse on",
		`bind n run-shell "'/opt/my tools/it'\''s/scrn' home"`,
		`bind s run-shell "'/opt/my tools/it'\''s/scrn' shell '#{pane_current_path}'"`,
		`bind j run-shell "'/opt/my tools/it'\''s/scrn' next"`,
		"bind C-Space last-pane",
		`bind ? run-shell "'/opt/my tools/it'\''s/scrn' keys '#{client_name}'"`,
		"bind v copy-mode",
		"bind q detach-client",
		"set -g history-limit 4242",
		"set -g automatic-rename off",
		"set -g main-pane-width 33",
		`set-hook -g window-resized 'if -F "#{@scrn_home}" "select-layout main-vertical"'`,
		`set -g status-left "#[fg=#B9A7FF,bold] scrn #[default]#{@scrn_tabs}"`,
	} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("the configuration lacks %q", want)
		}
	}
	// The root table is tmux's: that is where its mouse bindings live.
	if strings.Contains(conf, "-T root") {
		t.Error("the configuration touches the root table")
	}
}

package main

import (
	"strings"
	"testing"
)

func TestTheConfigurationBindsTheChordsToThisBuild(t *testing.T) {
	// The chords run the binary the launcher was, by its full path, quoted
	// so a directory with a space or a quote in its name still finds it.
	conf := tmuxConf("/opt/my tools/it's/scrn", 4242)
	for _, want := range []string{
		"set -g prefix C-Space",
		"unbind -a -T root",
		`bind n run-shell "'/opt/my tools/it'\''s/scrn' home"`,
		`bind s run-shell "'/opt/my tools/it'\''s/scrn' shell '#{pane_current_path}'"`,
		"bind C-Space last-window",
		"bind v copy-mode",
		"bind q detach-client",
		"set -g history-limit 4242",
		"set -g automatic-rename off",
	} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("the configuration lacks %q", want)
		}
	}
}

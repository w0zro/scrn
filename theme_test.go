package main

import (
	"strings"
	"testing"
)

func TestAStatusChipWashesTheLineAfterIt(t *testing.T) {
	chip := statusChip(tp.amber, "PRE#FIX")
	for _, want := range []string{"fg=" + tp.amber, "bg=" + tp.bg2, ",bold] PRE##FIX ", "fill=" + tp.bg1} {
		if !strings.Contains(chip, want) {
			t.Errorf("chip %q lacks %q", chip, want)
		}
	}
}

func TestTheConfigNamesTmuxsSide(t *testing.T) {
	// The navigator draws with the terminal's slots and needs no telling;
	// tmux takes colors, so the config says which side, dark unless it
	// says light.
	t.Cleanup(func() { applyTheme("") })
	applyTheme("light")
	if tp != tmuxLight || !strings.Contains(tmuxConf("/opt/scrn", 100, 28), "bg="+tmuxLight.bg1) {
		t.Error("theme light should have tmux draw the light side")
	}
	applyTheme("anything else")
	if tp != tmuxDark {
		t.Error("anything but light is dark")
	}
}

package main

import (
	"strings"
	"testing"
)

func TestAShadeIsTheColorPartWayFromTheGround(t *testing.T) {
	// The ground is where 0 lands, the color itself where 1 does, and the
	// status line's depths lie between: a chip's ground and a wash are
	// the same hue, one deeper than the other.
	if got := shade(darkBrand, 1); got != darkBrand {
		t.Errorf("shade(brand, 1) = %s, want the color itself", got)
	}
	if got := shade(darkBrand, 0); got != darkBg {
		t.Errorf("shade(brand, 0) = %s, want the ground", got)
	}
	chip, wash := shade(darkBrand, chipDepth), shade(darkBrand, washDepth)
	if chip == wash || chip == darkBrand || wash == darkBg {
		t.Errorf("chip %s and wash %s should be two distinct shades between %s and %s", chip, wash, darkBg, darkBrand)
	}
	if got := shade("nonsense", 0.5); got != "nonsense" {
		t.Errorf("a color that is not a color comes back as it was, got %s", got)
	}
}

func TestAStatusChipWashesTheLineAfterIt(t *testing.T) {
	chip := statusChip(darkAttention, "PRE#FIX")
	for _, want := range []string{"fg=" + darkAttention, "bg=" + shade(darkAttention, chipDepth), ",bold] PRE##FIX ", "fill=" + shade(darkAttention, washDepth)} {
		if !strings.Contains(chip, want) {
			t.Errorf("chip %q lacks %q", chip, want)
		}
	}
}

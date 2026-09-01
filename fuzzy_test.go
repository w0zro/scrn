package main

import (
	"strings"
	"testing"
)

func TestAQueryFindsSubsequences(t *testing.T) {
	cases := []struct {
		query, s string
		want     bool
	}{
		{"tsl", "tressle-api", true},
		{"scrn", "scrn", true},
		{"SCRN", "scrn", true},
		{"mono api", "mono/services/api", true}, // tokens, each in order
		{"api mono", "mono/services/api", true}, // whatever order they come
		{"npmdev", "npm run dev", true},
		{"xyz", "tressle-api", false},
		{"", "anything", true},
	}
	for _, c := range cases {
		if got := answers(c.query, c.s); got != c.want {
			t.Errorf("answers(%q, %q) = %v, want %v", c.query, c.s, got, c.want)
		}
	}
}

func TestAMatchPrefersWordStarts(t *testing.T) {
	// "api" should light the api of services/api, not the stray letters
	// scattered earlier.
	spans, ok := subseq("api", "parade/api")
	if !ok {
		t.Fatal("api should match parade/api")
	}
	if spans[0] != 7 {
		t.Errorf("spans = %v, want the match to start at the word", spans)
	}
}

func TestSpansAreSortedAndWhole(t *testing.T) {
	spans := matchSpans("mono api", "mono/services/api")
	if len(spans) != 7 {
		t.Fatalf("spans = %v, want both tokens' seven letters", spans)
	}
	for i := 1; i < len(spans); i++ {
		if spans[i] <= spans[i-1] {
			t.Fatalf("spans = %v, want them sorted unique", spans)
		}
	}
	if matchSpans("zz", "mono") != nil {
		t.Error("a query that does not find s should light nothing")
	}
}

func TestTheFilterFindsByFuzz(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "tsl")
	found := false
	for _, r := range m.rows {
		if r.kind != rowProc && r.project.Name == "tressle-api" {
			found = true
		}
	}
	if !found || len(m.rows) != 1 {
		t.Fatalf("rows = %d, want tsl to find tressle-api alone", len(m.rows))
	}
}

func TestThePickerFindsByFuzz(t *testing.T) {
	m := pickerOn(
		conversation{ID: "aaaa-1111", Prompt: "fix the resize race"},
		conversation{ID: "bbbb-2222", Prompt: "polish the site"},
	)
	for _, k := range []string{"f", "x", "r", "c"} { // fxrc ⊂ fix-resize-race
		m = press(m, k)
	}
	if got := m.resume.matches(); len(got) != 1 || got[0].ID != "aaaa-1111" {
		t.Fatalf("matches = %+v, want the subsequence to find the resize work", got)
	}
}

func TestTheMatchedLettersAreLit(t *testing.T) {
	m := typeFilter(press(narrowed(manyProjects(90, 14)), "/"), "tsl")
	var row string
	for i, r := range m.rows {
		if r.kind != rowProc && r.project.Name == "tressle-api" {
			row = m.renderRow(r, i == m.cursor)
		}
	}
	if row == "" {
		t.Fatal("setup: no tressle-api row rendered")
	}
	if !strings.Contains(row, matchStyle.Render("t")) {
		t.Errorf("row = %q, want the matched letters wearing matchStyle", row)
	}
	if stripANSI(row) == row {
		t.Error("a highlighted row should carry styling")
	}
}

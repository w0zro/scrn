package main

import (
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The filter and the picker find things the modern way: each word of the
// query has to appear in order inside the candidate, not contiguously —
// "tsl" finds tressle-api, "mono api" finds mono/services/api — and the
// letters that matched are lit in the results, so a narrowed list always
// shows why it narrowed.

// answers reports whether the query finds s: every space-separated token a
// case-folded subsequence of it. An empty query finds everything.
func answers(query, s string) bool {
	for _, token := range strings.Fields(query) {
		if _, ok := subseq(token, s); !ok {
			return false
		}
	}
	return true
}

// matchSpans is the rune positions of s that the query's tokens matched,
// sorted and deduplicated — what the highlight lights. Nothing when the
// query does not find s at all.
func matchSpans(query, s string) []int {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return nil
	}
	seen := map[int]bool{}
	for _, token := range tokens {
		positions, ok := subseq(token, s)
		if !ok {
			return nil
		}
		for _, p := range positions {
			seen[p] = true
		}
	}
	spans := make([]int, 0, len(seen))
	for p := range seen {
		spans = append(spans, p)
	}
	// Insertion sort: spans are few and nearly ordered.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j] < spans[j-1]; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	return spans
}

// subseq matches token as a subsequence of s, case-folded, preferring word
// starts: a token letter takes a position after a separator when one is
// there to take, so "api" lights the api in services/api rather than the
// stray letters before it. Greedy otherwise, which is enough to be right
// nearly always and cheap enough to run on every keystroke.
func subseq(token, s string) ([]int, bool) {
	runes := []rune(s)
	folded := make([]rune, len(runes))
	for i, r := range runes {
		folded[i] = unicode.ToLower(r)
	}
	want := []rune(strings.ToLower(token))
	if len(want) == 0 {
		return nil, true
	}

	positions := make([]int, 0, len(want))
	at := 0
	for _, w := range want {
		found := -1
		// First choice: the letter starting a word.
		for i := at; i < len(folded); i++ {
			if folded[i] == w && (i == 0 || isSep(folded[i-1])) {
				found = i
				break
			}
		}
		if found < 0 {
			for i := at; i < len(folded); i++ {
				if folded[i] == w {
					found = i
					break
				}
			}
		}
		if found < 0 {
			return nil, false
		}
		positions = append(positions, found)
		at = found + 1
	}
	return positions, true
}

// isSep is what ends a word inside a name: the joints of paths, flags and
// identifiers.
func isSep(r rune) bool {
	return r == '/' || r == '-' || r == '_' || r == '.' || r == ' ' || r == ':'
}

// highlight renders s with the matched runes lit, the rest in base. The
// result carries its styling, so anything cutting it afterwards has to cut
// ansi-aware.
func highlight(s string, spans []int, base lipgloss.Style) string {
	if len(spans) == 0 {
		return base.Render(s)
	}
	lit := map[int]bool{}
	for _, p := range spans {
		lit[p] = true
	}
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && lit[j] == lit[i] {
			j++
		}
		if lit[i] {
			b.WriteString(matchStyle.Render(string(runes[i:j])))
		} else {
			b.WriteString(base.Render(string(runes[i:j])))
		}
		i = j
	}
	return b.String()
}

// truncateStyled shortens a styled string to width columns, marking the cut
// with an ellipsis — the styled twin of truncate, cutting from the left for
// qualified names and from the right for everything else.
func truncateStyled(s string, width int, fromLeft bool) string {
	if width <= 0 {
		return ""
	}
	total := lipgloss.Width(s)
	if total <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	if fromLeft {
		return ansi.TruncateLeft(s, total-width+1, "…")
	}
	return ansi.Truncate(s, width, "…")
}

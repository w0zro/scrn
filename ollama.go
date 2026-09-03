package main

import (
	"strings"
)

// Ollama is the strategy layer's proof of graceful degradation: a REPL that
// advertises no status and keeps no conversations worth resuming. Its kind
// fills in nothing but how to start one, so its rows are plain processes —
// no marks, no picker entries — which is the whole truth about them.

var ollamaKind = agentKind{
	name:    "ollama",
	command: "ollama",
	run:     ollamaRun,
}

// ollamaRun is the command that starts an ollama REPL. ollama run wants a
// model named, and conn is not the one to pick a favorite: the first model
// the local daemon lists is the one already chosen by being pulled. With
// nothing to ask or nothing pulled, the bare command runs and says its own
// piece in the shell — which survives it, per the wrapper every run gets.
// The config's agentRuns names an exact model when the first is wrong.
func ollamaRun() string {
	out, err := listing(scanTimeout, "ollama", "list")
	if model := firstOllamaModel(string(out)); err == nil && model != "" {
		return "ollama run " + model
	}
	return "ollama run"
}

// firstOllamaModel reads the first model out of an ollama list: a header
// line, then one model per line, name first.
func firstOllamaModel(out string) string {
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return ""
	}
	fields := strings.Fields(lines[1])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

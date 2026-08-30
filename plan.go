package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Working on a project usually means the same few processes every time: a dev
// server, something watching the tests, a Claude. A project can say so, and
// scrn can start what is missing rather than leaving it to memory.
//
// It is a list to run, not a promise to keep. Nothing here restarts anything:
// if a dev server dies the row says so and you start it again.

// entry is one process a project says it needs.
type entry struct {
	Name string
	Run  string
}

// planFile is what a project writes when the convention is wrong for it. It is
// Procfile format because that is the smallest thing that says what this needs
// to say: a name and a command, one per line.
const planFile = ".scrn"

// plan is what a project needs, and where that was learned from.
type plan struct {
	Entries []entry
	Source  string
}

// readPlan works out what a project needs.
//
// A project that says so outright is believed. Otherwise the conventions are
// read in the order they are trustworthy: a Procfile means every line is a
// process, whereas a package.json means only that one of its scripts probably
// is — so the scripts are taken from a short list of names that are long
// running by convention, rather than every script it happens to define.
func readPlan(dir string) plan {
	if entries := readProcfile(filepath.Join(dir, planFile)); len(entries) > 0 {
		return plan{Entries: entries, Source: planFile}
	}
	if entries := readProcfile(filepath.Join(dir, "Procfile")); len(entries) > 0 {
		return plan{Entries: entries, Source: "Procfile"}
	}
	if entries := readPackageScripts(filepath.Join(dir, "package.json")); len(entries) > 0 {
		return plan{Entries: entries, Source: "package.json"}
	}
	return plan{}
}

// readProcfile reads "name: command" lines, which is all a Procfile is.
func readProcfile(path string) []entry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var entries []entry
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, run, ok := strings.Cut(line, ":")
		name, run = strings.TrimSpace(name), strings.TrimSpace(run)
		if !ok || name == "" || run == "" {
			continue
		}
		entries = append(entries, entry{Name: name, Run: run})
	}
	return entries
}

// longRunning are the script names that mean a process rather than a task. A
// package.json says nothing about which of its scripts keep going, and getting
// that wrong means starting a build as though it were a server, so only the
// names that are long running by convention are taken.
var longRunning = []string{"dev", "start"}

// readPackageScripts takes the one script a package.json suggests is a process.
// Only one: a project with both a dev and a start script means them as
// alternatives, not as two things to run at once.
func readPackageScripts(path string) []entry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return nil
	}

	for _, name := range longRunning {
		if pkg.Scripts[name] != "" {
			return []entry{{Name: name, Run: "npm run " + name}}
		}
	}
	return nil
}

// missing is the entries of a plan that are not already running.
func (p plan) missing(running map[string]bool) []entry {
	var out []entry
	for _, e := range p.Entries {
		if !running[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

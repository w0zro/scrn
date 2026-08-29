package main

import (
	"path/filepath"
	"testing"
)

func projectWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := writeFile(filepath.Join(dir, name), body); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAProjectThatSaysWhatItNeedsIsBelieved(t *testing.T) {
	dir := projectWith(t, map[string]string{
		planFile: "web:    npm run dev:web\nserver: npm run dev:server\nclaude: claude\n",
	})

	got := readPlan(dir)
	if got.Source != planFile {
		t.Errorf("source = %q, want the project's own file", got.Source)
	}
	want := []entry{
		{Name: "web", Run: "npm run dev:web"},
		{Name: "server", Run: "npm run dev:server"},
		{Name: "claude", Run: "claude"},
	}
	if len(got.Entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", got.Entries, want)
	}
	for i := range want {
		if got.Entries[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got.Entries[i], want[i])
		}
	}
}

func TestTheProjectsOwnFileWinsOverTheConventions(t *testing.T) {
	dir := projectWith(t, map[string]string{
		planFile:       "only: this one\n",
		"Procfile":     "web: not this\n",
		"package.json": `{"scripts":{"dev":"nor this"}}`,
	})

	got := readPlan(dir)
	if got.Source != planFile || len(got.Entries) != 1 || got.Entries[0].Name != "only" {
		t.Errorf("plan = %+v, want the project's own file to win", got)
	}
}

func TestAProcfileMeansEveryLineIsAProcess(t *testing.T) {
	dir := projectWith(t, map[string]string{
		"Procfile": "# a comment\n\nweb: bin/server\nworker: bin/worker\n",
	})

	got := readPlan(dir)
	if got.Source != "Procfile" || len(got.Entries) != 2 {
		t.Fatalf("plan = %+v, want both lines and neither the blank nor the comment", got)
	}
}

func TestAScriptIsOnlyTakenWhenItsNameMeansItKeepsGoing(t *testing.T) {
	// A package.json says nothing about which of its scripts keep running, and
	// starting a build as though it were a server is worse than starting
	// nothing.
	dir := projectWith(t, map[string]string{
		"package.json": `{"scripts":{"build":"tsc","lint":"eslint .","dev":"next dev"}}`,
	})

	got := readPlan(dir)
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %+v, want only the one that keeps going", got.Entries)
	}
	if got.Entries[0] != (entry{Name: "dev", Run: "npm run dev"}) {
		t.Errorf("entry = %+v, want the dev script", got.Entries[0])
	}
}

func TestDevAndStartAreAlternativesNotBoth(t *testing.T) {
	dir := projectWith(t, map[string]string{
		"package.json": `{"scripts":{"dev":"next dev","start":"next start"}}`,
	})

	got := readPlan(dir)
	if len(got.Entries) != 1 || got.Entries[0].Name != "dev" {
		t.Errorf("entries = %+v, want the development one alone", got.Entries)
	}
}

func TestStartIsUsedWhenThereIsNoDev(t *testing.T) {
	dir := projectWith(t, map[string]string{
		"package.json": `{"scripts":{"start":"parcel ./src/index.html"}}`,
	})

	if got := readPlan(dir); len(got.Entries) != 1 || got.Entries[0].Name != "start" {
		t.Errorf("entries = %+v, want the start script", got.Entries)
	}
}

func TestAProjectThatSaysNothingNeedsNothing(t *testing.T) {
	dir := projectWith(t, map[string]string{"README.md": "hello"})
	if got := readPlan(dir); len(got.Entries) != 0 || got.Source != "" {
		t.Errorf("plan = %+v, want nothing inferred", got)
	}
}

func TestAMalformedPackageJsonIsNotAProject_Plan(t *testing.T) {
	dir := projectWith(t, map[string]string{"package.json": "{ not json"})
	if got := readPlan(dir); len(got.Entries) != 0 {
		t.Errorf("plan = %+v, want nothing from a file that cannot be read", got)
	}
}

func TestMissingIsWhatIsNotAlreadyRunning(t *testing.T) {
	p := plan{Entries: []entry{{Name: "web"}, {Name: "server"}, {Name: "claude"}}}

	got := p.missing(map[string]bool{"web": true, "claude": true})
	if len(got) != 1 || got[0].Name != "server" {
		t.Errorf("missing = %+v, want the one that is not up", got)
	}
	if len(p.missing(nil)) != 3 {
		t.Error("with nothing running, everything is missing")
	}
	if len(p.missing(map[string]bool{"web": true, "server": true, "claude": true})) != 0 {
		t.Error("with everything running, nothing is missing")
	}
}

func TestWhatTheRealProjectsWouldInfer(t *testing.T) {
	// Not an assertion about any one project, just that reading a real tree
	// does not blow up and does find the convention where it exists.
	cfg, _ := loadConfig()
	ps, err := discoverProjects(expandPath(cfg.ProjectsDir))
	if err != nil {
		t.Skip(err)
	}
	var withPlan int
	for _, p := range ps {
		if len(readPlan(p.Path).Entries) > 0 {
			withPlan++
		}
	}
	if withPlan == 0 {
		t.Error("no project in the tree suggested anything, which is unlikely")
	}
	t.Logf("%d of %d projects suggest something to run", withPlan, len(ps))
}

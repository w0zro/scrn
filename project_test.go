package main

import (
	"os"
	"path/filepath"
	"testing"
)

// tree builds a directory layout; a path ending in /.git marks a repo.
func tree(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func names(ps []Project) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDiscoverFindsReposAtAnyDepth(t *testing.T) {
	root := tree(t,
		"flat/.git",
		"owner/nested/.git",
		"a/b/c/deep/.git",
		"notarepo/src",
	)

	got, err := discoverProjects(root)
	if err != nil {
		t.Fatalf("discoverProjects: %v", err)
	}
	if want := []string{"deep", "flat", "nested"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v (sorted, repos only)", names(got), want)
	}
}

func TestDiscoverDoesNotDescendIntoRepos(t *testing.T) {
	root := tree(t, "outer/.git", "outer/inner/.git")

	got, _ := discoverProjects(root)
	if want := []string{"outer"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v; a repo inside a repo belongs to its owner", names(got), want)
	}
}

func TestDiscoverSkipsVendorDirs(t *testing.T) {
	root := tree(t, "app/.git", "site/node_modules/dep/.git", "site/src")

	got, _ := discoverProjects(root)
	if want := []string{"app"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v", names(got), want)
	}
}

func TestDiscoverHandlesGitFileWorktrees(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A worktree or submodule has .git as a file, not a directory.
	if err := os.WriteFile(filepath.Join(root, "wt", ".git"), []byte("gitdir: /elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, _ := discoverProjects(root)
	if want := []string{"wt"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v", names(got), want)
	}
}

func TestDiscoverMissingRootErrors(t *testing.T) {
	if _, err := discoverProjects(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a missing projects directory should report an error")
	}
}

func TestNamesAreBareWhenUnique(t *testing.T) {
	root := tree(t, "w0zro/scrn/.git", "w0zro/notebook/.git")

	got, _ := discoverProjects(root)
	if want := []string{"notebook", "scrn"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v; unique names should not be qualified", names(got), want)
	}
}

func TestCollidingNamesGainParent(t *testing.T) {
	root := tree(t, "w0zro/site/.git", "archive/site/.git", "w0zro/scrn/.git")

	got, _ := discoverProjects(root)
	want := []string{"archive/site", "scrn", "w0zro/site"}
	if !equal(names(got), want) {
		t.Errorf("names = %v, want %v", names(got), want)
	}
}

func TestCollidingNamesGrowUntilUnique(t *testing.T) {
	// Both repos share a name *and* a parent, so one parent is not enough.
	root := tree(t,
		"archive/checklists.org/api/.git",
		"w0zro/archive/checklists.org/api/.git",
	)

	got, _ := discoverProjects(root)
	want := []string{
		"archive/checklists.org/api",
		"w0zro/archive/checklists.org/api",
	}
	if !equal(names(got), want) {
		t.Errorf("names = %v, want %v", names(got), want)
	}
}

func TestNamesStayDistinct(t *testing.T) {
	root := tree(t,
		"a/dup/.git", "b/dup/.git", "c/dup/.git",
		"x/solo/.git",
	)

	got, _ := discoverProjects(root)
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p.Name] {
			t.Errorf("duplicate display name %q in %v", p.Name, names(got))
		}
		seen[p.Name] = true
	}
}

func TestReposAreFoundByTheirRealPath(t *testing.T) {
	// A process list reports the resolved path, so repositories have to be
	// discovered under theirs too or nothing is ever attributed to them.
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	projects, err := discoverProjects(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %+v, want the one repo", projects)
	}

	resolved, _ := filepath.EvalSymlinks(filepath.Join(real, "repo"))
	if projects[0].Path != resolved {
		t.Errorf("path = %q, want the resolved %q", projects[0].Path, resolved)
	}
}

func TestProcessesUnderASymlinkedRootAreAttributed(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	projects, _ := discoverProjects(link)
	resolved, _ := filepath.EvalSymlinks(filepath.Join(real, "repo"))

	if !under(resolved, projects[0].Path) {
		t.Errorf("a process in %q was not attributed to the repo at %q", resolved, projects[0].Path)
	}
}

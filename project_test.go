package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	got, err := discoverProjects(root, nil)
	if err != nil {
		t.Fatalf("discoverProjects: %v", err)
	}
	if want := []string{"deep", "flat", "nested"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v (sorted, repos only)", names(got), want)
	}
}

func TestDiscoverDoesNotDescendIntoRepos(t *testing.T) {
	root := tree(t, "outer/.git", "outer/inner/.git")

	got, _ := discoverProjects(root, nil)
	if want := []string{"outer"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v; a repo inside a repo belongs to its owner", names(got), want)
	}
}

func TestDiscoverSkipsVendorDirs(t *testing.T) {
	root := tree(t, "app/.git", "site/node_modules/dep/.git", "site/src")

	got, _ := discoverProjects(root, nil)
	if want := []string{"app"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v", names(got), want)
	}
}

func TestDiscoverSkipsTaggedCacheDirs(t *testing.T) {
	// CACHEDIR.TAG is the marker caching tools leave so walkers pass them by;
	// it covers the generated directories no name list can.
	root := tree(t, "app/.git", "bazel-out/k8-fastbuild/some/.git")
	if err := os.WriteFile(filepath.Join(root, "bazel-out", "CACHEDIR.TAG"), []byte("Signature: 8a477f597d28d172789f06886806bc55"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, _ := discoverProjects(root, nil)
	if want := []string{"app"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v; a tagged cache is not walked", names(got), want)
	}
}

func TestDiscoverHonorsConfiguredSkips(t *testing.T) {
	// The config's skipDirs join the built-ins: the big generated directories
	// one machine grows are its own to name.
	root := tree(t, "app/.git", "dist/pkg/stray/.git", "site/node_modules/dep/.git")

	cfg := Config{SkipDirs: []string{"dist", " ", ""}}
	got, err := discoverProjects(root, cfg.skipSet())
	if err != nil {
		t.Fatalf("discoverProjects: %v", err)
	}
	if want := []string{"app"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v; both the configured and built-in skips hold", names(got), want)
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

	got, _ := discoverProjects(root, nil)
	if want := []string{"wt"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v", names(got), want)
	}
}

func TestDiscoverMissingRootErrors(t *testing.T) {
	if _, err := discoverProjects(filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Error("a missing projects directory should report an error")
	}
}

func TestNamesAreBareWhenUnique(t *testing.T) {
	root := tree(t, "w0zro/scrn/.git", "w0zro/notebook/.git")

	got, _ := discoverProjects(root, nil)
	if want := []string{"notebook", "scrn"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v; unique names should not be qualified", names(got), want)
	}
}

func TestCollidingNamesGainParent(t *testing.T) {
	root := tree(t, "w0zro/site/.git", "archive/site/.git", "w0zro/scrn/.git")

	got, _ := discoverProjects(root, nil)
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

	got, _ := discoverProjects(root, nil)
	// The second will not fit, so its parents are squeezed to initials. They
	// still tell the two apart, which is all the parents were ever for.
	want := []string{
		"archive/checklists.org/api",
		"w/a/c/api",
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

	got, _ := discoverProjects(root, nil)
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

	projects, err := discoverProjects(link, nil)
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

	projects, _ := discoverProjects(link, nil)
	resolved, _ := filepath.EvalSymlinks(filepath.Join(real, "repo"))

	if !under(resolved, projects[0].Path) {
		t.Errorf("a process in %q was not attributed to the repo at %q", resolved, projects[0].Path)
	}
}

func TestALongNameKeepsTheRepositoryAndSqueezesTheParents(t *testing.T) {
	// Cutting the name itself is what leaves two rows reading the same.
	root := tree(t, "archive/TressleAI/tressle-app/.git")

	got, _ := discoverProjects(root, nil)
	if len(got) != 1 || got[0].Name != "tressle-app" {
		t.Fatalf("names = %v, want the repository alone when nothing collides", names(got))
	}
}

func TestSqueezedNamesStillTellRepositoriesApart(t *testing.T) {
	// These are the two that rendered identically before: qualifying them was
	// the whole point, and truncation threw away the part that did it.
	root := tree(t,
		"archive/checklists.org/checklists-web/.git",
		"checklists.org/checklists-web/.git",
	)

	got, _ := discoverProjects(root, nil)
	seen := map[string]bool{}
	for _, p := range got {
		if len([]rune(p.Name)) > nameRoom() {
			t.Errorf("name %q is %d columns, wider than the %d it has", p.Name, len([]rune(p.Name)), nameRoom())
		}
		if seen[p.Name] {
			t.Errorf("two repositories are both shown as %q", p.Name)
		}
		seen[p.Name] = true
	}
}

func TestParentsGrowBackWhenInitialsWouldCollide(t *testing.T) {
	// Squeezed to one letter these would both be "a/x/api", so they are given
	// back exactly as much as it takes to differ.
	root := tree(t,
		"alpha/checklists.org/some-long-repository-name/.git",
		"apple/checklists.org/some-long-repository-name/.git",
	)

	got, _ := discoverProjects(root, nil)
	if len(got) != 2 {
		t.Fatalf("projects = %v", names(got))
	}
	if got[0].Name == got[1].Name {
		t.Fatalf("both shown as %q", got[0].Name)
	}
	for _, p := range got {
		if !strings.HasSuffix(p.Name, "some-long-repository-name") {
			t.Errorf("name = %q, want the repository's own name kept whole", p.Name)
		}
	}
}

func TestAShortNameIsLeftAlone(t *testing.T) {
	root := tree(t, "hsg/brand/.git", "other/brand/.git")

	got, _ := discoverProjects(root, nil)
	for _, p := range got {
		if !strings.Contains(p.Name, "/") || strings.Contains(p.Name, "//") {
			t.Errorf("name = %q, want it qualified but not squeezed", p.Name)
		}
	}
	if names(got)[0] != "hsg/brand" {
		t.Errorf("names = %v, want the parents whole when they fit", names(got))
	}
}

func TestDiscoverAllMergesRoots(t *testing.T) {
	a := tree(t, "scrn/.git")
	b := tree(t, "mono/.git")

	got, _, err := discoverAll([]string{a, b}, nil)
	if err != nil {
		t.Fatalf("discoverAll: %v", err)
	}
	if want := []string{"mono", "scrn"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v (both roots, one sorted list)", names(got), want)
	}
}

func TestDiscoverAllQualifiesAcrossRoots(t *testing.T) {
	// An api here and an api there: only the roots tell them apart.
	a := tree(t, "api/.git")
	b := tree(t, "api/.git")

	got, _, err := discoverAll([]string{a, b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name == got[1].Name {
		t.Fatalf("names = %v, want two, told apart by their roots", names(got))
	}
	for _, p := range got {
		if !strings.Contains(p.Name, "/") {
			t.Errorf("name = %q, want it qualified by its root", p.Name)
		}
	}
}

func TestDiscoverAllToleratesARootFromAnotherMachine(t *testing.T) {
	// The same config rides between machines; the work checkout not existing
	// at home should not blank the home projects.
	a := tree(t, "scrn/.git")
	got, _, err := discoverAll([]string{a, filepath.Join(t.TempDir(), "work-only")}, nil)
	if err != nil {
		t.Fatalf("discoverAll: %v", err)
	}
	if want := []string{"scrn"}; !equal(names(got), want) {
		t.Errorf("names = %v, want %v", names(got), want)
	}
}

func TestDiscoverAllErrorsWhenEveryRootFails(t *testing.T) {
	if _, _, err := discoverAll([]string{filepath.Join(t.TempDir(), "nope")}, nil); err == nil {
		t.Error("a lone mistyped root should still say so")
	}
}

func TestDiscoverAllDropsARepoListedByTwoRoots(t *testing.T) {
	// One root nested inside another finds the same repository twice.
	a := tree(t, "inner/scrn/.git")
	got, _, err := discoverAll([]string{a, filepath.Join(a, "inner")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("projects = %v, want the repo once", names(got))
	}
}

// repoWith makes a git repository holding the given files, contents empty.
func repoWith(t *testing.T, files ...string) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, f := range files {
		path := filepath.Join(repo, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestSubProjectsAreTheDirectoriesWithManifests(t *testing.T) {
	repo := repoWith(t,
		"package.json",              // the root is the repository's own row
		"services/api/package.json", // a service
		"services/api/src/main.js",  // not a manifest
		"tools/Procfile",            // an explicit plan
		"rust/Cargo.toml",           // not just npm, of course
		"notes/.scrn",               // untracked and personal still counts
	)

	got := subProjects(repo)
	want := []string{"notes", "rust", "services/api", "tools"}
	if !equal(names(got), want) {
		t.Errorf("subs = %v, want %v (sorted, root excluded)", names(got), want)
	}
	for _, s := range got {
		if !strings.HasPrefix(s.Path, repo) {
			t.Errorf("path = %q, want it inside the repository", s.Path)
		}
	}
}

func TestSubProjectsRespectTheIgnoreRules(t *testing.T) {
	repo := repoWith(t,
		".gitignore", // written below
		"web/app/package.json",
		"web/app/node_modules/dep/package.json",
	)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := subProjects(repo)
	if want := []string{"web/app"}; !equal(names(got), want) {
		t.Errorf("subs = %v, want %v; what git ignores, scrn ignores", names(got), want)
	}
}

func TestSubProjectsOfAPlainDirectoryAreNone(t *testing.T) {
	// Not a repository: git has no index to ask, and the answer is nothing
	// rather than an error.
	if got := subProjects(t.TempDir()); len(got) != 0 {
		t.Errorf("subs = %v, want none outside a repository", names(got))
	}
}

func TestReposSharingAFolderAreGrouped(t *testing.T) {
	// The shape is the declaration: the repositories were put in one folder
	// because they collectively make one project.
	root := tree(t, "checklists.org/api/.git", "checklists.org/web/.git", "solo/.git")

	ps, groups, err := discoverAll([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "checklists.org" {
		t.Fatalf("groups = %+v, want the shared folder alone", groups)
	}
	byName := map[string]Project{}
	for _, p := range ps {
		byName[p.Name] = p
	}
	for _, name := range []string{"api", "web"} {
		p, ok := byName[name]
		if !ok {
			t.Fatalf("names = %v, want the grouped repo by its own directory name", names(ps))
		}
		if p.Group != groups[0].Path {
			t.Errorf("%s.Group = %q, want %q", name, p.Group, groups[0].Path)
		}
	}
	if byName["solo"].Group != "" {
		t.Error("a repository directly under a root belongs to no group")
	}
}

func TestAFolderWithOneRepoIsNotAGroup(t *testing.T) {
	// A flat layout should not grow a header per repo.
	root := tree(t, "w0zro/scrn/.git")

	ps, groups, err := discoverAll([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %+v, want none for a folder of one", groups)
	}
	if len(ps) != 1 || ps[0].Group != "" {
		t.Errorf("projects = %+v, want the one repo, ungrouped", ps)
	}
}

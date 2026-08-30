package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Project is a place work happens: a git repository found under a configured
// root, a group of repositories that collectively make one project, or a
// sub-project inside a repository.
type Project struct {
	Name string // display name: the repo directory, qualified only if ambiguous
	Path string // absolute path to the repo

	// Group is the path of the folder grouping this repository with its
	// siblings, and empty for a repository that stands alone. A project is
	// often several repositories in one directory, worked on at that level.
	Group string
}

// skipDirs are never descended into. They hold vendored code, not the projects
// the navigator is for, and walking them dominates the scan. The config's
// skipDirs add to these — no built-in list can know the generated directories
// a particular machine grows.
var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	".build":       true,
	"Pods":         true,
	".venv":        true,
	"venv":         true,
}

// discoverAll finds the repositories under every root, in one sorted list.
//
// A root that cannot be walked is passed over so long as any other can: the
// same config rides between machines, and the work checkout not existing at
// home should not blank the home projects. Only every root failing is an
// error, so a lone mistyped root still says so.
//
// Names are assigned within each root, the way they always were; a name that
// collides across roots is qualified by its root's own name, which is the
// only thing that tells an api here from an api there.
//
// The second list is the groups: the folders holding two or more of the
// repositories, which is what a project often is — several repositories in
// one directory, worked on at that level. Repositories in a group carry its
// path in their Group field.
func discoverAll(roots []string, skip map[string]bool) ([]Project, []Project, error) {
	var found []Project
	rootOf := map[string]string{} // repo path → the root it was found under
	seen := map[string]bool{}
	var firstErr error
	walked := false

	// Resolved the same way discoverProjects resolves them, so a repo's
	// parent can be compared against the root it came from.
	resolved := make([]string, 0, len(roots))
	for _, root := range roots {
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		resolved = append(resolved, root)
	}

	for _, root := range resolved {
		ps, err := discoverProjects(root, skip)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		walked = true
		for _, p := range ps {
			if seen[p.Path] {
				continue // one root nested in another lists a repo twice
			}
			seen[p.Path] = true
			rootOf[p.Path] = root
			found = append(found, p)
		}
	}
	if !walked && firstErr != nil {
		return nil, nil, firstErr
	}

	groups := deriveGroups(found, rootOf)
	qualifyAcrossRoots(found, rootOf)
	byName := func(ps []Project) func(i, j int) bool {
		return func(i, j int) bool {
			a, b := strings.ToLower(ps[i].Name), strings.ToLower(ps[j].Name)
			if a != b {
				return a < b
			}
			return ps[i].Path < ps[j].Path
		}
	}
	sort.Slice(found, byName(found))
	sort.Slice(groups, byName(groups))
	return found, groups, nil
}

// deriveGroups finds the folders that group repositories: a repository's own
// parent, when it is not a root and holds at least one sibling repository.
// The shape is the declaration — the repositories were put in one folder
// because they collectively make one project. A folder with a single
// repository stays out, so a flat layout does not grow a header per repo.
//
// A group is named by its path within its root, and by its root's base name
// too when two roots would otherwise offer the same group.
func deriveGroups(ps []Project, rootOf map[string]string) []Project {
	isRoot := map[string]bool{}
	for _, root := range rootOf {
		isRoot[root] = true
	}

	siblings := map[string][]int{}
	for i, p := range ps {
		if parent := filepath.Dir(p.Path); !isRoot[parent] {
			siblings[parent] = append(siblings[parent], i)
		}
	}

	var groups []Project
	groupRoot := map[string]string{}
	for parent, idxs := range siblings {
		if len(idxs) < 2 {
			continue
		}
		root := rootOf[ps[idxs[0]].Path]
		name := filepath.Base(parent)
		if rel, err := filepath.Rel(root, parent); err == nil {
			name = filepath.ToSlash(rel)
		}
		for _, i := range idxs {
			ps[i].Group = parent
			// The group header carries the context, so the repository under
			// it goes by its own directory name; qualification would only
			// repeat what the row above already says.
			ps[i].Name = filepath.Base(ps[i].Path)
		}
		groups = append(groups, Project{Name: name, Path: parent})
		groupRoot[parent] = root
	}
	qualifyAcrossRoots(groups, groupRoot)
	return groups
}

// qualifyAcrossRoots prefixes the names that different roots both produced
// with the base name of each repository's root.
func qualifyAcrossRoots(ps []Project, rootOf map[string]string) {
	byName := map[string][]int{}
	for i, p := range ps {
		byName[p.Name] = append(byName[p.Name], i)
	}
	for _, idxs := range byName {
		if len(idxs) < 2 {
			continue
		}
		distinct := map[string]bool{}
		for _, i := range idxs {
			distinct[rootOf[ps[i].Path]] = true
		}
		if len(distinct) < 2 {
			continue // one root's own collisions were already settled
		}
		for _, i := range idxs {
			ps[i].Name = filepath.Base(rootOf[ps[i].Path]) + "/" + ps[i].Name
		}
	}
}

// discoverProjects finds every git repository nested under root, at any depth.
// A repository is not descended into, so nested checkouts inside a repo are
// left to the repo that owns them. skip is the directory names never entered;
// nil means the built-in list alone.
func discoverProjects(root string, skip map[string]bool) ([]Project, error) {
	if skip == nil {
		skip = skipDirs
	}
	var found []Project

	// Resolve the root before walking, so repository paths come out in the
	// same form the process list reports working directories in. lsof answers
	// with the real path, and on macOS a projects directory under /tmp — or
	// anywhere reached through a symlink — would otherwise never match the
	// processes running in it.
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory should not abort the whole scan.
			if path == root {
				return err
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (skip[name] || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		if isRepo(path) {
			found = append(found, Project{Path: path})
			return fs.SkipDir
		}
		// A directory tagged as a cache says itself it holds nothing to find.
		// CACHEDIR.TAG is the tag the caching tools agreed to leave for
		// walkers like this one, and it covers what no name list can.
		if path != root && isCacheDir(path) {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	assignNames(root, found)
	sort.Slice(found, func(i, j int) bool {
		a, b := strings.ToLower(found[i].Name), strings.ToLower(found[j].Name)
		if a != b {
			return a < b
		}
		return found[i].Path < found[j].Path
	})
	return found, nil
}

// assignNames gives each project the shortest name that tells it apart from the
// others: the repo directory alone, and for repos whose directory name is not
// unique, as many parent directories as it takes. Qualifying only the ambiguous
// names keeps the common case short — "scrn", not "w0zro/scrn".
func assignNames(root string, ps []Project) {
	segs := make([][]string, len(ps))
	depth := make([]int, len(ps))
	for i, p := range ps {
		rel, err := filepath.Rel(root, p.Path)
		if err != nil {
			rel = filepath.Base(p.Path)
		}
		segs[i] = strings.Split(filepath.ToSlash(rel), "/")
		depth[i] = 1
	}

	// Grow the ambiguous names one parent at a time. Two repos can share a
	// parent as well as a name, so this repeats until every name stands alone
	// or has taken in its whole path.
	for {
		groups := map[string][]int{}
		for i := range ps {
			n := nameAt(segs[i], depth[i])
			groups[n] = append(groups[n], i)
		}
		grew := false
		for _, idxs := range groups {
			if len(idxs) < 2 {
				continue
			}
			for _, i := range idxs {
				if depth[i] < len(segs[i]) {
					depth[i]++
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}

	for i := range ps {
		ps[i].Name = nameAt(segs[i], depth[i])
	}
	shortenNames(ps, segs, depth)
}

// nameRoom is the width a repository's name has in the navigator, which is
// what decides whether its parents have to be squeezed.
func nameRoom() int { return navWidth - 2 }

// shortenNames cuts the parent directories of a name that will not fit, so
// that the repository's own name survives.
//
// Cutting the name itself is worse than useless: the parents are there only to
// tell two repositories apart, and dropping them leaves two rows reading the
// same — which is the one thing qualifying the name was for. So the parents
// are squeezed to initials instead, and grown back only where that would make
// two names the same again.
func shortenNames(ps []Project, segs [][]string, depth []int) {
	keep := make([]int, len(ps)) // runes kept of each parent; 0 means all
	for i := range ps {
		if depth[i] > 1 && len([]rune(ps[i].Name)) > nameRoom() {
			keep[i] = 1
		}
	}

	for {
		for i := range ps {
			ps[i].Name = shortName(segs[i], depth[i], keep[i])
		}

		groups := map[string][]int{}
		for i := range ps {
			groups[ps[i].Name] = append(groups[ps[i].Name], i)
		}

		grew := false
		for _, idxs := range groups {
			if len(idxs) < 2 {
				continue
			}
			for _, i := range idxs {
				if keep[i] > 0 && keep[i] < widestParent(segs[i], depth[i]) {
					keep[i]++
					grew = true
				}
			}
		}
		if !grew {
			return
		}
	}
}

// shortName joins the last n segments, keeping only the first keep runes of
// each parent. A keep of zero leaves them whole.
func shortName(segs []string, n, keep int) string {
	if keep == 0 {
		return nameAt(segs, n)
	}
	if n > len(segs) {
		n = len(segs)
	}

	parts := make([]string, 0, n)
	tail := segs[len(segs)-n:]
	for _, seg := range tail[:len(tail)-1] {
		if r := []rune(seg); len(r) > keep {
			seg = string(r[:keep])
		}
		parts = append(parts, seg)
	}
	return strings.Join(append(parts, tail[len(tail)-1]), "/")
}

// widestParent is the longest parent in a name, which is as far as squeezing
// them can ever be undone.
func widestParent(segs []string, n int) int {
	if n > len(segs) {
		n = len(segs)
	}
	widest := 0
	for _, seg := range segs[len(segs)-n : len(segs)-1] {
		if r := len([]rune(seg)); r > widest {
			widest = r
		}
	}
	return widest
}

// nameAt joins the last n path segments.
func nameAt(segs []string, n int) string {
	if n > len(segs) {
		n = len(segs)
	}
	return strings.Join(segs[len(segs)-n:], "/")
}

// isRepo reports whether dir is the root of a git repository. .git is a
// directory in a normal clone and a file in a worktree or submodule.
func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// isCacheDir reports whether dir carries a CACHEDIR.TAG, the marker the Cache
// Directory Tagging Specification has tools leave so that scans pass them by.
func isCacheDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "CACHEDIR.TAG"))
	return err == nil
}

// A monorepo is a projects directory that happens to be one repository, and
// the services and apps inside it deserve what repositories get: a row, a
// filter hit, a shell opened in the right place, a plan run there. A
// directory inside a repository is one of those sub-projects when it carries
// a manifest — a plan file of scrn's own, or the file a package manager runs
// the project by.

// manifests are the files that mark a directory as a project of its own.
// Explicit plans first, then the package managers' own files: most projects
// run through one, and the manifest is where that is said.
var manifests = []string{
	".scrn", "Procfile",
	"package.json", "deno.json", "composer.json",
	"go.mod", "Cargo.toml", "Gemfile", "mix.exs",
	"pyproject.toml", "pom.xml", "build.gradle", "build.gradle.kts",
}

// subProjects finds the sub-projects of a repository, sorted by their path
// within it. The repository's own index answers instead of a walk of the
// tree: git ls-files reads what a work-sized checkout would take seconds to
// crawl, in milliseconds, and already knows what is ignored. Untracked files
// are included so a personal, unshared .scrn still marks its directory.
//
// The repository root is not a sub-project of itself: its manifest is what
// the repository's own row already stands for.
func subProjects(repo string) []Project {
	args := []string{"-C", repo, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}
	for _, f := range manifests {
		args = append(args, ":(glob)**/"+f)
	}
	out, err := listing(scanTimeout, "git", args...)
	if err != nil {
		return nil
	}

	seen := map[string]bool{}
	var subs []Project
	for f := range strings.SplitSeq(string(out), "\x00") {
		dir := filepath.Dir(f)
		if f == "" || dir == "." || seen[dir] {
			continue
		}
		seen[dir] = true
		subs = append(subs, Project{
			Name: filepath.ToSlash(dir),
			Path: filepath.Join(repo, dir),
		})
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })
	return subs
}

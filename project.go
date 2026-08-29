package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Project is one git repository found under the configured projects directory.
type Project struct {
	Name string // display name: the repo directory, qualified only if ambiguous
	Path string // absolute path to the repo
}

// skipDirs are never descended into. They hold vendored code, not the projects
// the navigator is for, and walking them dominates the scan.
var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	".build":       true,
	"Pods":         true,
	".venv":        true,
	"venv":         true,
}

// discoverProjects finds every git repository nested under root, at any depth.
// A repository is not descended into, so nested checkouts inside a repo are
// left to the repo that owns them.
func discoverProjects(root string) ([]Project, error) {
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
		if path != root && (skipDirs[name] || strings.HasPrefix(name, ".")) {
			return fs.SkipDir
		}
		if isRepo(path) {
			found = append(found, Project{Path: path})
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
const nameRoom = navWidth - 2

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
		if depth[i] > 1 && len([]rune(ps[i].Name)) > nameRoom {
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

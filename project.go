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

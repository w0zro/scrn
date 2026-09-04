package main

import (
	"os"
	"path/filepath"
	"sort"
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
// model named, and conn is not the one to pick a favorite: the model most
// recently pulled or run is the one already chosen. The models are read
// off the disk, where ollama keeps a manifest per pulled model, rather
// than asked of the daemon: the daemon is often not up until something
// needs it, and a list that needs it would name nothing exactly when a
// fresh session starts. The daemon is asked only when the disk says
// nothing, in case the models live somewhere conn did not think to look.
//
// With no model anywhere, ollama list runs in the shell instead: bare
// ollama run refuses with a message about arguments, and ollama list says
// the true thing — that the daemon is not running, or that nothing has
// been pulled — in a shell that survives it, per the wrapper every run
// gets. The config's agentRuns names an exact model when the newest is
// wrong.
func ollamaRun() string {
	if models := ollamaModelsOnDisk(ollamaModelsDir()); len(models) > 0 {
		return "ollama run " + models[0]
	}
	out, err := listing(scanTimeout, "ollama", "list")
	if model := firstOllamaModel(string(out)); err == nil && model != "" {
		return "ollama run " + model
	}
	return "ollama list"
}

// ollamaModelsDir is where ollama keeps its models: OLLAMA_MODELS when it is
// set, else ~/.ollama/models.
func ollamaModelsDir() string {
	if dir := os.Getenv("OLLAMA_MODELS"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ollama", "models")
}

// ollamaRegistry and ollamaLibrary are the parts of a model's name ollama
// leaves unsaid: qwen2.5-coder:7b is registry.ollama.ai/library/qwen2.5-coder:7b.
const (
	ollamaRegistry = "registry.ollama.ai"
	ollamaLibrary  = "library"
)

// ollamaModelsOnDisk lists the models pulled under dir, newest first, named
// the way ollama run wants them. A pulled model is a manifest file at
// manifests/<registry>/<namespace>/<model>/<tag>; its name is model:tag,
// with the namespace in front of it when it is not the library's and the
// registry in front of that when it is not the default one — the same
// shortening ollama list does. Newest is by the manifest's own time,
// which ollama touches on a pull and on a run, so the order is the one
// ollama list would give.
func ollamaModelsOnDisk(dir string) []string {
	if dir == "" {
		return nil
	}
	type found struct {
		name string
		when int64
	}
	var models []found
	root := filepath.Join(dir, "manifests")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 4 {
			return nil
		}
		registry, namespace, model, tag := parts[0], parts[1], parts[2], parts[3]
		name := model + ":" + tag
		if namespace != ollamaLibrary || registry != ollamaRegistry {
			name = namespace + "/" + name
		}
		if registry != ollamaRegistry {
			name = registry + "/" + name
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		models = append(models, found{name, info.ModTime().UnixNano()})
		return nil
	})
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].when != models[j].when {
			return models[i].when > models[j].when
		}
		return models[i].name < models[j].name
	})
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.name
	}
	return names
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

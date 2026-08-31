package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config is scrn's on-disk configuration.
type Config struct {
	// ProjectsDir is the directory scrn searches for git repositories.
	// It may contain ~ or environment variables such as $HOME.
	ProjectsDir string `json:"projectsDir"`

	// ProjectsDirs is several of them, for work that is not all under one
	// roof — home projects in one place and a work checkout in another. When
	// set it is the whole answer and ProjectsDir is not consulted.
	ProjectsDirs []string `json:"projectsDirs"`

	// Scrollback is how many lines of transcript each shell keeps once they
	// scroll off the pane. Zero means the default of 10000, which a work
	// build log can overrun. The daemon reads it when it starts, so raising
	// it takes a daemon restart (R) to reach the shells.
	Scrollback int `json:"scrollback,omitempty"`

	// SkipDirs are directory names the project walk never enters, on top of
	// the built-in list — for the big generated directories a machine grows
	// that no built-in list can know (bazel-out, dist, an extracted dataset).
	SkipDirs []string `json:"skipDirs,omitempty"`

	// NavWidth is the navigator column's width, for when the default 28 is
	// too tight for the qualified names a work checkout produces. Held
	// between 16 and 60; zero means the default.
	NavWidth int `json:"navWidth,omitempty"`

	// Agent names the kind of agent the a key starts — "claude" unless said
	// otherwise. A name scrn does not know falls back to the default.
	Agent string `json:"agent,omitempty"`

	// AgentRuns overrides what starting a kind runs, by the kind's name —
	// "ollama": "ollama run mistral" picks the model the kind would
	// otherwise guess at.
	AgentRuns map[string]string `json:"agentRuns,omitempty"`
}

func defaultConfig() Config {
	return Config{ProjectsDir: "$HOME/projects"}
}

// roots is every directory to search, expanded. A root that is only on the
// other machine is still listed here: whether it exists is the scan's
// business, not the config's.
func (c Config) roots() []string {
	dirs := c.ProjectsDirs
	if len(dirs) == 0 {
		dirs = []string{c.ProjectsDir}
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		out = append(out, expandPath(d))
	}
	return out
}

// skipSet is the built-in skip list joined with the config's own additions.
func (c Config) skipSet() map[string]bool {
	if len(c.SkipDirs) == 0 {
		return skipDirs
	}
	skip := make(map[string]bool, len(skipDirs)+len(c.SkipDirs))
	for name := range skipDirs {
		skip[name] = true
	}
	for _, name := range c.SkipDirs {
		if name = strings.TrimSpace(name); name != "" {
			skip[name] = true
		}
	}
	return skip
}

// configPath returns the config file location, honoring XDG_CONFIG_HOME.
func configPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "scrn", "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "scrn", "config.json")
}

// loadConfig reads the config file. A missing file is not an error: scrn is
// meant to work before it is configured, so the defaults stand in.
func loadConfig() (Config, error) {
	cfg := defaultConfig()

	b, err := os.ReadFile(configPath())
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return defaultConfig(), err
	}
	if strings.TrimSpace(cfg.ProjectsDir) == "" {
		cfg.ProjectsDir = defaultConfig().ProjectsDir
	}
	return cfg, nil
}

// expandPath resolves ~ and environment variables so a config file stays
// portable between machines.
func expandPath(p string) string {
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

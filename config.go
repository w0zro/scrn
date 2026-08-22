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
}

func defaultConfig() Config {
	return Config{ProjectsDir: "$HOME/projects"}
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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigMissingFileUsesDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("missing config should not error: %v", err)
	}
	if cfg.ProjectsDir != defaultConfig().ProjectsDir {
		t.Errorf("ProjectsDir = %q, want the default %q", cfg.ProjectsDir, defaultConfig().ProjectsDir)
	}
}

func TestLoadConfigReadsProjectsDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `{"projectsDir": "/srv/code"}`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ProjectsDir != "/srv/code" {
		t.Errorf("ProjectsDir = %q, want %q", cfg.ProjectsDir, "/srv/code")
	}
}

func TestLoadConfigReportsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `{"projectsDir":`)

	if _, err := loadConfig(); err == nil {
		t.Error("malformed config should report an error rather than pass silently")
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	t.Setenv("SCRN_TEST_ROOT", "/opt/x")

	for _, tc := range []struct{ in, want string }{
		{"$HOME/projects", filepath.Join(home, "projects")},
		{"~/projects", filepath.Join(home, "projects")},
		{"$SCRN_TEST_ROOT/y", "/opt/x/y"},
		{"/absolute", "/absolute"},
	} {
		if got := expandPath(tc.in); got != tc.want {
			t.Errorf("expandPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, "scrn", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

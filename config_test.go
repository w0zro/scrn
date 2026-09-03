package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLoadConfigReadsScrollback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `{"scrollback": 50000}`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Scrollback != 50000 {
		t.Errorf("Scrollback = %d, want 50000", cfg.Scrollback)
	}
}

func TestLoadConfigReportsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, `{"projectsDir":`)

	_, err := loadConfig()
	if err == nil {
		t.Fatal("malformed config should report an error rather than pass silently")
	}
	if got := err.Error(); !strings.Contains(got, "config.json") || !strings.Contains(got, "line 1") {
		t.Errorf("error = %q, want the file and the line named", got)
	}
}

func TestLoadConfigSaysWhereAValueIsTheWrongKind(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, "{\n  \"scrollback\": 1,\n  \"navWidth\": \"wide\"\n}\n")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("a string where a number goes should be an error")
	}
	if got := err.Error(); !strings.Contains(got, "line 3") {
		t.Errorf("error = %q, want line 3 named", got)
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	t.Setenv("CONN_TEST_ROOT", "/opt/x")

	for _, tc := range []struct{ in, want string }{
		{"$HOME/projects", filepath.Join(home, "projects")},
		{"~/projects", filepath.Join(home, "projects")},
		{"$CONN_TEST_ROOT/y", "/opt/x/y"},
		{"/absolute", "/absolute"},
	} {
		if got := expandPath(tc.in); got != tc.want {
			t.Errorf("expandPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, "conn", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRootsPreferProjectsDirs(t *testing.T) {
	cfg := Config{ProjectsDir: "/ignored", ProjectsDirs: []string{"/a", "/b"}}
	got := cfg.roots()
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("roots = %v, want projectsDirs to be the whole answer", got)
	}
}

func TestRootsFallBackToProjectsDir(t *testing.T) {
	cfg := Config{ProjectsDir: "/solo"}
	if got := cfg.roots(); len(got) != 1 || got[0] != "/solo" {
		t.Errorf("roots = %v, want the one projectsDir", got)
	}
}

func TestRootsExpandAndSkipBlanks(t *testing.T) {
	t.Setenv("CONN_TEST_ROOT", "/opt/x")
	cfg := Config{ProjectsDirs: []string{"$CONN_TEST_ROOT/code", "  ", ""}}
	if got := cfg.roots(); len(got) != 1 || got[0] != "/opt/x/code" {
		t.Errorf("roots = %v, want the blank entries dropped and the rest expanded", got)
	}
}

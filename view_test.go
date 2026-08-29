package main

import "testing"

func TestACommandLineReadsAsWhatWasRun(t *testing.T) {
	cases := map[string]string{
		// The thing you would call it, not the binary that happens to run it.
		"/opt/homebrew/bin/node /opt/homebrew/bin/npm run dev": "npm run dev",
		"node server.js --port 3000":                           "server.js --port 3000",
		"go run .":                                             "go run .",
		"caffeinate -i -t 300":                                 "caffeinate -i -t 300",
		"/usr/bin/python3 -m http.server 8931":                 "python3 -m http.server 8931",
		"/opt/homebrew/Caskroom/claude-code/2.1.231/claude":    "claude",
	}
	for argv, want := range cases {
		if got := shortArgv(argv); got != want {
			t.Errorf("shortArgv(%q) = %q, want %q", argv, got, want)
		}
	}
}

func TestAShellRunningAScriptIsJustAShell(t *testing.T) {
	// A script is somebody's idea of a command line rather than a command: it
	// is long, it starts with its own setup, and cut to fit it leaves a
	// fragment of a path. A bare shell says less but is at least true.
	for _, argv := range []string{
		"zsh -c cd /tmp/claude-ec64-cwd && exec zsh",
		"/bin/zsh -c source ~/.claude/shell-snapshots/snapshot.sh && eval 'ls'",
		"-zsh",
	} {
		if got := shortArgv(argv); got != "" {
			t.Errorf("shortArgv(%q) = %q, want the name used instead", argv, got)
		}
	}
}

func TestAProcessWithNoCommandLineKeepsItsName(t *testing.T) {
	n := &ProcNode{Proc: Proc{Command: "kernel_task"}}
	if got := commandOf(n); got != "kernel_task" {
		t.Errorf("commandOf = %q, want the name when there is nothing else", got)
	}
}

func TestACommandIsCutFromTheEnd(t *testing.T) {
	// What identifies a command is at the front; what identifies a repository
	// is at the back.
	if got := truncateTail("go test -count=1 -timeout 180s", 12); got != "go test -co…" {
		t.Errorf("truncateTail = %q, want the start kept", got)
	}
	if got := truncate("w0zro/archive/scrn", 12); got != "…rchive/scrn" {
		t.Errorf("truncate = %q, want the end kept", got)
	}
}

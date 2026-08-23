package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A running Claude Code instance says far more about what is happening in a
// repository than "claude" and a pid do. It leaves two things behind that scrn
// can read without asking it anything:
//
//   - <claude>/sessions/<pid>.json, a small file keyed by process id, holding
//     the session's name and whether it is working right now. Small enough to
//     re-read on every process scan, which is what lets the navigator mark the
//     busy instances without the cursor having to visit them.
//   - <claude>/projects/<encoded cwd>/<session id>.jsonl, the transcript. It
//     is megabytes, so only its tail is read, and only for the row the cursor
//     is on.

// claudeSession is what scrn knows about one running Claude Code instance.
type claudeSession struct {
	PID       int
	Name      string
	Status    string
	StatusFor time.Duration
	SessionID string
	Cwd       string
	Version   string

	// Filled in from the transcript, which is only read for the selected row.
	Model   string
	Branch  string
	Summary string
	Prompt  string
	Context int
}

// claudeDir is where Claude Code keeps its state.
func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// sessionFile is the on-disk shape of <claude>/sessions/<pid>.json. Only the
// fields scrn shows are named; the rest of the file is left alone.
type sessionFile struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
	Version         string `json:"version"`
}

// claudeSessions reads every live session Claude Code has advertised, keyed by
// the process id running it.
//
// A session file can outlive the process that wrote it, so callers pair this
// with the command name before believing a process is a Claude instance.
func claudeSessions() map[int]claudeSession {
	entries, err := os.ReadDir(filepath.Join(claudeDir(), "sessions"))
	if err != nil {
		return nil
	}

	out := make(map[int]claudeSession, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		// Only <pid>.json: the directory also holds per-session key files.
		if _, err := strconv.Atoi(strings.TrimSuffix(name, ".json")); err != nil {
			continue
		}
		s, ok := readSessionFile(filepath.Join(claudeDir(), "sessions", name))
		if ok {
			out[s.PID] = s
		}
	}
	return out
}

func readSessionFile(path string) (claudeSession, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return claudeSession{}, false
	}
	var f sessionFile
	if err := json.Unmarshal(b, &f); err != nil || f.PID == 0 {
		return claudeSession{}, false
	}

	s := claudeSession{
		PID:       f.PID,
		Name:      f.Name,
		Status:    f.Status,
		SessionID: f.SessionID,
		Cwd:       f.Cwd,
		Version:   f.Version,
	}
	if f.StatusUpdatedAt > 0 {
		s.StatusFor = time.Since(time.UnixMilli(f.StatusUpdatedAt))
		if s.StatusFor < 0 {
			s.StatusFor = 0
		}
	}
	return s, true
}

// encodePath is how Claude Code names a project's transcript directory: every
// character that is not a letter or digit becomes a dash.
var notAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

func encodePath(p string) string { return notAlnum.ReplaceAllString(p, "-") }

// transcriptPath locates a session's transcript. The encoded working directory
// finds it directly; the search is for the case where the directory has been
// renamed since, which leaves the transcript filed under the old name.
func transcriptPath(s claudeSession) string {
	if s.SessionID == "" {
		return ""
	}
	root := filepath.Join(claudeDir(), "projects")

	direct := filepath.Join(root, encodePath(s.Cwd), s.SessionID+".jsonl")
	if _, err := os.Stat(direct); err == nil {
		return direct
	}

	found, _ := filepath.Glob(filepath.Join(root, "*", s.SessionID+".jsonl"))
	if len(found) > 0 {
		return found[0]
	}
	return ""
}

// transcriptTail is how much of the end of a transcript is read. A turn can
// carry a lot of tool output, so the last prompt may be some way back, but the
// whole file is megabytes and is re-read while the cursor sits on the row.
const transcriptTail = 512 * 1024

// promptLimit and summaryLimit keep long prose from filling the pane and
// pushing the rest of the fields off the bottom. The summary is given more
// room because saying what a session is doing takes a sentence or two.
const (
	promptLimit  = 160
	summaryLimit = 240
)

// transcriptLine is the part of a transcript record scrn reads.
type transcriptLine struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	GitBranch   string `json:"gitBranch"`

	// Claude Code writes what it is doing as records of its own, rather than
	// leaving it to be inferred from the conversation. Content is the body of
	// a system record and is only a string on the ones scrn reads, so it is
	// held raw until it is wanted.
	LastPrompt string          `json:"lastPrompt"`
	Content    json.RawMessage `json:"content"`

	Message struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   struct {
			Input         int `json:"input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// readTranscript fills in what only the transcript knows: the model in use, the
// branch the session started on, how much context the last request carried,
// what the session says it is doing, and the last thing the user asked for.
//
// It reads backwards from the end and stops as soon as every field is filled,
// because the most recent record is the one that matters for all of them.
func readTranscript(path string, s *claudeSession) {
	lines, err := tailLines(path, transcriptTail)
	if err != nil {
		return
	}

	for i := len(lines) - 1; i >= 0; i-- {
		var rec transcriptLine
		if err := json.Unmarshal(lines[i], &rec); err != nil {
			continue
		}
		// A sidechain is a subagent's conversation, not the session's own.
		if rec.IsSidechain {
			continue
		}
		if s.Branch == "" {
			s.Branch = rec.GitBranch
		}
		if s.Model == "" && rec.Message.Model != "" {
			s.Model = rec.Message.Model
		}
		if s.Context == 0 {
			u := rec.Message.Usage
			s.Context = u.Input + u.CacheRead + u.CacheCreation
		}
		if s.Summary == "" && rec.Type == "system" && rec.Subtype == "away_summary" {
			s.Summary = plainText(rec.Content, summaryLimit)
		}
		// Claude Code records the last prompt itself. Reading a user record is
		// the fallback for a window that does not reach one of those.
		if s.Prompt == "" && rec.Type == "last-prompt" {
			s.Prompt = tidy(rec.LastPrompt, promptLimit)
		}
		if s.Prompt == "" && rec.Type == "user" && !rec.IsMeta {
			s.Prompt = userPrompt(rec.Message.Content)
		}
		if s.Branch != "" && s.Model != "" && s.Context != 0 && s.Prompt != "" && s.Summary != "" {
			return
		}
	}
}

// plainText reads a record body that is expected to be a string.
func plainText(raw json.RawMessage, limit int) string {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return ""
	}
	return tidy(text, limit)
}

// tidy flattens prose onto one line and cuts it to fit the pane.
func tidy(s string, limit int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return truncateRunes(strings.Join(strings.Fields(s), " "), limit)
}

// userPrompt returns the text of a user record, or "" if it is not something
// the user typed. Tool results arrive as a list rather than a string, and the
// harness delivers reminders and command output as text wrapped in tags.
func userPrompt(content json.RawMessage) string {
	var text string
	if err := json.Unmarshal(content, &text); err != nil {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "<") || strings.HasPrefix(text, "Caveat:") {
		return ""
	}
	return tidy(text, promptLimit)
}

// tailLines returns the complete lines in the last max bytes of a file. The
// first line read is dropped unless the read started at the beginning, because
// it is whatever the seek landed in the middle of.
func tailLines(path string, max int64) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	from := info.Size() - max
	if from < 0 {
		from = 0
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, err
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(b, []byte("\n"))
	if from > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	return lines, nil
}

// claudeFields describes a Claude Code instance, most useful first: what the
// session is called, whether it is working, and what it was last asked to do.
func claudeFields(s claudeSession) []field {
	fs := []field{}
	if s.Name != "" {
		fs = append(fs, field{"session", s.Name})
	}
	if s.Status != "" {
		status := s.Status
		if s.StatusFor > 0 {
			status += "  (" + shortDuration(s.StatusFor) + ")"
		}
		fs = append(fs, field{"status", status})
	}
	if s.Summary != "" {
		fs = append(fs, field{"summary", s.Summary})
	}
	if s.Model != "" {
		fs = append(fs, field{"model", s.Model})
	}
	if s.Context > 0 {
		fs = append(fs, field{"context", shortTokens(s.Context) + " tokens"})
	}
	if s.Prompt != "" {
		fs = append(fs, field{"asked", s.Prompt})
	}
	if s.Branch != "" {
		fs = append(fs, field{"branch", s.Branch})
	}
	if s.SessionID != "" {
		fs = append(fs, field{"session id", s.SessionID})
	}
	return fs
}

// shortDuration is a duration at a glance: one unit, no decimals.
func shortDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
	}
}

// shortTokens rounds a token count to something readable in a narrow pane.
func shortTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.Itoa(n/1_000) + "k"
	default:
		return strconv.Itoa(n)
	}
}

// truncateRunes shortens s without splitting a rune.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A running Claude Code instance says far more about what is happening in a
// repository than "claude" and a pid do. It leaves two things behind that conn
// can read without asking it anything:
//
//   - <claude>/sessions/<pid>.json, a small file keyed by process id, holding
//     the session's name and whether it is working right now. Small enough to
//     re-read on every process scan, which is what lets the navigator mark the
//     busy instances without the cursor having to visit them.
//   - <claude>/projects/<encoded cwd>/<session id>.jsonl, the transcript. It
//     is megabytes, so only its tail is read, and only for the row the cursor
//     is on.

// claudeSession is what conn knows about one running Claude Code instance.
type claudeSession struct {
	PID        int
	Name       string
	Status     string
	StatusFor  time.Duration
	WaitingFor string
	SessionID  string
	Cwd        string

	// Agents are the subagents this session has started and not yet heard back
	// from. A subagent is not a process — it runs inside the instance that
	// started it — so the transcript is the only place it shows at all.
	Agents []agentRun

	// Filled in from the transcript, which is only read for the selected row.
	Model   string
	Branch  string
	Summary string
	Prompt  string
	Context int
}

// busyStatus is what Claude Code calls a session that is working. Anything
// else is waiting on its user, whatever it is called.
const busyStatus = "busy"

// waitingStatus is what Claude Code calls a session stopped mid-turn on a
// specific ask. It comes with waitingFor, a few words naming the ask — a
// permission prompt, a question, a dialog.
const waitingStatus = "waiting"

// claudeCommand starts a Claude Code instance. It is run through a shell, so
// this is the name on the PATH rather than a path conn has to find.
const claudeCommand = "claude"

// claudeKind is Claude Code as a kind of agent — the first, and so the one
// the a key starts.
var claudeKind = agentKind{
	name:      "claude",
	command:   claudeCommand,
	run:       func() string { return claudeCommand },
	scan:      func() map[int]agent { return asAgents(claudeSessions()) },
	suspended: claudeSuspended,
	resume:    claudeResume,
}

// asAgents lifts Claude's sessions to the shape every kind is read through.
func asAgents(sessions map[int]claudeSession) map[int]agent {
	out := make(map[int]agent, len(sessions))
	for pid, s := range sessions {
		out[pid] = s
	}
	return out
}

func (s claudeSession) command() string { return claudeCommand }

func (s claudeSession) id() string { return s.SessionID }

func (s claudeSession) model() string { return s.Model }

func (s claudeSession) working() bool { return s.Status == busyStatus }

func (s claudeSession) blocked() (string, bool) {
	return s.WaitingFor, s.Status == waitingStatus
}

// describe reads the transcript into a copy of the session — the deeper look
// the scan does not take — and lays the whole of it out for the pane.
func (s claudeSession) describe() []field {
	readTranscript(transcriptPath(s), &s)
	return claudeFields(s)
}

// agentRun is one subagent, as its parent described it when starting it.
type agentRun struct {
	Description string
	Type        string
}

func (a agentRun) String() string {
	if a.Type == "" {
		return a.Description
	}
	return a.Description + "  (" + a.Type + ")"
}

// claudeDir is where Claude Code keeps its state.
func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home is no sessions, rather than a .claude wherever conn
		// happens to have been started.
		return ""
	}
	return filepath.Join(home, ".claude")
}

// sessionFile is the on-disk shape of <claude>/sessions/<pid>.json. Only the
// fields conn shows are named; the rest of the file is left alone.
type sessionFile struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
	WaitingFor      string `json:"waitingFor"`
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
			// The model the navigator's row names. It is the one thing the
			// scan takes from the transcript, and only because a session's
			// model holds steady enough to read once and keep.
			s.Model = sessionModel(s)
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
		PID:        f.PID,
		Name:       f.Name,
		Status:     f.Status,
		WaitingFor: f.WaitingFor,
		SessionID:  f.SessionID,
		Cwd:        f.Cwd,
	}
	if f.StatusUpdatedAt > 0 {
		s.StatusFor = max(time.Since(time.UnixMilli(f.StatusUpdatedAt)), 0)
	}
	return s, true
}

// modelCache keeps each session's model by id. The model a running instance
// is using is in its transcript, not its small session file, and the
// transcript is the read the scan is built to skip. A session's model holds
// steady, so it is read once — the tail, the light read the picker uses —
// and kept by session id; a switch mid-session is the detail pane's to show
// live, off its own full read.
var (
	modelCacheMu sync.Mutex
	modelCache   = map[string]string{}
)

// sessionModel is the model a running session is using, read once from its
// transcript and cached by session id. It is empty until the transcript has
// a turn to read the model from, and is not cached until it does, so a
// session that has not spoken yet is asked again rather than remembered as
// nothing.
func sessionModel(s claudeSession) string {
	if s.SessionID == "" {
		return ""
	}
	modelCacheMu.Lock()
	m, ok := modelCache[s.SessionID]
	modelCacheMu.Unlock()
	if ok {
		return m
	}
	m = readModel(transcriptPath(s))
	if m != "" {
		modelCacheMu.Lock()
		modelCache[s.SessionID] = m
		modelCacheMu.Unlock()
	}
	return m
}

// readModel reads a transcript's tail for the model of its most recent turn.
// It is readTranscript's model alone, for the scan rather than the pane:
// the row names the model, and that one field is all it wants.
func readModel(path string) string {
	lines, err := tailLines(path, convoTail)
	if err != nil {
		return ""
	}
	for _, line := range slices.Backward(lines) {
		var rec transcriptLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.IsSidechain {
			continue
		}
		if rec.Message.Model != "" {
			return rec.Message.Model
		}
	}
	return ""
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

// A transcript can also be at rest: the instance that was having the
// conversation has exited, and the file is what is left of it. Those are the
// conversations the resume picker lists — claude can be told to pick any of
// them back up by its session id, which is the transcript's own file name.

// claudeResume is the command that continues a suspended conversation. The id
// travels onto a shell command line, so only ids claudeSuspended vetted are
// ever handed here.
func claudeResume(id string) string { return claudeCommand + " --resume " + id }

// convoTail is how much of a transcript's end is read for the picker: enough
// to reach back past a tool-heavy turn to the last prompt, small enough that
// a directory of them is read on a keystroke.
const convoTail = 256 * 1024

// claudeSuspended lists the conversations at rest under the given
// directories, newest first. live names the ones running instances are
// carrying, which are not at rest whatever their files say.
func claudeSuspended(dirs []string, live map[string]bool) []conversation {
	root := filepath.Join(claudeDir(), "projects")

	// Claude encodes directories lossily, so two of them can share a
	// transcript directory; each conversation is taken once, for the first
	// directory that reached it.
	seen := map[string]bool{}
	var out []conversation
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(root, encodePath(dir)))
		if err != nil {
			continue
		}
		for _, e := range entries {
			id := strings.TrimSuffix(e.Name(), ".jsonl")
			if e.IsDir() || id == e.Name() || !isSessionID(id) || live[id] || seen[id] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			seen[id] = true
			c := conversation{ID: id, Dir: dir, When: info.ModTime()}
			readConvoMeta(filepath.Join(root, encodePath(dir), e.Name()), &c)
			out = append(out, c)
		}
	}
	slices.SortFunc(out, byRecency)
	return out
}

// isSessionID reports whether a transcript's stem is shaped like the ids
// Claude writes — hex and dashes. The id ends up on a shell command line, so
// anything else in the directory is not a session, whatever it is.
func isSessionID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r == '-':
		default:
			return false
		}
	}
	return true
}

// readConvoMeta fills in what a reader recognizes a conversation by: the
// branch it was on, the last thing asked of it, and what it said it was doing
// if it said. It is readTranscript's lighter sibling — many files are read on
// one keystroke, and the model, the context and the subagents are questions
// about a conversation that is still going.
func readConvoMeta(path string, c *conversation) {
	lines, err := tailLines(path, convoTail)
	if err != nil {
		return
	}
	for _, line := range slices.Backward(lines) {
		var rec transcriptLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.IsSidechain {
			continue
		}
		if c.Branch == "" {
			c.Branch = rec.GitBranch
		}
		if c.Summary == "" && rec.Type == "system" && rec.Subtype == "away_summary" {
			c.Summary = plainText(rec.Content, summaryLimit)
		}
		if c.Prompt == "" && rec.Type == "last-prompt" {
			c.Prompt = tidy(rec.LastPrompt, promptLimit)
		}
		if c.Prompt == "" && rec.Type == "user" && !rec.IsMeta {
			c.Prompt = userPrompt(rec.Message.Content)
		}
		if c.Branch != "" && c.Prompt != "" && c.Summary != "" {
			return
		}
	}
}

// shortAge is how long ago at a glance: one unit, none of them finer than the
// question "which conversation was that" needs.
func shortAge(when time.Time) string {
	d := time.Since(when)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	case d < 14*24*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	default:
		return strconv.Itoa(int(d.Hours()/(24*7))) + "w"
	}
}

// promptLimit and summaryLimit keep long prose from filling the pane and
// pushing the rest of the fields off the bottom. The summary is given more
// room because saying what a session is doing takes a sentence or two.
const (
	promptLimit  = 160
	summaryLimit = 240
)

// A subagent is not a process and has no session file, but it is not hidden
// either: it gets a transcript of its own beside its parent's, and a small
// file saying what it was started to do.
//
//	projects/<encoded cwd>/<session id>/subagents/agent-<id>.jsonl
//	                                             agent-<id>.meta.json
//
// The meta file says what an agent is. Whether it is still going is only in
// the parent's transcript: a foreground agent is finished when its tool call
// is answered, and a background one is answered the moment it starts, so that
// one is finished when its completion is recorded instead. Both are keyed by
// the tool call that started them, which the meta file names.

// agentTool is the tool that starts a subagent.
const agentTool = "Agent"

// agentMeta is what Claude Code writes beside a subagent's transcript.
type agentMeta struct {
	Type        string `json:"agentType"`
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
}

// contentBlock is a piece of a message, which is where a tool call and its
// answer are recorded.
type contentBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	Input     struct {
		RunInBackground bool `json:"run_in_background"`
	} `json:"input"`
}

// agentProgress is what the parent's transcript says about the calls that
// started its subagents. Which of these means finished depends on the call: a
// foreground agent is waited for, so its result arriving is the end of it,
// while a background one is answered the moment it starts and its finishing is
// recorded on its own.
type agentProgress struct {
	answered   map[string]bool // a result came back
	completed  map[string]bool // a finish was recorded
	background map[string]bool // the call did not wait
}

func newAgentProgress() *agentProgress {
	return &agentProgress{
		answered:   map[string]bool{},
		completed:  map[string]bool{},
		background: map[string]bool{},
	}
}

// done reports whether the agent started by a call has finished.
func (p *agentProgress) done(toolUseID string) bool {
	if p.completed[toolUseID] {
		return true
	}
	return !p.background[toolUseID] && p.answered[toolUseID]
}

// agentStale is how long after a subagent last wrote anything it stops being
// treated as one that might still be going. The parent's transcript is only
// read in a window, so a completion old enough to have fallen out of it would
// otherwise leave the agent listed forever.
const agentStale = 10 * time.Minute

// transcriptLine is the part of a transcript record conn reads.
type transcriptLine struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	GitBranch   string `json:"gitBranch"`

	// Claude Code writes what it is doing as records of its own, rather than
	// leaving it to be inferred from the conversation. Content is the body of
	// a system record and is only a string on the ones conn reads, so it is
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
// It reads backwards from the end, because for every one of those the most
// recent record is the one that matters, and takes the first answer it finds.
//
// It reads the whole window even so, rather than stopping once it has them.
// The subagents are the reason: which record means an agent has finished
// depends on how it was started, and the call that says so is older than
// everything answering it — so an early exit would leave the last few agents
// weighed against a question that had not been reached yet.
func readTranscript(path string, s *claudeSession) {
	lines, err := tailLines(path, transcriptTail)
	if err != nil {
		return
	}

	// What the transcript says about the subagents is gathered as it goes, and
	// weighed once the whole window has been read: which record means finished
	// depends on how the agent was started, and that is said by the call
	// itself, which is older than everything answering it.
	progress := newAgentProgress()

	for _, line := range slices.Backward(lines) {
		var rec transcriptLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		// A sidechain is a subagent's conversation, not the session's own.
		if rec.IsSidechain {
			continue
		}
		progress.read(line, rec)
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
	}
	readAgents(s, progress)
}

// read notes what one record says about the calls that started subagents.
func (p *agentProgress) read(line []byte, rec transcriptLine) {
	var blocks []contentBlock
	if err := json.Unmarshal(rec.Message.Content, &blocks); err == nil {
		for _, b := range blocks {
			switch {
			case b.Type == "tool_result" && b.ToolUseID != "":
				p.answered[b.ToolUseID] = true
			case b.Type == "tool_use" && b.Name == agentTool && b.ID != "":
				p.background[b.ID] = b.Input.RunInBackground
			}
		}
	}

	// A finish is not a message, so it is read from the line rather than
	// through the message shape.
	if bytes.Contains(line, []byte("<status>completed</status>")) {
		for _, m := range toolUseID.FindAllSubmatch(line, -1) {
			p.completed[string(m[1])] = true
		}
	}
}

// toolUseID pulls the tool calls a completion record refers to.
var toolUseID = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)

// readAgents lists the subagents of a session that have not finished.
func readAgents(s *claudeSession, progress *agentProgress) {
	dir := filepath.Join(strings.TrimSuffix(transcriptPath(*s), ".jsonl"), "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".meta.json") {
			continue
		}

		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var meta agentMeta
		if err := json.Unmarshal(b, &meta); err != nil || progress.done(meta.ToolUseID) {
			continue
		}

		// An agent that stopped writing long ago finished long ago, whether
		// or not the saying so is still in the window.
		body := filepath.Join(dir, strings.TrimSuffix(name, ".meta.json")+".jsonl")
		if info, err := os.Stat(body); err != nil || time.Since(info.ModTime()) > agentStale {
			continue
		}
		s.Agents = append(s.Agents, agentRun{Description: meta.Description, Type: meta.Type})
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
	defer func() { _ = f.Close() }()

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
	// What it is, then what it is doing, then what it is doing it with. The
	// prose is given a group of its own because it is the part worth reading
	// and the part that wraps.
	var what, doing, with []field
	add := func(fs *[]field, label, value string, tones ...tone) {
		if value == "" {
			return
		}
		f := field{label: label, value: value}
		if len(tones) > 0 {
			f.tone = tones[0]
		}
		*fs = append(*fs, f)
	}

	add(&what, "session", s.Name)
	if s.Status != "" {
		status := s.Status
		// The status reads in the color its mark wears in the navigator:
		// working is alive, blocked is the answer holding up work, and idle
		// recedes. A blocked session says what it is blocked on, which is
		// the part worth reading: "waiting" alone would send you to the pane
		// to find out, and the pane is where you already are.
		t := toneQuiet
		switch s.Status {
		case busyStatus:
			t = toneGood
		case waitingStatus:
			t = toneUrgent
			if s.WaitingFor != "" {
				status = "waiting on " + s.WaitingFor
			}
		}
		if s.StatusFor > 0 {
			status += "  (" + shortDuration(s.StatusFor) + ")"
		}
		add(&what, "status", status, t)
	}
	add(&what, "branch", s.Branch, toneAccent)

	add(&doing, "summary", s.Summary)
	add(&doing, "asked", s.Prompt)
	for i, a := range s.Agents {
		label := "agents"
		if i > 0 {
			label = "" // the rest line up under the first
		}
		add(&doing, label, a.String())
	}

	add(&with, "model", s.Model)
	if s.Context > 0 {
		add(&with, "context", shortTokens(s.Context)+" tokens")
	}
	add(&with, "session id", s.SessionID, toneQuiet)

	var fs []field
	for _, group := range [][]field{what, doing, with} {
		if len(group) == 0 {
			continue
		}
		fs = append(fs, gap())
		fs = append(fs, group...)
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

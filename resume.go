package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// A conversation an agent was having does not end when its instance exits;
// it is suspended, and the transcript on disk is enough to pick it back up.
// The picker lists a place's suspended conversations in the pane — newest
// first, searchable the way the filter searches — and enter continues the
// one under the cursor, in a shell like any other. A on a place opens it;
// as a chord it reaches back from wherever the keys are.

// resumeView is the picker while it is open: whose conversations, the
// listing once it lands, and the look narrowing it.
type resumeView struct {
	place  Project
	loaded bool           // the listing takes a beat; "empty" waits for it
	convos []conversation // newest first, as the kind listed them
	query  string
	cursor int // indexes into matches(), not convos
}

// convosMsg carries the listing for the place the picker was opened on. The
// path identifies the ask, so a listing that lands after the picker moved on
// is dropped.
type convosMsg struct {
	place  string
	convos []conversation
}

// openResume opens the picker for the selected row's place.
func (m *model) openResume() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	return m.openResumePlace(r.project)
}

// resumeHere opens the picker for the place the keys are in: the focused
// shell's place when one is taking them, else the selected row's — the same
// reach the other chords have.
func (m *model) resumeHere() tea.Cmd {
	t := m.focused()
	if t == nil {
		return m.openResume()
	}
	p, ok := m.placeAt(t.dir)
	if !ok {
		m.status, m.statusErr = "no project holds "+t.dir, true
		return nil
	}
	// The picker takes the pane and the keys, so the shell gives them up;
	// the cursor lands on the place whose conversations are being offered.
	m.scroll = nil
	m.setFocus(0)
	m.selectProject(p.Path)
	return m.openResumePlace(p)
}

// openResumePlace opens the picker and asks for the listing off the render
// path: it is dozens of files, and the pane says "looking" until they land.
func (m *model) openResumePlace(p Project) tea.Cmd {
	kind := agentKinds[0]
	dirs := m.convoDirs(p)
	live := m.liveConversations()
	m.resume = &resumeView{place: p}
	return func() tea.Msg {
		return convosMsg{place: p.Path, convos: kind.suspended(dirs, live)}
	}
}

// convoDirs is every directory a place's conversations could be filed under:
// its own, its sub-projects', and for a group each repository's in turn —
// a transcript is filed by the exact directory the conversation was had in.
func (m model) convoDirs(p Project) []string {
	dirs := []string{p.Path}
	for _, sp := range m.subs[p.Path] {
		dirs = append(dirs, sp.Path)
	}
	for _, rp := range m.grouped[p.Path] {
		dirs = append(dirs, rp.Path)
		for _, sp := range m.subs[rp.Path] {
			dirs = append(dirs, sp.Path)
		}
	}
	return dirs
}

// liveConversations is the id of every conversation a running instance is
// carrying — vetted against the process table the way agentFor vets, so a
// session file outliving its process does not hide the conversation it
// left behind.
func (m model) liveConversations() map[string]bool {
	live := map[string]bool{}
	for pid, a := range m.agents {
		n := m.nodes[pid]
		if n == nil || n.Command != a.command() {
			continue
		}
		if id := a.id(); id != "" {
			live[id] = true
		}
	}
	return live
}

// resumeKey handles a keystroke while the picker is open. It keeps the
// filter's grammar: letters type, the list narrows under a movable cursor,
// enter acts on what the cursor is on, and esc abandons the look.
func (m *model) resumeKey(msg tea.KeyPressMsg) tea.Cmd {
	v := m.resume
	switch msg.String() {
	case "enter":
		return m.resumePick()
	case "up", "ctrl+p":
		m.resumeMove(-1)
		return nil
	case "down", "ctrl+n":
		m.resumeMove(1)
		return nil
	case "esc":
		m.resume = nil
		return m.detailCmd()
	case "backspace":
		if r := []rune(v.query); len(r) > 0 {
			m.setResumeQuery(string(r[:len(r)-1]))
		}
		return nil
	case "ctrl+u":
		m.setResumeQuery("")
		return nil
	case "space":
		m.setResumeQuery(v.query + " ")
		return nil
	}
	if msg.Text != "" {
		m.status = ""
		m.setResumeQuery(v.query + msg.Text)
	}
	return nil
}

// setResumeQuery narrows the listing and starts the cursor over — unless
// nothing was narrowed, the same call setFilter makes: a trailing space
// changes no row, so it must not move the cursor either.
func (m *model) setResumeQuery(s string) {
	v := m.resume
	narrowed := !strings.EqualFold(strings.TrimSpace(v.query), strings.TrimSpace(s))
	v.query = s
	if narrowed {
		v.cursor = 0
	}
}

// resumeMove steps the cursor through what answers, wrapping at both ends
// the way the navigator's cursor does.
func (m *model) resumeMove(delta int) {
	v := m.resume
	n := len(v.matches())
	if n == 0 {
		return
	}
	v.cursor = (min(v.cursor, n-1) + delta + n) % n
}

// resumePick continues the conversation under the cursor: a shell opens in
// the directory the conversation was had in, running the command that picks
// it back up. From there it is a live instance like any other — marked in
// the navigator, killable, steppable-into.
func (m *model) resumePick() tea.Cmd {
	list := m.resume.matches()
	if len(list) == 0 {
		return nil
	}
	if m.daemon == nil {
		m.status, m.statusErr = "no daemon to hold it: "+m.daemonErr, true
		return nil
	}
	c := list[min(m.resume.cursor, len(list)-1)]
	m.resume = nil
	m.daemon.open(c.Dir, agentKinds[0].resume(c.ID), "", m.detailWidth(), m.paneHeight())
	return nil
}

// matches is the conversations that answer the query, by what a reader
// recognizes one by: the prompt, the summary, the branch, or where in the
// place it was had. An empty query is the whole listing.
func (v *resumeView) matches() []conversation {
	f := strings.ToLower(strings.TrimSpace(v.query))
	if f == "" {
		return v.convos
	}
	var out []conversation
	for _, c := range v.convos {
		if strings.Contains(strings.ToLower(c.Prompt), f) ||
			strings.Contains(strings.ToLower(c.Summary), f) ||
			strings.Contains(strings.ToLower(c.Branch), f) ||
			strings.Contains(strings.ToLower(c.Dir), f) {
			out = append(out, c)
		}
	}
	return out
}

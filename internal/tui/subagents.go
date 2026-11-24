package tui

import (
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// Claude Code subagents as rows of their own, nested └ under the session
// running them, as Codex subagents and codex exec runs are. Listed while
// alive (not finished, started by the session's current claude process:
// transcript.SubagentAlive) and for closedCooldown after one finishes,
// marked closed, then dropped: a busy session runs dozens a day, and its
// ◆N count and Subagents lines still cover every one. Only a live session's
// are looked at, so nothing is read for the thousands of finished ones.

// subagentRowsKey holds sessions.LiveReads while a tick reads subagent rows,
// so one stuck on a hung mount isn't started again.
const subagentRowsKey = "\x00subagent-rows"

// procStartOf parses a session file's procStart (time.ANSIC, spaces evened).
func procStartOf(start string) time.Time {
	t, _ := time.Parse(time.ANSIC, strings.Join(strings.Fields(start), " "))
	return t
}

// listedSubagent: s is shown as a row under a session whose claude process
// started at since.
func listedSubagent(s transcript.Subagent, since, now time.Time) bool {
	if s.Started.Before(since) { // an earlier process's: it ended with it
		return false
	}
	return !s.Finished || now.Sub(s.LastWrite) < closedCooldown
}

// subagentRowsFor is the subagent rows of parents (live Claude sessions),
// each read through transcript.SubagentConv from prev, its last read, so an
// unchanged transcript isn't read again and a growing one only from where
// it was. transcript.SubagentsOf's cache (subagentsTTL) is shared with the
// ◆ count, so the listing costs no extra globbing. Off the UI goroutine
// (liveCmd). subs is each listed one's meta, by row id.
func subagentRowsFor(parents []transcript.Conversation, starts map[string]time.Time, prev map[string]transcript.Conversation, now time.Time) (rows []transcript.Conversation, subs map[string]transcript.Subagent) {
	subs = map[string]transcript.Subagent{}
	for _, p := range parents {
		for _, s := range transcript.SubagentsOf(p) {
			if !listedSubagent(s, starts[p.SessionID], now) {
				continue
			}
			var last *transcript.Conversation
			if c, ok := prev[s.ID]; ok {
				last = &c
			}
			c, err := transcript.SubagentConv(last, p, s)
			if err != nil || c == nil {
				continue
			}
			rows = append(rows, *c)
			subs[s.ID] = s
		}
	}
	return rows, subs
}

// subagentParents is what liveCmd hands subagentRowsFor, from byID (its
// copy of the list): the live Claude sessions (headless ones only with
// --all), their claude process's start, and the subagent rows as last read.
func subagentParents(byID map[string]transcript.Conversation, live map[string]bool, starts map[string]string) ([]transcript.Conversation, map[string]time.Time, map[string]transcript.Conversation) {
	var parents []transcript.Conversation
	since := map[string]time.Time{}
	prev := map[string]transcript.Conversation{}
	for _, c := range byID {
		switch {
		case c.Kind == transcript.KindSubagent:
			prev[c.SessionID] = c
		case c.Kind == "" && live[c.SessionID] && (showHeadless || !c.Headless()):
			parents = append(parents, c)
			since[c.SessionID] = procStartOf(starts[c.SessionID])
		}
	}
	return parents, since, prev
}

// applySubagents brings the subagent rows in m.items up to rows: a set that
// changed (one started, or dropped) is rebuilt and the list filtered and
// sorted again, keeping the selection and its scroll; otherwise the rows
// whose transcript grew are returned for applyLive, as a live session's are.
func (m *model) applySubagents(rows []transcript.Conversation, subs map[string]transcript.Subagent) []transcript.Conversation {
	m.subagents = subs
	have := map[string]int64{}
	for _, it := range m.items {
		if it.conv.Kind == transcript.KindSubagent {
			have[it.conv.SessionID] = it.conv.Size
		}
	}
	same := len(have) == len(rows)
	var grew []transcript.Conversation
	for _, c := range rows {
		size, ok := have[c.SessionID]
		if !ok {
			same = false
			break
		}
		if size != c.Size {
			grew = append(grew, c)
		}
	}
	if same {
		return grew
	}
	m.listGen++
	var selectedID string
	if len(m.filtered) > 0 {
		selectedID = m.filtered[m.cursor].conv.SessionID
	}
	scroll := m.previewScroll
	items := make([]listItem, 0, len(m.items)+len(rows))
	for _, it := range m.items {
		if it.conv.Kind != transcript.KindSubagent {
			items = append(items, it)
		}
	}
	m.items = append(items, buildItems(rows)...)
	m.lastFilterQuery = "" // a full pass: the new rows were never matched
	m.updateFilter()
	if i := m.cursorOf(selectedID); i >= 0 {
		m.cursor = i
		m.previewScroll = min(scroll, m.maxPreviewScroll())
	}
	return nil
}

// cursorOf is id's place in m.filtered, -1 if it isn't shown.
func (m model) cursorOf(id string) int {
	for i, it := range m.filtered {
		if it.conv.SessionID == id {
			return i
		}
	}
	return -1
}

// subagentAliveRow: c is a subagent row whose subagent hasn't finished, so
// its family counts as running (nestFamilies) and it isn't marked closed.
func (m model) subagentAliveRow(c transcript.Conversation) bool {
	if c.Kind != transcript.KindSubagent {
		return false
	}
	s, ok := m.subagents[c.SessionID]
	return ok && !s.Finished
}

// subagentTitleColour is a subagent row's title colour: amber while it
// writes (transcript.SubagentIdle), plain when an idle teammate, grey once
// finished.
func (m model) subagentTitleColour(c transcript.Conversation) string {
	s, ok := m.subagents[c.SessionID]
	switch {
	case !ok || s.Finished:
		return "90"
	case time.Since(s.LastWrite) < transcript.SubagentIdle:
		return amber
	}
	return ""
}

// subagentParentOf is the listed session running subagent row c.
func (m *model) subagentParentOf(c transcript.Conversation) (transcript.Conversation, bool) {
	if i, ok := m.itemAt(c.ParentID); ok {
		return m.items[i].conv, true
	}
	return transcript.Conversation{}, false
}

// refuseSubagent: the selected row is a subagent, which has no session of
// its own to fork, rename, prune, delete, pin or put away; says so with a
// grey note.
func (m *model) refuseSubagent() bool {
	if len(m.filtered) == 0 || m.filtered[m.cursor].conv.Kind != transcript.KindSubagent {
		return false
	}
	m.note(subagentNote)
	return true
}

// subagentNote is the grey note for anything a subagent row can't do.
const subagentNote = "A subagent: it runs inside its session, which Enter opens; select that for anything else"

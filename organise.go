package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// sessionMarks is what ccs remembers about sessions between runs: when each
// was last looked at, so a session that finished since shows as done, and
// which are pinned, in order.
type sessionMarks struct {
	Since  time.Time            `json:"since"` // first run: nothing that finished before it is done
	Seen   map[string]time.Time `json:"seen,omitempty"`
	Pinned []string             `json:"pinned,omitempty"`
	// Collapsed are the ^X G folders (groupKey) showing only their most
	// urgent session.
	Collapsed []string `json:"collapsed,omitempty"`
}

// marksPath keeps sessionMarks beside the settings.
var marksPath = func() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccs", "sessions.json")
}()

// loadMarks reads them; with no file, everything so far counts as seen.
func loadMarks() *sessionMarks {
	s := &sessionMarks{}
	if b, err := os.ReadFile(marksPath); err == nil {
		json.Unmarshal(b, s)
	}
	if s.Since.IsZero() {
		s.Since = time.Now()
		s.save()
	}
	if s.Seen == nil {
		s.Seen = map[string]time.Time{}
	}
	for id, t := range s.Seen { // older than any session the list shows
		if time.Since(t) > 365*24*time.Hour {
			delete(s.Seen, id)
		}
	}
	return s
}

// save writes them atomically. A failure only means a session may show as
// done again after a restart, so it isn't reported.
func (s *sessionMarks) save() {
	data, _ := json.Marshal(s)
	if os.MkdirAll(filepath.Dir(marksPath), 0o700) != nil {
		return
	}
	tmp := marksPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, marksPath)
	}
}

// unseen: c's last message came after you last looked at it in ccs (or
// after ccs first ran, for one never looked at).
func (m model) unseen(c Conversation) bool {
	if m.marks == nil {
		return false
	}
	last, err := time.Parse(time.RFC3339, c.LastTimestamp)
	if err != nil {
		return false
	}
	seen := m.marks.Since
	if t := m.marks.Seen[c.SessionID]; t.After(seen) {
		seen = t
	}
	return last.After(seen)
}

// noteSeen marks the selected session seen while you can see it: ccs has the
// terminal's focus and its preview is drawn. True when it was done until now.
func (m *model) noteSeen() bool {
	if m.marks == nil || m.blurred || len(m.filtered) == 0 || !m.previewSettled() {
		return false
	}
	c := m.filtered[m.cursor].conv
	if m.liveGroup(c) != 2 { // only once done: a busy one would write on every reply
		return false
	}
	m.markSeen(c.SessionID)
	return true
}

// markSeen records that you've looked at session id now.
func (m *model) markSeen(id string) {
	if m.marks != nil {
		m.marks.Seen[id] = time.Now()
		m.marks.save()
	}
}

// pinIndex is id's place among the pinned sessions, -1 if not pinned.
func (m model) pinIndex(id string) int {
	if m.marks == nil {
		return -1
	}
	return slices.Index(m.marks.Pinned, id)
}

// togglePin (^X P) pins the selected session at the bottom of the pinned
// ones, or unpins it.
func (m *model) togglePin() {
	if m.marks == nil || len(m.filtered) == 0 {
		return
	}
	id := m.filtered[m.cursor].conv.SessionID
	if i := m.pinIndex(id); i >= 0 {
		m.marks.Pinned = slices.Delete(m.marks.Pinned, i, i+1)
	} else {
		m.marks.Pinned = append(m.marks.Pinned, id)
	}
	m.marks.save()
	m.sortList()
}

// movePin (Alt+↑/↓) moves the selected pinned session up or down among the
// pinned ones.
func (m *model) movePin(by int) {
	if len(m.filtered) == 0 {
		return
	}
	i := m.pinIndex(m.filtered[m.cursor].conv.SessionID)
	if j := i + by; i < 0 || j < 0 || j >= len(m.marks.Pinned) {
		return
	}
	p := m.marks.Pinned
	p[i], p[i+by] = p[i+by], p[i]
	m.marks.save()
	m.sortList()
}

// collapsed: the folder cwd is listed under shows only its first session.
func (m model) collapsed(cwd string) bool {
	return m.marks != nil && slices.Contains(m.marks.Collapsed, groupKey(cwd))
}

// toggleCollapse (^X C, grouped) collapses the selected session's folder to
// its most urgent session, or expands it again.
func (m *model) toggleCollapse() {
	if m.marks == nil || len(m.filtered) == 0 {
		return
	}
	k := groupKey(m.filtered[m.cursor].familyCwd)
	if i := slices.Index(m.marks.Collapsed, k); i >= 0 {
		m.marks.Collapsed = slices.Delete(m.marks.Collapsed, i, i+1)
	} else {
		m.marks.Collapsed = append(m.marks.Collapsed, k)
	}
	m.marks.save()
	m.sortList()
}

// hideCollapsed marks every session of a collapsed folder hidden but the
// first in each section, the most urgent. Pinned ones are never hidden.
func (m *model) hideCollapsed() {
	for i := range m.filtered {
		it := &m.filtered[i]
		it.hidden = m.grouped && it.familyState >= 0 && m.collapsed(it.familyCwd) &&
			i > 0 && groupOf(*it) == groupOf(m.filtered[i-1])
	}
}

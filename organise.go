package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// sessionMarks is what ccs remembers about sessions between runs: when each
// was last looked at, so a session that finished since shows as done.
type sessionMarks struct {
	Since time.Time            `json:"since"` // first run: nothing that finished before it is done
	Seen  map[string]time.Time `json:"seen,omitempty"`
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

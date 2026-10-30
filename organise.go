package main

import (
	"fmt"
	"strings"

	"encoding/json"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	// Snoozed sessions sit in the snoozed section until a time; settled ones
	// until their last message changes (kept here as it was). Either comes
	// back early when it waits on a menu or prompt.
	Snoozed map[string]time.Time `json:"snoozed,omitempty"`
	Settled map[string]string    `json:"settled,omitempty"`
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
	if s.Snoozed == nil {
		s.Snoozed = map[string]time.Time{}
	}
	if s.Settled == nil {
		s.Settled = map[string]string{}
	}
	for id, t := range s.Snoozed { // woken while ccs wasn't running
		if time.Now().After(t) {
			delete(s.Snoozed, id)
		}
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

// noteSeen notes the selected done session as being looked at while you can
// see it (ccs has the terminal's focus and its preview is drawn), and marks
// it seen once the selection moves off it, so its circle stays cyan while
// you read. True when the list needs sorting again.
func (m *model) noteSeen() bool {
	sel := ""
	if len(m.filtered) > 0 {
		sel = m.filtered[m.cursor].conv.SessionID
	}
	left := false
	if m.viewing != "" && m.viewing != sel {
		m.markSeen(m.viewing)
		m.viewing, left = "", true
	}
	if m.marks == nil || m.blurred || sel == "" || !m.previewSettled() {
		return left
	}
	if c := m.filtered[m.cursor].conv; m.liveGroup(c) == 2 && m.unseen(c) { // only once done: a busy one would write on every reply
		m.viewing = sel
	}
	return left
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

// Put-away states (putAway), each a section at the bottom of the list.
const (
	awake = iota
	settled
	snoozed
)

// putAway is whether c is settled or snoozed now. A session waiting on a
// menu or prompt is always awake: a question is never put away.
func (m model) putAway(c Conversation) int {
	if m.marks == nil || m.live[c.SessionID] && m.waitSignal(c) == waitPrompt {
		return awake
	}
	if t, ok := m.marks.Snoozed[c.SessionID]; ok && time.Now().Before(t) {
		return snoozed
	}
	if last, ok := m.marks.Settled[c.SessionID]; ok && last == c.LastTimestamp {
		return settled
	}
	return awake
}

// snoozeChoice is one line of the ^X S picker: a time to snooze until, or
// settle (zero until).
type snoozeChoice struct {
	label string
	until time.Time
}

// snoozeChoices are the ^X S picker's lines, from now.
func snoozeChoices(now time.Time) []snoozeChoice {
	morning := time.Date(now.Year(), now.Month(), now.Day()+1, 9, 0, 0, 0, now.Location())
	return []snoozeChoice{
		{"until it does something (settle)", time.Time{}},
		{"for an hour", now.Add(time.Hour)},
		{"for 4 hours", now.Add(4 * time.Hour)},
		{"until tomorrow " + morning.Format("Mon 15:04"), morning},
	}
}

// putAwayNow settles or snoozes the selected session as chosen in the picker.
func (m *model) putAwayNow(choice snoozeChoice) {
	if m.marks == nil || len(m.filtered) == 0 {
		return
	}
	c := m.filtered[m.cursor].conv
	if choice.until.IsZero() {
		m.marks.Settled[c.SessionID] = c.LastTimestamp
	} else {
		m.marks.Snoozed[c.SessionID] = choice.until
	}
	m.marks.save()
	m.sortList()
}

// wake brings a settled or snoozed session back.
func (m *model) wake(id string) {
	delete(m.marks.Snoozed, id)
	delete(m.marks.Settled, id)
	m.marks.save()
	m.sortList()
}

// snoozePopup is the ^X S picker.
func (m model) snoozePopup() string {
	var b strings.Builder
	b.WriteString("\033[1mPut it away\033[0m\n\n")
	for i, c := range snoozeChoices(time.Now()) {
		row := fmt.Sprintf("%d %s", i+1, c.label)
		if i == m.snoozeCursor {
			row = "\033[1m› " + row + "\033[0m"
		} else {
			row = "  " + row
		}
		b.WriteString(row + "\n")
	}
	b.WriteString("\n\033[90mback early if it waits on you\033[0m\n")
	b.WriteString("\n\033[90m" + hints("↑↓", "move", keyEnter, "put away", "1-4", "pick", "^X S/"+keyEsc, "close") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(b.String())
}

// snoozeKey handles a key while the ^X S picker is open.
func (m model) snoozeKey(k string) (tea.Model, tea.Cmd) {
	choices := snoozeChoices(time.Now())
	switch k {
	case "esc", "^x s":
		m.snoozeOpen = false
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "ctrl+p":
		m.snoozeCursor = max(m.snoozeCursor-1, 0)
	case "down", "ctrl+n":
		m.snoozeCursor = min(m.snoozeCursor+1, len(choices)-1)
	case "enter":
		m.snoozeOpen = false
		m.putAwayNow(choices[m.snoozeCursor])
	default:
		if len(k) == 1 && k[0] >= '1' && int(k[0]-'1') < len(choices) {
			m.snoozeOpen = false
			m.putAwayNow(choices[k[0]-'1'])
		}
	}
	return m, nil
}

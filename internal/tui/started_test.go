package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// startedModel lists three live sessions whose start order differs from
// their last-message order: b started last, c first but spoke last.
func startedModel(t *testing.T) model {
	t.Helper()
	mk := func(id, first, last, text string) transcript.Conversation {
		return transcript.Conversation{SessionID: id, Title: id + " session", FirstTimestamp: "2026-10-23T" + first + ":00Z",
			LastTimestamp: "2026-10-23T" + last + ":00Z", Messages: []transcript.Message{{Role: "assistant", Text: text}}}
	}
	m := initialModel(buildItems([]transcript.Conversation{
		mk("a", "10:00", "10:30", "done."), mk("b", "11:00", "11:05", "done."), mk("c", "09:00", "12:00", "done."),
	}), "", nil)
	m.width, m.height = 120, 30
	m.marks = &sessionMarks{Since: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), Seen: map[string]time.Time{}, Settled: map[string]string{}, Snoozed: map[string]time.Time{}}
	m.live = map[string]bool{"a": true, "b": true, "c": true}
	m.chatStatus = map[string]sessions.SessionStat{"a": {Status: "idle"}, "b": {Status: "idle"}, "c": {Status: "idle"}}
	m.sortList()
	return m
}

func startOrder(m model) string {
	var out []string
	for _, it := range m.filtered {
		out = append(out, it.conv.SessionID)
	}
	return strings.Join(out, ",")
}

// The list is newest started first, and a state change, a new message or
// an unread reply moves nothing; the cursor stays on its session.
func TestOrderIsByStartTime(t *testing.T) {
	m := startedModel(t)
	if got := startOrder(m); got != "b,a,c" {
		t.Fatalf("newest started first, whatever spoke last: %s", got)
	}
	m.cursor = 1 // a
	m.chatStatus = map[string]sessions.SessionStat{"a": {Status: "busy"}, "b": {Status: "idle"}, "c": waiting(time.Now(), "permission prompt")}
	for i := range m.filtered {
		if m.filtered[i].conv.SessionID == "b" { // a new reply, unread
			m.filtered[i].conv.LastTimestamp = "2026-10-23T13:00:00Z"
			m.filtered[i].conv.Messages = append(m.filtered[i].conv.Messages, transcript.Message{Role: "assistant", Text: "Want that?"})
		}
	}
	res, _ := m.Update(struct{}{}) // a tick: Update sorts
	m = res.(model)
	if got := startOrder(m); got != "b,a,c" {
		t.Errorf("states, new messages and unread replies reorder nothing: %s", got)
	}
	if selected(m) != "a" {
		t.Errorf("the cursor stays on its session: %s", selected(m))
	}
	if v := listView(m); strings.Contains(v, "waiting on you") || strings.Contains(v, "idle 2") || strings.Contains(v, "busy 1") {
		t.Errorf("no state headings:\n%s", v)
	}
	if c := m.titleColour(m.filtered[2]); c != waitColour {
		t.Errorf("the title's colour still shows the state: %q", c)
	}
}

// A new session goes on top; the rest keep their order.
func TestNewSessionGoesOnTop(t *testing.T) {
	m := startedModel(t)
	m.items = append(m.items, buildItems([]transcript.Conversation{{SessionID: "d", Title: "d session",
		FirstTimestamp: "2026-10-23T12:30:00Z", LastTimestamp: "2026-10-23T12:30:00Z", Messages: []transcript.Message{{Role: "user", Text: "hi"}}}})...)
	m.updateFilter()
	if got := startOrder(m); got != "d,b,a,c" {
		t.Errorf("the new session on top: %s", got)
	}
}

// With no timestamped line, a transcript's file birth is when it started.
func TestStartedFallsBackToFileBirth(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if got, want := startedAt(transcript.Conversation{FilePath: p}), fileBirth(info).UnixNano(); got != want || got == 0 {
		t.Errorf("file birth: got %d, want %d", got, want)
	}
	if got := startedAt(transcript.Conversation{FirstTimestamp: "2026-10-23T09:00:00.5Z", FilePath: p}); got != time.Date(2026, 10, 23, 9, 0, 0, 5e8, time.UTC).UnixNano() {
		t.Errorf("the first timestamp wins, fractions and all: %d", got)
	}
}

// Pins stay on top in pin order; settled then snoozed sessions sit at the
// bottom, whatever their start.
func TestPinsTopPutAwayBottom(t *testing.T) {
	m := startedModel(t)
	m.marks.Pinned = []string{"c"}
	m.marks.Settled["b"] = "2026-10-23T11:05:00Z"
	m.sortList()
	if got := startOrder(m); got != "c,a,b" {
		t.Errorf("pinned on top, settled at the bottom: %s", got)
	}
	v := listView(m)
	if !strings.Contains(v, "pinned 1") || !strings.Contains(v, "settled 1") || strings.Index(v, "pinned") > strings.Index(v, "settled") {
		t.Errorf("the pinned and settled headings stay:\n%s", v)
	}
	m.marks.Pinned = nil
	m.marks.Snoozed["a"] = time.Now().Add(time.Hour)
	m.sortList()
	if got := startOrder(m); got != "c,b,a" {
		t.Errorf("settled, then snoozed, at the bottom: %s", got)
	}
}

// While searching, the best match comes first, then started time.
func TestSearchRankBeforeStartTime(t *testing.T) {
	m := startedModel(t)
	for i := range m.items { // c is named after the search; a and b only mention it
		c := m.items[i].conv
		if c.SessionID == "c" {
			c.Title = "deploy"
		} else {
			c.Messages = append(c.Messages, transcript.Message{Role: "user", Text: "deploy it"})
		}
		c.SearchText = ""
		m.items[i] = buildItems([]transcript.Conversation{c})[0]
	}
	m.textInput.SetValue("deploy")
	m.updateFilter()
	if got := startOrder(m); got != "c,b,a" {
		t.Errorf("the session named after the search first, though it started first: %s", got)
	}
}

// Tab goes to what needs you most, whatever the list order: a menu before
// an idle question, though the question's session is listed first.
func TestTabByUrgencyNotListOrder(t *testing.T) {
	m := startedModel(t)
	for i := range m.filtered {
		if m.filtered[i].conv.SessionID == "b" {
			m.filtered[i].conv.Messages = []transcript.Message{{Role: "assistant", Text: "Shall I go on?"}}
		}
	}
	m.setStat("c", waiting(time.Now(), "permission prompt"))
	m.sortList()
	if got := startOrder(m); got != "b,a,c" {
		t.Fatalf("the order is by start: %s", got)
	}
	m.cursor = 0
	if m, _ = key(m, tea.KeyMsg{Type: tea.KeyTab}); selected(m) != "c" {
		t.Errorf("Tab goes to the menu first: %s", selected(m))
	}
}

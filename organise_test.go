package main

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestDoneUntilSeen(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	convs := []Conversation{
		{SessionID: "old", Title: "finished before ccs ran", LastTimestamp: at(-2 * time.Hour), Messages: []Message{{Role: "assistant", Text: "x"}}},
		{SessionID: "new", Title: "finished while away", LastTimestamp: at(-time.Minute), Messages: []Message{{Role: "assistant", Text: "y"}}},
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 140, 30
	m.marks = &sessionMarks{Since: now.Add(-time.Hour), Seen: map[string]time.Time{}}
	m.live = map[string]bool{"old": true, "new": true}
	m.chatStatus = map[string]sessionStat{"old": {status: "idle"}, "new": {status: "idle"}}
	m.blurred = true // not looking yet
	m.sortList()
	v := strip2(m.View())
	if !strings.Contains(v, "done 1") || !strings.Contains(v, "idle 1") || strings.Index(v, "finished while away") > strings.Index(v, "finished before") {
		t.Errorf("a session that finished since ccs last saw it is done, above idle:\n%s", v)
	}
	if r := m.formatListItem(m.filtered[0], false); !strings.Contains(r, "\033["+doneColour+"m●") {
		t.Errorf("done gets its own colour: %q", r)
	}
	m.cursor = 0 // the done one
	nm, _ := m.Update(tea.FocusMsg{})
	m = nm.(model)
	if v := strip2(m.View()); strings.Contains(v, "done 1") || !strings.Contains(v, "idle 2") {
		t.Errorf("looking at it (focused, selected) makes it plain idle:\n%s", v)
	}
	if _, ok := m.marks.Seen["new"]; !ok {
		t.Error("seen time not recorded")
	}
	// A new reply after that is done again.
	m.filtered[0].conv.LastTimestamp = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	m.blurred = true
	m.sortList()
	if m.liveGroup(m.filtered[0].conv) != 2 {
		t.Error("a reply after you looked is done again")
	}
}

func TestMarksFirstRunAndPrune(t *testing.T) {
	os.Remove(marksPath)
	s := loadMarks()
	if s.Since.IsZero() || time.Since(s.Since) > time.Minute {
		t.Fatalf("first run starts now: %v", s.Since)
	}
	s.Seen["ancient"] = time.Now().Add(-400 * 24 * time.Hour)
	s.Seen["recent"] = time.Now()
	s.save()
	s2 := loadMarks()
	if _, ok := s2.Seen["ancient"]; ok || !s2.Since.Equal(s.Since) {
		t.Errorf("reload keeps since, drops year-old seen times: %+v", s2)
	}
	if _, ok := s2.Seen["recent"]; !ok {
		t.Error("recent seen time lost")
	}
}

func TestPinnedSectionKeepsOrder(t *testing.T) {
	now := time.Now().UTC()
	var convs []Conversation
	for i, id := range []string{"a", "b", "c"} {
		convs = append(convs, Conversation{SessionID: id, Title: "session " + id, LastTimestamp: now.Add(time.Duration(-i) * time.Hour).Format(time.RFC3339), Messages: []Message{{Role: "user", Text: id}}})
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 140, 30
	m.marks = &sessionMarks{Since: now, Seen: map[string]time.Time{}}
	m.live = map[string]bool{"a": true}
	m.chatStatus = map[string]sessionStat{"a": {status: "waiting"}}
	m.sortList()
	sel := func(id string) {
		for i, it := range m.filtered {
			if it.conv.SessionID == id {
				m.cursor = i
			}
		}
	}
	sel("c")
	m, _ = chord(m, "p")
	sel("b")
	m, _ = chord(m, "p")
	v := strip2(m.View())
	ic, ib, ia := strings.Index(v, "session c"), strings.Index(v, "session b"), strings.Index(v, "session a")
	if !strings.Contains(v, "pinned 2") || !(ic < ib && ib < ia) {
		t.Errorf("pinned first in pin order, even above a session waiting on you:\n%s", v)
	}
	if m.filtered[m.cursor].conv.SessionID != "b" {
		t.Error("pinning keeps the cursor on the session")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if v := strip2(m.View()); strings.Index(v, "session b") > strings.Index(v, "session c") {
		t.Errorf("alt+up moves it above:\n%s", v)
	}
	if got := loadMarks().Pinned; len(got) != 2 || got[0] != "b" {
		t.Errorf("order saved: %v", got)
	}
	m, _ = chord(m, "p")
	if v := strip2(m.View()); !strings.Contains(v, "pinned 1") {
		t.Errorf("again unpins:\n%s", v)
	}
}

func TestCollapsedFolderShowsItsFirst(t *testing.T) {
	now := time.Now().UTC()
	convs := []Conversation{
		{SessionID: "a1", Title: "api one", Cwd: "/r/api", LastTimestamp: now.Format(time.RFC3339), Messages: []Message{{Role: "user", Text: "x"}}},
		{SessionID: "a2", Title: "api two", Cwd: "/r/api/.claude/worktrees/w", LastTimestamp: now.Add(-time.Hour).Format(time.RFC3339), Messages: []Message{{Role: "user", Text: "x"}}},
		{SessionID: "a3", Title: "api three", Cwd: "/r/api", LastTimestamp: now.Add(-2 * time.Hour).Format(time.RFC3339), Messages: []Message{{Role: "user", Text: "x"}}},
		{SessionID: "w1", Title: "web one", Cwd: "/r/web", LastTimestamp: now.Add(-3 * time.Hour).Format(time.RFC3339), Messages: []Message{{Role: "user", Text: "x"}}},
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 140, 30
	m.marks = &sessionMarks{Since: now, Seen: map[string]time.Time{}}
	m.grouped = true
	m.sortList()
	m.cursor = 1 // api two
	m, _ = chord(m, "c")
	v := strip2(m.View())
	if !strings.Contains(v, "▸ api 3 · 2 more") || strings.Contains(v, "api two") || strings.Contains(v, "api three") || !strings.Contains(v, "api one") {
		t.Errorf("a collapsed folder shows its first session and counts the rest:\n%s", v)
	}
	if m.filtered[m.cursor].conv.SessionID != "a1" {
		t.Errorf("cursor on a hidden session moves to the one shown, got %s", m.filtered[m.cursor].conv.SessionID)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.filtered[m.cursor].conv.SessionID != "w1" {
		t.Errorf("down skips the hidden ones, got %s", m.filtered[m.cursor].conv.SessionID)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyUp})
	m, _ = chord(m, "c")
	if v := strip2(m.View()); !strings.Contains(v, "api three") || strings.Contains(v, "▸") {
		t.Errorf("again expands:\n%s", v)
	}
	m.grouped = false
	m, _ = chord(m, "c")
	if len(m.marks.Collapsed) != 0 {
		t.Error("^X C does nothing ungrouped")
	}
}

func TestSnoozeAndSettle(t *testing.T) {
	now := time.Now().UTC()
	convs := []Conversation{
		{SessionID: "s", Title: "sleepy", LastTimestamp: now.Format(time.RFC3339), Messages: []Message{{Role: "user", Text: "x"}}},
		{SessionID: "o", Title: "other", LastTimestamp: now.Add(-time.Hour).Format(time.RFC3339), Messages: []Message{{Role: "user", Text: "x"}}},
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 140, 30
	m.marks = loadMarks()
	m.sortList()
	m.cursor = 0
	m, _ = chord(m, "s")
	if !m.snoozeOpen || !strings.Contains(strip2(m.View()), "until it does something (settle)") {
		t.Fatalf("^X S opens the picker:\n%s", strip2(m.View()))
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	v := strip2(m.View())
	if m.snoozeOpen || !strings.Contains(v, "settled 1") || strings.Index(v, "sleepy") < strings.Index(v, "other") {
		t.Errorf("settled goes to the bottom:\n%s", v)
	}
	if m.filtered[m.cursor].conv.SessionID != "s" {
		t.Error("the cursor stays on it")
	}
	m.filtered[m.cursor].conv.LastTimestamp = now.Add(time.Minute).Format(time.RFC3339)
	m.sortList()
	if strings.Contains(strip2(m.View()), "settled 1") {
		t.Error("a new message wakes a settled session")
	}
	m, _ = chord(m, "s")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(strip2(m.View()), "snoozed 1") {
		t.Errorf("snoozed for an hour:\n%s", strip2(m.View()))
	}
	m.live = map[string]bool{"s": true}
	m.chatStatus = map[string]sessionStat{"s": {status: "waiting"}}
	m.sortList()
	if v := strip2(m.View()); strings.Contains(v, "snoozed 1") || !strings.Contains(v, "waiting on you 1") {
		t.Errorf("a question wakes it:\n%s", v)
	}
	m.chatStatus = map[string]sessionStat{"s": {status: "idle"}}
	m.sortList()
	m, _ = chord(m, "s")
	if m.snoozeOpen || strings.Contains(strip2(m.View()), "snoozed") {
		t.Error("^X S on a put-away session brings it back")
	}
	if got := loadMarks(); len(got.Snoozed)+len(got.Settled) != 0 {
		t.Errorf("woken state saved: %+v", got)
	}
}

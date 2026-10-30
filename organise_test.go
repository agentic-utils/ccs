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

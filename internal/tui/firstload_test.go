package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/transcript"
)

func firstLoadItem(id, started, text string) listItem {
	return buildItems([]transcript.Conversation{{SessionID: id, FirstTimestamp: started, LastTimestamp: started,
		Messages: []transcript.Message{{Role: "user", Text: text, Ts: started}}}})[0]
}

// The first load lists what it parsed and says how many are still to come;
// the background scan's answer brings them in, keeping the selection on the
// same session, and searching works on what has loaded meanwhile.
func TestRestLoadsInTheBackground(t *testing.T) {
	a := firstLoadItem("a", "2026-09-20T10:00:00Z", "apple recent")
	b := firstLoadItem("b", "2026-08-01T10:00:00Z", "banana live and old")
	old := firstLoadItem("old", "2026-08-15T10:00:00Z", "apricot from weeks ago")
	m := initialModel([]listItem{a, b}, "", nil)
	scans := 0
	m.reload = func() ([]listItem, error) { scans++; return []listItem{a, b, old}, nil }
	m.restPending = 1
	m.sortList()
	m.cursor = 1 // b, under a
	if m.filtered[m.cursor].conv.SessionID != "b" {
		t.Fatalf("setup: want b selected, got %s", m.filtered[m.cursor].conv.SessionID)
	}
	if got := m.refreshNote(); got != " · loading 1 more" {
		t.Errorf("header note before the rest: %q", got)
	}
	m.width, m.height = 160, 30
	if !strings.Contains(m.View(), "loading 1 more") {
		t.Error("the note is drawn in the header")
	}

	// Searching meanwhile finds what has loaded.
	m.textInput.SetValue("ap")
	m.updateFilter()
	if len(m.filtered) != 1 || m.filtered[0].conv.SessionID != "a" {
		t.Errorf("search on what has loaded: %d rows", len(m.filtered))
	}
	m.textInput.SetValue("")
	m.updateFilter()
	m.cursor = 1

	// Init asks for the rest at once, and that runs the scan.
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init must start the rest's scan")
	}
	res, cmd := m.Update(loadRestMsg{})
	m = res.(model)
	if cmd == nil || m.refreshStarted.IsZero() {
		t.Fatal("loadRestMsg starts a background scan")
	}
	// A second ask while it runs starts nothing more.
	if _, again := m.Update(loadRestMsg{}); again != nil {
		t.Error("one scan at a time")
	}
	msg := cmd()
	if scans != 1 {
		t.Fatalf("the scan ran %d times", scans)
	}
	res, _ = m.Update(msg)
	m = res.(model)
	if len(m.items) != 3 || m.restPending != 0 {
		t.Errorf("the rest has loaded: %d items, %d pending", len(m.items), m.restPending)
	}
	if m.filtered[m.cursor].conv.SessionID != "b" {
		t.Errorf("selection stays on b, got %s", m.filtered[m.cursor].conv.SessionID)
	}
	if strings.Contains(m.refreshNote(), "loading") {
		t.Errorf("no note once loaded: %q", m.refreshNote())
	}
	if _, cmd := m.Update(loadRestMsg{}); cmd != nil {
		t.Error("nothing left to load: no scan")
	}
}

// A background scan that doesn't land (failed, or dropped under a prompt)
// leaves the note up and asks again soon, not a minute later.
func TestRestRetriedWhenTheScanDoesNotLand(t *testing.T) {
	m := initialModel([]listItem{firstLoadItem("a", "2026-09-20T10:00:00Z", "x")}, "", nil)
	m.reload = func() ([]listItem, error) { return nil, os.ErrPermission }
	m.restPending = 4
	res, cmd := m.Update(refreshMsg{err: os.ErrPermission, early: true})
	m = res.(model)
	if m.restPending != 4 || cmd == nil {
		t.Errorf("still pending with a retry scheduled: pending %d, cmd %v", m.restPending, cmd != nil)
	}
	if !strings.Contains(m.refreshNote(), "loading 4 more") {
		t.Errorf("note stays: %q", m.refreshNote())
	}
}

// After an update restart, a selected session among those still to load is
// selected once they land, unless the selection moved meanwhile.
func TestRestoreWaitsForTheRest(t *testing.T) {
	a := firstLoadItem("a", "2026-09-20T10:00:00Z", "a")
	old := firstLoadItem("old", "2026-08-15T10:00:00Z", "old")
	for _, moved := range []bool{false, true} {
		m := initialModel([]listItem{a, firstLoadItem("c", "2026-09-19T10:00:00Z", "c")}, "", nil)
		m.restPending = 1
		m.restore(uiState{Selected: "old"})
		if m.restoreSel != "old" {
			t.Fatalf("restore should wait for old, got %q", m.restoreSel)
		}
		if moved {
			m.cursor = 1
		}
		was := m.filtered[m.cursor].conv.SessionID
		res, _ := m.Update(refreshMsg{items: []listItem{a, m.items[1], old}})
		m = res.(model)
		got := m.filtered[m.cursor].conv.SessionID
		switch {
		case !moved && got != "old":
			t.Errorf("restore's session once loaded: got %s", got)
		case moved && got != was:
			t.Errorf("moved meanwhile: selection stays on %s, got %s", was, got)
		}
	}
}

// The offer to reopen sessions after a restart waits for the rest too, so a
// session open before it but idle for weeks is still offered.
func TestReopenOfferWaitsForTheRest(t *testing.T) {
	a := firstLoadItem("a", "2026-09-20T10:00:00Z", "a")
	old := firstLoadItem("old", "2026-08-15T10:00:00Z", "old")
	m := initialModel([]listItem{a}, "", nil)
	m.restPending = 1
	res, _ := m.Update(reopenMsg{ids: []string{"a", "old"}})
	m = res.(model)
	if len(m.reopen) != 0 || !m.reopenChecked {
		t.Fatalf("offer waits: %d offered", len(m.reopen))
	}
	res, _ = m.Update(refreshMsg{items: []listItem{a, old}})
	m = res.(model)
	if len(m.reopen) != 2 {
		t.Errorf("both offered once loaded, got %d", len(m.reopen))
	}
}

package tui

import (
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// attentionModel: "menu" waits on a menu (tier 1), "asks" is idle and its
// last message asks something (tier 2), "live" is idle and done since you
// last looked (tier 3). Every session is newer than the marks' Since, and
// none has been seen, so each idle one without a question is unread.
func attentionModel(t *testing.T) model {
	t.Helper()
	m := waitModel(t)
	m.width, m.height = 140, 40
	m.marks = &sessionMarks{Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Seen: map[string]time.Time{}}
	now := time.Now()
	m.setStat("menu", waiting(now, "input needed"))
	m.setStat("asks", sessions.SessionStat{Status: "idle", Since: now})
	m.setStat("live", sessions.SessionStat{Status: "idle", Since: now})
	m.sortList()
	m.cursor = len(m.filtered) - 1
	for id, want := range map[string]int{"menu": attentionWaits, "asks": attentionAsked, "live": attentionUnread} {
		if got := m.attention(m.items[m.indexOf(id)].conv); got != want {
			t.Fatalf("%s is tier %d, want %d", id, got, want)
		}
	}
	return m
}

func (m model) indexOf(id string) int {
	for i, it := range m.items {
		if it.conv.SessionID == id {
			return i
		}
	}
	panic("no session " + id)
}

// Strict priority: a held question or menu beats a session that asked
// something, which beats a done one not read, whatever their places in the
// list; within a tier Tab wraps, so it stays on the only one there.
func TestTabGoesByPriority(t *testing.T) {
	m := attentionModel(t)
	for i := range 2 {
		if m = tab(m); selected(m) != "menu" {
			t.Fatalf("Tab %d: on %s, want the menu (tier 1, wrapping onto itself)", i+1, selected(m))
		}
	}
	// The menu answered: the tier empties, so Tab goes on to the one that asked.
	m.setStat("menu", sessions.SessionStat{Status: "busy", Since: time.Now()})
	if m = tab(m); selected(m) != "asks" {
		t.Fatalf("tier 1 empty: on %s, want asks", selected(m))
	}
	// A new question comes up: it's the very next stop again.
	m.setStat("menu", waiting(time.Now(), "input needed"))
	if m = tab(m); selected(m) != "menu" {
		t.Fatalf("a new question is next: on %s", selected(m))
	}
	// With tiers 1 and 2 empty, the done one not read.
	m.setStat("menu", sessions.SessionStat{Status: "busy", Since: time.Now()})
	m.setStat("asks", sessions.SessionStat{Status: "busy", Since: time.Now()})
	if m = tab(m); selected(m) != "live" {
		t.Fatalf("tiers 1 and 2 empty: on %s, want live (done, unread)", selected(m))
	}
}

// Within a tier, Tab goes to the next one after the selection in list order
// and wraps round.
func TestTabCyclesWithinATier(t *testing.T) {
	m := attentionModel(t)
	m.setStat("asks", waiting(time.Now(), "permission prompt"))
	m.sortList()
	var order []string
	for _, it := range m.filtered {
		if m.attention(it.conv) == attentionWaits {
			order = append(order, it.conv.SessionID)
		}
	}
	if len(order) != 2 {
		t.Fatalf("two in tier 1: %v", order)
	}
	m.cursor = len(m.filtered) - 1
	for i, want := range []string{order[0], order[1], order[0]} {
		if m = tab(m); selected(m) != want {
			t.Fatalf("Tab %d: on %s, want %s", i+1, selected(m), want)
		}
	}
}

// Snoozed or settled sessions are never a stop, whatever their tier.
func TestTabSkipsPutAwaySessions(t *testing.T) {
	m := attentionModel(t)
	m.setStat("menu", sessions.SessionStat{Status: "busy", Since: time.Now()})
	m.marks.Snoozed = map[string]time.Time{"asks": time.Now().Add(time.Hour)}
	if m = tab(m); selected(m) != "live" {
		t.Fatalf("asks is snoozed: on %s, want live", selected(m))
	}
}

// A done one is read once you've looked at it and moved on, by the usual
// rule: Tab doesn't mark it, and the one being read isn't a stop. With
// nothing left, Tab says so.
func TestTabDoneUnreadClearsOnceRead(t *testing.T) {
	m := attentionModel(t)
	m.setStat("menu", sessions.SessionStat{Status: "busy", Since: time.Now()})
	m.setStat("asks", sessions.SessionStat{Status: "busy", Since: time.Now()})
	if m = tab(m); selected(m) != "live" || m.viewing != "live" {
		t.Fatalf("on %s, viewing %q: Tab goes to the unread done one and you're reading it", selected(m), m.viewing)
	}
	if !m.unseen(m.items[m.indexOf("live")].conv) {
		t.Fatal("not marked seen yet: only moving off it does that")
	}
	before := selected(m)
	if m = tab(m); selected(m) != before || m.noteMsg != "nothing needs you" {
		t.Fatalf("the one being read isn't a stop: on %s, note %q", selected(m), m.noteMsg)
	}
	m.cursor = 0
	if selected(m) == "live" {
		m.cursor = 1
	}
	m.noteSeen()
	if m.unseen(m.items[m.indexOf("live")].conv) {
		t.Fatal("moving off it marks it read")
	}
	if m.anyWaiting() {
		t.Fatal("all read and nothing waits: Tab has nowhere to go")
	}
}

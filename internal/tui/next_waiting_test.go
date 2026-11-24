package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// tabModel is waitModel with "live" and "menu" waiting on a menu, sorted,
// and the selection on the last row (a session that needs nothing).
func tabModel(t *testing.T) model {
	t.Helper()
	m := waitModel(t)
	m.width, m.height = 140, 40
	now := time.Now()
	m.setStat("live", waiting(now.Add(-time.Minute), "input needed"))
	m.setStat("menu", waiting(now, "permission prompt"))
	m.sortList()
	m.cursor = len(m.filtered) - 1
	if m.needsYou(m.filtered[m.cursor].conv) {
		t.Fatal("the last row needs nothing")
	}
	return m
}

func tab(m model) model {
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyTab})
	return m
}

func selected(m model) string { return m.filtered[m.cursor].conv.SessionID }

// Tab goes to the next session waiting on you, in list order, and wraps.
func TestTabCyclesThroughWaitingSessions(t *testing.T) {
	m := tabModel(t)
	var order []string
	for _, it := range m.filtered {
		if m.needsYou(it.conv) {
			order = append(order, it.conv.SessionID)
		}
	}
	if len(order) != 2 {
		t.Fatalf("two sessions wait: %v", order)
	}
	for i, want := range []string{order[0], order[1], order[0]} {
		if m = tab(m); m.showUsage || selected(m) != want {
			t.Fatalf("Tab %d: on %s (usage %v), want %s", i+1, selected(m), m.showUsage, want)
		}
	}
}

// A question ccs holds counts, even before its status says waiting; a
// snoozed one doesn't; with nothing waiting, Tab only says so: it's never
// the usage screen.
func TestTabCountsHeldSkipsSnoozedElseNothing(t *testing.T) {
	m := waitModel(t)
	m.width, m.height = 140, 40
	m.sortList()
	before := selected(m)
	if m = tab(m); m.showUsage || selected(m) != before || m.noteMsg != "nothing needs you" || m.errorMsg != m.noteMsg {
		t.Fatalf("nothing waits: Tab stays on the list (%s, usage %v) with a grey note (%q)", selected(m), m.showUsage, m.errorMsg)
	}
	m.heldPrompt = map[string]mcp.HeldPrompt{"asks": {ID: "h1", Session: "asks", Tool: "Bash"}}
	// Since now: nothing is done and unread, so only the held question could count.
	m.marks = &sessionMarks{Since: time.Now(), Snoozed: map[string]time.Time{"asks": time.Now().Add(time.Hour)}}
	if m.anyWaiting() {
		t.Fatal("a snoozed session doesn't count")
	}
	m.marks = &sessionMarks{Since: time.Now()}
	m.cursor = len(m.filtered) - 1
	if m = tab(m); m.showUsage || selected(m) != "asks" {
		t.Fatalf("a held question counts: on %s usage %v", selected(m), m.showUsage)
	}
}

// When the search hides every waiting session, Tab clears it to reach one.
func TestTabClearsASearchHidingTheTarget(t *testing.T) {
	m := tabModel(t)
	m.textInput.SetValue("Asking")
	m.updateFilter()
	for _, it := range m.filtered {
		if m.needsYou(it.conv) {
			t.Fatalf("the search hides the waiting sessions, not %s", it.conv.SessionID)
		}
	}
	if m = tab(m); m.textInput.Value() != "" || !m.needsYou(m.filtered[m.cursor].conv) {
		t.Fatalf("Tab cleared the search (%q) and selected a waiting session (%s)", m.textInput.Value(), selected(m))
	}
}

// The header says what Tab does, and ^T Y opens the usage screen while
// sessions wait.
func TestTabHintAndUsageChord(t *testing.T) {
	m := waitModel(t)
	m.width, m.height = 160, 40
	m.sortList()
	if head := strip2(strings.Split(m.View(), "\n")[0]); strings.Contains(head, "tab ") || !strings.Contains(head, "^T Y usage") {
		t.Errorf("nothing waits: no Tab hint, ^T Y usage: %q", head)
	}
	m = tabModel(t)
	m.width = 160
	head := strip2(strings.Split(m.View(), "\n")[0])
	if !strings.Contains(head, "tab needs you") || !strings.Contains(head, "^T Y usage") {
		t.Errorf("something waits: %q", head)
	}
	if m = usageChord(m); !m.showUsage {
		t.Fatal("^T Y opens the usage screen while sessions wait")
	}
	if m = usageChord(m); m.showUsage {
		t.Fatal("^T Y again goes back")
	}
}

// The usage screen is ^T Y's alone: Esc and ^T Y go back, and Tab there goes
// back to the list and on to the next waiting session.
func TestUsageScreenOnlyByChord(t *testing.T) {
	m := tabModel(t)
	m.width, m.height = 160, 40
	if m = usageChord(m); !m.showUsage {
		t.Fatal("^T Y opens the usage screen")
	}
	if v := strip2(m.View()); !strings.Contains(v, "esc back") || strings.Contains(v, "tab back") {
		t.Errorf("the usage screen's hints say esc goes back: %q", strings.Split(v, "\n")[0])
	}
	if m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc}); m.showUsage {
		t.Fatal("Esc goes back from the usage screen")
	}
	if m = usageChord(usageChord(m)); m.showUsage {
		t.Fatal("^T Y again goes back")
	}
	m = usageChord(m)
	if m = tab(m); m.showUsage || !m.needsYou(m.filtered[m.cursor].conv) {
		t.Fatalf("Tab on the usage screen goes back and to a waiting session (%s, usage %v)", selected(m), m.showUsage)
	}
	idle := waitModel(t)
	idle.width, idle.height = 160, 40
	idle.sortList()
	idle = usageChord(idle)
	if idle = tab(idle); idle.showUsage {
		t.Fatal("Tab on the usage screen with nothing waiting just goes back")
	}
}

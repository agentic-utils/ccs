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
// snoozed one doesn't; with nothing waiting, Tab is the usage screen.
func TestTabCountsHeldSkipsSnoozedElseUsage(t *testing.T) {
	m := waitModel(t)
	m.width, m.height = 140, 40
	m.sortList()
	if m = tab(m); !m.showUsage {
		t.Fatal("nothing waits: Tab opens the usage screen")
	}
	if m = tab(m); m.showUsage {
		t.Fatal("Tab again goes back")
	}
	m.heldPrompt = map[string]mcp.HeldPrompt{"asks": {ID: "h1", Session: "asks", Tool: "Bash"}}
	m.marks = &sessionMarks{Snoozed: map[string]time.Time{"asks": time.Now().Add(time.Hour)}}
	if m.anyWaiting() {
		t.Fatal("a snoozed session doesn't count")
	}
	m.marks = &sessionMarks{}
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

// The header says what Tab does, and ^X Y opens the usage screen while
// sessions wait.
func TestTabHintAndUsageChord(t *testing.T) {
	m := waitModel(t)
	m.width, m.height = 160, 40
	m.sortList()
	if head := strip2(strings.Split(m.View(), "\n")[0]); !strings.Contains(head, "tab usage") || strings.Contains(head, "next waiting") {
		t.Errorf("nothing waits: %q", head)
	}
	m = tabModel(t)
	m.width = 160
	head := strip2(strings.Split(m.View(), "\n")[0])
	if !strings.Contains(head, "tab next waiting") || !strings.Contains(head, "^X Y usage") {
		t.Errorf("something waits: %q", head)
	}
	if m = usageChord(m); !m.showUsage {
		t.Fatal("^X Y opens the usage screen while sessions wait")
	}
	if m = usageChord(m); m.showUsage {
		t.Fatal("^X Y again goes back")
	}
}

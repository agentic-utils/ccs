package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
)

func backTab(m model) model {
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	return m
}

// Shift+Tab goes to the session waiting on you before the selection, in
// list order, and wraps: Tab's stops, the other way round.
func TestBackTabCyclesBackwards(t *testing.T) {
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
	// From the last row (needs nothing) back: the later one first.
	for i, want := range []string{order[1], order[0], order[1]} {
		if m = backTab(m); m.showUsage || selected(m) != want {
			t.Fatalf("Shift+Tab %d: on %s, want %s", i+1, selected(m), want)
		}
	}
	// Tab then Shift+Tab comes back to where it was.
	before := selected(m)
	if m = backTab(tab(m)); selected(m) != before {
		t.Fatalf("Tab then Shift+Tab: on %s, want %s", selected(m), before)
	}
}

// Shift+Tab picks the tier as Tab does: an earlier tier beats the PR ones,
// re-checked every press, and with none, the PR sessions backwards.
func TestBackTabTierChoice(t *testing.T) {
	m := prTabModel(t)
	m.setStat("asks", waiting(time.Now(), "input needed"))
	for i := range 2 {
		if m = backTab(m); selected(m) != "asks" {
			t.Fatalf("Shift+Tab %d: on %s, want asks, waiting on a menu", i+1, selected(m))
		}
	}
	m.setStat("asks", sessions.SessionStat{Status: "busy", Since: time.Now()})
	if m = backTab(m); selected(m) != "menu" {
		t.Fatalf("the menu answered: on %s, want menu, ready to merge", selected(m))
	}
	m.prs[prReadyURL] = github.PRInfo{State: "MERGED"}
	var back []string
	for range 3 {
		m = backTab(m)
		back = append(back, selected(m))
	}
	var fwd []string
	for range 2 {
		m = tab(m)
		fwd = append(fwd, selected(m))
	}
	if back[0] == back[1] || back[2] != back[0] || fwd[0] != back[1] {
		t.Fatalf("open PRs: Shift+Tab goes through them backwards and wraps (%v), Tab the other way (%v)", back, fwd)
	}
}

// When the search hides every waiting session, Shift+Tab clears it to reach
// one; with nothing waiting it only says so, and from the usage screen it
// goes back to the list first.
func TestBackTabHiddenNoneAndUsage(t *testing.T) {
	m := tabModel(t)
	m.textInput.SetValue("Asking")
	m.updateFilter()
	if m = backTab(m); m.textInput.Value() != "" || !m.needsYou(m.filtered[m.cursor].conv) {
		t.Fatalf("Shift+Tab cleared the search (%q) and selected a waiting session (%s)", m.textInput.Value(), selected(m))
	}

	idle := waitModel(t)
	idle.width, idle.height = 140, 40
	idle.sortList()
	before := selected(idle)
	if idle = backTab(idle); idle.showUsage || selected(idle) != before || idle.noteMsg != "nothing needs you" {
		t.Fatalf("nothing waits: stays on %s (now %s), note %q", before, selected(idle), idle.noteMsg)
	}

	m = tabModel(t)
	if m = usageChord(m); !m.showUsage {
		t.Fatal("^T Y opens the usage screen")
	}
	if m = backTab(m); m.showUsage || !m.needsYou(m.filtered[m.cursor].conv) {
		t.Fatalf("Shift+Tab on the usage screen goes back and to a waiting session (%s, usage %v)", selected(m), m.showUsage)
	}
}

// The held menu never catches Shift+Tab, as it never catches Tab: it goes
// to the waiting session and answers nothing.
func TestMenuPassesBackTab(t *testing.T) {
	for name, set := range map[string]func(model) model{
		"active":  func(m model) model { return m },
		"focused": func(m model) model { m.heldFocus = true; return m },
	} {
		t.Run(name, func(t *testing.T) {
			m, calls, _ := menuPane(t, colour())
			m = set(m)
			if m = keyType(m, tea.KeyShiftTab); len(*calls) != 0 || selected(m) != "live" {
				t.Fatalf("Shift+Tab goes to the waiting session, never to the menu: calls %v on %s", *calls, selected(m))
			}
		})
	}
}

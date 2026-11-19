package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// flashModel is waitModel with "live" selected, asking an AskUserQuestion
// in its transcript (what askLines draws), on a clock the test moves.
func flashModel(t *testing.T) (model, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	old := heldNow
	heldNow = func() time.Time { return now }
	t.Cleanup(func() { heldNow = old })
	m := waitModel(t)
	m.width, m.height = 100, 40
	m.lastKeyAt = time.Now() // just typed: auto-select waits
	ask := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"AskUserQuestion","input":{"questions":[{"question":"Which fruit?","options":[{"label":"apple"},{"label":"pear"}]}]}}]}}`
	for i := range m.items {
		if m.items[i].conv.SessionID == "live" {
			transcript.ParseLine(&m.items[i].conv, []byte(ask))
		}
	}
	m.updateFilter()
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	return m, &now
}

func fruitHeld() mcp.HeldPrompt {
	return mcp.HeldPrompt{ID: "f1", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{
		{Question: "Which fruit?", Options: []string{"apple", "pear"}},
	}}
}

// The transcript's view of a question: its magenta "? " line, or the
// status line sending you to its tab.
func fallbackShown(m model) bool {
	v := m.View()
	return strings.Contains(v, "\033[1;"+waitColour+"m? ") || strings.Contains(strip2(v), "answer it in its tab")
}

func heldShown(m model) bool { return strings.Contains(strip2(m.View()), "also in its tab") }

// While something holds prompts, a question goes straight to the held menu:
// the transcript's view never shows in the moment before its held copy
// lands, nor in the moment after it's answered while the status still
// says waiting.
func TestHeldNoFlashBeforeOrAfter(t *testing.T) {
	m, now := flashModel(t)
	var prompts []mcp.HeldPrompt
	stubHeld(t)
	heldPrompts = func() ([]mcp.HeldPrompt, error) { return prompts, nil }
	m.applyHeld(heldMsg{}) // a holder is there, holding nothing yet

	// Before: the status says waiting; the held copy isn't in yet.
	m.heldAt = time.Now() // just asked: only a wait beginning hurries the next ask
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(*now, "input needed")})
	if fallbackShown(m) || !strings.Contains(strip2(m.View()), "waiting on you…") {
		t.Fatalf("before its held copy lands, a neutral line stands in for the transcript's view:\n%s", strip2(m.View()))
	}
	if m.heldCmd() == nil {
		t.Fatal("a wait beginning asks for its held copy at once")
	}
	m.heldBusy = false
	*now = now.Add(heldQuick)
	m.heldAt = time.Now().Add(-heldQuick)
	if m.heldCmd() == nil {
		t.Fatal("while its held copy is due, asks come every heldQuick")
	}
	prompts = []mcp.HeldPrompt{fruitHeld()}
	m.applyHeld(heldMsg{prompts: prompts})
	if !heldShown(m) || fallbackShown(m) {
		t.Fatalf("once held, only the held menu shows:\n%s", strip2(m.View()))
	}

	// After: answered, the held copy is dropped; the status still says waiting.
	*now = now.Add(5 * time.Second)
	prompts = nil
	m.applyHeld(heldMsg{})
	if fallbackShown(m) || heldShown(m) {
		t.Fatalf("just after the answer, neither view shows:\n%s", strip2(m.View()))
	}
	m.applyStats(map[string]sessions.SessionStat{"live": {Status: "busy"}})
	if fallbackShown(m) {
		t.Fatalf("moved on, nothing asks:\n%s", strip2(m.View()))
	}
}

// Answered from ccs, a question stays out of the transcript's view however
// long the session goes on reporting that same wait (here 3s, past
// heldGrace); once the wait ends or a new one begins, the usual rules apply.
func TestHeldAnsweredStaysHiddenWhileStatusLags(t *testing.T) {
	m, now := flashModel(t)
	stubHeld(t)
	since := *now
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(since, "input needed")})
	m.applyHeld(heldMsg{prompts: []mcp.HeldPrompt{fruitHeld()}})
	*now = now.Add(10 * time.Second)
	m.applyHeldDone(heldDoneMsg{name: "live", session: "live", prompt: "f1", text: "apple"})
	m.applyHeld(heldMsg{}) // the answer took it off the list
	for i := 0; i < 12; i++ {
		*now = now.Add(250 * time.Millisecond) // 3s, the status still on the answered wait
		m.applyStats(map[string]sessions.SessionStat{"live": waiting(since, "input needed")})
		if fallbackShown(m) {
			t.Fatalf("%v after the answer, still the same wait: no transcript view:\n%s", time.Duration(i+1)*250*time.Millisecond, strip2(m.View()))
		}
	}
	// A new wait (a new statusUpdatedAt) nothing holds shows once its grace has passed.
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(*now, "input needed")})
	if _, ok := m.heldAnswered["live"]; ok {
		t.Fatal("a new wait forgets the answered one")
	}
	*now = now.Add(heldGrace)
	if !fallbackShown(m) {
		t.Fatalf("a new wait nothing holds shows from the transcript:\n%s", strip2(m.View()))
	}
}

// While a wait's held copy is due, the held list is asked for again every
// heldQuick whatever is selected: an empty answer schedules the next ask.
func TestHeldRetriesWhileDue(t *testing.T) {
	m, now := flashModel(t)
	stubHeld(t)
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID != "live" })
	m.applyHeld(heldMsg{})
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(*now, "input needed")})
	res, cmd := m.update(heldMsg{}) // the ask at the wait's start came back empty
	m = res.(model)
	if cmd == nil {
		t.Fatal("still due: the next ask is scheduled")
	}
	m.heldAt = time.Now().Add(-heldQuick)
	if _, cmd := m.update(heldRetryMsg{}); cmd == nil {
		t.Fatal("the retry asks for the held list")
	}
	*now = now.Add(heldGrace)
	if m.heldRetry() != nil {
		t.Error("past heldGrace, no more quick asks")
	}
}

// A question nothing holds (no hook, an older one, or sent back to its tab)
// still shows from the transcript, once heldGrace has passed.
func TestHeldFallbackStillShows(t *testing.T) {
	t.Run("no holder", func(t *testing.T) {
		m, now := flashModel(t)
		m.applyStats(map[string]sessions.SessionStat{"live": waiting(*now, "input needed")})
		if !fallbackShown(m) {
			t.Fatalf("with nothing holding prompts, its view shows at once:\n%s", strip2(m.View()))
		}
	})
	t.Run("never held", func(t *testing.T) {
		m, now := flashModel(t)
		stubHeld(t)
		m.applyHeld(heldMsg{})
		m.applyStats(map[string]sessions.SessionStat{"live": waiting(*now, "input needed")})
		if fallbackShown(m) {
			t.Fatal("held back within heldGrace")
		}
		*now = now.Add(heldGrace)
		if !fallbackShown(m) {
			t.Fatalf("its held copy never came: the transcript's view shows:\n%s", strip2(m.View()))
		}
		if m.heldDue() {
			t.Error("past heldGrace, asks go back to heldEvery")
		}
	})
	t.Run("holder lost", func(t *testing.T) {
		m, now := flashModel(t)
		stubHeld(t)
		m.applyHeld(heldMsg{prompts: []mcp.HeldPrompt{fruitHeld()}})
		m.applyStats(map[string]sessions.SessionStat{"live": waiting(*now, "input needed")})
		*now = now.Add(time.Minute)
		m.applyHeld(heldMsg{err: errors.New("socket gone")}) // a failed ask forgets every held prompt
		if m.heldPrompt != nil || fallbackShown(m) {
			t.Fatal("a question held a moment ago isn't drawn from the transcript at once")
		}
		*now = now.Add(heldGrace)
		if !fallbackShown(m) {
			t.Fatalf("nothing holds it now: the transcript's view shows:\n%s", strip2(m.View()))
		}
	})
	t.Run("sent back", func(t *testing.T) {
		m, now := flashModel(t)
		stubHeld(t)
		m.applyHeld(heldMsg{prompts: []mcp.HeldPrompt{fruitHeld()}})
		m.applyStats(map[string]sessions.SessionStat{"live": waiting(*now, "input needed")})
		*now = now.Add(time.Minute)
		m.applyHeld(heldMsg{}) // handed back to its tab: still waiting
		if fallbackShown(m) {
			t.Fatal("held back within heldGrace of leaving the list")
		}
		*now = now.Add(heldGrace)
		if !fallbackShown(m) {
			t.Fatalf("still waiting in its tab: the transcript's view shows:\n%s", strip2(m.View()))
		}
	})
}

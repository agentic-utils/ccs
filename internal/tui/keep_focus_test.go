package tui

import (
	"slices"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/sessions"
)

// tick runs one message through Update that changes nothing by itself, so
// the focus rules Update applies afterwards get their turn.
func tick(m model) model {
	res, _ := m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	return res.(model)
}

func on(m model) string { return m.filtered[m.cursor].conv.SessionID }

// Answering a held question from ccs leaves the keys in that session's
// message box once the question has gone, as ^S would: typing goes there,
// not to the search, and the selection stays.
func TestAnswerLeavesTheChatFocused(t *testing.T) {
	m, calls, _ := permPane(t, mcp.HeldPrompt{ID: "p6", Session: "live", Tool: "Bash", Summary: "make deploy"})
	m = press(m, "a")
	if len(*calls) != 1 {
		t.Fatalf("a answers it: %+v", *calls)
	}
	m = tick(m)
	if m.chatFocus {
		t.Fatal("not while the answered question is still showing")
	}
	stubHeld(t)                               // the hook has its answer: nothing held now
	m.heldAt, m.heldBusy = time.Time{}, false // the next poll
	m = run(m, m.heldCmd())
	m.applyStats(map[string]sessions.SessionStat{"live": {Status: "busy", Since: time.Now()}})
	m = tick(m)
	if on(m) != "live" || !m.chatFocus || m.heldKeysActive() {
		t.Fatalf("the message box has the keys: on %s chat %v menu %v", on(m), m.chatFocus, m.heldKeysActive())
	}
	m = press(m, "h")
	if m.chatInput.Value() != "h" || m.textInput.Value() != "" {
		t.Errorf("typing goes to the message box: chat %q search %q", m.chatInput.Value(), m.textInput.Value())
	}
}

// When the same session asks again straight away, its next question's menu
// has the keys, not the message box.
func TestAnswerThenTheNextQuestionTakesTheKeys(t *testing.T) {
	m, calls, _ := permPane(t, mcp.HeldPrompt{ID: "p6", Session: "live", Tool: "Bash", Summary: "make deploy"})
	m = press(m, "a")
	if len(*calls) != 1 {
		t.Fatalf("answered: %+v", *calls)
	}
	stubHeld(t, mcp.HeldPrompt{ID: "p7", Session: "live", Tool: "Bash", Summary: "make test"})
	m.heldAt, m.heldBusy = time.Time{}, false // the next poll
	m = run(m, m.heldCmd())
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now().Add(time.Second), "permission prompt")})
	m = tick(m)
	if m.chatFocus || !m.heldKeysActive() || m.chatAfter != "" {
		t.Errorf("the next question's menu has the keys: chat %v menu %v pending %q", m.chatFocus, m.heldKeysActive(), m.chatAfter)
	}
}

// readingModel: "live" is selected, nothing typed for an hour, and another
// session ("menu") has a question ccs holds, waiting to be auto-selected.
func readingModel(t *testing.T) model {
	t.Helper()
	m := waitModel(t)
	m.width, m.height = 100, 40
	m.filtered = m.items
	m.lastKeyAt = time.Now().Add(-time.Hour)
	m.heldPrompt = map[string]mcp.HeldPrompt{"menu": {ID: "pm", Session: "menu", Tool: "Bash", At: time.Now()}}
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	return m
}

// Auto-select never takes you off a session that itself needs you, however
// long you've been reading it: a question ccs holds for it (even after Esc
// left its menu), a menu or prompt waiting in its tab, or unsent text in
// its box. It moves once the selection needs nothing.
func TestAutoSelectStaysWhileYouRead(t *testing.T) {
	m := readingModel(t)
	m.heldPrompt["live"] = mcp.HeldPrompt{ID: "pl", Session: "live", Tool: "Bash", At: time.Now()}
	if m.autoSelect(); on(m) != "live" {
		t.Errorf("a held question of its own, menu left with Esc: moved to %s", on(m))
	}

	m = readingModel(t)
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "input needed")})
	if m.autoSelect(); on(m) != "live" {
		t.Errorf("a menu waiting in its tab: moved to %s", on(m))
	}

	m = readingModel(t)
	m.chatFocus = true
	m.chatInput.SetValue("half a reply")
	if m.autoSelect(); on(m) != "live" {
		t.Errorf("a draft in its box: moved to %s", on(m))
	}

	m = readingModel(t)
	if m.autoSelect(); on(m) != "menu" || !m.heldFocus {
		t.Errorf("nothing needs you here: the held question is brought up: on %s pane %v", on(m), m.heldFocus)
	}
}

// The mouse counts as activity: a click or a wheel that moved the
// selection off a question ccs just brought up doesn't let the next one be
// selected in the same breath (two auto-selects in one second).
func TestAutoSelectWaitsAfterTheMouse(t *testing.T) {
	m := readingModel(t)
	m.heldPrompt["asks"] = mcp.HeldPrompt{ID: "pa", Session: "asks", Tool: "Bash", At: time.Now().Add(time.Second)}
	if m.autoSelect(); on(m) != "menu" {
		t.Fatalf("the oldest first: on %s", on(m))
	}
	// A click on "live" moves the selection off it.
	m.autoSelectAt = time.Now().Add(-time.Minute)
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m.selectionMoved()
	m.lastMouse = time.Now()
	if m.autoSelect(); on(m) != "live" {
		t.Errorf("just clicked: no auto-select, on %s", on(m))
	}
	m.lastMouse = time.Now().Add(-time.Minute)
	if m.autoSelect(); on(m) != "asks" {
		t.Errorf("idle again: the next held question, on %s", on(m))
	}
}

// ^T A doesn't switch to another session's tab while you're in ccs on a
// session waiting on you; out of ccs (blurred) or on one that needs
// nothing, it does.
func TestAutoFocusWaitsWhileYouReadInCCS(t *testing.T) {
	m := readingModel(t)
	m.heldPrompt = map[string]mcp.HeldPrompt{"live": {ID: "pl", Session: "live", Tool: "Bash"}}
	m.autoFocus = true
	m.livePIDs = map[string]int{"menu": 4242}
	m.chatStatus = map[string]sessions.SessionStat{"menu": waiting(time.Now(), "input needed")}
	if cmd := m.autoFocusCmd(); cmd != nil || m.focusBusy {
		t.Error("in ccs on a held question: no switching away")
	}
	m.blurred = true
	if cmd := m.autoFocusCmd(); cmd == nil {
		t.Error("ccs not focused: it switches")
	}
	m = readingModel(t)
	m.heldPrompt = nil
	m.autoFocus = true
	m.livePIDs = map[string]int{"menu": 4242}
	m.chatStatus = map[string]sessions.SessionStat{"menu": waiting(time.Now(), "input needed")}
	if cmd := m.autoFocusCmd(); cmd == nil {
		t.Error("in ccs on a session that needs nothing: it switches")
	}
}

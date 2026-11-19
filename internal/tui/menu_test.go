package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// menuClock stubs heldNow; tick moves it on.
func menuClock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := time.Now()
	old := heldNow
	heldNow = func() time.Time { return now }
	t.Cleanup(func() { heldNow = old })
	return func(d time.Duration) { now = now.Add(d) }
}

// menuPane is the bottom pane focused on a held question prompt, past the
// first-appearance pause.
func menuPane(t *testing.T, h mcp.HeldPrompt) (model, *[]heldCall, func(time.Duration)) {
	t.Helper()
	tick := menuClock(t)
	m, calls, _ := paneModel(t, h)
	m = keyType(m, tea.KeyEnter) // focus the pane
	if !m.heldFocus {
		t.Fatal("Enter focuses the pane")
	}
	tick(time.Second)
	return m, calls, tick
}

func colour() mcp.HeldPrompt {
	return mcp.HeldPrompt{ID: "q1", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{
		{Question: "Which colour?", Header: "Colour", Options: []string{"Red", "Green", "Blue"}, Descriptions: []string{"warm", "", "cool"}},
	}}
}

func twoQuestions() mcp.HeldPrompt {
	return mcp.HeldPrompt{ID: "q2", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{
		{Question: "Ship it?", Options: []string{"Yes", "No"}},
		{Question: "Which checks?", Multi: true, Options: []string{"lint", "tests", "vet"}},
	}}
}

// One question: ↑/↓ and j/k move the cursor over the options and Other…,
// Enter picks and it's sent at once, with no y/n.
func TestMenuSingleQuestion(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	v := strip2(m.View())
	for _, want := range []string{"Question 1 of 1 · Colour", "Which colour?", "1. Red", "warm", "Other…"} {
		if !strings.Contains(v, want) {
			t.Errorf("the menu lacks %q:\n%s", want, v)
		}
	}
	m = keyType(m, tea.KeyDown)
	m = press(m, "j")
	m = press(m, "k")
	m = keyType(m, tea.KeyUp)
	m = keyType(m, tea.KeyUp) // wraps to Other…
	m = keyType(m, tea.KeyDown)
	m = keyType(m, tea.KeyDown) // Green
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].choice != "answer" || (*calls)[0].labels[0][0] != "Green" || m.heldConfirmed != nil {
		t.Fatalf("Enter picks and sends at once: %+v", *calls)
	}
}

func TestMenuDigitPicks(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = press(m, "3")
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Blue" {
		t.Fatalf("a digit jumps to its option and picks it: %+v", *calls)
	}
	m, calls, _ = menuPane(t, colour())
	if m = press(m, "7"); len(*calls) != 0 || !strings.Contains(m.heldNote, "1-3") {
		t.Error("a digit past the options sends nothing")
	}
}

// A question that has just appeared ignores Enter, digits and Space for
// menuFresh; arrows work at once.
func TestMenuFirstAppearanceGuard(t *testing.T) {
	tick := menuClock(t)
	m, calls, _ := paneModel(t, colour())
	m = keyType(m, tea.KeyEnter) // focus: the menu shows now
	m = keyType(m, tea.KeyEnter)
	m = press(m, "1")
	if len(*calls) != 0 {
		t.Fatalf("an unread question isn't answered: %+v", *calls)
	}
	m = keyType(m, tea.KeyDown)
	tick(600 * time.Millisecond)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Green" {
		t.Errorf("the arrow moved at once; Enter picks once read: %+v", *calls)
	}
}

// Several questions: each answered in turn (a pick-any one with Space and
// digits, Enter confirming, an empty set refused), ← goes back keeping the
// answers, a review lists them, Esc goes back from it, Enter submits.
func TestMenuSeveralQuestions(t *testing.T) {
	m, calls, tick := menuPane(t, twoQuestions())
	m = press(m, "1") // Yes
	if len(*calls) != 0 || !strings.Contains(strip2(m.View()), "Question 2 of 2") {
		t.Fatalf("on to question 2: %+v\n%s", *calls, strip2(m.View()))
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter)
	if !strings.Contains(m.heldNote, "tick at least one") {
		t.Errorf("an empty pick-any set is refused: %q", m.heldNote)
	}
	m = press(m, " ") // tick lint
	m = press(m, "3") // tick vet
	m = press(m, "1") // untick lint
	m = press(m, "1") // tick lint again
	m = keyType(m, tea.KeyLeft)
	if v := strip2(m.View()); !strings.Contains(v, "Question 1 of 2") || !strings.Contains(v, "✓ 1. Yes") {
		t.Fatalf("← back, the answer kept:\n%s", v)
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter) // keeps Yes (cursor starts on it)
	tick(time.Second)
	if v := strip2(m.View()); !strings.Contains(v, "[x] 1. lint") || !strings.Contains(v, "[x] 3. vet") {
		t.Fatalf("question 2's ticks kept:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	v := strip2(m.View())
	if !strings.Contains(v, "Review your answers") || !strings.Contains(v, "Ship it? → Yes") || !strings.Contains(v, "Which checks? → lint, vet") || len(*calls) != 0 {
		t.Fatalf("the review:\n%s", v)
	}
	m = keyType(m, tea.KeyEsc)
	if !strings.Contains(strip2(m.View()), "Question 2 of 2") {
		t.Fatal("Esc goes back from the review")
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = keyType(m, tea.KeyEnter) // submit
	if len(*calls) != 1 || strings.Join((*calls)[0].labels[0], ",") != "Yes" || strings.Join((*calls)[0].labels[1], ",") != "lint,vet" {
		t.Errorf("submitted: %+v", *calls)
	}
}

// Other…: Enter opens a box; its text is the answer, flagged as Other; Esc
// closes the box without answering.
func TestMenuOther(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = keyType(m, tea.KeyUp) // Other…
	m = keyType(m, tea.KeyEnter)
	m = typeIn(m, "Te")
	m = keyType(m, tea.KeyEsc)
	if len(*calls) != 0 || !m.heldFocus {
		t.Fatal("Esc closes the box only")
	}
	m = keyType(m, tea.KeyEnter)
	m = typeIn(m, "al")
	if !strings.Contains(strip2(m.View()), "Teal") {
		t.Error("the box reopens with its text")
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Teal" || !(*calls)[0].other[0] {
		t.Errorf("Other's text, flagged: %+v", *calls)
	}
}

// T sends it back to its tab from any step; Esc at the first question
// leaves the pane and keeps the progress.
func TestMenuTAndEsc(t *testing.T) {
	m, calls, tick := menuPane(t, twoQuestions())
	m = press(m, "1")
	tick(time.Second)
	m = keyType(m, tea.KeyLeft)
	m = keyType(m, tea.KeyEsc)
	if m.heldFocus {
		t.Fatal("Esc at the first question leaves the pane")
	}
	m = keyType(m, tea.KeyEnter)
	if !m.heldFocus || !strings.Contains(strip2(m.View()), "✓ 1. Yes") {
		t.Fatalf("progress kept:\n%s", strip2(m.View()))
	}
	m = press(m, "t")
	if len(*calls) != 1 || (*calls)[0].choice != "mac" {
		t.Errorf("T sends it back: %+v", *calls)
	}
}

// After an auto-select, keys still being typed are dropped, Enter included.
func TestMenuAfterAutoSelect(t *testing.T) {
	tick := menuClock(t)
	m, calls, _ := paneModel(t, colour())
	m.lastKeyAt = heldNow().Add(-time.Hour)
	m.cursor = 0
	if m.filtered[0].conv.SessionID == "live" {
		m.cursor = 1
	}
	m.autoSelect()
	if !m.heldFocus {
		t.Fatal("auto-selected")
	}
	tick(700 * time.Millisecond)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || !strings.Contains(m.heldNote, "paused") {
		t.Errorf("dropped: %+v %q", *calls, m.heldNote)
	}
}

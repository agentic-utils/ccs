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

// menuPane is a held question prompt's session selected with the search
// empty, so its menu has the keys with no Enter first, past the
// first-appearance pause.
func menuPane(t *testing.T, h mcp.HeldPrompt) (model, *[]heldCall, func(time.Duration)) {
	t.Helper()
	tick := menuClock(t)
	m, calls, _ := paneModel(t, h)
	m.showMenu(m.heldShown())
	if !m.heldKeysActive() || m.heldFocus {
		t.Fatal("the menu has the keys at once, with no Enter")
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

func ctrl(m model, k tea.KeyType) model { return keyType(m, k) }

// One question: ↑/↓ and j/k move the cursor over the options and Other…
// with no Enter to focus first, the footer says what Enter sends, and
// Enter sends it, with no y/n. The pane says the menu has the keys.
func TestMenuSingleQuestion(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	v := strip2(m.View())
	for _, want := range []string{"also in its tab", "esc list", "Colour · 1 of 1", "Which colour?", "1. Red", "warm", "Other…", "enter sends: Red"} {
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
	if m.textInput.Value() != "" {
		t.Fatalf("j and k are the menu's, not the search's: %q", m.textInput.Value())
	}
	if v := strip2(m.View()); !strings.Contains(v, "enter sends: Green") {
		t.Errorf("the footer follows the cursor:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].choice != "answer" || (*calls)[0].labels[0][0] != "Green" || m.heldConfirmed != nil {
		t.Fatalf("Enter picks and sends at once: %+v", *calls)
	}
}

// A digit only highlights its option (a pick-any one: ticks it); Enter
// always sends, for every kind of question.
func TestMenuDigitHighlights(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = press(m, "3")
	if len(*calls) != 0 || !strings.Contains(strip2(m.View()), "enter sends: Blue") {
		t.Fatalf("a digit highlights, never sends: %+v", *calls)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Blue" {
		t.Fatalf("Enter sends the highlighted option: %+v", *calls)
	}
	m, calls, _ = menuPane(t, colour())
	if m = press(m, "7"); len(*calls) != 0 || !strings.Contains(m.heldNote, "1-3") {
		t.Error("a digit past the options sends nothing")
	}
}

// Letters the menu doesn't use go to the search, and once the search has
// text the menu lets go of every key.
func TestMenuLettersGoToTheSearch(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = press(m, "x")
	if m.textInput.Value() != "x" || m.heldKeysActive() {
		t.Fatalf("x goes to the search: %q, menu active %v", m.textInput.Value(), m.heldKeysActive())
	}
	m = press(m, "j")
	m = press(m, "1")
	if m.textInput.Value() != "xj1" || len(*calls) != 0 {
		t.Errorf("with the search typed, keys are the search's: %q %+v", m.textInput.Value(), *calls)
	}
}

// Esc leaves the menu for the list: ↑/↓ then move the list, coming back to
// it by selecting it again gives the menu the keys, as do → and Tab; ^P/^N
// always move the list, menu or not.
func TestMenuEscAndTheList(t *testing.T) {
	m, _, _ := menuPane(t, colour())
	on := func(m model) string { return m.filtered[m.cursor].conv.SessionID }
	if m = ctrl(m, tea.KeyCtrlN); on(m) == "live" {
		t.Fatal("^N moves the list while the menu has the keys")
	}
	if m = ctrl(m, tea.KeyCtrlP); on(m) != "live" || !m.heldKeysActive() {
		t.Fatalf("^P back: on %s, menu active %v", on(m), m.heldKeysActive())
	}
	m = keyType(m, tea.KeyEsc)
	if m.heldKeysActive() || !strings.Contains(strip2(m.View()), "answer it here") {
		t.Fatalf("Esc leaves the menu:\n%s", strip2(m.View()))
	}
	m = keyType(m, tea.KeyDown)
	if on(m) == "live" {
		t.Fatal("after Esc, ↓ moves the list")
	}
	m = keyType(m, tea.KeyUp)
	if on(m) != "live" || !m.heldKeysActive() {
		t.Fatalf("selected again, the menu has the keys: on %s active %v", on(m), m.heldKeysActive())
	}
	m = keyType(m, tea.KeyEsc)
	if m = keyType(m, tea.KeyRight); !m.heldKeysActive() {
		t.Error("→ gives the menu the keys again")
	}
	m = keyType(m, tea.KeyEsc)
	if m = keyType(m, tea.KeyTab); !m.showUsage {
		t.Errorf("Tab always opens the usage screen, even after Esc: usage %v", m.showUsage)
	}
}

// A question that has just appeared ignores Enter, digits and Space for
// menuFresh; arrows work at once.
func TestMenuFirstAppearanceGuard(t *testing.T) {
	tick := menuClock(t)
	m, calls, _ := paneModel(t, colour())
	m.showMenu(m.heldShown())
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

// The usage screen (Tab) hides the menu: no key reaches it there, even
// when it had focus from an auto-select, and coming back re-arms the
// first-appearance guard.
func TestMenuNotBehindTheUsageScreen(t *testing.T) {
	m, calls, tick := menuPane(t, colour())
	m.heldFocus = true
	m = keyType(m, tea.KeyTab)
	if !m.showUsage || m.heldFocus || m.heldKeysActive() {
		t.Fatalf("usage open, the menu loses the keys: usage %v focus %v", m.showUsage, m.heldFocus)
	}
	m = press(m, "2")
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 {
		t.Fatalf("a key on the usage screen answers nothing: %+v", *calls)
	}
	m = keyType(m, tea.KeyTab)
	if m.showUsage || !m.heldKeysActive() {
		t.Fatal("back from usage, the menu has the keys")
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 {
		t.Fatalf("shown again, the guard is re-armed: %+v", *calls)
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Red" {
		t.Errorf("then Enter sends: %+v", *calls)
	}
}

// Several questions: each answered in turn (a pick-any one with Space and
// digits, Enter confirming, an empty set refused), ← goes back keeping the
// answers, a review lists them, ← goes back from it, Enter submits.
func TestMenuSeveralQuestions(t *testing.T) {
	m, calls, tick := menuPane(t, twoQuestions())
	m = press(m, "1") // highlights Yes
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || !strings.Contains(strip2(m.View()), "2 of 2") {
		t.Fatalf("on to question 2: %+v\n%s", *calls, strip2(m.View()))
	}
	if strings.Contains(strip2(m.View()), "enter sends:") {
		t.Error("several questions end on the review, not a footer")
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
	if v := strip2(m.View()); !strings.Contains(v, "1 of 2") || !strings.Contains(v, "1. Yes ✓") {
		t.Fatalf("← back, the answer kept:\n%s", v)
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter) // keeps Yes (cursor starts on it)
	tick(time.Second)
	if v := strip2(m.View()); !strings.Contains(v, "1. [x] lint") || !strings.Contains(v, "3. [x] vet") {
		t.Fatalf("question 2's ticks kept:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	v := strip2(m.View())
	if !strings.Contains(v, "Review your answers") || !strings.Contains(v, "→ Yes") || !strings.Contains(v, "→ lint, vet") || len(*calls) != 0 {
		t.Fatalf("the review:\n%s", v)
	}
	m = keyType(m, tea.KeyLeft)
	if !strings.Contains(strip2(m.View()), "2 of 2") {
		t.Fatal("← goes back from the review")
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = keyType(m, tea.KeyEnter) // submit
	if len(*calls) != 1 || strings.Join((*calls)[0].labels[0], ",") != "Yes" || strings.Join((*calls)[0].labels[1], ",") != "lint,vet" {
		t.Errorf("submitted: %+v", *calls)
	}
}

// Other…: Enter opens a box (^A and ^K edit in it); its text is the
// answer, flagged as Other; Esc closes the box without answering.
func TestMenuOther(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = keyType(m, tea.KeyUp) // Other…
	m = keyType(m, tea.KeyEnter)
	m = typeIn(m, "xTe")
	m = ctrl(m, tea.KeyCtrlA)
	m = keyType(m, tea.KeyDelete)
	m = keyType(m, tea.KeyEsc)
	if len(*calls) != 0 || !m.heldKeysActive() || m.textInput.Value() != "" {
		t.Fatalf("Esc closes the box only; ^A edits the box, not the search: %q", m.textInput.Value())
	}
	m = keyType(m, tea.KeyEnter)
	m = typeIn(m, "al")
	if !strings.Contains(strip2(m.View()), "Teal") {
		t.Errorf("the box reopens with its text:\n%s", strip2(m.View()))
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Teal" || !(*calls)[0].other[0] {
		t.Errorf("Other's text, flagged: %+v", *calls)
	}
}

// A filled-in Other box left open, the selection moved away and back:
// Enter waits out the first-appearance guard before it sends.
func TestMenuOtherBoxGuardedWhenShownAgain(t *testing.T) {
	m, calls, tick := menuPane(t, colour())
	m = keyType(m, tea.KeyUp)
	m = keyType(m, tea.KeyEnter)
	m = typeIn(m, "Teal")
	m = ctrl(m, tea.KeyCtrlN)
	m = ctrl(m, tea.KeyCtrlP)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 {
		t.Fatalf("shown again, Enter in the box waits: %+v", *calls)
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Teal" {
		t.Errorf("then it sends: %+v", *calls)
	}
}

// n puts a note on a question's answer: prefilled when edited, Esc
// cancels, an empty note removes it; it shows dim under the question and
// in the footer, and goes with the answer.
func TestMenuNotes(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = press(m, "n")
	m = typeIn(m, "warm please")
	m = keyType(m, tea.KeyEnter)
	v := strip2(m.View())
	if !strings.Contains(v, "note: warm please") || !strings.Contains(v, "enter sends: Red · note: warm please") || len(*calls) != 0 {
		t.Fatalf("the note shows, nothing sent:\n%s", v)
	}
	m = press(m, "n")
	if got := m.menus["q1"].box.Value(); got != "warm please" {
		t.Errorf("prefilled: %q", got)
	}
	m = typeIn(m, " no")
	m = keyType(m, tea.KeyEsc)
	if m.menus["q1"].notes[0] != "warm please" {
		t.Errorf("Esc cancels: %q", m.menus["q1"].notes[0])
	}
	m = press(m, "n")
	m = ctrl(m, tea.KeyCtrlU)
	m = keyType(m, tea.KeyEnter)
	if _, ok := m.menus["q1"].notes[0]; ok || strings.Contains(strip2(m.View()), "note:") {
		t.Error("an empty note removes it")
	}
	m = press(m, "n")
	m = typeIn(m, "brighter")
	m = keyType(m, tea.KeyEnter)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Red" || (*calls)[0].notes[0] != "brighter" {
		t.Errorf("the note goes with the answer: %+v", *calls)
	}
}

// The review shows each question (cut if it must be), then the whole
// answer and note, wrapped: exactly what will be sent.
func TestMenuReviewShowsTheWholeAnswer(t *testing.T) {
	long := mcp.HeldPrompt{ID: "q3", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{
		{Question: "Which of these many interesting and lengthy options would you choose for the release " + strings.Repeat("plan ", 20), Options: []string{"A", "B"}},
		{Question: "Ship it?", Options: []string{"Yes", "No"}},
	}}
	m, calls, tick := menuPane(t, long)
	answer := "a long answer of my own " + strings.Repeat("with many words ", 8) + "and the very end"
	m = keyType(m, tea.KeyUp)
	m = keyType(m, tea.KeyEnter)
	m = typeIn(m, answer)
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = press(m, "n")
	m = typeIn(m, "a note that also runs long "+strings.Repeat("and on ", 10)+"to its end")
	m = keyType(m, tea.KeyEnter)
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	lines := m.menuLines(long, 40, true)
	joined := strings.Join(strings.Fields(strip2(strings.Join(lines, " "))), " ")
	for _, want := range []string{"Review your answers", "→ “" + strings.Join(strings.Fields(answer), " ") + "”", "and on to its end", "→ Yes"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the review lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	for _, l := range lines {
		if w := len([]rune(strip2(l))); w > 41 {
			t.Errorf("a line wider than the pane (%d): %q", w, strip2(l))
		}
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != answer || (*calls)[0].notes[1] == "" {
		t.Errorf("what the review showed is sent: %+v", *calls)
	}
}

// A prompt replaced while being answered (the same session, a new held
// prompt with a new id) starts its own menu afresh; the old one's
// progress is forgotten.
func TestMenuPromptReplaced(t *testing.T) {
	m, calls, _ := menuPane(t, twoQuestions())
	m = keyType(m, tea.KeyEnter) // Yes: on to question 2
	if m.menus["q2"].step != 1 {
		t.Fatal("on question 2")
	}
	next := twoQuestions()
	next.ID = "q2-new"
	heldPrompts = func() ([]mcp.HeldPrompt, error) { return []mcp.HeldPrompt{next}, nil }
	m.heldAt, m.heldBusy = time.Time{}, false
	m = run(m, m.heldCmd())
	m.showMenu(m.heldShown())
	if m.menus["q2"] != nil || m.menus["q2-new"] == nil || m.menus["q2-new"].step != 0 {
		t.Fatalf("the new prompt's menu starts afresh: %+v", m.menus)
	}
	if v := strip2(m.View()); !strings.Contains(v, "1 of 2") {
		t.Errorf("the new prompt shows from its first question:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter) // within its guard: nothing
	if len(*calls) != 0 {
		t.Errorf("nothing sent for either: %+v", *calls)
	}
}

// T sends it back to its tab from any step; Esc at the first question
// leaves the menu and keeps the progress.
func TestMenuTAndEsc(t *testing.T) {
	m, calls, tick := menuPane(t, twoQuestions())
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = keyType(m, tea.KeyLeft)
	m = keyType(m, tea.KeyEsc)
	if m.heldKeysActive() {
		t.Fatal("Esc at the first question leaves the menu")
	}
	m = keyType(m, tea.KeyRight)
	if !m.heldKeysActive() || !strings.Contains(strip2(m.View()), "1. Yes ✓") {
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
	m = press(m, "3")
	if len(*calls) != 0 {
		t.Errorf("a digit after an auto-select never sends: %+v", *calls)
	}
}

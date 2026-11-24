package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// typeLikeATerminal types s as a terminal sends it: Space as KeySpace, the rest as
// runes, one key at a time.
func typeLikeATerminal(m model, s string) model {
	for _, r := range s {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		if r == ' ' {
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{r}}
		}
		res, cmd := m.Update(msg)
		m = run(res.(model), cmd)
	}
	return m
}

// The Other… row is a text field, as in Claude Code's own menu: with ❯ on
// it every key typed is the answer's (n opens no note, digits pick
// nothing, Space ticks nothing, j/k move nothing), with no Enter first;
// Enter sends it, flagged as Other.
func TestMenuOtherFieldTakesEveryKey(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = keyType(m, tea.KeyUp) // Other…
	mn := m.menus["q1"]
	if mn.editing != boxOther {
		t.Fatalf("❯ on Other… gives its field the keys: editing %d", mn.editing)
	}
	const answer = "no, need 2 jk bots? 1 3 n"
	m = typeLikeATerminal(m, answer)
	if mn.box.Value() != answer || mn.drafts[0] != answer {
		t.Fatalf("every key went into the field: box %q drafts %q", mn.box.Value(), mn.drafts[0])
	}
	if len(mn.notes) != 0 || len(mn.picks[0]) != 0 || m.textInput.Value() != "" || len(*calls) != 0 {
		t.Fatalf("nothing else took a key: notes %v picks %v search %q calls %+v", mn.notes, mn.picks, m.textInput.Value(), *calls)
	}
	if v := strip2(m.View()); !strings.Contains(v, "❯ 4. "+answer) || !strings.Contains(v, "enter sends → (Other) "+answer) {
		t.Fatalf("the text is drawn on the row itself, and the footer says it's what Enter sends:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != answer || !(*calls)[0].other[0] {
		t.Fatalf("Enter sends the Other answer: %+v", *calls)
	}
}

// ↑/↓ move off the field keeping its text, and back onto it with the text
// and the cursor where it was; editing works as in the other boxes (^A
// goes to the field's start, not the search); Esc leaves for the list
// keeping the draft, and selecting the session again resumes it.
func TestMenuOtherFieldKeepsItsDraft(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = press(m, "4") // Other… is numbered 4: its field has the keys
	mn := m.menus["q1"]
	if mn.cursor != 3 || mn.editing != boxOther {
		t.Fatalf("4 highlights Other…: cursor %d editing %d", mn.cursor, mn.editing)
	}
	m = typeLikeATerminal(m, "why a bot")
	m = keyType(m, tea.KeyUp)
	if mn.editing != boxNone || mn.cursor != 2 || mn.drafts[0] != "why a bot" {
		t.Fatalf("↑ moves off, the draft kept: editing %d cursor %d drafts %v", mn.editing, mn.cursor, mn.drafts)
	}
	if v := strip2(m.View()); !strings.Contains(v, "4. why a bot") {
		t.Errorf("the draft stays on the row:\n%s", v)
	}
	m = keyType(m, tea.KeyDown)
	m = ctrl(m, tea.KeyCtrlA)
	m = typeLikeATerminal(m, "so ")
	if mn.box.Value() != "so why a bot" || m.textInput.Value() != "" {
		t.Fatalf("back on it, ^A edits the field: %q search %q", mn.box.Value(), m.textInput.Value())
	}
	m = keyType(m, tea.KeyEsc)
	if m.heldKeysActive() || mn.drafts[0] != "so why a bot" || len(*calls) != 0 {
		t.Fatalf("Esc leaves for the list, the draft kept: active %v drafts %v", m.heldKeysActive(), mn.drafts)
	}
	m = keyType(m, tea.KeyRight) // the menu again
	m = ctrl(m, tea.KeyCtrlE)
	m = typeLikeATerminal(m, "?")
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "so why a bot?" || !(*calls)[0].other[0] {
		t.Fatalf("the draft is sent: %+v", *calls)
	}
}

// Enter on an empty field sends nothing and says to type something; a
// note written first isn't the answer, and says so.
func TestMenuOtherFieldEmpty(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = press(m, "n") // a note on Red
	m = typeLikeATerminal(m, "why does it need a bot")
	m = keyType(m, tea.KeyEnter)
	m = keyType(m, tea.KeyUp) // Other…
	mn := m.menus["q1"]
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || mn.editing != boxOther || !strings.Contains(m.heldNote, "your note isn't the answer") {
		t.Fatalf("an empty Other… sends nothing and says why: %q editing %d", m.heldNote, mn.editing)
	}
	m = typeLikeATerminal(m, "no bot")
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "no bot" || !(*calls)[0].other[0] || (*calls)[0].notes[0] != "why does it need a bot" {
		t.Fatalf("the answer goes with its note: %+v", *calls)
	}
}

// Several questions: Enter in the field moves on, the review shows the
// Other answer, and ← back lands on the field with its text.
func TestMenuOtherFieldMovesOn(t *testing.T) {
	m, calls, tick := menuPane(t, twoQuestions())
	m = press(m, "3") // Other… on a two-option question
	m = typeLikeATerminal(m, "not 1 n now")
	m = keyType(m, tea.KeyEnter)
	mn := m.menus["q2"]
	if mn.step != 1 || mn.other[0] != "not 1 n now" || mn.editing != boxNone || len(*calls) != 0 {
		t.Fatalf("Enter moves to the next question: step %d other %v editing %d", mn.step, mn.other, mn.editing)
	}
	tick(time.Second)
	m = typeLikeATerminal(m, " ") // tick lint
	m = keyType(m, tea.KeyEnter)
	if mn.step != 2 || !strings.Contains(strip2(m.View()), "(Other) not 1 n now") {
		t.Fatalf("the review shows the Other answer:\n%s", strip2(m.View()))
	}
	m = keyType(m, tea.KeyLeft) // question 2
	m = keyType(m, tea.KeyLeft) // question 1, on its answer: the field
	if mn.step != 0 || mn.editing != boxOther || mn.box.Value() != "not 1 n now" {
		t.Fatalf("back on question 1, its field has its text: step %d editing %d box %q", mn.step, mn.editing, mn.box.Value())
	}
}

// A pick-any question, as Claude Code does it: text typed in the field
// ticks it, beside the other ticks, and the answer joins them ("Red,
// violet"), flagged as Other; clearing the text unticks it.
func TestMenuOtherFieldInPickAny(t *testing.T) {
	m, calls, tick := menuPane(t, twoQuestions())
	m = keyType(m, tea.KeyEnter) // Ship it? Yes
	tick(time.Second)
	mn := m.menus["q2"]
	m = typeLikeATerminal(m, " ")      // tick lint
	m = press(m, "4")                  // Other…
	m = typeLikeATerminal(m, "race 2") // Space and digits are letters here
	if v := strip2(m.View()); !strings.Contains(v, "[x] race 2") || !strings.Contains(v, "[x] lint") {
		t.Fatalf("typed text ticks Other…, beside lint:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	if mn.step != 2 || mn.other[1] != "race 2" {
		t.Fatalf("Enter confirms the ticks and the text: step %d other %v", mn.step, mn.other)
	}
	if v := strip2(m.View()); !strings.Contains(v, "lint, (Other) race 2") {
		t.Fatalf("the review shows both:\n%s", v)
	}
	tick(time.Second)
	m = redraw(m)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[1][0] != "lint, race 2" || !(*calls)[0].other[1] {
		t.Fatalf("one answer, joined as Claude Code sends it: %+v", *calls)
	}
}

func TestMenuOtherFieldClearedUnticks(t *testing.T) {
	m, _, tick := menuPane(t, twoQuestions())
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = press(m, "4")
	m = typeLikeATerminal(m, "x")
	m = ctrl(m, tea.KeyCtrlU)
	m = keyType(m, tea.KeyEnter)
	mn := m.menus["q2"]
	if mn.step != 1 || !strings.Contains(m.heldNote, "tick at least one") {
		t.Fatalf("an emptied field ticks nothing: step %d note %q", mn.step, m.heldNote)
	}
	if v := strip2(m.View()); !strings.Contains(v, "[ ] ") || strings.Contains(v, "[x]") {
		t.Fatalf("nothing ticked:\n%s", v)
	}
}

// The first-appearance guard covers the field: letters and Enter typed
// just as a question shows with ❯ on it land nowhere.
func TestMenuOtherFieldGuardedWhenShown(t *testing.T) {
	m, calls, tick := menuPane(t, colour())
	m = keyType(m, tea.KeyUp)
	m = typeLikeATerminal(m, "teal")
	mn := m.menus["q1"]
	mn.shownAt = heldNow()
	m = typeLikeATerminal(m, "xyz")
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || mn.box.Value() != "teal" || !strings.Contains(m.heldNote, "again") {
		t.Fatalf("held back, and Enter says so: box %q note %q %+v", mn.box.Value(), m.heldNote, *calls)
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "teal" {
		t.Fatalf("then it sends: %+v", *calls)
	}
}

// n on a single-select option chooses it: the note is drawn under it,
// it's marked chosen, and Enter after the note sends it with the note.
// A pick-any question's n ticks nothing.
func TestMenuNoteChoosesTheOption(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = keyType(m, tea.KeyDown) // Green
	m = press(m, "n")
	mn := m.menus["q1"]
	if len(mn.picks[0]) != 1 || mn.picks[0][0] != 1 {
		t.Fatalf("n chooses the highlighted option: %v", mn.picks)
	}
	m = typeLikeATerminal(m, "the darker one")
	m = keyType(m, tea.KeyEnter)
	if v := strip2(m.View()); !strings.Contains(v, "Green ✓") || !strings.Contains(v, "note: the darker one") {
		t.Fatalf("chosen, with its note:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Green" || (*calls)[0].notes[0] != "the darker one" {
		t.Fatalf("Enter sends it with the note: %+v", *calls)
	}

	m, _, tick := menuPane(t, twoQuestions())
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = press(m, "n")
	if p := m.menus["q2"].picks[1]; len(p) != 0 {
		t.Fatalf("a pick-any question's note ticks nothing: %v", p)
	}
}

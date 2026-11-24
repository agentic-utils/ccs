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

// In the Other… box every key typed is the answer's: n opens no note,
// digits pick nothing, Space ticks nothing, j/k move nothing; Enter sends
// it, flagged as Other.
func TestMenuOtherBoxTakesEveryKey(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = keyType(m, tea.KeyUp) // Other…
	m = keyType(m, tea.KeyEnter)
	const answer = "no, need 2 jk bots? 1 3 n"
	m = typeLikeATerminal(m, answer)
	mn := m.menus["q1"]
	if mn.editing != boxOther || mn.box.Value() != answer {
		t.Fatalf("every key went into the Other box: editing %d, box %q", mn.editing, mn.box.Value())
	}
	if len(mn.notes) != 0 || len(mn.picks[0]) != 0 || m.textInput.Value() != "" || len(*calls) != 0 {
		t.Fatalf("nothing else took a key: notes %v picks %v search %q calls %+v", mn.notes, mn.picks, m.textInput.Value(), *calls)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != answer || !(*calls)[0].other[0] {
		t.Fatalf("Enter sends the Other answer: %+v", *calls)
	}
}

// Typing on the Other… row opens its box with what was typed, as Claude
// Code's own menu does, rather than the search taking it; its number
// highlights it; Esc keeps the draft and leaves the box.
func TestMenuOtherRowTyping(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = press(m, "4") // Other… is numbered 4
	mn := m.menus["q1"]
	if mn.cursor != 3 || mn.editing != boxNone {
		t.Fatalf("4 highlights Other…: cursor %d editing %d", mn.cursor, mn.editing)
	}
	m = typeLikeATerminal(m, "why a bot")
	if mn.editing != boxOther || mn.box.Value() != "why a bot" || m.textInput.Value() != "" {
		t.Fatalf("typing on Other… starts the answer: editing %d box %q search %q", mn.editing, mn.box.Value(), m.textInput.Value())
	}
	m = keyType(m, tea.KeyEsc)
	if mn.editing != boxNone || mn.drafts[0] != "why a bot" || len(*calls) != 0 {
		t.Fatalf("Esc keeps the draft: editing %d drafts %v", mn.editing, mn.drafts)
	}
	m = keyType(m, tea.KeyEnter) // reopens with the draft
	m = typeLikeATerminal(m, "?")
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "why a bot?" || !(*calls)[0].other[0] {
		t.Fatalf("the draft is sent: %+v", *calls)
	}
}

// The reported loop: a note written first shows under Other… and looks
// like its answer, so Enter on the empty box seemed to do nothing. With a
// box open the note isn't drawn, and an empty Other… says the note isn't
// the answer. Only n outside a box opens a note.
func TestMenuNoteIsNotTheOtherAnswer(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = keyType(m, tea.KeyUp) // Other…
	m = press(m, "n")
	m = typeLikeATerminal(m, "why does it need a bot")
	m = keyType(m, tea.KeyEnter)
	mn := m.menus["q1"]
	if mn.notes[0] != "why does it need a bot" || mn.editing != boxNone {
		t.Fatalf("n outside a box writes a note: %v editing %d", mn.notes, mn.editing)
	}
	m = keyType(m, tea.KeyEnter) // opens Other…, empty
	if strings.Contains(strip2(m.View()), "note: why") {
		t.Errorf("with the Other box open the note isn't drawn under it:\n%s", strip2(m.View()))
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || mn.editing != boxOther || !strings.Contains(m.heldNote, "your note isn't the answer") {
		t.Fatalf("an empty Other… sends nothing and says why: %q editing %d", m.heldNote, mn.editing)
	}
	m = typeLikeATerminal(m, "no bot")
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "no bot" || (*calls)[0].notes[0] != "why does it need a bot" {
		t.Fatalf("the answer goes with its note: %+v", *calls)
	}
}

// Several questions: Enter in the Other box moves on, and the review
// shows the Other answer.
func TestMenuOtherBoxMovesOn(t *testing.T) {
	m, calls, tick := menuPane(t, twoQuestions())
	m = press(m, "3") // Other… on a two-option question
	m = keyType(m, tea.KeyEnter)
	m = typeLikeATerminal(m, "not 1 n now")
	m = keyType(m, tea.KeyEnter)
	mn := m.menus["q2"]
	if mn.step != 1 || mn.other[0] != "not 1 n now" || len(*calls) != 0 {
		t.Fatalf("Enter moves to the next question: step %d other %v", mn.step, mn.other)
	}
	tick(time.Second)
	m = typeLikeATerminal(m, " ") // tick lint
	m = keyType(m, tea.KeyEnter)
	if mn.step != 2 || !strings.Contains(strip2(m.View()), "(Other) not 1 n now") {
		t.Fatalf("the review shows the Other answer:\n%s", strip2(m.View()))
	}
}

// Enter in an open box just after the menu shows again is held back, and
// says so rather than seeming stuck.
func TestMenuOtherBoxFreshEnterSaysSo(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m = keyType(m, tea.KeyUp)
	m = keyType(m, tea.KeyEnter)
	m = typeLikeATerminal(m, "teal")
	m.menus["q1"].shownAt = heldNow()
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || !strings.Contains(m.heldNote, "again") {
		t.Fatalf("a held-back Enter says so: %q %+v", m.heldNote, *calls)
	}
}

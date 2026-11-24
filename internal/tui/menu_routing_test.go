package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/sessions"
)

// redraw runs a message that changes nothing, so Update's after-every-
// message bookkeeping (showMenu, noteReview) sees the current state.
func redraw(m model) model {
	res, cmd := m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	return run(res.(model), cmd)
}

// A held prompt whose session's status says it isn't waiting (a stale busy
// or idle) has no menu drawn, so the menu takes no keys and its appearance
// guard doesn't start; once it's drawn, the guard counts from that draw.
func TestMenuKeysOnlyWhileDrawn(t *testing.T) {
	tick := menuClock(t)
	m, calls, _ := paneModel(t, colour())
	m.applyStats(map[string]sessions.SessionStat{"live": {Status: "busy"}})
	m = redraw(m)
	tick(5 * time.Second)
	if strings.Contains(strip2(m.View()), "Which colour?") {
		t.Fatalf("a stale non-waiting status draws no menu:\n%s", strip2(m.View()))
	}
	if m.heldKeysActive() {
		t.Fatal("an undrawn menu takes no keys")
	}
	m = keyType(m, tea.KeyEnter)
	m = press(m, "1")
	if len(*calls) != 0 {
		t.Fatalf("an undrawn menu answered: %+v", *calls)
	}
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "input needed")})
	m = redraw(m)
	if !strings.Contains(strip2(m.View()), "Which colour?") || !m.heldKeysActive() {
		t.Fatalf("waiting again, the menu is drawn and has the keys:\n%s", strip2(m.View()))
	}
	m = keyType(m, tea.KeyEnter) // the guard counts from this first draw, not from the hold
	if len(*calls) != 0 {
		t.Fatalf("Enter right after the first draw answered: %+v", *calls)
	}
	tick(time.Second)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Red" {
		t.Fatalf("once read, Enter sends: %+v", *calls)
	}
}

// Typing into the search takes the keys from a held menu, even one focused
// by hand or by an auto-select: digits, j/k and Enter go to the search and
// nothing is sent. Clearing the search doesn't give them back; → does.
func TestMenuSearchTakesTheKeys(t *testing.T) {
	m, calls, _ := menuPane(t, colour())
	m.heldFocus = true
	m = press(m, "x")
	m = press(m, "y")
	if m.textInput.Value() != "xy" || m.heldKeysActive() || m.heldFocus {
		t.Fatalf("typing searches and the menu loses the keys: search %q active %v focus %v", m.textInput.Value(), m.heldKeysActive(), m.heldFocus)
	}
	for _, k := range []string{"1", "j", "k", "2"} {
		m = press(m, k)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || !strings.HasPrefix(m.textInput.Value(), "xy1jk2") {
		t.Fatalf("while searching, keys go to the search: calls %+v search %q", *calls, m.textInput.Value())
	}
	m = ctrl(m, tea.KeyCtrlU)
	if m.textInput.Value() != "" {
		t.Fatalf("^U clears the search: %q", m.textInput.Value())
	}
	on := func(m model) string { return m.filtered[m.cursor].conv.SessionID }
	if on(m) == "live" && m.heldKeysActive() {
		t.Fatal("clearing the search doesn't give the menu the keys back by itself")
	}
	// Selecting the session again gives them back (the search may have moved
	// the selection while it filtered the list).
	for i := 0; i < 8 && on(m) != "live"; i++ {
		m = keyType(m, tea.KeyDown)
	}
	for i := 0; i < 8 && on(m) != "live"; i++ {
		m = keyType(m, tea.KeyUp)
	}
	if on(m) != "live" || !m.heldKeysActive() {
		t.Fatalf("selected again, the menu has the keys: on %s active %v", on(m), m.heldKeysActive())
	}
	if len(*calls) != 0 {
		t.Fatalf("nothing was answered: %+v", *calls)
	}
}

// usageChord is ^T Y: the usage screen, even while sessions wait.
func usageChord(m model) model {
	return press(keyType(m, tea.KeyCtrlT), "y")
}

// The menu never catches Tab: with the menu active, focused, and after Esc,
// Tab goes to the next waiting session (here its own, the only one) and
// answers nothing; ^T Y opens the usage screen. The menu never catches a
// ^X chord either.
func TestMenuTabOpensUsage(t *testing.T) {
	for name, set := range map[string]func(model) model{
		"active":    func(m model) model { return m },
		"focused":   func(m model) model { m.heldFocus = true; return m },
		"after esc": func(m model) model { return keyType(m, tea.KeyEsc) },
	} {
		t.Run(name, func(t *testing.T) {
			m, calls, _ := menuPane(t, colour())
			m = set(m)
			if m = keyType(m, tea.KeyTab); m.showUsage || len(*calls) != 0 || m.filtered[m.cursor].conv.SessionID != "live" {
				t.Fatalf("Tab goes to the waiting session, never to the menu (%s): usage %v calls %v", name, m.showUsage, *calls)
			}
			if m = usageChord(m); !m.showUsage {
				t.Fatalf("^T Y opens the usage screen (%s)", name)
			}
			if m.heldKeysActive() {
				t.Fatal("behind the usage screen the menu takes no keys")
			}
			m = press(m, "1")
			m = keyType(m, tea.KeyEnter)
			if len(*calls) != 0 {
				t.Fatalf("nothing answered behind the usage screen: %+v", *calls)
			}
		})
	}
	m, _, _ := menuPane(t, colour())
	m = ctrl(m, tea.KeyCtrlX)
	if !m.chordX || m.heldKeysActive() {
		t.Fatalf("^X starts a chord, not a menu key: chord %v active %v", m.chordX, m.heldKeysActive())
	}
}

// A review taller than the pane scrolls, says there's more, and won't send
// until its last line has been on screen; then Enter sends what it showed.
func TestMenuReviewScrollsBeforeSending(t *testing.T) {
	long := mcp.HeldPrompt{ID: "q9", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{
		{Question: "Which plan?", Options: []string{"A", "B"}},
		{Question: "Ship it?", Options: []string{"Yes", "No"}},
	}}
	m, calls, tick := menuPane(t, long)
	m.height = 14 // the pane gives the question 7 rows
	answer := "my own plan " + strings.Repeat("with a great many careful words ", 10) + "THE END"
	m = keyType(m, tea.KeyUp) // Other…
	m = keyType(m, tea.KeyEnter)
	m = typeIn(m, answer)
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = press(m, "n")
	m = typeIn(m, "a note "+strings.Repeat("that goes on ", 12)+"NOTE END")
	m = keyType(m, tea.KeyEnter)
	m = keyType(m, tea.KeyEnter) // Yes: on to the review
	tick(time.Second)
	m = redraw(m)
	v := strip2(m.View())
	if !strings.Contains(v, "Review · 2 answers") || !strings.Contains(v, "↓ more") || strings.Contains(v, "NOTE END") {
		t.Fatalf("a tall review shows its start and says there's more:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || !strings.Contains(strip2(m.View()), "scroll to the end first") {
		t.Fatalf("Enter before the end is refused: calls %+v\n%s", *calls, strip2(m.View()))
	}
	pane := func(m model) string { return strip2(strings.Join(m.chatAsk(), "\n")) }
	for i := 0; i < 40 && strings.Contains(pane(m), "↓ more"); i++ {
		m = keyType(m, tea.KeyDown)
	}
	v = pane(m)
	if strings.Contains(v, "↓ more") || !strings.Contains(strings.Join(strings.Fields(v), " "), "NOTE END") {
		t.Fatalf("scrolled to the end, the last lines show:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	if strings.Contains(v, "scroll to the end first") {
		t.Errorf("once the end is seen, the refusal goes:\n%s", v)
	}
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != answer || (*calls)[0].labels[1][0] != "Yes" {
		t.Fatalf("seen to the end, Enter sends what it showed: %+v", *calls)
	}
}

// A review that fits sends at once.
func TestMenuShortReviewSendsAtOnce(t *testing.T) {
	m, calls, tick := menuPane(t, twoQuestions())
	m = keyType(m, tea.KeyEnter) // Yes
	tick(time.Second)
	m = press(m, " ") // tick lint
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = redraw(m)
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 {
		t.Fatalf("a review that fits sends on Enter: %+v\n%s", *calls, strip2(m.View()))
	}
}

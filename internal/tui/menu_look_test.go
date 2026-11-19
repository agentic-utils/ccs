package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// writeAccess is a two-option question with descriptions long enough to
// wrap past two lines at any of the widths tested.
func writeAccess() mcp.HeldPrompt {
	return mcp.HeldPrompt{ID: "wa", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{{
		Question: "In that form, should 'allow answering and sending' (--allow-write) start off or on?",
		Header:   "Write access",
		Options:  []string{"Off; tick to turn on (Recommended)", "Ask as a separate step"},
		Descriptions: []string{
			"Read-only unless you tick it, with a line saying what it allows (send messages, answer questions, launch, close and restart sessions from the phone). Costs: one tick. Leaves open: nothing at all.",
			"After the form, a y/n screen spelling out what write access allows. Costs: an extra step every time you set it up. Leaves open: nothing at all.",
		},
	}}}
}

// paneText is the bottom pane's held prompt as drawn, colours stripped.
func paneText(m model) []string { return strings.Split(strip2(strings.Join(m.chatAsk(), "\n")), "\n") }

// The pane looks like Claude Code's menu at 80 and 120 columns: one header
// line ("Write access · 1 of 1", "also in its tab" on the right), the question
// wrapped, options numbered with "❯" on the highlighted one, descriptions
// wrapped and indented under their label, two lines at most then "…",
// "Other…" numbered, one hint row, every line within the pane; and no
// "only shown in its own tab" footer for a held prompt.
func TestMenuPaneLook(t *testing.T) {
	for _, width := range []int{80, 120} {
		m, _, _ := menuPane(t, writeAccess())
		m.width = width
		m = redraw(m)
		lines := paneText(m)
		pw := m.paneWidth()
		var heads, hintRows int
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > pw {
				t.Errorf("%d cols: line %d wider than the pane (%d > %d): %q", width, i, w, pw, l)
			}
			if strings.Contains(l, "also in its tab") {
				heads++
				if !strings.Contains(l, "Write access · 1 of 1") {
					t.Errorf("%d cols: the header line: %q", width, l)
				}
			}
			if strings.Contains(l, "esc list") {
				hintRows++
				// fitted to the pane: ↑↓ goes first when it's narrow
				if !strings.Contains(l, "enter pick · n note · esc list") {
					t.Errorf("%d cols: the hint row: %q", width, l)
				}
			}
		}
		if heads != 1 || hintRows != 1 {
			t.Errorf("%d cols: %d header lines, %d hint rows, want 1 and 1:\n%s", width, heads, hintRows, strings.Join(lines, "\n"))
		}
		joined := strings.Join(lines, "\n")
		flat := strings.Join(strings.Fields(joined), " ")
		for _, want := range []string{"❯ 1. Off; tick to turn on (Recommended)", "2. Ask as a separate step", "3. Other…", "enter sends: Off; tick to turn on (Recommended)"} {
			if !strings.Contains(flat, want) {
				t.Errorf("%d cols: lacks %q:\n%s", width, want, joined)
			}
		}
		// The first option's description: indented under its label, at most
		// two lines, the second ending "…" since there was more.
		i := indexOf(lines, "1. Off")
		desc := 0
		for _, l := range lines[i+1:] {
			if !strings.HasPrefix(l, "     ") || strings.TrimSpace(l) == "" {
				break
			}
			desc++
			if desc == 2 && !strings.HasSuffix(strings.TrimRight(l, " "), "…") {
				t.Errorf("%d cols: a cut description ends with …: %q", width, l)
			}
		}
		if desc < 1 || desc > 2 {
			t.Errorf("%d cols: description lines %d, want 1 or 2:\n%s", width, desc, joined)
		}
		v := strip2(m.View())
		for _, gone := range []string{"only shown in its own tab", "answer it in its tab", "held by ccs", "not on screen in its tab"} {
			if strings.Contains(v, gone) {
				t.Errorf("%d cols: %q shows for a held prompt", width, gone)
			}
		}
	}
}

func indexOf(lines []string, sub string) int {
	for i, l := range lines {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

// The hint row shows only the keys that work at each step.
func TestMenuHintRowPerStep(t *testing.T) {
	m, _, tick := menuPane(t, twoQuestions())
	m.width = 140 // wide enough for every pair; TestMenuHintRowFits covers narrow panes
	hint := func(m model) string {
		for _, l := range paneText(m) {
			if strings.Contains(l, " · ") && (strings.Contains(l, "esc ") || strings.Contains(l, "enter ")) && !strings.Contains(l, "also in its tab") {
				return strings.TrimSpace(l)
			}
		}
		return ""
	}
	if h := hint(m); h != "↑↓ move · enter pick · n note · esc list" {
		t.Errorf("first question: %q", h)
	}
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	if h := hint(m); h != "↑↓ move · space tick · enter confirm · n note · ← back · esc list" {
		t.Errorf("a pick-any second question: %q", h)
	}
	m = keyType(m, tea.KeyUp) // Other…
	m = keyType(m, tea.KeyEnter)
	if h := hint(m); h != "enter save · esc cancel" {
		t.Errorf("the Other box: %q", h)
	}
	m = keyType(m, tea.KeyEsc)  // closes the box
	m = keyType(m, tea.KeyDown) // from Other… round to lint
	m = press(m, " ")           // tick lint
	m = keyType(m, tea.KeyEnter)
	tick(time.Second)
	m = redraw(m)
	if h := hint(m); h != "↑↓ scroll · enter send · ← back · esc list" {
		t.Errorf("the review: %q\n%s", h, strings.Join(paneText(m), "\n"))
	}
}

// A permission prompt gets the same header and hint row.
func TestPermissionPaneLook(t *testing.T) {
	m, _, _ := paneModel(t, mcp.HeldPrompt{ID: "p", Session: "live", Tool: "Bash", Summary: "make deploy"})
	joined := strings.Join(paneText(m), "\n")
	for _, want := range []string{"Bash · permission", "also in its tab", "make deploy", "a allow · d deny · enter open its tab · esc list"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lacks %q:\n%s", want, joined)
		}
	}
}

// An answer's outcome belongs to its session: it shows while that session
// is selected, never on another session's pane, and goes when the
// selection moves.
func TestMenuOutcomeScopedToItsSession(t *testing.T) {
	m, _, _ := menuPane(t, colour())
	res, cmd := m.Update(heldDoneMsg{name: "elsewhere", session: "another-id", text: "Answer: Blue"})
	m = run(res.(model), cmd)
	if v := strip2(m.View()); strings.Contains(v, "Answer: Blue") {
		t.Fatalf("another session's outcome shows here:\n%s", v)
	}
	res, cmd = m.Update(heldDoneMsg{name: "Menu one", session: "live", text: "Answer: Red"})
	m = run(res.(model), cmd)
	if v := strip2(m.View()); !strings.Contains(v, "✓ Menu one: Answer: Red") {
		t.Fatalf("its own outcome shows while it's selected:\n%s", v)
	}
	m = ctrl(m, tea.KeyCtrlN)
	if v := strip2(m.View()); strings.Contains(v, "Answer: Red") || m.heldNote != "" {
		t.Errorf("moving the selection clears it:\n%s", v)
	}
}

// In a narrow pane the hint row drops ↑↓ first, keeping Esc's way out.
func TestMenuHintRowFits(t *testing.T) {
	row := strip2(paneHintsFit(60, "↑↓", "move", "space", "tick", keyEnter, "confirm", "n", "note", "←", "back", keyEsc, "list"))
	if strings.Contains(row, "↑↓") || !strings.Contains(row, "esc list") || ansi.StringWidth(row) > 60 {
		t.Errorf("fitted row: %q", row)
	}
}

// A menu taller than its share of the screen keeps its header, the
// highlighted row and its hint row on screen, saying where there's more.
func TestMenuTallScrollsToTheCursor(t *testing.T) {
	m, _, _ := menuPane(t, writeAccess())
	m.width, m.height = 80, 16
	m = redraw(m)
	m = keyType(m, tea.KeyDown)
	m = keyType(m, tea.KeyDown) // Other…
	joined := strings.Join(paneText(m), "\n")
	for _, want := range []string{"also in its tab", "❯ 3. Other…", "esc list", "↑ more"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lacks %q:\n%s", want, joined)
		}
	}
	if n := len(paneText(m)); n > m.askLimit() {
		t.Errorf("%d lines, more than the pane's %d", n, m.askLimit())
	}
}

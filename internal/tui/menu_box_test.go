package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/mcp"
)

func threeQuestions() mcp.HeldPrompt {
	return mcp.HeldPrompt{ID: "q3", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{
		{Question: "Which colour for the probe?", Header: "Colour", Options: []string{"Red", "Blue"}},
		{Question: "Ship it today?", Header: "Ship", Options: []string{"Yes, after the deploy window", "No"}},
		{Question: "Which checks?", Header: "Checks", Options: []string{"lint", "vet", "race"}, Multi: true},
	}}
}

// boxRows is the held prompt's box as drawn, colours stripped.
func boxRows(m model) []string { return strings.Split(strip2(strings.Join(m.chatAsk(), "\n")), "\n") }

// The held menu is drawn inside the message box's own frame, the full pane
// wide, every row the same width, and never taller than two thirds of the
// pane, so the transcript above keeps a third.
func TestMenuInTheMessageBox(t *testing.T) {
	for _, width := range []int{80, 120} {
		for _, h := range []mcp.HeldPrompt{writeAccess(), threeQuestions(), {ID: "p", Session: "live", Tool: "Bash", Summary: "make deploy"}} {
			m, _, _ := paneModel(t, h)
			m.width, m.height = width, 40
			m = redraw(m)
			rows := boxRows(m)
			pw := m.paneWidth()
			if !strings.HasPrefix(rows[0], "╭") || !strings.HasPrefix(rows[len(rows)-1], "╰") {
				t.Fatalf("%d cols, %s: not boxed:\n%s", width, h.ID, strings.Join(rows, "\n"))
			}
			for i, r := range rows {
				if w := ansi.StringWidth(r); w != pw {
					t.Errorf("%d cols, %s: row %d is %d wide, want %d: %q", width, h.ID, i, w, pw, r)
				}
				if i > 0 && i < len(rows)-1 && !strings.HasPrefix(r, "│") {
					t.Errorf("%d cols, %s: row %d outside the box: %q", width, h.ID, i, r)
				}
			}
			if !strings.Contains(rows[1], "also in its tab") {
				t.Errorf("%d cols, %s: the header isn't the box's first line: %q", width, h.ID, rows[1])
			}
			_, _, top := m.listLayout()
			if pane := m.height - top; m.chatRows() > pane*2/3 {
				t.Errorf("%d cols, %s: the bottom pane takes %d of %d rows", width, h.ID, m.chatRows(), pane)
			}
		}
	}
}

// A note longer than the box wraps inside it, at most four lines, the
// cursor always on screen, and the text itself gets no newline.
func TestMenuNoteWraps(t *testing.T) {
	for _, width := range []int{80, 120} {
		m, _, _ := menuPane(t, colour())
		m.width, m.height = width, 40
		m = redraw(m)
		m = press(m, "n")
		long := strings.Repeat("a careful note that goes on ", 20) + "NOTE END"
		m = typeIn(m, long)
		mn := m.menus["q1"]
		if strings.Contains(mn.box.Value(), "\n") {
			t.Fatal("the note got a newline")
		}
		rows := boxRows(m)
		for _, r := range rows {
			if ansi.StringWidth(r) != m.paneWidth() {
				t.Errorf("%d cols: a row overflows the box: %q", width, r)
			}
		}
		joined := strings.Join(rows, "\n")
		if !strings.Contains(joined, "NOTE END") {
			t.Errorf("%d cols: the end (where the cursor is) isn't shown:\n%s", width, joined)
		}
		cursor := strings.Count(strings.Join(m.chatAsk(), "\n"), "\033[7m")
		if cursor != 1 {
			t.Errorf("%d cols: %d cursors drawn, want 1", width, cursor)
		}
		drawn := menuBoxLines(mn, m.heldInner())
		if len(drawn) != menuBoxMaxLines {
			t.Errorf("%d cols: the box is %d lines, want %d", width, len(drawn), menuBoxMaxLines)
		}
		// Back to the start: the first line shows, the cursor with it.
		m = keyType(m, tea.KeyCtrlA)
		drawn = menuBoxLines(m.menus["q1"], m.heldInner())
		if first := strip2(drawn[0]); !strings.Contains(first, "note › a careful") || !strings.Contains(drawn[0], "\033[7ma\033[27m") {
			t.Errorf("%d cols: ^A shows the start with the cursor on it: %q", width, drawn[0])
		}
		m = keyType(m, tea.KeyEnter)
		if got := m.menus["q1"].notes[0]; got != strings.TrimSpace(long) {
			t.Errorf("%d cols: Enter saves the whole note: %q", width, got)
		}
	}
}

// The review reads like Claude Code's: "Review · 3 answers", each question
// numbered and dim, its answer bold after →, a note dim under it, your own
// text marked (Other), pick-any answers joined with ", ".
func TestMenuReviewLook(t *testing.T) {
	for _, width := range []int{80, 120} {
		m, _, tick := menuPane(t, threeQuestions())
		m.width, m.height = width, 40
		m = redraw(m)
		m = press(m, "n")
		m = typeIn(m, "warmer fits the brand")
		m = keyType(m, tea.KeyEnter)
		m = keyType(m, tea.KeyEnter) // Red
		tick(time.Second)
		m = keyType(m, tea.KeyUp) // Other…
		m = keyType(m, tea.KeyEnter)
		m = typeIn(m, "Please also bump the chart version")
		m = keyType(m, tea.KeyEnter)
		tick(time.Second)
		m = press(m, " ") // lint
		m = keyType(m, tea.KeyDown)
		m = press(m, " ") // vet
		m = keyType(m, tea.KeyEnter)
		tick(time.Second)
		m = redraw(m)
		lines := paneText(m)
		joined := strings.Join(lines, "\n")
		for _, want := range []string{
			"1  Which colour for the probe?", "   → Red", "     note: warmer fits the brand",
			"2  Ship it today?", "   → (Other) Please also bump the chart",
			"3  Which checks?", "   → lint, vet",
			"enter send · ← edit · ↑↓ scroll · esc list",
		} {
			if indexOf(lines, want) < 0 {
				t.Errorf("%d cols: the review lacks %q:\n%s", width, want, joined)
			}
		}
		if !strings.HasPrefix(lines[0], "Review · 3 answers") || !strings.HasSuffix(lines[0], "also in its tab") {
			t.Errorf("%d cols: the header: %q", width, lines[0])
		}
		raw := strings.Join(m.chatAsk(), "\n")
		if !strings.Contains(raw, "\033[1mRed") || !strings.Contains(raw, "\033[90mWhich colour for the probe?") {
			t.Errorf("%d cols: answers bold, questions dim:\n%q", width, raw)
		}
		if os.Getenv("CCS_RENDER") != "" && width == 80 {
			m.width = 100
			m = redraw(m)
			t.Logf("review at 100 columns:\n%s", strings.Join(boxRows(m), "\n"))
		}
	}
}

// For the report: a note being typed, at 100 columns.
func TestMenuNoteRender(t *testing.T) {
	if os.Getenv("CCS_RENDER") == "" {
		t.Skip("set CCS_RENDER to print")
	}
	m, _, _ := menuPane(t, colour())
	m.width, m.height = 100, 40
	m = redraw(m)
	m = press(m, "n")
	m = typeIn(m, "warmer fits the brand, and it matches the probe's old colour on the status page so nobody has to relearn it")
	t.Logf("a note being typed at 100 columns:\n%s", strings.Join(boxRows(m), "\n"))
}

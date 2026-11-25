package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// drafts is a question whose options carry previews (made-up text), the
// second option's holding a ❯ of its own, the third none.
func drafts(first string) mcp.HeldPrompt {
	return mcp.HeldPrompt{ID: "pv", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{{
		Question:     "Which reply should I send?",
		Header:       "Reply",
		Options:      []string{"Short", "Mockup", "Skip"},
		Descriptions: []string{"two lines", "", "send nothing"},
		Previews:     []string{first, "❯ 1. Yes\n  2. No", ""},
	}}}
}

const shortDraft = "Hello team,\n\nThe nightly build is green again.\n\nThanks"

// previewPane is the pane at a terminal size, with the menu's keys.
func previewPane(t *testing.T, h mcp.HeldPrompt, width, height int) model {
	t.Helper()
	m, _, _ := menuPane(t, h)
	m.width, m.height = width, height
	return redraw(m)
}

// boxRowsOf is the preview box's content rows as drawn, from paneText.
func boxRowsOf(lines []string) []string {
	var out []string
	for _, l := range lines {
		i := strings.Index(l, "│ ")
		if i < 0 {
			continue
		}
		out = append(out, strings.TrimRight(strings.TrimSuffix(strings.TrimRight(l[i+len("│ "):], " "), "│"), " "))
	}
	return out
}

// On a wide pane the highlighted option's preview is a box beside the
// options, its text verbatim with its blank lines; moving the cursor shows
// the next option's, an option without one says so, and every line fits.
func TestMenuPreviewBesideOptions(t *testing.T) {
	m := previewPane(t, drafts(shortDraft), 200, 40)
	if _, side := previewColumns(m.heldInner()); !side {
		t.Fatalf("a %d-column pane puts previews beside the options", m.heldInner())
	}
	lines := paneText(m)
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > m.paneWidth() {
			t.Errorf("line %d wider than the pane (%d): %q", i, w, l)
		}
	}
	joined := strings.Join(lines, "\n")
	if got := strings.Join(boxRowsOf(lines), "\n"); got != shortDraft {
		t.Errorf("the box holds %q, want the preview verbatim %q:\n%s", got, shortDraft, joined)
	}
	row := ""
	for _, l := range lines {
		if strings.Contains(l, "❯ 1. Short") {
			row = l
		}
	}
	if !strings.Contains(row, "┌") && !strings.Contains(row, "│ ") {
		t.Errorf("the box sits beside the options, not under them:\n%s", joined)
	}
	if !strings.Contains(joined, "enter pick · n note · esc list") {
		t.Errorf("the hint row stays:\n%s", joined)
	}

	m = keyType(m, tea.KeyDown)
	lines = paneText(m)
	if got := strings.Join(boxRowsOf(lines), "\n"); got != "❯ 1. Yes\n  2. No" {
		t.Errorf("moving down shows the next option's preview, got %q:\n%s", got, strings.Join(lines, "\n"))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "❯ 2. Mockup") {
		t.Errorf("a ❯ in the preview never moves the menu's own:\n%s", strings.Join(lines, "\n"))
	}
	m = keyType(m, tea.KeyDown)
	if got := strings.Join(boxRowsOf(paneText(m)), "\n"); got != "no preview" {
		t.Errorf("an option without a preview: %q", got)
	}
}

// On a narrow pane the preview goes under the options, the full width.
func TestMenuPreviewUnderOptionsWhenNarrow(t *testing.T) {
	m := previewPane(t, drafts(shortDraft), 90, 50)
	if _, side := previewColumns(m.heldInner()); side {
		t.Fatalf("a %d-column pane is too narrow for a box beside the options", m.heldInner())
	}
	lines := paneText(m)
	other, top := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "4. Other…") {
			other = i
		}
		if strings.HasPrefix(l, "┌") && top < 0 {
			top = i
		}
	}
	if other < 0 || top <= other {
		t.Fatalf("the box (row %d) comes after the options (Other… row %d):\n%s", top, other, strings.Join(lines, "\n"))
	}
	if got := strings.Join(boxRowsOf(lines), "\n"); got != shortDraft {
		t.Errorf("the box holds %q:\n%s", got, strings.Join(lines, "\n"))
	}
}

// longDraft is a made-up preview of n numbered lines, far taller than any pane.
func longDraft(n int) string {
	var long []string
	for i := range n {
		long = append(long, fmt.Sprintf("draft line %02d", i+1))
	}
	return strings.Join(long, "\n")
}

// boxFirst is the first line of the preview box on screen.
func boxFirst(t *testing.T, m model) string {
	t.Helper()
	rows := boxRowsOf(paneText(m))
	if len(rows) == 0 {
		t.Fatalf("no preview box:\n%s", strings.Join(paneText(m), "\n"))
	}
	return strings.TrimSpace(strings.TrimSuffix(rows[0], "┃"))
}

// A preview taller than its box scrolls inside it, beside the options or
// under them: the box's right edge is a scrollbar, PgDn and PgUp page it
// (keeping a line of the last view), it stops at either end, nothing says
// "in its tab", and the highlighted option and the hint row stay on screen.
func TestMenuPreviewScrolls(t *testing.T) {
	for _, size := range [][2]int{{200, 30}, {90, 60}} {
		m := previewPane(t, drafts(longDraft(80)), size[0], size[1])
		joined := strings.Join(paneText(m), "\n")
		if len(m.chatAsk()) > m.heldLimit() {
			t.Errorf("%v: the pane is %d rows, over its limit %d", size, len(m.chatAsk()), m.heldLimit())
		}
		for _, want := range []string{"❯ 1. Short", "┃", "pgup/dn preview", "esc list"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%v: %q isn't on screen:\n%s", size, want, joined)
			}
		}
		if strings.Contains(joined, "lines in its tab") {
			t.Errorf("%v: a preview that scrolls isn't cut:\n%s", size, joined)
		}
		if got := boxFirst(t, m); got != "draft line 01" {
			t.Fatalf("%v: the box starts at the top, got %q", size, got)
		}
		view := len(boxRowsOf(paneText(m)))
		m = keyType(m, tea.KeyPgDown)
		if got, want := boxFirst(t, m), fmt.Sprintf("draft line %02d", view); got != want {
			t.Errorf("%v: PgDn pages the box less a line: first %q, want %q", size, got, want)
		}
		if strings.Contains(strings.Join(paneText(m), "\n"), "❯ 1. Short") == false {
			t.Errorf("%v: PgDn moved the menu, not the preview", size)
		}
		for range 40 {
			m = keyType(m, tea.KeyPgDown)
		}
		rows := boxRowsOf(paneText(m))
		if last := strings.TrimSpace(strings.TrimSuffix(rows[len(rows)-1], "┃")); last != "draft line 80" {
			t.Errorf("%v: PgDn stops at the end, last line %q", size, last)
		}
		m = keyType(m, tea.KeyPgUp)
		if got := boxFirst(t, m); got == fmt.Sprintf("draft line %02d", 80-view+1) {
			t.Errorf("%v: one PgUp from the end moves straight back (never past the end)", size)
		}
		for range 40 {
			m = keyType(m, tea.KeyPgUp)
		}
		if got := boxFirst(t, m); got != "draft line 01" {
			t.Errorf("%v: PgUp stops at the top, got %q", size, got)
		}
	}
}

// The offset belongs to the highlighted option: moving to another starts
// its preview at the top, and coming back starts the first's at the top too.
func TestMenuPreviewScrollResetsOnMove(t *testing.T) {
	h := drafts(longDraft(80))
	h.Questions[0].Previews[1] = longDraft(60)
	m := previewPane(t, h, 200, 30)
	m = keyType(m, tea.KeyPgDown)
	if boxFirst(t, m) == "draft line 01" {
		t.Fatal("PgDn scrolled nothing")
	}
	m = keyType(m, tea.KeyDown)
	if got := boxFirst(t, m); got != "draft line 01" {
		t.Errorf("the next option's preview starts at its top, got %q", got)
	}
	m = keyType(m, tea.KeyUp)
	if got := boxFirst(t, m); got != "draft line 01" {
		t.Errorf("back on the first option its preview starts at the top again, got %q", got)
	}
}

// The wheel over the box scrolls it three lines a notch; the wheel over the
// conversation above leaves the box alone.
func TestMenuPreviewWheel(t *testing.T) {
	for _, size := range [][2]int{{200, 30}, {90, 60}} {
		m := previewPane(t, drafts(longDraft(80)), size[0], size[1])
		x, y := -1, -1
		lines := m.chatAsk()
		top := m.height - m.chatRows() + 1
		paneX := 0
		if sw := m.sidebarWidth(); sw > 0 {
			paneX = sw + 1
		}
		for i, l := range lines {
			s := ansi.Strip(l)
			if j := strings.Index(s, "draft line 02"); j >= 0 {
				x, y = paneX+ansi.StringWidth(s[:j]), top+i
			}
		}
		if y < 0 {
			t.Fatalf("%v: no box drawn", size)
		}
		if !m.overPreviewBox(x, y) || m.overPreviewBox(x, 3) {
			t.Fatalf("%v: overPreviewBox is wrong at (%d,%d) or the conversation", size, x, y)
		}
		wheel := func(m model, x, y int, b tea.MouseButton) model {
			res, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Button: b, Action: tea.MouseActionPress})
			return run(res.(model), cmd)
		}
		m = wheel(m, x, y, tea.MouseButtonWheelDown)
		if got := boxFirst(t, m); got != "draft line 04" {
			t.Errorf("%v: a notch down over the box shows from line 4, got %q", size, got)
		}
		m = wheel(m, x, 3, tea.MouseButtonWheelDown)
		if got := boxFirst(t, m); got != "draft line 04" {
			t.Errorf("%v: the wheel over the conversation leaves the box, got %q", size, got)
		}
		m = wheel(m, x, y, tea.MouseButtonWheelUp)
		m = wheel(m, x, y, tea.MouseButtonWheelUp)
		if got := boxFirst(t, m); got != "draft line 01" {
			t.Errorf("%v: the wheel up stops at the top, got %q", size, got)
		}
	}
}

// A preview that fits its box doesn't scroll: no scrollbar, no hint, and
// PgUp/PgDn aren't the menu's (they move the list, as without a preview).
func TestMenuPreviewFitsNoScroll(t *testing.T) {
	m := previewPane(t, drafts(shortDraft), 200, 40)
	joined := strings.Join(paneText(m), "\n")
	if strings.Contains(joined, "┃") || strings.Contains(joined, "pgup/dn") {
		t.Errorf("a preview that fits has no scrollbar or hint:\n%s", joined)
	}
	if _, done := m.heldPaneKey(tea.KeyMsg{Type: tea.KeyPgDown}, time.Time{}); done {
		t.Error("PgDn with nothing to scroll is the list's")
	}
}

// previewBox keeps tabs as spaces and wraps a line too long for the box.
func TestPreviewBoxWrapsAndExpandsTabs(t *testing.T) {
	box := previewBox("\tindented\n"+strings.Repeat("word ", 12), 24, 20, 0)
	for _, l := range box {
		if w := ansi.StringWidth(l); w != 24 {
			t.Errorf("row %q is %d wide, want 24", strip2(l), w)
		}
		if strings.Contains(l, "\t") {
			t.Errorf("a tab reached the screen: %q", l)
		}
	}
	if got := strip2(box[1]); !strings.Contains(got, "│     indented") {
		t.Errorf("the tab is four spaces: %q", got)
	}
	if len(box) < 6 {
		t.Errorf("the long line wraps: %q", box)
	}
}

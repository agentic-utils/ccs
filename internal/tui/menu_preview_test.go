package tui

import (
	"strings"
	"testing"

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

// A preview taller than the pane is cut inside its box, saying how much (on
// a short narrow pane with no room for a box, one line says where it is),
// and the highlighted option and the hint row stay on screen.
func TestMenuPreviewCutToFit(t *testing.T) {
	var long []string
	for i := range 80 {
		long = append(long, "line "+string(rune('a'+i%26)))
	}
	for _, width := range []int{200, 90} {
		m := previewPane(t, drafts(strings.Join(long, "\n")), width, 30)
		lines := paneText(m)
		joined := strings.Join(lines, "\n")
		if len(m.chatAsk()) > m.heldLimit() {
			t.Errorf("%d cols: the pane is %d rows, over its limit %d", width, len(m.chatAsk()), m.heldLimit())
		}
		for _, want := range []string{"❯ 1. Short", "lines in its tab", "esc list"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%d cols: %q isn't on screen:\n%s", width, want, joined)
			}
		}
	}
}

// previewBox keeps tabs as spaces and wraps a line too long for the box.
func TestPreviewBoxWrapsAndExpandsTabs(t *testing.T) {
	box := previewBox("\tindented\n"+strings.Repeat("word ", 12), 24, 20)
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

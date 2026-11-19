package tui

// The menu a held AskUserQuestion is answered with, in the main view's
// bottom pane, as Claude Code's own menu works: one question at a time, ↑/↓
// (j/k) over its options and a last "Other…" row, Enter picks (a digit
// jumps to an option and picks it), Space ticks a pick-any question's
// options and Enter confirms them, "Other…" opens a one-line box for your
// own answer, ← goes back a question keeping the answers. One question is
// sent as soon as it's answered; several end on a review (Enter submits,
// Esc goes back). A stray key never answers an unread question: Enter, a
// digit and Space do nothing for menuFresh after a question first shows.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// menuFresh is how long Enter, digits and Space do nothing after a question
// first shows (arrow keys work at once), so a key meant for something else
// can't answer it unread.
const menuFresh = 500 * time.Millisecond

// heldMenu is the progress through one held prompt's questions; kept per
// prompt, so leaving the pane and coming back resumes it.
type heldMenu struct {
	id      string
	step    int             // the question shown; len(questions) is the review
	cursor  int             // over the options and the "Other…" row
	picks   map[int][]int   // a question's chosen options, by index
	other   map[int]string  // a question answered with your own text
	drafts  map[int]string  // text typed into a question's Other box and closed with Esc
	editing bool            // the "Other…" box is open
	box     textinput.Model // the "Other…" box
	shownAt time.Time       // when this step first showed: menuFresh from it
}

func newHeldMenu(id string) *heldMenu {
	b := textinput.New()
	b.Prompt = "› "
	b.Placeholder = "your answer"
	b.CharLimit = 2000
	return &heldMenu{id: id, picks: map[int][]int{}, other: map[int]string{}, drafts: map[int]string{}, box: b}
}

// menuFor is the menu for a held prompt, made the first time it shows.
func (m *model) menuFor(h mcp.HeldPrompt) *heldMenu {
	if m.menus == nil {
		m.menus = map[string]*heldMenu{}
	}
	mn := m.menus[h.ID]
	if mn == nil {
		mn = newHeldMenu(h.ID)
		m.menus[h.ID] = mn
	}
	return mn
}

// showMenu notes that a held prompt's menu is on screen now: the first time
// it (or a later question) shows, menuFresh starts. Called after every
// update with the prompt shown, if any.
func (m *model) showMenu(h mcp.HeldPrompt, ok bool) {
	if !ok || len(h.Questions) == 0 {
		m.menuShown = ""
		return
	}
	mn := m.menuFor(h)
	if m.menuShown != h.ID {
		m.menuShown, mn.shownAt = h.ID, heldNow()
	}
	for id := range m.menus { // forget prompts no longer held
		if _, held := m.heldByID(id); !held {
			delete(m.menus, id)
		}
	}
}

// heldShown is the held prompt on screen: the bottom pane's.
func (m model) heldShown() (mcp.HeldPrompt, bool) {
	h, _, ok := m.selectedHeld()
	return h, ok
}

// menuKey handles a key on a held question menu. done is false for keys
// that aren't the menu's (Esc at the first question with no box open, so
// the caller leaves the pane).
func (m *model) menuKey(h mcp.HeldPrompt, name, k string, msg tea.KeyMsg) (tea.Cmd, bool) {
	mn := m.menuFor(h)
	qs := h.Questions
	if mn.step > len(qs) {
		mn.step = len(qs)
	}
	fresh := heldNow().Sub(mn.shownAt) < menuFresh
	pickKey := k == "enter" || k == " " || k == "space" || (len(k) == 1 && k >= "1" && k <= "9")
	if mn.editing {
		switch k {
		case "esc":
			mn.drafts[mn.step] = mn.box.Value() // kept for when it's opened again
			mn.editing = false
			mn.box.Blur()
		case "enter":
			text := strings.TrimSpace(mn.box.Value())
			if text == "" {
				m.setHeldNote("type your answer, or esc")
				return nil, true
			}
			mn.other[mn.step], mn.picks[mn.step] = text, nil
			mn.editing = false
			mn.box.Blur()
			return m.menuNext(h, name, mn), true
		default:
			mn.box, _ = editKey(mn.box, msg)
		}
		return nil, true
	}
	if mn.step == len(qs) { // the review
		switch k {
		case "enter":
			if fresh {
				return nil, true
			}
			return m.menuSubmit(h, name, mn), true
		case "esc", "left", "h":
			mn.step, mn.shownAt = len(qs)-1, heldNow()
		case "T", "t":
			return m.answerHeldCmd(h, name, "mac", nil, nil), true
		}
		return nil, true
	}
	q := qs[mn.step]
	rows := len(q.Options) + 1 // and "Other…"
	if fresh && pickKey {
		return nil, true // unread: a stray key never answers it
	}
	switch {
	case k == "up" || k == "k":
		mn.cursor = (mn.cursor - 1 + rows) % rows
	case k == "down" || k == "j":
		mn.cursor = (mn.cursor + 1) % rows
	case k == "left" || k == "h":
		if mn.step > 0 {
			mn.step, mn.cursor, mn.shownAt = mn.step-1, 0, heldNow()
		}
	case k == "esc":
		if mn.step == 0 {
			return nil, false // leave the pane; progress is kept
		}
		mn.step, mn.cursor, mn.shownAt = mn.step-1, 0, heldNow()
	case k == "T" || k == "t":
		return m.answerHeldCmd(h, name, "mac", nil, nil), true
	case len(k) == 1 && k >= "1" && k <= "9":
		n := int(k[0] - '1')
		if n >= len(q.Options) {
			m.setHeldNote(fmt.Sprintf("options here are 1-%d", min(len(q.Options), 9)))
			return nil, true
		}
		mn.cursor = n
		if q.Multi {
			mn.toggle(n)
			return nil, true
		}
		mn.picks[mn.step], mn.other[mn.step] = []int{n}, ""
		return m.menuNext(h, name, mn), true
	case k == " " || k == "space":
		if q.Multi && mn.cursor < len(q.Options) {
			mn.toggle(mn.cursor)
		} else if mn.cursor == len(q.Options) {
			m.menuOther(mn)
		}
	case k == "enter":
		if mn.cursor == len(q.Options) {
			m.menuOther(mn)
			return nil, true
		}
		if q.Multi {
			if len(mn.picks[mn.step]) == 0 && mn.other[mn.step] == "" {
				m.setHeldNote("tick at least one (space), or choose Other…")
				return nil, true
			}
			return m.menuNext(h, name, mn), true
		}
		mn.picks[mn.step], mn.other[mn.step] = []int{mn.cursor}, ""
		return m.menuNext(h, name, mn), true
	}
	return nil, true
}

func (mn *heldMenu) toggle(n int) {
	delete(mn.other, mn.step)
	if i := slices.Index(mn.picks[mn.step], n); i >= 0 {
		mn.picks[mn.step] = slices.Delete(mn.picks[mn.step], i, i+1)
		return
	}
	mn.picks[mn.step] = append(mn.picks[mn.step], n)
	slices.Sort(mn.picks[mn.step])
}

func (m *model) menuOther(mn *heldMenu) {
	mn.editing = true
	text := mn.other[mn.step]
	if text == "" {
		text = mn.drafts[mn.step]
	}
	mn.box.SetValue(text)
	mn.box.CursorEnd()
	mn.box.Focus()
}

// menuNext moves past an answered question: one question is sent at once,
// several go on to the next, then the review.
func (m *model) menuNext(h mcp.HeldPrompt, name string, mn *heldMenu) tea.Cmd {
	if len(h.Questions) == 1 {
		return m.menuSubmit(h, name, mn)
	}
	mn.step, mn.shownAt = mn.step+1, heldNow()
	mn.cursor = 0
	if mn.step < len(h.Questions) {
		if p := mn.picks[mn.step]; len(p) > 0 { // coming back: start on its answer
			mn.cursor = p[0]
		} else if mn.other[mn.step] != "" {
			mn.cursor = len(h.Questions[mn.step].Options)
		}
	}
	return nil
}

// menuSubmit sends every question's answer by the prompt's id.
func (m *model) menuSubmit(h mcp.HeldPrompt, name string, mn *heldMenu) tea.Cmd {
	labels, other := map[int][]string{}, map[int]bool{}
	for i, q := range h.Questions {
		if t := mn.other[i]; t != "" {
			labels[i], other[i] = []string{t}, true
			continue
		}
		if len(mn.picks[i]) == 0 {
			mn.step, mn.shownAt = i, heldNow()
			m.setHeldNote("question " + fmt.Sprint(i+1) + " has no answer yet")
			return nil
		}
		for _, n := range mn.picks[i] {
			labels[i] = append(labels[i], q.Options[n])
		}
	}
	return m.answerHeldCmd(h, name, "answer", labels, other)
}

// menuAnswer is a question's answer as the review shows it.
func (mn *heldMenu) menuAnswer(i int, q mcp.HeldQuestion) string {
	if t := mn.other[i]; t != "" {
		return "“" + t + "”"
	}
	var out []string
	for _, n := range mn.picks[i] {
		out = append(out, q.Options[n])
	}
	return strings.Join(out, ", ")
}

// menuLines draws the menu: the question with its header and options (the
// cursor, ticks for pick-any, each description dim below), "Other…", the
// box when open; or the review.
func (m model) menuLines(h mcp.HeldPrompt, width int) []string {
	mn := m.menus[h.ID]
	if mn == nil {
		mn = newHeldMenu(h.ID)
	}
	cut := func(s string) string { return transcript.Truncate(s, max(width, 10)) }
	qs := h.Questions
	var lines []string
	if mn.step >= len(qs) {
		lines = append(lines, "\033[1mReview your answers\033[0m")
		for i, q := range qs {
			lines = append(lines, cut("  "+q.Question+" → \033[1m"+mn.menuAnswer(i, q)+"\033[0m"))
		}
		return append(lines, "\033[90m"+hints(keyEnter, "submit", keyEsc, "back", "T", "back to its tab")+"\033[0m")
	}
	q := qs[mn.step]
	head := fmt.Sprintf("Question %d of %d", mn.step+1, len(qs))
	if q.Header != "" {
		head += " · " + q.Header
	}
	lines = append(lines, "\033[90m"+head+"\033[0m", cut("\033[1m"+q.Question+"\033[0m"))
	for i, o := range q.Options {
		mark := "  "
		if i == mn.cursor && !mn.editing {
			mark = "\033[1;" + amber + "m❯\033[0m "
		}
		box := ""
		if q.Multi {
			box = "[ ] "
			if slices.Contains(mn.picks[mn.step], i) {
				box = "[x] "
			}
		} else if slices.Contains(mn.picks[mn.step], i) {
			box = "✓ "
		}
		lines = append(lines, cut(fmt.Sprintf("%s%s%d. %s", mark, box, i+1, o)))
		if i < len(q.Descriptions) && q.Descriptions[i] != "" {
			lines = append(lines, cut("      \033[90m"+q.Descriptions[i]+"\033[0m"))
		}
	}
	mark := "  "
	if mn.cursor == len(q.Options) && !mn.editing {
		mark = "\033[1;" + amber + "m❯\033[0m "
	}
	other := "Other…"
	if t := mn.other[mn.step]; t != "" {
		other += " “" + t + "”"
	}
	lines = append(lines, cut(mark+other))
	if mn.editing {
		lines = append(lines, "  "+mn.box.View(), "\033[90m"+hints(keyEnter, "use it", keyEsc, "close")+"\033[0m")
		return lines
	}
	keys := []string{"↑↓", "move", keyEnter, "pick"}
	if q.Multi {
		keys = []string{"↑↓", "move", "space", "tick", keyEnter, "confirm"}
	}
	if mn.step > 0 {
		keys = append(keys, "←", "back")
	}
	keys = append(keys, "T", "back to its tab")
	return append(lines, "\033[90m"+hints(keys...)+"\033[0m")
}

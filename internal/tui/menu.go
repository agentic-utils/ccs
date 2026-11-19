package tui

// The menu a held AskUserQuestion is answered with, in the main view's
// bottom pane, as Claude Code's own menu works: one question at a time,
// ↑/↓ (j/k) over its options and a last "Other…" row, Enter picks (a digit
// jumps to an option and picks it), Space ticks a pick-any question's
// options and Enter confirms them, "Other…" opens a one-line box for your
// own answer, n adds a note to the question's answer (as Claude Code's own
// menu does), ← goes back a question keeping the answers. One question is
// sent as soon as it's answered; several end on a review (Enter submits,
// Esc goes back). The menu takes these keys as soon as its session is
// selected with the search empty (held.go, heldKeysActive); any other key
// goes to the search. A stray key never answers an unread question: Enter,
// a digit and Space do nothing for menuFresh after a question first shows.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// menuFresh is how long Enter, digits and Space do nothing after a question
// first shows (arrow keys work at once), so a key meant for something else
// can't answer it unread.
const menuFresh = 500 * time.Millisecond

// The menu's one-line boxes.
const (
	boxNone  = iota
	boxOther // "Other…": your own answer
	boxNote  // n: a note on the question's answer
)

// heldMenu is the progress through one held prompt's questions; kept per
// prompt, so leaving the menu and coming back resumes it.
type heldMenu struct {
	id      string
	step    int             // the question shown; len(questions) is the review
	cursor  int             // over the options and the "Other…" row
	picks   map[int][]int   // a question's chosen options, by index
	other   map[int]string  // a question answered with your own text
	drafts  map[int]string  // text typed into a question's Other box and closed with Esc
	notes   map[int]string  // a note on a question's answer (Claude Code's annotations)
	editing int             // which box is open: boxNone, boxOther or boxNote
	box     textinput.Model // the open box
	shownAt time.Time       // when this step first showed: menuFresh from it
	revTop  int             // the review's first line on screen, when it's taller than the pane
	revSeen bool            // the review's last line has been on screen: only then does Enter send it
}

func newHeldMenu(id string) *heldMenu {
	b := textinput.New()
	b.Prompt = "› "
	b.CharLimit = 2000
	return &heldMenu{id: id, picks: map[int][]int{}, other: map[int]string{}, drafts: map[int]string{}, notes: map[int]string{}, box: b}
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

// heldShown is the held prompt on screen: the bottom pane's, while nothing
// covers it (the usage screen, help, a popup), so a menu hidden and shown
// again re-arms menuFresh.
func (m model) heldShown() (mcp.HeldPrompt, bool) {
	h, _, ok := m.heldDrawn()
	return h, ok
}

// menuKey handles a key on a held question menu. done is false for a key
// the menu doesn't use, which goes to the search instead, and for Esc at
// the first question with no box open, which leaves the menu.
func (m *model) menuKey(h mcp.HeldPrompt, name, k string, msg tea.KeyMsg) (tea.Cmd, bool) {
	mn := m.menuFor(h)
	qs := h.Questions
	if mn.step > len(qs) {
		mn.step = len(qs)
	}
	// The appearance guard comes before anything else, an open box included:
	// Enter there would send a filled-in Other… at once.
	fresh := heldNow().Sub(mn.shownAt) < menuFresh
	pickKey := k == "enter" || k == " " || k == "space" || (len(k) == 1 && k >= "1" && k <= "9")
	if fresh && (k == "enter" || (mn.editing == boxNone && pickKey)) {
		return nil, true // unread: a stray key never answers it
	}
	if mn.editing != boxNone {
		return m.menuBoxKey(h, name, mn, k, msg), true
	}
	if mn.step == len(qs) { // the review
		switch k {
		case "enter":
			if !mn.revSeen { // never send answers and notes not yet seen
				m.setHeldNote("scroll to the end first: ↓ shows the rest of your answers")
				return nil, true
			}
			return m.menuSubmit(h, name, mn), true
		case "up", "k":
			mn.revTop = max(0, mn.revTop-1)
			return nil, true
		case "down", "j":
			mn.revTop++ // clamped by noteReview
			return nil, true
		case "pgup":
			mn.revTop = max(0, mn.revTop-max(m.askLimit()-2, 1))
			return nil, true
		case "pgdown":
			mn.revTop += max(m.askLimit()-2, 1)
			return nil, true
		case "esc", "left":
			mn.step, mn.shownAt = len(qs)-1, heldNow()
			return nil, true
		case "T", "t":
			return m.answerHeldCmd(h, name, "mac", nil, nil, nil), true
		}
		return nil, false
	}
	q := qs[mn.step]
	rows := len(q.Options) + 1 // and "Other…"
	switch {
	case k == "up" || k == "k":
		mn.cursor = (mn.cursor - 1 + rows) % rows
	case k == "down" || k == "j":
		mn.cursor = (mn.cursor + 1) % rows
	case k == "left":
		if mn.step > 0 {
			mn.step, mn.cursor, mn.shownAt = mn.step-1, 0, heldNow()
		}
	case k == "esc":
		if mn.step == 0 {
			return nil, false // leave the menu; progress is kept
		}
		mn.step, mn.cursor, mn.shownAt = mn.step-1, 0, heldNow()
	case k == "T" || k == "t":
		return m.answerHeldCmd(h, name, "mac", nil, nil, nil), true
	case k == "n" || k == "N":
		m.menuOpen(mn, boxNote, mn.notes[mn.step], "a note on your answer")
	case len(k) == 1 && k >= "1" && k <= "9":
		n := int(k[0] - '1')
		if n >= len(q.Options) {
			m.setHeldNote(fmt.Sprintf("options here are 1-%d", min(len(q.Options), 9)))
			return nil, true
		}
		mn.cursor = n // highlights it; Enter picks
		if q.Multi {
			mn.toggle(n)
		}
	case k == " " || k == "space":
		if q.Multi && mn.cursor < len(q.Options) {
			mn.toggle(mn.cursor)
		} else if mn.cursor == len(q.Options) {
			m.menuOtherBox(mn)
		}
	case k == "enter":
		if mn.cursor == len(q.Options) {
			m.menuOtherBox(mn)
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
	default:
		return nil, false // not the menu's: the search takes it
	}
	return nil, true
}

// menuBoxKey is a key in an open box: Enter uses it (Other…: your answer,
// then on to the next; a note: saved, or removed when empty), Esc closes it
// (Other…'s text kept as a draft), anything else edits it.
func (m *model) menuBoxKey(h mcp.HeldPrompt, name string, mn *heldMenu, k string, msg tea.KeyMsg) tea.Cmd {
	kind := mn.editing
	switch k {
	case "esc":
		if kind == boxOther {
			mn.drafts[mn.step] = mn.box.Value() // kept for when it's opened again
		}
		mn.editing = boxNone
		mn.box.Blur()
	case "enter":
		text := strings.TrimSpace(mn.box.Value())
		mn.editing = boxNone
		mn.box.Blur()
		if kind == boxNote {
			if text == "" {
				delete(mn.notes, mn.step)
			} else {
				mn.notes[mn.step] = text
			}
			return nil
		}
		if text == "" {
			mn.editing = boxOther
			mn.box.Focus()
			m.setHeldNote("type your answer, or esc")
			return nil
		}
		mn.other[mn.step], mn.picks[mn.step] = text, nil
		return m.menuNext(h, name, mn)
	default:
		mn.box, _ = editKey(mn.box, msg)
	}
	return nil
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

func (m *model) menuOtherBox(mn *heldMenu) {
	text := mn.other[mn.step]
	if text == "" {
		text = mn.drafts[mn.step]
	}
	m.menuOpen(mn, boxOther, text, "your answer")
}

func (m *model) menuOpen(mn *heldMenu, kind int, text, placeholder string) {
	mn.editing = kind
	mn.box.Placeholder = placeholder
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
	mn.cursor, mn.revTop, mn.revSeen = 0, 0, false
	if mn.step < len(h.Questions) {
		if p := mn.picks[mn.step]; len(p) > 0 { // coming back: start on its answer
			mn.cursor = p[0]
		} else if mn.other[mn.step] != "" {
			mn.cursor = len(h.Questions[mn.step].Options)
		}
	}
	return nil
}

// menuSubmit sends every question's answer, and its note, by the prompt's id.
func (m *model) menuSubmit(h mcp.HeldPrompt, name string, mn *heldMenu) tea.Cmd {
	labels, other, notes := map[int][]string{}, map[int]bool{}, map[int]string{}
	for i, q := range h.Questions {
		if n := mn.notes[i]; n != "" {
			notes[i] = n
		}
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
	return m.answerHeldCmd(h, name, "answer", labels, other, notes)
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
// cursor, ticks for pick-any, each description dim below), "Other…", its
// note, the box when open; or the review. active says the menu has the
// keys, so its hints show.
func (m model) menuLines(h mcp.HeldPrompt, width int, active bool) []string {
	mn := m.menus[h.ID]
	if mn == nil {
		mn = newHeldMenu(h.ID)
	}
	cut := func(s string) string { return transcript.Truncate(s, max(width, 10)) }
	dim := func(s string) string { return "\033[90m" + s + "\033[0m" }
	qs := h.Questions
	var lines []string
	if mn.step >= len(qs) {
		lines = append(lines, "\033[1mReview your answers\033[0m")
		for i, q := range qs {
			lines = append(lines, cut("  "+q.Question)) // the question may be cut: what's sent is below, whole
			lines = append(lines, wrapIndent("    → ", "\033[1m", mn.menuAnswer(i, q), width)...)
			if n := mn.notes[i]; n != "" {
				lines = append(lines, wrapIndent("    note: ", "\033[90m", n, width)...)
			}
		}
		if active {
			lines = append(lines, dim(hints(keyEnter, "submit", keyEsc, "back", "T", "back to its tab")))
		}
		return lines
	}
	q := qs[mn.step]
	head := fmt.Sprintf("Question %d of %d", mn.step+1, len(qs))
	if q.Header != "" {
		head += " · " + q.Header
	}
	lines = append(lines, dim(head), cut("\033[1m"+q.Question+"\033[0m"))
	for i, o := range q.Options {
		mark := "  "
		if i == mn.cursor && mn.editing == boxNone {
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
			lines = append(lines, cut("      "+dim(q.Descriptions[i])))
		}
	}
	mark := "  "
	if mn.cursor == len(q.Options) && mn.editing == boxNone {
		mark = "\033[1;" + amber + "m❯\033[0m "
	}
	other := "Other…"
	if t := mn.other[mn.step]; t != "" {
		other += " “" + t + "”"
	}
	lines = append(lines, cut(mark+other))
	if n := mn.notes[mn.step]; n != "" && mn.editing != boxNote {
		lines = append(lines, cut("  "+dim("note: "+n)))
	}
	switch mn.editing {
	case boxOther:
		return append(lines, "  "+mn.box.View(), dim(hints(keyEnter, "use it", keyEsc, "close")))
	case boxNote:
		return append(lines, "  note "+mn.box.View(), dim(hints(keyEnter, "save (empty removes it)", keyEsc, "cancel")))
	}
	if !active {
		return lines
	}
	if len(qs) == 1 { // no review: say what Enter sends, with its note, before it's sent
		sends := mn.pending(q)
		if n := mn.notes[mn.step]; n != "" {
			sends += " · note: " + n
		}
		lines = append(lines, wrapIndent("  enter sends: ", "\033[90m", sends, width)...)
	}
	keys := []string{"↑↓ 1-9", "move", keyEnter, "pick"}
	if q.Multi {
		keys = []string{"↑↓", "move", "space 1-9", "tick", keyEnter, "confirm"}
	}
	if mn.step > 0 {
		keys = append(keys, "←", "back")
	}
	keys = append(keys, "n", "note", "T", "back to its tab")
	return append(lines, dim(hints(keys...)))
}

// pending is what Enter would send for this question now: the highlighted
// option, the ticked ones of a pick-any question, or Other…'s text.
func (mn *heldMenu) pending(q mcp.HeldQuestion) string {
	if q.Multi {
		if t := mn.other[mn.step]; t != "" {
			return "“" + t + "”"
		}
		var out []string
		for _, n := range mn.picks[mn.step] {
			out = append(out, q.Options[n])
		}
		if len(out) == 0 {
			return "nothing ticked yet"
		}
		return strings.Join(out, ", ")
	}
	if mn.cursor < len(q.Options) {
		return q.Options[mn.cursor]
	}
	if t := mn.other[mn.step]; t != "" {
		return "“" + t + "”"
	}
	return "Other… (enter opens its box)"
}

// wrapIndent wraps text to width after prefix, its later lines indented to
// match, in style: nothing of it is cut.
func wrapIndent(prefix, style, text string, width int) []string {
	indent := strings.Repeat(" ", ansi.StringWidth(prefix))
	var out []string
	for i, l := range strings.Split(ansi.Wrap(text, max(width-len(indent), 10), " "), "\n") {
		lead := indent
		if i == 0 {
			lead = prefix
		}
		out = append(out, lead+style+l+"\033[0m")
	}
	return out
}

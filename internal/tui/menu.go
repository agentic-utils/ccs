package tui

// The menu a held AskUserQuestion is answered with, in the main view's
// bottom pane, as Claude Code's own menu works: one question at a time,
// ↑/↓ (j/k) over its options and a last "Other…" row, Enter picks (a digit
// highlights an option, Other… included), Space ticks a pick-any
// question's options and Enter confirms them. "Other…" is a text field on
// its row, as Claude Code's "Type something." is: with ❯ on it every key
// typed is the answer's (n, j/k, digits and Space included), ↑/↓ move off
// it keeping the text, Enter sends it (a pick-any question: it counts as
// ticked, joined to the ticks), Esc leaves for the list keeping it. n adds
// a note to the question's answer (as Claude Code's own menu does); on a
// single-select question it also chooses the highlighted option. ← goes
// back a question keeping the answers. One question is sent as soon as
// it's answered; several end on a review (Enter submits, Esc goes back).
// The menu takes these keys as soon as its session is selected with the
// search empty (held.go, heldKeysActive); any other key goes to the
// search, except on the Other… row. A stray key never answers an unread
// question: Enter, a digit, Space and letters for the field do nothing
// for menuFresh after a question first shows.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// menuFresh is how long Enter, digits and Space do nothing after a question
// first shows (arrow keys work at once), so a key meant for something else
// can't answer it unread.
const menuFresh = 500 * time.Millisecond

// What has the menu's text box: nothing, the Other… row's field (drawn on
// its row by otherLines) or the note box (drawn under the options by
// menuBoxLines).
const (
	boxNone  = iota
	boxOther // "Other…": your own answer, while ❯ is on its row
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
	drafts  map[int]string  // the text in a question's Other… field
	notes   map[int]string  // a note on a question's answer (Claude Code's annotations)
	editing int             // what has the box: boxNone, boxOther or boxNote
	box     textinput.Model // the Other… field or the note box, whichever is open
	shownAt time.Time       // when this step first showed: menuFresh from it
	revTop  int             // the review's first line on screen, when it's taller than the pane
	revSeen bool            // the review's last line has been on screen: only then does Enter send it
	prevTop int             // the preview box's first line on screen, for prevAt's option
	prevAt  [2]int          // the step and cursor prevTop scrolls: another option's preview starts at its top
}

// previewTop is the first line of the highlighted option's preview on
// screen: 0 once the highlighted option (or question) has changed.
func (mn *heldMenu) previewTop() int {
	if mn.prevAt != [2]int{mn.step, mn.cursor} {
		return 0
	}
	return mn.prevTop
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
	if !ok {
		m.menuShown = ""
		return
	}
	mn := m.menuFor(h)
	if m.menuShown != h.ID {
		m.menuShown, mn.shownAt = h.ID, heldNow()
	}
	if mn.step < len(h.Questions) {
		mn.syncOther(h.Questions[mn.step]) // ❯ on Other… (a question with no options): its field has the keys
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
// the menu doesn't use, which goes to the search instead, and for Esc with
// no note box open, which leaves the menu.
func (m *model) menuKey(h mcp.HeldPrompt, name, k string, msg tea.KeyMsg) (tea.Cmd, bool) {
	mn := m.menuFor(h)
	qs := h.Questions
	if mn.step > len(qs) {
		mn.step = len(qs)
	}
	if mn.step < len(qs) {
		mn.syncOther(qs[mn.step])
	}
	// The appearance guard comes before anything else, the Other… field and
	// the note box included: Enter there would send at once, and a letter
	// typed for something else would land in the field.
	fresh := heldNow().Sub(mn.shownAt) < menuFresh
	pickKey := k == "enter" || k == " " || k == "space" || (len(k) == 1 && k >= "1" && k <= "9")
	typing := msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace
	if fresh && (k == "enter" || (mn.editing == boxNone && pickKey) || (mn.editing == boxOther && typing)) {
		if mn.editing != boxNone && k == "enter" { // say so: a silent Enter in a box reads as stuck
			m.setHeldNote("just shown: press " + keyEnter + " again")
		}
		return nil, true // unread: a stray key never answers it
	}
	if mn.editing == boxNote {
		m.menuNoteKey(mn, k, msg)
		return nil, true
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
			mn.revTop = max(0, mn.revTop-max(m.heldRoom()-2, 1))
			return nil, true
		case "pgdown":
			mn.revTop += max(m.heldRoom()-2, 1)
			return nil, true
		case "left":
			mn.goStep(len(qs)-1, qs)
			return nil, true
		case "esc":
			return nil, false // back to the list; the answers are kept
		}
		return nil, false
	}
	q := qs[mn.step]
	rows := len(q.Options) + 1 // and "Other…"
	// PgUp/PgDn page the highlighted option's preview, a box less a line at a time.
	if k == "pgup" || k == "pgdown" {
		if page, ok := m.previewPage(h, mn); ok {
			if k == "pgup" {
				page = -page
			}
			m.scrollPreview(h, mn, page)
			return nil, true
		}
		return nil, false // nothing to scroll: the list's
	}
	if mn.editing == boxOther {
		return m.menuFieldKey(h, name, mn, q, k, msg)
	}
	switch {
	case k == "up" || k == "k":
		mn.cursor = (mn.cursor - 1 + rows) % rows
	case k == "down" || k == "j":
		mn.cursor = (mn.cursor + 1) % rows
	case k == "left":
		if mn.step > 0 {
			mn.goStep(mn.step-1, qs)
		}
		return nil, true
	case k == "esc":
		return nil, false // back to the list at any step; the answers are kept
	case k == "n" || k == "N":
		if !q.Multi { // a note on an option chooses it: Enter then sends it with the note
			mn.picks[mn.step], mn.other[mn.step] = []int{mn.cursor}, ""
		}
		m.menuOpen(mn, boxNote, mn.notes[mn.step], "a note on your answer")
	case len(k) == 1 && k >= "1" && k <= "9":
		n := int(k[0] - '1')
		if n > len(q.Options) {
			m.setHeldNote(fmt.Sprintf("options here are 1-%d", min(rows, 9)))
			return nil, true
		}
		mn.cursor = n // highlights it (Other… too, numbered last, whose field then takes the keys)
		if q.Multi && n < len(q.Options) {
			mn.toggle(n)
		}
	case k == " " || k == "space":
		if q.Multi {
			mn.toggle(mn.cursor)
		}
	case k == "enter":
		if q.Multi {
			return m.menuConfirm(h, name, mn, q), true
		}
		mn.picks[mn.step], mn.other[mn.step] = []int{mn.cursor}, ""
		return m.menuNext(h, name, mn), true
	default:
		return nil, false // not the menu's: the search takes it
	}
	mn.syncOther(q)
	return nil, true
}

// menuFieldKey is a key while ❯ is on the Other… row, whose field has the
// keys, as in Claude Code's own menu: ↑/↓ move off it (its text kept),
// Enter sends its text (a pick-any question: confirms the ticks and it),
// Esc leaves for the list, anything else edits it (n, j/k, digits and
// Space are letters here).
func (m *model) menuFieldKey(h mcp.HeldPrompt, name string, mn *heldMenu, q mcp.HeldQuestion, k string, msg tea.KeyMsg) (tea.Cmd, bool) {
	rows := len(q.Options) + 1
	switch k {
	case "up":
		mn.cursor = (mn.cursor - 1 + rows) % rows
	case "down":
		mn.cursor = (mn.cursor + 1) % rows
	case "esc":
		return nil, false // back to the list; the text is kept
	case "enter":
		if q.Multi {
			return m.menuConfirm(h, name, mn, q), true
		}
		text := strings.TrimSpace(mn.box.Value())
		if text == "" {
			if mn.notes[mn.step] != "" { // the note isn't the answer: say so, or this reads as a loop
				m.setHeldNote("Other… is empty (your note isn't the answer): type your answer, or ↑ to pick an option")
			} else {
				m.setHeldNote("Other… is empty: type your answer, or ↑ to pick an option")
			}
			return nil, true
		}
		mn.other[mn.step], mn.picks[mn.step] = text, nil
		return m.menuNext(h, name, mn), true
	default:
		mn.box, _ = editKey(mn.box, msg)
		mn.drafts[mn.step] = mn.box.Value()
		return nil, true
	}
	mn.syncOther(q)
	return nil, true
}

// menuConfirm confirms a pick-any question: its ticks, and the Other…
// field's text when there is any, which counts as ticked (Claude Code
// joins them: "Red, violet").
func (m *model) menuConfirm(h mcp.HeldPrompt, name string, mn *heldMenu, q mcp.HeldQuestion) tea.Cmd {
	text := strings.TrimSpace(mn.drafts[mn.step])
	if len(mn.picks[mn.step]) == 0 && text == "" {
		m.setHeldNote("tick at least one (space), or type your own on the Other… row")
		return nil
	}
	if text == "" {
		delete(mn.other, mn.step)
	} else {
		mn.other[mn.step] = text
	}
	return m.menuNext(h, name, mn)
}

// menuNoteKey is a key in the open note box: Enter saves it (removed when
// empty), Esc closes it unchanged, anything else edits it.
func (m *model) menuNoteKey(mn *heldMenu, k string, msg tea.KeyMsg) {
	switch k {
	case "esc":
		mn.editing = boxNone
		mn.box.Blur()
	case "enter":
		if text := strings.TrimSpace(mn.box.Value()); text == "" {
			delete(mn.notes, mn.step)
		} else {
			mn.notes[mn.step] = text
		}
		mn.editing = boxNone
		mn.box.Blur()
	default:
		mn.box, _ = editKey(mn.box, msg)
	}
}

func (mn *heldMenu) toggle(n int) {
	if i := slices.Index(mn.picks[mn.step], n); i >= 0 {
		mn.picks[mn.step] = slices.Delete(mn.picks[mn.step], i, i+1)
		return
	}
	mn.picks[mn.step] = append(mn.picks[mn.step], n)
	slices.Sort(mn.picks[mn.step])
}

// otherPlaceholder is the empty Other… field's dim text while ❯ is on it.
const otherPlaceholder = "Other… type your answer"

// syncOther gives the Other… field the keys while ❯ is on its row, with
// the text typed there before, and takes them back (keeping the text) once
// ❯ moves off it. Called after every move of the cursor or the step.
func (mn *heldMenu) syncOther(q mcp.HeldQuestion) {
	on := mn.cursor == len(q.Options)
	switch {
	case on && mn.editing == boxNone:
		text := mn.drafts[mn.step]
		if text == "" {
			text = mn.other[mn.step]
		}
		mn.menuOpen(boxOther, text, otherPlaceholder)
	case !on && mn.editing == boxOther:
		mn.leaveField()
	}
}

// leaveField takes the keys back from the Other… field, its text kept.
func (mn *heldMenu) leaveField() {
	if mn.editing == boxOther {
		mn.drafts[mn.step] = mn.box.Value()
		mn.editing = boxNone
		mn.box.Blur()
	}
}

func (m *model) menuOpen(mn *heldMenu, kind int, text, placeholder string) {
	mn.menuOpen(kind, text, placeholder)
}

func (mn *heldMenu) menuOpen(kind int, text, placeholder string) {
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
	mn.revTop, mn.revSeen = 0, false
	mn.goStep(mn.step+1, h.Questions)
	return nil
}

// goStep shows question i (len(qs): the review) with ❯ on its answer, if
// it has one (its Other… field when that's the answer), else on its
// first option; the field it leaves keeps its text.
func (mn *heldMenu) goStep(i int, qs []mcp.HeldQuestion) {
	mn.leaveField()
	mn.step, mn.shownAt, mn.cursor = i, heldNow(), 0
	if i >= len(qs) {
		return
	}
	if p := mn.picks[i]; len(p) > 0 {
		mn.cursor = p[0]
	} else if mn.other[i] != "" {
		mn.cursor = len(qs[i].Options)
	}
	mn.syncOther(qs[i])
}

// menuSubmit sends every question's answer, and its note, by the prompt's id.
func (m *model) menuSubmit(h mcp.HeldPrompt, name string, mn *heldMenu) tea.Cmd {
	labels, other, notes := map[int][]string{}, map[int]bool{}, map[int]string{}
	for i, q := range h.Questions {
		if n := mn.notes[i]; n != "" {
			notes[i] = n
		}
		t := mn.other[i]
		if t == "" && len(mn.picks[i]) == 0 {
			mn.goStep(i, h.Questions)
			m.setHeldNote("question " + fmt.Sprint(i+1) + " has no answer yet")
			return nil
		}
		if t != "" && !q.Multi {
			labels[i], other[i] = []string{t}, true
			continue
		}
		for _, n := range mn.picks[i] {
			labels[i] = append(labels[i], q.Options[n])
		}
		if t != "" { // ticks and your own text, joined as Claude Code sends them
			labels[i], other[i] = []string{strings.Join(append(labels[i], t), ", ")}, true
		}
	}
	return m.answerHeldCmd(h, name, "answer", labels, other, notes)
}

// menuAnswer is a question's answer as the review shows it: the chosen
// options joined with ", ", or "(Other)" and your own text.
func (mn *heldMenu) menuAnswer(i int, q mcp.HeldQuestion) string {
	t := mn.other[i]
	if t != "" && !q.Multi {
		return "\033[22;90m(Other)\033[0;1m " + t
	}
	var out []string
	for _, n := range mn.picks[i] {
		out = append(out, q.Options[n])
	}
	if t != "" {
		out = append(out, "\033[22;90m(Other)\033[0;1m "+t)
	}
	return strings.Join(out, ", ")
}

// menuLines draws the menu in Claude Code's style: one header line (the
// question's header and "i of n", "also in its tab" on the right), the
// question wrapped, a blank line, then the options, "❯" on the highlighted
// one, each "n. label" with its description dim, wrapped and indented
// under it (two lines at most), ticks for pick-any, "Other…" numbered
// last, a note under the chosen option, and, when the question has
// previews, the highlighted option's in a box beside the options (under
// them on a narrow pane); or the review. One dim hint row
// shows only the keys that work at that step, and only while the menu has
// the keys (active).
func (m model) menuLines(h mcp.HeldPrompt, width int, active bool) []string {
	mn := m.menus[h.ID]
	if mn == nil {
		mn = newHeldMenu(h.ID)
	}
	width = max(width, 20)
	qs := h.Questions
	var lines []string
	if mn.step >= len(qs) {
		answers := "answers"
		if len(qs) == 1 {
			answers = "answer"
		}
		lines = append(lines, paneHeader(fmt.Sprintf("\033[1mReview\033[0m \033[90m· %d %s\033[0m", len(qs), answers), width), paneRule(width))
		for i, q := range qs {
			num := fmt.Sprintf("%-3d", i+1)
			lines = append(lines, wrapIndent(num, "\033[90m", q.Question, width)...)
			lines = append(lines, wrapIndent("   → ", "\033[1m", mn.menuAnswer(i, q), width)...)
			if n := mn.notes[i]; n != "" {
				lines = append(lines, wrapIndent("     note: ", "\033[90m", n, width)...)
			}
		}
		if active {
			lines = append(lines, paneRule(width), paneHintsFit(width, keyEnter, "send", "←", "edit", "↑↓", "scroll", keyEsc, "list"))
		}
		return lines
	}
	q := qs[mn.step]
	head := fmt.Sprintf("%d of %d", mn.step+1, len(qs))
	if q.Header != "" {
		head = q.Header + " · " + head
	}
	lines = append(lines, paneHeader("\033[1;"+amber+"m"+head+"\033[0m", width))
	lines = append(lines, wrapIndent("", "\033[1m", q.Question, width)...)
	lines = append(lines, "")
	above := len(lines)
	// With previews, the options take a column of their own (side by side)
	// or the full width with the preview under them.
	ow, side := width, false
	if len(q.Previews) > 0 {
		ow, side = previewColumns(width)
	}
	upper := lines
	lines = mn.optionLines(q, ow, active) // the options, at ow
	if len(q.Previews) > 0 {
		// The box's height never depends on active, so heldSplit's count of
		// the body agrees with what's drawn.
		text, bw, rows, fits := m.previewFit(q, mn, width, above, len(lines))
		switch {
		case side:
			lines = besideColumn(lines, ow, previewBox(text, bw, rows, mn.previewTop()))
		case fits:
			lines = append(append(lines, ""), previewBox(text, bw, rows, mn.previewTop())...)
		case text != "": // no room for a box under the options: say where it is
			lines = append(lines, ansi.Truncate(fmt.Sprintf("\033[90m▸ preview: %d lines in its tab\033[0m", strings.Count(text, "\n")+1), width, "…"))
		}
	}
	lines = append(upper, lines...)
	switch mn.editing {
	case boxNote:
		lines = append(append(lines, ""), menuBoxLines(mn, width)...)
		return append(lines, paneHintsFit(width, keyEnter, "save (empty removes it)", keyEsc, "cancel"))
	}
	if !active {
		return lines
	}
	if len(qs) == 1 { // no review: say what Enter sends, with its note, as the review would
		lines = append(lines, paneRule(width))
		lines = append(lines, wrapIndent("\033[90menter sends\033[0m → ", "\033[1m", mn.pending(q), width)...)
		if nt := mn.notes[mn.step]; nt != "" {
			lines = append(lines, wrapIndent("     note: ", "\033[90m", nt, width)...)
		}
	} else {
		lines = append(lines, "")
	}
	keys := []string{"↑↓", "move", keyEnter, "pick"}
	if q.Multi {
		keys = []string{"↑↓", "move", "space", "tick", keyEnter, "confirm"}
	}
	if mn.editing == boxOther { // the field has the keys: n, ← and Space are its own
		keys = []string{"↑↓", "move", "type", "your answer", keyEnter, "send"}
		if q.Multi {
			keys = []string{"↑↓", "move", "type", "your own (ticks it)", keyEnter, "confirm"}
		}
	} else {
		if m.previewScrolls(h) {
			keys = append(keys, "pgup/dn", "preview")
		}
		keys = append(keys, "n", "note")
		if mn.step > 0 {
			keys = append(keys, "←", "back")
		}
	}
	keys = append(keys, keyEsc, "list")
	return append(lines, paneHintsFit(width, keys...))
}

// optionLines is a question's options as menuLines draws them, ow wide:
// "❯" on the highlighted one, each "n. label" with its description and a
// note under the chosen option, then "Other…".
func (mn *heldMenu) optionLines(q mcp.HeldQuestion, ow int, active bool) []string {
	var lines []string
	cursor := func(i int) string {
		if i == mn.cursor && mn.editing != boxNote {
			return "\033[1;" + amber + "m❯\033[0m "
		}
		return "  "
	}
	// The note shows under the chosen option, else under the highlighted
	// one; never while a box is open, where it would read as the box's text.
	noteAt := -1
	if n := mn.notes[mn.step]; n != "" && mn.editing == boxNone {
		noteAt = mn.cursor
		if p := mn.picks[mn.step]; len(p) > 0 {
			noteAt = p[0]
		} else if mn.other[mn.step] != "" {
			noteAt = len(q.Options)
		}
	}
	number := func(i int) string { return fmt.Sprintf("%d. ", i+1) }
	for i, o := range q.Options {
		mark := ""
		chosen := slices.Contains(mn.picks[mn.step], i)
		if q.Multi {
			mark = "[ ] "
			if chosen {
				mark = "[x] "
			}
		}
		label := o
		if chosen {
			label = "\033[1m" + o + "\033[22m"
			if !q.Multi {
				label += " ✓"
			}
		}
		lines = append(lines, ansi.Truncate(cursor(i)+number(i)+mark+label, ow, "…"))
		indent := strings.Repeat(" ", 2+len(number(i))+len(mark))
		if i < len(q.Descriptions) && q.Descriptions[i] != "" {
			lines = append(lines, descLines(q.Descriptions[i], indent, ow, 2)...)
		}
		if noteAt == i {
			lines = append(lines, wrapIndent(indent+"note: ", "\033[90m", mn.notes[mn.step], ow)...)
		}
	}
	n := len(q.Options)
	mark := ""
	if q.Multi {
		mark = "[ ] "
		if strings.TrimSpace(mn.otherText()) != "" { // typed text ticks it, as in Claude Code
			mark = "[x] "
		}
	}
	lines = append(lines, mn.otherLines(cursor(n)+number(n)+mark, q, ow, active)...)
	if noteAt == n {
		lines = append(lines, wrapIndent(strings.Repeat(" ", 2+len(number(n)))+"note: ", "\033[90m", mn.notes[mn.step], ow)...)
	}
	return lines
}

// menuPreviewSide is the inner width from which a question's previews go
// beside its options, as Claude Code puts them on a wide terminal; on a
// narrower pane the preview goes under the options, the full width.
const menuPreviewSide = 90

// previewGap is the space between the options column and the preview box.
const previewGap = 2

// previewReserve is the rows a question's footer (what Enter sends, the
// hint row, an outcome note) is left under its preview box, so the box is
// cut, never the footer, and the highlighted option stays on screen.
const previewReserve = 5

// previewColumns is the options column's width beside a preview box, and
// whether there is room for one beside them at all.
func previewColumns(width int) (left int, side bool) {
	if width < menuPreviewSide {
		return width, false
	}
	return min(max(width*2/5, 32), 56), true
}

// previewFit is where the highlighted option's preview goes in a question
// drawn width wide with above rows over its options and opts option rows:
// its text, the box's width and its most rows, borders included. Beside the
// options it always fits; under them fits is false when fewer than 3 rows
// are left, and menuLines then says where it is instead.
func (m model) previewFit(q mcp.HeldQuestion, mn *heldMenu, width, above, opts int) (text string, bw, rows int, fits bool) {
	if mn.cursor < len(q.Previews) {
		text = q.Previews[mn.cursor]
	}
	if ow, side := previewColumns(width); side {
		return text, width - ow - previewGap, max(m.heldRoom()-above-previewReserve, 3), true
	}
	rows = m.heldRoom() - above - opts - 1 - previewReserve
	return text, width, rows, rows >= 3
}

// previewView is the highlighted option's preview box as menuLines draws
// it: its body lines and how many show at once. ok is false with no box
// (no previews, the review, a note box open, no room).
func (m model) previewView(h mcp.HeldPrompt, mn *heldMenu) (body []string, view int, ok bool) {
	if mn.step >= len(h.Questions) || mn.editing == boxNote {
		return nil, 0, false
	}
	q := h.Questions[mn.step]
	if len(q.Previews) == 0 {
		return nil, 0, false
	}
	width := max(m.heldInner(), 20)
	above := 2 + len(wrapIndent("", "\033[1m", q.Question, width))
	ow, _ := previewColumns(width)
	text, bw, rows, fits := m.previewFit(q, mn, width, above, len(mn.optionLines(q, ow, false)))
	if !fits {
		return nil, 0, false
	}
	return previewBody(text, bw), max(rows, 3) - 2, true
}

// previewPage is how far PgUp/PgDn move the preview box: its view less a
// line, so a line of the last view stays on screen. ok is false with no
// box, or when the preview fits in it.
func (m model) previewPage(h mcp.HeldPrompt, mn *heldMenu) (int, bool) {
	body, view, ok := m.previewView(h, mn)
	if !ok || len(body) <= view {
		return 0, false
	}
	return max(view-1, 1), true
}

// scrollPreview moves the highlighted option's preview by delta lines,
// kept between its top and the last line on screen.
func (m model) scrollPreview(h mcp.HeldPrompt, mn *heldMenu, delta int) {
	body, view, ok := m.previewView(h, mn)
	if !ok {
		return
	}
	top := mn.previewTop() + delta
	mn.prevTop, mn.prevAt = min(max(top, 0), max(len(body)-view, 0)), [2]int{mn.step, mn.cursor}
}

// previewBody is an option's preview as its box's lines, inner the box's
// width less borders and padding: its text verbatim, line breaks kept, long
// lines wrapped, tabs as four spaces; a dim "no preview" when there's none.
func previewBody(text string, width int) []string {
	inner := max(width, 12) - 4
	if strings.TrimSpace(text) == "" {
		return []string{"\033[90mno preview\033[0m"}
	}
	var body []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n") {
		for _, w := range strings.Split(ansi.Wrap(l, inner, " "), "\n") {
			body = append(body, strings.TrimRight(w, " "))
		}
	}
	return body
}

// previewBox draws an option's preview as Claude Code does: in a grey box
// width wide (previewBody), at most rows rows tall borders included. A
// preview taller than that shows from line top, and the box's right edge
// is a scrollbar (scrollThumb, barCell) saying where; PgUp/PgDn and the
// wheel scroll it (scrollPreview). The highlighted option without a
// preview (Other… included) gets a dim "no preview", so the layout doesn't
// jump.
func previewBox(text string, width, rows, top int) []string {
	width, rows = max(width, 12), max(rows, 3)
	inner := width - 4
	body := previewBody(text, width)
	view := rows - 2
	top = min(max(top, 0), max(len(body)-view, 0))
	thumb, size, bar := scrollThumb(len(body), top, view)
	if len(body) > view {
		body = body[top : top+view]
	}
	edge := "\033[90m"
	out := []string{edge + "┌" + strings.Repeat("─", width-2) + "┐\033[0m"}
	for r, l := range body {
		l = ansi.Truncate(l, inner, "…")
		right := edge + "│\033[0m"
		if bar {
			right = barCell(r, thumb, size)
		}
		out = append(out, edge+"│\033[0m "+l+"\033[0m"+strings.Repeat(" ", max(inner-ansi.StringWidth(l), 0))+" "+right)
	}
	return append(out, edge+"└"+strings.Repeat("─", width-2)+"┘\033[0m")
}

// besideColumn puts right beside left, left padded to width and a
// previewGap between them, as many rows as the taller of the two.
func besideColumn(left []string, width int, right []string) []string {
	out := make([]string, max(len(left), len(right)))
	for i := range out {
		l, r := "", ""
		if i < len(left) {
			l = ansi.Truncate(left[i], width, "…") + "\033[0m"
		}
		if i < len(right) {
			r = right[i]
		}
		out[i] = l + strings.Repeat(" ", max(width-ansi.StringWidth(l), 0)+previewGap) + r
	}
	return out
}

// descLines is an option's description: dim, wrapped to width after
// indent, at most max lines, the last cut with "…" if there was more.
func descLines(text, indent string, width, most int) []string {
	room := max(width-ansi.StringWidth(indent), 10)
	wrapped := strings.Split(ansi.Wordwrap(text, room, " "), "\n")
	cutMore := len(wrapped) > most
	if cutMore {
		wrapped = wrapped[:most]
	}
	out := make([]string, 0, len(wrapped))
	for i, w := range wrapped {
		w = strings.TrimRight(w, " ")
		if i == len(wrapped)-1 && cutMore {
			w = ansi.Truncate(w, room-1, "") + "…"
		}
		out = append(out, indent+"\033[90m"+ansi.Truncate(w, room, "…")+"\033[0m")
	}
	return out
}

// pending is what Enter would send for this question now: the highlighted
// option, the ticked ones of a pick-any question, or Other…'s text.
func (mn *heldMenu) pending(q mcp.HeldQuestion) string {
	t := strings.TrimSpace(mn.otherText())
	if q.Multi {
		var out []string
		for _, n := range mn.picks[mn.step] {
			out = append(out, q.Options[n])
		}
		if t != "" {
			out = append(out, "(Other) "+t)
		}
		if len(out) == 0 {
			return "nothing ticked yet"
		}
		return strings.Join(out, ", ")
	}
	if mn.cursor < len(q.Options) {
		return q.Options[mn.cursor]
	}
	if t != "" {
		return "(Other) " + t
	}
	return "nothing yet: type your answer"
}

// otherText is the Other… field's text now: the field's while it has the
// keys, else what was typed there before, else the answer it gave.
func (mn *heldMenu) otherText() string {
	if mn.editing == boxOther {
		return mn.box.Value()
	}
	if t := mn.drafts[mn.step]; t != "" {
		return t
	}
	return mn.other[mn.step]
}

// otherLines draws the Other… row as the text field it is: prefix (❯, its
// number, a pick-any question's tick), then its text soft-wrapped as
// menuBoxLines wraps a box, with the cursor (reverse video) while the
// field has the keys and the menu is active; "Other…" while it's empty
// and ❯ is elsewhere; " ✓" after an answer it gave. The rows depend on the
// menu's state only, never on active, so heldSplit's count agrees.
func (mn *heldMenu) otherLines(prefix string, q mcp.HeldQuestion, width int, active bool) []string {
	text := mn.otherText()
	focused := mn.editing == boxOther
	if !focused {
		if text == "" {
			return []string{ansi.Truncate(prefix+"Other…", width, "…")}
		}
		rows := fieldRows(prefix, []rune(text), len([]rune(text)), "", width, false)
		if !q.Multi && mn.other[mn.step] != "" && mn.other[mn.step] == strings.TrimSpace(text) {
			last := len(rows) - 1
			rows[last] = ansi.Truncate(rows[last]+" ✓", width, "…")
		}
		return rows
	}
	pos := min(max(mn.box.Position(), 0), len([]rune(text)))
	return fieldRows(prefix, []rune(text), pos, otherPlaceholder, width, active)
}

// wrapIndent wraps text to width after prefix, its later lines indented to
// match, in style: nothing of it is cut.
func wrapIndent(prefix, style, text string, width int) []string {
	indent := strings.Repeat(" ", ansi.StringWidth(prefix))
	var out []string
	for i, l := range strings.Split(ansi.Wrap(text, max(width-len(indent), 10), " "), "\n") {
		l = strings.TrimRight(l, " ")
		lead := indent
		if i == 0 {
			lead = prefix
		}
		out = append(out, lead+style+l+"\033[0m")
	}
	return out
}

// paneRule is a dim rule across a held prompt's box, between its parts.
func paneRule(width int) string { return "\033[90m" + strings.Repeat("─", max(width, 1)) + "\033[0m" }

// menuBoxMaxLines is how many lines an open Other… or note box grows to.
const menuBoxMaxLines = 4

// menuBoxLines draws the open Other… or note box soft-wrapped to width: its
// text split into rows that fit, at most menuBoxMaxLines of them, the rows
// on screen following the cursor, which is always drawn (reverse video).
// The text itself never gets a newline; editing is the textinput's
// (editKey), only the drawing is ours, since a textinput scrolls sideways.
func menuBoxLines(mn *heldMenu, width int) []string {
	prefix := "\033[1;" + amber + "m›\033[0m "
	if mn.editing == boxNote {
		prefix = "\033[90mnote\033[0m \033[1;" + amber + "m›\033[0m "
	}
	val := []rune(mn.box.Value())
	pos := min(max(mn.box.Position(), 0), len(val))
	return fieldRows(prefix, val, pos, mn.box.Placeholder, width, true)
}

// fieldRows draws text val after prefix soft-wrapped to width, at most
// menuBoxMaxLines rows following the cursor at pos, which is drawn (reverse
// video) when show is set; an empty val is the dim placeholder. Whether the
// cursor shows never changes the rows.
func fieldRows(prefix string, val []rune, pos int, placeholder string, width int, show bool) []string {
	pw := ansi.StringWidth(prefix)
	room := max(width-pw, 10)
	cursor := func(r string) string {
		if !show {
			return r
		}
		return "\033[7m" + r + "\033[27m"
	}
	if len(val) == 0 { // the cursor on the placeholder's first letter, as a textinput draws it
		ph := []rune(ansi.Truncate(placeholder, max(room, 2), "…"))
		if len(ph) == 0 {
			return []string{prefix + cursor(" ")}
		}
		return []string{prefix + "\033[90m" + cursor(string(ph[0])) + string(ph[1:]) + "\033[0m"}
	}
	type span struct{ start, end int }
	var rows []span
	cells := func(rs []rune) (n int) {
		for _, r := range rs {
			n += max(ansi.StringWidth(string(r)), 1)
		}
		return n
	}
	start, w, brk := 0, 0, -1 // brk: just after the row's last space, where a word wrap cuts
	for i, r := range val {
		rw := max(ansi.StringWidth(string(r)), 1)
		if w+rw > room {
			cut := i
			if brk > start && r != ' ' { // wrap the word whole; a word longer than the row is cut
				cut = brk
			}
			rows = append(rows, span{start, cut})
			start, w, brk = cut, cells(val[cut:i]), -1
		}
		w += rw
		if r == ' ' {
			brk = i + 1
		}
	}
	rows = append(rows, span{start, len(val)})
	if pos == len(val) && w >= room { // the cursor past a full last row starts a new one
		rows = append(rows, span{len(val), len(val)})
	}
	cur := len(rows) - 1
	for i, r := range rows {
		if pos >= r.start && pos < r.end {
			cur = i
			break
		}
	}
	top := max(cur-(menuBoxMaxLines-1), 0)
	if len(rows)-top > menuBoxMaxLines && cur < top+menuBoxMaxLines {
		rows = rows[:top+menuBoxMaxLines]
	}
	rows = rows[top:]
	indent := strings.Repeat(" ", pw)
	out := make([]string, 0, len(rows))
	for i, r := range rows {
		lead := indent
		if i == 0 && top == 0 {
			lead = prefix
		} else if i == 0 {
			lead = strings.Repeat(" ", max(pw-2, 0)) + "\033[90m…\033[0m " // earlier lines are above
		}
		text := ""
		if pos >= r.start && pos < r.end {
			text = string(val[r.start:pos]) + cursor(string(val[pos])) + string(val[pos+1:r.end])
		} else {
			text = string(val[r.start:r.end])
			if i+top == cur { // the cursor at the end of the text
				text += cursor(" ")
			}
		}
		out = append(out, lead+text)
	}
	return out
}

// previewScrolls says the selected held question's highlighted option has
// a preview taller than its box, which PgUp/PgDn and the wheel then scroll.
func (m model) previewScrolls(h mcp.HeldPrompt) bool {
	_, ok := m.previewPage(h, mn(m, h))
	return ok
}

// heldPreviewScrolls is previewScrolls for the held prompt on screen.
func (m model) heldPreviewScrolls() bool {
	h, _, ok := m.heldDrawn()
	return ok && len(h.Questions) > 0 && m.previewScrolls(h)
}

// overPreviewBox says screen cell (x, y) is on the held question's preview
// box, so the wheel there scrolls it (scrollPreview) and not the
// conversation. The box is found as drawn: its corners' columns, and the
// rows whose cell there is its left edge.
func (m model) overPreviewBox(x, y int) bool {
	if !m.heldPreviewScrolls() {
		return false
	}
	lines := m.chatAsk()
	row := y - (m.height - m.chatRows() + 1) // the status line is above the held box
	if row < 0 || row >= len(lines) {
		return false
	}
	paneX := 0
	if sw := m.sidebarWidth(); sw > 0 {
		paneX = sw + 1
	}
	left, right := -1, -1
	for _, l := range lines {
		s := ansi.Strip(l)
		for _, c := range []string{"┌", "└"} {
			i, j := strings.Index(s, c), strings.LastIndexAny(s, "┐┘")
			if i >= 0 && j > i {
				left = ansi.StringWidth(s[:i])
				right = left + ansi.StringWidth(s[i:j]) // the right corner's column
			}
		}
	}
	col := x - paneX
	if left < 0 || col < left || col > right {
		return false
	}
	edge := ansi.Strip(ansi.Cut(lines[row], left, left+1))
	return edge == "┌" || edge == "│" || edge == "└"
}

// notePreview starts the held menu's preview box at its top whenever the
// highlighted option or question has changed since it was scrolled, so
// coming back to an option shows its preview from the top too. From
// Update, after every message, as noteReview is.
func (m *model) notePreview() {
	for _, mn := range m.menus {
		if at := [2]int{mn.step, mn.cursor}; mn.prevAt != at {
			mn.prevTop, mn.prevAt = 0, at
		}
	}
}

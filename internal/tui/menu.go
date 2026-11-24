package tui

// The menu a held AskUserQuestion is answered with, in the main view's
// bottom pane, as Claude Code's own menu works: one question at a time,
// ↑/↓ (j/k) over its options and a last "Other…" row, Enter picks (a digit
// highlights an option, Other… included), Space ticks a pick-any
// question's options and Enter confirms them, "Other…" opens a wrapping box
// for your own answer (Enter, or typing on its row: every key typed then is
// the answer's, n, digits and Space included; Enter sends it, Esc keeps it
// as a draft), n outside a box adds a note to the question's answer (as
// Claude Code's own menu does), ← goes back a question keeping the answers.
// One question is sent as soon as it's answered; several end on a review
// (Enter submits, Esc goes back). The menu takes these keys as soon as its
// session is selected with the search empty (held.go, heldKeysActive); any
// other key goes to the search, except on the Other… row, where it starts
// your answer. A stray key never answers an unread question: Enter,
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
)

// menuFresh is how long Enter, digits and Space do nothing after a question
// first shows (arrow keys work at once), so a key meant for something else
// can't answer it unread.
const menuFresh = 500 * time.Millisecond

// The menu's boxes, drawn soft-wrapped by menuBoxLines.
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
	if !ok {
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
		if mn.editing != boxNone && k == "enter" { // say so: a silent Enter in a box reads as stuck
			m.setHeldNote("just shown: press " + keyEnter + " again to save")
		}
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
			mn.revTop = max(0, mn.revTop-max(m.heldRoom()-2, 1))
			return nil, true
		case "pgdown":
			mn.revTop += max(m.heldRoom()-2, 1)
			return nil, true
		case "left":
			mn.step, mn.shownAt = len(qs)-1, heldNow()
			return nil, true
		case "esc":
			return nil, false // back to the list; the answers are kept
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
		return nil, false // back to the list at any step; the answers are kept
	case k == "n" || k == "N":
		m.menuOpen(mn, boxNote, mn.notes[mn.step], "a note on your answer")
	case len(k) == 1 && k >= "1" && k <= "9":
		n := int(k[0] - '1')
		if n > len(q.Options) {
			m.setHeldNote(fmt.Sprintf("options here are 1-%d", min(rows, 9)))
			return nil, true
		}
		mn.cursor = n // highlights it (Other… too, numbered last); Enter picks or opens its box
		if q.Multi && n < len(q.Options) {
			mn.toggle(n)
		}
	case mn.cursor == len(q.Options) && msg.Type == tea.KeyRunes && !msg.Alt:
		// Typing on the Other… row starts your answer, as in Claude Code's
		// own menu: the box opens with the text typed. The row's own keys
		// (j/k, n, digits, Space) are taken above.
		m.menuOtherBox(mn)
		mn.box, _ = editKey(mn.box, msg)
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
			if mn.notes[mn.step] != "" { // the note isn't the answer: say so, or this reads as a loop
				m.setHeldNote("Other… is empty (your note isn't the answer): type your answer, or esc")
			} else {
				m.setHeldNote("Other… is empty: type your answer, or esc")
			}
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

// menuAnswer is a question's answer as the review shows it: the chosen
// options joined with ", ", or "(Other)" and your own text.
func (mn *heldMenu) menuAnswer(i int, q mcp.HeldQuestion) string {
	if t := mn.other[i]; t != "" {
		return "\033[22;90m(Other)\033[0;1m " + t
	}
	var out []string
	for _, n := range mn.picks[i] {
		out = append(out, q.Options[n])
	}
	return strings.Join(out, ", ")
}

// menuLines draws the menu in Claude Code's style: one header line (the
// question's header and "i of n", "also in its tab" on the right), the
// question wrapped, a blank line, then the options, "❯" on the highlighted
// one, each "n. label" with its description dim, wrapped and indented
// under it (two lines at most), ticks for pick-any, "Other…" numbered
// last, a note under the chosen option; or the review. One dim hint row
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
	cursor := func(i int) string {
		if i == mn.cursor && mn.editing == boxNone {
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
		lines = append(lines, ansi.Truncate(cursor(i)+number(i)+mark+label, width, "…"))
		indent := strings.Repeat(" ", 2+len(number(i))+len(mark))
		if i < len(q.Descriptions) && q.Descriptions[i] != "" {
			lines = append(lines, descLines(q.Descriptions[i], indent, width, 2)...)
		}
		if noteAt == i {
			lines = append(lines, wrapIndent(indent+"note: ", "\033[90m", mn.notes[mn.step], width)...)
		}
	}
	other := "Other…"
	if t := mn.other[mn.step]; t != "" {
		other += " \033[1m“" + t + "”\033[22m"
	}
	n := len(q.Options)
	lines = append(lines, ansi.Truncate(cursor(n)+number(n)+other, width, "…"))
	if noteAt == n {
		lines = append(lines, wrapIndent(strings.Repeat(" ", 2+len(number(n)))+"note: ", "\033[90m", mn.notes[mn.step], width)...)
	}
	switch mn.editing {
	case boxOther:
		lines = append(append(lines, ""), menuBoxLines(mn, width)...)
		return append(lines, paneHintsFit(width, keyEnter, "save", keyEsc, "cancel"))
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
	if mn.cursor == len(q.Options) {
		keys = []string{"↑↓", "move", "type", "your answer"}
	}
	if q.Multi {
		keys = []string{"↑↓", "move", "space", "tick", keyEnter, "confirm"}
	}
	keys = append(keys, "n", "note")
	if mn.step > 0 {
		keys = append(keys, "←", "back")
	}
	keys = append(keys, keyEsc, "list")
	return append(lines, paneHintsFit(width, keys...))
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
	if q.Multi {
		if t := mn.other[mn.step]; t != "" {
			return "(Other) " + t
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
		return "(Other) " + t
	}
	return "Other… (type, or enter, to answer)"
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
	pw := ansi.StringWidth(prefix)
	room := max(width-pw, 10)
	val := []rune(mn.box.Value())
	pos := min(max(mn.box.Position(), 0), len(val))
	cursor := func(r string) string { return "\033[7m" + r + "\033[27m" }
	if len(val) == 0 {
		ph := ansi.Truncate(mn.box.Placeholder, max(room-1, 1), "…")
		return []string{prefix + cursor(" ") + "\033[90m" + ph + "\033[0m"}
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

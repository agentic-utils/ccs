package tui

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Orchestrator trees: a session that started others through launch_session
// or ccs launch (or adopted them, ccs adopt / adopt_session) is a
// orchestrator. Its family sits in the orchestrator section as a tree, the
// sessions it launched indented under it, sub-orchestrators' under theirs.
// ^X C shows or hides the closed sessions behind one "N closed" line, the
// one the selection sits under (foldOwner), each fold on its own and kept
// in the settings; a search shows every match, closed or not.

// reloadParents rereads launch-parents.json when it changed, from each
// live tick, so a launch or adoption from any session shows up.
func (m *model) reloadParents() {
	info, err := os.Stat(sessions.ParentsPath())
	mod := time.Time{}
	if err == nil {
		mod = info.ModTime()
	}
	if mod.Equal(m.launchMod) && m.launchLoaded {
		return
	}
	m.launchMod, m.launchLoaded = mod, true
	m.launchRecs = sessions.LoadParents()
	m.launchResN = -1
}

// launchParents is each listed session's recorded parent, by session id,
// worked out again when the records or the list's length change.
func (m *model) launchParents() map[string]string {
	if !m.launchLoaded {
		m.reloadParents()
	}
	if len(m.launchRecs.Parents) == 0 {
		return nil
	}
	if m.launchResN != len(m.items) {
		convs := make([]transcript.Conversation, len(m.items))
		for i := range m.items {
			convs[i] = m.items[i].conv
		}
		m.launchRes, m.launchResN = sessions.ResolveParents(convs, m.launchRecs), len(m.items)
	}
	return m.launchRes
}

// Each parent's closed sessions wait behind its own "▸ N closed" line until
// ^X C opens that one fold (m.openClosed, by the parent's session id, saved
// in the settings as OpenClosed). Every fold starts closed: the old global
// view (tree_view) is no longer read, and the next save drops it.

// showingClosed: the closed sessions under the parent with this session id
// are listed in its tree.
func (m model) showingClosed(id string) bool { return m.openClosed[id] }

// foldOwner is the parent whose "N closed" line ^X C opens or closes from
// the row at i: the session itself when it has closed sessions under it,
// else the nearest ancestor that has, which is the fold the row sits in.
// ok is false when no fold is above it.
func (m model) foldOwner(i int) (id, name string, ok bool) {
	for i >= 0 && i < len(m.filtered) {
		it := m.filtered[i]
		if it.closedKids > 0 {
			return it.conv.SessionID, foldName(it.conv), true
		}
		if it.depth == 0 {
			break
		}
		i = m.parentRow(i)
	}
	return "", "", false
}

// parentRow is the row of the session the row at i is nested under: the
// nearest row above it that is shallower. -1 for a top-level row. A nested
// row's parent is always listed (nestFamilies nests only under a shown
// session, and a fold or a closed line hides a parent's whole tree with it),
// so the parent's own row is the one found, never a further ancestor.
func (m model) parentRow(i int) int {
	if i < 0 || i >= len(m.filtered) || m.filtered[i].depth == 0 {
		return -1
	}
	d := m.filtered[i].depth
	j := i - 1
	for j >= 0 && m.filtered[j].depth >= d {
		j--
	}
	return j
}

// foldName is how a fold's owner is named in notes and help.
func foldName(c transcript.Conversation) string {
	return transcript.Truncate(transcript.GetTopic(c), 28)
}

// toggleClosed is ^X C: the fold the selection sits in, opened or closed.
func (m *model) toggleClosed() {
	if len(m.filtered) == 0 {
		return
	}
	id, name, ok := m.foldOwner(min(m.cursor, len(m.filtered)-1))
	if !ok {
		m.note("no closed sessions under this one")
		return
	}
	m.toggleFold(id, name)
}

// toggleFold opens or closes the closed sessions under the session id,
// keeping the selection: on the same session, or on the fold's owner when
// closing it hid the one selected.
func (m *model) toggleFold(id, name string) {
	if m.openClosed == nil {
		m.openClosed = map[string]bool{}
	}
	if m.openClosed[id] {
		delete(m.openClosed, id)
		delete(m.foldShown, id) // opened again, it starts at its first page
		m.note("closed sessions under " + name + " hidden; ^X C shows them")
	} else {
		m.openClosed[id] = true
		m.note("closed sessions under " + name + " shown; ^X C hides them")
	}
	saveSettings(m.settings())
	sel := ""
	if len(m.filtered) > 0 {
		sel = m.filtered[min(m.cursor, len(m.filtered)-1)].conv.SessionID
	}
	m.sortList()
	if sel == "" || slices.ContainsFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == sel }) {
		return
	}
	if i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id }); i >= 0 {
		m.cursor = i
		m.previewScroll = 0
		m.selectionMoved()
	}
}

// openFolds is m.openClosed as the settings keep it, sorted.
func (m model) openFolds() []string {
	ids := make([]string, 0, len(m.openClosed))
	for id := range m.openClosed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// foldColour draws the closed lines: ccs's accent (the focused box's border
// and the selection), so a fold's controls stand out from the grey rows.
const foldColour = "38;5;62"

// foldPage is how many closed sessions an open fold lists at first, and how
// many more each time its footer scrolls into view (revealFolds).
const foldPage = 20

// foldShownCount is how many of a fold's n closed sessions are listed.
func (m model) foldShownCount(id string, n int) int {
	return min(max(m.foldShown[id], foldPage), n)
}

// revealFolds lists another foldPage of each open fold whose footer is on
// screen with sessions still behind it, until none is: an open fold fills
// the list as it scrolls, never all at once. The selection stays put.
func (m *model) revealFolds() {
	for range 100 { // each pass lists at least foldPage more, so this ends
		_, h, _ := m.listLayout()
		rows := m.listRows()
		start, _, body := m.listView(rows, h)
		grew := false
		for r := start; r < min(len(rows), start+body); r++ {
			if l := rows[r].line; rows[r].fold != "" && l.footer && l.shown < l.n {
				if m.foldShown == nil {
					m.foldShown = map[string]int{}
				}
				m.foldShown[l.id] = l.shown + foldPage
				grew = true
			}
		}
		if !grew {
			return
		}
		m.sortList()
		m.noteListTop()
	}
}

// stickyFold is the open fold the list's view starts inside, its "▾ N
// closed" heading above start and its footer at or below it: the innermost
// when folds nest. ok is false when the view starts in none.
func stickyFold(rows []listRow, start int) (l closedLine, ok bool) {
	var open []closedLine
	for _, row := range rows[:min(start, len(rows))] {
		if row.fold == "" || !row.line.open {
			continue
		}
		if !row.line.footer {
			open = append(open, row.line)
			continue
		}
		if i := slices.IndexFunc(open, func(o closedLine) bool { return o.id == row.line.id }); i >= 0 {
			open = open[:i]
		}
	}
	if len(open) == 0 {
		return closedLine{}, false
	}
	return open[len(open)-1], true
}

// listView is the list's view of h rows: the first row of rows in view, and,
// when that falls inside an open fold whose heading has scrolled off, the
// heading pinned on the view's top row, leaving body rows for the list.
// Everything that draws the list or reads a click goes through it.
//
// The pin covers the view's top row, so the list below it starts one row
// later and its bottom row stays where it was, unless the selection starts
// on that top row, which then stays and the bottom row gives way. m.listTop
// keeps listStart's own start, so pinning never moves the list on its own.
func (m model) listView(rows []listRow, h int) (start int, pin *closedLine, body int) {
	start = m.listStart(rows, h)
	if _, ok := stickyFold(rows, start); !ok || h < 2 {
		return start, nil, h
	}
	s := start + 1
	if start < len(rows) && rows[start].item == m.cursor {
		s = start
	}
	for s < len(rows) && rows[s].part > 0 { // never under half a session
		s++
	}
	if l, ok := stickyFold(rows, s); ok {
		return s, &l, h - 1
	}
	return start, nil, h
}

// closedLineText draws a closed line in foldColour, lined up with the rows'
// connectors: the tree's guides, then └ ▸ N closed (hidden behind it),
// ├ ▾ N closed (shown below it) or, ending an open fold, └ ▴ hide N closed,
// and the key that changes that, so it can be found where the closed
// sessions are.
func (m model) closedLineText(l closedLine) string {
	conn, text := "└", fmt.Sprintf("▸ %d closed · ^X C", l.n)
	switch {
	case l.footer:
		text = fmt.Sprintf("▴ hide %d closed · ^X C", l.n)
		if l.shown < l.n {
			text += fmt.Sprintf(" · %d of %d listed, more as you scroll", l.shown, l.n)
		}
	case l.open:
		conn, text = "├", fmt.Sprintf("▾ %d closed · ^X C", l.n)
	}
	return m.foldLine(l.guides + conn + " " + text)
}

// stickyText is an open fold's heading pinned at the top of the list while
// its own line is scrolled off: at its level, saying how to hide it.
func (m model) stickyText(l closedLine) string {
	return m.foldLine(fmt.Sprintf("%s├ ▾ %d closed · ^X C hide", l.guides, l.n))
}

// foldLine indents a closed line's text to the rows' connectors and colours it.
func (m model) foldLine(text string) string {
	lead := " "
	if m.grouped {
		lead = "   "
	}
	return lead + strings.Repeat(" ", colMarks-2) + "\033[" + foldColour + "m" + text + "\033[0m"
}

// inTree: the session is in the orchestrator section, drawn as a tree.
func inTree(it listItem) bool { return sectionOf(it.familyState) == orchestratorSec }

// treeLines is what an orchestrator's tree draws before a row, on its title
// line and on its grey line: the │ its ancestors' later siblings carry
// down, then its own ├ or └ (│ or space below it, on the grey line). A
// child's connector sits under its parent's agent glyph, so its glyph sits
// under the parent's name. "" for the tree's top and for any row outside a
// tree.
func treeLines(it listItem) (title, grey string) {
	if !inTree(it) || it.depth == 0 {
		return "", ""
	}
	if it.lastKid {
		return it.guides + "└ ", it.guides + "  "
	}
	return it.guides + "├ ", it.guides + "│ "
}

// treePrefix is what goes before a title: a folded row's ▸N, and outside the
// sidebar's tree layout (the table, a spawned session) its connector too.
func (m model) treePrefix(it listItem) string {
	if !inTree(it) {
		return nestPrefix(it.depth)
	}
	conn, _ := treeLines(it)
	if !m.table {
		conn = "" // the sidebar draws it before the agent's glyph (sidebarRow)
	}
	return conn
}

// titleColour is the colour a row's title says its state in, in place of a
// status dot: magenta waiting on a menu or prompt, light magenta its last
// message asks something, amber busy, cyan done since you last looked;
// plain when idle, so the list isn't all one colour; dim when it isn't
// running.
func (m model) titleColour(it listItem) string {
	conv := it.conv
	if conv.Kind == transcript.KindSubagent {
		return m.subagentTitleColour(conv)
	}
	if !m.live[conv.SessionID] {
		return "90"
	}
	switch m.waitSignal(conv) {
	case waitPrompt:
		return waitColour
	case waitAsked:
		return askedColour
	}
	if m.chatStatus[conv.SessionID].Status == "busy" {
		return amber
	}
	if m.unseen(conv) && conv.SessionID != m.viewing { // being read now: no longer unread
		return doneColour
	}
	return ""
}

// paintTitle colours a title, leaving it as it is for "".
func paintTitle(title, colour string) string {
	if colour == "" {
		return title
	}
	return "\033[" + colour + "m" + title + "\033[0m"
}

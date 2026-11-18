package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Orchestrator trees: a session that started others through launch_session
// or ccs launch (or adopted them, ccs adopt / adopt_session) is a
// orchestrator. Its family sits in the orchestrator section as a tree, the
// sessions it launched indented under it, sub-orchestrators' under theirs.
// ^X C shows or hides closed sessions in every tree at once (saved in the
// settings); a search shows every match, closed or not.

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

// The tree views, one for every tree at once: ^X C toggles them, saved in
// the settings (TreeView). A view saved by an older ccs that folded every
// tree (2) reads as treeLive.
const (
	treeLive   = iota // closed sessions wait behind each parent's "▸ N closed" line
	treeClosed        // closed sessions shown where they belong, beside the live ones
)

// treeView is the saved ^X C view: shown, or else hidden, the old folded
// view (2) included.
func (s settings) treeView() int {
	if s.TreeView == treeClosed {
		return treeClosed
	}
	return treeLive
}

// showingClosed: closed sessions are listed in their trees.
func (m model) showingClosed() bool { return m.treeView == treeClosed }

// toggleClosed is ^X C: closed sessions shown in every tree, or hidden.
func (m *model) toggleClosed() {
	if m.treeView == treeClosed {
		m.treeView = treeLive
		m.note("closed sessions hidden; ^X C shows them")
	} else {
		m.treeView = treeClosed
		m.note("closed sessions shown; ^X C hides them")
	}
	saveSettings(m.settings())
	m.sortList()
}

// closedLineText draws a closed line, all dim and lined up with the rows'
// connectors: the tree's guides, then └ ▸ N closed (hidden behind it) or
// ├ ▾ N closed (shown below it), and the key that changes that, so it can
// be found where the closed sessions are.
func (m model) closedLineText(l closedLine) string {
	lead := " "
	if m.grouped {
		lead = "   "
	}
	conn, mark := "└", "▸"
	if l.open {
		conn, mark = "├", "▾"
	}
	return fmt.Sprintf("%s%s\033[90m%s%s %s %d closed · ^X C\033[0m", lead, strings.Repeat(" ", colMarks-2), l.guides, conn, mark, l.n)
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

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
// sessions it launched indented under it, sub-orchestrators' under theirs,
// and its row counts what its tree holds: waiting on you, stalled, done.
// ^X B folds a tree to its orchestrator row and unfolds it again (saved in
// the marks); a search shows every match, folded or not.

// rollupStall is how long an idle session with nothing unread waits before
// its orchestrator's row counts it stalled: session_triage's default.
const rollupStall = 15 * time.Minute

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

// folded: the orchestrator's tree is folded to its own row.
func (m model) folded(id string) bool {
	return m.marks != nil && slices.Contains(m.marks.Folded, id)
}

// toggleFold is ^X B: fold the selected orchestrator's tree to its row, or
// unfold it. A session with nothing launched under it has nothing to fold.
func (m *model) toggleFold() {
	if m.marks == nil || len(m.filtered) == 0 {
		return
	}
	it := m.filtered[m.cursor]
	if it.kids == 0 {
		m.note("nothing launched under this session to fold")
		return
	}
	id := it.conv.SessionID
	if i := slices.Index(m.marks.Folded, id); i >= 0 {
		m.marks.Folded = slices.Delete(m.marks.Folded, i, i+1)
	} else {
		m.marks.Folded = append(m.marks.Folded, id)
	}
	m.marks.save()
	m.sortList()
}

// rollup counts an orchestrator's tree below it: waiting on you (a menu,
// prompt or question), stalled (idle past rollupStall, nothing unread) and
// done (finished since you last looked).
type rollup struct{ waiting, stalled, done int }

func (m model) rollupOf(it *listItem, now time.Time) rollup {
	var r rollup
	switch {
	case it.state == 0 || it.state == 1:
		r.waiting++
	case it.state == 2 && m.unseen(it.conv):
		r.done++
	case it.state == 2:
		if t, err := time.Parse(time.RFC3339, it.conv.LastTimestamp); err == nil && now.Sub(t) >= rollupStall {
			r.stalled++
		}
	}
	return r
}

func (r *rollup) add(o rollup) {
	r.waiting += o.waiting
	r.stalled += o.stalled
	r.done += o.done
}

// rollupCell is an orchestrator row's counts, plain and coloured: ●N waiting
// on you, ◦N stalled, ✓N done; "" when nothing needs a look.
func rollupCell(r rollup, coloured bool) string {
	var parts []string
	add := func(n int, glyph, colour string) {
		if n == 0 {
			return
		}
		s := fmt.Sprintf("%s%d", glyph, n)
		if coloured {
			s = "\033[" + colour + "m" + s + "\033[0m"
		}
		parts = append(parts, s)
	}
	add(r.waiting, "●", waitColour)
	add(r.stalled, "◦", amber)
	add(r.done, "✓", doneColour)
	return strings.Join(parts, " ")
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
		return nestPrefix(it.depth) + it.foldGlyph()
	}
	conn, _ := treeLines(it)
	if !m.table {
		conn = "" // the sidebar draws it before the agent's glyph (sidebarRow)
	}
	return conn + it.foldGlyph()
}

// treeBadge is a folded tree's counts: what the sessions it hides hold.
// Unfolded, they show their own.
func (m model) treeBadge(it listItem, coloured bool) string {
	if it.hiddenKids > 0 {
		return rollupCell(it.roll, coloured)
	}
	return ""
}

// foldGlyph marks a folded row: ▸ and how many sessions it hides.
func (it listItem) foldGlyph() string {
	switch {
	case it.hiddenKids > 0:
		return fmt.Sprintf("▸%d ", it.hiddenKids)
	}
	return ""
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

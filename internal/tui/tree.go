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

// Coordinator trees: a session that started others through launch_session
// or ccs launch (or adopted them, ccs adopt / adopt_session) is a
// coordinator. Its family sits in the coordinator section as a tree, the
// sessions it launched indented under it, sub-coordinators' under theirs,
// and its row counts what its tree holds: waiting on you, stalled, done.
// ^X B folds a tree to its coordinator row and unfolds it again (saved in
// the marks); a search shows every match, folded or not.

// rollupStall is how long an idle session with nothing unread waits before
// its coordinator's row counts it stalled: session_triage's default.
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

// folded: the coordinator's tree is folded to its own row.
func (m model) folded(id string) bool {
	return m.marks != nil && slices.Contains(m.marks.Folded, id)
}

// toggleFold is ^X B: fold the selected coordinator's tree to its row, or
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

// rollup counts a coordinator's tree below it: waiting on you (a menu,
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

// rollupCell is a coordinator row's counts, plain and coloured: ●N waiting
// on you, ◌N stalled, ✓N done; "" when nothing needs a look.
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
	add(r.stalled, "◌", amber)
	add(r.done, "✓", doneColour)
	return strings.Join(parts, " ")
}

// foldGlyph marks a coordinator's row: ▾ unfolded, ▸ folded with how many
// it hides.
func (it listItem) foldGlyph() string {
	switch {
	case it.kids == 0:
		return ""
	case it.hiddenKids > 0:
		return fmt.Sprintf("▸%d ", it.hiddenKids)
	}
	return "▾ "
}

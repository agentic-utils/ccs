package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

type refreshMsg struct {
	items []listItem
	err   error // scan failed: keep the current list
	live  map[string]bool
	gen   int  // m.gen when the scan started
	early bool // started by the live tick, outside the one-minute schedule
}

// refreshStalled reports a scan running far longer than normal (e.g. blocked
// on a hung network mount under ~/.claude/projects). It can't be cancelled,
// so the header just says so rather than showing a silently frozen list.
// refreshNote says what the background refresh is doing, for the header:
// "refreshing…", "refreshed 20s ago", "refresh failed 3m ago". Empty when
// auto-refresh is off, or when a stall is reported instead.
func (m model) refreshNote() string {
	switch {
	case m.reload == nil || m.refreshStalled():
		return ""
	case !m.refreshStarted.IsZero():
		return " · refreshing…"
	case m.lastRefresh.IsZero():
		return ""
	}
	// Minute granularity: a note that changes every second would make an
	// idle ccs redraw constantly (terminals show that as tab activity).
	when := "just now"
	if ago := time.Since(m.lastRefresh); ago >= time.Minute {
		when = fmt.Sprintf("%dm ago", int(ago.Minutes()))
	}
	if m.refreshFailed {
		return " · refresh failed, list from " + when
	}
	return " · refreshed " + when
}

func (m model) refreshStalled() bool {
	return !m.refreshStarted.IsZero() && time.Since(m.refreshStarted) > 5*time.Minute
}

// keepNewer returns scanned, but where the list holds a copy of a
// conversation read from disk after the scan read it (the live tick got
// there later), keeps that copy, so a slow scan can't roll it back.
func keepNewer(current, scanned []listItem) []listItem {
	have := make(map[string]listItem, len(current))
	for _, item := range current {
		have[item.conv.SessionID] = item
	}
	out := make([]listItem, len(scanned))
	for i, item := range scanned {
		if cur, ok := have[item.conv.SessionID]; ok && cur.conv.FilePath == item.conv.FilePath && cur.conv.ReadAt.After(item.conv.ReadAt) {
			item = cur
		}
		out[i] = item
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].conv.LastTimestamp > out[j].conv.LastTimestamp
	})
	return out
}

// startRefresh begins a full scan off the UI goroutine.
func (m *model) startRefresh(early bool) tea.Cmd {
	m.refreshStarted = time.Now()
	reload, gen := m.reload, m.gen
	return func() tea.Msg {
		items, err := reload()
		return refreshMsg{items: items, err: err, live: sessions.ReadLiveSessions(), gen: gen, early: early}
	}
}

// applyRefresh swaps in freshly loaded items, keeping the cursor on the same
// conversation and re-running the current filter.
func (m *model) applyRefresh(msg refreshMsg) {
	if msg.err != nil {
		return
	}
	var selectedID string
	if len(m.filtered) > 0 {
		selectedID = m.filtered[m.cursor].conv.SessionID
	}
	prevScroll := m.previewScroll
	m.items = msg.items
	m.lastFilterQuery = "" // force a full rescan, not incremental narrowing
	m.hits = &hitCounter{byID: make(map[string]int)}
	m.preview = &previewCache{}
	m.updateFilter()
	for i, item := range m.filtered {
		if item.conv.SessionID == selectedID {
			m.cursor = i
			m.previewScroll = min(prevScroll, m.maxPreviewScroll())
			break
		}
	}
}

// ============================================================================
// Data loading (preserved from original)
// ============================================================================

// buildItems creates list items from conversations
func buildItems(conversations []transcript.Conversation) []listItem {
	items := make([]listItem, 0, len(conversations))
	for _, conv := range conversations {
		if conv.SearchText == "" { // not from parseConversationFile (e.g. tests)
			conv.SearchText = transcript.SearchTextOf(conv)
			conv.SearchLower = strings.ToLower(conv.SearchText)
		}
		items = append(items, listItem{conv: conv, searchText: conv.SearchText, searchLower: conv.SearchLower})
	}
	return items
}

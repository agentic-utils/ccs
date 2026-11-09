package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// A session's PRs are the ones Claude Code itself links to it: a "pr-link"
// record each time a gh command in the session touches a PR (create, view,
// checks, merge...), the same record behind Claude Code's own PR badge.
// Claude Code writes none for a PR named only through shell variables (a
// loop over repos) or for its subagents, so the PRs gh pr create and merge
// print count too, in the session and its subagents (prsOf).

// GitHub's colours: green ready, amber waiting, red to fix or closed, purple merged.
const prPurple = "38;5;141"

// prMark is a group's glyph and colour.
func prMark(group int) (string, string) {
	switch group {
	case github.PRReady:
		return "✓", "32"
	case github.PRFix:
		return "✗", "31"
	case github.PRWaiting:
		return "○", amber
	case github.PRMerged:
		return "●", prPurple
	case github.PRClosed:
		return "●", "31"
	}
	return " ", "90"
}

// sessionPRs is conv's PRs, ready first, merged and closed last, otherwise
// in the order the session first touched them.
func (m model) sessionPRs(conv transcript.Conversation) []string {
	urls := slices.Clone(github.PRsOf(conv))
	slices.SortStableFunc(urls, func(a, b string) int {
		ga, _ := m.prs[a].Status()
		gb, _ := m.prs[b].Status()
		return ga - gb
	})
	return urls
}

// prCounts is how many of conv's PRs are in each group (prReady…prClosed);
// one GitHub can't find isn't counted.
func (m model) prCounts(conv transcript.Conversation) (n [github.PRClosed + 1]int) {
	for _, u := range github.PRsOf(conv) {
		if s := m.prs[u].State; s == "" || s == "GONE" { // not looked up yet, or not found
			continue
		}
		g, _ := m.prs[u].Status()
		n[g]++
	}
	return
}

// prCell is the PRS column: "✓1 ✗1 2 ●3" (ready, needing you, waiting,
// merged; closed ones left out), blank when there are none. Plain for the
// selected row, else coloured.
func (m model) prCell(conv transcript.Conversation, coloured bool) string {
	n := m.prCounts(conv)
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
	add(n[github.PRReady], "✓", "32")
	add(n[github.PRFix], "✗", "31")
	add(n[github.PRWaiting], "", amber)
	add(n[github.PRMerged], "●", prPurple)
	s := strings.Join(parts, " ")
	return strings.Repeat(" ", max(colPRs-ansi.StringWidth(s), 0)) + s
}

// prLines is the preview header's PRs, width wide: one line, a link, for each
// open PR (at most maxPRLines), then one line counting the merged and closed.
func (m model) prLines(conv transcript.Conversation, width int) []string {
	urls := m.sessionPRs(conv)
	if len(urls) == 0 {
		return nil
	}
	var lines []string
	merged, closed, gone := 0, 0, 0
	for _, u := range urls {
		switch g, _ := m.prs[u].Status(); {
		case g == github.PRMerged:
			merged++
		case m.prs[u].State == "GONE":
			gone++
		case g == github.PRClosed:
			closed++
		case len(lines) < github.MaxPRLines:
			lines = append(lines, m.prLine(u))
		}
	}
	var rest []string
	if more := len(urls) - merged - closed - gone - len(lines); more > 0 {
		rest = append(rest, fmt.Sprintf("+%d more open", more))
	}
	if merged > 0 {
		rest = append(rest, fmt.Sprintf("\033[%sm%d merged\033[90m", prPurple, merged))
	}
	if closed > 0 {
		rest = append(rest, fmt.Sprintf("\033[31m%d closed\033[90m", closed))
	}
	if gone > 0 {
		rest = append(rest, fmt.Sprintf("%d not found", gone))
	}
	if len(rest) > 0 {
		lines = append(lines, "\033[90m"+strings.Join(rest, " · ")+" · ^O all\033[0m")
	}
	if m.prErr != "" && slices.ContainsFunc(urls, func(u string) bool { return m.prs[u].State == "" }) {
		lines = append(lines, "\033[90mcouldn't ask GitHub: "+m.prErr+"\033[0m")
	}
	for i, l := range lines {
		lead := "         "
		if i == 0 {
			lead = "\033[1;33mPRs:\033[0m     "
		}
		lines[i] = ansi.Truncate(lead+l, max(width, 20), "…") + "\033[0m"
	}
	return lines
}

// prLine is one PR: "✓ ccs#113 title · ready to merge", the number a link.
func (m model) prLine(u string) string {
	p := m.prs[u]
	g, why := p.Status()
	if p.State == "" && m.fetchPRs == nil {
		why = "needs gh to look it up"
	}
	glyph, colour := prMark(g)
	if p.Draft || p.State == "" { // grey on GitHub too: nothing is happening to it yet
		colour = "90"
	}
	name := github.PRName(u)
	title := p.Title
	return "\033[" + colour + "m" + glyph + "\033[0m " + hyperlink(u, name) + " " + title + " \033[" + colour + "m· " + why + "\033[0m"
}

// prLinks is conv's PRs for the ^O popup, in the order the header lists them.
// Their text is filled in as the popup draws (prLinkText), so a lookup that
// lands while it's open shows.
func (m model) prLinks(conv transcript.Conversation) []linkItem {
	var out []linkItem
	for _, u := range m.sessionPRs(conv) {
		out = append(out, linkItem{u, ""})
	}
	return out
}

// prLinkText is a PR's row in the ^O popup: its mark and status before the
// title, so a long title is what gets cut.
func (m model) prLinkText(u string) string {
	p := m.prs[u]
	g, why := p.Status()
	glyph, colour := prMark(g)
	if p.Draft || p.State == "" {
		colour = "90"
	}
	return "\033[" + colour + "m" + glyph + "\033[39m " + github.PRName(u) + " \033[" + colour + "m" + why + "\033[39m · " + p.Title
}

// prScope is how far back a session's open PRs are kept up to date; older
// sessions are looked up when selected.
const prScope = 7 * 24 * time.Hour

type prDoneMsg struct {
	infos map[string]github.PRInfo
	err   error
	round bool // from the minute's round, not the selected session's own request
}

// prParallel is how many of a round's requests run at once.
const prParallel = 4

// prCmd asks GitHub about PRs. Landing on a session refreshes its open PRs, in
// a request of their own that never waits behind a round, and any PR not looked
// up yet is asked about the same way. Once a
// minute (prNext, after the last round has finished) a round asks about every
// PR not yet merged or closed of the sessions that are live, active within
// prScope or selected, prBatch at a time, prParallel requests at once, each
// shown as it lands. Merged and closed PRs never change, so they're asked once.
func (m *model) prCmd() tea.Cmd {
	if m.fetchPRs == nil {
		return nil
	}
	var cmds []tea.Cmd
	fetch := func(urls []string, round bool) tea.Cmd {
		f := m.fetchPRs
		return func() tea.Msg {
			infos, err := f(urls)
			return prDoneMsg{infos, err, round}
		}
	}
	if m.prRound == 0 && len(m.prQueue) == 0 && time.Now().After(m.prNext) {
		m.prNext, m.prSelAsked = time.Now().Add(transcript.RefreshInterval), map[string]bool{} // failures are retried once a round
		cutoff := time.Now().Add(-prScope).UTC().Format(time.RFC3339)
		selected := ""
		if len(m.filtered) > 0 {
			selected = m.filtered[m.cursor].conv.SessionID
		}
		var urls []string
		seen := map[string]bool{}
		for _, it := range m.items {
			if !m.live[it.conv.SessionID] && it.conv.LastTimestamp < cutoff && it.conv.SessionID != selected {
				continue
			}
			for _, u := range github.PRsOf(it.conv) {
				if !seen[u] && !m.prs[u].Final() {
					seen[u] = true
					urls = append(urls, u)
				}
			}
		}
		for i := 0; i < len(urls); i += github.PRBatch {
			m.prQueue = append(m.prQueue, urls[i:min(i+github.PRBatch, len(urls))])
		}
	}
	if !m.prSelBusy && len(m.filtered) > 0 && m.previewSettled() { // not for each session scrolled past
		conv := m.filtered[m.cursor].conv
		landed := m.prSelected != conv.SessionID // just selected: its open PRs are refreshed once
		m.prSelected = conv.SessionID
		var urls []string
		for _, u := range github.PRsOf(conv) {
			p, have := m.prs[u]
			if ((landed && !p.Final()) || (!have && !m.prSelAsked[u])) && len(urls) < github.PRBatch {
				urls = append(urls, u)
				m.prSelAsked[u] = true
			}
		}
		if len(urls) > 0 {
			m.prSelBusy = true
			cmds = append(cmds, fetch(urls, false))
		}
	}
	for m.prRound < prParallel && len(m.prQueue) > 0 {
		m.prRound++
		cmds = append(cmds, fetch(m.prQueue[0], true))
		m.prQueue = m.prQueue[1:]
	}
	return tea.Batch(cmds...)
}

// applyPRs takes in what GitHub said.
func (m *model) applyPRs(msg prDoneMsg) {
	if msg.round {
		m.prRound--
	} else {
		m.prSelBusy = false
	}
	m.prErr = ""
	if msg.err != nil {
		m.prErr = transcript.Truncate(strings.TrimSpace(msg.err.Error()), 80)
	}
	for u, p := range msg.infos {
		m.prs[u] = p
	}
}

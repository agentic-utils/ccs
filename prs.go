package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// A session's PRs are the ones Claude Code itself links to it: a "pr-link"
// record each time a gh command in the session touches a PR (create, view,
// checks, merge...), the same record behind Claude Code's own PR badge.

// prURL is a github.com PR address; anything else in a pr-link is ignored,
// since its parts go into a GraphQL query.
var prURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)$`)

// prInfo is what GitHub says about a PR.
type prInfo struct {
	Number     int
	Title      string
	State      string // OPEN, MERGED, CLOSED
	Draft      bool
	MergeState string // mergeStateStatus: CLEAN, UNSTABLE, BLOCKED, BEHIND, DIRTY, DRAFT, HAS_HOOKS, UNKNOWN
	Review     string // reviewDecision: APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED or ""
	Checks     string // the head commit's check rollup: SUCCESS, FAILURE, ERROR, PENDING, EXPECTED or ""
}

func (p prInfo) final() bool { return p.State == "MERGED" || p.State == "CLOSED" }

// PR groups, in the order a session lists them: what you can act on first,
// merged and closed last.
const (
	prReady   = iota // ready to merge
	prFix            // needs you: failing checks, conflicts, changes requested, behind
	prWaiting        // checks running, review required, draft, or not looked up yet
	prMerged
	prClosed
)

// status is p's group and what to say about it. GitHub's mergeStateStatus is
// the verdict on "ready to merge" (branch protection, required checks and
// reviews); the rest only explains why not.
func (p prInfo) status() (int, string) {
	switch {
	case p.State == "MERGED":
		return prMerged, "merged"
	case p.State == "CLOSED":
		return prClosed, "closed"
	case p.State == "GONE":
		return prClosed, "not found: deleted, or gh can't see it"
	case p.State == "":
		return prWaiting, "checking…"
	case p.Draft:
		return prWaiting, "draft"
	case p.MergeState == "CLEAN" || p.MergeState == "HAS_HOOKS":
		return prReady, "ready to merge"
	case p.MergeState == "UNSTABLE":
		return prReady, "ready to merge · optional checks failing"
	case p.MergeState == "DIRTY":
		return prFix, "conflicts"
	case p.Checks == "FAILURE" || p.Checks == "ERROR":
		return prFix, "checks failing"
	case p.Review == "CHANGES_REQUESTED":
		return prFix, "changes requested"
	case p.MergeState == "BEHIND":
		return prFix, "behind its base"
	case p.Checks == "PENDING" || p.Checks == "EXPECTED":
		return prWaiting, "checks running"
	case p.Review == "REVIEW_REQUIRED":
		return prWaiting, "review required"
	case p.MergeState == "UNKNOWN":
		return prWaiting, "github is checking it"
	}
	return prWaiting, "blocked"
}

// GitHub's colours: green ready, amber waiting, red to fix or closed, purple merged.
const prPurple = "38;5;141"

// prMark is a group's glyph and colour.
func prMark(group int) (string, string) {
	switch group {
	case prReady:
		return "✓", "32"
	case prFix:
		return "✗", "31"
	case prWaiting:
		return "○", amber
	case prMerged:
		return "●", prPurple
	case prClosed:
		return "●", "31"
	}
	return " ", "90"
}

// sessionPRs is conv's PRs, ready first, merged and closed last, otherwise
// in the order the session first touched them.
func (m model) sessionPRs(conv Conversation) []string {
	urls := slices.Clone(conv.PRs)
	slices.SortStableFunc(urls, func(a, b string) int {
		ga, _ := m.prs[a].status()
		gb, _ := m.prs[b].status()
		return ga - gb
	})
	return urls
}

// prCounts is how many of conv's open PRs are ready, need fixing, or wait.
func (m model) prCounts(conv Conversation) (ready, fix, waiting int) {
	for _, u := range conv.PRs {
		if m.prs[u].State == "" { // not looked up yet: not known to be open
			continue
		}
		switch g, _ := m.prs[u].status(); g {
		case prReady:
			ready++
		case prFix:
			fix++
		case prWaiting:
			waiting++
		}
	}
	return
}

// prCell is the PRS column: "✓1 ✗1 2" (ready, needing you, waiting), blank
// when nothing is open. Plain for the selected row, else coloured.
func (m model) prCell(conv Conversation, coloured bool) string {
	ready, fix, waiting := m.prCounts(conv)
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
	add(ready, "✓", "32")
	add(fix, "✗", "31")
	add(waiting, "", amber)
	s := strings.Join(parts, " ")
	return strings.Repeat(" ", max(colPRs-ansi.StringWidth(s), 0)) + s
}

// maxPRLines is how many PRs the preview header lists; ^T lists them all.
const maxPRLines = 5

// prLines is the preview header's PRs, width wide: one line, a link, for each
// open PR (at most maxPRLines), then one line counting the merged and closed.
func (m model) prLines(conv Conversation, width int) []string {
	urls := m.sessionPRs(conv)
	if len(urls) == 0 {
		return nil
	}
	var lines []string
	merged, closed, gone := 0, 0, 0
	for _, u := range urls {
		switch g, _ := m.prs[u].status(); {
		case g == prMerged:
			merged++
		case m.prs[u].State == "GONE":
			gone++
		case g == prClosed:
			closed++
		case len(lines) < maxPRLines:
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
		lines = append(lines, "\033[90m"+strings.Join(rest, " · ")+" · ^T all\033[0m")
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
	g, why := p.status()
	if p.State == "" && m.fetchPRs == nil {
		why = "needs gh to look it up"
	}
	glyph, colour := prMark(g)
	if p.Draft || p.State == "" { // grey on GitHub too: nothing is happening to it yet
		colour = "90"
	}
	name := prName(u)
	title := p.Title
	return "\033[" + colour + "m" + glyph + "\033[0m " + hyperlink(u, name) + " " + title + " \033[" + colour + "m· " + why + "\033[0m"
}

// prName is "repo#N" from a PR address.
func prName(u string) string {
	if p := prURL.FindStringSubmatch(u); p != nil {
		return p[2] + "#" + p[3]
	}
	return u
}

// prLinks is conv's PRs for the ^T popup, in the order the header lists them.
func (m model) prLinks(conv Conversation) []linkItem {
	var out []linkItem
	for _, u := range m.sessionPRs(conv) {
		_, why := m.prs[u].status()
		out = append(out, linkItem{u, strings.TrimSpace(prName(u) + " " + m.prs[u].Title + " · " + why)})
	}
	return out
}

// prScope is how far back a session's open PRs are kept up to date; older
// sessions are looked up when selected.
const prScope = 7 * 24 * time.Hour

type prDoneMsg struct {
	infos map[string]prInfo
	err   error
	round bool // from the minute's round, not the selected session's own request
}

// prParallel is how many of a round's requests run at once.
const prParallel = 4

// prCmd asks GitHub about PRs. The selected session's PRs not looked up yet
// go first, in a request of their own that never waits behind a round. Once a
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
		m.prNext, m.prSelAsked = time.Now().Add(refreshInterval), map[string]bool{} // failures are retried once a round
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
			for _, u := range it.conv.PRs {
				if !seen[u] && !m.prs[u].final() {
					seen[u] = true
					urls = append(urls, u)
				}
			}
		}
		for i := 0; i < len(urls); i += prBatch {
			m.prQueue = append(m.prQueue, urls[i:min(i+prBatch, len(urls))])
		}
	}
	if !m.prSelBusy && len(m.filtered) > 0 {
		var urls []string
		for _, u := range m.filtered[m.cursor].conv.PRs {
			if _, have := m.prs[u]; !have && !m.prSelAsked[u] && len(urls) < prBatch {
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
		m.prErr = truncate(strings.TrimSpace(msg.err.Error()), 80)
	}
	for u, p := range msg.infos {
		m.prs[u] = p
	}
}

// ghFetchPRs is fetchPRs when gh is installed: nil otherwise, so PRs are
// never looked up.
func ghFetchPRs() func([]string) (map[string]prInfo, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return nil
	}
	return func(urls []string) (map[string]prInfo, error) { return ghPRBatch(gh, urls) }
}

// prBatch is how many PRs one GraphQL request asks about.
const prBatch = 40

// ghPRBatch asks about urls in one GraphQL request (each PR an aliased
// field). A PR that's gone or private comes back null with an error for that
// field only, and gh exits non-zero, so the data is read whatever the exit.
func ghPRBatch(gh string, urls []string) (map[string]prInfo, error) {
	var q strings.Builder
	q.WriteString("query{")
	for i, u := range urls {
		p := prURL.FindStringSubmatch(u) // validated when parsed; owner and repo are [A-Za-z0-9_.-]
		fmt.Fprintf(&q, `p%d:repository(owner:%q,name:%q){pullRequest(number:%s){number title state isDraft mergeStateStatus reviewDecision commits(last:1){nodes{commit{statusCheckRollup{state}}}}}}`, i, p[1], p[2], p[3])
	}
	q.WriteString("}")
	raw, runErr := runBounded(30*time.Second, []string{"GH_PROMPT_DISABLED=1"}, gh, "api", "graphql", "-f", "query="+q.String())
	var resp struct {
		Data map[string]*struct {
			PullRequest *struct {
				Number           int
				Title            string
				State            string
				IsDraft          bool
				MergeStateStatus string
				ReviewDecision   string
				Commits          struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct{ State string }
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, err
	}
	out := map[string]prInfo{}
	for i, u := range urls {
		r, answered := resp.Data[fmt.Sprintf("p%d", i)]
		if !answered {
			continue
		}
		if r == nil || r.PullRequest == nil { // deleted, or gh's account can't see it
			out[u] = prInfo{State: "GONE"}
			continue
		}
		pr := r.PullRequest
		info := prInfo{Number: pr.Number, Title: pr.Title, State: pr.State, Draft: pr.IsDraft, MergeState: pr.MergeStateStatus, Review: pr.ReviewDecision}
		if n := pr.Commits.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
			info.Checks = n[0].Commit.StatusCheckRollup.State
		}
		out[u] = info
	}
	if len(out) == 0 && runErr != nil {
		return nil, runErr
	}
	return out, nil
}

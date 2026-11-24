package github

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// A switch (sessions.SwitchSession) lists the old session's PRs in the new
// one's brief and claims its open ones for the new session through these.
func init() {
	sessions.SwitchPRs = SwitchPRs
	sessions.ClaimUnowned = ClaimUnowned
}

// switchFetch is FetchPRs, stubbed in tests.
var switchFetch = FetchPRs

// SwitchPRs is conv's PRs for a switch brief: one line for each open one
// with its status, then one counting the merged and closed; and the open
// ones' addresses. Without gh, or when the lookup fails, each is listed
// with its status unknown and counted as open, so none is left unowned.
func SwitchPRs(conv transcript.Conversation) (lines, open []string) {
	urls := PRsOf(conv)
	if len(urls) == 0 {
		return nil, nil
	}
	var info map[string]PRInfo
	if fetch := switchFetch(); fetch != nil {
		var err error
		if info, err = fetch(urls[:min(len(urls), PRBatch)]); err != nil {
			state.LogActivity("switch: couldn't look up %d PR(s): %v", len(urls), err)
		}
	}
	var ended []string
	for _, u := range urls {
		p, ok := info[u]
		if !ok {
			lines, open = append(lines, u+" (status unknown)"), append(open, u)
			continue
		}
		if g, _ := p.Status(); g == PRMerged || g == PRClosed {
			ended = append(ended, u)
			continue
		}
		_, status := p.Status()
		title := ""
		if p.Title != "" {
			title = " " + transcript.CleanText(p.Title) + " ·"
		}
		lines, open = append(lines, fmt.Sprintf("%s%s %s", u, title, status)), append(open, u)
	}
	if len(ended) > 0 {
		lines = append(lines, fmt.Sprintf("%d merged or closed: %s", len(ended), strings.Join(ended, ", ")))
	}
	return lines, open
}

// ClaimUnowned claims each of prs that no running session owns for the
// running session called to, as claim_pr does, returning those claimed. A PR
// another live session owns is left to it.
func ClaimUnowned(prs []string, to string) []string {
	if len(prs) == 0 {
		return nil
	}
	live := LiveByPID()
	claims := LoadClaims().Claims
	now := time.Now()
	var out []string
	for _, pr := range prs {
		if slices.ContainsFunc(claims[pr], func(c PRClaim) bool { return claimLive(c, live) }) {
			continue
		}
		if _, err := ClaimAs(pr, to, false, now); err != nil {
			state.LogActivity("switch: couldn't claim %s for %s: %v", pr, to, err)
			continue
		}
		out = append(out, pr)
	}
	return out
}

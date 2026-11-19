package github

import (
	"slices"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
)

// A handover close (sessions.CloseHandover) moves the closed session's PR
// claims to its parent through this.
func init() { sessions.MoveClaims = MoveClaims }

// MoveClaims gives every PR the session with id from claimed to the running
// session called to, as `ccs claim` run there would, and drops from's claims.
// Returns the PRs moved.
func MoveClaims(from, to string) ([]string, error) {
	if _, err := namedSession(to); err != nil {
		return nil, err
	}
	var prs []string
	for pr, cs := range LoadClaims().Claims {
		if slices.ContainsFunc(cs, func(c PRClaim) bool { return c.Session == from || c.Thread == from }) {
			prs = append(prs, pr)
		}
	}
	slices.Sort(prs)
	now := time.Now()
	var moved []string
	for _, pr := range prs {
		if _, err := ClaimAs(pr, to, false, now); err != nil {
			state.LogActivity("handover: couldn't claim %s for %s: %v", pr, to, err)
			continue
		}
		moved = append(moved, pr)
	}
	err := WithClaims(func(c *ClaimsFile) bool {
		changed := false
		for _, pr := range moved {
			kept := slices.DeleteFunc(slices.Clone(c.Claims[pr]), func(x PRClaim) bool { return x.Session == from || x.Thread == from })
			if len(kept) != len(c.Claims[pr]) {
				c.Claims[pr], changed = kept, true
			}
		}
		return changed
	})
	return moved, err
}

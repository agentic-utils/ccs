package github

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// A switch brief lists the open PRs with their status and counts the ended
// ones; a PR the lookup didn't answer counts as open.
func TestSwitchPRsListsOpenOnesWithStatus(t *testing.T) {
	old := switchFetch
	t.Cleanup(func() { switchFetch = old })
	const open, merged, unknown = "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2", "https://github.com/o/r/pull/3"
	switchFetch = func() func([]string) (map[string]PRInfo, error) {
		return func([]string) (map[string]PRInfo, error) {
			return map[string]PRInfo{
				open:   {State: "OPEN", Title: "Fix it", MergeState: "BLOCKED", Checks: "PENDING"},
				merged: {State: "MERGED"},
			}, nil
		}
	}
	if sessions.SwitchPRs == nil || sessions.ClaimUnowned == nil {
		t.Fatal("the github package sets the switch hooks")
	}
	lines, opens := SwitchPRs(transcript.Conversation{PRs: []string{open, merged, unknown}})
	text := strings.Join(lines, "\n")
	for _, want := range []string{open + " Fix it · checks running", unknown + " (status unknown)", "1 merged or closed: " + merged} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q:\n%s", want, text)
		}
	}
	if !slices.Equal(opens, []string{open, unknown}) {
		t.Errorf("open: %v", opens)
	}
}

// ClaimUnowned leaves a PR a running session owns and claims the rest.
func TestClaimUnownedLeavesOwnedPRs(t *testing.T) {
	claimEnv(t)
	old := LiveNamed
	t.Cleanup(func() { LiveNamed = old })
	LiveNamed = func(name string) []PRClaim {
		return []PRClaim{{Session: "s-new", Name: name, PID: 4243, ProcStart: "Tue Nov  3 09:00:00 2026"}}
	}
	WithClaims(func(c *ClaimsFile) bool {
		c.Claims[claimA] = []PRClaim{{Session: "s-alpha", Name: "alpha", PID: os.Getppid(), ProcStart: "Tue Nov  3 07:00:00 2026"}}
		return true
	})
	got := ClaimUnowned([]string{claimA, claimB}, "worker")
	if !slices.Equal(got, []string{claimB}) {
		t.Errorf("claimed %v", got)
	}
	if c := LoadClaims().Claims; len(c[claimA]) != 1 || len(c[claimB]) != 1 || c[claimB][0].Session != "s-new" {
		t.Errorf("claims: %+v", c)
	}
}

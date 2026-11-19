package github

import (
	"slices"
	"testing"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// A handover moves the closed session's claims to its parent, as a claim run
// there would record them, and drops the closed session's; others' claims
// stay.
func TestMoveClaimsToTheParent(t *testing.T) {
	claimEnv(t)
	old := LiveNamed
	t.Cleanup(func() { LiveNamed = old })
	LiveNamed = func(name string) []PRClaim {
		if name == "lead" {
			return []PRClaim{{Session: "s-lead", Name: "lead", PID: 4242, ProcStart: "Tue Nov  3 08:00:00 2026"}}
		}
		return nil
	}
	if sessions.MoveClaims == nil {
		t.Fatal("the github package sets sessions.MoveClaims")
	}
	WithClaims(func(c *ClaimsFile) bool {
		c.Claims[claimA] = []PRClaim{{Session: "s-worker", Name: "worker"}, {Session: "s-other", Name: "other"}}
		c.Claims[claimB] = []PRClaim{{Session: "s-worker", Name: "worker"}}
		c.Claims[claimC] = []PRClaim{{Session: "s-other", Name: "other"}}
		return true
	})
	moved, err := sessions.MoveClaims("s-worker", "lead")
	if err != nil || !slices.Equal(moved, []string{claimA, claimB}) {
		t.Fatalf("moved %v: %v", moved, err)
	}
	c := LoadClaims().Claims
	names := func(pr string) []string {
		var out []string
		for _, x := range c[pr] {
			out = append(out, x.Name)
		}
		return out
	}
	if got := names(claimA); !slices.Equal(got, []string{"other", "lead"}) {
		t.Errorf("A: worker's claim goes to lead, other's stays: %v", got)
	}
	if got := names(claimB); !slices.Equal(got, []string{"lead"}) || c[claimB][0].Session != "s-lead" || c[claimB][0].PID != 4242 {
		t.Errorf("B: lead owns it now: %+v", c[claimB])
	}
	if got := names(claimC); !slices.Equal(got, []string{"other"}) {
		t.Errorf("C isn't touched: %v", got)
	}
	if _, err := MoveClaims("s-worker", "nobody"); err == nil {
		t.Error("a parent that isn't running is refused")
	}
}

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
)

const (
	prReadyURL  = "https://github.com/o/r/pull/1"
	prFixURL    = "https://github.com/o/r/pull/2"
	prDraftURL  = "https://github.com/o/r/pull/3"
	prMergedURL = "https://github.com/o/r/pull/4"
	prNewURL    = "https://github.com/o/r/pull/5"
)

// prTabModel: nothing needs you (every live session busy, nothing unread),
// and the sessions' PRs: "menu" one ready to merge, "asks" one with checks
// failing, "live" a draft, "idle" (exited) one merged and one not looked up
// yet. The selection is on the last row.
func prTabModel(t *testing.T) model {
	t.Helper()
	m := waitModel(t)
	m.width, m.height = 160, 40
	m.marks = &sessionMarks{Since: time.Now().Add(time.Hour)}
	for _, id := range []string{"menu", "asks", "live"} {
		m.setStat(id, sessions.SessionStat{Status: "busy", Since: time.Now()})
	}
	m.prs = map[string]github.PRInfo{
		prReadyURL:  {State: "OPEN", MergeState: "CLEAN"},
		prFixURL:    {State: "OPEN", MergeState: "BLOCKED", Checks: "FAILURE"},
		prDraftURL:  {State: "OPEN", Draft: true, MergeState: "CLEAN"},
		prMergedURL: {State: "MERGED"},
	}
	setPRs(&m, map[string][]string{
		"menu": {prReadyURL},
		"asks": {prFixURL},
		"live": {prDraftURL},
		"idle": {prMergedURL, prNewURL},
	})
	m.sortList()
	m.cursor = len(m.filtered) - 1
	if _, tier, ok := m.nextWaiting(); !ok || tier < attentionReady {
		t.Fatalf("only the PR tiers have sessions, got tier %d", tier)
	}
	return m
}

func setPRs(m *model, prs map[string][]string) {
	for i := range m.items {
		m.items[i].conv.PRs = prs[m.items[i].conv.SessionID]
	}
	m.updateFilter()
}

// With nothing needing you, Tab goes to the session whose PR is ready to
// merge, staying on it while it's the only one; once that merges, on through
// the sessions with other open PRs in list order, wrapping. Merged PRs, ones
// not looked up yet, and exited sessions' circles don't matter: only open
// PRs do.
func TestTabCyclesReadyThenOpenPRs(t *testing.T) {
	m := prTabModel(t)
	if head := topRow(m); !strings.Contains(head, "tab PRs") || strings.Contains(head, "needs you") {
		t.Errorf("only PRs left: the header says tab PRs: %q", head)
	}
	for i := range 2 {
		if m = tab(m); selected(m) != "menu" || m.noteMsg != "" {
			t.Fatalf("Tab %d: on %s (note %q), want menu, ready to merge", i+1, selected(m), m.noteMsg)
		}
	}
	m.prs[prReadyURL] = github.PRInfo{State: "MERGED"}
	var got []string
	for range 3 {
		m = tab(m)
		got = append(got, selected(m))
	}
	if !(got[0] == "asks" && got[1] == "live" || got[0] == "live" && got[1] == "asks") || got[2] != got[0] {
		t.Fatalf("open PRs: Tab goes through asks and live and wraps, got %v", got)
	}
	// An exited session with an open PR is a stop too.
	m.prs[prNewURL] = github.PRInfo{State: "OPEN", MergeState: "CLEAN"}
	if m = tab(m); selected(m) != "idle" {
		t.Fatalf("exited idle has a ready PR now: on %s", selected(m))
	}
	// Every PR merged or closed: nowhere to go.
	for _, u := range []string{prFixURL, prDraftURL, prNewURL} {
		m.prs[u] = github.PRInfo{State: "CLOSED"}
	}
	before := selected(m)
	if m = tab(m); selected(m) != before || m.noteMsg != "nothing needs you" {
		t.Fatalf("no open PRs: on %s, note %q", selected(m), m.noteMsg)
	}
}

// The earlier tiers always win over the PR ones, re-checked every press.
func TestTabEarlierTiersBeatPRs(t *testing.T) {
	m := prTabModel(t)
	m.setStat("asks", waiting(time.Now(), "input needed"))
	if head := topRow(m); !strings.Contains(head, "tab needs you") {
		t.Errorf("a menu waits: the header says tab needs you: %q", head)
	}
	for i := range 2 {
		if m = tab(m); selected(m) != "asks" {
			t.Fatalf("Tab %d: on %s, want asks, waiting on a menu", i+1, selected(m))
		}
	}
	m.setStat("asks", sessions.SessionStat{Status: "busy", Since: time.Now()})
	if m = tab(m); selected(m) != "menu" {
		t.Fatalf("the menu answered: on %s, want menu, ready to merge", selected(m))
	}
}

// A snoozed session is never a stop, PRs or not.
func TestTabPRsSkipPutAway(t *testing.T) {
	m := prTabModel(t)
	m.marks.Snoozed = map[string]time.Time{"menu": time.Now().Add(time.Hour)}
	if m = tab(m); selected(m) == "menu" || m.attention(m.filtered[m.cursor].conv) != attentionOpenPR {
		t.Fatalf("menu is snoozed: on %s, want one with an open PR", selected(m))
	}
}

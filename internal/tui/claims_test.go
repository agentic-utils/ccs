package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestPRsCLIAndMCP(t *testing.T) {
	claimEnv(t)
	writeWatchState(t, time.Now(), &github.WatchedPR{URL: claimA, Repo: "o/r", Number: 1, Title: "t", State: "open", Checks: "success", Review: "approved"})
	var out, errs strings.Builder
	if code := github.RunPRs([]string{"--json", "--owner", "o"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var rep github.PRsReport
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil || len(rep.PRs) != 1 || !rep.PRs[0].Unowned {
		t.Errorf("--json: %v %s", err, out.String())
	}
	out.Reset()
	if code := github.RunClaim([]string{claimA}, &out, &errs); code != 0 || !strings.Contains(out.String(), "alpha owns") {
		t.Errorf("ccs claim: %d %q %q", code, out.String(), errs.String())
	}
	out.Reset()
	if code := github.RunClaim([]string{claimA, "--release"}, &out, &errs); code != 0 || !strings.Contains(out.String(), "released") {
		t.Errorf("ccs claim <url> --release: %d %q", code, out.String())
	}
	if code := github.RunClaim(nil, &out, &errs); code != 2 {
		t.Errorf("no URL: exit %d", code)
	}
	if res, err := mcp.Call("list_prs", nil, nil); err != nil || !strings.Contains(fmt.Sprint(res), claimA) {
		t.Errorf("list_prs defaults to the running watcher's author and owner: %v %v", res, err)
	}
	os.Remove(github.PRWatchStatePath())
	if _, err := mcp.Call("list_prs", nil, nil); err == nil {
		t.Error("no watcher state and no gh user: list_prs should fail")
	}
	out.Reset()
	if code := github.RunPRs([]string{"--statusline"}, &out, &errs); code != 0 || out.Len() != 0 {
		t.Errorf("--statusline with nothing saved prints nothing and asks nothing: %d %q", code, out.String())
	}
}

// Copies of internal/github test helpers, for the tests here that claim PRs.

const (
	claimA = "https://github.com/o/r/pull/1"
	claimB = "https://github.com/o/r/pull/2"
	claimC = "https://github.com/o/r/pull/3"
)

// claimEnv points the claims and watcher state at a temp folder and stubs
// the process tree: the test's parent pid runs under session "alpha".
func claimEnv(t *testing.T) (live map[int]github.PRClaim) {
	t.Helper()
	oldSettings, oldLive, oldParent, oldFetch, oldEnd := state.SettingsPath, github.LiveByPID, github.ParentPID, github.FetchOpenPRs, github.EndState
	state.SettingsPath = t.TempDir() + "/settings.json"
	live = map[int]github.PRClaim{
		os.Getppid(): {Session: "s-alpha", Name: "alpha", PID: os.Getppid(), ProcStart: "Tue Nov  3 07:00:00 2026"},
	}
	github.LiveByPID = func() map[int]github.PRClaim { return live }
	github.ParentPID = func(int) int { return 0 }
	github.FetchOpenPRs = func(string, string) (map[string]*github.WatchedPR, error) {
		return nil, errors.New("no network in tests")
	}
	github.EndState = func(string) (string, error) { return "", errors.New("no network in tests") }
	github.ResetClaimedPRs() // a session's PRs read the claims: not another test's
	t.Cleanup(func() {
		state.SettingsPath, github.LiveByPID, github.ParentPID, github.FetchOpenPRs, github.EndState = oldSettings, oldLive, oldParent, oldFetch, oldEnd
		github.ResetClaimedPRs()
	})
	return live
}

// writeWatchState saves a watcher state for o's PRs by the author me.
func writeWatchState(t *testing.T, updated time.Time, prs ...*github.WatchedPR) {
	t.Helper()
	st := github.PRWatchState{Author: "me", Owner: "o", Started: updated, Updated: updated, PRs: map[string]*github.WatchedPR{}}
	for _, p := range prs {
		st.PRs[p.URL] = p
	}
	if err := github.SavePRWatchState(st); err != nil {
		t.Fatal(err)
	}
}

// A PR claimed for a session, which its transcript never linked (opened
// before a /clear), is in its PRS column, its preview's PR lines and the
// PR lookups; claims of another session with the same name aren't.
func TestClaimedPRsShowInTheList(t *testing.T) {
	claimEnv(t) // the calling session is s-alpha
	if code := github.RunClaim([]string{claimA}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("ccs claim: exit %d", code)
	}
	github.WithClaims(func(c *github.ClaimsFile) bool {
		c.Claims[claimB] = []github.PRClaim{{Session: "s-other", Name: "alpha"}}
		return true
	})
	github.ResetClaimedPRs()
	conv := transcript.Conversation{SessionID: "s-alpha", Title: "alpha", FilePath: t.TempDir() + "/s-alpha.jsonl"}
	m := model{prs: map[string]github.PRInfo{
		claimA: {State: "OPEN", MergeState: "BLOCKED", Review: "REVIEW_REQUIRED", Title: "claimed"},
		claimB: {State: "OPEN", MergeState: "CLEAN", Title: "someone else's"},
	}, width: 100}
	if got := strings.TrimSpace(m.prCell(conv, false)); got != "◷1" {
		t.Errorf("PRS column counts the claimed PR only: %q", got)
	}
	lines := m.prLines(conv, 100, github.MaxPRLines, true)
	if len(lines) != 1 || !strings.Contains(strip2(lines[0]), "claimed · review required") {
		t.Errorf("preview's PR lines: %q", lines)
	}
	if got := m.attention(conv); got != attentionOpenPR {
		t.Errorf("Tab's PR tier counts the claimed PR: %d", got)
	}
}

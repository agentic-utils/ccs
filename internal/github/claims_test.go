package github

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

const (
	claimA = "https://github.com/o/r/pull/1"
	claimB = "https://github.com/o/r/pull/2"
	claimC = "https://github.com/o/r/pull/3"
)

// claimEnv points the claims and watcher state at a temp folder and stubs
// the process tree: the test's parent pid runs under session "alpha".
func claimEnv(t *testing.T) (live map[int]PrClaim) {
	t.Helper()
	oldSettings, oldLive, oldParent, oldFetch, oldEnd := state.SettingsPath, LiveByPID, ParentPID, FetchOpenPRs, EndState
	state.SettingsPath = t.TempDir() + "/settings.json"
	live = map[int]PrClaim{
		os.Getppid(): {Session: "s-alpha", Name: "alpha", PID: os.Getppid(), ProcStart: "Tue Nov  3 07:00:00 2026"},
	}
	LiveByPID = func() map[int]PrClaim { return live }
	ParentPID = func(int) int { return 0 }
	FetchOpenPRs = func(string, string) (map[string]*WatchedPR, error) {
		return nil, errors.New("no network in tests")
	}
	EndState = func(string) (string, error) { return "", errors.New("no network in tests") }
	t.Cleanup(func() {
		state.SettingsPath, LiveByPID, ParentPID, FetchOpenPRs, EndState = oldSettings, oldLive, oldParent, oldFetch, oldEnd
	})
	return live
}

// asSession makes the claiming process run under the session at pid.
func asSession(pid int) {
	ParentPID = func(p int) int {
		if p == os.Getppid() {
			return pid
		}
		return 0
	}
}

func TestClaimRecordsTheCallingSession(t *testing.T) {
	claimEnv(t)
	msg, err := Claim(claimA, false, time.Now())
	if err != nil || msg != "alpha owns "+claimA {
		t.Fatalf("claim: %q %v", msg, err)
	}
	c := LoadClaims().Claims[claimA]
	if len(c) != 1 || c[0].Session != "s-alpha" || c[0].Name != "alpha" || c[0].PID != os.Getppid() || c[0].ProcStart == "" {
		t.Errorf("recorded %+v", c)
	}
	Claim(claimA, false, time.Now()) // again: still one claim
	if n := len(LoadClaims().Claims[claimA]); n != 1 {
		t.Errorf("a session claiming twice has one claim, got %d", n)
	}
	if msg, _ := Claim(claimA, true, time.Now()); msg != "alpha released "+claimA || len(LoadClaims().Claims) != 0 {
		t.Errorf("release: %q, left %v", msg, LoadClaims().Claims)
	}
}

func TestClaimRefusals(t *testing.T) {
	live := claimEnv(t)
	if _, err := Claim("https://gitlab.com/o/r/pull/1", false, time.Now()); err == nil {
		t.Error("a non-GitHub address is refused")
	}
	delete(live, os.Getppid())
	if _, err := Claim(claimA, false, time.Now()); err == nil || !strings.Contains(err.Error(), "not run from a Claude Code or Codex session") {
		t.Errorf("outside a session: %v", err)
	}
	if _, err := os.Stat(ClaimsPath()); err == nil {
		t.Error("nothing is written when refused")
	}
}

func TestClaimFindsAnAncestorSession(t *testing.T) {
	live := claimEnv(t)
	delete(live, os.Getppid()) // the shell between ccs and claude
	live[4242] = PrClaim{Session: "s-beta", Name: "beta", PID: 4242, ProcStart: "x"}
	asSession(4242)
	if msg, err := Claim(claimA, false, time.Now()); err != nil || !strings.HasPrefix(msg, "beta owns") {
		t.Errorf("the nearest live claude ancestor owns it: %q %v", msg, err)
	}
}

func TestSecondClaimIsKeptAndConcurrentClaimsAllLand(t *testing.T) {
	live := claimEnv(t)
	Claim(claimA, false, time.Now())
	live[4242] = PrClaim{Session: "s-beta", Name: "beta", PID: 4242, ProcStart: "x"}
	delete(live, os.Getppid())
	asSession(4242)
	msg, _ := Claim(claimA, false, time.Now())
	if !strings.Contains(msg, "also claimed by 1 other") {
		t.Errorf("the second owner is told: %q", msg)
	}
	if c := LoadClaims().Claims[claimA]; len(c) != 2 || c[0].Name != "alpha" || c[1].Name != "beta" {
		t.Errorf("both claims kept, in order: %+v", c)
	}

	// Many claims at once through the lock: none lost.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			WithClaims(func(c *ClaimsFile) bool {
				u := "https://github.com/o/r/pull/" + string(rune('a'+i))
				c.Claims[u] = append(c.Claims[u], PrClaim{Session: "s"})
				return true
			})
		}()
	}
	wg.Wait()
	if n := len(LoadClaims().Claims); n != 21 {
		t.Errorf("concurrent claims: %d PRs, want 21", n)
	}
}

// writeWatchState saves a watcher state for o's PRs by the author me.
func writeWatchState(t *testing.T, updated time.Time, prs ...*WatchedPR) {
	t.Helper()
	st := PrWatchState{Author: "me", Owner: "o", Started: updated, Updated: updated, PRs: map[string]*WatchedPR{}}
	for _, p := range prs {
		st.PRs[p.URL] = p
	}
	if err := SavePRWatchState(st); err != nil {
		t.Fatal(err)
	}
}

func TestPRsListsOwnersAndFlags(t *testing.T) {
	live := claimEnv(t)
	now := time.Now()
	writeWatchState(t, now.Add(-time.Minute),
		&WatchedPR{URL: claimA, Repo: "o/r", Number: 1, Title: "ready one", State: "open", Checks: "success", Review: "approved"},
		&WatchedPR{URL: claimB, Repo: "o/r", Number: 2, Title: "optional fail", State: "open", Checks: "failing", Failing: []string{"setup"}, Review: "approved"},
		&WatchedPR{URL: claimC, Repo: "o/r", Number: 3, Title: "nobody's", State: "open", Checks: "pending", Review: "required"},
		&WatchedPR{URL: "https://github.com/o/r/pull/9", Repo: "o/r", Number: 9, State: "merged"},
	)
	Claim(claimA, false, now)
	Claim(claimB, false, now)
	Claim("https://github.com/o/r/pull/9", false, now) // merged: dropped on read
	live[4242] = PrClaim{Session: "s-beta", Name: "beta", PID: 4242, ProcStart: "x"}
	delete(live, os.Getppid())
	asSession(4242)
	Claim(claimB, false, now) // contested
	live[os.Getppid()] = PrClaim{Session: "s-alpha", Name: "alpha", PID: os.Getppid(), ProcStart: "Tue Nov  3 07:00:00 2026"}

	rep, err := PrsList("me", "o", false, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.PRs) != 3 {
		t.Fatalf("open PRs only: %+v", rep.PRs)
	}
	got := map[string]PrRow{}
	for _, r := range rep.PRs {
		got[r.PR] = r
	}
	if r := got[claimB]; r.Group != "needs_you" || r.Status != "checks failing: setup" || !r.Contested || len(r.Owners) != 2 {
		t.Errorf("a failed optional check needs you, claimed twice: %+v", r)
	}
	if r := got[claimA]; r.Group != "ready" || r.Unowned || r.Contested || r.Owners[0] != "alpha" {
		t.Errorf("owned and ready: %+v", r)
	}
	if r := got[claimC]; !r.Unowned || r.Group != "waiting" {
		t.Errorf("unowned: %+v", r)
	}
	if rep.PRs[0].PR != claimB {
		t.Errorf("needing you first: %v", rep.PRs[0].PR)
	}
	if _, ok := LoadClaims().Claims["https://github.com/o/r/pull/9"]; ok {
		t.Error("a merged PR's claim is dropped")
	}
	if s := rep.StatusLine(); s != "PRs 1 ready · 1 need you · 1 unowned · 1 contested" {
		t.Errorf("statusline %q", s)
	}
	if txt := rep.Text(); !strings.Contains(txt, "UNOWNED") || !strings.Contains(txt, "CONTESTED: alpha, beta") {
		t.Errorf("text flags them:\n%s", txt)
	}

	// The beta session stops: its claim goes, so claimB is no longer contested.
	delete(live, 4242)
	rep, _ = PrsList("me", "o", true, now)
	for _, r := range rep.PRs {
		if r.PR == claimB && (r.Contested || len(r.Owners) != 1) {
			t.Errorf("a stopped session's claim is dropped: %+v", r)
		}
	}
	if c := LoadClaims().Claims[claimB]; len(c) != 1 || c[0].Name != "alpha" {
		t.Errorf("pruned on disk: %+v", c)
	}
}

func TestClaimOfARecycledPIDIsDropped(t *testing.T) {
	live := claimEnv(t)
	Claim(claimA, false, time.Now())
	live[os.Getppid()] = PrClaim{Session: "s-alpha", Name: "alpha", PID: os.Getppid(), ProcStart: "another start"}
	writeWatchState(t, time.Now(), &WatchedPR{URL: claimA, Repo: "o/r", Number: 1, State: "open"})
	rep, _ := PrsList("me", "o", true, time.Now())
	if len(rep.PRs) != 1 || !rep.PRs[0].Unowned {
		t.Errorf("same pid, different process: not the owner: %+v", rep.PRs)
	}
}

func TestClaimFollowsARename(t *testing.T) {
	live := claimEnv(t)
	Claim(claimA, false, time.Now())
	live[os.Getppid()] = PrClaim{Session: "s-alpha", Name: "alpha-renamed", PID: os.Getppid(), ProcStart: "Tue Nov  3 07:00:00 2026"}
	writeWatchState(t, time.Now(), &WatchedPR{URL: claimA, Repo: "o/r", Number: 1, State: "open"})
	rep, _ := PrsList("me", "o", true, time.Now())
	if len(rep.PRs) != 1 || !slices.Equal(rep.PRs[0].Owners, []string{"alpha-renamed"}) {
		t.Errorf("a /rename after the claim: still its owner, under its new name: %+v", rep.PRs)
	}
	if c := LoadClaims().Claims[claimA]; len(c) != 1 || c[0].Name != "alpha-renamed" {
		t.Errorf("the stored claim takes the new name: %+v", c)
	}
}

func TestPRsFreshnessAndStatusline(t *testing.T) {
	claimEnv(t)
	now := time.Now()
	asked := 0
	FetchOpenPRs = func(string, string) (map[string]*WatchedPR, error) {
		asked++
		return map[string]*WatchedPR{claimA: {URL: claimA, Repo: "o/r", Number: 1, State: "open", Checks: "success", Review: "approved"}}, nil
	}
	// No watcher state: statusline prints nothing and asks nobody.
	if rep, _ := PrsList("me", "o", true, now); rep.StatusLine() != "" || asked != 0 {
		t.Errorf("offline with no state: %q, asked %d", rep.StatusLine(), asked)
	}
	// Stale state: ccs prs asks GitHub; the statusline uses it and says so.
	writeWatchState(t, now.Add(-time.Hour), &WatchedPR{URL: claimB, Repo: "o/r", Number: 2, State: "open", Checks: "success", Review: "approved"})
	rep, _ := PrsList("me", "o", false, now)
	if asked != 1 || len(rep.PRs) != 1 || rep.PRs[0].PR != claimA {
		t.Errorf("stale state: asked %d, %+v", asked, rep.PRs)
	}
	rep, _ = PrsList("me", "o", true, now)
	if s := rep.StatusLine(); s != "PRs 1 ready · 1 unowned (stale)" || asked != 1 {
		t.Errorf("statusline from stale state: %q (asked %d)", s, asked)
	}
	// Fresh state: no GitHub call.
	writeWatchState(t, now.Add(-time.Minute))
	if _, err := PrsList("me", "o", false, now); err != nil || asked != 1 {
		t.Errorf("fresh state is used: asked %d, %v", asked, err)
	}
}

func TestPRsForOneSession(t *testing.T) {
	live := claimEnv(t)
	now := time.Now()
	writeWatchState(t, now,
		&WatchedPR{URL: claimA, Repo: "o/r", Number: 1, Title: "a", State: "open", Checks: "success", Review: "approved"},
		&WatchedPR{URL: claimB, Repo: "o/r", Number: 2, Title: "b", State: "open", Checks: "failing", Failing: []string{"x"}},
		&WatchedPR{URL: claimC, Repo: "o/r", Number: 3, Title: "c", State: "open", Checks: "pending"},
	)
	Claim(claimA, false, now)
	Claim(claimB, false, now)
	alpha := live[os.Getppid()]
	live[4242] = PrClaim{Session: "s-beta", Name: "beta", PID: 4242, ProcStart: "x"}
	delete(live, os.Getppid())
	asSession(4242)
	Claim(claimC, false, now)
	live[os.Getppid()] = alpha

	var out, errs strings.Builder
	if code := RunPRs([]string{"--json", "--claude-session-id", "s-alpha"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var rep PrsReport
	json.Unmarshal([]byte(out.String()), &rep)
	if len(rep.PRs) != 2 || rep.PRs[0].PR != claimB || rep.PRs[1].PR != claimA {
		t.Errorf("only alpha's PRs, needing you first: %s", out.String())
	}
	if d := rep.PRs[0].OwnersDetail; len(d) != 1 || d[0] != (PrOwner{Name: "alpha", Kind: "claude", Session: "s-alpha"}) {
		t.Errorf("owners_detail: %+v", d)
	}
	out.Reset()
	if code := RunPRs([]string{"--statusline", "--claude-session-id", "s-alpha"}, &out, &errs); code != 0 || out.String() != "PRs ✓1 ✗1\n" {
		t.Errorf("statusline for one session: %d %q", code, out.String())
	}
	out.Reset()
	if RunPRs([]string{"--statusline", "--claude-session-id", "nobody"}, &out, &errs); out.Len() != 0 {
		t.Errorf("a session with no claims prints nothing: %q", out.String())
	}

	// A Codex claim is found by its thread id.
	cx := PrOwner{Name: "cx", Kind: "codex", Session: "th-1"}
	if o := OwnerOf(PrClaim{Session: "th-1", Kind: "codex", Thread: "th-1", Name: "cx"}, live); o != cx {
		t.Errorf("codex owner: %+v", o)
	}
	r := PrsReport{PRs: []PrRow{{PR: claimC, OwnersDetail: []PrOwner{cx}}}}
	if got := r.OwnedBy("codex", "th-1"); len(got.PRs) != 1 {
		t.Errorf("by codex thread: %+v", got)
	}
	if got := r.OwnedBy("claude", "th-1"); len(got.PRs) != 0 {
		t.Errorf("--claude-session-id never matches a Codex claim: %+v", got)
	}
	out.Reset()
	if RunPRs([]string{"--json", "--codex-session-id", "s-alpha"}, &out, &errs); strings.Contains(out.String(), claimA) {
		t.Errorf("--codex-session-id never matches a Claude claim: %s", out.String())
	}
	if code := RunPRs([]string{"--session-id", "s-alpha"}, &out, &errs); code != 2 {
		t.Errorf("no bare --session-id: exit %d", code)
	}
}

func TestPrsListAsksAboutUnknownClaimsAtMostEveryInterval(t *testing.T) {
	claimEnv(t)
	now := time.Now()
	if _, err := Claim(claimA, false, now); err != nil {
		t.Fatal(err)
	}
	FetchOpenPRs = func(string, string) (map[string]*WatchedPR, error) { return map[string]*WatchedPR{}, nil }
	asked := 0
	EndState = func(string) (string, error) { asked++; return "open", nil }
	list := func(at time.Time) {
		t.Helper()
		if _, err := PrsList("a", "o", false, at); err != nil {
			t.Fatal(err)
		}
	}
	list(now)
	list(now.Add(time.Minute)) // another session's status line, a minute on
	if asked != 1 {
		t.Errorf("asked %d times within the interval, want 1", asked)
	}
	list(now.Add(EndCheckEvery))
	if asked != 2 {
		t.Errorf("asked %d times once the interval passed, want 2", asked)
	}
}

func TestPrsListWaitsAfterAFailedEndCheck(t *testing.T) {
	claimEnv(t)
	now := time.Now()
	if _, err := Claim(claimA, false, now); err != nil {
		t.Fatal(err)
	}
	FetchOpenPRs = func(string, string) (map[string]*WatchedPR, error) { return map[string]*WatchedPR{}, nil }
	asked := 0
	EndState = func(string) (string, error) { asked++; return "", errors.New("rate limited") }
	for _, at := range []time.Time{now, now.Add(time.Minute)} {
		if _, err := PrsList("a", "o", false, at); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times after a failure, want 1", asked)
	}
	if got := LoadClaims().Claims[claimA]; len(got) != 1 {
		t.Errorf("a failed check must keep the claim, got %+v", got)
	}
}

func TestPrsListDropsAnEndedClaimAndItsCheck(t *testing.T) {
	claimEnv(t)
	now := time.Now()
	if _, err := Claim(claimA, false, now); err != nil {
		t.Fatal(err)
	}
	FetchOpenPRs = func(string, string) (map[string]*WatchedPR, error) { return map[string]*WatchedPR{}, nil }
	EndState = func(string) (string, error) { return "merged", nil }
	if _, err := PrsList("a", "o", false, now); err != nil {
		t.Fatal(err)
	}
	c := LoadClaims()
	if _, ok := c.Claims[claimA]; ok {
		t.Errorf("merged PR's claim kept: %+v", c.Claims)
	}
	if _, ok := c.Checked[claimA]; ok {
		t.Errorf("merged PR's check kept: %+v", c.Checked)
	}
}

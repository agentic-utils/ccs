package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	claimA = "https://github.com/o/r/pull/1"
	claimB = "https://github.com/o/r/pull/2"
	claimC = "https://github.com/o/r/pull/3"
)

// claimEnv points the claims and watcher state at a temp folder and stubs
// the process tree: the test's parent pid runs under session "alpha".
func claimEnv(t *testing.T) (live map[int]prClaim) {
	t.Helper()
	oldSettings, oldLive, oldParent, oldFetch, oldEnd := settingsPath, liveByPID, parentPID, fetchOpenPRs, endState
	settingsPath = t.TempDir() + "/settings.json"
	live = map[int]prClaim{
		os.Getppid(): {Session: "s-alpha", Name: "alpha", PID: os.Getppid(), ProcStart: "Tue Nov  3 07:00:00 2026"},
	}
	liveByPID = func() map[int]prClaim { return live }
	parentPID = func(int) int { return 0 }
	fetchOpenPRs = func(string, string) (map[string]*watchedPR, error) { return nil, errors.New("no network in tests") }
	endState = func(string) (string, error) { return "", errors.New("no network in tests") }
	t.Cleanup(func() {
		settingsPath, liveByPID, parentPID, fetchOpenPRs, endState = oldSettings, oldLive, oldParent, oldFetch, oldEnd
	})
	return live
}

// asSession makes the claiming process run under the session at pid.
func asSession(pid int) {
	parentPID = func(p int) int {
		if p == os.Getppid() {
			return pid
		}
		return 0
	}
}

func TestClaimRecordsTheCallingSession(t *testing.T) {
	claimEnv(t)
	msg, err := claim(claimA, false, time.Now())
	if err != nil || msg != "alpha owns "+claimA {
		t.Fatalf("claim: %q %v", msg, err)
	}
	c := loadClaims().Claims[claimA]
	if len(c) != 1 || c[0].Session != "s-alpha" || c[0].Name != "alpha" || c[0].PID != os.Getppid() || c[0].ProcStart == "" {
		t.Errorf("recorded %+v", c)
	}
	claim(claimA, false, time.Now()) // again: still one claim
	if n := len(loadClaims().Claims[claimA]); n != 1 {
		t.Errorf("a session claiming twice has one claim, got %d", n)
	}
	if msg, _ := claim(claimA, true, time.Now()); msg != "alpha released "+claimA || len(loadClaims().Claims) != 0 {
		t.Errorf("release: %q, left %v", msg, loadClaims().Claims)
	}
}

func TestClaimRefusals(t *testing.T) {
	live := claimEnv(t)
	if _, err := claim("https://gitlab.com/o/r/pull/1", false, time.Now()); err == nil {
		t.Error("a non-GitHub address is refused")
	}
	delete(live, os.Getppid())
	if _, err := claim(claimA, false, time.Now()); err == nil || !strings.Contains(err.Error(), "not run from a Claude Code or Codex session") {
		t.Errorf("outside a session: %v", err)
	}
	if _, err := os.Stat(claimsPath()); err == nil {
		t.Error("nothing is written when refused")
	}
}

func TestClaimFindsAnAncestorSession(t *testing.T) {
	live := claimEnv(t)
	delete(live, os.Getppid()) // the shell between ccs and claude
	live[4242] = prClaim{Session: "s-beta", Name: "beta", PID: 4242, ProcStart: "x"}
	asSession(4242)
	if msg, err := claim(claimA, false, time.Now()); err != nil || !strings.HasPrefix(msg, "beta owns") {
		t.Errorf("the nearest live claude ancestor owns it: %q %v", msg, err)
	}
}

func TestSecondClaimIsKeptAndConcurrentClaimsAllLand(t *testing.T) {
	live := claimEnv(t)
	claim(claimA, false, time.Now())
	live[4242] = prClaim{Session: "s-beta", Name: "beta", PID: 4242, ProcStart: "x"}
	delete(live, os.Getppid())
	asSession(4242)
	msg, _ := claim(claimA, false, time.Now())
	if !strings.Contains(msg, "also claimed by 1 other") {
		t.Errorf("the second owner is told: %q", msg)
	}
	if c := loadClaims().Claims[claimA]; len(c) != 2 || c[0].Name != "alpha" || c[1].Name != "beta" {
		t.Errorf("both claims kept, in order: %+v", c)
	}

	// Many claims at once through the lock: none lost.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			withClaims(func(c *claimsFile) bool {
				u := "https://github.com/o/r/pull/" + string(rune('a'+i))
				c.Claims[u] = append(c.Claims[u], prClaim{Session: "s"})
				return true
			})
		}()
	}
	wg.Wait()
	if n := len(loadClaims().Claims); n != 21 {
		t.Errorf("concurrent claims: %d PRs, want 21", n)
	}
}

// writeWatchState saves a watcher state for o's PRs by the author me.
func writeWatchState(t *testing.T, updated time.Time, prs ...*watchedPR) {
	t.Helper()
	st := prWatchState{Author: "me", Owner: "o", Started: updated, Updated: updated, PRs: map[string]*watchedPR{}}
	for _, p := range prs {
		st.PRs[p.URL] = p
	}
	if err := savePRWatchState(st); err != nil {
		t.Fatal(err)
	}
}

func TestPRsListsOwnersAndFlags(t *testing.T) {
	live := claimEnv(t)
	now := time.Now()
	writeWatchState(t, now.Add(-time.Minute),
		&watchedPR{URL: claimA, Repo: "o/r", Number: 1, Title: "ready one", State: "open", Checks: "success", Review: "approved"},
		&watchedPR{URL: claimB, Repo: "o/r", Number: 2, Title: "optional fail", State: "open", Checks: "failing", Failing: []string{"setup"}, Review: "approved"},
		&watchedPR{URL: claimC, Repo: "o/r", Number: 3, Title: "nobody's", State: "open", Checks: "pending", Review: "required"},
		&watchedPR{URL: "https://github.com/o/r/pull/9", Repo: "o/r", Number: 9, State: "merged"},
	)
	claim(claimA, false, now)
	claim(claimB, false, now)
	claim("https://github.com/o/r/pull/9", false, now) // merged: dropped on read
	live[4242] = prClaim{Session: "s-beta", Name: "beta", PID: 4242, ProcStart: "x"}
	delete(live, os.Getppid())
	asSession(4242)
	claim(claimB, false, now) // contested
	live[os.Getppid()] = prClaim{Session: "s-alpha", Name: "alpha", PID: os.Getppid(), ProcStart: "Tue Nov  3 07:00:00 2026"}

	rep, err := prsList("me", "o", false, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.PRs) != 3 {
		t.Fatalf("open PRs only: %+v", rep.PRs)
	}
	got := map[string]prRow{}
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
	if _, ok := loadClaims().Claims["https://github.com/o/r/pull/9"]; ok {
		t.Error("a merged PR's claim is dropped")
	}
	if s := rep.statusLine(); s != "PRs 1 ready · 1 need you · 1 unowned · 1 contested" {
		t.Errorf("statusline %q", s)
	}
	if txt := rep.text(); !strings.Contains(txt, "UNOWNED") || !strings.Contains(txt, "CONTESTED: alpha, beta") {
		t.Errorf("text flags them:\n%s", txt)
	}

	// The beta session stops: its claim goes, so claimB is no longer contested.
	delete(live, 4242)
	rep, _ = prsList("me", "o", true, now)
	for _, r := range rep.PRs {
		if r.PR == claimB && (r.Contested || len(r.Owners) != 1) {
			t.Errorf("a stopped session's claim is dropped: %+v", r)
		}
	}
	if c := loadClaims().Claims[claimB]; len(c) != 1 || c[0].Name != "alpha" {
		t.Errorf("pruned on disk: %+v", c)
	}
}

func TestClaimOfARecycledPIDIsDropped(t *testing.T) {
	live := claimEnv(t)
	claim(claimA, false, time.Now())
	live[os.Getppid()] = prClaim{Session: "s-alpha", Name: "alpha", PID: os.Getppid(), ProcStart: "another start"}
	writeWatchState(t, time.Now(), &watchedPR{URL: claimA, Repo: "o/r", Number: 1, State: "open"})
	rep, _ := prsList("me", "o", true, time.Now())
	if len(rep.PRs) != 1 || !rep.PRs[0].Unowned {
		t.Errorf("same pid, different process: not the owner: %+v", rep.PRs)
	}
}

func TestClaimFollowsARename(t *testing.T) {
	live := claimEnv(t)
	claim(claimA, false, time.Now())
	live[os.Getppid()] = prClaim{Session: "s-alpha", Name: "alpha-renamed", PID: os.Getppid(), ProcStart: "Tue Nov  3 07:00:00 2026"}
	writeWatchState(t, time.Now(), &watchedPR{URL: claimA, Repo: "o/r", Number: 1, State: "open"})
	rep, _ := prsList("me", "o", true, time.Now())
	if len(rep.PRs) != 1 || !slices.Equal(rep.PRs[0].Owners, []string{"alpha-renamed"}) {
		t.Errorf("a /rename after the claim: still its owner, under its new name: %+v", rep.PRs)
	}
	if c := loadClaims().Claims[claimA]; len(c) != 1 || c[0].Name != "alpha-renamed" {
		t.Errorf("the stored claim takes the new name: %+v", c)
	}
}

func TestPRsFreshnessAndStatusline(t *testing.T) {
	claimEnv(t)
	now := time.Now()
	asked := 0
	fetchOpenPRs = func(string, string) (map[string]*watchedPR, error) {
		asked++
		return map[string]*watchedPR{claimA: {URL: claimA, Repo: "o/r", Number: 1, State: "open", Checks: "success", Review: "approved"}}, nil
	}
	// No watcher state: statusline prints nothing and asks nobody.
	if rep, _ := prsList("me", "o", true, now); rep.statusLine() != "" || asked != 0 {
		t.Errorf("offline with no state: %q, asked %d", rep.statusLine(), asked)
	}
	// Stale state: ccs prs asks GitHub; the statusline uses it and says so.
	writeWatchState(t, now.Add(-time.Hour), &watchedPR{URL: claimB, Repo: "o/r", Number: 2, State: "open", Checks: "success", Review: "approved"})
	rep, _ := prsList("me", "o", false, now)
	if asked != 1 || len(rep.PRs) != 1 || rep.PRs[0].PR != claimA {
		t.Errorf("stale state: asked %d, %+v", asked, rep.PRs)
	}
	rep, _ = prsList("me", "o", true, now)
	if s := rep.statusLine(); s != "PRs 1 ready · 1 unowned (stale)" || asked != 1 {
		t.Errorf("statusline from stale state: %q (asked %d)", s, asked)
	}
	// Fresh state: no GitHub call.
	writeWatchState(t, now.Add(-time.Minute))
	if _, err := prsList("me", "o", false, now); err != nil || asked != 1 {
		t.Errorf("fresh state is used: asked %d, %v", asked, err)
	}
}

func TestPRsCLIAndMCP(t *testing.T) {
	claimEnv(t)
	writeWatchState(t, time.Now(), &watchedPR{URL: claimA, Repo: "o/r", Number: 1, Title: "t", State: "open", Checks: "success", Review: "approved"})
	var out, errs strings.Builder
	if code := runPRs([]string{"--json", "--owner", "o"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var rep prsReport
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil || len(rep.PRs) != 1 || !rep.PRs[0].Unowned {
		t.Errorf("--json: %v %s", err, out.String())
	}
	out.Reset()
	if code := runClaim([]string{claimA}, &out, &errs); code != 0 || !strings.Contains(out.String(), "alpha owns") {
		t.Errorf("ccs claim: %d %q %q", code, out.String(), errs.String())
	}
	out.Reset()
	if code := runClaim([]string{claimA, "--release"}, &out, &errs); code != 0 || !strings.Contains(out.String(), "released") {
		t.Errorf("ccs claim <url> --release: %d %q", code, out.String())
	}
	if code := runClaim(nil, &out, &errs); code != 2 {
		t.Errorf("no URL: exit %d", code)
	}
	if res, err := mcpCall("list_prs", nil, nil); err != nil || !strings.Contains(fmt.Sprint(res), claimA) {
		t.Errorf("list_prs defaults to the running watcher's author and owner: %v %v", res, err)
	}
	os.Remove(prWatchStatePath())
	if _, err := mcpCall("list_prs", nil, nil); err == nil {
		t.Error("no watcher state and no gh user: list_prs should fail")
	}
	out.Reset()
	if code := runPRs([]string{"--statusline"}, &out, &errs); code != 0 || out.Len() != 0 {
		t.Errorf("--statusline with nothing saved prints nothing and asks nothing: %d %q", code, out.String())
	}
}

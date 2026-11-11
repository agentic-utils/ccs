package github

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// limitEnv points the backoff file at a temp folder and stubs gh: refused
// says which resources GitHub refuses, and rateLimit is the rate_limit answer.
func limitEnv(t *testing.T, refused map[string]bool, rateLimit string) (calls *[]string) {
	t.Helper()
	oldSettings, oldGH := state.SettingsPath, RunGH
	state.SettingsPath = t.TempDir() + "/settings.json"
	calls = &[]string{}
	RunGH = func(args ...string) ([]byte, error) {
		*calls = append(*calls, fmt.Sprint(args))
		if len(args) == 2 && args[1] == "rate_limit" {
			return []byte(rateLimit), nil
		}
		if refused[ghResource(args)] {
			return nil, errors.New("exit status 1: gh: API rate limit exceeded for user ID 1. (HTTP 403)")
		}
		return []byte(`{}`), nil
	}
	t.Cleanup(func() { state.SettingsPath, RunGH = oldSettings, oldGH })
	return calls
}

func TestGHResource(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"search", "prs", "--author", "me"}, "search"},
		{[]string{"api", "search/issues?q=x"}, "search"},
		{[]string{"api", "graphql", "-f", "query=x"}, "graphql"},
		{[]string{"api", "repos/o/r/pulls/1"}, "core"},
		{[]string{"api", "user", "--jq", ".login"}, "core"},
	} {
		if got := ghResource(c.args); got != c.want {
			t.Errorf("ghResource(%v) = %s, want %s", c.args, got, c.want)
		}
	}
}

func TestGHCallBacksOffUntilTheReset(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	calls := limitEnv(t, map[string]bool{"core": true}, fmt.Sprintf(`{"resources":{"core":{"remaining":0,"reset":%d},"search":{"remaining":30,"reset":%d}}}`, reset.Unix(), reset.Unix()))

	_, err := ghCall("api", "repos/o/r/pulls/1")
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.Resource != "core" || !rl.Until.Equal(reset) || rl.Err == nil {
		t.Fatalf("a refusal backs off until GitHub's reset: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("the call, then rate_limit: %v", *calls)
	}
	// Every later core call, from any ccs process, fails without running gh.
	for range 5 {
		if _, err := ghCall("api", "repos/o/r/pulls/2"); !IsRateLimited(err) {
			t.Fatalf("still limited: %v", err)
		}
	}
	if len(*calls) != 2 {
		t.Errorf("no gh while backing off: %v", *calls)
	}
	if until, ok := loadGHLimits()["core"]; !ok || !until.Equal(reset) {
		t.Errorf("the backoff is shared through the file: %v", loadGHLimits())
	}
	// Another resource isn't held back.
	if _, err := ghCall("search", "prs"); err != nil {
		t.Errorf("search has its own allowance: %v", err)
	}
	// Past the reset, it asks again.
	writeJSON(ghLimitPath(), map[string]time.Time{"core": time.Now().Add(-time.Second)})
	if _, ok := limitedUntil("core", time.Now()); ok {
		t.Error("a backoff in the past doesn't hold")
	}
}

func TestGHCallSecondaryLimitBacksOffAMinute(t *testing.T) {
	// A secondary limit leaves the primary allowance: no reset to wait for.
	limitEnv(t, map[string]bool{"core": true}, `{"resources":{"core":{"remaining":4000,"reset":1}}}`)
	before := time.Now()
	_, err := ghCall("api", "repos/o/r/pulls/1")
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.Until.Before(before.Add(limitFallback)) || rl.Until.After(time.Now().Add(limitFallback)) {
		t.Errorf("backs off limitFallback: %v", err)
	}
	// An unreadable rate_limit answer does the same.
	limitEnv(t, map[string]bool{"core": true}, `not json`)
	if _, err := ghCall("api", "repos/o/r/pulls/1"); !errors.As(err, &rl) || rl.Until.After(time.Now().Add(limitFallback)) {
		t.Errorf("no reset known: %v", err)
	}
}

func TestGHCallOtherErrorsDontBackOff(t *testing.T) {
	calls := limitEnv(t, nil, `{}`)
	RunGH = func(args ...string) ([]byte, error) {
		*calls = append(*calls, fmt.Sprint(args))
		return nil, errors.New("exit status 1: gh: Not Found (HTTP 404)")
	}
	for range 2 {
		if _, err := ghCall("api", "repos/o/r/pulls/1"); err == nil || IsRateLimited(err) {
			t.Errorf("a 404 is only a 404: %v", err)
		}
	}
	if len(*calls) != 2 || len(loadGHLimits()) != 0 {
		t.Errorf("asked each time, nothing recorded: %v %v", *calls, loadGHLimits())
	}
}

func TestLimitedUntilIsCapped(t *testing.T) {
	limitEnv(t, nil, `{}`)
	now := time.Now()
	writeJSON(ghLimitPath(), map[string]time.Time{"core": now.Add(24 * time.Hour)})
	if until, ok := limitedUntil("core", now); ok {
		t.Errorf("a reset beyond limitMax is ignored, never waited for: %v", until)
	}
}

func TestPRDetailsAllStopsAtTheLimit(t *testing.T) {
	reset := time.Now().Add(10 * time.Minute)
	calls := limitEnv(t, map[string]bool{"core": true}, fmt.Sprintf(`{"resources":{"core":{"remaining":0,"reset":%d}}}`, reset.Unix()))
	var open []WatchedPR
	for i := range 20 {
		open = append(open, WatchedPR{URL: fmt.Sprintf("https://github.com/o/r/pull/%d", i), Repo: "o/r", Number: i})
	}
	for _, d := range prDetailsAll(open) {
		if d != nil {
			t.Fatal("no PR could be read")
		}
	}
	// At most the PRs already in flight when the first refusal landed ask gh.
	asked := 0
	for _, c := range *calls {
		if c != "[api rate_limit]" {
			asked++
		}
	}
	if asked > prWatchParallel {
		t.Errorf("%d calls after the limit was hit, want at most %d", asked, prWatchParallel)
	}
}

// sharedEnv is claimEnv with a stale watcher state and a counted fetch.
func sharedEnv(t *testing.T, now time.Time) (asked *int) {
	t.Helper()
	claimEnv(t)
	oldRunning := PRWatchRunning
	PRWatchRunning = func() bool { return false }
	t.Cleanup(func() { PRWatchRunning = oldRunning })
	asked = new(int)
	FetchOpenPRs = func(string, string) (map[string]*WatchedPR, error) {
		*asked++
		return map[string]*WatchedPR{claimA: {URL: claimA, Repo: "o/r", Number: 1, State: "open", Checks: "success", Review: "approved"}}, nil
	}
	writeWatchState(t, now.Add(-time.Hour), &WatchedPR{URL: claimB, Repo: "o/r", Number: 2, State: "open", Checks: "success", Review: "approved"})
	return asked
}

func TestPRsSharesOneFetchAcrossCallers(t *testing.T) {
	now := time.Now()
	asked := sharedEnv(t, now)
	// Ten sessions' status lines in the same minute: one fetch.
	for i := range 10 {
		rep, err := PRsList("me", "o", false, now.Add(time.Duration(i)*time.Second))
		if err != nil || len(rep.PRs) != 1 || rep.PRs[0].PR != claimA {
			t.Fatalf("call %d: %v %+v", i, err, rep.PRs)
		}
	}
	if *asked != 1 {
		t.Errorf("one fetch for every caller, asked %d", *asked)
	}
	// prsFresh later, the next caller asks again.
	if _, err := PRsList("me", "o", false, now.Add(prsFresh+time.Second)); err != nil || *asked != 2 {
		t.Errorf("a stale fetch is redone: asked %d, %v", *asked, err)
	}
	// Another author's answer isn't this one's.
	if f, ok := loadPRsFetch("someone", "o"); ok {
		t.Errorf("fetch is per author: %+v", f)
	}
}

func TestPRsFailedFetchIsntRetriedEachCall(t *testing.T) {
	now := time.Now()
	asked := sharedEnv(t, now)
	FetchOpenPRs = func(string, string) (map[string]*WatchedPR, error) {
		*asked++
		return nil, &RateLimitedError{Resource: "core", Until: now.Add(time.Hour)}
	}
	for i := range 5 {
		rep, err := PRsList("me", "o", false, now.Add(time.Duration(i)*time.Second))
		if err != nil || len(rep.PRs) != 1 || rep.PRs[0].PR != claimB || !rep.Stale {
			t.Fatalf("falls back to the watcher's stale state: %v %+v", err, rep)
		}
	}
	if *asked != 1 {
		t.Errorf("one try each prsFresh, failed or not: asked %d", *asked)
	}
	// With no watcher state at all, the failure is reported.
	os.Remove(PRWatchStatePath())
	os.Remove(prsFetchPath())
	if _, err := PRsList("me", "o", false, now); err == nil {
		t.Error("nothing to show: an error")
	}
}

func TestPRsWithWatcherRunningNeverFetch(t *testing.T) {
	now := time.Now()
	asked := sharedEnv(t, now)
	PRWatchRunning = func() bool { return true }
	rep, err := PRsList("me", "o", false, now)
	if err != nil || *asked != 0 || !rep.Stale || len(rep.PRs) != 1 || rep.PRs[0].PR != claimB {
		t.Errorf("a running watcher's state stands, stale or not: asked %d, %v %+v", *asked, err, rep)
	}
}

func TestPRsPrefersTheNewerOfWatcherAndFetch(t *testing.T) {
	now := time.Now()
	asked := sharedEnv(t, now)
	// A week-old shared answer, then a watcher that ran until 15 minutes ago.
	writeJSON(prsFetchPath(), prsFetch{Author: "me", Owner: "o", Tried: now.Add(-time.Minute), Updated: now.Add(-7 * 24 * time.Hour),
		PRs: map[string]*WatchedPR{claimA: {URL: claimA, Repo: "o/r", Number: 1, State: "open"}}})
	writeWatchState(t, now.Add(-15*time.Minute), &WatchedPR{URL: claimB, Repo: "o/r", Number: 2, State: "open", Checks: "success", Review: "approved"})
	rep, err := PRsList("me", "o", false, now)
	if err != nil || *asked != 0 || len(rep.PRs) != 1 || rep.PRs[0].PR != claimB {
		t.Errorf("the watcher's newer state wins: asked %d, %v %+v", *asked, err, rep.PRs)
	}
}

func TestPRsDistrustsALongStaleRunningWatcher(t *testing.T) {
	now := time.Now()
	asked := sharedEnv(t, now)
	PRWatchRunning = func() bool { return true }
	writeWatchState(t, now.Add(-watchTrust-time.Minute), &WatchedPR{URL: claimB, Repo: "o/r", Number: 2, State: "open"})
	if _, err := PRsList("me", "o", false, now); err != nil || *asked != 1 {
		t.Errorf("a watcher with no pass in watchTrust doesn't stop the fetch: asked %d, %v", *asked, err)
	}
}

func TestPRsFetchInFlightElsewhere(t *testing.T) {
	now := time.Now()
	asked := sharedEnv(t, now)
	f, err := os.OpenFile(prsFetchLock(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	rep, err := PRsList("me", "o", false, now)
	if err != nil || *asked != 0 || rep.PRs[0].PR != claimB {
		t.Errorf("another process is asking: use what there is: asked %d, %v %+v", *asked, err, rep)
	}
}

func TestPRWatchRunning(t *testing.T) {
	limitEnv(t, nil, `{}`)
	if PRWatchRunning() {
		t.Error("no lock file: not running")
	}
	lock, err := LockPRWatch()
	if err != nil {
		t.Fatal(err)
	}
	if !PRWatchRunning() {
		t.Error("lock held: running")
	}
	lock.Close()
	os.WriteFile(prWatchLockPath(), []byte("999999999\n"), 0o600)
	if PRWatchRunning() {
		t.Error("the pid it names has exited: not running")
	}
}

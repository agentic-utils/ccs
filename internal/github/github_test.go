package github

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestUnstableSaysWhyNotClean(t *testing.T) {
	for _, c := range []struct {
		p     PrInfo
		group int
		want  string
	}{
		{PrInfo{State: "OPEN", MergeState: "UNSTABLE", Checks: "PENDING"}, PrReady, "ready to merge · optional checks running"},
		// mergeable, but a failed check needs looking at, whether required or not
		{PrInfo{State: "OPEN", MergeState: "UNSTABLE", Checks: "FAILURE", Failing: []string{"lint"}}, PrFix, "optional checks failing: lint"},
		{PrInfo{State: "OPEN", MergeState: "UNSTABLE", Checks: "SUCCESS"}, PrReady, "ready to merge"},
	} {
		if g, why := c.p.Status(); g != c.group || why != c.want {
			t.Errorf("%+v: %d %q, want %q", c.p, g, why, c.want)
		}
	}
}

func TestUnresolvedThreadsNeedYou(t *testing.T) {
	for _, c := range []struct {
		p     PrInfo
		group int
		want  string
	}{
		{PrInfo{State: "OPEN", MergeState: "CLEAN", Checks: "SUCCESS", Unresolved: 2}, PrFix, "2 unresolved threads"},
		{PrInfo{State: "OPEN", MergeState: "UNSTABLE", Checks: "PENDING", Unresolved: 1}, PrFix, "1 unresolved thread"},
		// conflicts and failing checks still say so first
		{PrInfo{State: "OPEN", MergeState: "DIRTY", Unresolved: 1}, PrFix, "conflicts"},
		{PrInfo{State: "OPEN", MergeState: "BLOCKED", Checks: "FAILURE", Failing: []string{"test"}, Unresolved: 1}, PrFix, "checks failing: test"},
		{PrInfo{State: "OPEN", Draft: true, Unresolved: 1}, PrWaiting, "draft"},
		{PrInfo{State: "OPEN", MergeState: "CLEAN", Checks: "SUCCESS"}, PrReady, "ready to merge"},
	} {
		if g, why := c.p.Status(); g != c.group || why != c.want {
			t.Errorf("%+v: %d %q, want %d %q", c.p, g, why, c.group, c.want)
		}
	}
}

func TestThreadsBeyondTheBatchAreNotClear(t *testing.T) {
	p := PrInfo{State: "OPEN", MergeState: "CLEAN", Checks: "SUCCESS", ThreadsUnread: true}
	if g, why := p.Status(); g == PrReady || !strings.Contains(why, "100 review threads") {
		t.Errorf("unread threads: never ready: %d %q", g, why)
	}
	p.Unresolved = 2
	if _, why := p.Status(); why != "2 unresolved threads" {
		t.Errorf("an unresolved one read is the reason: %q", why)
	}
}

func TestNeedsReviewNeedsAThreadCount(t *testing.T) {
	zero, one := 0, 1
	p := WatchedPR{State: "open", Checks: "success", Review: "required"}
	if p.NeedsReview() {
		t.Error("threads never counted: not yet")
	}
	if p.Threads = &one; p.NeedsReview() {
		t.Error("an unresolved thread: no")
	}
	if p.Threads = &zero; !p.NeedsReview() {
		t.Error("green, counted, none open: yes")
	}
	if g, why := p.status(); g != PrWaiting || why != "review required" {
		t.Errorf("ccs prs: %d %q", g, why)
	}
	p.Threads = &one
	if g, why := p.status(); g != PrFix || why != "1 unresolved thread" {
		t.Errorf("ccs prs shows the threads: %d %q", g, why)
	}
}

func TestWatchDefaultsToTheGhUserInEveryOwner(t *testing.T) {
	old, oldRun := GhUser, GhRun
	t.Cleanup(func() { GhUser, GhRun = old, oldRun })
	GhUser = func() (string, error) { return "someone", nil }
	a := ""
	if err := defaultAuthor(&a); err != nil || a != "someone" {
		t.Errorf("author defaults to the gh user: %q %v", a, err)
	}
	a = "given"
	if defaultAuthor(&a); a != "given" {
		t.Errorf("--author wins: %q", a)
	}
	GhUser = func() (string, error) { return "", errors.New("not logged in") }
	a = ""
	if err := defaultAuthor(&a); err == nil {
		t.Error("no gh user: accepted")
	}
	var args []string
	GhRun = func(a ...string) ([]byte, error) { args = a; return []byte("[]"), nil }
	searchPRs("someone", "", "--state", "open")
	if slices.Contains(args, "--owner") {
		t.Errorf("no owner searches every owner: %v", args)
	}
	searchPRs("someone", "o", "--state", "open")
	if i := slices.Index(args, "--owner"); i < 0 || args[i+1] != "o" {
		t.Errorf("--owner o: %v", args)
	}
}

func TestRemovedReviewFlagsSayWhereReviewsWent(t *testing.T) {
	for _, args := range [][]string{{"--review"}, {"--once", "-review-channel=C1"}, {"--dry-run"}, {"--review-hook", "x"}} {
		if got := RunWatch(args); got != 2 {
			t.Errorf("%v: exit %d, want 2 before anything runs", args, got)
		}
	}
}

// A fake gh answering one PR and failing another the way GitHub does: the
// data for the rest, an error for that field, and a non-zero exit.
func TestGhPRBatchReadsPartialAnswers(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	os.WriteFile(gh, []byte(`#!/bin/sh
case "$*" in *'repository(owner:"o",name:"r"){pullRequest(number:1)'*) ;; *) exit 3;; esac
echo '{"data":{"p0":{"pullRequest":{"number":1,"title":"t","state":"OPEN","isDraft":false,"mergeStateStatus":"BLOCKED","reviewDecision":"APPROVED","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE","contexts":{"nodes":[{"name":"test","conclusion":"FAILURE"},{"name":"lint","conclusion":"SUCCESS"},{"context":"ci/deploy","state":"ERROR"}]}}}}]}}},"p1":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND"}]}'
exit 1
`), 0o755)
	got, err := GhPRBatch(gh, []string{prA, prB})
	want := PrInfo{Number: 1, Title: "t", State: "OPEN", MergeState: "BLOCKED", Review: "APPROVED", Checks: "FAILURE", Failing: []string{"test", "ci/deploy"}}
	if err != nil || !reflect.DeepEqual(got[prA], want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, why := got[prA].Status(); why != "checks failing: test, ci/deploy" {
		t.Errorf("a PR with failing checks names them: %q", why)
	}
	if got[prB].State != "GONE" {
		t.Errorf("a PR GitHub couldn't find says so instead of checking forever: %+v", got[prB])
	}
}

const (
	prA = "https://github.com/o/r/pull/1"
	prB = "https://github.com/o/r/pull/2"
	prC = "https://github.com/o/r/pull/3"
	prD = "https://github.com/o/r/pull/4"
)

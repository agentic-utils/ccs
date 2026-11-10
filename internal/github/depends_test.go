package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// names is a pass's events as "event repo#N".
func names(events []PREvent) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, strings.TrimSpace(e.Event+" "+PRName(e.PR)))
	}
	return out
}

func TestDependsRecordsAndRefuses(t *testing.T) {
	watchWorld(t)
	claimEnv(t)
	run := func(args ...string) (int, string) {
		var out, errs bytes.Buffer
		code := RunDepends(args, &out, &errs)
		return code, out.String() + errs.String()
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "no merge order recorded") {
		t.Errorf("nothing yet: %d %q", code, out)
	}
	a, b, c := "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2", "https://github.com/o/r/pull/3"
	if code, out := run(b, "--after", a); code != 0 || !strings.Contains(out, "r#2 waits for r#1") {
		t.Errorf("recorded: %d %q", code, out)
	}
	// Flags may come first, a second prerequisite is added beside the first,
	// and owner/repo#N is an address.
	if code, out := run("--after", "o/r#3", b); code != 0 || !strings.Contains(out, "r#2 waits for r#1, r#3") {
		t.Errorf("added: %d %q", code, out)
	}
	if d := LoadDepends().Depends[b]; d == nil || !slices.Equal(d.After, []string{a, c}) || d.By != "alpha" {
		t.Errorf("saved with the session that said so: %+v", d)
	}
	// One spelling per PR: a padded number is the same PR, so it can't wait for itself.
	if code, out := run("o/r#01", "--after", a); code == 0 {
		t.Errorf("o/r#01 is r#1, which can't wait for itself: %q", out)
	}
	// repo#N works once ccs knows one repo of that name.
	if code, out := run("r#3", "--after", "r#1"); code != 0 || !strings.Contains(out, "r#3 waits for r#1") {
		t.Errorf("short names: %d %q", code, out)
	}
	for _, bad := range [][]string{
		{a, "--after", a},           // itself
		{a, "--after", b},           // a circle: b already waits for a
		{a, "--after", c},           // a longer one: c waits for a
		{"nope#1", "--after", a},    // a repo ccs doesn't know
		{"not a pr", "--after", a},  // not a PR
		{a},                         // neither --after nor --drop
		{a, "--after", b, "--drop"}, // both
		{a, b, "--after", c},        // two PRs
	} {
		if code, out := run(bad...); code == 0 {
			t.Errorf("%v should be refused: %q", bad, out)
		}
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "r#2 after r#1, r#3") || !strings.Contains(out, "r#3 after r#1") {
		t.Errorf("listed: %d %q", code, out)
	}
	var rows []DependRow
	if code, out := run("--json"); code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 || !slices.Equal(rows[0].Waiting, []string{a, c}) {
		t.Errorf("as JSON: %d %q", code, out)
	}
	if code, out := run(b, "--drop"); code != 0 || !strings.Contains(out, "no longer waits") || LoadDepends().Depends[b] != nil {
		t.Errorf("dropped: %d %q", code, out)
	}
	if code, out := run(b, "--drop"); code != 0 || !strings.Contains(out, "waited for nothing") {
		t.Errorf("dropping nothing says so: %d %q", code, out)
	}
}

func TestWatchHoldsAPRThatMustMergeAfterAnother(t *testing.T) {
	g := watchWorld(t)
	claimEnv(t)
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	first := &fakePR{repo: "o/infra", number: 1, state: "open", head: "h1", mergeable: "clean"}
	second := &fakePR{repo: "o/infra", number: 2, state: "open", head: "h2", mergeable: "clean"}
	pending(first)
	pending(second)
	g.prs = []*fakePR{first, second}
	passEvents(t, now)
	step := func(want ...string) []PREvent {
		t.Helper()
		now = now.Add(3 * time.Minute)
		got := eventsSince(t, now)
		if !slices.Equal(names(got), want) {
			t.Errorf("got %v, want %v", names(got), want)
		}
		return got
	}
	if _, err := Depend(second.url(), []string{first.url()}, now); err != nil {
		t.Fatal(err)
	}
	if got := step("blocked infra#2"); len(got) == 1 && !slices.Equal(got[0].After, []string{first.url()}) {
		t.Errorf("blocked says what it waits for: %+v", got[0])
	}
	// Approved and green, it still isn't ready: only its verdict is said.
	green(second)
	second.reviews = [][2]string{{"rev", "APPROVED"}}
	step("checks_passed infra#2", "approved infra#2")
	row := func() PRRow {
		t.Helper()
		rep, err := PRsList("me", "o", true, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rep.PRs {
			if r.PR == second.url() {
				return r
			}
		}
		t.Fatalf("ccs prs doesn't list %s: %+v", second.url(), rep.PRs)
		return PRRow{}
	}
	if r := row(); r.Group != "waiting" || r.Status != "blocked: after infra#1" || !slices.Equal(r.BlockedBy, []string{first.url()}) {
		t.Errorf("ccs prs shows it blocked, not ready: %+v", r)
	}
	// The list, which asks GitHub itself, never calls it ready either.
	if g, why := (PRInfo{State: "OPEN", MergeState: "CLEAN", BlockedBy: BlockedNow(second.url())}).Status(); g != PRWaiting || why != "blocked: after infra#1" {
		t.Errorf("the list shows it blocked: %d %q", g, why)
	}
	// Auto-merge turned on for it: said at once, and its owner is told.
	var told []string
	oldSend := SendNudge
	SendNudge = func(c PRClaim, text string) error { told = append(told, text); return nil }
	t.Cleanup(func() { SendNudge = oldSend })
	if _, err := Claim(second.url(), false, now); err != nil {
		t.Fatal(err)
	}
	second.autoMerge = true
	step("blocked_automerge infra#2")
	if err := NudgeTick(now); err != nil || len(told) != 1 || !strings.Contains(told[0], "auto-merge on, though it must not merge before infra#1 has") || !strings.Contains(told[0], "gh pr merge --disable-auto "+second.url()) {
		t.Errorf("the owner is told how to stop it: %v %q", err, told)
	}
	NudgeTick(now)
	if len(told) != 1 {
		t.Errorf("told once: %q", told)
	}
	if r := row(); r.Group != "needs_you" || !strings.Contains(r.Status, "auto-merge is on") {
		t.Errorf("auto-merge on a blocked PR needs you: %+v", r)
	}
	step() // said once
	second.autoMerge = false
	// The prerequisite merges: the dependent is free, and ready at last.
	first.state, first.mergedAt = "merged", now.Add(time.Minute)
	if got := step("unblocked infra#2", "ready infra#2", "merged infra#1"); len(got) == 3 && !slices.Equal(got[0].After, []string{first.url()}) {
		t.Errorf("unblocked says what it waited for: %+v", got[0])
	}
	second.state, second.mergedAt = "merged", now.Add(time.Minute)
	step("merged infra#2") // in order: nothing more to say
	if d := LoadDepends().Depends; len(d) != 0 {
		t.Errorf("a merged PR's dependency is forgotten: %+v", d)
	}
}

func TestWatchSaysWhenAPRMergedBeforeWhatItWaitedFor(t *testing.T) {
	g := watchWorld(t)
	claimEnv(t)
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	first := &fakePR{repo: "o/infra", number: 1, state: "open", head: "h1", mergeable: "clean"}
	second := &fakePR{repo: "o/infra", number: 2, state: "open", head: "h2", mergeable: "clean"}
	third := &fakePR{repo: "o/infra", number: 3, state: "open", head: "h3", mergeable: "clean"}
	g.prs = []*fakePR{first, second, third}
	passEvents(t, now)
	Depend(second.url(), []string{first.url()}, now)
	Depend(third.url(), []string{first.url()}, now)
	eventsSince(t, now.Add(3*time.Minute)) // both blocked

	// Both merge between two passes, the dependent 12 seconds before its
	// prerequisite; the third merges after it, as it should.
	at := now.Add(4 * time.Minute)
	second.state, second.mergedAt = "merged", at
	first.state, first.mergedAt = "merged", at.Add(12*time.Second)
	third.state, third.mergedAt = "merged", at.Add(time.Minute)
	got := eventsSince(t, now.Add(6*time.Minute))
	early := 0
	for _, e := range got {
		if e.Event == "merged_early" {
			early++
			if e.PR != second.url() || !slices.Equal(e.After, []string{first.url()}) {
				t.Errorf("merged_early names the PR and what it skipped: %+v", e)
			}
		}
	}
	if early != 1 {
		t.Errorf("one PR merged early: %v", names(got))
	}
}

func TestDependsOnAPRTheWatchDoesNotCover(t *testing.T) {
	g := watchWorld(t)
	claimEnv(t)
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	mine := &fakePR{repo: "o/app", number: 5, state: "open", head: "h5", mergeable: "clean"}
	g.prs = []*fakePR{mine}
	passEvents(t, now)
	theirs := "https://github.com/other/lib/pull/9" // someone else's PR: not in the watcher's searches
	Depend(mine.url(), []string{theirs}, now)
	state, at := "open", ""
	fail := false
	oldEnd := PREnd
	t.Cleanup(func() { PREnd = oldEnd })
	PREnd = func(u string) (string, string, error) {
		if fail || u != theirs {
			return "", "", errors.New("rate limited")
		}
		return state, at, nil
	}
	if got := names(eventsSince(t, now.Add(3*time.Minute))); !slices.Equal(got, []string{"blocked app#5"}) {
		t.Errorf("GitHub is asked about a prerequisite the watch doesn't list: %v", got)
	}
	fail = true // not knowing isn't merged: it stays blocked
	if got := names(eventsSince(t, now.Add(6*time.Minute))); len(got) != 0 {
		t.Errorf("an unknown prerequisite keeps it blocked: %v", got)
	}
	// A declaration made a moment ago holds the PR in ccs prs at once, and
	// one the watcher has judged isn't asked about again offline.
	rep, _ := PRsList("me", "o", true, now.Add(7*time.Minute))
	if len(rep.PRs) != 1 || !slices.Equal(rep.PRs[0].BlockedBy, []string{theirs}) {
		t.Errorf("blocked in the list: %+v", rep.PRs)
	}
	// It merges, but 12 seconds after mine did: mine merged early, though
	// both had merged by the time anyone looked.
	mergedAt := now.Add(8 * time.Minute)
	fail, state, at = false, "merged", mergedAt.Add(12*time.Second).UTC().Format(time.RFC3339)
	mine.state, mine.mergedAt = "merged", mergedAt
	if got := names(eventsSince(t, now.Add(9*time.Minute))); !slices.Equal(got, []string{"merged app#5", "merged_early app#5"}) {
		t.Errorf("merge times decide it for a prerequisite the watch doesn't cover: %v", got)
	}
	if d := LoadDepends().Depends; len(d) != 0 {
		t.Errorf("forgotten once said and saved: %+v", d)
	}
}

func TestWatchMergeOrderHoldsThroughTheQuietPaths(t *testing.T) {
	g := watchWorld(t)
	claimEnv(t)
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	first := &fakePR{repo: "o/infra", number: 1, state: "open", head: "h1", mergeable: "clean", updatedAt: "u1"}
	second := &fakePR{repo: "o/infra", number: 2, state: "open", head: "h2", mergeable: "clean", updatedAt: "u2", autoMerge: true}
	green(first)
	green(second)
	second.reviews = [][2]string{{"rev", "APPROVED"}}
	g.prs = []*fakePR{first, second}
	// Declared, with auto-merge already on, before the watch ever ran: the
	// first pass is otherwise quiet, but never about this.
	Depend(second.url(), []string{first.url()}, now)
	if got := names(eventsSince(t, now)); !slices.Contains(got, "blocked_automerge infra#2") || slices.Contains(got, "ready infra#2") {
		t.Errorf("the first pass warns of auto-merge on a waiting PR: %v", got)
	}
	if got := names(eventsSince(t, now.Add(3*time.Minute))); len(got) != 0 {
		t.Errorf("said once: %v", got)
	}
	// Dropped and declared again while nothing on GitHub changes (the PR's
	// details aren't read again: its updatedAt is the same).
	Undepend(second.url())
	if got := names(eventsSince(t, now.Add(6*time.Minute))); !slices.Equal(got, []string{"unblocked infra#2", "ready infra#2"}) {
		t.Errorf("free, through the unchanged-PR path: %v", got)
	}
	Depend(second.url(), []string{first.url()}, now.Add(7*time.Minute))
	if got := names(eventsSince(t, now.Add(9*time.Minute))); !slices.Equal(got, []string{"blocked infra#2", "blocked_automerge infra#2"}) {
		t.Errorf("held again, through the unchanged-PR path: %v", got)
	}
	// Its details can't be read: a dependency declared since still holds it.
	Undepend(second.url())
	eventsSince(t, now.Add(12*time.Minute))
	Depend(second.url(), []string{first.url()}, now.Add(13*time.Minute))
	second.failDetails, second.updatedAt = true, "u3"
	if got := names(eventsSince(t, now.Add(15*time.Minute))); !slices.Equal(got, []string{"blocked infra#2", "blocked_automerge infra#2"}) {
		t.Errorf("held though GitHub couldn't be read: %v", got)
	}
	if s, _ := LoadPRWatchState(); s.PRs[second.url()].ready() {
		t.Error("and never saved as ready")
	}
	second.failDetails = false

	// The prerequisite merges first, but the merged search hasn't caught up
	// when the dependent merges too: GitHub is asked, and nothing is early.
	at := now.Add(16 * time.Minute)
	first.state, first.mergedAt = "merged", at
	second.state, second.mergedAt = "merged", at.Add(12*time.Second)
	g.hideMerged = first.url()
	got := names(eventsSince(t, now.Add(18*time.Minute)))
	if slices.Contains(got, "merged_early infra#2") || !slices.Contains(got, "merged infra#2") || !slices.Contains(got, "merged infra#1") {
		t.Errorf("merged in order, whatever the search shows: %v", got)
	}
}

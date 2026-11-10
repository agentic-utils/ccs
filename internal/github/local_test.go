package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// localWorld points the transcripts at a fresh folder and forgets what was read.
func localWorld(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := transcript.GetProjectsDir
	transcript.GetProjectsDir = func() string { return dir }
	forget := func() {
		localCache.Lock()
		localCache.files = map[string]*localFile{}
		localCache.Unlock()
	}
	forget()
	t.Cleanup(func() { transcript.GetProjectsDir = old; forget() })
	return dir
}

// say appends lines to a session's transcript (rel under the projects folder).
func say(t *testing.T, rel string, lines ...string) {
	t.Helper()
	path := filepath.Join(transcript.GetProjectsDir(), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(strings.Join(lines, "\n") + "\n")
}

// ran is a Bash call's line and answered its result's, as Claude Code writes them.
func ran(id, cmd string, at time.Time) string {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"tool_use","id":%q,"name":"Bash","input":%s}]}}`, at.UTC().Format(time.RFC3339), id, in)
}

func answered(id, out string, failed bool, at time.Time) string {
	text, _ := json.Marshal(out)
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":%t,"content":%s}]}}`, at.UTC().Format(time.RFC3339), id, failed, text)
}

func prLink(u string) string { return fmt.Sprintf(`{"type":"pr-link","prUrl":%q}`, u) }

func named(name string) string {
	return fmt.Sprintf(`{"type":"custom-title","customTitle":%q}`, name)
}

func TestGhActsReadsCreateAndMergeCommands(t *testing.T) {
	type want struct {
		merge, auto, bare, disable, mixed bool
		urls                              []string
		nums                              []int
	}
	for cmd, wants := range map[string][]want{
		"gh pr create --fill":                                 {{}},
		"gh pr merge 12 --merge":                              {{merge: true, nums: []int{12}}},
		"gh pr merge --auto --merge":                          {{merge: true, auto: true, bare: true}},
		`gh pr merge "$PR" --merge 2>&1 | tail -1`:            {{merge: true, bare: true}},
		"gh pr merge https://github.com/o/r/pull/3 --squash":  {{merge: true, urls: []string{"https://github.com/o/r/pull/3"}}},
		"gh pr merge 7 --repo o/r --merge --delete-branch":    {{merge: true, urls: []string{"https://github.com/o/r/pull/7"}}},
		"gh pr merge -R o/r --match-head-commit abc123 7":     {{merge: true, urls: []string{"https://github.com/o/r/pull/7"}}},
		"gh pr merge --repo=o/r 7 --body 'ship it' --merge":   {{merge: true, urls: []string{"https://github.com/o/r/pull/7"}}},
		"gh pr create --fill && gh pr merge --auto --merge":   {{}, {merge: true, auto: true, bare: true}},
		"gh pr merge 1 --merge; gh pr merge 2 --merge":        {{merge: true, nums: []int{1}}, {merge: true, nums: []int{2}}},
		"gh pr view 5 --json state && git log --merges":       nil,
		"gh pr merge 7 --disable-auto":                        {{merge: true, disable: true, nums: []int{7}}},
		"gh pr create --fill; gh pr view 3 --json url":        {{mixed: true}},
		"gh pr create --fill || gh pr create --draft --fill":  {{mixed: true}},
		"cd /tmp && gh pr checks 9 --watch && gh pr merge 9 ": {{merge: true, nums: []int{9}}},
	} {
		got := ghActs(cmd)
		if len(got) != len(wants) {
			t.Errorf("%q: %d acts, want %d: %+v", cmd, len(got), len(wants), got)
			continue
		}
		for i, w := range wants {
			a := got[i]
			if a.Merge != w.merge || a.Auto != w.auto || a.Bare != w.bare || a.Disable != w.disable || a.Mixed != w.mixed || !slices.Equal(a.URLs, w.urls) || !slices.Equal(a.Nums, w.nums) {
				t.Errorf("%q: act %d is %+v, want %+v", cmd, i, a, w)
			}
		}
	}
}

func TestLocalActsFindWhoOpenedAPR(t *testing.T) {
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	pr := "https://github.com/o/r/pull/1"
	say(t, "p/s-alpha.jsonl", named("alpha"),
		ran("t1", "gh pr create --fill", now), answered("t1", "Creating pull request\n"+pr+"\n", false, now.Add(3*time.Second)))
	// beta only looked at it: no gh pr create, so never its opener.
	say(t, "p/s-beta.jsonl", ran("t1", "gh pr view "+pr, now), answered("t1", pr, false, now), prLink(pr))
	l := LocalActs(now)
	created := now.Add(2 * time.Second).UTC().Format(time.RFC3339)
	if by := l.opener(&WatchedPR{URL: pr, CreatedAt: created}); by == nil || by.Session != "s-alpha" || by.Name != "alpha" {
		t.Errorf("the session whose gh pr create printed it opened it: %+v", by)
	}
	if by := l.opener(&WatchedPR{URL: pr}); by == nil || by.Session != "s-alpha" {
		t.Errorf("with no creation time the address alone decides: %+v", by)
	}
	// A create that printed the address of a PR made at another time (a list
	// in the same command) didn't open it.
	if by := l.opener(&WatchedPR{URL: pr, CreatedAt: now.Add(-time.Hour).UTC().Format(time.RFC3339)}); by != nil {
		t.Errorf("a PR created an hour before the command wasn't opened by it: %+v", by)
	}
	if by := l.opener(&WatchedPR{URL: "https://github.com/o/r/pull/2"}); by != nil {
		t.Errorf("nobody here opened #2: %+v", by)
	}
	// A subagent's gh pr create is its session's.
	sub := "https://github.com/o/r/pull/9"
	say(t, "p/s-alpha/subagents/agent-a1.jsonl", ran("t1", "gh pr create --fill", now), answered("t1", sub, false, now))
	if by := LocalActs(now).opener(&WatchedPR{URL: sub}); by == nil || by.Session != "s-alpha" || by.Name != "alpha" {
		t.Errorf("a subagent's PR belongs to the session that started it: %+v", by)
	}
	// A create beside another gh command: the address printed may be the
	// other's (the create may not even have run), so it opened nothing.
	seen := "https://github.com/o/r/pull/7"
	say(t, "p/s-beta.jsonl", ran("t3", "false && gh pr create --fill; gh pr view 7 --json url --jq .url", now), answered("t3", seen, false, now))
	if by := LocalActs(now).opener(&WatchedPR{URL: seen}); by != nil {
		t.Errorf("an address another gh command may have printed isn't a create's: %+v", by)
	}
	// A refused or failed create opened nothing.
	failed := "https://github.com/o/r/pull/8"
	say(t, "p/s-beta.jsonl", ran("t2", "gh pr create --fill", now), answered("t2", "a pull request already exists:\n"+failed, true, now))
	if by := LocalActs(now).opener(&WatchedPR{URL: failed}); by != nil {
		t.Errorf("a failed gh pr create opened nothing: %+v", by)
	}
}

func TestLocalActsFindHowAPRMerged(t *testing.T) {
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	p := &WatchedPR{URL: "https://github.com/o/r/pull/4", Repo: "o/r", Number: 4}
	merged := now.Add(time.Minute)
	if by, how := LocalActs(now).merger(p, merged); by != nil || how != MergeNotLocal {
		t.Errorf("no session merged it: %+v %q", by, how)
	}
	// alpha turned on auto-merge earlier; gh named the current branch, and
	// the PR is one of alpha's.
	say(t, "p/s-alpha.jsonl", named("alpha"), prLink(p.URL),
		ran("t1", "gh pr merge --auto --merge", now), answered("t1", "", false, now.Add(time.Second)))
	if by, how := LocalActs(now).merger(p, merged); by == nil || by.Name != "alpha" || how != MergeAuto {
		t.Errorf("auto-merge alpha turned on: %+v %q", by, how)
	}
	// beta ran gh pr merge 4 as it merged: beta merged it, whatever was armed.
	say(t, "p/s-beta.jsonl", named("beta"), prLink(p.URL),
		ran("t1", "gh pr merge 4 --merge", merged.Add(-2*time.Second)), answered("t1", "", false, merged.Add(time.Second)))
	if by, how := LocalActs(now).merger(p, merged); by == nil || by.Name != "beta" || how != MergeLocal {
		t.Errorf("the session whose gh pr merge ran when it merged: %+v %q", by, how)
	}
	// The same command at another time, a refused one, and a number that is
	// another repo's PR don't account for a merge.
	other := &WatchedPR{URL: "https://github.com/o/other/pull/4", Repo: "o/other", Number: 4}
	if by, how := LocalActs(now).merger(other, merged); by != nil || how != MergeNotLocal {
		t.Errorf("gh pr merge 4 isn't a merge of another repo's #4 that merged at the same moment: %+v %q", by, how)
	}
	if by, how := LocalActs(now).merger(p, merged.Add(time.Hour)); by == nil || by.Name != "alpha" || how != MergeAuto {
		t.Errorf("a gh pr merge an hour before it merged didn't merge it: %+v %q", by, how)
	}
	// Auto-merge turned off again: a later merge isn't alpha's auto-merge,
	// and turning it off merges nothing.
	say(t, "p/s-alpha.jsonl", ran("t9", "gh pr merge --disable-auto", merged.Add(2*time.Hour)), answered("t9", "", false, merged.Add(2*time.Hour)))
	if by, how := LocalActs(now).merger(p, merged.Add(2*time.Hour)); by != nil || how != MergeNotLocal {
		t.Errorf("auto-merge turned off, then merged elsewhere: %+v %q", by, how)
	}
	localWorld(t)
	say(t, "p/s-gamma.jsonl", ran("t1", "gh pr merge 4 --merge", merged), answered("t1", "Permission for this action was denied", true, merged))
	if by, how := LocalActs(now).merger(p, merged); by != nil || how != MergeNotLocal {
		t.Errorf("a refused gh pr merge merged nothing: %+v %q", by, how)
	}
	// A gh pr merge that hasn't come back (waiting on a permission prompt)
	// merged nothing yet: not said either way until it does.
	say(t, "p/s-gamma.jsonl", prLink(p.URL), ran("t2", "gh pr merge 4 --merge", merged))
	if by, how := LocalActs(now).merger(p, merged.Add(time.Minute)); by != nil || how != "" {
		t.Errorf("a merge command still running decides nothing: %+v %q", by, how)
	}
	say(t, "p/s-gamma.jsonl", answered("t2", "", false, merged.Add(2*time.Minute)))
	if by, how := LocalActs(now).merger(p, merged.Add(time.Minute)); by == nil || by.Session != "s-gamma" || how != MergeLocal {
		t.Errorf("once it comes back, it merged it: %+v %q", by, how)
	}
	if by, how := LocalActs(now).merger(p, time.Time{}); by != nil || how != "" {
		t.Errorf("with no merge time nothing is said: %+v %q", by, how)
	}
}

func TestLocalActsReadOnlyWhatWasAppended(t *testing.T) {
	dir := localWorld(t)
	now := time.Now().Truncate(time.Second)
	a, b := "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"
	say(t, "p/s-alpha.jsonl", ran("t1", "gh pr create --fill", now))
	if by := LocalActs(now).opener(&WatchedPR{URL: a}); by != nil {
		t.Errorf("a call still running has opened nothing: %+v", by)
	}
	say(t, "p/s-alpha.jsonl", answered("t1", a, false, now)) // its result, on a later read
	if by := LocalActs(now).opener(&WatchedPR{URL: a}); by == nil {
		t.Error("the result of a call read earlier is matched to it")
	}
	path := filepath.Join(dir, "p", "s-alpha.jsonl")
	localCache.Lock()
	off := localCache.files[path].off
	localCache.Unlock()
	if info, _ := os.Stat(path); off != info.Size() {
		t.Errorf("read up to %d of %d", off, info.Size())
	}
	// A transcript nobody wrote to for days isn't read, and is forgotten.
	say(t, "p/s-old.jsonl", ran("t1", "gh pr create --fill", now), answered("t1", b, false, now))
	old := now.Add(-localLookback - time.Hour)
	os.Chtimes(filepath.Join(dir, "p", "s-old.jsonl"), old, old)
	if by := LocalActs(now).opener(&WatchedPR{URL: b}); by != nil {
		t.Errorf("an old transcript isn't read: %+v", by)
	}
	os.Chtimes(path, old, old)
	LocalActs(now)
	localCache.Lock()
	n := len(localCache.files)
	localCache.Unlock()
	if n != 0 {
		t.Errorf("transcripts gone quiet are forgotten: %d kept", n)
	}
}

// eventsSince runs one pass and returns its events, decoded.
func eventsSince(t *testing.T, now time.Time) []PREvent {
	t.Helper()
	before, _ := os.ReadFile(PREventsPath())
	if err := WatchPass("me", "o", now); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(PREventsPath())
	var out []PREvent
	for _, line := range bytes.Split(bytes.TrimSpace(after[len(before):]), []byte("\n")) {
		var e PREvent
		if len(line) > 0 && json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func TestWatchClaimsAPRForTheSessionThatOpenedIt(t *testing.T) {
	g := watchWorld(t)
	live := claimEnv(t)
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	passEvents(t, now) // the watch starts with nothing open

	mine := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean", createdAt: now.Add(time.Minute)}
	routine := &fakePR{repo: "o/a", number: 2, state: "open", head: "h2", mergeable: "clean", createdAt: now.Add(time.Minute)}
	pending(mine)
	pending(routine)
	g.prs = []*fakePR{mine, routine}
	say(t, "p/s-alpha.jsonl", ran("t1", "gh pr create --fill", now.Add(time.Minute)), answered("t1", mine.url(), false, now.Add(time.Minute)))

	events := eventsSince(t, now.Add(3*time.Minute))
	byPR := map[string]PREvent{}
	for _, e := range events {
		byPR[e.Event+" "+e.PR] = e
	}
	if e := byPR["new "+mine.url()]; e.Origin != OriginLocal || e.OpenedBy == nil || e.OpenedBy.Session != "s-alpha" {
		t.Errorf("new says which session opened it: %+v", e)
	}
	if e := byPR["new "+routine.url()]; e.Origin != OriginNotLocal || e.OpenedBy != nil {
		t.Errorf("new says no local session opened the other: %+v", e)
	}
	if e := byPR["claimed "+mine.url()]; !e.Auto || e.Session != "alpha" || e.SessionID != "s-alpha" || e.Head != "h1" {
		t.Errorf("the running session that opened it is given the claim: %+v", e)
	}
	if _, ok := byPR["claimed "+routine.url()]; ok {
		t.Error("a PR no local session opened is claimed for nobody")
	}
	c := LoadClaims().Claims[mine.url()]
	if len(c) != 1 || !c[0].Auto || c[0].Session != "s-alpha" || c[0].PID != os.Getppid() || c[0].ProcStart == "" {
		t.Errorf("the claim is the live session's: %+v", c)
	}
	rep, err := PRsList("me", "o", true, now.Add(4*time.Minute))
	if err != nil || len(rep.PRs) != 2 {
		t.Fatalf("prs: %+v %v", rep, err)
	}
	for _, r := range rep.PRs {
		switch r.PR {
		case mine.url():
			if r.Unowned || !slices.Equal(r.Owners, []string{"alpha"}) || r.Origin != OriginLocal {
				t.Errorf("owned by the session that opened it: %+v", r)
			}
		case routine.url():
			if !r.Unowned || r.Origin != OriginNotLocal {
				t.Errorf("unowned, and not a local session's: %+v", r)
			}
		}
	}
	if text := rep.Text(); !strings.Contains(text, "UNOWNED: no local session opened it") {
		t.Errorf("the list says why it's unowned:\n%s", text)
	}

	// Claimed once: nothing more is said while it stays owned.
	if got := eventsSince(t, now.Add(6*time.Minute)); len(got) != 0 {
		t.Errorf("no second claim: %+v", got)
	}
	// Released by its session: not claimed back.
	if _, err := Claim(mine.url(), true, now); err != nil {
		t.Fatal(err)
	}
	if got := eventsSince(t, now.Add(9*time.Minute)); len(got) != 0 || len(LoadClaims().Claims) != 0 {
		t.Errorf("a released PR stays released: %+v %+v", got, LoadClaims().Claims)
	}
	// The session gone: nobody to claim it for, and the list says who opened it.
	delete(live, os.Getppid())
	rep, _ = PRsList("me", "o", true, now.Add(10*time.Minute))
	if text := rep.Text(); !strings.Contains(text, "UNOWNED: s-alpha opened it and isn't running") {
		t.Errorf("an unowned PR names the session that opened it:\n%s", text)
	}
	// The release ends with the PR.
	mine.state, mine.mergedAt = "merged", now.Add(11*time.Minute)
	eventsSince(t, now.Add(12*time.Minute))
	if rel := LoadClaims().Released; len(rel) != 0 {
		t.Errorf("a release is forgotten once its PR ends: %+v", rel)
	}
}

func TestWatchLeavesAnOwnedPRAndCanBeToldNotToClaim(t *testing.T) {
	g := watchWorld(t)
	live := claimEnv(t)
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	passEvents(t, now)
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	pending(a)
	g.prs = []*fakePR{a}
	say(t, "p/s-alpha.jsonl", ran("t1", "gh pr create --fill", now), answered("t1", a.url(), false, now))

	AutoClaim = false
	t.Cleanup(func() { AutoClaim = true })
	if got := passEvents(t, now.Add(3*time.Minute)); !slices.Equal(got, []string{"new " + a.url()}) || len(LoadClaims().Claims) != 0 {
		t.Errorf("--no-auto-claim claims nothing: %v", got)
	}
	AutoClaim = true
	// beta took it over by hand: the opener isn't added beside it.
	// (alpha runs on as another process; the claiming one is beta's.)
	alpha := live[os.Getppid()]
	alpha.PID = 4242
	live[4242] = alpha
	live[os.Getppid()] = PRClaim{Session: "s-beta", Name: "beta", PID: os.Getppid(), ProcStart: "Tue Nov  3 08:00:00 2026"}
	if _, err := Claim(a.url(), false, now); err != nil {
		t.Fatal(err)
	}
	if got := passEvents(t, now.Add(6*time.Minute)); len(got) != 0 {
		t.Errorf("an owned PR is left alone: %v", got)
	}
	if c := LoadClaims().Claims[a.url()]; len(c) != 1 || c[0].Session != "s-beta" {
		t.Errorf("one owner: %+v", c)
	}
}

func TestWatchLearnsLateWhoOpenedAPR(t *testing.T) {
	g := watchWorld(t)
	claimEnv(t)
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	passEvents(t, now)
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	pending(a)
	g.prs = []*fakePR{a}
	// GitHub lists the PR before the gh pr create that made it has come back.
	say(t, "p/s-alpha.jsonl", ran("t1", "gh pr create --fill", now))
	if got := eventsSince(t, now.Add(3*time.Minute)); len(got) != 1 || got[0].Event != "new" || got[0].Origin != OriginNotLocal {
		t.Errorf("not known to be local yet: %+v", got)
	}
	say(t, "p/s-alpha.jsonl", answered("t1", a.url(), false, now.Add(4*time.Minute)))
	got := eventsSince(t, now.Add(6*time.Minute))
	if len(got) != 1 || got[0].Event != "claimed" || got[0].SessionID != "s-alpha" {
		t.Errorf("the late result gives it its opener and its owner: %+v", got)
	}
	if s, _ := LoadPRWatchState(); s.PRs[a.url()].Origin != OriginLocal || s.PRs[a.url()].OpenedBy == nil {
		t.Errorf("the state is corrected: %+v", s.PRs[a.url()])
	}
	// A release of a PR this watch doesn't cover is kept.
	if err := WithClaims(func(c *ClaimsFile) bool {
		c.Released = map[string][]string{"https://github.com/other/r/pull/5": {"s-alpha"}}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	eventsSince(t, now.Add(9*time.Minute))
	if rel := LoadClaims().Released; len(rel) != 1 {
		t.Errorf("a release outside this watch's PRs stays: %+v", rel)
	}
	// ccs prs asking GitHub itself (the state gone stale) still says who opened it.
	FetchOpenPRs = func(string, string) (map[string]*WatchedPR, error) {
		return map[string]*WatchedPR{a.url(): {URL: a.url(), Repo: "o/a", Number: 1, State: "open"}}, nil
	}
	rep, err := PRsList("me", "o", false, now.Add(time.Hour))
	if err != nil || len(rep.PRs) != 1 || rep.PRs[0].Origin != OriginLocal || rep.PRs[0].OpenedBy == nil {
		t.Errorf("a fresh list keeps the opener: %+v %v", rep, err)
	}
}

func TestClaimIsAnEvent(t *testing.T) {
	watchWorld(t)
	claimEnv(t)
	now := time.Now()
	if _, err := Claim(claimA, false, now); err != nil {
		t.Fatal(err)
	}
	Claim(claimA, false, now) // again: the same owner, nothing new to say
	data, _ := os.ReadFile(PREventsPath())
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var e PREvent
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &e) != nil {
		t.Fatalf("one claimed event: %q", data)
	}
	want := PREvent{At: e.At, Event: "claimed", PR: claimA, Repo: "o/r", Number: 1, Session: "alpha", SessionID: "s-alpha"}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("got %+v, want %+v", e, want)
	}
}

func TestWatchSaysHowAPRMerged(t *testing.T) {
	g := watchWorld(t)
	claimEnv(t)
	localWorld(t)
	now := time.Now().Truncate(time.Second)
	byHand := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	auto := &fakePR{repo: "o/a", number: 2, state: "open", head: "h2", mergeable: "clean"}
	browser := &fakePR{repo: "o/a", number: 3, state: "open", head: "h3", mergeable: "clean"}
	gone := &fakePR{repo: "o/a", number: 4, state: "open", head: "h4", mergeable: "clean"}
	g.prs = []*fakePR{byHand, auto, browser, gone}
	passEvents(t, now)

	at := now.Add(time.Minute)
	say(t, "p/s-alpha.jsonl", named("alpha"), prLink(auto.url()), prLink(byHand.url()),
		ran("t1", "gh pr merge --auto --merge", now), answered("t1", "", false, now),
		ran("t2", "gh pr merge 1 --merge", at.Add(-time.Second)), answered("t2", "", false, at.Add(time.Second)),
		ran("t3", "gh pr merge 4 --repo o/a --merge", at.Add(-time.Second)), answered("t3", "", false, at.Add(time.Second)))
	for _, p := range []*fakePR{byHand, auto, browser, gone} {
		p.state, p.mergedAt = "merged", at
	}
	// #4 merged without the merged search listing it: asked about directly.
	g.hideMerged = gone.url()
	got := map[string]PREvent{}
	for _, e := range eventsSince(t, now.Add(3*time.Minute)) {
		if e.Event == "merged" {
			got[e.PR] = e
		}
	}
	for p, want := range map[*fakePR][2]string{byHand: {MergeLocal, "alpha"}, auto: {MergeAuto, "alpha"}, browser: {MergeNotLocal, ""}, gone: {MergeLocal, "alpha"}} {
		e, who := got[p.url()], ""
		if e.MergedBy != nil {
			who = e.MergedBy.Name
		}
		if e.Merge != want[0] || who != want[1] {
			t.Errorf("#%d merged %q by %q, want %q by %q", p.number, e.Merge, who, want[0], want[1])
		}
	}
	rows := MergedList()
	if len(rows) != 4 {
		t.Fatalf("ccs prs --merged lists what merged: %+v", rows)
	}
	text := mergedText(rows)
	for _, want := range []string{"a#1", "merged by alpha", "auto-merge, turned on by alpha", "not merged by a local session"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q missing from:\n%s", want, text)
		}
	}
	var out, errs bytes.Buffer
	if RunPRs([]string{"--merged", "--json"}, &out, &errs) != 0 {
		t.Fatalf("ccs prs --merged --json: %s", errs.String())
	}
	var decoded []MergedRow
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || len(decoded) != 4 {
		t.Errorf("JSON rows: %v %s", err, out.String())
	}
}

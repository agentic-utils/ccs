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

// fakePR is one PR in fakeGitHub.
type fakePR struct {
	repo        string
	number      int
	state       string // open, merged, closed
	head        string
	draft       bool
	mergeable   string
	runs        [][3]string // name, status, conclusion
	reviews     [][2]string // login, state
	mergedAt    time.Time
	failDetails bool
	unresolved  int  // review threads not resolved
	threadsFail bool // the GraphQL thread count fails
}

func (p *fakePR) url() string { return fmt.Sprintf("https://github.com/%s/pull/%d", p.repo, p.number) }

// fakeGitHub answers the gh calls the watcher makes; mergedSince records the
// merged-at filters it was asked for.
type fakeGitHub struct {
	sync.Mutex
	prs         []*fakePR
	searchFails bool
	mergedSince []string
}

func (g *fakeGitHub) run(args ...string) ([]byte, error) {
	g.Lock()
	defer g.Unlock()
	if args[0] == "search" {
		if g.searchFails {
			return nil, errors.New("rate limited")
		}
		var rows []map[string]any
		filter := args[len(args)-2:]
		for _, p := range g.prs {
			switch filter[0] {
			case "--state":
				if p.state != "open" {
					continue
				}
			case "--merged-at":
				since, _ := time.Parse(time.RFC3339, strings.TrimPrefix(filter[1], ">="))
				if p.state != "merged" || p.mergedAt.Before(since) {
					continue
				}
			}
			rows = append(rows, map[string]any{"url": p.url(), "title": "t" + fmt.Sprint(p.number), "number": p.number, "isDraft": p.draft, "repository": map[string]string{"nameWithOwner": p.repo}})
		}
		if filter[0] == "--merged-at" {
			g.mergedSince = append(g.mergedSince, filter[1])
		}
		rows = append(rows, map[string]any{"url": "  "}) // a blank row is dropped
		return json.Marshal(rows)
	}
	if args[1] == "graphql" {
		var owner, name, number string
		for i, a := range args {
			if a == "-F" {
				k, v, _ := strings.Cut(args[i+1], "=")
				switch k {
				case "owner":
					owner = v
				case "name":
					name = v
				case "number":
					number = v
				}
			}
		}
		for _, p := range g.prs {
			if p.repo == owner+"/"+name && fmt.Sprint(p.number) == number {
				if p.threadsFail {
					return nil, errors.New("RESOURCE_LIMITS_EXCEEDED")
				}
				var nodes []map[string]bool
				for i := range p.unresolved + 1 { // one resolved thread besides
					nodes = append(nodes, map[string]bool{"isResolved": i == p.unresolved})
				}
				return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"nodes": nodes}}}}})
			}
		}
		return nil, fmt.Errorf("unexpected graphql %v", args)
	}
	path := args[1]
	for _, p := range g.prs {
		base := fmt.Sprintf("repos/%s/pulls/%d", p.repo, p.number)
		commit := fmt.Sprintf("repos/%s/commits/%s/", p.repo, p.head)
		switch {
		case p.failDetails && strings.HasPrefix(path, "repos/"+p.repo):
			return nil, errors.New("502")
		case path == base:
			merged := ""
			if p.state == "merged" {
				merged = p.mergedAt.Format(time.RFC3339)
			}
			state := "open"
			if p.state != "open" {
				state = "closed"
			}
			return json.Marshal(map[string]any{"draft": p.draft, "mergeable_state": p.mergeable, "head": map[string]string{"sha": p.head}, "state": state, "merged_at": merged})
		case strings.HasPrefix(path, commit+"check-runs"):
			var runs []map[string]string
			for _, r := range p.runs {
				runs = append(runs, map[string]string{"name": r[0], "status": r[1], "conclusion": r[2]})
			}
			return json.Marshal(map[string]any{"check_runs": runs})
		case path == commit+"status":
			return []byte(`{"statuses":[]}`), nil
		case strings.HasPrefix(path, base+"/reviews"):
			var rs []map[string]any
			for _, r := range p.reviews {
				rs = append(rs, map[string]any{"user": map[string]string{"login": r[0]}, "state": r[1]})
			}
			return json.Marshal(rs)
		}
	}
	return nil, fmt.Errorf("unexpected gh %v", args)
}

// watchWorld stubs gh with a fake GitHub and points the watcher's files at a
// fresh directory.
func watchWorld(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{}
	oldRun, oldSettings, oldLog := ghRun, settingsPath, updateLogPath
	dir := t.TempDir()
	ghRun, settingsPath, updateLogPath = g.run, dir+"/settings.json", dir+"/update.log"
	t.Cleanup(func() { ghRun, settingsPath, updateLogPath = oldRun, oldSettings, oldLog })
	return g
}

// passEvents runs one pass at now and returns its events as "event url" lines.
func passEvents(t *testing.T, now time.Time) []string {
	t.Helper()
	before, _ := os.ReadFile(prEventsPath())
	if err := watchPass("me", "o", now); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(prEventsPath())
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(after[len(before):])), "\n") {
		if line == "" {
			continue
		}
		var e prEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		got = append(got, strings.TrimSpace(e.Event+" "+e.PR))
	}
	return got
}

func green(p *fakePR)   { p.runs = [][3]string{{"test", "completed", "success"}} }
func pending(p *fakePR) { p.runs = [][3]string{{"test", "in_progress", ""}} }

func TestWatchSeedsQuietly(t *testing.T) {
	g := watchWorld(t)
	now := time.Now()
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	green(a)
	old := &fakePR{repo: "o/a", number: 2, state: "merged", head: "h2", mergedAt: now.Add(-time.Hour)}
	g.prs = []*fakePR{a, old}
	if got := passEvents(t, now); !slices.Equal(got, []string{"watch_started"}) {
		t.Errorf("the first pass only says it started: %v", got)
	}
	if got := passEvents(t, now.Add(3*time.Minute)); len(got) != 0 {
		t.Errorf("nothing changed, nothing reported, and a PR green before the start isn't asked about: %v", got)
	}
	s, ok := loadPRWatchState()
	if !ok || s.Updated.IsZero() || s.PRs[a.url()].Checks != "success" {
		t.Errorf("state is readable with its freshness: %+v", s)
	}
}

func TestWatchTransitions(t *testing.T) {
	g := watchWorld(t)
	now := time.Now()
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	pending(a)
	g.prs = []*fakePR{a}
	passEvents(t, now)
	step := func(want ...string) {
		t.Helper()
		now = now.Add(3 * time.Minute)
		got := passEvents(t, now)
		for i := range got {
			got[i] = strings.Fields(got[i])[0]
		}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	b := &fakePR{repo: "o/b", number: 7, state: "open", head: "b1", mergeable: "clean"}
	pending(b)
	g.prs = append(g.prs, b)
	step("new")
	a.runs = [][3]string{{"lint", "completed", "success"}, {"test", "completed", "failure"}}
	step("checks_failing")
	step() // still failing, same head: once
	a.head = "h2"
	a.runs = [][3]string{{"test", "completed", "failure"}}
	step("checks_failing") // a new commit that fails again
	green(a)
	step("checks_passed", "needs_review")
	a.mergeable = "unknown"
	step() // mergeability still being worked out: no change
	a.mergeable = "dirty"
	step("conflicts")
	a.mergeable = "unknown"
	step() // keeps the conflict, no repeat
	a.mergeable = "clean"
	a.reviews = [][2]string{{"rev", "COMMENTED"}, {"rev", "APPROVED"}}
	step("ready")
	a.head = "h3"
	a.reviews = [][2]string{{"rev", "DISMISSED"}}
	step("needs_review") // new head, approval dismissed: asked again
	a.state, a.mergedAt = "merged", now
	b.state = "closed"
	step("merged", "closed")
	step() // never again
}

func TestWatchMergedWindow(t *testing.T) {
	g := watchWorld(t)
	start := time.Now()
	g.prs = []*fakePR{{repo: "o/a", number: 1, state: "merged", mergedAt: start.Add(-48 * time.Hour)}}
	passEvents(t, start)
	// days later, a merge from before the watch started is still not news,
	// and the search never looks back past the window.
	later := start.Add(5 * 24 * time.Hour)
	if got := passEvents(t, later); len(got) != 0 {
		t.Errorf("an old merge is never new: %v", got)
	}
	last := g.mergedSince[len(g.mergedSince)-1]
	if since, _ := time.Parse(time.RFC3339, strings.TrimPrefix(last, ">=")); since.Before(later.Add(-mergedWindow - time.Second)) {
		t.Errorf("merged search looks back at most mergedWindow: %s", last)
	}
	// a PR opened and merged between two passes is reported once.
	g.prs = append(g.prs, &fakePR{repo: "o/a", number: 3, state: "merged", mergedAt: later.Add(time.Minute)})
	if got := passEvents(t, later.Add(3*time.Minute)); !slices.Equal(got, []string{"merged https://github.com/o/a/pull/3"}) {
		t.Errorf("got %v", got)
	}
	if got := passEvents(t, later.Add(6*time.Minute)); len(got) != 0 {
		t.Errorf("reported once: %v", got)
	}
}

func TestWatchFailuresChangeNothing(t *testing.T) {
	g := watchWorld(t)
	now := time.Now()
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	pending(a)
	g.prs = []*fakePR{a}
	passEvents(t, now)
	g.searchFails = true
	if err := watchPass("me", "o", now.Add(time.Minute)); err == nil {
		t.Error("a failed search is an error")
	}
	g.searchFails = false
	a.failDetails = true
	green(a)
	if got := passEvents(t, now.Add(2*time.Minute)); len(got) != 0 {
		t.Errorf("a PR that couldn't be read reports nothing: %v", got)
	}
	a.failDetails = false
	if got := passEvents(t, now.Add(3*time.Minute)); !slices.Contains(got, "checks_passed "+a.url()) {
		t.Errorf("the change is reported once it can be read, not lost: %v", got)
	}
}

func TestWatchRestartDoesntReplay(t *testing.T) {
	g := watchWorld(t)
	now := time.Now()
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	pending(a)
	g.prs = []*fakePR{a}
	passEvents(t, now)
	green(a)
	passEvents(t, now.Add(time.Minute))
	// a new process reads the same state file: nothing is reported again
	if got := passEvents(t, now.Add(2*time.Minute)); len(got) != 0 {
		t.Errorf("restart replays nothing: %v", got)
	}
	// a change while it was down is reported, not lost
	a.state, a.mergedAt = "merged", now.Add(time.Hour)
	if got := passEvents(t, now.Add(2*time.Hour)); !slices.Equal(got, []string{"merged " + a.url()}) {
		t.Errorf("got %v", got)
	}
}

func TestWatchSingleInstance(t *testing.T) {
	watchWorld(t)
	f, err := lockPRWatch()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockPRWatch(); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a second watcher is refused: %v", err)
	}
	f.Close()
	g, err := lockPRWatch()
	if err != nil {
		t.Errorf("free again once the first exits: %v", err)
	}
	g.Close()
}

func TestWatchAnyFailedCheckIsNotReady(t *testing.T) {
	g := watchWorld(t)
	now := time.Now()
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "unstable", reviews: [][2]string{{"rev", "APPROVED"}}}
	b := &fakePR{repo: "o/b", number: 2, state: "open", head: "b1", mergeable: "unstable"}
	pending(a)
	pending(b)
	g.prs = []*fakePR{a, b}
	passEvents(t, now)
	// GitHub calls these mergeable (unstable), but setup failed and the
	// rest were skipped: not ready, never asked to review.
	for _, p := range []*fakePR{a, b} {
		p.runs = [][3]string{{"setup", "completed", "failure"}, {"build", "completed", "skipped"}, {"lint", "completed", "success"}}
	}
	got := passEvents(t, now.Add(3*time.Minute))
	if !slices.Equal(got, []string{"checks_failing " + a.url(), "checks_failing " + b.url()}) {
		t.Errorf("any failed check is checks_failing, never ready or needs_review: %v", got)
	}
}

func TestWatchUnresolvedThreads(t *testing.T) {
	g := watchWorld(t)
	pr := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean", runs: [][3]string{{"test", "completed", "success"}}}
	g.prs = []*fakePR{pr}
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	pass := func() []string {
		t.Helper()
		now = now.Add(3 * time.Minute)
		got := passEvents(t, now)
		for i := range got {
			got[i] = strings.Fields(got[i])[0]
		}
		return got
	}
	pass() // seeds: green, no review, no thread, so no event even then

	pr.unresolved = 2
	pr.head = "h2" // a new head: needs_review would fire again, but threads are open
	if got := pass(); !slices.Equal(got, []string{"threads_unresolved"}) {
		t.Errorf("open threads: %v, want threads_unresolved and no needs_review", got)
	}
	if data, _ := os.ReadFile(prEventsPath()); !strings.Contains(string(data), `"event":"threads_unresolved","pr":"https://github.com/o/a/pull/1"`) || !strings.Contains(string(data), `"unresolved":2`) {
		t.Errorf("the event carries the count: %s", data)
	}
	if st, _ := loadPRWatchState(); st.PRs[pr.url()].needsReview() {
		t.Error("a PR with unresolved threads never needs review")
	}

	pr.threadsFail = true // unknown: no change, no events, count kept
	if got := pass(); len(got) != 0 {
		t.Errorf("a failed count changes nothing: %v", got)
	}
	if st, _ := loadPRWatchState(); st.PRs[pr.url()].Threads == nil || *st.PRs[pr.url()].Threads != 2 {
		t.Errorf("the last count is kept: %+v", st.PRs[pr.url()])
	}

	pr.threadsFail, pr.unresolved = false, 0
	if got := pass(); !slices.Equal(got, []string{"threads_resolved", "needs_review"}) {
		t.Errorf("resolved: %v, want threads_resolved then needs_review", got)
	}

	pr.runs = [][3]string{{"test", "completed", "failure"}}
	pr.head, pr.unresolved = "h3", 1 // not counted while checks fail
	if got := pass(); !slices.Equal(got, []string{"checks_failing"}) {
		t.Errorf("failing checks: %v", got)
	}
}

func TestNeedsReviewNeedsAThreadCount(t *testing.T) {
	zero, one := 0, 1
	p := watchedPR{State: "open", Checks: "success", Review: "required"}
	if p.needsReview() {
		t.Error("threads never counted: not yet")
	}
	if p.Threads = &one; p.needsReview() {
		t.Error("an unresolved thread: no")
	}
	if p.Threads = &zero; !p.needsReview() {
		t.Error("green, counted, none open: yes")
	}
	if g, why := p.status(); g != prWaiting || why != "review required" {
		t.Errorf("ccs prs: %d %q", g, why)
	}
	p.Threads = &one
	if g, why := p.status(); g != prFix || why != "1 unresolved thread" {
		t.Errorf("ccs prs shows the threads: %d %q", g, why)
	}
}

func TestWatchDefaultsToTheGhUserInEveryOwner(t *testing.T) {
	old, oldRun := ghUser, ghRun
	t.Cleanup(func() { ghUser, ghRun = old, oldRun })
	ghUser = func() (string, error) { return "someone", nil }
	a := ""
	if err := defaultAuthor(&a); err != nil || a != "someone" {
		t.Errorf("author defaults to the gh user: %q %v", a, err)
	}
	a = "given"
	if defaultAuthor(&a); a != "given" {
		t.Errorf("--author wins: %q", a)
	}
	ghUser = func() (string, error) { return "", errors.New("not logged in") }
	a = ""
	if err := defaultAuthor(&a); err == nil {
		t.Error("no gh user: accepted")
	}
	var args []string
	ghRun = func(a ...string) ([]byte, error) { args = a; return []byte("[]"), nil }
	searchPRs("someone", "", "--state", "open")
	if slices.Contains(args, "--owner") {
		t.Errorf("no owner searches every owner: %v", args)
	}
	searchPRs("someone", "o", "--state", "open")
	if i := slices.Index(args, "--owner"); i < 0 || args[i+1] != "o" {
		t.Errorf("--owner o: %v", args)
	}
}

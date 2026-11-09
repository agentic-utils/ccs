package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
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
	unresolved  int          // review threads not resolved, each last commented on by a reviewer
	threadsFail bool         // the GraphQL thread count fails
	threads     []fakeThread // when set, the unresolved threads instead of unresolved
	updatedAt   string       // what the search says; empty leaves it out
}

// fakeThread is an unresolved review thread: its id, last comment id and who wrote it.
type fakeThread struct {
	id   string
	last int64
	by   string
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
			rows = append(rows, map[string]any{"url": p.url(), "title": "t" + fmt.Sprint(p.number), "number": p.number, "isDraft": p.draft, "updatedAt": p.updatedAt, "repository": map[string]string{"nameWithOwner": p.repo}})
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
				comment := func(id int64, by string) map[string]any {
					return map[string]any{"nodes": []map[string]any{{"databaseId": id, "author": map[string]string{"login": by}}}}
				}
				nodes := []map[string]any{{"id": "resolved", "isResolved": true, "comments": comment(1, "reviewer")}} // one resolved thread besides
				threads := p.threads
				if threads == nil {
					for i := range p.unresolved {
						threads = append(threads, fakeThread{fmt.Sprintf("T%d", i), int64(100 + i), "reviewer"})
					}
				}
				for _, t := range threads {
					nodes = append(nodes, map[string]any{"id": t.id, "isResolved": false, "comments": comment(t.last, t.by)})
				}
				start := 0 // served 100 a page, the cursor being the next index
				for i, a := range args {
					if a == "-f" && strings.HasPrefix(args[i+1], "after=") {
						start, _ = strconv.Atoi(strings.TrimPrefix(args[i+1], "after="))
					}
				}
				end := min(start+100, len(nodes))
				more := end < len(nodes)
				return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
					"pageInfo": map[string]any{"hasNextPage": more, "endCursor": fmt.Sprint(end)}, "nodes": nodes[start:end]}}}}})
			}
		}
		return nil, fmt.Errorf("unexpected graphql %v", args)
	}
	path, query, _ := strings.Cut(args[1], "?")
	page := func(n int) (int, int) { // REST paging from per_page and page, as GitHub serves it
		vals, _ := url.ParseQuery(query)
		per, _ := strconv.Atoi(vals.Get("per_page"))
		pg, _ := strconv.Atoi(vals.Get("page"))
		if per == 0 {
			per = 30
		}
		pg = max(pg, 1)
		return min((pg-1)*per, n), min(pg*per, n)
	}
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
			a, b := page(len(runs))
			return json.Marshal(map[string]any{"check_runs": runs[a:b]})
		case path == commit+"status":
			return []byte(`{"statuses":[]}`), nil
		case strings.HasPrefix(path, base+"/reviews"):
			var rs []map[string]any
			for _, r := range p.reviews {
				rs = append(rs, map[string]any{"user": map[string]string{"login": r[0]}, "state": r[1]})
			}
			a, b := page(len(rs))
			return json.Marshal(rs[a:b])
		}
	}
	return nil, fmt.Errorf("unexpected gh %v", args)
}

// watchWorld stubs gh with a fake GitHub and points the watcher's files at a
// fresh directory.
func watchWorld(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{}
	oldRun, oldSettings, oldLog := ghRun, state.SettingsPath, state.UpdateLogPath
	dir := t.TempDir()
	ghRun, state.SettingsPath, state.UpdateLogPath = g.run, dir+"/settings.json", dir+"/update.log"
	t.Cleanup(func() { ghRun, state.SettingsPath, state.UpdateLogPath = oldRun, oldSettings, oldLog })
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
	// The first pass says it started, and which PRs await review already
	// (a consumer starting with it must know); nothing else.
	if got := passEvents(t, now); !slices.Equal(got, []string{"watch_started", "needs_review " + a.url()}) {
		t.Errorf("the first pass says it started and what awaits review: %v", got)
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
	step("review_not_needed", "ready") // approved: no longer awaiting review
	a.head = "h3"
	a.reviews = [][2]string{{"rev", "DISMISSED"}}
	step("needs_review") // new head, approval dismissed: asked again
	a.state, a.mergedAt = "merged", now
	b.state = "closed"
	step("review_not_needed", "merged", "closed") // a was awaiting review again at h3
	step()                                        // never again
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
	if got := passEvents(t, now.Add(2*time.Hour)); !slices.Equal(got, []string{"review_not_needed " + a.url(), "merged " + a.url()}) {
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
	// It was awaiting review (needs_review at the seed): a new head says it
	// no longer is, then the threads.
	if got := pass(); !slices.Equal(got, []string{"review_not_needed", "threads_unresolved", "threads_unanswered"}) {
		t.Errorf("open threads: %v, want review_not_needed, threads_unresolved, threads_unanswered and no needs_review", got)
	}
	if data, _ := os.ReadFile(prEventsPath()); !strings.Contains(string(data), `"event":"review_not_needed"`) || !strings.Contains(string(data), `"reason":"new_head"`) {
		t.Errorf("review_not_needed says why: %s", data)
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
	if got := pass(); !slices.Equal(got, []string{"review_not_needed", "checks_failing"}) {
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

func TestNeedsReviewIsRecheckedAndEndsWithReviewNotNeeded(t *testing.T) {
	g := watchWorld(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	pending(a)
	g.prs = []*fakePR{a}
	passEvents(t, now) // seed: not green yet

	// The pass says green and waiting, but GitHub, asked again just before
	// the event, has it approved: no needs_review.
	green(a)
	old := reviewRecheck
	t.Cleanup(func() { reviewRecheck = old })
	reviewRecheck = func(p watchedPR) (*watchedPR, error) { p.Review = "approved"; return &p, nil }
	if got := passEvents(t, now.Add(3*time.Minute)); slices.Contains(got, "needs_review "+a.url()) {
		t.Errorf("stale pass data never says awaiting review: %v", got)
	}
	reviewRecheck = old
	if got := passEvents(t, now.Add(6*time.Minute)); !slices.Contains(got, "needs_review "+a.url()) {
		t.Errorf("asked again next pass, and still waiting: %v", got)
	}
	if st, _ := loadPRWatchState(); !st.PRs[a.url()].Awaiting || st.Version != prWatchVersion {
		t.Errorf("awaiting is kept: %+v", st.PRs[a.url()])
	}

	a.draft = true
	got := passEvents(t, now.Add(9*time.Minute))
	data, _ := os.ReadFile(prEventsPath())
	if !slices.Contains(got, "review_not_needed "+a.url()) || !strings.Contains(string(data), `"reason":"draft"`) {
		t.Errorf("leaving it says so, with why: %v\n%s", got, data)
	}
	if got := passEvents(t, now.Add(12*time.Minute)); len(got) != 0 {
		t.Errorf("said once: %v", got)
	}
}

func TestWatchStateFromBeforeAwaitingIsMigratedQuietly(t *testing.T) {
	g := watchWorld(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean"}
	green(a)
	g.prs = []*fakePR{a}
	passEvents(t, now)
	st, _ := loadPRWatchState()
	st.Version, st.PRs[a.url()].Awaiting = 0, false // as an older ccs wrote it
	savePRWatchState(st)
	if got := passEvents(t, now.Add(3*time.Minute)); len(got) != 0 {
		t.Errorf("an upgrade emits nothing: %v", got)
	}
	a.reviews = [][2]string{{"rev", "APPROVED"}}
	if got := passEvents(t, now.Add(6*time.Minute)); !slices.Equal(got, []string{"review_not_needed " + a.url(), "ready " + a.url()}) {
		t.Errorf("but knows it was awaiting: %v", got)
	}
}

func TestThreadsUnansweredByTheOperator(t *testing.T) {
	g := watchWorld(t)
	pr := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean", runs: [][3]string{{"test", "completed", "success"}}}
	g.prs = []*fakePR{pr}
	setThreadSelves("me", "my-bot, Co-Author")
	t.Cleanup(func() { setThreadSelves("", "") })
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
	pass() // seeds

	pr.threads = []fakeThread{{"T1", 10, "reviewer"}, {"T2", 20, "me"}, {"T3", 30, "my-bot"}, {"T4", 40, "co-author"}}
	if got := pass(); !slices.Equal(got, []string{"review_not_needed", "threads_unresolved", "threads_unanswered"}) {
		t.Errorf("four unresolved, one unanswered: %v", got)
	}
	st, _ := loadPRWatchState()
	if p := st.PRs[pr.url()]; len(p.Unanswered) != 1 || p.Unanswered["T1"] != 10 || *p.Threads != 4 {
		t.Errorf("the operator's, its bot's and co-author's replies answer a thread: %+v", p)
	}
	if data, _ := os.ReadFile(prEventsPath()); !strings.Contains(string(data), `"event":"threads_unanswered"`) || !strings.Contains(string(data), `"count":1,"threads":["T1"]`) {
		t.Errorf("the event carries the count and ids: %s", data)
	}

	pr.threads[0].by, pr.threads[0].last = "me", 11 // answered
	if got := pass(); len(got) != 0 {
		t.Errorf("answered, still unresolved: no event: %v", got)
	}
	pr.threads[1].by, pr.threads[1].last = "reviewer", 21 // a reviewer replies again
	if got := pass(); !slices.Equal(got, []string{"threads_unanswered"}) {
		t.Errorf("a new reviewer comment: %v", got)
	}
}

func TestRemovedReviewFlagsSayWhereReviewsWent(t *testing.T) {
	for _, args := range [][]string{{"--review"}, {"--once", "-review-channel=C1"}, {"--dry-run"}, {"--review-hook", "x"}} {
		if got := runWatch(args); got != 2 {
			t.Errorf("%v: exit %d, want 2 before anything runs", args, got)
		}
	}
}

func TestPRDetailsReadsEveryPage(t *testing.T) {
	g := watchWorld(t)
	var ok [][3]string
	for i := range 100 {
		ok = append(ok, [3]string{fmt.Sprintf("job-%d", i), "completed", "success"})
	}
	var comments [][2]string
	for range 100 {
		comments = append(comments, [2]string{"bot", "COMMENTED"})
	}
	var resolvedOnly []fakeThread // 120 threads answered by the operator, one unresolved on page 2
	for i := range 120 {
		resolvedOnly = append(resolvedOnly, fakeThread{fmt.Sprintf("T%d", i), int64(i), "me"})
	}
	resolvedOnly = append(resolvedOnly, fakeThread{"late", 999, "reviewer"})
	g.prs = []*fakePR{
		{repo: "o/r", number: 1, state: "open", head: "a", runs: append(slices.Clone(ok), [3]string{"late", "completed", "failure"})},
		{repo: "o/r", number: 2, state: "open", head: "b", runs: ok, reviews: append(slices.Clone(comments), [2]string{"x", "APPROVED"})},
		{repo: "o/r", number: 3, state: "open", head: "c", runs: ok, threads: resolvedOnly},
	}
	setThreadSelves("me", "")
	d := func(n int) *watchedPR {
		p, err := prDetails(watchedPR{URL: fmt.Sprintf("https://github.com/o/r/pull/%d", n), Repo: "o/r", Number: n})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := d(1); p.Checks != "failing" || !slices.Equal(p.Failing, []string{"late"}) {
		t.Errorf("a failing check on page 2 counts: %s %v", p.Checks, p.Failing)
	}
	if p := d(2); p.Review != "approved" {
		t.Errorf("an approval on page 2 counts: %s", p.Review)
	}
	if p := d(3); p.Threads == nil || *p.Threads != 121 || len(p.Unanswered) != 1 {
		t.Errorf("threads on page 2 count: %v %v", p.Threads, p.Unanswered)
	}
}

// countDetailReads counts the REST calls about PRs from here on.
func countDetailReads(t *testing.T) *int {
	t.Helper()
	n, run := 0, ghRun
	ghRun = func(a ...string) ([]byte, error) {
		if a[0] == "api" && strings.HasPrefix(a[1], "repos/") {
			n++
		}
		return run(a...)
	}
	return &n
}

func TestWatchSkipsAnUnchangedSettledPRUntilAFullRead(t *testing.T) {
	g := watchWorld(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean", updatedAt: "2026-11-03T08:00:00Z"}
	green(a)
	g.prs = []*fakePR{a}
	passEvents(t, now)
	reads := countDetailReads(t)
	a.runs = [][3]string{{"test", "completed", "failure"}} // a re-run: updatedAt doesn't move

	if got := passEvents(t, now.Add(3*time.Minute)); len(got) != 0 || *reads != 0 {
		t.Errorf("unchanged: events %v, %d reads", got, *reads)
	}
	if got := passEvents(t, now.Add(prFullEvery)); !strings.Contains(strings.Join(got, " "), "checks_failing") || *reads == 0 {
		t.Errorf("after prFullEvery: events %v, %d reads", got, *reads)
	}
}

func TestWatchReadsAPRWhoseUpdatedAtMoved(t *testing.T) {
	g := watchWorld(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean", updatedAt: "2026-11-03T08:00:00Z"}
	green(a)
	g.prs = []*fakePR{a}
	passEvents(t, now)
	a.head, a.updatedAt = "h2", "2026-11-03T09:01:00Z"
	a.runs = [][3]string{{"test", "completed", "failure"}}
	if got := passEvents(t, now.Add(3*time.Minute)); !strings.Contains(strings.Join(got, " "), "checks_failing") {
		t.Errorf("a moved PR is read: %v", got)
	}
}

func TestWatchKeepsReadingPRsNotSettled(t *testing.T) {
	g := watchWorld(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	const at = "2026-11-03T08:00:00Z"
	running := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean", updatedAt: at}
	pending(running)
	uncounted := &fakePR{repo: "o/a", number: 2, state: "open", head: "h2", mergeable: "clean", updatedAt: at, threadsFail: true}
	green(uncounted)
	noChecks := &fakePR{repo: "o/a", number: 3, state: "open", head: "h3", mergeable: "clean", updatedAt: at}
	g.prs = []*fakePR{running, uncounted, noChecks}
	passEvents(t, now)
	reads := countDetailReads(t)
	green(running)
	if got := passEvents(t, now.Add(3*time.Minute)); !strings.Contains(strings.Join(got, " "), "checks_passed") {
		t.Errorf("pending checks are read every pass: %v", got)
	}
	if *reads < 3 { // each of the three PRs at least once
		t.Errorf("%d reads, want every PR read", *reads)
	}
}

func TestWatchKeepsThreadCountsOfASkippedPR(t *testing.T) {
	g := watchWorld(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean", updatedAt: "2026-11-03T08:00:00Z", unresolved: 2}
	green(a)
	g.prs = []*fakePR{a}
	passEvents(t, now)
	if got := passEvents(t, now.Add(3*time.Minute)); len(got) != 0 {
		t.Errorf("a skipped PR emits nothing: %v", got)
	}
	st, _ := loadPRWatchState()
	p := st.PRs[a.url()]
	if p.Threads == nil || *p.Threads != 2 || p.Head != "h1" {
		t.Errorf("skipped PR's state kept: %+v", p)
	}
	if !p.FullAt.Equal(now) {
		t.Errorf("a skip must not move FullAt: %v", p.FullAt)
	}
}

func TestWatchSkipsAnUnchangedGreenDraft(t *testing.T) {
	g := watchWorld(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	a := &fakePR{repo: "o/a", number: 1, state: "open", head: "h1", mergeable: "clean", draft: true, updatedAt: "2026-11-03T08:00:00Z"}
	green(a)
	g.prs = []*fakePR{a}
	passEvents(t, now)
	reads := countDetailReads(t)
	if got := passEvents(t, now.Add(3*time.Minute)); len(got) != 0 || *reads != 0 {
		t.Errorf("unchanged draft: events %v, %d reads", got, *reads)
	}
	a.draft, a.updatedAt = false, "2026-11-03T09:04:00Z" // marked ready: updatedAt moves
	if passEvents(t, now.Add(6*time.Minute)); *reads == 0 {
		t.Error("a draft marked ready must be read")
	}
	if p := func() *watchedPR { st, _ := loadPRWatchState(); return st.PRs[a.url()] }(); p.Draft || p.Threads == nil {
		t.Errorf("after ready: draft %v threads %v", p.Draft, p.Threads)
	}
}

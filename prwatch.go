package main

// ccs watch: a foreground loop that watches one author's open PRs in one
// GitHub owner and appends an event line for each change (prEventsPath), for
// a coordinating session to follow with tail -F. REST only: GitHub's GraphQL
// search hits RESOURCE_LIMITS_EXCEEDED across many PRs.

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// watchedPR is what the watcher last saw of one PR.
type watchedPR struct {
	URL       string   `json:"url"`
	Repo      string   `json:"repo"` // owner/name
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"` // open, merged, closed
	Head      string   `json:"head,omitempty"`
	Draft     bool     `json:"draft,omitempty"`
	Checks    string   `json:"checks,omitempty"` // success, pending, failing, none
	Failing   []string `json:"failing,omitempty"`
	Review    string   `json:"review,omitempty"` // approved, changes_requested, required
	Conflicts bool     `json:"conflicts,omitempty"`
	Threads   *int     `json:"threads,omitempty"`    // unresolved review threads; nil until counted (only GraphQL knows)
	AskedHead string   `json:"asked_head,omitempty"` // the head needs_review was emitted for
	Awaiting  bool     `json:"awaiting,omitempty"`   // needs_review emitted and no review_not_needed since
	Ended     string   `json:"ended,omitempty"`      // when it merged or closed (RFC3339), to prune it later

	conflictsUnknown bool // GitHub hadn't worked out mergeability yet: keep the last answer
	threadsUnknown   bool // not counted this pass (checks not green, or the query failed): keep the last count
}

// threadsClear: its review threads were counted and none is unresolved.
func (p *watchedPR) threadsClear() bool { return p.Threads != nil && *p.Threads == 0 }

// ready: approved, checks green, no unresolved thread, not a draft.
func (p *watchedPR) ready() bool {
	return p.State == "open" && !p.Draft && p.Checks == "success" && p.threadsClear() && p.Review == "approved"
}

// needsReview: green, no unresolved thread and not a draft, waiting for an
// approval. The one test of whether to ask anyone for a review (review
// batching uses it too).
func (p *watchedPR) needsReview() bool {
	return p.State == "open" && !p.Draft && p.Checks == "success" && p.threadsClear() && p.Review == "required"
}

// prWatchState is the watcher's record, kept between passes and restarts.
// Updated says how fresh it is, for readers such as ccs prs.
type prWatchState struct {
	Author  string                `json:"author"`
	Owner   string                `json:"owner"`
	Started time.Time             `json:"started"` // the first pass: merges before it are never news
	Updated time.Time             `json:"updated"` // the last complete pass
	PRs     map[string]*watchedPR `json:"prs"`
	Version int                   `json:"version,omitempty"` // 2: Awaiting is kept
}

// reviewDeprecated is printed by ccs watch --review and its flags, and by
// ccs review, for one release before they go.
const reviewDeprecated = "ccs watch --review is deprecated and will be removed in the next release: review requests move to the two-slack plugin's consumer, which reads ccs watch's events"

// prWatchVersion is the state format: 2 added Awaiting.
const prWatchVersion = 2

// prEvent is one line of prEventsPath.
type prEvent struct {
	At     string   `json:"at"`
	Event  string   `json:"event"`
	PR     string   `json:"pr,omitempty"`
	Repo   string   `json:"repo,omitempty"`
	Number int      `json:"number,omitempty"`
	Title  string   `json:"title,omitempty"`
	Head   string   `json:"head,omitempty"`
	Checks []string `json:"checks,omitempty"` // failing checks, on checks_failing
	Review string   `json:"review,omitempty"`
	// Unresolved review threads, on threads_unresolved.
	Unresolved int `json:"unresolved,omitempty"`
	// Why a PR stopped awaiting review, on review_not_needed.
	Reason string `json:"reason,omitempty"`
}

// notNeededReason is why p, which was awaiting review at head asked, no
// longer is: "" while it still is.
func notNeededReason(p *watchedPR, asked string) string {
	switch {
	case p.State == "merged" || p.State == "closed":
		return p.State
	case p.Head != asked:
		return "new_head"
	case p.Review == "approved":
		return "approved"
	case p.Review == "changes_requested":
		return "changes_requested"
	case p.Draft:
		return "draft"
	case p.Checks != "success":
		return "checks"
	case !p.threadsClear():
		return "threads"
	}
	return ""
}

// stillNeedsReview asks GitHub about p again just before needs_review is
// emitted (prDetails, shared with review batching's recheck), so stale pass
// data never says a PR is waiting when it isn't. An error means not now.
func stillNeedsReview(p *watchedPR) bool {
	f, err := reviewRecheck(*p)
	if err != nil {
		logActivity("pr watch: couldn't recheck %s before needs_review: %v", p.URL, err)
		return false
	}
	return f.Review != "approved" && f.Review != "changes_requested" && f.Head == p.Head && !f.Draft && f.Checks == "success"
}

func prWatchStatePath() string { return filepath.Join(filepath.Dir(settingsPath), "pr-watch.json") }
func prWatchLockPath() string  { return filepath.Join(filepath.Dir(settingsPath), "pr-watch.lock") }

// prEventsPath is where events go, beside the update and activity logs.
func prEventsPath() string { return filepath.Join(filepath.Dir(updateLogPath), "pr-events.jsonl") }

// ghRun runs gh; tests stub it.
var ghRun = func(args ...string) ([]byte, error) {
	out, err := runBounded(30*time.Second, nil, "gh", args...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return out, err
}

// prWatchParallel is how many PRs one pass asks about at once.
const prWatchParallel = 4

// mergedKeep is how long a merged or closed PR stays in the state, and
// mergedWindow how far back the merged search looks: the window is always
// inside what's kept, so an old merge is never news again.
const (
	mergedKeep   = 7 * 24 * time.Hour
	mergedWindow = 3 * 24 * time.Hour
)

// loadPRWatchState is the watcher's last record; ok is false when there's
// none (it hasn't run, or the file is unreadable).
func loadPRWatchState() (s prWatchState, ok bool) {
	b, err := os.ReadFile(prWatchStatePath())
	if err != nil || json.Unmarshal(b, &s) != nil || s.PRs == nil {
		return prWatchState{}, false
	}
	return s, true
}

func savePRWatchState(s prWatchState) error {
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	path := prWatchStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lockPRWatch takes the single-instance lock, held until the process exits.
func lockPRWatch() (*os.File, error) {
	path := prWatchLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(path)
		f.Close()
		return nil, fmt.Errorf("ccs watch is already running (pid %s)", strings.TrimSpace(string(b)))
	}
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, nil
}

// runWatch is `ccs watch`; it returns the exit code.
func runWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	interval := fs.Duration("interval", 3*time.Minute, "time between passes")
	author := fs.String("author", "", "PR author (default: the user gh is logged in as)")
	owner := fs.String("owner", "", "GitHub owner (org or user) whose repos to watch (default: every owner)")
	once := fs.Bool("once", false, "make one pass and exit")
	noNudge := fs.Bool("no-nudge", false, "don't tell the sessions that claimed PRs (ccs claim) about failing checks, conflicts or new unresolved threads")
	review := fs.Bool("review", false, "DEPRECATED, removed next release. also request reviews: batch PRs that are green and need review, post to --review-channel what the hook routes there, draft the rest (ccs review)")
	dryRun := fs.Bool("dry-run", false, "DEPRECATED. with --review: print the plan for the PRs waiting now, from the last pass's state, and post nothing")
	fs.StringVar(&reviewCfg.Channel, "review-channel", "", "DEPRECATED. with --review: the Slack channel id review requests are posted to")
	fs.StringVar(&reviewCfg.Mention, "review-mention", "", "DEPRECATED. with --review: the mention that post must carry, e.g. <!subteam^S…>")
	fs.StringVar(&reviewCfg.Hook, "review-hook", "", "DEPRECATED. with --review: the command that routes a batch (run with sh -c): the batch as JSON on its stdin, the plan as JSON on its stdout")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: ccs watch [flags]\n\nWatch an author's open PRs and append one JSON line per change to\n%s (and stdout).\nEvents: watch_started, new, ready, needs_review, review_not_needed, checks_failing, checks_passed,\nconflicts, threads_unresolved, threads_resolved, merged, closed; with --review also\nreview_requested, review_dm_pending, review_held, review_plan_failed; unless --no-nudge,\nnudged and nudge_failed (the live sessions that claimed a PR are told of its failing\nchecks, conflicts and new unresolved threads). Only one ccs watch runs at a time.\n\n", prEventsPath())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := defaultAuthor(author); err != nil {
		fmt.Fprintln(os.Stderr, "ccs watch:", err)
		return 1
	}
	deprecated := false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "review", "dry-run", "review-channel", "review-mention", "review-hook":
			deprecated = true
		}
	})
	if deprecated {
		fmt.Fprintln(os.Stderr, reviewDeprecated)
	}
	if *review {
		if err := reviewCfg.check(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if *dryRun {
		if !*review {
			fmt.Fprintln(os.Stderr, "--dry-run needs --review")
			return 2
		}
		if _, ok := loadPRWatchState(); !ok {
			fmt.Fprintln(os.Stderr, "no watcher state yet: run ccs watch (or ccs watch --once) first")
			return 1
		}
		if err := reviewTick(time.Now(), true, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "ccs watch:", err)
			return 1
		}
		return 0
	}
	lock, err := lockPRWatch()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer lock.Close()
	for {
		if err := watchPass(*author, *owner, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "ccs watch:", err)
			logActivity("pr watch: %v", err)
		} else {
			afterPass(!*noNudge, *review)
		}
		if *once {
			return 0
		}
		time.Sleep(*interval)
	}
}

// afterPass runs what follows a successful pass: nudges, then review requests.
func afterPass(nudge, review bool) {
	if nudge {
		if err := nudgeTick(time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "ccs watch:", err)
			logActivity("nudge: %v", err)
		}
	}
	if review {
		if err := reviewTick(time.Now(), false, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "ccs watch:", err)
			logActivity("review: %v", err)
		}
	}
}

// watchPass makes one pass: fetch, diff against the saved state, emit,
// save. With no saved state (or one for another author/owner) it only
// records what it finds and emits watch_started, so the first pass doesn't
// report every PR as news. A search that fails leaves the state untouched.
func watchPass(author, owner string, now time.Time) error {
	old, ok := loadPRWatchState()
	seeding := !ok || old.Author != author || old.Owner != owner
	if seeding {
		old = prWatchState{Author: author, Owner: owner, Started: now, PRs: map[string]*watchedPR{}}
	}
	open, err := searchPRs(author, owner, "--state", "open")
	if err != nil {
		return fmt.Errorf("open PRs: %w", err)
	}
	since := max(old.Started.Unix(), now.Add(-mergedWindow).Unix())
	merged, err := searchPRs(author, owner, "--merged-at", ">="+time.Unix(since, 0).UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("merged PRs: %w", err)
	}

	if !seeding && old.Version < 2 {
		// From before Awaiting was kept: a PR asked about at its current head
		// and still waiting was awaiting review.
		for _, p := range old.PRs {
			p.Awaiting = p.AskedHead != "" && p.AskedHead == p.Head && p.needsReview()
		}
	}
	next := prWatchState{Author: author, Owner: owner, Started: old.Started, Updated: now, PRs: map[string]*watchedPR{}, Version: prWatchVersion}
	var events []prEvent
	emit := func(p *watchedPR, event string) {
		e := prEvent{At: now.UTC().Format(time.RFC3339), Event: event, PR: p.URL, Repo: p.Repo, Number: p.Number, Title: p.Title, Head: p.Head}
		switch event {
		case "checks_failing":
			e.Checks = p.Failing
		case "ready", "needs_review":
			e.Review = p.Review
		case "threads_unresolved":
			e.Unresolved = *p.Threads
		}
		events = append(events, e)
	}
	// notNeeded emits review_not_needed for a PR that was awaiting review and
	// isn't now (n carries its state now, o what it was awaiting at).
	notNeeded := func(n, o *watchedPR) {
		if o == nil || !o.Awaiting {
			return
		}
		why := notNeededReason(n, o.AskedHead)
		if why == "" {
			n.Awaiting = true
			return
		}
		n.Awaiting = false
		e := prEvent{At: now.UTC().Format(time.RFC3339), Event: "review_not_needed", PR: n.URL, Repo: n.Repo, Number: n.Number, Title: n.Title, Head: o.AskedHead, Reason: why}
		events = append(events, e)
	}

	// Open PRs: details for each, in parallel, then diff.
	details := prDetailsAll(open)
	openSet := map[string]bool{}
	for i, p := range open {
		openSet[p.URL] = true
		n, o := details[i], old.PRs[p.URL]
		if n == nil { // couldn't be read: keep what was known, report nothing
			if o != nil {
				next.PRs[p.URL] = o
			}
			continue
		}
		if o != nil {
			n.AskedHead, n.Awaiting = o.AskedHead, o.Awaiting
			if n.conflictsUnknown {
				n.Conflicts = o.Conflicts
			}
			if n.threadsUnknown {
				n.Threads = o.Threads
			}
		}
		next.PRs[p.URL] = n
		if seeding {
			// Awaiting review already: said now, so a consumer starting
			// with the watcher knows (once per head, as ever).
			if n.needsReview() && stillNeedsReview(n) {
				n.AskedHead, n.Awaiting = n.Head, true
				emit(n, "needs_review")
			}
			continue
		}
		notNeeded(n, o)
		prev := o
		if prev == nil || prev.State != "open" {
			emit(n, "new")
			prev = &watchedPR{State: "open"}
		}
		switch {
		case n.Checks == "failing" && (prev.Checks != "failing" || prev.Head != n.Head):
			emit(n, "checks_failing")
		case n.Checks == "success" && o != nil && o.State == "open" && prev.Checks != "success":
			emit(n, "checks_passed")
		}
		if n.Conflicts && !prev.Conflicts {
			emit(n, "conflicts")
		}
		if !n.threadsUnknown {
			was := 0
			if prev.Threads != nil {
				was = *prev.Threads
			}
			switch {
			case *n.Threads > was:
				emit(n, "threads_unresolved")
			case *n.Threads == 0 && was > 0:
				emit(n, "threads_resolved")
			}
		}
		if n.ready() && !prev.ready() {
			emit(n, "ready")
		}
		if n.needsReview() && n.AskedHead != n.Head && stillNeedsReview(n) {
			n.AskedHead, n.Awaiting = n.Head, true
			emit(n, "needs_review")
		}
	}

	// Merged, by the search: a PR merged before the watch started never shows.
	mergedSet := map[string]bool{}
	for _, p := range merged {
		mergedSet[p.URL] = true
		o := old.PRs[p.URL]
		if o != nil && o.State == "merged" {
			next.PRs[p.URL] = o
			continue
		}
		n := p
		n.State, n.Ended = "merged", now.UTC().Format(time.RFC3339)
		if o != nil {
			n.Head = o.Head
		}
		next.PRs[p.URL] = &n
		if !seeding {
			notNeeded(&n, o)
			emit(&n, "merged")
		}
	}

	// Open before, gone now and not in the merged search: ask it directly.
	for url, o := range old.PRs {
		if openSet[url] || mergedSet[url] {
			continue
		}
		if o.State != "open" {
			if t, err := time.Parse(time.RFC3339, o.Ended); err == nil && now.Sub(t) < mergedKeep {
				next.PRs[url] = o
			}
			continue
		}
		state, err := prEndState(o)
		if err != nil || state == "open" { // unknown or still open (search lag): keep it as it was
			if err != nil {
				logActivity("pr watch: %s: %v", url, err)
			}
			next.PRs[url] = o
			continue
		}
		n := *o
		n.State, n.Ended = state, now.UTC().Format(time.RFC3339)
		next.PRs[url] = &n
		notNeeded(&n, o)
		emit(&n, state)
	}

	if seeding {
		events = append([]prEvent{{At: now.UTC().Format(time.RFC3339), Event: "watch_started", Review: fmt.Sprintf("%d open PRs by %s in %s", len(open), author, cmp.Or(owner, "any owner"))}}, events...)
	}
	if err := writePREvents(events); err != nil {
		return fmt.Errorf("writing events: %w", err)
	}
	return savePRWatchState(next)
}

// writePREvents appends events to prEventsPath and prints them.
func writePREvents(events []prEvent) error {
	all := make([]any, len(events))
	for i, e := range events {
		all[i] = e
	}
	return writeEventLines(all)
}

// writeEventLines appends one JSON line per event to prEventsPath and prints them.
func writeEventLines(events []any) error {
	if len(events) == 0 {
		return nil
	}
	var b strings.Builder
	for _, e := range events {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	path := prEventsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return err
	}
	fmt.Print(b.String())
	return nil
}

// ghUser is the login gh is signed in as. Stubbed in tests.
var ghUser = func() (string, error) {
	out, err := ghRun("api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultAuthor fills an empty --author with ghUser, failing loudly when gh
// can't say who it is.
func defaultAuthor(author *string) error {
	if *author != "" {
		return nil
	}
	login, err := ghUser()
	if err != nil || login == "" {
		return fmt.Errorf("--author not given and gh can't say who you are (gh auth login?): %v", err)
	}
	*author = login
	return nil
}

// searchPRs is gh search prs for author's PRs in owner's repos, with extra
// filters; blank or malformed entries are dropped.
func searchPRs(author, owner string, filters ...string) ([]watchedPR, error) {
	args := []string{"search", "prs", "--author", author}
	if owner != "" { // else every owner's repos
		args = append(args, "--owner", owner)
	}
	args = append(append(args, "--sort", "updated", "--limit", "200", "--json", "url,title,number,repository,isDraft"), filters...)
	out, err := ghRun(args...)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		URL        string `json:"url"`
		Title      string `json:"title"`
		Number     int    `json:"number"`
		IsDraft    bool   `json:"isDraft"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("reading gh search: %w", err)
	}
	var prs []watchedPR
	for _, r := range rows {
		if strings.TrimSpace(r.URL) == "" || !prURL.MatchString(r.URL) {
			continue
		}
		repo := r.Repository.NameWithOwner
		if repo == "" {
			p := prURL.FindStringSubmatch(r.URL)
			repo = p[1] + "/" + p[2]
		}
		prs = append(prs, watchedPR{URL: r.URL, Repo: repo, Number: r.Number, Title: r.Title, Draft: r.IsDraft, State: "open"})
	}
	return prs, nil
}

// prDetails reads an open PR's head, draft, mergeability, checks and
// reviews over REST.
func prDetails(p watchedPR) (*watchedPR, error) {
	base := "repos/" + p.Repo
	var pr struct {
		Draft          bool   `json:"draft"`
		MergeableState string `json:"mergeable_state"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := ghJSON(&pr, fmt.Sprintf("%s/pulls/%d", base, p.Number)); err != nil {
		return nil, err
	}
	n := p
	n.Head, n.Draft = pr.Head.SHA, pr.Draft
	n.Conflicts = pr.MergeableState == "dirty"
	n.conflictsUnknown = pr.MergeableState == "unknown" || pr.MergeableState == ""
	var runs struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := ghJSON(&runs, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", base, n.Head)); err != nil {
		return nil, err
	}
	var status struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := ghJSON(&status, fmt.Sprintf("%s/commits/%s/status", base, n.Head)); err != nil {
		return nil, err
	}
	pending, total := false, 0
	for _, c := range runs.CheckRuns {
		total++
		switch {
		case c.Status != "completed":
			pending = true
		case failedCheck[strings.ToUpper(c.Conclusion)]:
			n.Failing = append(n.Failing, c.Name)
		}
	}
	for _, s := range status.Statuses {
		total++
		switch s.State {
		case "pending":
			pending = true
		case "failure", "error":
			n.Failing = append(n.Failing, s.Context)
		}
	}
	slices.Sort(n.Failing)
	n.Failing = slices.Compact(n.Failing)
	switch {
	case len(n.Failing) > 0:
		n.Checks = "failing"
	case pending:
		n.Checks = "pending"
	case total == 0:
		n.Checks = "none"
	default:
		n.Checks = "success"
	}
	var reviews []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State string `json:"state"`
	}
	if err := ghJSON(&reviews, fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", base, p.Number)); err != nil {
		return nil, err
	}
	n.threadsUnknown = true
	if n.Checks == "success" && !n.Draft { // only then could it be ready or need a review
		if c, err := unresolvedThreads(p); err != nil {
			logActivity("pr watch: %s: counting review threads: %v", p.URL, err)
		} else {
			n.Threads, n.threadsUnknown = &c, false
		}
	}
	latest := map[string]string{} // each reviewer's last approve, request-changes or dismissal
	for _, r := range reviews {
		if r.State == "APPROVED" || r.State == "CHANGES_REQUESTED" || r.State == "DISMISSED" {
			latest[r.User.Login] = r.State
		}
	}
	n.Review = "required"
	for _, s := range latest {
		if s == "CHANGES_REQUESTED" {
			n.Review = "changes_requested"
			break
		}
		if s == "APPROVED" {
			n.Review = "approved"
		}
	}
	return &n, nil
}

// unresolvedThreads is how many of a PR's review threads are unresolved:
// one small GraphQL query, since REST doesn't say (at most the first 100).
func unresolvedThreads(p watchedPR) (int, error) {
	owner, name, _ := strings.Cut(p.Repo, "/")
	out, err := ghRun("api", "graphql",
		"-f", "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{isResolved}}}}}",
		"-F", "owner="+owner, "-F", "name="+name, "-F", fmt.Sprintf("number=%d", p.Number))
	if err != nil {
		return 0, err
	}
	var resp struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes []struct{ IsResolved bool }
					}
				}
			}
		}
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return 0, err
	}
	if resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
		return 0, errors.New("pull request not found")
	}
	n := 0
	for _, t := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if !t.IsResolved {
			n++
		}
	}
	return n, nil
}

// prDetailsAll is prDetails for each PR, prWatchParallel at a time; a PR
// that couldn't be read is nil (and logged).
func prDetailsAll(open []watchedPR) []*watchedPR {
	details := make([]*watchedPR, len(open))
	var wg sync.WaitGroup
	sem := make(chan struct{}, prWatchParallel)
	for i, p := range open {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := prDetails(p)
			if err != nil {
				logActivity("pr watch: %s skipped this pass: %v", p.URL, err)
				return
			}
			details[i] = d
		}()
	}
	wg.Wait()
	return details
}

// prEndState is "merged", "closed" or "open" for a PR the open search no
// longer lists.
func prEndState(p *watchedPR) (string, error) {
	var pr struct {
		State    string `json:"state"`
		MergedAt string `json:"merged_at"`
	}
	if err := ghJSON(&pr, fmt.Sprintf("repos/%s/pulls/%d", p.Repo, p.Number)); err != nil {
		return "", err
	}
	switch {
	case pr.MergedAt != "":
		return "merged", nil
	case pr.State == "closed":
		return "closed", nil
	}
	return "open", nil
}

func ghJSON(v any, path string) error {
	out, err := ghRun("api", path)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

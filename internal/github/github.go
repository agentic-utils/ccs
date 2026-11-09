// Package github is ccs's view of the user's pull requests: looking them up
// and grouping their status (ready, needs you, waiting), the PRs each
// session touched, ccs watch and its events, the PR owner registry (ccs
// claim, ccs prs) and the nudges ccs watch sends to the sessions that own
// a PR.
package github

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// WatchedPR is what the watcher last saw of one PR.
type WatchedPR struct {
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
	Threads   *int     `json:"threads,omitempty"` // unresolved review threads; nil until counted (only GraphQL knows)
	// Unanswered: the unresolved threads whose last comment isn't the
	// operator's (threadSelves), thread id to that comment's id.
	Unanswered map[string]int64 `json:"unanswered,omitempty"`
	AskedHead  string           `json:"asked_head,omitempty"` // the head needs_review was emitted for
	Awaiting   bool             `json:"awaiting,omitempty"`   // needs_review emitted and no review_not_needed since
	Ended      string           `json:"ended,omitempty"`      // when it merged or closed (RFC3339), to prune it later
	UpdatedAt  string           `json:"updated_at,omitempty"` // GitHub's updatedAt from the search
	FullAt     time.Time        `json:"full_at,omitzero"`     // when its details were last read

	conflictsUnknown bool // GitHub hadn't worked out mergeability yet: keep the last answer
	threadsUnknown   bool // not counted this pass (checks not green, or the query failed): keep the last count
}

// threadsClear: its review threads were counted and none is unresolved.
func (p *WatchedPR) threadsClear() bool { return p.Threads != nil && *p.Threads == 0 }

// ready: approved, checks green, no unresolved thread, not a draft.
func (p *WatchedPR) ready() bool {
	return p.State == "open" && !p.Draft && p.Checks == "success" && p.threadsClear() && p.Review == "approved"
}

// NeedsReview: green, no unresolved thread and not a draft, waiting for an
// approval. The one test of whether to ask anyone for a review (review
// batching uses it too).
func (p *WatchedPR) NeedsReview() bool {
	return p.State == "open" && !p.Draft && p.Checks == "success" && p.threadsClear() && p.Review == "required"
}

// PrWatchState is the watcher's record, kept between passes and restarts.
// Updated says how fresh it is, for readers such as ccs prs.
type PrWatchState struct {
	Author  string                `json:"author"`
	Owner   string                `json:"owner"`
	Started time.Time             `json:"started"` // the first pass: merges before it are never news
	Updated time.Time             `json:"updated"` // the last complete pass
	PRs     map[string]*WatchedPR `json:"prs"`
	Version int                   `json:"version,omitempty"` // 2: Awaiting is kept
}

// ReviewMoved answers ccs watch --review and its flags, and ccs review,
// which are gone: ccs reports, something else posts.
const ReviewMoved = "review requests moved out of ccs: the two-slack plugin's review-requests consumer posts them, reading ccs watch's needs_review and review_not_needed events (or GitHub directly)"

// ReviewRecheck is a PR as GitHub has it now (prDetails), asked just before
// needs_review is emitted. Stubbed in tests.
var ReviewRecheck = func(p WatchedPR) (*WatchedPR, error) { return PrDetails(p) }

// PrWatchVersion is the state format: 2 added Awaiting.
const PrWatchVersion = 2

// PrEvent is one line of prEventsPath.
type PrEvent struct {
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
	// Unanswered review threads, on threads_unanswered: how many and their ids.
	Count   int      `json:"count,omitempty"`
	Threads []string `json:"threads,omitempty"`
	// Why a PR stopped awaiting review, on review_not_needed.
	Reason string `json:"reason,omitempty"`
}

// notNeededReason is why p, which was awaiting review at head asked, no
// longer is: "" while it still is.
func notNeededReason(p *WatchedPR, asked string) string {
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
// emitted (prDetails, reviewRecheck), so stale pass
// data never says a PR is waiting when it isn't. An error means not now.
func stillNeedsReview(p *WatchedPR) bool {
	f, err := ReviewRecheck(*p)
	if err != nil {
		state.LogActivity("pr watch: couldn't recheck %s before needs_review: %v", p.URL, err)
		return false
	}
	return f.Review != "approved" && f.Review != "changes_requested" && f.Head == p.Head && !f.Draft && f.Checks == "success"
}

func PrWatchStatePath() string {
	return filepath.Join(filepath.Dir(state.SettingsPath), "pr-watch.json")
}

func prWatchLockPath() string {
	return filepath.Join(filepath.Dir(state.SettingsPath), "pr-watch.lock")
}

// PrEventsPath is where events go, beside the update and activity logs.
func PrEventsPath() string {
	return filepath.Join(filepath.Dir(state.UpdateLogPath), "pr-events.jsonl")
}

// GhRun runs gh; tests stub it.
var GhRun = func(args ...string) ([]byte, error) {
	out, err := proc.RunBounded(30*time.Second, nil, "gh", args...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return out, err
}

// prWatchParallel is how many PRs one pass asks about at once.
const prWatchParallel = 4

// mergedKeep is how long a merged or closed PR stays in the state, and
// MergedWindow how far back the merged search looks: the window is always
// inside what's kept, so an old merge is never news again.
const (
	mergedKeep   = 7 * 24 * time.Hour
	MergedWindow = 3 * 24 * time.Hour
)

// LoadPRWatchState is the watcher's last record; ok is false when there's
// none (it hasn't run, or the file is unreadable).
func LoadPRWatchState() (s PrWatchState, ok bool) {
	b, err := os.ReadFile(PrWatchStatePath())
	if err != nil || json.Unmarshal(b, &s) != nil || s.PRs == nil {
		return PrWatchState{}, false
	}
	return s, true
}

func SavePRWatchState(s PrWatchState) error {
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	path := PrWatchStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LockPRWatch takes the single-instance lock, held until the process exits.
func LockPRWatch() (*os.File, error) {
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

// ccs watch: a foreground loop that watches one author's open PRs in one
// GitHub owner and appends an event line for each change (PrEventsPath), for
// a coordinating session to follow with tail -F. REST only: GitHub's GraphQL
// search hits RESOURCE_LIMITS_EXCEEDED across many PRs.

// RunWatch is `ccs watch`; it returns the exit code.
func RunWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	interval := fs.Duration("interval", 3*time.Minute, "time between passes")
	author := fs.String("author", "", "PR author (default: the user gh is logged in as)")
	owner := fs.String("owner", "", "GitHub owner (org or user) whose repos to watch (default: every owner)")
	once := fs.Bool("once", false, "make one pass and exit")
	noNudge := fs.Bool("no-nudge", false, "don't tell the sessions that claimed PRs (ccs claim) about failing checks, conflicts or new unanswered review threads")
	selfLogins := fs.String("self-logins", "", "comma-separated GitHub logins whose replies also answer a review thread (bots or co-authors acting for you), besides --author")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: ccs watch [flags]\n\nWatch an author's open PRs and append one JSON line per change to\n%s (and stdout).\nEvents: watch_started, new, ready, needs_review, review_not_needed, checks_failing, checks_passed,\nconflicts, threads_unresolved, threads_resolved, threads_unanswered, merged, closed; unless --no-nudge, nudged and nudge_failed (the live sessions that claimed a PR are told of its failing\nchecks, conflicts and new unanswered review threads). Only one ccs watch runs at a time.\n\n", PrEventsPath())
		fs.PrintDefaults()
	}
	for _, a := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && slices.Contains([]string{"review", "dry-run", "review-channel", "review-mention", "review-hook"}, name) {
			fmt.Fprintf(os.Stderr, "ccs watch: --%s is gone: %s\n", name, ReviewMoved)
			return 2
		}
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
	SetThreadSelves(*author, *selfLogins)
	lock, err := LockPRWatch()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer lock.Close()
	for {
		if err := WatchPass(*author, *owner, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "ccs watch:", err)
			state.LogActivity("pr watch: %v", err)
		} else {
			afterPass(!*noNudge)
		}
		if *once {
			return 0
		}
		time.Sleep(*interval)
	}
}

// afterPass runs what follows a successful pass: the nudges.
func afterPass(nudge bool) {
	if nudge {
		if err := NudgeTick(time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "ccs watch:", err)
			state.LogActivity("nudge: %v", err)
		}
	}
}

// WatchPass makes one pass: fetch, diff against the saved state, emit,
// save. With no saved state (or one for another author/owner) it only
// records what it finds and emits watch_started, so the first pass doesn't
// report every PR as news. A search that fails leaves the state untouched.
func WatchPass(author, owner string, now time.Time) error {
	old, ok := LoadPRWatchState()
	seeding := !ok || old.Author != author || old.Owner != owner
	if seeding {
		old = PrWatchState{Author: author, Owner: owner, Started: now, PRs: map[string]*WatchedPR{}}
	}
	open, err := searchPRs(author, owner, "--state", "open")
	if err != nil {
		return fmt.Errorf("open PRs: %w", err)
	}
	since := max(old.Started.Unix(), now.Add(-MergedWindow).Unix())
	merged, err := searchPRs(author, owner, "--merged-at", ">="+time.Unix(since, 0).UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("merged PRs: %w", err)
	}

	if !seeding && old.Version < 2 {
		// From before Awaiting was kept: a PR asked about at its current head
		// and still waiting was awaiting review.
		for _, p := range old.PRs {
			p.Awaiting = p.AskedHead != "" && p.AskedHead == p.Head && p.NeedsReview()
		}
	}
	next := PrWatchState{Author: author, Owner: owner, Started: old.Started, Updated: now, PRs: map[string]*WatchedPR{}, Version: PrWatchVersion}
	var events []PrEvent
	emit := func(p *WatchedPR, event string) {
		e := PrEvent{At: now.UTC().Format(time.RFC3339), Event: event, PR: p.URL, Repo: p.Repo, Number: p.Number, Title: p.Title, Head: p.Head}
		switch event {
		case "checks_failing":
			e.Checks = p.Failing
		case "ready", "needs_review":
			e.Review = p.Review
		case "threads_unresolved":
			e.Unresolved = *p.Threads
		case "threads_unanswered":
			e.Count, e.Threads = len(p.Unanswered), sortedKeys(p.Unanswered)
		}
		events = append(events, e)
	}
	// notNeeded emits review_not_needed for a PR that was awaiting review and
	// isn't now (n carries its state now, o what it was awaiting at).
	notNeeded := func(n, o *WatchedPR) {
		if o == nil || !o.Awaiting {
			return
		}
		why := notNeededReason(n, o.AskedHead)
		if why == "" {
			n.Awaiting = true
			return
		}
		n.Awaiting = false
		e := PrEvent{At: now.UTC().Format(time.RFC3339), Event: "review_not_needed", PR: n.URL, Repo: n.Repo, Number: n.Number, Title: n.Title, Head: o.AskedHead, Reason: why}
		events = append(events, e)
	}

	// Open PRs: details for each, in parallel, then diff.
	details := prDetailsPass(open, old.PRs, now)
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
				n.Threads, n.Unanswered = o.Threads, o.Unanswered
			}
		}
		next.PRs[p.URL] = n
		if seeding {
			// Awaiting review already: said now, so a consumer starting
			// with the watcher knows (once per head, as ever).
			if n.NeedsReview() && stillNeedsReview(n) {
				n.AskedHead, n.Awaiting = n.Head, true
				emit(n, "needs_review")
			}
			continue
		}
		notNeeded(n, o)
		prev := o
		if prev == nil || prev.State != "open" {
			emit(n, "new")
			prev = &WatchedPR{State: "open"}
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
			if newReplies(n.Unanswered, prev.Unanswered) {
				emit(n, "threads_unanswered")
			}
		}
		if n.ready() && !prev.ready() {
			emit(n, "ready")
		}
		if n.NeedsReview() && n.AskedHead != n.Head && stillNeedsReview(n) {
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
		ended, err := prEndState(o)
		if err != nil || ended == "open" { // unknown or still open (search lag): keep it as it was
			if err != nil {
				state.LogActivity("pr watch: %s: %v", url, err)
			}
			next.PRs[url] = o
			continue
		}
		n := *o
		n.State, n.Ended = ended, now.UTC().Format(time.RFC3339)
		next.PRs[url] = &n
		notNeeded(&n, o)
		emit(&n, ended)
	}

	if seeding {
		events = append([]PrEvent{{At: now.UTC().Format(time.RFC3339), Event: "watch_started", Review: fmt.Sprintf("%d open PRs by %s in %s", len(open), author, cmp.Or(owner, "any owner"))}}, events...)
	}
	if err := writePREvents(events); err != nil {
		return fmt.Errorf("writing events: %w", err)
	}
	return SavePRWatchState(next)
}

// writePREvents appends events to prEventsPath and prints them.
func writePREvents(events []PrEvent) error {
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
	path := PrEventsPath()
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

// GhUser is the login gh is signed in as. Stubbed in tests.
var GhUser = func() (string, error) {
	out, err := GhRun("api", "user", "--jq", ".login")
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
	login, err := GhUser()
	if err != nil || login == "" {
		return fmt.Errorf("--author not given and gh can't say who you are (gh auth login?): %v", err)
	}
	*author = login
	return nil
}

// searchPRs is gh search prs for author's PRs in owner's repos, with extra
// filters; blank or malformed entries are dropped.
func searchPRs(author, owner string, filters ...string) ([]WatchedPR, error) {
	args := []string{"search", "prs", "--author", author}
	if owner != "" { // else every owner's repos
		args = append(args, "--owner", owner)
	}
	args = append(append(args, "--sort", "updated", "--limit", "200", "--json", "url,title,number,repository,isDraft,updatedAt"), filters...)
	out, err := GhRun(args...)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		URL        string `json:"url"`
		Title      string `json:"title"`
		Number     int    `json:"number"`
		IsDraft    bool   `json:"isDraft"`
		UpdatedAt  string `json:"updatedAt"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("reading gh search: %w", err)
	}
	var prs []WatchedPR
	for _, r := range rows {
		if strings.TrimSpace(r.URL) == "" || !transcript.PrURL.MatchString(r.URL) {
			continue
		}
		repo := r.Repository.NameWithOwner
		if repo == "" {
			p := transcript.PrURL.FindStringSubmatch(r.URL)
			repo = p[1] + "/" + p[2]
		}
		prs = append(prs, WatchedPR{URL: r.URL, Repo: repo, Number: r.Number, Title: r.Title, Draft: r.IsDraft, UpdatedAt: r.UpdatedAt, State: "open"})
	}
	return prs, nil
}

// prDetails reads an open PR's head, draft, mergeability, checks and
// reviews over REST.
type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type commitStatus struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

type prReview struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	State string `json:"state"`
}

// ghMaxPages bounds ghPaged and the thread pages: past it a PR is unread,
// never taken as clear.
const ghMaxPages = 50

// ghPaged is every page of a REST list, 100 at a time until a short page;
// key picks the list out of an object page (check-runs, status).
func ghPaged[T any](path, key string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var all []T
	for page := 1; page <= ghMaxPages; page++ {
		u := fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page)
		var items []T
		if key == "" {
			if err := ghJSON(&items, u); err != nil {
				return nil, err
			}
		} else {
			var obj map[string]json.RawMessage
			if err := ghJSON(&obj, u); err != nil {
				return nil, err
			}
			if raw, ok := obj[key]; ok {
				if err := json.Unmarshal(raw, &items); err != nil {
					return nil, err
				}
			}
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("%s: more than %d pages", path, ghMaxPages)
}

func PrDetails(p WatchedPR) (*WatchedPR, error) {
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
		CheckRuns []checkRun `json:"check_runs"`
	}
	// Every page: a failing or pending check after the first 100 still counts.
	var err error
	if runs.CheckRuns, err = ghPaged[checkRun](fmt.Sprintf("%s/commits/%s/check-runs", base, n.Head), "check_runs"); err != nil {
		return nil, err
	}
	var status struct {
		Statuses []commitStatus `json:"statuses"`
	}
	if status.Statuses, err = ghPaged[commitStatus](fmt.Sprintf("%s/commits/%s/status", base, n.Head), "statuses"); err != nil {
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
	reviews, err := ghPaged[prReview](fmt.Sprintf("%s/pulls/%d/reviews", base, p.Number), "") // every page: a later approval counts
	if err != nil {
		return nil, err
	}
	n.threadsUnknown = true
	if n.Checks == "success" && !n.Draft { // only then could it be ready or need a review
		if c, unanswered, err := unresolvedThreads(p); err != nil {
			state.LogActivity("pr watch: %s: counting review threads: %v", p.URL, err)
		} else {
			n.Threads, n.Unanswered, n.threadsUnknown = &c, unanswered, false
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

// threadSelves is the operator: the logins whose replies answer a review
// thread. The gh user ccs watch runs as (--author) and --self-logins.
var threadSelves = map[string]bool{}

// SetThreadSelves sets threadSelves from the author and a comma-separated list.
func SetThreadSelves(author, extra string) {
	threadSelves = map[string]bool{}
	for _, l := range append([]string{author}, strings.Split(extra, ",")...) {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			threadSelves[l] = true
		}
	}
}

// unresolvedThreads is how many of a PR's review threads are unresolved,
// and of those the unanswered: thread id to its last comment's id, where
// that comment isn't the operator's (threadSelves). A small GraphQL query a
// page (REST doesn't say), every page read.
func unresolvedThreads(p WatchedPR) (int, map[string]int64, error) {
	owner, name, _ := strings.Cut(p.Repo, "/")
	n := 0
	unanswered := map[string]int64{}
	after := ""
	for range ghMaxPages {
		args := []string{"api", "graphql",
			"-f", "query=query($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$after){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{databaseId author{login}}}}}}}}",
			"-F", "owner=" + owner, "-F", "name=" + name, "-F", fmt.Sprintf("number=%d", p.Number)}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		out, err := GhRun(args...)
		if err != nil {
			return 0, nil, err
		}
		var resp struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewThreads struct {
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
							Nodes []struct {
								ID         string `json:"id"`
								IsResolved bool   `json:"isResolved"`
								Comments   struct {
									Nodes []struct {
										DatabaseID int64 `json:"databaseId"`
										Author     *struct {
											Login string `json:"login"`
										} `json:"author"`
									} `json:"nodes"`
								} `json:"comments"`
							} `json:"nodes"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return 0, nil, err
		}
		if resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
			return 0, nil, errors.New("pull request not found")
		}
		rt := resp.Data.Repository.PullRequest.ReviewThreads
		for _, t := range rt.Nodes {
			if t.IsResolved {
				continue
			}
			n++
			if c := t.Comments.Nodes; len(c) > 0 {
				last := c[len(c)-1]
				if last.Author == nil || !threadSelves[strings.ToLower(last.Author.Login)] {
					unanswered[t.ID] = last.DatabaseID
				}
			}
		}
		if !rt.PageInfo.HasNextPage {
			return n, unanswered, nil
		}
		after = rt.PageInfo.EndCursor
	}
	return 0, nil, fmt.Errorf("more than %d pages of review threads", ghMaxPages)
}

// newReplies: now has a thread, or a later reviewer comment on one, that
// before didn't.
func newReplies(now, before map[string]int64) bool {
	for id, c := range now {
		if b, ok := before[id]; !ok || b != c {
			return true
		}
	}
	return false
}

// sortedKeys is m's keys in order.
func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// prDetailsAll is prDetails for each PR, prWatchParallel at a time; a PR
// that couldn't be read is nil (and logged).
func prDetailsAll(open []WatchedPR) []*WatchedPR {
	details := make([]*WatchedPR, len(open))
	var wg sync.WaitGroup
	sem := make(chan struct{}, prWatchParallel)
	for i, p := range open {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := PrDetails(p)
			if err != nil {
				state.LogActivity("pr watch: %s skipped this pass: %v", p.URL, err)
				return
			}
			details[i] = d
		}()
	}
	wg.Wait()
	return details
}

// PrFullEvery is how long an unchanged PR with settled checks goes without its
// details being read. A re-run check or a conflict from the base moving doesn't
// change updatedAt, so those can be noticed this late.
const PrFullEvery = 15 * time.Minute

// prSettled: what the search shows hasn't changed since old was read, and old
// isn't waiting on anything a pass needs to see, so old can stand in for it.
func prSettled(old *WatchedPR, p WatchedPR, now time.Time) bool {
	if old == nil || old.State != "open" || old.Head == "" || p.UpdatedAt == "" || p.UpdatedAt != old.UpdatedAt || now.Sub(old.FullAt) >= PrFullEvery {
		return false
	}
	// a draft's threads are never counted, so it can't wait for them
	return old.Checks == "failing" || (old.Checks == "success" && (old.Threads != nil || old.Draft))
}

// prDetailsPass is prDetailsAll for one pass, reading only the PRs that aren't
// settled; the rest keep what old says, with the search's title.
func prDetailsPass(open []WatchedPR, old map[string]*WatchedPR, now time.Time) []*WatchedPR {
	details := make([]*WatchedPR, len(open))
	var read []WatchedPR
	var at []int
	for i, p := range open {
		if o := old[p.URL]; prSettled(o, p, now) {
			n := *o
			n.Title, n.Draft = p.Title, p.Draft
			n.conflictsUnknown, n.threadsUnknown = true, true // the last answers stand
			details[i] = &n
			continue
		}
		read, at = append(read, p), append(at, i)
	}
	for j, d := range prDetailsAll(read) {
		if d != nil {
			d.FullAt = now
		}
		details[at[j]] = d
	}
	return details
}

// prEndState is "merged", "closed" or "open" for a PR the open search no
// longer lists.
func prEndState(p *WatchedPR) (string, error) {
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
	out, err := GhRun("api", path)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

// The PR owner registry: a session runs `ccs claim <PR>` for each PR it
// owns, and `ccs prs` lists the author's open PRs with their owners, flagging
// PRs nobody owns and PRs two live sessions both claim. Claims of merged or
// closed PRs, and of sessions no longer running, are dropped on every read.

// PrClaim is one session's claim on a PR.
type PrClaim struct {
	Session   string    `json:"session"`          // Claude Code session id, or a Codex thread id
	Kind      string    `json:"kind,omitempty"`   // "codex" for a Codex session, else Claude Code
	Thread    string    `json:"thread,omitempty"` // a Codex session's thread id
	Name      string    `json:"name"`             // the session's name when it claimed
	PID       int       `json:"pid"`              // its claude process
	ProcStart string    `json:"proc_start"`       // that process's start, as its session file says: a recycled pid isn't it
	ClaimedAt time.Time `json:"claimed_at"`
	Notify    bool      `json:"notify,omitempty"` // reserved: the session asks to be told of its PR's failures
}

// ClaimsFile is claimsPath: each PR's claims, in the order they were made.
type ClaimsFile struct {
	Claims map[string][]PrClaim `json:"claims"`
	// Checked is when each claimed PR outside the watcher's state was last
	// asked about, so every session's status line shares one check.
	Checked map[string]time.Time `json:"checked,omitempty"`
}

func ClaimsPath() string { return filepath.Join(filepath.Dir(state.SettingsPath), "claims.json") }

func claimsLockPath() string { return filepath.Join(filepath.Dir(state.SettingsPath), "claims.lock") }

// prsFresh is how old the watcher's state may be before ccs prs asks GitHub itself.
const prsFresh = 10 * time.Minute

// EndCheckEvery is how often a claimed PR outside the watcher's state is asked
// about, across all sessions: ccs prs runs per session, about once a minute.
const EndCheckEvery = 10 * time.Minute

// dueEndChecks stamps and returns the claimed PRs in urls not asked about
// within endCheckEvery. Stamping under the lock makes one caller do each check,
// and a failed check waits too, so a refused request isn't retried every run.
func dueEndChecks(urls []string, now time.Time) ([]string, error) {
	var due []string
	err := WithClaims(func(c *ClaimsFile) bool {
		for _, u := range urls {
			if _, claimed := c.Claims[u]; !claimed || now.Sub(c.Checked[u]) < EndCheckEvery {
				continue
			}
			if c.Checked == nil {
				c.Checked = map[string]time.Time{}
			}
			c.Checked[u] = now
			due = append(due, u)
		}
		return len(due) > 0
	})
	return due, err
}

// WithClaims runs f on the claims under an exclusive lock and saves them if
// f says they changed (a unique temp file, then a rename), so sessions
// claiming at once never lose each other's claims.
func WithClaims(f func(c *ClaimsFile) bool) error {
	path := ClaimsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(claimsLockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	c := LoadClaims()
	if !f(&c) {
		return nil
	}
	data, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "claims-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// LoadClaims reads the claims without the lock (a reader sees the old or the
// new file, never half of one).
func LoadClaims() ClaimsFile {
	var c ClaimsFile
	if b, err := os.ReadFile(ClaimsPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.Claims == nil {
		c.Claims = map[string][]PrClaim{}
	}
	return c
}

// ParentPID is pid's parent, 0 when unknown. Stubbed in tests.
var ParentPID = func(pid int) int {
	out, err := proc.RunBounded(2*time.Second, nil, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return p
}

// LiveByPID is each running claude session by its pid, as scanSessions
// checks them (the pid's start time matches its session file). Stubbed in tests.
var LiveByPID = func() map[int]PrClaim {
	out := map[int]PrClaim{}
	for id, s := range sessions.ScanSessions(nil) {
		out[s.Pid] = PrClaim{Session: id, Name: s.Stat.Name, PID: s.Pid, ProcStart: s.Start}
	}
	return out
}

// callingSession is the claude session this process runs under: the first
// ancestor that is a live session's claude process.
func callingSession() (PrClaim, error) {
	if c, ok := codexSession(); ok {
		return c, nil
	}
	live := LiveByPID()
	pid := os.Getppid()
	for range 30 {
		if pid <= 1 {
			break
		}
		if s, ok := live[pid]; ok {
			return s, nil
		}
		pid = ParentPID(pid)
	}
	return PrClaim{}, errors.New("not run from a Claude Code or Codex session: ccs claim must be run by the session that owns the PR")
}

// claimLive: the claim's session still runs in the same process (a Codex
// session: codexLive).
func claimLive(c PrClaim, live map[int]PrClaim) bool {
	if c.Kind == "codex" {
		return sessions.CodexLive(c.Thread)
	}
	s, ok := live[c.PID]
	return ok && s.Session == c.Session && s.ProcStart == c.ProcStart
}

// ownerName is the name c's session goes by now: a /rename after the claim
// changes the session file's name, never the claim's session or pid.
func ownerName(c PrClaim, live map[int]PrClaim) string {
	if s, ok := live[c.PID]; ok && c.Kind != "codex" && s.Session == c.Session && s.Name != "" {
		return s.Name
	}
	return c.Name
}

// Claim records (or with release, drops) the calling session's Claim on pr.
// Another session's Claim on it stays: two owners is something to flag.
func Claim(pr string, release bool, now time.Time) (string, error) {
	if !transcript.PrURL.MatchString(pr) {
		return "", fmt.Errorf("not a GitHub PR address: %s", pr)
	}
	me, err := callingSession()
	if err != nil {
		return "", err
	}
	var msg string
	err = WithClaims(func(c *ClaimsFile) bool {
		mine := func(x PrClaim) bool { return x.Session == me.Session }
		has := slices.ContainsFunc(c.Claims[pr], mine)
		if release {
			if !has {
				msg = fmt.Sprintf("%s hadn't claimed %s", me.Name, pr)
				return false
			}
			c.Claims[pr] = slices.DeleteFunc(slices.Clone(c.Claims[pr]), mine)
			if len(c.Claims[pr]) == 0 {
				delete(c.Claims, pr)
			}
			msg = fmt.Sprintf("%s released %s", me.Name, pr)
			return true
		}
		me.ClaimedAt = now
		if has { // the same session again: refresh its process and name
			i := slices.IndexFunc(c.Claims[pr], mine)
			me.Notify = c.Claims[pr][i].Notify
			c.Claims[pr][i] = me
		} else {
			c.Claims[pr] = append(c.Claims[pr], me)
		}
		msg = fmt.Sprintf("%s owns %s", me.Name, pr)
		if others := len(c.Claims[pr]) - 1; others > 0 {
			msg += fmt.Sprintf(" (also claimed by %d other session(s): see ccs prs)", others)
		}
		return true
	})
	return msg, err
}

// PrRow is one line of ccs prs.
type PrRow struct {
	PR     string   `json:"pr"`
	Repo   string   `json:"repo"`
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Owners []string `json:"owners"` // names of the live sessions claiming it
	// OwnersDetail is the same owners with their kind ("claude" or "codex")
	// and id (the Claude Code session id, or the Codex thread id).
	OwnersDetail []PrOwner `json:"owners_detail,omitempty"`
	Head         string    `json:"head,omitempty"` // head commit SHA
	// NeedsReview: awaiting review now (watchedPR.needsReview, as the
	// needs_review event); from a fresh fetch, the same pass data.
	NeedsReview bool   `json:"needs_review"`
	Group       string `json:"group"`  // ready, needs_you, waiting
	Status      string `json:"status"` // why, as the list says it
	Unowned     bool   `json:"unowned,omitempty"`
	Contested   bool   `json:"contested,omitempty"` // claimed by two or more live sessions
}

// PrOwner is one live session claiming a PR.
type PrOwner struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Session string `json:"session"`
}

// OwnerOf is c as a prOwner: a Codex claim's id is its thread.
func OwnerOf(c PrClaim, live map[int]PrClaim) PrOwner {
	o := PrOwner{Name: ownerName(c, live), Kind: "claude", Session: c.Session}
	if c.Kind == "codex" {
		o.Kind = "codex"
		if c.Thread != "" {
			o.Session = c.Thread
		}
	}
	return o
}

// OwnedBy keeps the PRs the session of that kind ("claude": its session
// id; "codex": its thread id) claims; a claim of the other kind never matches.
func (r PrsReport) OwnedBy(kind, id string) PrsReport {
	out := r
	out.PRs = nil
	for _, p := range r.PRs {
		if slices.ContainsFunc(p.OwnersDetail, func(o PrOwner) bool { return o.Kind == kind && o.Session == id }) {
			out.PRs = append(out.PRs, p)
		}
	}
	return out
}

// sessionLine is --statusline for one session: its PRs by group, "PRs ✓1
// ✗1 2" (ready, needing you, waiting), empty when it claims none.
func (r PrsReport) sessionLine() string {
	n := map[string]int{}
	for _, p := range r.PRs {
		n[p.Group]++
	}
	var parts []string
	if n["ready"] > 0 {
		parts = append(parts, fmt.Sprintf("✓%d", n["ready"]))
	}
	if n["needs_you"] > 0 {
		parts = append(parts, fmt.Sprintf("✗%d", n["needs_you"]))
	}
	if n["waiting"] > 0 {
		parts = append(parts, fmt.Sprint(n["waiting"]))
	}
	if len(parts) == 0 {
		return ""
	}
	s := "PRs " + strings.Join(parts, " ")
	if r.Stale {
		s += " (stale)"
	}
	return s
}

// PrsReport is ccs prs: the author's open PRs and how fresh the answer is.
type PrsReport struct {
	Updated time.Time `json:"updated"` // when GitHub was last asked
	Stale   bool      `json:"stale,omitempty"`
	PRs     []PrRow   `json:"prs"`
}

// status is the PR's group and why, by the list's rules (prInfo.status): a
// failed check, optional or not, needs you.
func (p *WatchedPR) status() (int, string) {
	switch {
	case p.Draft:
		return PrWaiting, "draft"
	case p.Conflicts:
		return PrFix, "conflicts"
	case p.Checks == "failing":
		return PrFix, "checks failing: " + strings.Join(p.Failing, ", ")
	case p.Threads != nil && *p.Threads > 0:
		return PrFix, threadsText(*p.Threads)
	case p.Review == "changes_requested":
		return PrFix, "changes requested"
	case p.Checks == "pending":
		return PrWaiting, "checks running"
	case p.Review == "approved":
		return PrReady, "ready to merge"
	}
	return PrWaiting, "review required"
}

var groupNames = map[int]string{PrReady: "ready", PrFix: "needs_you", PrWaiting: "waiting"}

// FetchOpenPRs asks GitHub (REST) for the author's open PRs; one that
// couldn't be read is left out. Stubbed in tests.
var FetchOpenPRs = func(author, owner string) (map[string]*WatchedPR, error) {
	open, err := searchPRs(author, owner, "--state", "open")
	if err != nil {
		return nil, err
	}
	out := map[string]*WatchedPR{}
	for _, d := range prDetailsAll(open) {
		if d != nil {
			out[d.URL] = d
		}
	}
	return out, nil
}

// EndState is prEndState for a claimed PR the open list doesn't have. Stubbed in tests.
var EndState = func(pr string) (string, error) {
	p := transcript.PrURL.FindStringSubmatch(pr)
	n, _ := strconv.Atoi(p[3])
	return prEndState(&WatchedPR{Repo: p[1] + "/" + p[2], Number: n})
}

// PrsList builds ccs prs. With offline, it never asks GitHub: the watcher's
// state however old (Stale says so), and claims are pruned only of ended
// sessions and of PRs the state knows merged or closed.
func PrsList(author, owner string, offline bool, now time.Time) (PrsReport, error) {
	st, ok := LoadPRWatchState()
	ok = ok && st.Author == author && st.Owner == owner
	var rep PrsReport
	open := map[string]*WatchedPR{}
	ended := map[string]bool{}
	switch {
	case ok && (offline || now.Sub(st.Updated) < prsFresh):
		rep.Updated, rep.Stale = st.Updated, now.Sub(st.Updated) >= prsFresh
		for u, p := range st.PRs {
			if p.State == "open" {
				open[u] = p
			} else {
				ended[u] = true
			}
		}
	case offline:
		return rep, nil
	default:
		fetched, err := FetchOpenPRs(author, owner)
		if err != nil {
			return rep, err
		}
		open, rep.Updated = fetched, now
	}

	live := LiveByPID()
	claims := LoadClaims()
	// A claimed PR not open in the list has ended, or isn't the author's:
	// ask, at most every endCheckEvery (online only), so ended ones are dropped.
	if !offline {
		var unknown []string
		for u := range claims.Claims {
			if open[u] == nil && !ended[u] {
				unknown = append(unknown, u)
			}
		}
		due, err := dueEndChecks(unknown, now)
		if err != nil {
			state.LogActivity("ccs prs: couldn't record PR checks: %v", err)
		}
		for _, u := range due {
			if s, err := EndState(u); err == nil && s != "open" {
				ended[u] = true
			}
		}
	}
	prune := func(c *ClaimsFile) bool {
		changed := false
		for u, cs := range c.Claims {
			kept := slices.DeleteFunc(slices.Clone(cs), func(x PrClaim) bool { return !claimLive(x, live) })
			for i := range kept {
				if n := ownerName(kept[i], live); n != kept[i].Name {
					kept[i].Name = n // renamed since it claimed
					changed = true
				}
			}
			if ended[u] || len(kept) == 0 {
				delete(c.Claims, u)
				delete(c.Checked, u)
				changed = true
			} else if changed || len(kept) != len(cs) {
				c.Claims[u] = kept
				changed = true
			}
		}
		return changed
	}
	if prune(&claims) {
		if err := WithClaims(prune); err != nil {
			state.LogActivity("ccs prs: couldn't prune claims: %v", err)
		}
	}

	for u, p := range open {
		g, why := p.status()
		row := PrRow{PR: u, Repo: p.Repo, Number: p.Number, Title: p.Title, Head: p.Head, NeedsReview: p.NeedsReview(), Group: groupNames[g], Status: why}
		for _, c := range claims.Claims[u] {
			o := OwnerOf(c, live)
			row.Owners = append(row.Owners, o.Name)
			row.OwnersDetail = append(row.OwnersDetail, o)
		}
		row.Unowned = len(row.Owners) == 0
		row.Contested = len(row.Owners) > 1
		rep.PRs = append(rep.PRs, row)
	}
	order := map[string]int{"needs_you": 0, "ready": 1, "waiting": 2}
	slices.SortFunc(rep.PRs, func(a, b PrRow) int {
		if d := order[a.Group] - order[b.Group]; d != 0 {
			return d
		}
		return strings.Compare(a.PR, b.PR)
	})
	return rep, nil
}

// StatusLine is ccs prs --statusline: counts only, "" when there's nothing.
func (r PrsReport) StatusLine() string {
	var ready, fix, unowned, contested int
	for _, p := range r.PRs {
		switch p.Group {
		case "ready":
			ready++
		case "needs_you":
			fix++
		}
		if p.Unowned {
			unowned++
		}
		if p.Contested {
			contested++
		}
	}
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(ready, "ready")
	add(fix, "need you")
	add(unowned, "unowned")
	add(contested, "contested")
	if len(parts) == 0 {
		return ""
	}
	s := "PRs " + strings.Join(parts, " · ")
	if r.Stale {
		s += " (stale)"
	}
	return s
}

// Text is ccs prs for people: one line per PR, needing you first.
func (r PrsReport) Text() string {
	if len(r.PRs) == 0 {
		return "no open PRs\n"
	}
	marks := map[string]string{"ready": "✓", "needs_you": "✗", "waiting": "·"}
	var b strings.Builder
	for _, p := range r.PRs {
		owner := strings.Join(p.Owners, ", ")
		switch {
		case p.Unowned:
			owner = "UNOWNED"
		case p.Contested:
			owner = "CONTESTED: " + owner
		}
		repo := p.Repo[strings.LastIndex(p.Repo, "/")+1:]
		fmt.Fprintf(&b, "%s %s#%d  %s  [%s]  %s\n   %s\n", marks[p.Group], repo, p.Number, p.Status, owner, p.Title, p.PR)
	}
	if r.Stale {
		fmt.Fprintf(&b, "(as of %s: ccs watch isn't keeping it fresh)\n", r.Updated.Local().Format("15:04"))
	}
	return b.String()
}

// RunClaim is `ccs claim <PR URL> [--release]`.
func RunClaim(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	release := fs.Bool("release", false, "drop this session's claim instead")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: ccs claim <PR URL> [--release]\n\nRecord that the Claude Code session running this command owns the PR, for ccs prs.\nClaims end when the PR merges or closes, or the session stops.\n\n")
		fs.PrintDefaults()
	}
	var urls []string
	for len(args) > 0 { // flags may come after the URL
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		args = fs.Args()
		if len(args) > 0 {
			urls, args = append(urls, args[0]), args[1:]
		}
	}
	if len(urls) != 1 {
		fs.Usage()
		return 2
	}
	msg, err := Claim(urls[0], *release, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "ccs claim:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// RunPRs is `ccs prs [--json] [--statusline]`.
func RunPRs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	line := fs.Bool("statusline", false, "print one short line of counts from saved state, without asking GitHub")
	author := fs.String("author", "", "PR author (default: the user gh is logged in as, or the running ccs watch's)")
	owner := fs.String("owner", "", "GitHub owner (org or user) whose repos to list (default: every owner, or the running ccs watch's)")
	claudeID := fs.String("claude-session-id", "", "only the PRs this Claude Code session claims")
	codexID := fs.String("codex-session-id", "", "only the PRs this Codex session (thread id) claims")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: ccs prs [flags]\n\nList the author's open PRs with the session that owns each (ccs claim),\nflagging unowned PRs and PRs claimed by two live sessions. Uses ccs watch's\nstate when it is under %s old, else asks GitHub.\n\n", prsFresh)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	// Defaults: the running ccs watch's author and owner (no network, for
	// --statusline), else the gh user's PRs in every owner's repos.
	if *author == "" {
		ownerSet := false
		fs.Visit(func(f *flag.Flag) { ownerSet = ownerSet || f.Name == "owner" })
		if st, ok := LoadPRWatchState(); ok && (!ownerSet || st.Owner == *owner) {
			*author, *owner = st.Author, st.Owner
		} else if *line {
			return 0 // nothing saved to count, and no network
		} else if err := defaultAuthor(author); err != nil {
			fmt.Fprintln(stderr, "ccs prs:", err)
			return 1
		}
	}
	rep, err := PrsList(*author, *owner, *line, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "ccs prs:", err)
		return 1
	}
	one := *claudeID != "" || *codexID != ""
	if *claudeID != "" {
		rep = rep.OwnedBy("claude", *claudeID)
	}
	if *codexID != "" {
		rep = rep.OwnedBy("codex", *codexID)
	}
	switch {
	case *line && one:
		if s := rep.sessionLine(); s != "" {
			fmt.Fprintln(stdout, s)
		}
	case *line:
		if s := rep.StatusLine(); s != "" {
			fmt.Fprintln(stdout, s)
		}
	case *asJSON:
		if rep.PRs == nil {
			rep.PRs = []PrRow{}
		}
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(b))
	default:
		fmt.Fprint(stdout, rep.Text())
	}
	return 0
}

// Codex sessions: ccs launch --codex starts one, ccs claim works inside one
// (Codex gives every command it runs CODEX_THREAD_ID), and ccs watch nudges
// one with `codex queue`, which delivers into a running Codex tab. Codex's
// sandbox lets a command neither run ps nor reach a socket, so a Codex
// claim is keyed by thread id, never by process, and its liveness is worked
// out by ccs watch from outside the sandbox (sessions.CodexLive).

// codexSession is the Codex session this process runs in, from the
// CODEX_THREAD_ID Codex gives every command; ok false outside Codex.
func codexSession() (PrClaim, bool) {
	thread := os.Getenv("CODEX_THREAD_ID")
	if !transcript.CodexThreadID.MatchString(thread) {
		return PrClaim{}, false
	}
	c := PrClaim{Session: thread, Kind: "codex", Thread: thread}
	if m, ok := transcript.ReadCodexMeta(transcript.CodexRollout(thread)); ok {
		c.Name = transcript.CodexLaunchName(m)
	}
	if c.Name == "" {
		c.Name = sessions.CodexThreadName(thread)
	}
	if c.Name == "" {
		c.Name = thread
	}
	return c, true
}

// PrsOf is conv's PRs and then its subagents' not among them.
func PrsOf(conv transcript.Conversation) []string {
	sub := subagentPRs(conv)
	if len(sub) == 0 {
		return conv.PRs
	}
	urls := slices.Clone(conv.PRs)
	for _, u := range sub {
		if !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	return urls
}

// subagentPRsTTL is how long subagentPRs trusts what it last read.
const subagentPRsTTL = 10 * time.Second

var SubPRCache = struct {
	sync.Mutex
	Dirs  map[string]subPRsRead
	files map[string]*subPRFile
}{Dirs: map[string]subPRsRead{}, files: map[string]*subPRFile{}}

type subPRsRead struct {
	at  time.Time
	prs []string
}

// subPRFile is how far one subagent transcript has been read: its complete
// lines up to off, the gh calls still awaiting output, and the PRs found.
type subPRFile struct {
	off     int64
	pending []string
	prs     []string
}

// subagentPRs is the PRs conv's subagents opened or merged (ghPRCall),
// reread at most every subagentPRsTTL and then only what each transcript
// appended (the list asks on every frame).
func subagentPRs(conv transcript.Conversation) []string {
	dir := filepath.Join(strings.TrimSuffix(conv.FilePath, ".jsonl"), "subagents")
	SubPRCache.Lock()
	defer SubPRCache.Unlock()
	if r, ok := SubPRCache.Dirs[dir]; ok && time.Since(r.at) < subagentPRsTTL {
		return r.prs
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	more, _ := filepath.Glob(filepath.Join(dir, "workflows", "*", "agent-*.jsonl"))
	var prs []string
	for _, path := range append(paths, more...) {
		f := SubPRCache.files[path]
		if f == nil {
			f = &subPRFile{}
			SubPRCache.files[path] = f
		}
		f.read(path)
		for _, u := range f.prs {
			if !slices.Contains(prs, u) {
				prs = append(prs, u)
			}
		}
	}
	SubPRCache.Dirs[dir] = subPRsRead{time.Now(), prs}
	return prs
}

// read takes in what path appended since the last read; a file that shrank
// is read again from the start.
func (f *subPRFile) read(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || info.Size() == f.off {
		return
	} else if info.Size() < f.off {
		*f = subPRFile{}
	}
	if _, err := file.Seek(f.off, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(file, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil { // an unfinished line waits for the next read
			return
		}
		f.off += int64(len(line))
		if bytes.Contains(line, []byte("gh pr ")) || len(f.pending) > 0 && bytes.Contains(line, []byte(`"tool_result"`)) {
			f.line(line)
		}
	}
}

// line notes a line's gh calls and takes the PRs from their output.
func (f *subPRFile) line(line []byte) {
	var rec struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || len(rec.Message.Content) == 0 || rec.Message.Content[0] != '[' {
		return
	}
	var blocks []struct {
		Type      string                   `json:"type"`
		ID        string                   `json:"id"`
		Name      string                   `json:"name"`
		Input     struct{ Command string } `json:"input"`
		ToolUseID string                   `json:"tool_use_id"`
		Content   json.RawMessage          `json:"content"`
	}
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch {
		case b.Type == "tool_use" && b.Name == "Bash" && transcript.GhPRCall(b.Input.Command):
			f.pending = append(f.pending, b.ID)
		case b.Type == "tool_result" && slices.Contains(f.pending, b.ToolUseID):
			f.pending = slices.DeleteFunc(f.pending, func(id string) bool { return id == b.ToolUseID })
			for _, u := range transcript.GhOutputPRs(transcript.ResultText(b.Content)) {
				if transcript.PrURL.MatchString(u) && !slices.Contains(f.prs, u) {
					f.prs = append(f.prs, u)
				}
			}
		}
	}
}

// PrInfo is what GitHub says about a PR.
type PrInfo struct {
	Number     int
	Title      string
	State      string // OPEN, MERGED, CLOSED
	Draft      bool
	MergeState string   // mergeStateStatus: CLEAN, UNSTABLE, BLOCKED, BEHIND, DIRTY, DRAFT, HAS_HOOKS, UNKNOWN
	Review     string   // reviewDecision: APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED or ""
	Checks     string   // the head commit's check rollup: SUCCESS, FAILURE, ERROR, PENDING, EXPECTED or ""
	Failing    []string // the names of its failing checks, first seen first
	Unresolved int      // its unresolved review threads (of the first 100)
	// ThreadsUnread: more threads than the batch reads and none of those
	// unresolved, so it can't be called clear (the watcher reads them all).
	ThreadsUnread bool
}

// threadsText is "1 unresolved thread" or "N unresolved threads".
func threadsText(n int) string {
	if n == 1 {
		return "1 unresolved thread"
	}
	return fmt.Sprintf("%d unresolved threads", n)
}

// failingNames is the failing checks for a status line: at most three, then +N.
func (p PrInfo) failingNames() string {
	if len(p.Failing) == 0 {
		return ""
	}
	names := p.Failing[:min(len(p.Failing), 3)]
	s := ": " + strings.Join(names, ", ")
	if more := len(p.Failing) - len(names); more > 0 {
		s += fmt.Sprintf(" +%d", more)
	}
	return s
}

func (p PrInfo) Final() bool { return p.State == "MERGED" || p.State == "CLOSED" }

// PR groups, in the order a session lists them: what you can act on first,
// merged and closed last.
const (
	PrReady   = iota // ready to merge
	PrFix            // needs you: failing checks, conflicts, changes requested, behind
	PrWaiting        // checks running, review required, draft, or not looked up yet
	PrMerged
	PrClosed
)

// Status is p's group and what to say about it. GitHub's mergeStateStatus is
// the verdict on "ready to merge" (branch protection, required checks and
// reviews); the rest only explains why not.
func (p PrInfo) Status() (int, string) {
	switch {
	case p.State == "MERGED":
		return PrMerged, "merged"
	case p.State == "CLOSED":
		return PrClosed, "closed"
	case p.State == "GONE":
		return PrClosed, "not found: deleted, or gh can't see it"
	case p.State == "":
		return PrWaiting, "checking…"
	case p.Draft:
		return PrWaiting, "draft"
	case p.MergeState == "DIRTY":
		return PrFix, "conflicts"
	case p.MergeState == "UNSTABLE" && len(p.Failing) > 0:
		// Mergeable, but a failed check often skips what follows it (build, deploy).
		return PrFix, "optional checks failing" + p.failingNames()
	case p.Checks == "FAILURE" || p.Checks == "ERROR":
		return PrFix, "checks failing" + p.failingNames()
	case p.Unresolved > 0: // never ready while a review thread is open, even if GitHub would merge it
		return PrFix, threadsText(p.Unresolved)
	case p.ThreadsUnread:
		return PrWaiting, "over 100 review threads: see them on GitHub"
	case p.MergeState == "CLEAN" || p.MergeState == "HAS_HOOKS":
		return PrReady, "ready to merge"
	case p.MergeState == "UNSTABLE" && (p.Checks == "PENDING" || p.Checks == "EXPECTED"):
		// UNSTABLE is any optional check not passed yet, running included.
		return PrReady, "ready to merge · optional checks running"
	case p.MergeState == "UNSTABLE":
		return PrReady, "ready to merge"
	case p.Review == "CHANGES_REQUESTED":
		return PrFix, "changes requested"
	case p.MergeState == "BEHIND":
		return PrFix, "behind its base"
	case p.Checks == "PENDING" || p.Checks == "EXPECTED":
		return PrWaiting, "checks running"
	case p.Review == "REVIEW_REQUIRED":
		return PrWaiting, "review required"
	case p.MergeState == "UNKNOWN":
		return PrWaiting, "github is checking it"
	}
	return PrWaiting, "blocked"
}

// MaxPRLines is how many PRs the preview header lists; ^O lists them all.
const MaxPRLines = 5

// PrName is "repo#N" from a PR address.
func PrName(u string) string {
	if p := transcript.PrURL.FindStringSubmatch(u); p != nil {
		return p[2] + "#" + p[3]
	}
	return u
}

// GhFetchPRs is fetchPRs when gh is installed: nil otherwise, so PRs are
// never looked up.
func GhFetchPRs() func([]string) (map[string]PrInfo, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return nil
	}
	return func(urls []string) (map[string]PrInfo, error) { return GhPRBatch(gh, urls) }
}

// PrBatch is how many PRs one GraphQL request asks about.
const PrBatch = 40

// ghPRBatch asks about urls in one GraphQL request (each PR an aliased
// field). A PR that's gone or private comes back null with an error for that
// field only, and gh exits non-zero, so the data is read whatever the exit.
// failedCheck is the check-run conclusions GitHub's rollup counts as failing.
var failedCheck = map[string]bool{"FAILURE": true, "TIMED_OUT": true, "CANCELLED": true, "ACTION_REQUIRED": true, "STARTUP_FAILURE": true}

func GhPRBatch(gh string, urls []string) (map[string]PrInfo, error) {
	var q strings.Builder
	q.WriteString("query{")
	for i, u := range urls {
		p := transcript.PrURL.FindStringSubmatch(u) // validated when parsed; owner and repo are [A-Za-z0-9_.-]
		fmt.Fprintf(&q, `p%d:repository(owner:%q,name:%q){pullRequest(number:%s){number title state isDraft mergeStateStatus reviewDecision reviewThreads(first:100){totalCount nodes{isResolved}} commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:50){nodes{...on CheckRun{name conclusion} ...on StatusContext{context state}}}}}}}}}`, i, p[1], p[2], p[3])
	}
	q.WriteString("}")
	raw, runErr := proc.RunBounded(30*time.Second, []string{"GH_PROMPT_DISABLED=1"}, gh, "api", "graphql", "-f", "query="+q.String())
	var resp struct {
		Data map[string]*struct {
			PullRequest *struct {
				Number           int
				Title            string
				State            string
				IsDraft          bool
				MergeStateStatus string
				ReviewDecision   string
				ReviewThreads    struct {
					TotalCount int
					Nodes      []struct{ IsResolved bool }
				}
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								State    string
								Contexts struct {
									Nodes []struct {
										Name, Conclusion string // a check run
										Context, State   string // a commit status
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, err
	}
	out := map[string]PrInfo{}
	for i, u := range urls {
		r, answered := resp.Data[fmt.Sprintf("p%d", i)]
		if !answered {
			continue
		}
		if r == nil || r.PullRequest == nil { // deleted, or gh's account can't see it
			out[u] = PrInfo{State: "GONE"}
			continue
		}
		pr := r.PullRequest
		info := PrInfo{Number: pr.Number, Title: pr.Title, State: pr.State, Draft: pr.IsDraft, MergeState: pr.MergeStateStatus, Review: pr.ReviewDecision}
		for _, t := range pr.ReviewThreads.Nodes {
			if !t.IsResolved {
				info.Unresolved++
			}
		}
		info.ThreadsUnread = info.Unresolved == 0 && pr.ReviewThreads.TotalCount > len(pr.ReviewThreads.Nodes)
		if n := pr.Commits.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
			roll := n[0].Commit.StatusCheckRollup
			info.Checks = roll.State
			for _, c := range roll.Contexts.Nodes {
				name, failed := c.Name, failedCheck[c.Conclusion]
				if c.Context != "" { // a commit status rather than a check run
					name, failed = c.Context, c.State == "FAILURE" || c.State == "ERROR"
				}
				if failed && name != "" && !slices.Contains(info.Failing, name) {
					info.Failing = append(info.Failing, name)
				}
			}
		}
		out[u] = info
	}
	if len(out) == 0 && runErr != nil {
		return nil, runErr
	}
	return out, nil
}

// nudgeSender is the from-name nudges carry: not ccsSender, which says the
// user typed it.
const (
	nudgeSender = "ccs watch"
	NudgeNote   = "[Automatic status notice from ccs watch, the user's PR watcher. Not typed by the user and not from another Claude session: there is no one to reply to.]\n\n"
)

// nudgeSent is what one session was last told about one PR, at its head.
type nudgeSent struct {
	Head      string `json:"head"`
	Checks    bool   `json:"checks,omitempty"`
	Conflicts bool   `json:"conflicts,omitempty"`
	// Told: the unanswered threads it was told of, each at its last comment;
	// kept across heads, since a push doesn't answer a thread.
	Told map[string]int64 `json:"told,omitempty"`
}

// nudgeState is nudgesPath: what was sent, by nudgeKey.
type nudgeState struct {
	Sent map[string]*nudgeSent `json:"sent"`
}

func NudgesPath() string { return filepath.Join(filepath.Dir(state.SettingsPath), "nudges.json") }

func nudgesLockPath() string { return filepath.Join(filepath.Dir(state.SettingsPath), "nudges.lock") }

// nudgeKey is one session's record for one PR.
func nudgeKey(pr, session string) string { return pr + " " + session }

// NudgeEvent is a line of prEventsPath about a nudge.
type NudgeEvent struct {
	At      string   `json:"at"`
	Event   string   `json:"event"` // nudged, nudge_failed
	PR      string   `json:"pr"`
	Head    string   `json:"head,omitempty"`
	Session string   `json:"session"` // the session's name
	Reasons []string `json:"reasons"` // threads, checks, conflicts
	Error   string   `json:"error,omitempty"`
}

// SendNudge delivers a nudge to a live session; stubbed in tests.
var SendNudge = func(c PrClaim, text string) error {
	if c.Kind == "codex" {
		return sessions.CodexQueue(c.Thread, sessions.CodexNudgeNote+text)
	}
	f, err := sessions.ReadSessionFile(c.PID)
	if err != nil {
		return fmt.Errorf("session file: %w", err)
	}
	if f.SessionID != c.Session {
		return fmt.Errorf("pid %d is now another session", c.PID)
	}
	if f.Socket == "" {
		return fmt.Errorf("it has no message socket")
	}
	_, err = sessions.SendEnvelope(f.Socket, sessions.PeerToken(c.PID, f.Socket), nudgeSender, NudgeNote, text, false)
	return err
}

// nudgeText is what a session is told about p, for the reasons given.
func nudgeText(p *WatchedPR, reasons []string) string {
	var what []string
	for _, r := range reasons {
		switch r {
		case "threads":
			n := len(p.Unanswered)
			what = append(what, fmt.Sprintf("%d review thread%s waiting on a reply", n, map[bool]string{true: "", false: "s"}[n == 1]))
		case "checks":
			what = append(what, "checks failing: "+strings.Join(p.Failing, ", "))
		case "conflicts":
			what = append(what, "merge conflicts with its base")
		}
	}
	return fmt.Sprintf("%s (%s) has %s.\nThis session claimed it with ccs claim. If it's yours, deal with it; if not, run: ccs claim %s --release", p.URL, p.Title, strings.Join(what, "; "), p.URL)
}

// nudgeReasons is what to tell a session about p that it hasn't been told
// at this head, given what it was told (nil: nothing yet).
func nudgeReasons(p *WatchedPR, sent *nudgeSent) []string {
	var told map[string]int64
	if sent != nil {
		told = sent.Told
	}
	if sent != nil && sent.Head != p.Head {
		sent = nil // a new head: failing checks and conflicts are news again
	}
	if sent == nil {
		sent = &nudgeSent{}
	}
	var reasons []string
	if newReplies(p.Unanswered, told) {
		reasons = append(reasons, "threads")
	}
	if p.Checks == "failing" && !sent.Checks {
		reasons = append(reasons, "checks")
	}
	if p.Conflicts && !sent.Conflicts {
		reasons = append(reasons, "conflicts")
	}
	return reasons
}

// Nudges: after each pass, ccs watch tells the session that claimed a PR
// (ccs claim) what's wrong with it: review threads waiting on an answer
// (unresolved, the last comment not the operator's; told again only when one
// is new or has a new reviewer comment), failing checks, or conflicts with
// its base. Only live
// owners are told, each once per PR, head and reason, over the session's own
// message socket, in an envelope that says it's an automatic notice from
// ccs watch, never typed by the user. A PR nobody owns gets nothing beyond
// the watcher's usual events.

// NudgeTick nudges the live owners of the watcher's open PRs, records what
// was sent and appends the events. Records of PRs no longer open are dropped.
func NudgeTick(now time.Time) error {
	st, ok := LoadPRWatchState()
	if !ok {
		return nil
	}
	claims := LoadClaims()
	live := LiveByPID()
	var events []any
	err := withNudgeState(func(s *nudgeState) bool {
		changed := false
		for key := range s.Sent {
			pr, _, _ := strings.Cut(key, " ")
			if p := st.PRs[pr]; p == nil || p.State != "open" {
				delete(s.Sent, key)
				changed = true
			}
		}
		urls := make([]string, 0, len(claims.Claims))
		for u := range claims.Claims {
			urls = append(urls, u)
		}
		slices.Sort(urls)
		for _, u := range urls {
			p := st.PRs[u]
			if p == nil || p.State != "open" || p.Draft {
				continue
			}
			for _, c := range claims.Claims[u] {
				if !claimLive(c, live) {
					continue
				}
				key := nudgeKey(u, c.Session)
				reasons := nudgeReasons(p, s.Sent[key])
				if len(reasons) == 0 {
					continue
				}
				name := ownerName(c, live)
				e := NudgeEvent{At: now.UTC().Format(time.RFC3339), Event: "nudged", PR: u, Head: p.Head, Session: name, Reasons: reasons}
				if err := SendNudge(c, nudgeText(p, reasons)); err != nil {
					e.Event, e.Error = "nudge_failed", err.Error()
					state.LogActivity("nudge %s about %s: %v", name, u, err)
					events = append(events, e)
					continue // not recorded: tried again next pass
				}
				events = append(events, e)
				sent := s.Sent[key]
				if sent == nil || sent.Head != p.Head {
					var told map[string]int64
					if sent != nil {
						told = sent.Told
					}
					sent = &nudgeSent{Head: p.Head, Told: told}
					s.Sent[key] = sent
				}
				for _, r := range reasons {
					switch r {
					case "threads":
						sent.Told = maps.Clone(p.Unanswered)
					case "checks":
						sent.Checks = true
					case "conflicts":
						sent.Conflicts = true
					}
				}
				changed = true
			}
		}
		return changed
	})
	if werr := writeEventLines(events); err == nil {
		err = werr
	}
	return err
}

// withNudgeState runs f on the nudge record under an exclusive lock and
// saves it if f says it changed (a unique temp file, then a rename).
func withNudgeState(f func(s *nudgeState) bool) error {
	dir := filepath.Dir(NudgesPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(nudgesLockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	var s nudgeState
	if b, err := os.ReadFile(NudgesPath()); err == nil {
		json.Unmarshal(b, &s)
	}
	if s.Sent == nil {
		s.Sent = map[string]*nudgeSent{}
	}
	if !f(&s) {
		return nil
	}
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "nudges-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), NudgesPath()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

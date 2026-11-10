package github

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Merge order: `ccs depends <PR> --after <PR>` records that one PR must not
// merge before another has. ccs watch then holds the dependent's ready event,
// ccs prs shows it blocked, and the events say at once when auto-merge is on
// for a blocked PR or one merged early. ccs refuses no merge: GitHub's
// auto-merge, the browser and other machines are out of its reach, so it
// tells, and the owner acts.

// PRDepend is one PR's prerequisites.
type PRDepend struct {
	After []string  `json:"after"`        // the PRs that must merge first
	By    string    `json:"by,omitempty"` // the session that last declared it, when one did
	At    time.Time `json:"at"`
}

// DependsFile is dependsPath: each dependent PR's prerequisites.
type DependsFile struct {
	Depends map[string]*PRDepend `json:"depends"`
}

func DependsPath() string { return filepath.Join(filepath.Dir(state.SettingsPath), "depends.json") }

func dependsLockPath() string {
	return filepath.Join(filepath.Dir(state.SettingsPath), "depends.lock")
}

// LoadDepends reads the dependencies without the lock (a reader sees the old
// or the new file, never half of one).
func LoadDepends() DependsFile {
	var d DependsFile
	if b, err := os.ReadFile(DependsPath()); err == nil {
		json.Unmarshal(b, &d)
	}
	if d.Depends == nil {
		d.Depends = map[string]*PRDepend{}
	}
	return d
}

// WithDepends runs f on the dependencies under an exclusive lock and saves
// them if f says they changed (a unique temp file, then a rename).
func WithDepends(f func(d *DependsFile) bool) error {
	path := DependsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(dependsLockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	d := LoadDepends()
	if !f(&d) {
		return nil
	}
	data, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "depends-*.tmp")
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

var prShort = regexp.MustCompile(`^(?:([A-Za-z0-9_.-]+)/)?([A-Za-z0-9_.-]+)#([0-9]+)$`)

// resolvePR turns what was typed into a PR's address: the address itself,
// owner/repo#N, or repo#N when the PRs ccs knows (the watcher's state, the
// claims and the dependencies) have exactly one repo of that name.
func resolvePR(ref string) (string, error) {
	// One spelling per PR: "02" and "2" are the same number, and what is
	// stored must be what the watcher looks up.
	address := func(owner, repo, number string) string {
		n, _ := strconv.Atoi(number)
		return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, n)
	}
	if p := transcript.PRURL.FindStringSubmatch(ref); p != nil {
		return address(p[1], p[2], p[3]), nil
	}
	m := prShort.FindStringSubmatch(ref)
	if m == nil {
		return "", fmt.Errorf("not a PR: %s (give its address, owner/repo#N, or repo#N)", ref)
	}
	if m[1] != "" {
		return address(m[1], m[2], m[3]), nil
	}
	owners := map[string]bool{}
	note := func(u string) {
		if p := transcript.PRURL.FindStringSubmatch(u); p != nil && p[2] == m[2] {
			owners[p[1]] = true
		}
	}
	st, _ := LoadPRWatchState()
	for u := range st.PRs {
		note(u)
	}
	for u := range LoadClaims().Claims {
		note(u)
	}
	for u, d := range LoadDepends().Depends {
		note(u)
		for _, a := range d.After {
			note(a)
		}
	}
	switch len(owners) {
	case 0:
		return "", fmt.Errorf("ccs knows no repo called %s: give %s as owner/%s#%s or its address", m[2], ref, m[2], m[3])
	case 1:
		for o := range owners {
			return address(o, m[2], m[3]), nil
		}
	}
	return "", fmt.Errorf("more than one owner has a repo called %s: give %s as owner/%s#%s", m[2], ref, m[2], m[3])
}

// dependsOn: pr waits, through any chain, for target.
func (d DependsFile) dependsOn(pr, target string, seen map[string]bool) bool {
	if seen[pr] {
		return false
	}
	seen[pr] = true
	dep := d.Depends[pr]
	if dep == nil {
		return false
	}
	for _, a := range dep.After {
		if a == target || d.dependsOn(a, target, seen) {
			return true
		}
	}
	return false
}

// Depend records that pr must merge after each of after. A PR can't wait for
// itself, nor for one that waits for it.
func Depend(pr string, after []string, now time.Time) (string, error) {
	by := ""
	if me, err := callingSession(); err == nil {
		by = me.Name
	}
	var msg string
	var failed error
	err := WithDepends(func(d *DependsFile) bool {
		dep := d.Depends[pr]
		if dep == nil {
			dep = &PRDepend{}
		}
		added := false
		for _, a := range after {
			if a == pr {
				failed = fmt.Errorf("%s can't wait for itself", PRName(pr))
				return false
			}
			if d.dependsOn(a, pr, map[string]bool{}) {
				failed = fmt.Errorf("%s already waits for %s: that would be a circle", PRName(a), PRName(pr))
				return false
			}
			if !slices.Contains(dep.After, a) {
				dep.After = append(dep.After, a)
				added = true
			}
		}
		msg = fmt.Sprintf("%s waits for %s", PRName(pr), prNames(dep.After))
		if !added {
			return false
		}
		dep.By, dep.At = by, now
		d.Depends[pr] = dep
		return true
	})
	if failed != nil {
		return "", failed
	}
	return msg, err
}

// Undepend drops pr's prerequisites.
func Undepend(pr string) (string, error) {
	msg := PRName(pr) + " waited for nothing"
	err := WithDepends(func(d *DependsFile) bool {
		if d.Depends[pr] == nil {
			return false
		}
		delete(d.Depends, pr)
		msg = PRName(pr) + " no longer waits for anything"
		return true
	})
	return msg, err
}

func prNames(urls []string) string {
	names := make([]string, len(urls))
	for i, u := range urls {
		names[i] = PRName(u)
	}
	return strings.Join(names, ", ")
}

// unmetAfter is the prerequisites of pr that haven't merged, by what merged
// says of each (unknown counts as not merged).
func (d DependsFile) unmetAfter(pr string, merged func(string) bool) []string {
	dep := d.Depends[pr]
	if dep == nil {
		return nil
	}
	var unmet []string
	for _, a := range dep.After {
		if !merged(a) {
			unmet = append(unmet, a)
		}
	}
	return unmet
}

// PREnd is a PR's end state as GitHub has it now ("merged", "closed" or
// "open") and when it merged. Stubbed in tests.
var PREnd = func(pr string) (ended, mergedAt string, err error) {
	p := transcript.PRURL.FindStringSubmatch(pr)
	if p == nil {
		return "", "", fmt.Errorf("not a PR: %s", pr)
	}
	n, _ := strconv.Atoi(p[3])
	return prEnd(&WatchedPR{Repo: p[1] + "/" + p[2], Number: n})
}

// mergeOrder answers, for one pass, whether and when a prerequisite merged:
// this pass's searches first, then what the state knew, and GitHub itself
// (PREnd, once a pass) for a PR neither settles: one the watch doesn't
// cover, or one the state has open that the open search no longer lists
// (the merged search can lag). Not knowing counts as not merged.
type mergeOrder struct {
	deps   DependsFile
	known  map[string]*WatchedPR
	open   map[string]bool
	merged map[string]time.Time // each PR known to have merged, and when (zero: time unknown)
	asked  map[string]bool
}

func newMergeOrder(deps DependsFile, known map[string]*WatchedPR, open, merged []WatchedPR) *mergeOrder {
	q := &mergeOrder{deps: deps, known: known, open: map[string]bool{}, merged: map[string]time.Time{}, asked: map[string]bool{}}
	for _, p := range open {
		q.open[p.URL] = true
	}
	for u, p := range known {
		if p.State == "merged" {
			q.merged[u] = parseTime(p.MergedAt)
		}
	}
	for _, p := range merged {
		q.merged[p.URL] = parseTime(p.MergedAt)
	}
	return q
}

// mergedAt: whether u has merged, and when.
func (q *mergeOrder) mergedAt(u string) (bool, time.Time) {
	if at, ok := q.merged[u]; ok {
		return true, at
	}
	if p := q.known[u]; q.open[u] || q.asked[u] || p != nil && p.State == "closed" {
		return false, time.Time{}
	}
	q.asked[u] = true
	ended, at, err := PREnd(u)
	if err != nil {
		state.LogActivity("pr watch: couldn't tell whether %s, which another PR waits for, has merged: %v", u, err)
		return false, time.Time{}
	}
	if ended != "merged" {
		return false, time.Time{}
	}
	q.merged[u] = parseTime(at)
	return true, q.merged[u]
}

// unmet is the prerequisites of pr that haven't merged.
func (q *mergeOrder) unmet(pr string) []string {
	return q.deps.unmetAfter(pr, func(a string) bool {
		ok, _ := q.mergedAt(a)
		return ok
	})
}

// early is what pr, merged at t, was to merge after that hadn't merged by
// then: not merged at all, or merged later.
func (q *mergeOrder) early(pr string, t time.Time) []string {
	return q.deps.unmetAfter(pr, func(a string) bool {
		ok, at := q.mergedAt(a)
		return ok && (t.IsZero() || !at.After(t))
	})
}

// listBlocked is what an open PR still waits for, for ccs prs and the list,
// which have no pass of their own: what the watcher's state st knows has
// merged; for a prerequisite it doesn't list, the watcher's own answer when
// it has judged this declaration (made before its last pass), else GitHub
// when online, else not merged.
func listBlocked(u string, deps DependsFile, st PRWatchState, online bool) []string {
	dep := deps.Depends[u]
	if dep == nil {
		return nil
	}
	judged := st.PRs[u] != nil && st.PRs[u].State == "open" && !dep.At.After(st.Updated)
	return deps.unmetAfter(u, func(a string) bool {
		if p := st.PRs[a]; p != nil {
			return p.State == "merged"
		}
		if judged {
			return !slices.Contains(st.PRs[u].BlockedBy, a)
		}
		if online {
			ended, _, err := PREnd(a)
			return err == nil && ended == "merged"
		}
		return false
	})
}

// BlockedNow is what pr still waits for, from saved state only (no network):
// for the list, which draws each PR's status from GitHub's own answer.
func BlockedNow(pr string) []string {
	deps := LoadDepends()
	if deps.Depends[pr] == nil {
		return nil
	}
	st, _ := LoadPRWatchState()
	return listBlocked(pr, deps, st, false)
}

// pruneDepends forgets the dependencies of PRs the state says have ended.
func pruneDepends(st *PRWatchState) {
	ended := func(d *DependsFile) bool {
		changed := false
		for u := range d.Depends {
			if p := st.PRs[u]; p != nil && p.State != "open" {
				delete(d.Depends, u)
				changed = true
			}
		}
		return changed
	}
	if d := LoadDepends(); !ended(&d) {
		return
	}
	if err := WithDepends(ended); err != nil {
		state.LogActivity("pr watch: couldn't prune the PR dependencies: %v", err)
	}
}

// DependRow is one line of ccs depends: a PR and what it still waits for.
type DependRow struct {
	PR      string   `json:"pr"`
	After   []string `json:"after"`
	Waiting []string `json:"waiting"` // those of After not known to have merged
	By      string   `json:"by,omitempty"`
}

// DependsList is every recorded dependency, by what the watcher's state
// knows (a prerequisite it doesn't know counts as still waited for).
func DependsList() []DependRow {
	d := LoadDepends()
	st, _ := LoadPRWatchState()
	merged := func(u string) bool { return st.PRs[u] != nil && st.PRs[u].State == "merged" }
	rows := []DependRow{}
	for u, dep := range d.Depends {
		w := d.unmetAfter(u, merged)
		if w == nil {
			w = []string{}
		}
		rows = append(rows, DependRow{PR: u, After: dep.After, Waiting: w, By: dep.By})
	}
	slices.SortFunc(rows, func(a, b DependRow) int { return strings.Compare(a.PR, b.PR) })
	return rows
}

// afterFlag collects repeated --after values.
type afterFlag []string

func (a *afterFlag) String() string     { return strings.Join(*a, ",") }
func (a *afterFlag) Set(v string) error { *a = append(*a, v); return nil }

// RunDepends is `ccs depends [<PR> --after <PR>... | <PR> --drop] [--json]`.
func RunDepends(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("depends", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var after afterFlag
	fs.Var(&after, "after", "a PR that must merge first (repeat for several)")
	drop := fs.Bool("drop", false, "forget what the PR waits for")
	asJSON := fs.Bool("json", false, "print the list as JSON")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: ccs depends                          list the recorded merge order\n       ccs depends <PR> --after <PR>...     <PR> must not merge before the others have\n       ccs depends <PR> --drop              forget what <PR> waits for\n\nA PR is its address, owner/repo#N, or repo#N. ccs watch holds a waiting PR's ready\nevent, ccs prs shows it blocked, and blocked_automerge and merged_early say when\nauto-merge is on for it or it merged first. ccs refuses no merge itself.\n\n")
		fs.PrintDefaults()
	}
	var refs []string
	for len(args) > 0 { // flags may come after the PR
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		args = fs.Args()
		if len(args) > 0 {
			refs, args = append(refs, args[0]), args[1:]
		}
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "ccs depends:", err)
		return 1
	}
	if len(refs) == 0 && len(after) == 0 && !*drop {
		rows := DependsList()
		if *asJSON {
			b, _ := json.MarshalIndent(rows, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		if len(rows) == 0 {
			fmt.Fprintln(stdout, "no merge order recorded")
		}
		for _, r := range rows {
			line := fmt.Sprintf("%s after %s", PRName(r.PR), prNames(r.After))
			if len(r.Waiting) == 0 {
				line += "  (all merged: free to merge)"
			} else if len(r.Waiting) < len(r.After) {
				line += "  (still waiting for " + prNames(r.Waiting) + ")"
			}
			fmt.Fprintf(stdout, "%s\n   %s\n", line, r.PR)
		}
		return 0
	}
	if len(refs) != 1 || *drop == (len(after) > 0) {
		fs.Usage()
		return 2
	}
	pr, err := resolvePR(refs[0])
	if err != nil {
		return fail(err)
	}
	if *drop {
		msg, err := Undepend(pr)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	var afters []string
	for _, a := range after {
		u, err := resolvePR(a)
		if err != nil {
			return fail(err)
		}
		afters = append(afters, u)
	}
	msg, err := Depend(pr, afters, time.Now())
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The PR owner registry: a session runs `ccs claim <PR>` for each PR it
// owns, and `ccs prs` lists the author's open PRs with their owners, flagging
// PRs nobody owns and PRs two live sessions both claim. Claims of merged or
// closed PRs, and of sessions no longer running, are dropped on every read.

// prClaim is one session's claim on a PR.
type prClaim struct {
	Session   string    `json:"session"`          // Claude Code session id, or a Codex thread id
	Kind      string    `json:"kind,omitempty"`   // "codex" for a Codex session, else Claude Code
	Thread    string    `json:"thread,omitempty"` // a Codex session's thread id
	Name      string    `json:"name"`             // the session's name when it claimed
	PID       int       `json:"pid"`              // its claude process
	ProcStart string    `json:"proc_start"`       // that process's start, as its session file says: a recycled pid isn't it
	ClaimedAt time.Time `json:"claimed_at"`
	Notify    bool      `json:"notify,omitempty"` // reserved: the session asks to be told of its PR's failures
}

// claimsFile is claimsPath: each PR's claims, in the order they were made.
type claimsFile struct {
	Claims map[string][]prClaim `json:"claims"`
	// Checked is when each claimed PR outside the watcher's state was last
	// asked about, so every session's status line shares one check.
	Checked map[string]time.Time `json:"checked,omitempty"`
}

func claimsPath() string     { return filepath.Join(filepath.Dir(settingsPath), "claims.json") }
func claimsLockPath() string { return filepath.Join(filepath.Dir(settingsPath), "claims.lock") }

// prsFresh is how old the watcher's state may be before ccs prs asks GitHub itself.
const prsFresh = 10 * time.Minute

// endCheckEvery is how often a claimed PR outside the watcher's state is asked
// about, across all sessions: ccs prs runs per session, about once a minute.
const endCheckEvery = 10 * time.Minute

// dueEndChecks stamps and returns the claimed PRs in urls not asked about
// within endCheckEvery. Stamping under the lock makes one caller do each check,
// and a failed check waits too, so a refused request isn't retried every run.
func dueEndChecks(urls []string, now time.Time) ([]string, error) {
	var due []string
	err := withClaims(func(c *claimsFile) bool {
		for _, u := range urls {
			if _, claimed := c.Claims[u]; !claimed || now.Sub(c.Checked[u]) < endCheckEvery {
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

// withClaims runs f on the claims under an exclusive lock and saves them if
// f says they changed (a unique temp file, then a rename), so sessions
// claiming at once never lose each other's claims.
func withClaims(f func(c *claimsFile) bool) error {
	path := claimsPath()
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
	c := loadClaims()
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

// loadClaims reads the claims without the lock (a reader sees the old or the
// new file, never half of one).
func loadClaims() claimsFile {
	var c claimsFile
	if b, err := os.ReadFile(claimsPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.Claims == nil {
		c.Claims = map[string][]prClaim{}
	}
	return c
}

// parentPID is pid's parent, 0 when unknown. Stubbed in tests.
var parentPID = func(pid int) int {
	out, err := runBounded(2*time.Second, nil, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return p
}

// liveByPID is each running claude session by its pid, as scanSessions
// checks them (the pid's start time matches its session file). Stubbed in tests.
var liveByPID = func() map[int]prClaim {
	out := map[int]prClaim{}
	for id, s := range scanSessions(nil) {
		out[s.pid] = prClaim{Session: id, Name: s.stat.name, PID: s.pid, ProcStart: s.start}
	}
	return out
}

// callingSession is the claude session this process runs under: the first
// ancestor that is a live session's claude process.
func callingSession() (prClaim, error) {
	if c, ok := codexSession(); ok {
		return c, nil
	}
	live := liveByPID()
	pid := os.Getppid()
	for range 30 {
		if pid <= 1 {
			break
		}
		if s, ok := live[pid]; ok {
			return s, nil
		}
		pid = parentPID(pid)
	}
	return prClaim{}, errors.New("not run from a Claude Code or Codex session: ccs claim must be run by the session that owns the PR")
}

// claimLive: the claim's session still runs in the same process (a Codex
// session: codexLive).
func claimLive(c prClaim, live map[int]prClaim) bool {
	if c.Kind == "codex" {
		return codexLive(c.Thread)
	}
	s, ok := live[c.PID]
	return ok && s.Session == c.Session && s.ProcStart == c.ProcStart
}

// ownerName is the name c's session goes by now: a /rename after the claim
// changes the session file's name, never the claim's session or pid.
func ownerName(c prClaim, live map[int]prClaim) string {
	if s, ok := live[c.PID]; ok && c.Kind != "codex" && s.Session == c.Session && s.Name != "" {
		return s.Name
	}
	return c.Name
}

// claim records (or with release, drops) the calling session's claim on pr.
// Another session's claim on it stays: two owners is something to flag.
func claim(pr string, release bool, now time.Time) (string, error) {
	if !prURL.MatchString(pr) {
		return "", fmt.Errorf("not a GitHub PR address: %s", pr)
	}
	me, err := callingSession()
	if err != nil {
		return "", err
	}
	var msg string
	err = withClaims(func(c *claimsFile) bool {
		mine := func(x prClaim) bool { return x.Session == me.Session }
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

// prRow is one line of ccs prs.
type prRow struct {
	PR     string   `json:"pr"`
	Repo   string   `json:"repo"`
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Owners []string `json:"owners"` // names of the live sessions claiming it
	// OwnersDetail is the same owners with their kind ("claude" or "codex")
	// and id (the Claude Code session id, or the Codex thread id).
	OwnersDetail []prOwner `json:"owners_detail,omitempty"`
	Head         string    `json:"head,omitempty"` // head commit SHA
	// NeedsReview: awaiting review now (watchedPR.needsReview, as the
	// needs_review event); from a fresh fetch, the same pass data.
	NeedsReview bool   `json:"needs_review"`
	Group       string `json:"group"`  // ready, needs_you, waiting
	Status      string `json:"status"` // why, as the list says it
	Unowned     bool   `json:"unowned,omitempty"`
	Contested   bool   `json:"contested,omitempty"` // claimed by two or more live sessions
}

// prOwner is one live session claiming a PR.
type prOwner struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Session string `json:"session"`
}

// ownerOf is c as a prOwner: a Codex claim's id is its thread.
func ownerOf(c prClaim, live map[int]prClaim) prOwner {
	o := prOwner{Name: ownerName(c, live), Kind: "claude", Session: c.Session}
	if c.Kind == "codex" {
		o.Kind = "codex"
		if c.Thread != "" {
			o.Session = c.Thread
		}
	}
	return o
}

// ownedBy keeps the PRs the session of that kind ("claude": its session
// id; "codex": its thread id) claims; a claim of the other kind never matches.
func (r prsReport) ownedBy(kind, id string) prsReport {
	out := r
	out.PRs = nil
	for _, p := range r.PRs {
		if slices.ContainsFunc(p.OwnersDetail, func(o prOwner) bool { return o.Kind == kind && o.Session == id }) {
			out.PRs = append(out.PRs, p)
		}
	}
	return out
}

// sessionLine is --statusline for one session: its PRs by group, "PRs ✓1
// ✗1 2" (ready, needing you, waiting), empty when it claims none.
func (r prsReport) sessionLine() string {
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

// prsReport is ccs prs: the author's open PRs and how fresh the answer is.
type prsReport struct {
	Updated time.Time `json:"updated"` // when GitHub was last asked
	Stale   bool      `json:"stale,omitempty"`
	PRs     []prRow   `json:"prs"`
}

// status is the PR's group and why, by the list's rules (prInfo.status): a
// failed check, optional or not, needs you.
func (p *watchedPR) status() (int, string) {
	switch {
	case p.Draft:
		return prWaiting, "draft"
	case p.Conflicts:
		return prFix, "conflicts"
	case p.Checks == "failing":
		return prFix, "checks failing: " + strings.Join(p.Failing, ", ")
	case p.Threads != nil && *p.Threads > 0:
		return prFix, threadsText(*p.Threads)
	case p.Review == "changes_requested":
		return prFix, "changes requested"
	case p.Checks == "pending":
		return prWaiting, "checks running"
	case p.Review == "approved":
		return prReady, "ready to merge"
	}
	return prWaiting, "review required"
}

var groupNames = map[int]string{prReady: "ready", prFix: "needs_you", prWaiting: "waiting"}

// fetchOpenPRs asks GitHub (REST) for the author's open PRs; one that
// couldn't be read is left out. Stubbed in tests.
var fetchOpenPRs = func(author, owner string) (map[string]*watchedPR, error) {
	open, err := searchPRs(author, owner, "--state", "open")
	if err != nil {
		return nil, err
	}
	out := map[string]*watchedPR{}
	for _, d := range prDetailsAll(open) {
		if d != nil {
			out[d.URL] = d
		}
	}
	return out, nil
}

// endState is prEndState for a claimed PR the open list doesn't have. Stubbed in tests.
var endState = func(pr string) (string, error) {
	p := prURL.FindStringSubmatch(pr)
	n, _ := strconv.Atoi(p[3])
	return prEndState(&watchedPR{Repo: p[1] + "/" + p[2], Number: n})
}

// prsList builds ccs prs. With offline, it never asks GitHub: the watcher's
// state however old (Stale says so), and claims are pruned only of ended
// sessions and of PRs the state knows merged or closed.
func prsList(author, owner string, offline bool, now time.Time) (prsReport, error) {
	st, ok := loadPRWatchState()
	ok = ok && st.Author == author && st.Owner == owner
	var rep prsReport
	open := map[string]*watchedPR{}
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
		fetched, err := fetchOpenPRs(author, owner)
		if err != nil {
			return rep, err
		}
		open, rep.Updated = fetched, now
	}

	live := liveByPID()
	claims := loadClaims()
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
			logActivity("ccs prs: couldn't record PR checks: %v", err)
		}
		for _, u := range due {
			if s, err := endState(u); err == nil && s != "open" {
				ended[u] = true
			}
		}
	}
	prune := func(c *claimsFile) bool {
		changed := false
		for u, cs := range c.Claims {
			kept := slices.DeleteFunc(slices.Clone(cs), func(x prClaim) bool { return !claimLive(x, live) })
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
		if err := withClaims(prune); err != nil {
			logActivity("ccs prs: couldn't prune claims: %v", err)
		}
	}

	for u, p := range open {
		g, why := p.status()
		row := prRow{PR: u, Repo: p.Repo, Number: p.Number, Title: p.Title, Head: p.Head, NeedsReview: p.needsReview(), Group: groupNames[g], Status: why}
		for _, c := range claims.Claims[u] {
			o := ownerOf(c, live)
			row.Owners = append(row.Owners, o.Name)
			row.OwnersDetail = append(row.OwnersDetail, o)
		}
		row.Unowned = len(row.Owners) == 0
		row.Contested = len(row.Owners) > 1
		rep.PRs = append(rep.PRs, row)
	}
	order := map[string]int{"needs_you": 0, "ready": 1, "waiting": 2}
	slices.SortFunc(rep.PRs, func(a, b prRow) int {
		if d := order[a.Group] - order[b.Group]; d != 0 {
			return d
		}
		return strings.Compare(a.PR, b.PR)
	})
	return rep, nil
}

// statusLine is ccs prs --statusline: counts only, "" when there's nothing.
func (r prsReport) statusLine() string {
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

// text is ccs prs for people: one line per PR, needing you first.
func (r prsReport) text() string {
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

// runClaim is `ccs claim <PR URL> [--release]`.
func runClaim(args []string, stdout, stderr io.Writer) int {
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
	msg, err := claim(urls[0], *release, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "ccs claim:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// runPRs is `ccs prs [--json] [--statusline]`.
func runPRs(args []string, stdout, stderr io.Writer) int {
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
		if st, ok := loadPRWatchState(); ok && (!ownerSet || st.Owner == *owner) {
			*author, *owner = st.Author, st.Owner
		} else if *line {
			return 0 // nothing saved to count, and no network
		} else if err := defaultAuthor(author); err != nil {
			fmt.Fprintln(stderr, "ccs prs:", err)
			return 1
		}
	}
	rep, err := prsList(*author, *owner, *line, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "ccs prs:", err)
		return 1
	}
	one := *claudeID != "" || *codexID != ""
	if *claudeID != "" {
		rep = rep.ownedBy("claude", *claudeID)
	}
	if *codexID != "" {
		rep = rep.ownedBy("codex", *codexID)
	}
	switch {
	case *line && one:
		if s := rep.sessionLine(); s != "" {
			fmt.Fprintln(stdout, s)
		}
	case *line:
		if s := rep.statusLine(); s != "" {
			fmt.Fprintln(stdout, s)
		}
	case *asJSON:
		if rep.PRs == nil {
			rep.PRs = []prRow{}
		}
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(b))
	default:
		fmt.Fprint(stdout, rep.text())
	}
	return 0
}

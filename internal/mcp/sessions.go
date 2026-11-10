package mcp

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// `ccs sessions` and the live part of list_sessions: what each session is
// doing right now, so a coordinator needn't read tabs or message sessions
// to find the one that's stuck.

// liveSessions is the sessions running now. Stubbed in tests.
var liveSessions = func() map[string]sessions.LiveSession { return sessions.ScanSessions(nil) }

// watchedOpen is the PRs ccs watch last saw open, nil when it has kept no
// state: without it, which of a session's PRs are still open isn't known
// (asking GitHub per session is the watcher's job). The watcher follows one
// author's PRs, so another author's open PR is never in it. Read at most
// every few seconds; stubbed in tests.
var watchedOpen = func() map[string]bool {
	openCache.Lock()
	defer openCache.Unlock()
	if time.Since(openCache.at) < 5*time.Second {
		return openCache.open
	}
	openCache.at, openCache.open = time.Now(), nil
	if s, ok := github.LoadPRWatchState(); ok {
		openCache.open = map[string]bool{}
		for u, p := range s.PRs {
			if p.State == "open" {
				openCache.open[u] = true
			}
		}
	}
	return openCache.open
}

var openCache struct {
	sync.Mutex
	at   time.Time
	open map[string]bool
}

// SessionRow is one session as `ccs sessions --json` gives it. The fields
// from Status down are only set for a session that's running.
type SessionRow struct {
	Name        string   `json:"name"` // its title, or what getTopic shows for it
	Session     string   `json:"session"`
	Kind        string   `json:"kind"` // claude or codex
	Cwd         string   `json:"cwd,omitempty"`
	Live        bool     `json:"live"`
	PID         int      `json:"pid,omitempty"`
	Status      string   `json:"status,omitempty"`      // busy, idle, waiting (on the user) or shell
	Since       string   `json:"since,omitempty"`       // when it entered that status (RFC3339)
	WaitingFor  string   `json:"waiting_for,omitempty"` // with waiting: "permission prompt", "input needed"
	Question    string   `json:"question,omitempty"`    // with waiting: the menu or the tool call it asks about
	Asked       bool     `json:"asked,omitempty"`       // idle, and its last message ends in a question
	LastError   string   `json:"last_error,omitempty"`  // the latest API error, until a later reply succeeds
	LastErrorAt string   `json:"last_error_at,omitempty"`
	PRs         []string `json:"prs,omitempty"`          // every PR it or its subagents opened, merged or touched
	OpenPRs     []string `json:"open_prs,omitempty"`     // those ccs watch has as open; absent when no watcher state says
	Tasks       int      `json:"tasks,omitempty"`        // background commands and monitors running
	Subagents   int      `json:"subagents,omitempty"`    // subagents running
	LastMessage string   `json:"last_message,omitempty"` // when the last message was said (RFC3339)
	Messages    int      `json:"messages,omitempty"`
}

// sessionRow is conv as a row; l is its live session, ok false when it isn't
// running.
func sessionRow(conv transcript.Conversation, l sessions.LiveSession, ok bool, now time.Time) SessionRow {
	r := SessionRow{Name: transcript.GetTopic(conv), Session: conv.SessionID, Kind: "claude", Cwd: conv.Cwd,
		LastError: conv.LastError, LastErrorAt: conv.LastErrorTs, PRs: github.PRsOf(conv),
		LastMessage: conv.LastTimestamp, Messages: len(conv.Messages)}
	if conv.Kind == transcript.KindCodex {
		r.Kind = transcript.KindCodex
	}
	if open := watchedOpen(); open != nil {
		for _, u := range r.PRs {
			if open[u] {
				r.OpenPRs = append(r.OpenPRs, u)
			}
		}
	}
	if !ok {
		return r
	}
	r.Live, r.PID, r.Status, r.WaitingFor = true, l.PID, l.Stat.Status, l.Stat.WaitingFor
	if l.Stat.Name != "" {
		r.Name = l.Stat.Name
	}
	if !l.Stat.Since.IsZero() {
		r.Since = l.Stat.Since.UTC().Format(time.RFC3339)
	}
	switch l.Stat.Status {
	case "waiting":
		r.Question = conv.PendingAsk()
	case "idle":
		r.Asked = conv.AskedLast()
	}
	since, _ := time.Parse(time.ANSIC, strings.Join(strings.Fields(l.Start), " "))
	r.Tasks = len(transcript.RunningTasks(conv, since, now))
	for _, s := range transcript.SubagentsOf(conv) {
		if transcript.SubagentRunning(s, since, now) {
			r.Subagents++
		}
	}
	return r
}

// SessionRows is the running sessions, those needing the user first (waiting,
// then asked, then with an error), then the most recently active; with all,
// the sessions that aren't running follow, newest first. A running session
// with no transcript yet (just started) has only what its session file says.
func SessionRows(convs []transcript.Conversation, live map[string]sessions.LiveSession, all bool, now time.Time) []SessionRow {
	var rows []SessionRow
	seen := map[string]bool{}
	for _, c := range convs {
		l, ok := live[c.SessionID]
		seen[c.SessionID] = true
		if ok || all {
			rows = append(rows, sessionRow(c, l, ok, now))
		}
	}
	for id, l := range live {
		if !seen[id] {
			c := transcript.Conversation{SessionID: id, Title: l.Stat.Name}
			if l.Codex {
				c.Kind = transcript.KindCodex
			}
			r := sessionRow(c, l, true, now)
			r.Name = l.Stat.Name
			rows = append(rows, r)
		}
	}
	rank := func(r SessionRow) int {
		switch {
		case !r.Live:
			return 4
		case r.Status == "waiting":
			return 0
		case r.Asked:
			return 1
		case r.LastError != "":
			return 2
		}
		return 3
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := rank(rows[i]), rank(rows[j]); a != b {
			return a < b
		}
		if rows[i].LastMessage != rows[j].LastMessage {
			return rows[i].LastMessage > rows[j].LastMessage
		}
		return rows[i].Session < rows[j].Session
	})
	return rows
}

// state is the row's status in words: "idle 5m", "waiting 2m: input needed",
// "not running".
func (r SessionRow) state(now time.Time) string {
	if !r.Live {
		return "not running"
	}
	s := r.Status
	if s == "" {
		s = "running"
	}
	if r.Since != "" {
		s += " " + strings.TrimSuffix(transcript.FormatAgo(r.Since, now), " ago")
	}
	if r.WaitingFor != "" {
		s += ": " + r.WaitingFor
	}
	return s
}

// details is what's worth knowing about the row beyond its state, one item
// a line: its question, its error, its PRs, what it runs in the background.
func (r SessionRow) details(now time.Time) []string {
	var out []string
	if r.Question != "" {
		out = append(out, "asks: "+strings.ReplaceAll(r.Question, "\n", "\n  "))
	}
	if r.Asked {
		out = append(out, "its last message asks something")
	}
	if r.LastError != "" {
		at := ""
		if r.LastErrorAt != "" {
			at = " (" + transcript.FormatAgo(r.LastErrorAt, now) + ")"
		}
		out = append(out, "last error: "+r.LastError+at)
	}
	var bg []string
	if r.Tasks > 0 {
		bg = append(bg, fmt.Sprintf("%d background task(s)", r.Tasks))
	}
	if r.Subagents > 0 {
		bg = append(bg, fmt.Sprintf("%d subagent(s)", r.Subagents))
	}
	if len(bg) > 0 {
		out = append(out, "running: "+strings.Join(bg, ", "))
	}
	names := func(urls []string) string {
		out := make([]string, len(urls))
		for i, u := range urls {
			out[i] = github.PRName(u)
		}
		return strings.Join(out, ", ")
	}
	switch {
	case len(r.PRs) == 0:
	case watchedOpen() == nil: // nothing says which are open: all it touched
		out = append(out, "PRs: "+names(r.PRs))
	case len(r.OpenPRs) > 0:
		line := "open PRs: " + names(r.OpenPRs)
		if rest := len(r.PRs) - len(r.OpenPRs); rest > 0 {
			line += fmt.Sprintf(" (and %d merged, closed or not yours)", rest)
		}
		out = append(out, line)
	default:
		out = append(out, fmt.Sprintf("no open PRs (%d merged, closed or not yours)", len(r.PRs)))
	}
	return out
}

// liveLine is list_sessions' extra lines for a running session, "" for one
// that isn't.
func (r SessionRow) liveLine(now time.Time) string {
	if !r.Live {
		return ""
	}
	s := "  live: " + r.state(now)
	for _, d := range r.details(now) {
		s += "\n  " + d
	}
	return s
}

// sessionsText is `ccs sessions` without --json.
func sessionsText(rows []SessionRow, now time.Time) string {
	if len(rows) == 0 {
		return "No sessions running.\n"
	}
	var b strings.Builder
	for _, r := range rows {
		last := ""
		if r.LastMessage != "" {
			last = " · last message " + transcript.FormatAgo(r.LastMessage, now)
		}
		fmt.Fprintf(&b, "%s · %s%s\n  %s · session %s · %s\n", r.Name, r.state(now), last, r.Kind, r.Session, r.Cwd)
		for _, d := range r.details(now) {
			b.WriteString("  " + d + "\n")
		}
	}
	return b.String()
}

// RunSessions is `ccs sessions`.
func RunSessions(args []string, stdout, stderr io.Writer, load func() ([]transcript.Conversation, error)) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON: {\"sessions\": [...]}")
	all := fs.Bool("all", false, "also the sessions that aren't running (the last 60 days), and one-shot headless runs")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs sessions [--json] [--all]\n\nThe sessions running now and what each is doing: busy, idle or waiting on you and since when,\nthe question it asks, its last API error, its PRs, and the background tasks and subagents it runs.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	convs, err := load()
	if err != nil {
		fmt.Fprintln(stderr, "ccs sessions: couldn't read sessions:", err)
		return 1
	}
	live := liveSessions()
	if !*all {
		// A headless run is left out unless it's running: then it's a session to know about.
		kept := convs[:0:0]
		for _, c := range convs {
			if _, ok := live[c.SessionID]; ok || !c.Headless() {
				kept = append(kept, c)
			}
		}
		convs = kept
	}
	now := time.Now()
	rows := SessionRows(convs, live, *all, now)
	if *asJSON {
		if rows == nil {
			rows = []SessionRow{}
		}
		data, _ := json.MarshalIndent(map[string]any{"sessions": rows}, "", " ")
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	fmt.Fprint(stdout, sessionsText(rows, now))
	return 0
}

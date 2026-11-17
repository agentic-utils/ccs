package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// `ccs sessions --triage` and session_triage: each running session in
// exactly one bucket, so an orchestrator can tell the one stuck on nothing
// from the one waiting on CI, the user, or nobody at all.

// The buckets, in the order they're tested: a session lands in the first
// that fits.
const (
	BucketWaitingOnUser     = "waiting_on_user"     // a menu, a prompt, a question, a command to run, a PR to merge
	BucketWorking           = "working"             // mid-turn
	BucketWaitingOnExternal = "waiting_on_external" // CI, a reviewer, a merge order, its tasks, subagents or wake-ups
	BucketDone              = "done"                // nothing open, and its last message says it's finished
	BucketStalled           = "stalled"             // idle past the threshold with nothing pending
	BucketIdle              = "idle"                // idle, not long enough to call stalled
)

// bucketOrder is how triage lists them: what needs someone first.
var bucketOrder = map[string]int{BucketWaitingOnUser: 0, BucketStalled: 1, BucketDone: 2, BucketIdle: 3, BucketWaitingOnExternal: 4, BucketWorking: 5}

// DefaultStallAfter is how long an idle session with nothing pending waits
// before it's stalled.
const DefaultStallAfter = 15 * time.Minute

// TriagePR is one of a session's open PRs and where it stands, as list_prs says it.
type TriagePR struct {
	PR        string   `json:"pr"`
	Group     string   `json:"group"` // ready, needs_you, waiting
	Status    string   `json:"status"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// TriageRow is one running session as triage sees it.
type TriageRow struct {
	Name    string `json:"name"`
	Session string `json:"session"`
	Kind    string `json:"kind"`
	Parent  string `json:"parent,omitempty"` // the session that launched or adopted it, by name
	Cwd     string `json:"cwd,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Bucket  string `json:"bucket"`
	Reason  string `json:"reason"` // one line: why this bucket
	Status  string `json:"status,omitempty"`
	// Idle is how long it has been in its status (idle, waiting) or, without
	// one, since its last message; IdleSince when that began (RFC3339).
	Idle        string     `json:"idle,omitempty"`
	IdleSeconds int64      `json:"idle_seconds"`
	IdleSince   string     `json:"idle_since,omitempty"`
	OpenPRs     []TriagePR `json:"open_prs,omitempty"`
	Tasks       int        `json:"tasks,omitempty"`
	Subagents   int        `json:"subagents,omitempty"`
	// NextWake is its soonest ScheduleWakeup or CronCreate fire time
	// (RFC3339) and NextWakeFor what it's for; Schedules counts every
	// pending one, recurring crons with no known time included.
	NextWake    string `json:"next_wake,omitempty"`
	NextWakeFor string `json:"next_wake_for,omitempty"`
	Schedules   int    `json:"schedules,omitempty"`
	LastSaid    string `json:"last_said,omitempty"` // the last thing it or the user said, cut
}

// ownedPRs is each claimant's open PRs (github.OpenPRsByOwner: saved state,
// read only, so triage never prunes a claim), keyed by github.OwnerKey; nil
// when no watcher state says. Stubbed in tests.
var ownedPRs = func(time.Time) map[string][]github.PRRow {
	m, _ := github.OpenPRsByOwner()
	return m
}

func ownerKey(kind, id string) string { return github.OwnerKey(kind, id) }

// asksToRun: its message asks the user to run a command themselves. "You
// can run this in your terminal" is an offer, not one.
var asksToRun = regexp.MustCompile("(?i)\\b(please run|(can|could|would) you (please )?run|need you to run|you('ll| will)? need to run|you need to run|run (this|it|that) yourself)\\b|`! ")

// waitsOnYou: its message hands the next step to the user ("send me the
// link once it's open"). An offer isn't one: "let me know if you'd like…",
// "send me a message if anything breaks".
var waitsOnYou = regexp.MustCompile(`(?i)\b(send me (the|a|your) (link|url|address|id|name|token|key|output|logs?|screenshot|details|answer|number)|tell me (when|once)|ping me (when|once)|once you('ve| have)|when you('ve| have)|waiting (on|for) (you|your)|over to you|needs? your (go|approval|decision|input|answer))\b`)

// saysFinished: its message says the work is over, not that some step of
// it is ("the backtest has finished", "the prerequisite has been merged").
// saysNotFinished undoes it ("not done yet", "isn't finished"), and so does
// saysMore, work it names as still to come ("next I'll update the app",
// "the deploy still needs to be done").
var (
	saysFinished    = regexp.MustCompile(`(?i)\b(all done|i'm done|we're done|that's (all|everything)|(work|task|job|this|everything) is (all )?(done|finished|complete)|i've finished|wrapped up|all set|nothing (else|more|further) (to do|for me to do|is needed|needed|outstanding|pending)|nothing (left|outstanding)( to do)?|no further (action|work|changes)|merged and released)\b|\bdone[.!]?\s*$`)
	saysNotFinished = regexp.MustCompile(`(?i)\b(not|isn't|aren't|wasn't|haven't|hasn't|yet to be|not yet)\s+(\w+\s+)?(done|finished|complete|merged)\b`)
	saysMore        = regexp.MustCompile(`(?i)\b(next,? i('ll| will)|next up|next step|then i('ll| will)|i('ll| will) (now|next|then|start|continue|move on|update|open|push|run|wait)|still (needs?|has|have) to|still needs?|needs? to be (done|finished|run|merged|deployed)|remaining work)\b`)
)

// sentenceEnd ends a sentence: a stop after a word and then a space (not the dots of v1.2.0, nor the `! of a command), or a line break.
var sentenceEnd = regexp.MustCompile(`[\p{L}\p{N})"'*_][.!?]+(\s|$)|\n`)

// matched is the sentence of s where re first matches, flattened and cut,
// for a reason; "" when it doesn't match.
func matched(re *regexp.Regexp, s string, n int) string {
	loc := re.FindStringIndex(s)
	if loc == nil {
		return ""
	}
	start := 0
	if ends := sentenceEnd.FindAllStringIndex(s[:loc[0]], -1); len(ends) > 0 {
		start = ends[len(ends)-1][1]
	}
	end := len(s)
	if i := sentenceEnd.FindStringIndex(s[loc[1]:]); i != nil {
		end = loc[1] + i[1]
	}
	return cut(transcript.CleanText(strings.Join(strings.Fields(s[start:end]), " ")), n)
}

// lastFrom is the last thing said in conv if the session said it, else "".
func lastFrom(conv transcript.Conversation) string {
	n := transcript.LastSaid(conv.Messages)
	if n < 0 || conv.Messages[n].Role != "assistant" {
		return ""
	}
	return conv.Messages[n].Text
}

// lastLine is s's last non-blank line, flattened and cut, for a reason.
func lastLine(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.Join(strings.Fields(lines[len(lines)-1]), " ")
	return cut(transcript.CleanText(l), n)
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// shortAgo is a duration as "40m", "2h", "3d".
func shortAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// triageOf is conv's row; l its live session, prs its open PRs (nil when no
// watcher state says).
func triageOf(conv transcript.Conversation, l sessions.LiveSession, prs []github.PRRow, now time.Time, stallAfter time.Duration) TriageRow {
	r := sessionRow(conv, l, true, now)
	t := TriageRow{Name: r.Name, Session: r.Session, Kind: r.Kind, Cwd: r.Cwd, PID: r.PID, Status: r.Status, Tasks: r.Tasks, Subagents: r.Subagents}
	if n := transcript.LastSaid(conv.Messages); n >= 0 {
		t.LastSaid = cut(strings.Join(strings.Fields(transcript.CleanText(conv.Messages[n].Text)), " "), 300)
	}
	since := l.Stat.Since
	if since.IsZero() || (r.Status != "idle" && r.Status != "waiting") {
		if last, err := time.Parse(time.RFC3339Nano, conv.LastTimestamp); err == nil && (since.IsZero() || r.Status == "") {
			since = last
		}
	}
	if !since.IsZero() {
		d := max(now.Sub(since), 0)
		t.IdleSince, t.IdleSeconds, t.Idle = since.UTC().Format(time.RFC3339), int64(d.Seconds()), shortAgo(d)
	}
	for _, p := range prs {
		t.OpenPRs = append(t.OpenPRs, TriagePR{p.PR, p.Group, p.Status, p.BlockedBy})
	}
	start, _ := time.Parse(time.ANSIC, strings.Join(strings.Fields(l.Start), " "))
	scheds := transcript.PendingSchedules(conv, start, now)
	t.Schedules = len(scheds)
	if len(scheds) > 0 {
		if !scheds[0].Next.IsZero() {
			t.NextWake = scheds[0].Next.UTC().Format(time.RFC3339)
		}
		t.NextWakeFor = cut(scheds[0].Desc, 120)
	}
	t.Bucket, t.Reason = classify(r, t, lastFrom(conv), scheds, now, stallAfter)
	return t
}

// classify picks t's bucket and says why in one line.
func classify(r SessionRow, t TriageRow, said string, scheds []transcript.Schedule, now time.Time, stallAfter time.Duration) (string, string) {
	name := func(p TriagePR) string { return github.PRName(p.PR) }
	switch r.Status {
	case "waiting":
		q := strings.SplitN(r.Question, "\n", 2)[0]
		switch {
		case r.WaitingFor == "permission prompt" && q != "":
			return BucketWaitingOnUser, "permission prompt: " + cut(q, 160)
		case q != "":
			return BucketWaitingOnUser, "asks you: " + cut(q, 160)
		}
		return BucketWaitingOnUser, "waiting on you: " + cmpOr(r.WaitingFor, "a menu or prompt")
	case "busy", "shell":
		return BucketWorking, "mid-turn for " + cmpOr(t.Idle, "a moment")
	}
	if said != "" && transcript.Asks(said) {
		return BucketWaitingOnUser, "its last message asks: " + lastLine(said, 160)
	}
	if m := matched(asksToRun, said, 160); m != "" {
		return BucketWaitingOnUser, "asks you to run a command: " + m
	}
	if m := matched(waitsOnYou, said, 160); m != "" {
		return BucketWaitingOnUser, "waits on you: " + m
	}
	var ready, waiting, fix []string
	for _, p := range t.OpenPRs {
		switch p.Group {
		case "ready":
			ready = append(ready, name(p))
		case "waiting":
			waiting = append(waiting, name(p)+" "+p.Status)
		default:
			fix = append(fix, name(p)+" "+p.Status)
		}
	}
	if len(ready) > 0 {
		return BucketWaitingOnUser, strings.Join(ready, ", ") + " ready to merge: needs your go"
	}
	var ext []string
	if t.Tasks > 0 {
		ext = append(ext, plural(t.Tasks, "background task"))
	}
	if t.Subagents > 0 {
		ext = append(ext, plural(t.Subagents, "subagent"))
	}
	if len(scheds) > 0 {
		s := scheds[0]
		w := "wakes " + s.Kind
		if !s.Next.IsZero() {
			w = "wakes in " + shortAgo(s.Next.Sub(now))
		} else if s.Kind == "cron" {
			w = "recurring cron"
		}
		if s.Desc != "" {
			w += " (" + cut(s.Desc, 60) + ")"
		}
		ext = append(ext, w)
	}
	ext = append(ext, waiting...)
	if len(ext) > 0 {
		return BucketWaitingOnExternal, strings.Join(ext, "; ")
	}
	if m := matched(saysFinished, said, 160); len(t.OpenPRs) == 0 && m != "" && !saysNotFinished.MatchString(said) && !saysMore.MatchString(said) {
		return BucketDone, "says it's finished: " + m
	}
	why := "nothing pending"
	if len(fix) > 0 {
		why = strings.Join(fix, "; ")
	}
	if idle := time.Duration(t.IdleSeconds) * time.Second; t.IdleSince != "" && idle >= stallAfter {
		return BucketStalled, "idle " + t.Idle + ", " + why
	}
	return BucketIdle, "idle " + cmpOr(t.Idle, "just now") + ", " + why
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// TriageRows is every running session's row, those needing someone first
// (bucketOrder), then the longest idle.
func TriageRows(convs []transcript.Conversation, live map[string]sessions.LiveSession, now time.Time, stallAfter time.Duration) []TriageRow {
	prs := ownedPRs(now)
	parents := sessions.ParentNames(convs)
	byID := map[string]transcript.Conversation{}
	for _, c := range convs {
		byID[c.SessionID] = c
	}
	rows := []TriageRow{}
	for id, l := range live {
		c, ok := byID[id]
		if !ok {
			c = transcript.Conversation{SessionID: id, Title: l.Stat.Name}
			if l.Codex {
				c.Kind = transcript.KindCodex
			}
		}
		kind := "claude"
		if c.Kind == transcript.KindCodex || l.Codex {
			kind = transcript.KindCodex
		}
		r := triageOf(c, l, prs[ownerKey(kind, id)], now, stallAfter)
		r.Parent = parents[id]
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := bucketOrder[rows[i].Bucket], bucketOrder[rows[j].Bucket]; a != b {
			return a < b
		}
		if rows[i].IdleSeconds != rows[j].IdleSeconds {
			return rows[i].IdleSeconds > rows[j].IdleSeconds
		}
		return rows[i].Session < rows[j].Session
	})
	return rows
}

// triageText is triage without --json: a count per bucket, then each
// session with its reason.
func triageText(rows []TriageRow) string {
	if len(rows) == 0 {
		return "No sessions running.\n"
	}
	n := map[string]int{}
	for _, r := range rows {
		n[r.Bucket]++
	}
	var counts []string
	for _, b := range []string{BucketWaitingOnUser, BucketStalled, BucketDone, BucketIdle, BucketWaitingOnExternal, BucketWorking} {
		if n[b] > 0 {
			counts = append(counts, fmt.Sprintf("%s %d", b, n[b]))
		}
	}
	var b strings.Builder
	b.WriteString(strings.Join(counts, " · ") + "\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "\n%s · %s\n  %s\n  %s · session %s · %s\n", r.Name, r.Bucket, r.Reason, r.Kind, r.Session, r.Cwd)
		if r.Parent != "" {
			fmt.Fprintf(&b, "  under %s\n", r.Parent)
		}
		for _, p := range r.OpenPRs {
			fmt.Fprintf(&b, "  PR %s: %s\n", github.PRName(p.PR), p.Status)
		}
		if r.NextWake != "" {
			fmt.Fprintf(&b, "  next wake-up %s (%s)\n", r.NextWake, r.NextWakeFor)
		}
	}
	return b.String()
}

// triageJSON is the JSON both the CLI and the MCP tool give.
func triageJSON(rows []TriageRow, stallAfter time.Duration) string {
	data, _ := json.MarshalIndent(map[string]any{"stall_after_seconds": int64(stallAfter.Seconds()), "sessions": rows}, "", " ")
	return string(data)
}

// watchConvs is the live sessions' transcripts as ccs watch last read them,
// so each pass parses only what was appended since (ParseAppended).
var watchConvs = map[string]*transcript.Conversation{}

// liveConversations reads just the running sessions' transcripts, for ccs
// watch, which keeps no list of every session: the first time in full, then
// only their new lines. Sessions that stopped are forgotten.
func liveConversations(live map[string]sessions.LiveSession) []transcript.Conversation {
	var out []transcript.Conversation
	for id := range watchConvs {
		if _, ok := live[id]; !ok {
			delete(watchConvs, id)
		}
	}
	for id, l := range live {
		path := transcript.TranscriptPath(id)
		if l.Codex {
			path = transcript.CodexRollout(id)
		}
		if path == "" {
			continue
		}
		var c *transcript.Conversation
		if prev := watchConvs[id]; prev != nil && prev.FilePath == path {
			c, _ = transcript.ParseAppended(prev)
		} else if l.Codex {
			c, _ = transcript.ParseCodexFile(path, time.Time{}, 0)
		} else {
			c, _ = transcript.ParseConversationFile(path, time.Time{}, 0)
		}
		if c == nil {
			c = watchConvs[id] // unreadable this pass: what was read before
		}
		if c != nil {
			watchConvs[id] = c
			out = append(out, *c)
		}
	}
	return out
}

// SessionEvent is a live session's change in pr-events.jsonl:
// session_started (first seen running), session_stalled, session_done,
// session_active (it left a stall or a finish: From says which and Seconds
// how long it sat there) and session_ended (gone from two scans in a row; At
// is when it was last seen, Seconds how long it ran as far as ccs saw, From
// its last stall or finish if it ended in one).
type SessionEvent struct {
	At          string `json:"at"`
	Event       string `json:"event"`
	Session     string `json:"session"` // its name
	SessionID   string `json:"session_id"`
	Kind        string `json:"kind"`
	Cwd         string `json:"cwd,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Idle        string `json:"idle,omitempty"`
	IdleSeconds int64  `json:"idle_seconds,omitempty"`
	From        string `json:"from,omitempty"`
	Seconds     int64  `json:"seconds,omitempty"`
}

// sessionEventsPath keeps which stall and which finish each live session was
// last reported for, beside the settings.
var sessionEventsPath = func() string {
	return filepath.Join(filepath.Dir(state.SettingsPath), "session-events.json")
}

// sessionEventRec is what was last emitted for one session: the IdleSince of
// the stall, the IdleSince of the finish, when the session was last seen
// running, and since when it has sat stalled or done (State, StateAt; empty
// once it moved on). First is when it was first seen running; Missed counts
// the scans in a row it was missing from, and Ended that its end was told.
type sessionEventRec struct {
	Stalled string    `json:"stalled,omitempty"`
	Done    string    `json:"done,omitempty"`
	Seen    time.Time `json:"seen"`
	Name    string    `json:"name,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	Cwd     string    `json:"cwd,omitempty"`
	First   time.Time `json:"first,omitzero"`
	State   string    `json:"state,omitempty"`
	StateAt time.Time `json:"state_at,omitzero"`
	Since   string    `json:"since,omitempty"` // the IdleSince of that stall or finish
	Missed  int       `json:"missed,omitempty"`
	Ended   bool      `json:"ended,omitempty"`
}

// sessionEventKeep is how long a session missing from the scans is
// remembered: a scan that missed it once (an unreadable session file, a
// failed process listing) mustn't make its stall news again.
const sessionEventKeep = 24 * time.Hour

// endAfterMisses is how many scans in a row a session must be missing from
// before session_ended: one missed scan is a flaky read, not an exit.
const endAfterMisses = 2

// sessionEvents is the events rows call for, given what was emitted before
// (prev, updated in place; a session unseen for sessionEventKeep dropped):
// one per stall and one per finish, a stall or finish being one stretch of
// idleness (IdleSince), so a session flapping between buckets in it isn't
// reported twice; session_active when it leaves one (new activity, or a
// bucket that is neither idle, stalled nor done); session_started for a
// session not in prev, except on the first scan ever (empty prev), which
// only records; session_ended once it is missing from endAfterMisses scans.
func sessionEvents(rows []TriageRow, prev map[string]sessionEventRec, now time.Time) []any {
	seeding := len(prev) == 0
	at := now.UTC().Format(time.RFC3339)
	var events []any
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Session] = true
		rec, known := prev[r.Session]
		if (!known || rec.Ended) && !seeding {
			events = append(events, SessionEvent{At: at, Event: "session_started", Session: r.Name, SessionID: r.Session, Kind: r.Kind, Cwd: r.Cwd})
		}
		if !known || rec.Ended {
			rec.First, rec.Ended, rec.State, rec.StateAt, rec.Since = now, false, "", time.Time{}, ""
		}
		rec.Seen, rec.Missed, rec.Name, rec.Kind, rec.Cwd = now, 0, r.Name, r.Kind, r.Cwd
		if rec.State != "" {
			moved := r.IdleSince != rec.Since || (r.Bucket != BucketIdle && r.Bucket != BucketStalled && r.Bucket != BucketDone)
			if moved {
				events = append(events, SessionEvent{At: at, Event: "session_active", Session: r.Name, SessionID: r.Session, Kind: r.Kind, Cwd: r.Cwd, Reason: r.Reason, From: rec.State, Seconds: int64(now.Sub(rec.StateAt).Seconds())})
				rec.State, rec.StateAt, rec.Since = "", time.Time{}, ""
			}
		}
		ev := ""
		switch {
		case r.Bucket == BucketStalled && rec.Stalled != r.IdleSince:
			ev, rec.Stalled = "session_stalled", r.IdleSince
		case r.Bucket == BucketDone && rec.Done != r.IdleSince:
			ev, rec.Done = "session_done", r.IdleSince
		}
		if ev != "" {
			rec.State, rec.StateAt, rec.Since = strings.TrimPrefix(ev, "session_"), now, r.IdleSince
			events = append(events, SessionEvent{At: at, Event: ev, Session: r.Name, SessionID: r.Session, Kind: r.Kind, Cwd: r.Cwd, Reason: r.Reason, Idle: r.Idle, IdleSeconds: r.IdleSeconds})
		}
		prev[r.Session] = rec
	}
	ids := make([]string, 0, len(prev))
	for id := range prev {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := prev[id]
		if seen[id] {
			continue
		}
		if now.Sub(rec.Seen) > sessionEventKeep {
			delete(prev, id)
			continue
		}
		if rec.Ended {
			continue
		}
		rec.Missed++
		if rec.Missed >= endAfterMisses && rec.Name == "" {
			rec.Ended = true // recorded before ccs kept names: its start was never told either
		} else if rec.Missed >= endAfterMisses {
			rec.Ended = true
			e := SessionEvent{At: rec.Seen.UTC().Format(time.RFC3339), Event: "session_ended", Session: rec.Name, SessionID: id, Kind: rec.Kind, Cwd: rec.Cwd, From: rec.State}
			if !rec.First.IsZero() {
				e.Seconds = int64(rec.Seen.Sub(rec.First).Seconds())
			}
			events = append(events, e)
		}
		prev[id] = rec
	}
	return events
}

// WatchSessions is github.SessionTick: triage the running sessions and
// write session_stalled and session_done for those that entered either.
func WatchSessions(stallAfter time.Duration, now time.Time) error {
	live := liveSessions()
	rows := TriageRows(liveConversations(live), live, now, stallAfter)
	prev := map[string]sessionEventRec{}
	if data, err := os.ReadFile(sessionEventsPath()); err == nil {
		json.Unmarshal(data, &prev)
	}
	events := sessionEvents(rows, prev, now)
	if err := github.WriteEventLines(events); err != nil {
		return err
	}
	return github.WriteJSON(sessionEventsPath(), prev)
}

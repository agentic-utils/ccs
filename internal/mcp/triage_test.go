package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// triageCase is one live session and the bucket it must land in.
type triageCase struct {
	id, status, waitingFor string
	idle                   time.Duration // in its status
	said                   string        // its last message, the session's
	pending                string        // a tool call still without a result: name and input
	tasks                  []transcript.BgTask
	scheds                 []transcript.Schedule
	prs                    []github.PRRow
	bucket, reason         string // reason: a part it must contain
}

func TestTriageBuckets(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ts := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339) }
	pr := func(n, group, status string, blocked ...string) github.PRRow {
		return github.PRRow{PR: "https://github.com/o/r/pull/" + n, Group: group, Status: status, BlockedBy: blocked}
	}
	cases := []triageCase{
		{id: "menu", status: "waiting", waitingFor: "input needed", idle: 3 * time.Minute, pending: `AskUserQuestion {"questions":[{"question":"Which plan?","options":[{"label":"A"}]}]}`,
			bucket: BucketWaitingOnUser, reason: "asks you: Which plan?"},
		{id: "prompt", status: "waiting", waitingFor: "permission prompt", pending: `Bash {"command":"rm -rf build"}`,
			bucket: BucketWaitingOnUser, reason: "permission prompt: wants to use Bash rm -rf build"},
		{id: "asked", status: "idle", idle: 40 * time.Minute, said: "PR is green. Shall I merge it?",
			bucket: BucketWaitingOnUser, reason: "its last message asks: PR is green. Shall I merge it?"},
		{id: "run-cmd", status: "idle", idle: 3 * time.Hour, said: "The token lapsed. Please run `! glogin -S` and tell me when it's done.",
			bucket: BucketWaitingOnUser, reason: "asks you to run a command: Please run `! glogin -S` and tell me when it's done."},
		{id: "ready-pr", status: "idle", idle: time.Hour, said: "Opened the PR.", prs: []github.PRRow{pr("1", "ready", "ready to merge")},
			bucket: BucketWaitingOnUser, reason: "r#1 ready to merge: needs your go"},
		{id: "busy", status: "busy", idle: 5 * time.Minute, said: "All done.",
			bucket: BucketWorking, reason: "mid-turn for 5m"},
		{id: "ci", status: "idle", idle: time.Hour, said: "Pushed; waiting on CI.", prs: []github.PRRow{pr("2", "waiting", "checks running")},
			bucket: BucketWaitingOnExternal, reason: "r#2 checks running"},
		{id: "review", status: "idle", idle: time.Hour, said: "Requested a review.", prs: []github.PRRow{pr("3", "waiting", "review required")},
			bucket: BucketWaitingOnExternal, reason: "r#3 review required"},
		{id: "depends", status: "idle", idle: time.Hour, said: "Waiting for the parent.", prs: []github.PRRow{pr("4", "waiting", "blocked: after r#3", "https://github.com/o/r/pull/3")},
			bucket: BucketWaitingOnExternal, reason: "r#4 blocked: after r#3"},
		{id: "task", status: "idle", idle: time.Hour, said: "Started the monitor.", tasks: []transcript.BgTask{{ID: "m", Kind: "monitor", Started: ts(time.Hour - time.Minute), Expires: now.Add(time.Hour)}},
			bucket: BucketWaitingOnExternal, reason: "1 background task"},
		{id: "wakeup", status: "idle", idle: time.Hour, said: "Checking back later.", scheds: []transcript.Schedule{{Kind: "wakeup", Desc: "watching CI", Set: ts(time.Minute), Next: now.Add(20 * time.Minute)}},
			bucket: BucketWaitingOnExternal, reason: "wakes in 20m (watching CI)"},
		{id: "cron", status: "idle", idle: time.Hour, said: "Polling.", scheds: []transcript.Schedule{{ID: "c", Kind: "cron", Recurring: true, Desc: "Every 3 minutes: poll", Set: ts(time.Minute)}},
			bucket: BucketWaitingOnExternal, reason: "recurring cron (Every 3 minutes: poll)"},
		{id: "done", status: "idle", idle: 2 * time.Hour, said: "Merged and released v1.2.0. Nothing left to do.",
			bucket: BucketDone, reason: "says it's finished: Merged and released v1.2.0."},
		{id: "step-merged", status: "idle", idle: 2 * time.Hour, said: "The prerequisite has been merged. Next I'll update the application.",
			bucket: BucketStalled, reason: "idle 2h00m"},
		{id: "still-needs", status: "idle", idle: 2 * time.Hour, said: "Tests pass. The deployment still needs to be done.",
			bucket: BucketStalled, reason: "idle 2h00m"},
		{id: "offer-run", status: "idle", idle: 2 * time.Hour, said: "All done. You can run this in your terminal to see it.",
			bucket: BucketDone, reason: "says it's finished: All done."},
		{id: "offer-send", status: "idle", idle: 2 * time.Hour, said: "All done. Send me a message if anything breaks.",
			bucket: BucketDone, reason: "says it's finished: All done."},
		{id: "hands-over", status: "idle", idle: 2 * time.Hour, said: "All four PRs are merged, so nothing more is needed on them.\nThe only thing left is your infra PR. Send me the link once it's open.",
			bucket: BucketWaitingOnUser, reason: "waits on you: Send me the link once it's open."},
		{id: "step-finished", status: "idle", idle: 2 * time.Hour, said: "The backtest has now finished and it passed. Next up is the 23 Nov check",
			bucket: BucketStalled, reason: "idle 2h00m, nothing pending"},
		{id: "offer", status: "idle", idle: 2 * time.Hour, said: "That's everything. Let me know if you'd like the chart too",
			bucket: BucketDone, reason: "says it's finished: That's everything."},
		{id: "not-done", status: "idle", idle: 2 * time.Hour, said: "The migration is not done yet: half the rows remain.",
			bucket: BucketStalled, reason: "idle 2h00m, nothing pending"},
		{id: "done-with-pr", status: "idle", idle: 2 * time.Hour, said: "All done.", prs: []github.PRRow{pr("5", "needs_you", "checks failing: test")},
			bucket: BucketStalled, reason: "r#5 checks failing: test"},
		{id: "stalled", status: "idle", idle: 20 * time.Minute, said: "Reading the logs next.",
			bucket: BucketStalled, reason: "idle 20m, nothing pending"},
		{id: "idle", status: "idle", idle: 5 * time.Minute, said: "Reading the logs next.",
			bucket: BucketIdle, reason: "idle 5m, nothing pending"},
		{id: "old-cron", status: "idle", idle: 20 * time.Minute, said: "Reading on.", scheds: []transcript.Schedule{{ID: "c", Kind: "cron", Recurring: true, Set: ts(3 * time.Hour)}}, // an earlier process's
			bucket: BucketStalled, reason: "idle 20m"},
	}
	start := now.Add(-2 * time.Hour).UTC().Format(time.ANSIC)
	live := map[string]sessions.LiveSession{}
	var convs []transcript.Conversation
	owned := map[string][]github.PRRow{}
	for _, c := range cases {
		conv := transcript.Conversation{SessionID: c.id, Title: c.id, Cwd: "/code/" + c.id, LastTimestamp: ts(c.idle), Tasks: c.tasks, Schedules: c.scheds}
		if c.said != "" {
			conv.Messages = []transcript.Message{{Role: "user", Text: "go", Ts: ts(c.idle + time.Minute)}, {Role: "assistant", Text: c.said, Ts: ts(c.idle)}}
		}
		if c.pending != "" {
			name, input, _ := strings.Cut(c.pending, " ")
			pend(t, &conv, name, input)
		}
		convs = append(convs, conv)
		live[c.id] = sessions.LiveSession{PID: 100, Start: start, Stat: sessions.SessionStat{Status: c.status, WaitingFor: c.waitingFor, Since: now.Add(-c.idle), Name: c.id}}
		owned[ownerKey("claude", c.id)] = c.prs
	}
	defer stubOwned(owned)()

	rows := TriageRows(convs, live, now, DefaultStallAfter)
	by := map[string]TriageRow{}
	for _, r := range rows {
		by[r.Session] = r
	}
	for _, c := range cases {
		r := by[c.id]
		if r.Bucket != c.bucket || !strings.Contains(r.Reason, c.reason) {
			t.Errorf("%s: %s %q, want %s with %q", c.id, r.Bucket, r.Reason, c.bucket, c.reason)
		}
	}
	if r := by["wakeup"]; r.NextWake != "2026-11-17T12:20:00Z" || r.NextWakeFor != "watching CI" || r.Schedules != 1 {
		t.Errorf("the next wake-up: %+v", r)
	}
	if r := by["stalled"]; r.Idle != "20m" || r.IdleSeconds != 1200 || r.IdleSince != "2026-11-17T11:40:00Z" || r.LastSaid != "Reading the logs next." {
		t.Errorf("idle time and since when: %+v", r)
	}
	if r := by["ci"]; len(r.OpenPRs) != 1 || r.OpenPRs[0].Status != "checks running" {
		t.Errorf("its open PRs with their status: %+v", r.OpenPRs)
	}
	// What needs someone first, the longest idle first within a bucket.
	if rows[0].Bucket != BucketWaitingOnUser || rows[0].Session != "run-cmd" || rows[len(rows)-1].Bucket != BucketWorking {
		t.Errorf("order: first %s/%s, last %s", rows[0].Session, rows[0].Bucket, rows[len(rows)-1].Bucket)
	}

	// A longer threshold keeps a 20-minute-idle session idle.
	rows = TriageRows(convs, live, now, 30*time.Minute)
	for _, r := range rows {
		if r.Session == "stalled" && r.Bucket != BucketIdle {
			t.Errorf("under a 30m threshold: %s", r.Bucket)
		}
	}
}

// stubOwned makes ownedPRs return m, and undoes it.
func stubOwned(m map[string][]github.PRRow) func() {
	old := ownedPRs
	ownedPRs = func(time.Time) map[string][]github.PRRow { return m }
	return func() { ownedPRs = old }
}

func TestTriageWithoutTranscript(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	defer stubOwned(nil)()
	live := map[string]sessions.LiveSession{"new": {PID: 1, Stat: sessions.SessionStat{Status: "idle", Name: "just-started"}}}
	rows := TriageRows(nil, live, now, DefaultStallAfter)
	if len(rows) != 1 || rows[0].Name != "just-started" || rows[0].Bucket != BucketIdle || rows[0].IdleSince != "" {
		t.Errorf("a session with no transcript and no status time is idle, never stalled: %+v", rows)
	}
}

func TestSessionEvents(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	stalled := TriageRow{Name: "a", Session: "s-a", Kind: "claude", Bucket: BucketStalled, Reason: "idle 20m, nothing pending", Idle: "20m", IdleSeconds: 1200, IdleSince: "2026-11-17T11:40:00Z"}
	done := TriageRow{Name: "b", Session: "s-b", Kind: "claude", Bucket: BucketDone, Reason: "says it's finished: done.", IdleSince: "2026-11-17T11:00:00Z"}
	prev := map[string]sessionEventRec{"gone": {Stalled: "x", Seen: now.Add(-sessionEventKeep - time.Minute)}}

	evs := sessionEvents([]TriageRow{stalled, done}, prev, now)
	if len(evs) != 2 {
		t.Fatalf("one event each: %+v", evs)
	}
	e := evs[0].(SessionEvent)
	if e.Event != "session_stalled" || e.Session != "a" || e.SessionID != "s-a" || e.Reason != stalled.Reason || e.Idle != "20m" || e.IdleSeconds != 1200 || e.At != "2026-11-17T12:00:00Z" {
		t.Errorf("session_stalled: %+v", e)
	}
	if e := evs[1].(SessionEvent); e.Event != "session_done" || e.Session != "b" {
		t.Errorf("session_done: %+v", e)
	}
	if _, ok := prev["gone"]; ok {
		t.Error("a session unseen for a day is forgotten")
	}
	// A scan that misses it once (an unreadable session file) doesn't make the same stall news again.
	sessionEvents(nil, prev, now.Add(3*time.Minute))
	if evs := sessionEvents([]TriageRow{stalled}, prev, now.Add(6*time.Minute)); len(evs) != 0 {
		t.Errorf("stalled, missed by one scan, stalled: one event: %+v", evs)
	}

	// The same stall again, or after flapping to idle and back: nothing.
	if evs := sessionEvents([]TriageRow{stalled, done}, prev, now); len(evs) != 0 {
		t.Errorf("at most once per stall: %+v", evs)
	}
	flap := stalled
	flap.Bucket = BucketIdle
	sessionEvents([]TriageRow{flap}, prev, now)
	if evs := sessionEvents([]TriageRow{stalled}, prev, now); len(evs) != 0 {
		t.Errorf("the same stretch of idleness is one stall: %+v", evs)
	}
	// It worked, then stalled again: a new stall.
	again := stalled
	again.IdleSince = "2026-11-17T12:30:00Z"
	if evs := sessionEvents([]TriageRow{again}, prev, now); len(evs) != 1 {
		t.Errorf("a new stall is reported: %+v", evs)
	}
}

func TestWatchSessionsWritesEvents(t *testing.T) {
	now := time.Now()
	defer stubOwned(nil)()
	oldLive := liveSessions
	defer func() { liveSessions = oldLive }()
	liveSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{"s-x": {PID: 1, Stat: sessions.SessionStat{Status: "idle", Since: now.Add(-time.Hour), Name: "quiet"}}}
	}
	os.Remove(github.PREventsPath())
	for range 2 {
		if err := WatchSessions(DefaultStallAfter, now); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(github.PREventsPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []SessionEvent
	for s := bufio.NewScanner(f); s.Scan(); {
		var e SessionEvent
		json.Unmarshal(s.Bytes(), &e)
		events = append(events, e)
	}
	if len(events) != 1 || events[0].Event != "session_stalled" || events[0].Session != "quiet" || events[0].IdleSeconds != 3600 {
		t.Errorf("one session_stalled across two passes: %+v", events)
	}
}

func TestTriageCLIAndTool(t *testing.T) {
	now := time.Now()
	defer stubOwned(nil)()
	oldLive := liveSessions
	defer func() { liveSessions = oldLive }()
	liveSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{
			"s-x": {PID: 1, Stat: sessions.SessionStat{Status: "idle", Since: now.Add(-20 * time.Minute), Name: "quiet"}},
			"s-y": {PID: 2, Stat: sessions.SessionStat{Status: "busy", Since: now.Add(-time.Minute), Name: "busy-one"}},
		}
	}
	convs := []transcript.Conversation{{SessionID: "s-x", Cwd: "/code/x"}, {SessionID: "s-y", Cwd: "/code/y"}}
	load := func() ([]transcript.Conversation, error) { return convs, nil }

	var out, errs strings.Builder
	if code := RunSessions([]string{"--triage", "--json", "--stall-after", "30m"}, &out, &errs, load); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var got struct {
		StallAfter int64       `json:"stall_after_seconds"`
		Sessions   []TriageRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatal(err, out.String())
	}
	if got.StallAfter != 1800 || len(got.Sessions) != 2 || got.Sessions[0].Bucket != BucketIdle || got.Sessions[1].Bucket != BucketWorking {
		t.Errorf("--triage --json with a 30m threshold: %s", out.String())
	}

	out.Reset()
	RunSessions([]string{"--triage"}, &out, &errs, load)
	if s := out.String(); !strings.HasPrefix(s, "stalled 1 · working 1\n") || !strings.Contains(s, "quiet · stalled\n  idle 20m, nothing pending") {
		t.Errorf("--triage text:\n%s", s)
	}
	if code := RunSessions([]string{"--triage", "--all"}, &out, &errs, load); code != 2 {
		t.Errorf("--triage with --all is refused: %d", code)
	}

	text, err := Call("session_triage", json.RawMessage(`{"stall_after_minutes":10,"project":"/code/y"}`), load)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil || got.StallAfter != 600 || len(got.Sessions) != 1 || got.Sessions[0].Session != "s-y" {
		t.Errorf("session_triage with project and stall_after_minutes: %s", text)
	}
}

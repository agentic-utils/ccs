package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// pend gives conv a tool call still without a result, as its transcript would.
func pend(t *testing.T, conv *transcript.Conversation, name, input string) {
	t.Helper()
	line := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":%q,"input":%s}]}}`, name, input)
	if !transcript.ParseLine(conv, []byte(line)) || conv.Pending == nil {
		t.Fatalf("no pending call from %s", line)
	}
}

// statusConvs: one waiting on a menu, one idle that asked, one busy with an
// API error and a task, one not running.
func statusConvs(t *testing.T, now time.Time) ([]transcript.Conversation, map[string]sessions.LiveSession) {
	t.Helper()
	ts := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339) }
	menu := transcript.Conversation{SessionID: "s-menu", Cwd: "/code/a", Title: "pick-a-plan", IsCustomTitle: true, LastTimestamp: ts(2 * time.Minute),
		Messages: []transcript.Message{{Role: "assistant", Text: "Let me ask.", Ts: ts(2 * time.Minute)}}}
	pend(t, &menu, "AskUserQuestion", `{"questions":[{"question":"Which plan?","multiSelect":false,"options":[{"label":"Small","description":"one PR"},{"label":"Large","description":""}]}]}`)
	asked := transcript.Conversation{SessionID: "s-asked", Cwd: "/code/b", Title: "fix-login", LastTimestamp: ts(10 * time.Minute),
		Messages: []transcript.Message{{Role: "assistant", Text: "Shall I push?", Ts: ts(10 * time.Minute)}}}
	busy := transcript.Conversation{SessionID: "s-busy", Cwd: "/code/c", Title: "migrate", LastTimestamp: ts(time.Minute),
		LastError: "529 Overloaded", LastErrorTs: ts(time.Minute), PRs: []string{"https://github.com/o/r/pull/7"},
		Messages: []transcript.Message{{Role: "user", Text: "go", Ts: ts(time.Minute)}},
		Tasks: []transcript.BgTask{
			{ID: "t1", Kind: "bash", Desc: "make watch", Started: ts(30 * time.Second)},
			{ID: "t2", Kind: "bash", Desc: "old", Started: ts(3 * time.Hour)},                          // an earlier process's
			{ID: "t3", Kind: "bash", Desc: "done", Started: ts(20 * time.Second), Status: "completed"}, // ended
		}}
	// A running tool call isn't a question.
	pend(t, &busy, "Bash", `{"command":"go test ./..."}`)
	closed := transcript.Conversation{SessionID: "s-closed", Cwd: "/code/d", Title: "old work", LastTimestamp: ts(48 * time.Hour),
		Messages: []transcript.Message{{Role: "assistant", Text: "Done?", Ts: ts(48 * time.Hour)}}}
	start := now.Add(-time.Hour).UTC().Format(time.ANSIC)
	live := map[string]sessions.LiveSession{
		"s-menu":  {PID: 11, Start: start, Stat: sessions.SessionStat{Status: "waiting", WaitingFor: "input needed", Since: now.Add(-2 * time.Minute), Name: "pick-a-plan"}},
		"s-asked": {PID: 12, Start: start, Stat: sessions.SessionStat{Status: "idle", Since: now.Add(-10 * time.Minute), Name: "fix-login"}},
		"s-busy":  {PID: 13, Start: start, Stat: sessions.SessionStat{Status: "busy", Since: now.Add(-time.Minute), Name: "migrate"}},
		"s-new":   {PID: 14, Start: start, Stat: sessions.SessionStat{Status: "busy", Name: "just-started"}}, // no transcript yet
	}
	return []transcript.Conversation{closed, busy, asked, menu}, live
}

func TestSessionRows(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	convs, live := statusConvs(t, now)
	rows := SessionRows(convs, live, false, now)
	var ids []string
	by := map[string]SessionRow{}
	for _, r := range rows {
		ids = append(ids, r.Session)
		by[r.Session] = r
	}
	// Those needing the user first, then an error, then the rest; not running left out.
	if got := strings.Join(ids, " "); got != "s-menu s-asked s-busy s-new" {
		t.Fatalf("order: %s", got)
	}
	if r := by["s-menu"]; r.Status != "waiting" || r.WaitingFor != "input needed" || r.Since != "2026-11-10T11:58:00Z" ||
		!strings.Contains(r.Question, "Which plan?") || !strings.Contains(r.Question, "1. Small: one PR") || !strings.Contains(r.Question, "2. Large") || r.Asked {
		t.Errorf("a waiting session carries its question: %+v", r)
	}
	if r := by["s-asked"]; r.Status != "idle" || !r.Asked || r.Question != "" {
		t.Errorf("an idle session whose last message asks: %+v", r)
	}
	if r := by["s-busy"]; r.Status != "busy" || r.Question != "" || r.LastError != "529 Overloaded" || r.LastErrorAt == "" ||
		r.Tasks != 1 || len(r.PRs) != 1 || r.Messages != 1 || r.PID != 13 {
		t.Errorf("a busy session: its error, its PRs, only its own process's running tasks, and no question: %+v", r)
	}
	if r := by["s-new"]; !r.Live || r.Name != "just-started" || r.Status != "busy" || r.Kind != "claude" {
		t.Errorf("a session with no transcript yet still shows: %+v", r)
	}
	all := SessionRows(convs, live, true, now)
	if last := all[len(all)-1]; len(all) != 5 || last.Session != "s-closed" || last.Live || last.Status != "" || last.Asked {
		t.Errorf("--all adds the ones not running, last, with no status: %+v", all)
	}
}

func TestRunSessions(t *testing.T) {
	now := time.Now()
	convs, live := statusConvs(t, now)
	old := liveSessions
	t.Cleanup(func() { liveSessions = old })
	liveSessions = func() map[string]sessions.LiveSession { return live }
	load := func() ([]transcript.Conversation, error) { return convs, nil }

	var out, errs strings.Builder
	if code := RunSessions([]string{"--json"}, &out, &errs, load); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var got struct {
		Sessions []SessionRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil || len(got.Sessions) != 4 || got.Sessions[0].Session != "s-menu" {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `"waiting_for": "input needed"`) || !strings.Contains(out.String(), `"last_error": "529 Overloaded"`) {
		t.Errorf("field names are the interface: %s", out.String())
	}

	out.Reset()
	RunSessions(nil, &out, &errs, load)
	text := out.String()
	for _, want := range []string{"pick-a-plan · waiting 2m: input needed", "asks: Which plan?", "fix-login · idle 10m", "its last message asks something",
		"last error: 529 Overloaded", "running: 1 background task(s)", "PRs: r#7", "just-started · busy"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "old work") {
		t.Errorf("a session not running shows only with --all:\n%s", text)
	}
	out.Reset()
	RunSessions([]string{"--all"}, &out, &errs, load)
	if !strings.Contains(out.String(), "old work · not running") {
		t.Errorf("--all:\n%s", out.String())
	}

	out.Reset()
	liveSessions = func() map[string]sessions.LiveSession { return nil }
	RunSessions([]string{"--json"}, &out, &errs, load)
	if strings.Join(strings.Fields(out.String()), "") != `{"sessions":[]}` {
		t.Errorf("none running is an empty list, not null: %s", out.String())
	}
	if code := RunSessions([]string{"extra"}, &out, &errs, load); code != 2 {
		t.Errorf("an argument it doesn't take: exit %d", code)
	}
}

func TestSessionsOpenPRsFromTheWatcher(t *testing.T) {
	now := time.Now()
	convs, live := statusConvs(t, now)
	for i := range convs {
		if convs[i].SessionID == "s-busy" {
			convs[i].PRs = []string{"https://github.com/o/r/pull/7", "https://github.com/o/r/pull/8", "https://github.com/o/r/pull/9"}
		}
	}
	old := watchedOpen
	t.Cleanup(func() { watchedOpen = old })
	watchedOpen = func() map[string]bool { return map[string]bool{"https://github.com/o/r/pull/8": true} }
	var busy SessionRow
	for _, r := range SessionRows(convs, live, false, now) {
		if r.Session == "s-busy" {
			busy = r
		}
	}
	if len(busy.PRs) != 3 || len(busy.OpenPRs) != 1 || busy.OpenPRs[0] != "https://github.com/o/r/pull/8" {
		t.Fatalf("open PRs are the ones the watcher has open: %+v", busy)
	}
	if d := strings.Join(busy.details(now), "\n"); !strings.Contains(d, "open PRs: r#8 (and 2 merged, closed or not yours)") {
		t.Errorf("text: %s", d)
	}
	watchedOpen = func() map[string]bool { return map[string]bool{} }
	busy.OpenPRs = nil
	if d := strings.Join(busy.details(now), "\n"); !strings.Contains(d, "no open PRs (3 merged, closed or not yours)") {
		t.Errorf("none open: %s", d)
	}
}

func TestMCPListSessionsSaysWhatLiveOnesDo(t *testing.T) {
	now := time.Now()
	convs, live := statusConvs(t, now)
	old := liveSessions
	t.Cleanup(func() { liveSessions = old })
	liveSessions = func() map[string]sessions.LiveSession { return live }
	load := func() ([]transcript.Conversation, error) { return convs, nil }

	text, err := Call("list_sessions", nil, load)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"live: waiting 2m: input needed", "asks: Which plan?", "live: idle 10m", "last error: 529 Overloaded", "old work"} {
		if !strings.Contains(text, want) {
			t.Errorf("list_sessions lacks %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "live: "); n != 3 {
		t.Errorf("only running sessions get a live line: %d\n%s", n, text)
	}
	text, _ = Call("list_sessions", json.RawMessage(`{"live_only":true}`), load)
	if strings.Contains(text, "old work") || !strings.HasPrefix(text, "3 sessions") {
		t.Errorf("live_only:\n%s", text)
	}
}

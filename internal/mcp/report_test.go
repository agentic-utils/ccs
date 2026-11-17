package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func writeLines(t *testing.T, path string, lines ...any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	var b bytes.Buffer
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func userLine(at time.Time, text string, meta bool) map[string]any {
	return map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339Nano), "cwd": "/orch", "isMeta": meta, "message": map[string]any{"role": "user", "content": text}}
}

func assistantLine(at time.Time, blocks ...map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "timestamp": at.Format(time.RFC3339Nano), "cwd": "/orch", "message": map[string]any{"role": "assistant", "content": blocks}}
}

func bash(cmd string) map[string]any {
	return map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": cmd}}
}

func say(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

const viaCCSMsg = "Another Claude session sent a message:\n<cross-session-message from-name=\"the user, via ccs\">\n[Typed by the user in ccs, their session browser.]\n\n%s\n</cross-session-message>\n\nThis came from another Claude session."

// reportFixture lays out an orchestrator "boss" with a sub-orchestrator
// "sub" running "worker-one"; "worker-two" is boss's own. Returns the period's start.
func reportFixture(t *testing.T) (t0 time.Time) {
	t.Helper()
	dir := t.TempDir()
	oldProjects, oldParents, oldTop, oldReports := transcript.GetProjectsDir, sessions.ParentsPath, reportTopID, reportDir
	t.Cleanup(func() {
		transcript.GetProjectsDir, sessions.ParentsPath, reportTopID, reportDir = oldProjects, oldParents, oldTop, oldReports
		os.Remove(github.PREventsPath())
	})
	transcript.GetProjectsDir = func() string { return filepath.Join(dir, "projects") }
	sessions.ParentsPath = func() string { return filepath.Join(dir, "launch-parents.json") }
	reportTopID = func() (string, string) { return "boss-id", "" }
	reportDir = func() string { return filepath.Join(dir, "reports") }

	t0 = time.Now().UTC().Add(-5 * time.Hour).Truncate(time.Minute)
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	parents := map[string]any{"parents": map[string]any{
		"sub":        map[string]any{"child": "sub", "child_id": "sub-id", "parent": "boss", "parent_id": "boss-id"},
		"worker-one": map[string]any{"child": "worker-one", "child_id": "worker-one-id", "parent": "sub", "parent_id": "sub-id"},
		"worker-two": map[string]any{"child": "worker-two", "child_id": "worker-two-id", "parent": "boss", "parent_id": "boss-id"},
	}}
	data, _ := json.Marshal(parents)
	os.WriteFile(sessions.ParentsPath(), data, 0o600)

	ev := func(at time.Time, kv ...any) map[string]any {
		e := map[string]any{"at": at.Format(time.RFC3339)}
		for i := 0; i < len(kv); i += 2 {
			e[kv[i].(string)] = kv[i+1]
		}
		return e
	}
	pr := "https://github.com/o/r/pull/7"
	writeLines(t, github.PREventsPath(),
		ev(m(-60), "event", "new", "pr", pr, "repo", "o/r", "number", 7, "opened_by", map[string]any{"name": "worker-one", "session": "worker-one-id"}),
		ev(m(10), "event", "session_stalled", "session", "worker-one", "session_id", "worker-one-id"),
		ev(m(40), "event", "session_active", "session", "worker-one", "session_id", "worker-one-id", "from", "stalled"),
		ev(m(50), "event", "session_done", "session", "worker-two", "session_id", "worker-two-id"),
		ev(m(80), "event", "session_ended", "session", "worker-two", "session_id", "worker-two-id", "from", "done"),
		ev(m(90), "event", "ready", "pr", pr, "repo", "o/r", "number", 7),
		ev(m(120), "event", "merged", "pr", pr, "repo", "o/r", "number", 7),
		ev(m(200), "event", "session_stalled", "session", "worker-two", "session_id", "worker-two-id"), // after the period
		ev(m(210), "event", "session_ended", "session", "worker-two", "session_id", "worker-two-id", "from", "stalled"),
	)
	writeLines(t, filepath.Join(dir, "projects", "-orch", "boss-id.jsonl"),
		userLine(m(-30), "before the period: i see 3 idle sessions", false),
		userLine(m(5), "launch a session for the flaky test", false),
		assistantLine(m(6), bash("ccs launch --name flaky --brief b.md"), map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "/orch/needs-bharat.md"}}),
		userLine(m(15), "<task-notification>worker-one idle 9 idle sessions</task-notification>", false),
		userLine(m(20), strings.Replace(viaCCSMsg, "%s", "i see 8 idle sessions...", 1), true),
		assistantLine(m(22), say("worker-one has stalled; I've told it to carry on."), bash("cd /repo && git log -3")),
		userLine(m(30), "no, ask me questions, dont give me a list", false),
		assistantLine(m(31), map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "/repo/main.go"}}),
		assistantLine(m(95), say("[r#7](https://github.com/o/r/pull/7) is ready to merge.")),
	)
	writeLines(t, filepath.Join(dir, "projects", "-sub", "sub-id.jsonl"),
		userLine(m(12), "anything stalled?", false),
		assistantLine(m(14), say("worker-one is stalled, nudging it")),
	)
	return t0
}

func TestBuildReport(t *testing.T) {
	t0 := reportFixture(t)
	cards, err := BuildReport(ReportOpts{From: t0, To: t0.Add(3 * time.Hour), Now: t0.Add(10 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[0].Orchestrator != "boss" || cards[1].Orchestrator != "sub" {
		t.Fatalf("boss (the one in orchestrator.json) first, then sub: %+v", cards)
	}
	b := cards[0]
	if b.Sessions != 3 || b.Depth != 2 {
		t.Errorf("boss's tree: 3 sessions, 2 deep: %d %d", b.Sessions, b.Depth)
	}
	// Response: the stall (answered 12m later), the finish (never named), ready (5m).
	if b.Response.N != 2 || b.Response.Missed != 1 || b.Response.P50 != 5 || b.Response.P90 != 12 {
		t.Errorf("response: %+v", b.Response)
	}
	if b.StalledMinutes != 30 || b.WorstStall != "worker-one 30m" {
		t.Errorf("stalled 30m, worker-one the worst; the stall after the period not counted: %v %q", b.StalledMinutes, b.WorstStall)
	}
	if b.DoneToClose.N != 1 || b.DoneToClose.P50 != 30 || b.DoneToClose.Open != 0 {
		t.Errorf("done to close: %+v", b.DoneToClose)
	}
	if b.UserMessages != 3 || b.IdleNudges != 1 || b.Corrections != 1 || len(b.Quotes) != 2 {
		t.Errorf("user messages: 3 in the period (the harness note and the one before don't count), one nudge typed in ccs, one correction: %+v", b)
	}
	if b.ToolCalls != 4 || b.HandsOn != 2 || b.CCSViaBash != 1 {
		t.Errorf("tools: 4 calls, git and a Read outside its folder hands-on, one ccs launch via Bash: %+v", b)
	}
	if strings.Join(b.HandsOnTop, ",") != "Bash git ×1,Read ×1" {
		t.Errorf("hands-on top: %v", b.HandsOnTop)
	}
	if b.Merged != 1 || b.ReadyToMerge.P50 != 30 {
		t.Errorf("merged one, 30m after ready: %d %+v", b.Merged, b.ReadyToMerge)
	}

	s := cards[1]
	if s.Sessions != 1 || s.StalledMinutes != 30 || s.Merged != 1 {
		t.Errorf("sub answers for worker-one and its PR only: %+v", s)
	}
	if s.Response.N != 1 || s.Response.Missed != 1 || s.Response.P50 != 4 {
		t.Errorf("sub named the stall 4m after it but never the ready PR: %+v", s.Response)
	}
}

func TestReportSavesAndCompares(t *testing.T) {
	t0 := reportFixture(t)
	day1, err := BuildReport(ReportOpts{From: t0, To: t0.Add(3 * time.Hour), Orchestrator: "boss", Now: t0.Add(10 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	withPrevious(day1, true)
	if _, err := os.Stat(reportFile("boss", t0.Add(3*time.Hour))); err != nil {
		t.Fatalf("saved as the day it ends: %v", err)
	}
	day2, _ := BuildReport(ReportOpts{From: t0.Add(24 * time.Hour), To: t0.Add(27 * time.Hour), Orchestrator: "boss", Now: t0.Add(40 * time.Hour)})
	day2 = withPrevious(day2, false)
	if day2[0].Prev == nil || day2[0].Prev.StalledMinutes != 30 {
		t.Fatalf("day 2 compares with day 1: %+v", day2[0].Prev)
	}
	out := formatScorecard(day2[0])
	for _, want := range []string{"boss · ", "stalled minutes    0        (30)     ▲", "your idle nudges   0        (1)      ▲", "merged             0        (1)      ▼"} {
		if !strings.Contains(out, want) {
			t.Errorf("scorecard lacks %q:\n%s", want, out)
		}
	}
	if _, err := BuildReport(ReportOpts{From: t0, To: t0.Add(time.Hour), Orchestrator: "nobody"}); err == nil {
		t.Error("an unknown orchestrator is an error")
	}
}

func TestReportWithoutOrchestrator(t *testing.T) {
	dir := t.TempDir()
	oldParents, oldTop := sessions.ParentsPath, reportTopID
	defer func() { sessions.ParentsPath, reportTopID = oldParents, oldTop }()
	sessions.ParentsPath = func() string { return filepath.Join(dir, "none.json") }
	reportTopID = func() (string, string) { return "", "" }
	var out, errb bytes.Buffer
	if code := RunReport([]string{"--no-save"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no orchestrator found") {
		t.Errorf("no orchestrator: exit 1 and say so: %d %q", code, errb.String())
	}
}

func TestReportCLIAndTool(t *testing.T) {
	reportFixture(t)
	var out, errb bytes.Buffer
	if code := RunReport([]string{"--no-save", "--since", "48h", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var r struct{ Scorecards []Scorecard }
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || len(r.Scorecards) != 2 {
		t.Fatalf("JSON scorecards: %v %s", err, out.String())
	}
	text, err := Call("orchestrator_report", json.RawMessage(`{"orchestrator":"sub","save":false}`), nil)
	if err != nil || !strings.Contains(text, `"orchestrator": "sub"`) || strings.Contains(text, `"orchestrator": "boss"`) {
		t.Errorf("tool, one orchestrator: %v %s", err, text)
	}
	if entries, _ := os.ReadDir(reportDir()); len(entries) != 0 {
		t.Error("save false saves nothing")
	}
}

func TestTypedByUser(t *testing.T) {
	for in, want := range map[string]string{
		`"plain words"`: "plain words",
		`"<task-notification>x</task-notification>"`:                     "",
		`"Another Claude session sent a message:\n<teammate-message>x"`:  "",
		`[{"type":"tool_result","content":"x"}]`:                         "",
		`[{"type":"text","text":"from the tab"}]`:                        "from the tab",
		`"This session is being continued from a previous conversation"`: "",
	} {
		if got := typedByUser(json.RawMessage(in)); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
	msg, _ := json.Marshal(strings.Replace(viaCCSMsg, "%s", "close the done ones", 1))
	if got := typedByUser(msg); got != "close the done ones" {
		t.Errorf("typed in ccs: %q", got)
	}
}

func TestHandsOn(t *testing.T) {
	use := func(name string, in map[string]any) toolUse {
		data, _ := json.Marshal(in)
		return toolUse{Name: name, Input: data}
	}
	for _, c := range []struct {
		u    toolUse
		want bool
	}{
		{use("Bash", map[string]any{"command": "ccs sessions --json"}), false},
		{use("Bash", map[string]any{"command": "gh pr view 12 --json state"}), false},
		{use("Bash", map[string]any{"command": "gh pr diff 12"}), true},
		{use("Bash", map[string]any{"command": "cd /x && GIT_PAGER=cat git log"}), true},
		{use("Bash", map[string]any{"command": "kubectl --context c get pods"}), true},
		{use("Read", map[string]any{"file_path": "/orch/briefs/a.md"}), false},
		{use("Edit", map[string]any{"file_path": "/repo/a.go"}), true},
		{use("Agent", map[string]any{"prompt": "go read /repo"}), false},
		{use("WebFetch", map[string]any{"url": "https://x"}), true},
	} {
		if got, _ := handsOn(c.u, "/orch"); got != c.want {
			t.Errorf("%s %s: %v", c.u.Name, c.u.Input, got)
		}
	}
	for cmd, want := range map[string]bool{
		"ccs sessions --triage": true, "nohup ccs watch &": false, "echo; ccs close --graceful x": true, "ccs claim x": true, "ccs orchestrator-report": false,
	} {
		if ccsBash.MatchString(cmd) != want {
			t.Errorf("ccs via bash %q: want %v", cmd, want)
		}
	}
}

func TestPatterns(t *testing.T) {
	for _, s := range []string{"i see 8 idle sessions...", "idle prs are piling up", "i spy with my little eyes 11 idle sessions", "any sessions reapable"} {
		if !idleNudge.MatchString(s) {
			t.Errorf("idle nudge missed: %q", s)
		}
	}
	for _, s := range []string{"ask me questions pls... dont give me a list", "there are far too many in flight", "no, that was me", "why did you merge it", "use the mcp not bash", "did u actually check?", "did u ever check if the workflows:pii pr is safe to merge?", "ur forgetting to hand off investigations to sessions, pls reload your system instructions", "thats a lot of things that needs me", "also why are you not reacting?", "18 resumed sounds like too many"} {
		if !correction.MatchString(s) {
			t.Errorf("correction missed: %q", s)
		}
	}
	for _, s := range []string{"some grants are too long to evaluate", "launch a session for the idle timeout bug fix", "nothing more for now", "know the answer?"} {
		if idleNudge.MatchString(s) || correction.MatchString(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}

// reportTopID reads a coordinator.json from before the rename, naming the
// session coordinator, as ccs then called it, when the record names none.
func TestReportTopIDReadsTheCoordinatorRecord(t *testing.T) {
	old := state.SettingsPath
	state.SettingsPath = filepath.Join(t.TempDir(), "settings.json")
	defer func() { state.SettingsPath = old }()
	os.WriteFile(state.LegacyOrchestratorPath(), []byte(`{"session_id":"old-id"}`), 0o600)
	if id, name := reportTopID(); id != "old-id" || name != "coordinator" {
		t.Errorf("coordinator.json: %q %q", id, name)
	}
	os.WriteFile(state.OrchestratorPath(), []byte(`{"session_id":"new-id"}`), 0o600)
	if id, name := reportTopID(); id != "new-id" || name != "" {
		t.Errorf("orchestrator.json: %q %q", id, name)
	}
}

package mcp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// askConv is a session that asked two AskUserQuestion menus: one answered
// with a choice and notes, one declined.
func askConv(t *testing.T) transcript.Conversation {
	t.Helper()
	conv := transcript.Conversation{SessionID: "ask-1", Cwd: "/code/a", LastTimestamp: "2026-11-01T10:05:00Z"}
	lines := []string{
		`{"type":"user","timestamp":"2026-11-01T10:00:00Z","message":{"content":"ship it?"}}`,
		`{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","message":{"content":[{"type":"text","text":"CI is green."},{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Merge PR 12 now?","multiSelect":false,"options":[{"label":"Merge now (Recommended)","description":"merge commit"},{"label":"Wait","description":""}]}]}}]}}`,
		`{"type":"user","timestamp":"2026-11-01T10:02:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"q1","content":"Your questions have been answered: \"Merge PR 12 now?\"=\"Merge now (Recommended)\". You can now continue."}]},"toolUseResult":{"questions":[],"answers":{"Merge PR 12 now?":"Merge now (Recommended)"},"annotations":{"Merge PR 12 now?":{"notes":"after the deploy window"}}}}`,
		`{"type":"assistant","timestamp":"2026-11-01T10:03:00Z","message":{"content":[{"type":"text","text":"Merging."}]}}`,
		`{"type":"assistant","timestamp":"2026-11-01T10:04:00Z","message":{"content":[{"type":"tool_use","id":"q2","name":"AskUserQuestion","input":{"questions":[{"question":"Close the old PR?","multiSelect":true,"options":[{"label":"Yes","description":""}]}]}}]}}`,
		`{"type":"user","timestamp":"2026-11-01T10:05:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"q2","is_error":true,"content":"The user doesn't want to proceed with this tool use."}]}}`,
	}
	for _, l := range lines {
		if !transcript.ParseLine(&conv, []byte(l)) {
			t.Fatalf("unparsed: %s", l)
		}
	}
	return conv
}

func TestReadSessionShowsAskUserQuestion(t *testing.T) {
	conv := askConv(t)
	load := func() ([]transcript.Conversation, error) { return []transcript.Conversation{conv}, nil }
	text, err := Call("read_session", json.RawMessage(`{"session_id":"ask-1"}`), load)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Claude asks (AskUserQuestion):\nMerge PR 12 now?\n1. Merge now (Recommended): merge commit\n2. Wait",
		"User answered:\nMerge PR 12 now?\n→ Merge now (Recommended)\nNotes: after the deploy window",
		"Close the old PR? (pick any)",
		"User declined:\nThe user doesn't want to proceed",
		"messages 1-7 of 7",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("read_session lacks %q:\n%s", want, text)
		}
	}
	// In order: the reply's text, the question, the answer, the next reply.
	if !(strings.Index(text, "CI is green.") < strings.Index(text, "Merge PR 12 now?\n1.") &&
		strings.Index(text, "Merge PR 12 now?\n1.") < strings.Index(text, "→ Merge now") &&
		strings.Index(text, "→ Merge now") < strings.Index(text, "Merging.")) {
		t.Errorf("asks out of place:\n%s", text)
	}
	if len(conv.Messages) != 3 {
		t.Errorf("asks must not become messages (counts, times and the list ignore them): %d", len(conv.Messages))
	}

	text, _ = Call("read_session", json.RawMessage(`{"session_id":"ask-1","query":"deploy window"}`), load)
	if !strings.Contains(text, "Notes: after the deploy window") || !strings.Contains(text, "messages 1-3 of 3") {
		t.Errorf("the query should match an answer's notes, with its neighbours:\n%s", text)
	}
}

// A message that arrives while the menu waits comes between the question and
// its answer.
func TestReadSessionAnswerAfterWhatArrivedMeanwhile(t *testing.T) {
	conv := transcript.Conversation{SessionID: "ask-3"}
	for _, l := range []string{
		`{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","message":{"content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}]}}]}}`,
		`{"type":"user","timestamp":"2026-11-01T10:02:00Z","message":{"content":"meanwhile, CI went red"}}`,
		`{"type":"user","timestamp":"2026-11-01T10:03:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"q1","content":"answered"}]},"toolUseResult":{"answers":{"Which?":"B"}}}`,
	} {
		transcript.ParseLine(&conv, []byte(l))
	}
	got, _ := mcpRead([]transcript.Conversation{conv}, "ask-3", "", 0, 40)
	got = got[strings.Index(got, "\n[0]"):] // past the header, whose topic is the first user message
	q, mid, ans := strings.Index(got, "Which?\n1. A"), strings.Index(got, "meanwhile"), strings.Index(got, "→ B")
	if q < 0 || mid < 0 || ans < 0 || !(q < mid && mid < ans) {
		t.Errorf("want question, then the message, then the answer:\n%s", got)
	}
}

// A pruned transcript has no toolUseResult: the result's text is the answer.
func TestReadSessionAskWithoutToolUseResult(t *testing.T) {
	conv := transcript.Conversation{SessionID: "ask-2"}
	for _, l := range []string{
		`{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","message":{"content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}]}}]}}`,
		`{"type":"user","timestamp":"2026-11-01T10:02:00Z","message":{"content":[{"type":"tool_result","tool_use_id":"q1","content":"User has answered your questions: \"Which?\"=\"B\"."}]}}`,
	} {
		transcript.ParseLine(&conv, []byte(l))
	}
	got, err := mcpRead([]transcript.Conversation{conv}, "ask-2", "", 0, 40)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "User answered:\nUser has answered your questions: \"Which?\"=\"B\".") {
		t.Errorf("fallback answer:\n%s", got)
	}
}

// A session not running still lists everything needed to resume it.
func TestListSessionsRowsCarryWhatResumeNeeds(t *testing.T) {
	text := mcpList(mcpConvs(), nil, nil, 20)
	for _, want := range []string{"kind claude", "session aaaa-1111", "project /code/payments", "last active 2026-10-01", "resume: cd /code/payments && claude --resume aaaa-1111"} {
		if !strings.Contains(text, want) {
			t.Errorf("list_sessions row lacks %q:\n%s", want, text)
		}
	}
}

func TestListPRsMerged(t *testing.T) {
	t.Cleanup(func() { os.Remove(github.PRWatchStatePath()) })
	u := "https://github.com/o/r/pull/7"
	if err := github.SavePRWatchState(github.PRWatchState{Author: "me", PRs: map[string]*github.WatchedPR{
		u: {Repo: "o/r", Number: 7, Title: "t", State: "merged", MergedAt: "2026-11-01T10:00:00Z", Merge: github.MergeLocal, MergedBy: &github.PRActor{Name: "worker", Session: "sid-1"}},
	}}); err != nil {
		t.Fatal(err)
	}
	text, err := Call("list_prs", json.RawMessage(`{"merged":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var rows []github.MergedRow
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, text)
	}
	if len(rows) != 1 || rows[0].Merge != "local" || rows[0].MergedBy == nil || rows[0].MergedBy.Name != "worker" || rows[0].MergedBy.Session != "sid-1" {
		t.Errorf("merged rows: %s", text)
	}
}

func TestClaimPRForANamedSession(t *testing.T) {
	t.Cleanup(func() { os.Remove(github.ClaimsPath()) })
	old := github.LiveNamed
	t.Cleanup(func() { github.LiveNamed = old })
	github.LiveNamed = func(name string) []github.PRClaim {
		if name == "worker" {
			return []github.PRClaim{{Session: "sid-w", Name: "worker", PID: 4242, ProcStart: "x"}}
		}
		return nil
	}
	u := "https://github.com/o/r/pull/9"
	text, err := Call("claim_pr", json.RawMessage(`{"pr":"`+u+`","session":"worker"}`), nil)
	if err != nil || text != "worker owns "+u {
		t.Fatalf("claim: %q %v", text, err)
	}
	if c := github.LoadClaims().Claims[u]; len(c) != 1 || c[0].Session != "sid-w" || c[0].PID != 4242 {
		t.Errorf("claims: %+v", c)
	}
	if _, err := Call("claim_pr", json.RawMessage(`{"pr":"`+u+`","session":"nobody"}`), nil); err == nil || !strings.Contains(err.Error(), "no running session named nobody") {
		t.Errorf("an unknown name: %v", err)
	}
	if _, err := Call("claim_pr", json.RawMessage(`{"pr":"o/r#9"}`), nil); err == nil {
		t.Error("claim_pr takes an address only, as ccs claim does")
	}
	if text, err := Call("claim_pr", json.RawMessage(`{"pr":"`+u+`","session":"worker","release":true}`), nil); err != nil || !strings.Contains(text, "released") {
		t.Errorf("release: %q %v", text, err)
	}
	if c := github.LoadClaims().Claims[u]; len(c) != 0 {
		t.Errorf("released claim kept: %+v", c)
	}
}

func TestDependsTool(t *testing.T) {
	t.Cleanup(func() { os.Remove(github.DependsPath()) })
	old := github.LiveByPID
	t.Cleanup(func() { github.LiveByPID = old })
	github.LiveByPID = func() map[int]github.PRClaim { return nil }
	a, b := "https://github.com/o/r/pull/1", "https://github.com/o/r/pull/2"
	text, err := Call("depends", json.RawMessage(`{"pr":"`+b+`","after":["o/r#1"]}`), nil)
	if err != nil || !strings.Contains(text, "r#2 waits for r#1") {
		t.Fatalf("add: %q %v", text, err)
	}
	text, err = Call("depends", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rows []github.DependRow
	if err := json.Unmarshal([]byte(text), &rows); err != nil || len(rows) != 1 || rows[0].PR != b || rows[0].After[0] != a || len(rows[0].Waiting) != 1 {
		t.Errorf("list: %v\n%s", err, text)
	}
	if _, err := Call("depends", json.RawMessage(`{"pr":"`+a+`","after":["`+b+`"]}`), nil); err == nil || !strings.Contains(err.Error(), "circle") {
		t.Errorf("a circle should be refused: %v", err)
	}
	for _, bad := range []string{`{"after":["o/r#1"]}`, `{"pr":"o/r#2"}`, `{"pr":"o/r#2","drop":true,"after":["o/r#1"]}`} {
		if _, err := Call("depends", json.RawMessage(bad), nil); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}
	if text, err := Call("depends", json.RawMessage(`{"pr":"o/r#2","drop":true}`), nil); err != nil || !strings.Contains(text, "no longer waits") {
		t.Errorf("drop: %q %v", text, err)
	}
	if text, _ := Call("depends", nil, nil); strings.TrimSpace(text) != "[]" {
		t.Errorf("list after drop: %s", text)
	}
}

package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

const (
	agyConvA = "82853300-f2a5-404c-bbdb-b6206c001a3e"
	agyConvB = "32fc4442-255e-45ad-98ff-05f4c88e892e"
)

func agyEnv(t *testing.T, summaries map[string]transcript.AgySummary) {
	t.Helper()
	oldHome := transcript.AgyHome
	oldQuery := transcript.QueryAgySummaries
	oldLive := sessions.LiveAgySessions
	oldLocks := sessions.AgyPresenceLocks
	home := t.TempDir()
	transcript.AgyHome = func() string { return home }
	if summaries != nil {
		os.WriteFile(filepath.Join(home, "conversation_summaries.db"), []byte("db"), 0o600)
		transcript.QueryAgySummaries = func(string) (map[string]transcript.AgySummary, error) {
			return summaries, nil
		}
	} else {
		transcript.QueryAgySummaries = func(string) (map[string]transcript.AgySummary, error) {
			return map[string]transcript.AgySummary{}, nil
		}
	}
	resetAgyCaches()
	t.Cleanup(func() {
		transcript.AgyHome = oldHome
		transcript.QueryAgySummaries = oldQuery
		sessions.LiveAgySessions = oldLive
		sessions.AgyPresenceLocks = oldLocks
		resetAgyCaches()
	})
}

func resetAgyCaches() {
	transcript.AgySummaryCache.Lock()
	transcript.AgySummaryCache.Rows = nil
	transcript.AgySummaryCache.Unlock()
	transcript.AgyHistoryCache.Lock()
	transcript.AgyHistoryCache.Cwds = nil
	transcript.AgyHistoryCache.Unlock()
	sessions.AgyLiveCache.Lock()
	sessions.AgyLiveCache.Out = nil
	sessions.AgyLiveCache.Unlock()
}

func writeAgyTranscript(t *testing.T, id string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(transcript.AgyHome(), "brain", id, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func agyFixture(t *testing.T, id, cwd, title, parentID, status string, extra ...string) string {
	t.Helper()
	uris, _ := json.Marshal([]string{"file://" + cwd})
	agyEnv(t, map[string]transcript.AgySummary{
		id: {
			ID:            id,
			Title:         title,
			WorkspaceURIs: string(uris),
			Status:        status,
			ParentID:      parentID,
		},
	})
	lines := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T02:39:41Z","content":"<USER_REQUEST>\nopen a pull request for the flux capacitor\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nlocal time\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T02:39:45Z","model":"gemini-4-neon-medium","input_tokens":2205,"cache_read_tokens":10746,"output_tokens":476,"tool_calls":[{"name":"run_command","args":{"CommandLine":"\"gh pr create --fill\""}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-22T02:39:48Z","content":"Created At: 2026-11-22T02:39:46Z\nCommand exited with code 0.\n\nCommand output:\nhttps://github.com/o/r/pull/42\n"}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T02:39:50Z","model":"gemini-4-neon-medium","input_tokens":1500,"cache_read_tokens":12000,"output_tokens":120,"content":"Opened the capacitor PR."}`,
	}
	lines = append(lines, extra...)
	return writeAgyTranscript(t, id, lines...)
}

func TestAgyTranscriptParses(t *testing.T) {
	path := agyFixture(t, agyConvA, "/w/flux", "Flux Capacitor", "", "CASCADE_RUN_STATUS_IDLE")
	c, err := transcript.ParseAgyFile(path, time.Time{}, 0)
	if err != nil || c == nil {
		t.Fatalf("parse: %v %v", c, err)
	}
	if c.Kind != transcript.KindAgy || c.SessionID != agyConvA || c.Cwd != "/w/flux" || c.ActiveModel != "gemini-4-neon-medium" {
		t.Errorf("identity: %+v", c)
	}
	if len(c.Messages) != 2 || c.Messages[0].Text != "open a pull request for the flux capacitor" || c.Messages[1].Role != "assistant" || c.Messages[1].Text != "Opened the capacitor PR." {
		t.Errorf("messages: %+v", c.Messages)
	}
	if c.ContextTokens != 13500 || c.WindowSize() != 1_000_000 {
		t.Errorf("context %d of %d, want 13500 of 1000000", c.ContextTokens, c.WindowSize())
	}
	if c.Usage.Input != 3705 || c.Usage.CacheRead != 22746 || c.Usage.Output != 596 {
		t.Errorf("usage: %+v", c.Usage)
	}
	if !slices.Equal(c.PRs, []string{"https://github.com/o/r/pull/42"}) {
		t.Errorf("PR from gh pr create: %v", c.PRs)
	}
	if c.Title != "Flux Capacitor" || c.IsCustomTitle || c.Spawned || c.AgyBusy {
		t.Errorf("title/flags: %+v", c)
	}
	if !strings.Contains(c.SearchLower, "agy") {
		t.Error("searching agy finds every agy session")
	}
	if st := sessions.AgyStat(agyConvA, path); st.Status != "idle" {
		t.Errorf("finished turn is idle: %+v", st)
	}
	appendTo(t, path, `{"step_index":4,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T02:41:00Z","content":"<USER_REQUEST>\nnext step\n</USER_REQUEST>"}`+"\n")
	if st := sessions.AgyStat(agyConvA, path); st.Status != "busy" {
		t.Errorf("pending user input is busy: %+v", st)
	}
}

func TestAgySubagentAndScan(t *testing.T) {
	path := agyFixture(t, agyConvB, "/w/flux", "Subagent", agyConvA, "CASCADE_RUN_STATUS_IDLE")
	c, err := transcript.ParseAgyFile(path, time.Time{}, 0)
	if err != nil || c == nil || !c.Spawned || c.ParentID != agyConvA {
		t.Fatalf("subagent: %+v %v", c, err)
	}
	old := transcript.GetProjectsDir
	projects := t.TempDir()
	transcript.GetProjectsDir = func() string { return projects }
	t.Cleanup(func() { transcript.GetProjectsDir = old })
	convs, err := transcript.GetConversations(time.Time{}, 0, nil)
	if err != nil || len(convs) != 1 || convs[0].Kind != transcript.KindAgy {
		t.Fatalf("agy sessions listed with Claude's: %v %+v", err, convs)
	}
}

func TestAgyLiveSessionsFromProcessesAndLocks(t *testing.T) {
	agyFixture(t, agyConvA, "/w/flux", "Flux", "", "CASCADE_RUN_STATUS_IDLE")
	writeAgyTranscript(t, agyConvB, `{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T02:39:41Z","content":"hi"}`)
	oldPS, oldStart, oldLocks := proc.PsAll, proc.ProcessStartTimes, sessions.AgyPresenceLocks
	t.Cleanup(func() { proc.PsAll, proc.ProcessStartTimes, sessions.AgyPresenceLocks = oldPS, oldStart, oldLocks })
	proc.PsAll = func() (string, error) {
		return strings.Join([]string{
			"100 ttys001  /Users/u/.local/bin/agy",
			"101 ttys002  agy --conversation " + agyConvB,
			"102 ttys003  agy models",
			"103 ttys004  agy -p 'hello'",
			"104 ??       /Users/u/.local/bin/agy remote-control start",
			"105 ttys005  /usr/bin/vim agy",
		}, "\n"), nil
	}
	started := time.Now().Add(-time.Hour)
	proc.ProcessStartTimes = func(pids map[string]int) map[int]time.Time {
		out := map[int]time.Time{}
		for _, p := range pids {
			out[p] = started
		}
		return out
	}
	sessions.AgyPresenceLocks = func() map[int]string {
		return map[int]string{100: agyConvA}
	}
	resetAgyCaches()
	live := sessions.AgyLiveSessions()
	if len(live) != 2 || live[agyConvA].PID != 100 || live[agyConvB].PID != 101 || !live[agyConvA].Agy {
		t.Errorf("two TUIs matched, subcommands/print/non-tty ignored: %+v", live)
	}
}

func TestAgyClaimNudgeAndOwnedBy(t *testing.T) {
	live := claimEnv(t)
	delete(live, os.Getppid())
	launchPath := filepath.Join(state.LaunchDir(), "fix-agy-pr")
	urisA, _ := json.Marshal([]string{"file:///w/app", "file://" + launchPath})
	agyEnv(t, map[string]transcript.AgySummary{
		agyConvA: {ID: agyConvA, Title: "Auto Title", WorkspaceURIs: string(urisA)},
	})
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", agyConvA)
	msg, err := github.Claim(claimA, false, time.Now())
	if err != nil || !strings.Contains(msg, "fix-agy-pr owns") {
		t.Fatalf("agy claim: %q %v", msg, err)
	}
	c := github.LoadClaims().Claims[claimA][0]
	if c.Kind != transcript.KindAgy || c.Session != agyConvA || c.PID != 0 {
		t.Errorf("claim: %+v", c)
	}

	var gotID, gotText string
	oldDeliver := sessions.AgyDeliver
	sessions.AgyDeliver = func(id, text string) error { gotID, gotText = id, text; return nil }
	t.Cleanup(func() { sessions.AgyDeliver = oldDeliver })
	if err := github.SendNudge(c, "checks failing"); err != nil {
		t.Fatalf("SendNudge: %v", err)
	}
	if gotID != agyConvA || !strings.HasPrefix(gotText, sessions.AgyNudgeNote) {
		t.Errorf("nudge went to AgyDeliver: id=%q text=%q", gotID, gotText)
	}

	rep := github.PRsReport{
		PRs: []github.PRRow{
			{PR: "https://github.com/o/r/pull/1", OwnersDetail: []github.PROwner{{Name: "a", Kind: transcript.KindAgy, Session: agyConvA}}},
			{PR: "https://github.com/o/r/pull/2", OwnersDetail: []github.PROwner{{Name: "b", Kind: "claude", Session: agyConvA}}},
		},
	}
	got := rep.OwnedBy(transcript.KindAgy, agyConvA)
	if len(got.PRs) != 1 || got.PRs[0].PR != "https://github.com/o/r/pull/1" {
		t.Errorf("OwnedBy agy: %+v", got.PRs)
	}
}

func TestAgyRowsPreviewAndRefusals(t *testing.T) {
	conv := transcript.Conversation{
		SessionID:     agyConvA,
		Kind:          transcript.KindAgy,
		Title:         "flux",
		Cwd:           "/w/flux",
		ActiveModel:   "gemini-4-neon-medium",
		ContextTokens: 13500,
		Usage:         transcript.TokenUsage{Input: 3705, CacheRead: 22746, Output: 596},
		LastTimestamp: time.Now().UTC().Format(time.RFC3339),
		Messages: []transcript.Message{
			{Role: "user", Text: "hello"},
			{Role: "assistant", Text: "world"},
		},
	}
	m := initialModel(buildItems([]transcript.Conversation{conv, {SessionID: "c1", Title: "claude one", Messages: []transcript.Message{{Role: "user", Text: "y"}}}}), "", nil)
	m.width, m.height = 140, 30
	m.live = map[string]bool{agyConvA: true}
	v := strip2(m.View())
	if !strings.Contains(v, " ✦ flux") || !strings.Contains(v, " ✻ claude one") || !strings.Contains(v, "agy · ") {
		t.Errorf("row glyph and agy prefix:\n%s", v)
	}
	if !strings.Contains(v, "Agent:   ✦ agy") || !strings.Contains(v, "of 1.0M") {
		t.Errorf("preview agent and 1.0M window:\n%s", v)
	}
	if strings.Contains(v, "at API prices") {
		t.Errorf("agy preview should not guess Claude API price:\n%s", v)
	}
	if ansi.StringWidth("✦") != 1 {
		t.Errorf("✦ must be one cell wide")
	}
	for _, letter := range []rune{'d', 'e', 'x'} {
		m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlX})
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{letter}})
		if m.confirmDelete || m.renaming || m.confirmPrune || !strings.Contains(m.errorMsg, "agy") {
			t.Errorf("^X %c: %q", letter, m.errorMsg)
		}
	}
	if h := mcp.Header(conv); !strings.Contains(h, "kind agy") || !strings.Contains(h, "agy --conversation "+agyConvA) {
		t.Errorf("MCP header:\n%s", h)
	}
	if _, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "both", Brief: "hi", Codex: true, Agy: true}); err == nil {
		t.Error("codex and agy together should be rejected")
	}
	if !transcript.IsSpawn(`agy -p "check logs"`) || !transcript.IsSpawn(`agy --print "check logs"`) {
		t.Error("agy -p / --print should be recognized as spawn")
	}
}

func TestAgyHistoryFallbackErrorsAndDeliver(t *testing.T) {
	agyEnv(t, nil)
	writeAgyTranscript(t, agyConvA,
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T02:39:41Z","content":"hello"}`,
		`{"step_index":1,"source":"SYSTEM","type":"ERROR_MESSAGE","status":"DONE","created_at":"2026-11-22T02:39:42Z","error":"rate limited\nmore details"}`,
	)
	os.WriteFile(filepath.Join(transcript.AgyHome(), "history.jsonl"), []byte(`{"workspace":"/w/from-history","conversationId":"`+agyConvA+`"}`+"\n"), 0o600)
	c, err := transcript.ParseAgyFile(transcript.AgyTranscript(agyConvA), time.Time{}, 0)
	if err != nil || c == nil {
		t.Fatalf("parse: %v %v", c, err)
	}
	if c.Cwd != "/w/from-history" || c.LastError != "rate limited" {
		t.Errorf("history cwd and error: cwd=%q err=%q", c.Cwd, c.LastError)
	}
	if note, err := sessions.DeliverAgy(100, agyConvA, "flux", "hello", false); err == nil || !strings.Contains(note, "couldn't be verified") {
		t.Errorf("unverified pid refused: %q %v", note, err)
	}
}

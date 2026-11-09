package app

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// codexFixture is a rollout in the real format with invented content.
func codexFixture(t *testing.T, thread, cwd string, source any, extra ...string) string {
	t.Helper()
	src, _ := json.Marshal(source)
	roots := ""
	lines := []string{
		`{"timestamp":"2026-11-03T10:00:00.000Z","type":"session_meta","payload":{"id":"` + thread + `","timestamp":"2026-11-03T10:00:00.000Z","cwd":"` + cwd + `","originator":"codex-tui","source":` + string(src) + roots + `}}`,
		`{"timestamp":"2026-11-03T10:00:01.000Z","type":"turn_context","payload":{"model":"gpt-6.1-sol","cwd":"` + cwd + `"}}`,
		`{"timestamp":"2026-11-03T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<skills_instructions>x</skills_instructions>"}]}}`,
		`{"timestamp":"2026-11-03T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /w\n\nbe nice"},{"type":"input_text","text":"<environment_context>\n<cwd>/w</cwd>\n</environment_context>"}]}}`,
		`{"timestamp":"2026-11-03T10:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"open a pull request for the flux capacitor"}]}}`,
		`{"timestamp":"2026-11-03T10:00:02.500Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"timestamp":"2026-11-03T10:00:03.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/zsh","-lc","gh pr create --fill"],"aggregated_output":"https://github.com/o/r/pull/7\n"}}}`,
		`{"timestamp":"2026-11-03T10:00:03.500Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/zsh","-lc","gh search prs"],"aggregated_output":"https://github.com/o/r/pull/8\n"}}}`,
		`{"timestamp":"2026-11-03T10:00:04.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Opened the capacitor PR."}]}}`,
		`{"timestamp":"2026-11-03T10:00:04.500Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":900,"cached_input_tokens":600,"output_tokens":50},"last_token_usage":{"input_tokens":42000},"model_context_window":475000}}}`,
		`{"timestamp":"2026-11-03T10:00:05.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
	}
	lines = append(lines, extra...)
	dir := filepath.Join(transcript.CodexHome(), "sessions", "2026", "11", "03")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "rollout-2026-11-03T10-00-00-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(filepath.Join(transcript.CodexHome(), "sessions"))
		os.Remove(filepath.Join(transcript.CodexHome(), "session_index.jsonl"))
	})
	return path
}

const (
	cxA = "01a1a103-0221-75b2-86f5-7662d5bc71c7"
	cxB = "01a1a104-555b-7833-a5f9-ceb4180957c2"
)

func TestCodexRolloutParses(t *testing.T) {
	path := codexFixture(t, cxA, "/w/flux", "cli")
	os.WriteFile(filepath.Join(transcript.CodexHome(), "session_index.jsonl"), []byte(`{"id":"`+cxA+`","thread_name":"Old name"}`+"\n"+`{"id":"`+cxA+`","thread_name":"Flux PR"}`+"\n"), 0o644)
	c, err := transcript.ParseCodexFile(path, time.Time{}, 0)
	if err != nil || c == nil {
		t.Fatalf("parse: %v %v", c, err)
	}
	if c.Kind != transcript.KindCodex || c.SessionID != cxA || c.Cwd != "/w/flux" || c.ActiveModel != "gpt-6.1-sol" {
		t.Errorf("identity: %+v", c)
	}
	if len(c.Messages) != 2 || c.Messages[0].Text != "open a pull request for the flux capacitor" || c.Messages[1].Role != "assistant" {
		t.Errorf("only typed and replied text, harness blocks left out: %+v", c.Messages)
	}
	if c.ContextTokens != 42000 || c.WindowSize() != 475000 {
		t.Errorf("context from the last token count, window as stated: %d of %d", c.ContextTokens, c.WindowSize())
	}
	if !slices.Equal(c.PRs, []string{"https://github.com/o/r/pull/7"}) {
		t.Errorf("a PR gh pr create printed, not one a search mentioned: %v", c.PRs)
	}
	if c.Title != "Flux PR" || c.IsCustomTitle || c.Spawned || c.CodexBusy {
		t.Errorf("title from the index (latest wins), interactive, turn finished: %+v", c)
	}
	if !strings.Contains(c.SearchLower, "codex") {
		t.Error("searching codex finds every Codex session")
	}
	if st := codexStat(path); st.status != "idle" {
		t.Errorf("a finished turn is idle: %+v", st)
	}
	// A turn that's started and not finished is busy.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"timestamp":"2026-11-03T10:01:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}` + "\n")
	f.Close()
	if st := codexStat(path); st.status != "busy" {
		t.Errorf("an unfinished turn is busy: %+v", st)
	}
	if c, _ := transcript.ParseCodexFile(path, time.Time{}, 0); c == nil || !c.CodexBusy {
		t.Error("the parse sees the unfinished turn too")
	}
}

func TestCodexLaunchNameAndNesting(t *testing.T) {
	// ccs launch's folder among the workspace roots names the session.
	dir := filepath.Join(state.LaunchDir(), "flux-fixer")
	path := codexFixture(t, cxA, "/w", "cli")
	data, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(data), `"source":"cli"`, `"source":"cli","runtime_workspace_roots":["/w","`+dir+`"]`, 1)), 0o644)
	if c, _ := transcript.ParseCodexFile(path, time.Time{}, 0); c == nil || c.Title != "flux-fixer" || !c.IsCustomTitle {
		t.Errorf("a launched session goes by its launch name: %+v", c)
	}
	// A subagent nests under its parent thread; an exec run is spawned.
	sub := codexFixture(t, cxB, "/w", map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": cxA, "depth": 1}}})
	c, _ := transcript.ParseCodexFile(sub, time.Time{}, 0)
	if c == nil || !c.Spawned || c.ParentID != cxA {
		t.Fatalf("subagent: %+v", c)
	}
	var m model
	if p := m.spawnParent(*c); p != cxA {
		t.Errorf("a subagent's parent is its parent thread: %q", p)
	}
}

func TestCodexExecNestsUnderTheClaudeSessionThatRanIt(t *testing.T) {
	start := time.Now().Add(-time.Hour).UTC()
	child := transcript.Conversation{SessionID: cxA, Kind: transcript.KindCodex, Spawned: true, FirstTimestamp: start.Format(time.RFC3339),
		Messages: []transcript.Message{{Role: "user", Text: "Review the flux capacitor diff for safety problems please"}}}
	if !transcript.IsSpawn(`codex exec -s read-only "Review the flux capacitor diff for safety problems please"`) {
		t.Fatal("codex exec is a spawn")
	}
	parent := transcript.Conversation{SessionID: "claude1", Spawns: []transcript.SpawnCmd{{Ts: start.Add(-30 * time.Second).Format(time.RFC3339), Text: transcript.SpawnText(`codex exec -s read-only "Review the flux capacitor diff for safety problems please"`)}}}
	if p := findSpawnParent(child, buildItems([]transcript.Conversation{parent, child})); p != "claude1" {
		t.Errorf("an exec run nests under the session whose Bash ran it: %q", p)
	}
}

func TestCodexSessionsInTheScan(t *testing.T) {
	codexFixture(t, cxA, "/w", "cli")
	old := transcript.GetProjectsDir
	projects := t.TempDir()
	transcript.GetProjectsDir = func() string { return projects }
	defer func() { transcript.GetProjectsDir = old }()
	convs, err := transcript.GetConversations(time.Time{}, 0, nil)
	if err != nil || len(convs) != 1 || convs[0].Kind != transcript.KindCodex {
		t.Fatalf("Codex sessions are listed with Claude's: %v %+v", err, convs)
	}
}

func TestCodexLiveSessionsFromProcesses(t *testing.T) {
	path := codexFixture(t, cxA, "/w/flux", "cli")
	codexFixture(t, cxB, "/w/other", "cli")
	oldPS, oldStart, oldCwd := proc.PsAll, proc.ProcessStartTimes, transcript.CodexProcCwd
	defer func() { proc.PsAll, proc.ProcessStartTimes, transcript.CodexProcCwd = oldPS, oldStart, oldCwd }()
	proc.PsAll = func() (string, error) {
		return strings.Join([]string{
			"100 ttys001  /opt/homebrew/bin/codex",                             // a TUI: its thread from its folder
			"101 ttys002  codex resume " + cxB,                                 // a resume names its thread
			"102 ??       /x/codex app-server --listen unix://",                // the daemon
			"103 ttys003  codex exec 'one-off'",                                // headless
			"104 ttys004  /usr/bin/vim codex",                                  // not codex
			"105 ttys005  /opt/homebrew/bin/codex -s workspace-write -a never", // a TUI with no turn yet in its folder
		}, "\n"), nil
	}
	started := time.Now().Add(-time.Hour)
	os.Chtimes(path, time.Now(), time.Now())
	proc.ProcessStartTimes = func(pids map[string]int) map[int]time.Time {
		out := map[int]time.Time{}
		for _, p := range pids {
			out[p] = started
		}
		return out
	}
	transcript.CodexProcCwd = func(pid int) string {
		if pid == 100 {
			return "/w/flux"
		}
		return "/w/empty"
	}
	codexLiveCache.Lock()
	codexLiveCache.out = nil
	codexLiveCache.Unlock()
	live := codexLiveSessions()
	if len(live) != 2 || live[cxA].pid != 100 || live[cxB].pid != 101 || !live[cxA].codex {
		t.Errorf("two TUIs on two threads, servers and exec runs left out: %+v", live)
	}
	if live[cxA].stat.status != "idle" {
		t.Errorf("status from its rollout: %+v", live[cxA].stat)
	}
}

func TestCodexRowsAndPreview(t *testing.T) {
	conv := transcript.Conversation{SessionID: cxA, Kind: transcript.KindCodex, Title: "flux", Cwd: "/w/flux", ActiveModel: "gpt-6.1-sol", ContextTokens: 1000, Window: 475000,
		LastTimestamp: time.Now().UTC().Format(time.RFC3339), Messages: []transcript.Message{{Role: "user", Text: "x"}}}
	m := initialModel(buildItems([]transcript.Conversation{conv, {SessionID: "c1", Title: "claude one", Messages: []transcript.Message{{Role: "user", Text: "y"}}}}), "", nil)
	m.width, m.height = 140, 30
	m.live = map[string]bool{cxA: true}
	v := strip2(m.View())
	if !strings.Contains(v, " ⬡ flux") || !strings.Contains(v, " ✻ claude one") || !strings.Contains(v, "codex · ") {
		t.Errorf("each row carries its agent's glyph, and a Codex row says codex:\n%s", v)
	}
	if !strings.Contains(v, "Agent:   ⬡ codex") || !strings.Contains(v, "of 475k") {
		t.Errorf("the preview names the agent and the window it states:\n%s", v)
	}
	for _, g := range []string{"✻", "⬡"} {
		if ansi.StringWidth(g) != 1 {
			t.Errorf("%s must be one cell wide", g)
		}
	}
	// The table's rows stay aligned whatever the agent.
	m.table = true
	a, b := m.formatListItem(m.filtered[0], true), m.formatListItem(m.filtered[1], true)
	if ansi.StringWidth(a) != ansi.StringWidth(b) {
		t.Errorf("rows differ in width: %d and %d", ansi.StringWidth(a), ansi.StringWidth(b))
	}
}

func TestCodexSessionsRefuseRenameDeletePrune(t *testing.T) {
	conv := transcript.Conversation{SessionID: cxA, Kind: transcript.KindCodex, FilePath: "/nowhere", Messages: []transcript.Message{{Role: "user", Text: "x"}}}
	for _, k := range []tea.KeyMsg{{Type: tea.KeyCtrlD}} {
		m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
		m, _ = key(m, k)
		if m.confirmDelete || !strings.Contains(m.errorMsg, "Codex") {
			t.Errorf("delete: %q", m.errorMsg)
		}
	}
	for _, letter := range []rune{'e', 'x'} {
		m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlX})
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{letter}})
		if m.renaming || m.confirmPrune || !strings.Contains(m.errorMsg, "Codex") {
			t.Errorf("^X %c: %q", letter, m.errorMsg)
		}
	}
}

func TestCodexResumeAndSend(t *testing.T) {
	conv := transcript.Conversation{SessionID: cxA, Kind: transcript.KindCodex}
	args := strings.Join(transcript.CodexResumeArgs("codex", conv, false), " ")
	if !strings.HasPrefix(args, "codex resume "+cxA+" -s workspace-write -a on-request -c sandbox_workspace_write.network_access=true --add-dir ") {
		t.Errorf("resume with ccs launch's sandbox: %s", args)
	}
	if args := transcript.CodexResumeArgs("codex", conv, true); args[1] != "fork" {
		t.Errorf("a fork: %v", args)
	}
	var got []string
	old := codexQueue
	codexQueue = func(thread, text string) error { got = []string{thread, text}; return nil }
	defer func() { codexQueue = old }()
	note, err := deliverCodex(cxA, "flux", "carry on")
	if err != nil || !slices.Equal(got, []string{cxA, "carry on"}) || !strings.Contains(note, "codex queue") {
		t.Errorf("the message box queues into the thread: %v %q %v", got, note, err)
	}
	if h := mcpHeader(transcript.Conversation{SessionID: cxA, Kind: transcript.KindCodex, Cwd: "/w"}); !strings.Contains(h, "kind codex") || !strings.Contains(h, "codex resume "+cxA) {
		t.Errorf("MCP says it's Codex and how to resume it:\n%s", h)
	}
}

func TestCodexUsageIsPricedPerRequest(t *testing.T) {
	tc := func(in, cached, write, out, reasoning int) string {
		return fmt.Sprintf(`{"timestamp":"2026-11-03T10:00:06.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d},"model_context_window":475000}}}`, in, cached, write, out, reasoning)
	}
	// The fixture's own event: 42000 input, nothing cached, no output.
	path := codexFixture(t, cxA, "/w", "cli",
		tc(100_000, 80_000, 10_000, 2_000, 500), // standard tier
		tc(300_000, 200_000, 0, 1_000, 0),       // over 272K: the long-context tier
		`{"timestamp":"2026-11-03T10:00:07.000Z","type":"turn_context","payload":{"model":"gpt-unknown","cwd":"/w"}}`,
		tc(1_000, 0, 0, 10, 0), // a model with no price: tokens, no cost
	)
	c, err := transcript.ParseCodexFile(path, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := (42_000*2.0)/1e6 + // fixture: fresh input at $2
		(10_000*2.0+80_000*0.10+10_000*2.50+2_000*10.0)/1e6 + // standard
		(100_000*4.0+200_000*0.20+1_000*15.0)/1e6 // long context
	if math.Abs(c.Cost-want) > 1e-9 || !c.Unpriced {
		t.Errorf("cost %.6f, want %.6f; unpriced %v", c.Cost, want, c.Unpriced)
	}
	u := c.Usage
	if u.Input != 42_000+10_000+100_000+1_000 || u.CacheRead != 280_000 || u.Cache5m != 10_000 || u.Output != 3_010 || c.Reasoning != 500 {
		t.Errorf("usage summed over requests: %+v reasoning %d", u, c.Reasoning)
	}
	line := strings.Join(sessionStats(*c), "\n")
	if !strings.Contains(strip2(line), "out 3k (reasoning 500)") || !strings.Contains(line, "some usage on a model with no price") {
		t.Errorf("Tokens line: %q", strip2(line))
	}
	c.Cost, c.ActiveModel = 0, "gpt-unknown"
	if line := strip2(strings.Join(sessionStats(*c), "\n")); !strings.Contains(line, "no price for gpt-unknown") || strings.Contains(line, "$") {
		t.Errorf("no cost guessed: %q", line)
	}
}

func TestCodexToolCallsInPreview(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-11-03T10-00-00-tools.jsonl")
	lines := []string{
		`{"timestamp":"2026-11-03T10:00:00.000Z","type":"session_meta","payload":{"id":"tools","cwd":"/code/x","source":"cli"}}`,
		`{"timestamp":"2026-11-03T10:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}}`,
		`{"timestamp":"2026-11-03T10:00:02.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["/bin/zsh","-lc","pwd\n  git status"],"aggregated_output":"/code/x\n"}}}`,
		`{"timestamp":"2026-11-03T10:00:03.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"McpToolCall","server":"ccs","tool":"list_prs","arguments":{"query":"mine"}}}}`,
		`{"timestamp":"2026-11-03T10:00:04.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","changes":{"/code/x/a.go":{"type":"add","content":"secret body"}}}}}`,
		`{"timestamp":"2026-11-03T10:00:05.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"Extension","kind":"web.search","query":"go wrap"}}}`,
		`{"timestamp":"2026-11-03T10:00:06.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning"}}}`,
		`{"timestamp":"2026-11-03T10:00:07.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		`{"timestamp":"2026-11-03T10:00:08.000Z","type":"event_msg","payload":{"type":"task_complete","duration_ms":48010}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conv, err := transcript.ParseCodexFile(path, time.Time{}, 0)
	if err != nil || conv == nil {
		t.Fatalf("parse: %v %v", conv, err)
	}
	if len(conv.Messages) != 2 {
		t.Errorf("tool calls must stay out of Messages: %+v", conv.Messages)
	}
	plain := strip2(strings.Join(buildPreviewLines(*conv, "", 80), "\n"))
	for _, want := range []string{"❯ go", "● Shell(pwd\n    git status)", "● ccs - list_prs (MCP)(mine)", "● Edit(/code/x/a.go)", "● WebSearch(go wrap)", "● done", "  ⬡ 48s · " + transcript.FormatTimestamp("2026-11-03T10:00:08Z")[11:]} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "secret body") || strings.Contains(plain, "Reasoning") {
		t.Errorf("only the path of a file change, and no non-tool items:\n%s", plain)
	}
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackgroundTasks(t *testing.T) {
	var conv Conversation
	ts := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	use := func(id, name, input string) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`, ts, id, name, input)
	}
	result := func(id, text string) string {
		return fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`, id, text)
	}
	for _, l := range []string{
		use("b1", "Bash", `{"command":"sleep 99","description":"Watch PR 139","run_in_background":true}`),
		result("b1", "Command running in background with ID: bx1. Output is being written to: /tmp/bx1.output"),
		use("b2", "Bash", `{"command":"grep x log"}`), // only its output mentions it
		result("b2", "found: Command running in background with ID: bx9"),
		use("a1", "Agent", `{"description":"An async agent","prompt":"…"}`), // a subagent: counted from its meta.json, not as a task
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a1","content":[{"type":"text","text":"Async agent launched. agentId: x"}]}]}}`,
		use("b3", "Bash", `{"command":"sleep 99","description":"Review the diff","run_in_background":true}`),
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b3","content":[{"type":"text","text":"Command running in background with ID: bx3."}]}]}}`,
		use("m1", "Monitor", `{"description":"Tail the deploy"}`),
		result("m1", "Monitor started (task m, expires in 1m unless the source ends first)"),
		`{"type":"user","message":{"content":"<task-notification>\n<task-id>bx1</task-id>\n<tool-use-id>b1</tool-use-id>\n<status>completed</status>\n</task-notification>"}}`,
	} {
		if !parseLine(&conv, []byte(l)) {
			t.Fatalf("not parsed: %s", l)
		}
	}
	m := initialModel(nil, "", nil)
	if len(m.runningTasks(conv)) != 0 {
		t.Error("a session not open in claude has no running tasks")
	}
	conv.SessionID = "s"
	m.live = map[string]bool{"s": true}
	m.lastScan = map[string]liveSession{"s": {start: time.Now().Add(-time.Hour).UTC().Format(time.ANSIC)}}
	var running []string
	for _, task := range m.runningTasks(conv) {
		running = append(running, task.kind+":"+task.desc)
	}
	// b1 completed, b2 was never a task, a1 is a subagent, m1's minute is up: only b3 runs.
	if strings.Join(running, ",") != "bash:Review the diff" {
		t.Errorf("running = %v", running)
	}
	if got := m.taskCell(conv, false); got != "⧗1" {
		t.Errorf("list count = %q", got)
	}
	lines := strip2(strings.Join(m.taskLines(conv, 100), "\n"))
	for _, want := range []string{"Tasks:", "⧗ Review the diff · bash · 5m", "1 completed", "1 ended with the session"} {
		if !strings.Contains(lines, want) {
			t.Errorf("preview should show %q:\n%s", want, lines)
		}
	}
	// A task from before the session's current claude process (say it was
	// resumed) died with the old one.
	m.lastScan = map[string]liveSession{"s": {start: time.Now().UTC().Format(time.ANSIC)}}
	if len(m.runningTasks(conv)) != 0 {
		t.Error("tasks started before the current process aren't running")
	}
}

func TestTaskCountOnTheTitleRow(t *testing.T) {
	conv := Conversation{SessionID: "s", Title: "work", PRs: []string{"https://github.com/o/r/pull/1"},
		tasks: []bgTask{{id: "b1", kind: "bash", desc: "watch", started: time.Now().UTC().Format(time.RFC3339)}}}
	m := initialModel(buildItems([]Conversation{conv}), "", nil)
	m.live = map[string]bool{"s": true}
	m.prs = map[string]prInfo{"https://github.com/o/r/pull/1": {State: "OPEN", MergeState: "CLEAN"}}
	row := m.sidebarRow(m.filtered[0], false, 40)
	if !strings.Contains(row[0], "\033["+taskColour+"m⧗1") {
		t.Errorf("the task count goes on the title row, in its own colour: %q", row[0])
	}
	if strings.Contains(row[1], "⧗") || !strings.Contains(strip2(row[1]), "✓1") {
		t.Errorf("the grey row keeps the PRs to itself: %q", strip2(row[1]))
	}
}

func TestTasksBeforeHits(t *testing.T) {
	conv := Conversation{SessionID: "s", Title: "work", Messages: []Message{{Role: "user", Text: "needle"}},
		tasks: []bgTask{{id: "b1", kind: "bash", desc: "watch", started: time.Now().UTC().Format(time.RFC3339)}}}
	m := initialModel(buildItems([]Conversation{conv}), "needle", nil)
	m.live = map[string]bool{"s": true}
	if row := strip2(m.sidebarRow(m.filtered[0], false, 40)[0]); !strings.HasSuffix(strings.TrimRight(row, " "), "⧗1 1") {
		t.Errorf("tasks first, then the hits at the edge: %q", row)
	}
}

func TestSubagentsInThePreviewAndTheList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o600)
	subs := filepath.Join(dir, "s1", "subagents")
	os.MkdirAll(subs, 0o755)
	started := time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339)
	for name, finished := range map[string]bool{"otel-bumps": false, "filelock-floor": false, "aries-mlflow": true} {
		os.WriteFile(filepath.Join(subs, "agent-"+name+".meta.json"), []byte(fmt.Sprintf(`{"name":%q,"finished":%v}`, name, finished)), 0o600)
		os.WriteFile(filepath.Join(subs, "agent-"+name+".jsonl"), []byte(fmt.Sprintf(`{"timestamp":%q}`+"\n", started)), 0o600)
	}
	conv := Conversation{SessionID: "s1", FilePath: path,
		tasks: []bgTask{{id: "b1", kind: "bash", desc: "watch", started: time.Now().UTC().Format(time.RFC3339)}}}
	m := initialModel(buildItems([]Conversation{conv}), "", nil)
	m.live = map[string]bool{"s1": true}
	m.lastScan = map[string]liveSession{"s1": {start: time.Now().Add(-time.Hour).UTC().Format(time.ANSIC)}}
	if got := m.taskCell(conv, false); got != "⧗1 ◆2" {
		t.Errorf("background tasks and subagents are counted apart: %q", got)
	}
	lines := strip2(strings.Join(m.subagentLines(conv, 100), "\n"))
	for _, want := range []string{"Subagents:", "◆ otel-bumps · 3m", "◆ filelock-floor · 3m", "1 finished"} {
		if !strings.Contains(lines, want) {
			t.Errorf("preview should show %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "aries-mlflow") {
		t.Errorf("a finished subagent is only counted:\n%s", lines)
	}
}

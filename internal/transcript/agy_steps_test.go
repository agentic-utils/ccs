package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAgy writes lines as agy conversation id's transcript under a fresh
// AgyHome, with mod as its time, and returns its path.
func writeAgy(t *testing.T, id string, mod time.Time, lines ...string) string {
	t.Helper()
	old := AgyHome
	home := t.TempDir()
	AgyHome = func() string { return home }
	t.Cleanup(func() { AgyHome = old })
	p := filepath.Join(home, "brain", id, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

// The shape agy 1.5.18 writes: a planner step with thinking and tool calls,
// one result record per call in order (a failed read, a command exiting 1),
// then the final reply, written as the turn ends.
var agyTurn = []string{
	`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"2026-03-02T10:00:00Z","content":"<USER_REQUEST>\ntidy the flowerbeds\n</USER_REQUEST>"}`,
	`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-03-02T10:00:00Z","model":"gemini-x","output_tokens":120,"thinking":"**Planning the Weeding**\n\nlots of thought","tool_calls":[{"name":"run_command","args":{"CommandLine":"\"ls beds\"","toolSummary":"\"list\""}},{"name":"read_file","args":{"Path":"\"/garden/roses.txt\""}},{"name":"run_command","args":{"CommandLine":"\"ls weeds\""}}]}`,
	`{"step_index":2,"type":"GENERIC","status":"DONE","created_at":"2026-03-02T10:00:09Z","content":"Command exited with code 0.\n\nCommand output:\nbeds"}`,
	`{"step_index":3,"type":"GENERIC","status":"ERROR","created_at":"2026-03-02T10:00:09Z","error":"file does not exist","content":"Encountered error"}`,
	`{"step_index":4,"type":"GENERIC","status":"DONE","created_at":"2026-03-02T10:00:10Z","content":"Command exited with code 1.\n\nCommand output:\nls: weeds: No such file"}`,
	`{"step_index":5,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-03-02T10:00:11Z","output_tokens":30,"thinking":"**Writing It Up**","tool_calls":[{"name":"edit_file","args":{"Path":"\"/garden/roses.txt\"","Replacement":"\"secret new text\"","Target":"\"old\""}}]}`,
	`{"step_index":6,"type":"GENERIC","status":"DONE","created_at":"2026-03-02T10:00:20Z","content":"Made 1 replacement"}`,
	`{"step_index":7,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-03-02T10:00:21Z","output_tokens":10,"thinking":"**Summing Up**","content":"done"}`,
}

func TestAgyToolsThinkingAndTurnEnd(t *testing.T) {
	id := "0e5a1b2c-0000-4000-8000-0000000000b1"
	end := time.Date(2026, 3, 2, 10, 0, 31, 0, time.UTC)
	p := writeAgy(t, id, end, agyTurn...)
	conv, err := ParseAgyFile(p, time.Time{}, 0)
	if err != nil || conv == nil {
		t.Fatalf("parse: %v %v", conv, err)
	}
	type want struct {
		name, arg string
		failed    bool
	}
	wants := []want{
		{"Thought", "9s · Planning the Weeding", false},
		{"Bash", "ls beds", false},
		{"Read", "/garden/roses.txt", true},
		{"Bash", "ls weeds", true},
		{"Thought", "9s · Writing It Up", false},
		{"Edit", "/garden/roses.txt", false},
		{"Thought", "10s · Summing Up", false}, // its step ends with the turn: the file's time
	}
	var tools []ToolUse
	var ends []ToolUse
	for _, tu := range conv.Tools {
		if tu.Took > 0 {
			ends = append(ends, tu)
		} else {
			tools = append(tools, tu)
		}
	}
	if len(tools) != len(wants) {
		t.Fatalf("tools: got %+v", tools)
	}
	for i, w := range wants {
		if g := tools[i]; g.Name != w.name || g.Arg != w.arg || g.Failed != w.failed {
			t.Errorf("tool %d: got %q %q failed=%v, want %+v", i, g.Name, g.Arg, g.Failed, w)
		}
	}
	for _, tu := range tools {
		if strings.Contains(tu.Arg, "secret") || strings.Contains(tu.Arg, "lots of thought") {
			t.Errorf("an edit's strings or the thinking's body reached the preview: %q", tu.Arg)
		}
	}
	if len(ends) != 1 || ends[0].Took != 31*time.Second || ends[0].At != len(conv.Messages) {
		t.Fatalf("turn end: got %+v (messages %d)", ends, len(conv.Messages))
	}
	if n := conv.OutputSince(time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)); n != 160 {
		t.Errorf("output tokens this turn: got %d, want 160", n)
	}
}

// An earlier turn ends where its last reply began (nothing records when that
// reply was done), and that reply's thinking gets no duration: the next
// prompt came whenever it was typed. A turn still running has no end.
func TestAgyEarlierTurnAndRunningTurn(t *testing.T) {
	id := "0e5a1b2c-0000-4000-8000-0000000000b2"
	lines := append(append([]string{}, agyTurn...),
		`{"step_index":8,"type":"USER_INPUT","status":"DONE","created_at":"2026-03-02T11:00:00Z","content":"<USER_REQUEST>\nand the lawn\n</USER_REQUEST>"}`,
		`{"step_index":9,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-03-02T11:00:00Z","tool_calls":[{"name":"run_command","args":{"CommandLine":"\"mow\""}}]}`,
	)
	p := writeAgy(t, id, time.Date(2026, 3, 2, 11, 0, 5, 0, time.UTC), lines...)
	conv, err := ParseAgyFile(p, time.Time{}, 0)
	if err != nil || conv == nil {
		t.Fatalf("parse: %v %v", conv, err)
	}
	var ends []time.Duration
	var last ToolUse
	for _, tu := range conv.Tools {
		if tu.Took > 0 {
			ends = append(ends, tu.Took)
		}
		if tu.Name == "Thought" {
			last = tu
		}
	}
	if len(ends) != 1 || ends[0] != 21*time.Second {
		t.Errorf("turn ends: got %v, want just the first, 21s", ends)
	}
	if last.Arg != "Summing Up" {
		t.Errorf("the earlier turn's last thinking: got %q, want no duration", last.Arg)
	}
	if lastTool := conv.Tools[len(conv.Tools)-1]; lastTool.Name != "Bash" || lastTool.Arg != "mow" || lastTool.Failed {
		t.Errorf("running call: got %+v", lastTool)
	}
}

func TestAgyRecordShapes(t *testing.T) {
	if !AgyFinal(AgyRecord{Type: "PLANNER_RESPONSE", Content: "x"}) || AgyFinal(AgyRecord{Type: "GENERIC"}) {
		t.Error("AgyFinal")
	}
	ask := AgyRecord{Type: "PLANNER_RESPONSE"}
	ask.ToolCalls = append(ask.ToolCalls, struct {
		Name string            `json:"name"`
		Args map[string]string `json:"args"`
	}{Name: "ask_question"})
	if !AgyAsking(ask) || AgyFinal(ask) || AgyAsking(AgyRecord{Type: "PLANNER_RESPONSE"}) {
		t.Error("AgyAsking")
	}
	if agyFailed(AgyRecord{Content: "Command exited with code 0."}) || !agyFailed(AgyRecord{Content: "x\nCommand exited with code 2.\n"}) || !agyFailed(AgyRecord{Status: "ERROR"}) {
		t.Error("agyFailed")
	}
}

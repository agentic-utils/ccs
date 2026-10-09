package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func mcpConvs() []Conversation {
	return []Conversation{
		{SessionID: "aaaa-1111", Cwd: "/code/payments", Title: "Fix flaky checkout test", LastTimestamp: "2026-10-01T10:00:00Z",
			Messages: []Message{
				{Role: "user", Text: "the checkout test is flaky", Ts: "2026-10-01T09:00:00Z"},
				{Role: "assistant", Text: "It depends on dict order; flaky because of a set.", Ts: "2026-10-01T09:01:00Z"},
				{Role: "user", Text: "<task-notification>\n<summary>CI passed</summary>\n</task-notification>", Ts: "2026-10-01T09:02:00Z"},
			}},
		{SessionID: "aaab-2222", Cwd: "/code/infra", Title: "Staging deploy", LastTimestamp: "2026-10-02T10:00:00Z",
			Messages: []Message{{Role: "user", Text: "staging deploy timed out", Ts: "2026-10-02T09:00:00Z"}}},
	}
}

func mcpRoundTrip(t *testing.T, reqs ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := runMCP(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out, func() ([]Conversation, error) { return mcpConvs(), nil }); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("bad response %q: %v", l, err)
		}
		resps = append(resps, r)
	}
	return resps
}

func toolText(t *testing.T, r map[string]any) string {
	t.Helper()
	res, _ := r["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in %v", r)
	}
	return content[0].(map[string]any)["text"].(string)
}

func TestMCPHandshakeAndTools(t *testing.T) {
	resps := mcpRoundTrip(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
	)
	if len(resps) != 3 {
		t.Fatalf("notifications get no reply; want 3 responses, got %d", len(resps))
	}
	if v := resps[0]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Errorf("should answer in the client's older protocol version, got %v", v)
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 {
		t.Errorf("want 3 tools, got %d", len(tools))
	}
	if resps[2]["error"] == nil {
		t.Error("unknown method should be an error")
	}
}

func TestMCPSearchListRead(t *testing.T) {
	resps := mcpRoundTrip(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"FLAKY dict"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"flaky","project":"infra"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_session","arguments":{"session_id":"aaaa"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"read_session","arguments":{"session_id":"aaa"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"read_session","arguments":{"session_id":"aaaa-1111","offset":-1}}}`,
	)
	search := toolText(t, resps[0])
	if !strings.Contains(search, "1 sessions match") || !strings.Contains(search, "Fix flaky checkout test") ||
		!strings.Contains(search, "claude --resume aaaa-1111") || !strings.Contains(search, "2 matching messages") {
		t.Errorf("search:\n%s", search)
	}
	if s := toolText(t, resps[1]); !strings.Contains(s, "No sessions") {
		t.Errorf("project filter should exclude payments:\n%s", s)
	}
	if s := toolText(t, mcpRoundTrip(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"staging deploy"}}}`)[0]); !strings.Contains(s, "1. Staging deploy") {
		t.Errorf("a name match should rank first:\n%s", s)
	}
	list := toolText(t, resps[2])
	if strings.Index(list, "Staging deploy") > strings.Index(list, "Fix flaky") {
		t.Errorf("list should be newest first:\n%s", list)
	}
	read := toolText(t, resps[3])
	if !strings.Contains(read, "User:\nthe checkout test is flaky") || !strings.Contains(read, "Note:\ntask-notification · CI passed") {
		t.Errorf("read:\n%s", read)
	}
	if r := resps[4]["result"].(map[string]any); r["isError"] != true {
		t.Errorf("an ambiguous prefix should be a tool error: %v", r)
	}
	if last := toolText(t, resps[5]); !strings.Contains(last, "messages 3-3 of 3") {
		t.Errorf("negative offset should count from the end:\n%s", last)
	}
}

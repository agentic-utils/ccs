package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// A question only about the running sessions reads only theirs; the rest
// still read everything. Every ccs mcp server once kept all of them
// (~620 MB each), and a burst of triage calls froze the machine.
func TestLiveQuestionsLoadOnlyLiveSessions(t *testing.T) {
	oldLive, oldLoadLive := liveSessions, LoadLive
	t.Cleanup(func() { liveSessions, LoadLive = oldLive, oldLoadLive })
	liveSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{"run-id": {PID: 1, Stat: sessions.SessionStat{Name: "runner", Status: "idle"}}}
	}
	all := []transcript.Conversation{
		{SessionID: "run-id", Title: "runner", LastTimestamp: "2026-11-18T10:00:00Z", Cwd: "/r"},
		{SessionID: "old-id", Title: "old", LastTimestamp: "2026-11-01T10:00:00Z", Cwd: "/r"},
	}
	var fullLoads, liveLoads int
	load := func() ([]transcript.Conversation, error) { fullLoads++; return all, nil }
	LoadLive = func() ([]transcript.Conversation, error) { liveLoads++; return all[:1], nil }

	for _, call := range []struct{ tool, args string }{{"session_triage", `{}`}, {"list_sessions", `{"live_only":true}`}} {
		if _, err := Call(call.tool, json.RawMessage(call.args), load); err != nil {
			t.Fatalf("%s: %v", call.tool, err)
		}
	}
	var out, errs bytes.Buffer
	RunSessions([]string{"--json"}, &out, &errs, load)
	if liveLoads != 3 || fullLoads != 0 {
		t.Errorf("triage, live_only and ccs sessions read only the running sessions: live %d, full %d", liveLoads, fullLoads)
	}
	Call("list_sessions", nil, load)
	Call("search_sessions", json.RawMessage(`{"query":"old"}`), load)
	RunSessions([]string{"--all", "--json"}, &out, &errs, load)
	if fullLoads != 3 {
		t.Errorf("list_sessions, search_sessions and --all still read every session: %d", fullLoads)
	}
}

// With LoadLive off (tests, and anything that didn't ask), load is used.
func TestLiveOrFallsBack(t *testing.T) {
	old := LoadLive
	t.Cleanup(func() { LoadLive = old })
	LoadLive = nil
	n := 0
	liveOr(func() ([]transcript.Conversation, error) { n++; return nil, nil })()
	if n != 1 {
		t.Error("no LoadLive: the given load")
	}
}

// The server runs AfterCall after each tool call, never for the handshake.
func TestAfterCallRunsAfterToolCalls(t *testing.T) {
	old := AfterCall
	t.Cleanup(func() { AfterCall = old })
	n := 0
	AfterCall = func() { n++ }
	reqs := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"x"}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := RunMCP(strings.NewReader(reqs), &out, func() ([]transcript.Conversation, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("AfterCall once per tools/call: %d", n)
	}
}

// A running session whose parent is closed (so not loaded) keeps the name
// its record gives.
func TestParentOfALiveSessionUnderAClosedOne(t *testing.T) {
	os.Remove(sessions.ParentsPath())
	t.Cleanup(func() { os.Remove(sessions.ParentsPath()) })
	sessions.SetParents([]sessions.ParentRec{{Child: "runner", Parent: "closed-orchestrator", ParentID: "gone-id"}})
	got := sessions.ParentNamesLoaded([]transcript.Conversation{{SessionID: "run-id", Title: "runner"}})
	if got["run-id"] != "closed-orchestrator" {
		t.Errorf("the record's parent name: %v", got)
	}
}

// GetConversationsFor parses only the named sessions; DropParseCache empties
// the cache.
func TestGetConversationsForAndDrop(t *testing.T) {
	old := transcript.GetProjectsDir
	dir := t.TempDir()
	transcript.GetProjectsDir = func() string { return dir }
	t.Cleanup(func() { transcript.GetProjectsDir = old })
	os.MkdirAll(filepath.Join(dir, "p"), 0o755)
	line := `{"type":"user","timestamp":"2026-11-18T10:00:00Z","sessionId":"%s","cwd":"/r","message":{"role":"user","content":"hello there"}}` + "\n"
	for _, id := range []string{"aaaa1111-0000-0000-0000-000000000001", "bbbb2222-0000-0000-0000-000000000002"} {
		os.WriteFile(filepath.Join(dir, "p", id+".jsonl"), []byte(strings.ReplaceAll(line, "%s", id)), 0o644)
	}
	got := transcript.GetConversationsFor([]string{"aaaa1111-0000-0000-0000-000000000001", "not-there"})
	if len(got) != 1 || got[0].SessionID != "aaaa1111-0000-0000-0000-000000000001" {
		t.Fatalf("only the asked session: %+v", got)
	}
	transcript.DropParseCache()
	transcript.ParseCacheMu.Lock()
	n := len(transcript.ParseCache)
	transcript.ParseCacheMu.Unlock()
	if n != 0 {
		t.Errorf("cache dropped: %d left", n)
	}
}

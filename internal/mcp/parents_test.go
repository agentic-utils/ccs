package mcp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestMCPParents(t *testing.T) {
	os.Remove(sessions.ParentsPath())
	t.Cleanup(func() { os.Remove(sessions.ParentsPath()) })
	oldLive, oldCaller := liveSessions, sessions.CallingSession
	t.Cleanup(func() { liveSessions, sessions.CallingSession = oldLive, oldCaller })
	live := map[string]sessions.LiveSession{
		"hub-id":    {PID: 1, Stat: sessions.SessionStat{Name: "hub", Status: "idle"}},
		"worker-id": {PID: 2, Stat: sessions.SessionStat{Name: "worker", Status: "idle"}},
	}
	liveSessions = func() map[string]sessions.LiveSession { return live }
	convs := []transcript.Conversation{
		{SessionID: "hub-id", Title: "hub", LastTimestamp: "2026-11-17T10:00:00Z", Cwd: "/r"},
		{SessionID: "worker-id", Title: "worker", LastTimestamp: "2026-11-17T10:00:00Z", Cwd: "/r"},
		{SessionID: "other-id", Title: "other", LastTimestamp: "2026-11-17T09:00:00Z", Cwd: "/r"},
	}
	load := func() ([]transcript.Conversation, error) { return convs, nil }

	sessions.CallingSession = func() (sessions.Caller, bool) { return sessions.Caller{ID: "hub-id", Name: "hub"}, true }
	if text, err := Call("adopt_session", json.RawMessage(`{"name":"worker"}`), load); err != nil || !strings.Contains(text, "worker is under hub") {
		t.Fatalf("adopt_session: %q %v", text, err)
	}

	text, err := Call("list_sessions", nil, load)
	if err != nil || !strings.Contains(text, "parent: hub") || strings.Count(text, "parent:") != 1 {
		t.Errorf("list_sessions gives the parent: %v\n%s", err, text)
	}
	text, _ = Call("list_sessions", json.RawMessage(`{"parent":"hub"}`), load)
	if !strings.HasPrefix(text, "1 sessions") || !strings.Contains(text, "worker") {
		t.Errorf("parent filter:\n%s", text)
	}

	text, _ = Call("session_triage", nil, load)
	var tri struct{ Sessions []TriageRow }
	json.Unmarshal([]byte(text), &tri)
	got := map[string]string{}
	for _, r := range tri.Sessions {
		got[r.Name] = r.Parent
	}
	if got["worker"] != "hub" || got["hub"] != "" || len(got) != 2 {
		t.Errorf("triage rows carry the parent: %v", got)
	}
	text, _ = Call("session_triage", json.RawMessage(`{"parent":"hub"}`), load)
	tri.Sessions = nil
	json.Unmarshal([]byte(text), &tri)
	if len(tri.Sessions) != 1 || tri.Sessions[0].Name != "worker" {
		t.Errorf("triage parent filter: %s", text)
	}

	if text, err := Call("adopt_session", json.RawMessage(`{"name":"worker","parent":"none"}`), load); err != nil || !strings.Contains(text, "no parent now") {
		t.Errorf("detach: %q %v", text, err)
	}
}

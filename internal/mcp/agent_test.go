package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// launch_session's agent: claude by default, the older booleans still work,
// and a contradiction or an unknown agent is refused.
func TestLaunchAgent(t *testing.T) {
	for _, c := range []struct {
		agent      string
		codex, agy bool
		wantC      bool
		wantA      bool
		err        string
	}{
		{agent: ""},
		{agent: "claude"},
		{agent: "codex", wantC: true},
		{agent: "agy", wantA: true},
		{codex: true, wantC: true},
		{agy: true, wantA: true},
		{agent: "agy", agy: true, wantA: true},
		{agent: "codex", codex: true, wantC: true},
		{agent: "agy", codex: true, err: `agent "agy" contradicts codex: true`},
		{agent: "claude", agy: true, err: `agent "claude" contradicts agy: true`},
		{codex: true, agy: true, err: "codex and agy are both true"},
		{agent: "gemini", err: `agent "gemini": use claude, codex or agy`},
	} {
		c2, a2, err := launchAgent(c.agent, c.codex, c.agy)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%+v: want %q, got %v", c, c.err, err)
			}
			continue
		}
		if err != nil || c2 != c.wantC || a2 != c.wantA {
			t.Errorf("%+v: got codex %v agy %v err %v", c, c2, a2, err)
		}
	}
}

// launch_session hands the agent to the one LaunchSession path ccs launch
// uses, and a refusal launches nothing.
func TestLaunchSessionAgentRoutes(t *testing.T) {
	old, oldCap := launchFn, sessionCallCap
	t.Cleanup(func() { launchFn, sessionCallCap = old, oldCap })
	sessionCallCap = 5 * time.Second
	var got []sessions.LaunchOpts
	launchFn = func(o sessions.LaunchOpts) (sessions.Launched, error) {
		got = append(got, o)
		return sessions.Launched{PID: 42, TTY: "/dev/ttys001"}, nil
	}
	call := func(args string) error {
		_, err := mcpSessionCall("launch_session", json.RawMessage(args))
		return err
	}
	if err := call(`{"name":"gem","cwd":"/tmp","brief":"hi","agent":"agy"}`); err != nil {
		t.Fatal(err)
	}
	if err := call(`{"name":"cx","cwd":"/tmp","brief":"hi","codex":true}`); err != nil {
		t.Fatal(err)
	}
	if err := call(`{"name":"cl","cwd":"/tmp","brief":"hi"}`); err != nil {
		t.Fatal(err)
	}
	if err := call(`{"name":"bad","cwd":"/tmp","brief":"hi","agent":"agy","codex":true}`); err == nil {
		t.Error("a contradiction should be refused")
	}
	if len(got) != 3 {
		t.Fatalf("want 3 launches, got %d", len(got))
	}
	if !got[0].Agy || got[0].Codex || !got[0].RecordParent || got[0].Name != "gem" {
		t.Errorf("agy: %+v", got[0])
	}
	if !got[1].Codex || got[1].Agy {
		t.Errorf("codex: %+v", got[1])
	}
	if got[2].Codex || got[2].Agy {
		t.Errorf("claude by default: %+v", got[2])
	}
}

// tools/list gives launch_session an agent enum beside the older booleans.
func TestLaunchSessionSchemaHasAgent(t *testing.T) {
	resps := mcpRoundTrip(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, tl := range resps[0]["result"].(map[string]any)["tools"].([]any) {
		tool := tl.(map[string]any)
		if tool["name"] != "launch_session" {
			continue
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		agent, ok := props["agent"].(map[string]any)
		if !ok {
			t.Fatalf("no agent field: %v", props)
		}
		enum, _ := json.Marshal(agent["enum"])
		if string(enum) != `["claude","codex","agy"]` {
			t.Errorf("enum: %s", enum)
		}
		for _, k := range []string{"name", "brief", "resume", "cwd", "agent", "codex", "agy", "preamble"} {
			if _, ok := props[k]; !ok {
				t.Errorf("missing %s", k)
			}
		}
		if len(props) != 8 {
			t.Errorf("want 8 fields, got %d", len(props))
		}
		return
	}
	t.Fatal("no launch_session")
}

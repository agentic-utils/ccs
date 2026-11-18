package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A teammate's first line names its team; the team's config names its lead.
func TestTeamLead(t *testing.T) {
	old := TeamsDir
	dir := t.TempDir()
	TeamsDir = func() string { return dir }
	t.Cleanup(func() { TeamsDir = old })
	os.MkdirAll(filepath.Join(dir, "session-f3356504"), 0o755)
	os.WriteFile(filepath.Join(dir, "session-f3356504", "config.json"),
		[]byte(`{"name":"session-f3356504","leadAgentId":"team-lead@session-f3356504","leadSessionId":"f3356504-9a4c-435e-ac3a-771f5928bac3","members":[]}`), 0o644)
	if got := TeamLead("session-f3356504"); got != "f3356504-9a4c-435e-ac3a-771f5928bac3" {
		t.Errorf("the config's leadSessionId: %q", got)
	}
	for _, bad := range []string{"", "missing", "../session-f3356504", "a/b"} {
		if got := TeamLead(bad); got != "" {
			t.Errorf("%q: no lead, got %q", bad, got)
		}
	}
	if TeamSessionPrefix("session-f3356504") != "f3356504" || TeamSessionPrefix("number-printers") != "" || TeamSessionPrefix("session-zz") != "" {
		t.Error("only a session-<id> name gives a prefix")
	}

	// The first user line's teamName is kept as Team.
	path := filepath.Join(t.TempDir(), "35494c10-d3f0-4c9f-b7e3-c2c34da967be.jsonl")
	os.WriteFile(path, []byte(strings.Join([]string{
		`{"type":"agent-setting","agentSetting":"general-purpose","sessionId":"35494c10-d3f0-4c9f-b7e3-c2c34da967be"}`,
		`{"type":"user","timestamp":"2026-11-18T11:48:34Z","teamName":"session-f3356504","agentName":"sonnet-x","entrypoint":"cli","cwd":"/r","message":{"role":"user","content":"<teammate-message teammate_id=\"team-lead\">cancel the ticket</teammate-message>"}}`,
	}, "\n")+"\n"), 0o644)
	c, err := ParseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil || c.Team != "session-f3356504" || !c.Spawned {
		t.Fatalf("a teammate keeps its team and is spawned: %+v %v", c, err)
	}
}

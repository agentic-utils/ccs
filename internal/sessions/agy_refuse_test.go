package sessions

import (
	"strings"
	"testing"
	"time"
)

// stubAgyLive makes an agy session called name run as pid for the test.
func stubAgyLive(t *testing.T, name string, pid int) {
	t.Helper()
	old := LiveAgySessions
	LiveAgySessions = func() map[string]LiveSession {
		return map[string]LiveSession{"0e5a1b2c-0000-4000-8000-000000000001": {PID: pid, Agy: true, Stat: SessionStat{Name: name, Status: "idle"}}}
	}
	t.Cleanup(func() { LiveAgySessions = old })
}

// restart and a graceful close refuse an agy session, naming it as one and
// saying why, and stop nothing.
func TestRestartAndGracefulCloseRefuseAgy(t *testing.T) {
	stubAgyLive(t, "gem-worker", 987654)
	if _, err := findRestartTarget("gem-worker", ""); err == nil ||
		!strings.Contains(err.Error(), "(an agy session)") || !strings.Contains(err.Error(), "Claude Code sessions only") {
		t.Errorf("restart: %v", err)
	}
	_, _, err := closeGraceful("gem-worker", "", time.Second, nil, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "(an agy session)") || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Errorf("graceful close: %v", err)
	}
}

// A pid neither scan knows is still told apart from Claude Code, as a guess.
func TestNotClaudeGuesses(t *testing.T) {
	if got := notClaude(987655); got != "a Codex or agy session, perhaps" {
		t.Errorf("got %q", got)
	}
}

// Adopting under a running agy session records its kind, so a handover to it
// is refused up front instead of failing on a missing socket.
func TestAdoptUnderAgyRecordsItsKind(t *testing.T) {
	newHandoverFixture(t)
	stubAgyLive(t, "gem-lead", 987656)
	if _, err := Adopt("worker", "gem-lead"); err != nil {
		t.Fatal(err)
	}
	if rec := LoadParents().Parents["worker"]; rec.Parent != "gem-lead" || rec.ParentKind != "agy" {
		t.Fatalf("record: %+v", rec)
	}
	if _, err := HandoverParentName("worker"); err == nil || !strings.Contains(err.Error(), "is an agy session, which can't be sent a handover") {
		t.Errorf("handover: %v", err)
	}
}

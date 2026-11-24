package transcript

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-utils/ccs/internal/state"
)

// agy lists only ccs's --add-dir folders for a session ccs launched, so its
// folder comes from the launch's cwd file; any other workspace comes first,
// and with neither the answer is "" so agy's history is tried.
func TestAgyPrimaryCwd(t *testing.T) {
	if err := os.MkdirAll(state.LaunchDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	launch, err := os.MkdirTemp(state.LaunchDir(), "gem-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Dir(state.SettingsPath)
	if got := AgyPrimaryCwd([]string{settings, launch}); got != "" {
		t.Errorf("no cwd file: got %q", got)
	}
	if err := os.WriteFile(filepath.Join(launch, LaunchCwdFile), []byte("/work/repo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := AgyPrimaryCwd([]string{settings, launch}); got != "/work/repo" {
		t.Errorf("launched: got %q", got)
	}
	if got := AgyPrimaryCwd([]string{launch, "/elsewhere"}); got != "/elsewhere" {
		t.Errorf("a real workspace wins: got %q", got)
	}
	if got := AgyPrimaryCwd(nil); got != "" {
		t.Errorf("none: got %q", got)
	}
}

// GetConversationsFor, the running sessions' fast load (list_sessions
// live_only, session_triage, ccs sessions), finds an agy conversation's
// transcript too.
func TestGetConversationsForFindsAgy(t *testing.T) {
	old := AgyHome
	home := t.TempDir()
	AgyHome = func() string { return home }
	t.Cleanup(func() { AgyHome = old })
	id := "0e5a1b2c-0000-4000-8000-0000000000aa"
	p := filepath.Join(AgyHome(), "brain", id, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T02:39:41Z","content":"hi"}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got := GetConversationsFor([]string{id})
	if len(got) != 1 || got[0].SessionID != id || got[0].Kind != KindAgy {
		t.Fatalf("got %+v", got)
	}
}

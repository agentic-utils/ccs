package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

const (
	threadA = "01a1a104-555b-7833-a5f9-ceb4180957c2"
	threadB = "01a1a103-0221-75b2-86f5-7662d5bc71c7"
)

// writeRollout writes a Codex transcript for thread with the given workspace
// roots into the test's Codex folder, as Codex lays them out.
func writeRollout(t *testing.T, thread string, roots ...string) string {
	t.Helper()
	dir := filepath.Join(transcript.CodexHome(), "sessions", "2026", "11", "03")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-11-03T16-31-18-"+thread+".jsonl")
	q := func(s string) string { return fmt.Sprintf("%q", s) }
	var rs []string
	for _, r := range roots {
		rs = append(rs, q(r))
	}
	line := `{"type":"session_meta","payload":{"id":` + q(thread) + `,"cwd":"/w","runtime_workspace_roots":[` + strings.Join(rs, ",") + `]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// codexEnv gives a test its own Codex folder.
func codexEnv(t *testing.T) {
	t.Helper()
	old := transcript.CodexHome
	home := t.TempDir()
	transcript.CodexHome = func() string { return home }
	t.Cleanup(func() { transcript.CodexHome = old })
}

func TestCodexClaimByThread(t *testing.T) {
	live := claimEnv(t)
	delete(live, os.Getppid()) // not under a Claude Code session either
	codexEnv(t)
	writeRollout(t, threadA, "/w", filepath.Join(state.LaunchDir(), "repay-pr-refresh"))
	t.Setenv("CODEX_THREAD_ID", threadA)
	msg, err := claim(claimA, false, time.Now())
	if err != nil || !strings.Contains(msg, "repay-pr-refresh owns") {
		t.Fatalf("a Codex session ccs launched claims under its launch name: %q %v", msg, err)
	}
	c := loadClaims().Claims[claimA][0]
	if c.Kind != "codex" || c.Thread != threadA || c.Session != threadA || c.PID != 0 {
		t.Errorf("keyed by its thread, no process (Codex's sandbox can't run ps): %+v", c)
	}

	// Not launched by ccs: Codex's own title, else the thread id.
	writeRollout(t, threadB, "/w")
	os.WriteFile(filepath.Join(transcript.CodexHome(), "session_index.jsonl"), []byte(
		`{"id":"`+threadB+`","thread_name":"Old title"}`+"\n"+`{"id":"`+threadB+`","thread_name":"Fix repay PR"}`+"\n"), 0o600)
	t.Setenv("CODEX_THREAD_ID", threadB)
	if msg, _ := claim(claimB, false, time.Now()); !strings.Contains(msg, "Fix repay PR owns") {
		t.Errorf("Codex's latest title for the thread: %q", msg)
	}
	t.Setenv("CODEX_THREAD_ID", "not-a-uuid")
	if _, err := claim(claimC, false, time.Now()); err == nil {
		t.Error("a malformed thread id isn't a Codex session")
	}
}

func TestCodexLiveness(t *testing.T) {
	codexEnv(t)
	name := "codex-live"
	path := writeRollout(t, threadA, "/w", filepath.Join(state.LaunchDir(), name))
	oldPS := proc.PsAll
	t.Cleanup(func() { proc.PsAll = oldPS })
	proc.PsAll = func() (string, error) { return "", nil }
	if sessions.CodexLive(threadA) {
		t.Error("a thread ccs launched is live only while its codex process runs")
	}
	proc.PsAll = func() (string, error) {
		return "  101 ttys001 /bin/codex -s workspace-write --add-dir " + filepath.Join(state.LaunchDir(), name) + " --add-dir /s x\n", nil
	}
	if !sessions.CodexLive(threadA) {
		t.Error("its codex process is running")
	}

	// Not launched by ccs: live while its transcript is recent.
	path = writeRollout(t, threadB, "/w")
	if !sessions.CodexLive(threadB) {
		t.Error("a recently written thread counts as live")
	}
	old := time.Now().Add(-sessions.CodexIdle - time.Hour)
	os.Chtimes(path, old, old)
	if sessions.CodexLive(threadB) {
		t.Error("a thread quiet for longer than codexIdle doesn't")
	}
	if sessions.CodexLive("01a1a1ff-0000-0000-0000-000000000000") {
		t.Error("a thread with no transcript isn't live")
	}
}

func TestCodexThreadForLaunch(t *testing.T) {
	codexEnv(t)
	writeRollout(t, threadB, "/w", filepath.Join(state.LaunchDir(), "other"))
	writeRollout(t, threadA, "/w", filepath.Join(state.LaunchDir(), "mine"))
	if got := sessions.CodexThreadFor("mine"); got != threadA {
		t.Errorf("the thread whose roots hold the name's launch folder: %q", got)
	}
	if got := sessions.CodexThreadFor("nobody"); got != "" {
		t.Errorf("no thread for a name never launched: %q", got)
	}
}

func TestCodexNudgeGoesThroughQueue(t *testing.T) {
	claimEnv(t)
	codexEnv(t)
	oldQ, oldLive := sessions.CodexQueue, sessions.CodexLive
	t.Cleanup(func() { sessions.CodexQueue, sessions.CodexLive = oldQ, oldLive })
	var got []string
	sessions.CodexQueue = func(thread, text string) error { got = append(got, thread+"|"+text); return nil }
	sessions.CodexLive = func(thread string) bool { return thread == threadA }
	c := prClaim{Session: threadA, Kind: "codex", Thread: threadA, Name: "repay-pr-refresh"}
	if err := withClaims(func(f *claimsFile) bool { f.Claims[claimA] = []prClaim{c}; return true }); err != nil {
		t.Fatal(err)
	}
	writeWatchState(t, time.Now(), &watchedPR{URL: claimA, Title: "a", State: "open", Head: "h", Checks: "failing", Failing: []string{"setup"}})
	nudgeTick(time.Now())
	nudgeTick(time.Now())
	if len(got) != 1 {
		t.Fatalf("one queued nudge per head and reason: %q", got)
	}
	if !strings.HasPrefix(got[0], threadA+"|"+sessions.CodexNudgeNote) || !strings.Contains(got[0], "checks failing: setup") || strings.Contains(got[0], transcript.CcsNote) {
		t.Errorf("queued into its thread, saying it's an automatic notice and not the user: %q", got[0])
	}
}

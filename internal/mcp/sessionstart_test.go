package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
)

// startFixture stubs the running sessions, the process tree and the
// transcripts for the SessionStart hook, with a clean parents file.
func startFixture(t *testing.T, live map[string]sessions.LiveSession, tree string, ppid int, transcripts map[string]string) {
	t.Helper()
	os.Remove(sessions.ParentsPath())
	oldSessions, oldTree, oldPPID, oldTranscript, oldWait := hookSessions, proc.PsTree, startPPID, startTranscript, startFileWait
	t.Cleanup(func() {
		hookSessions, proc.PsTree, startPPID, startTranscript, startFileWait = oldSessions, oldTree, oldPPID, oldTranscript, oldWait
		os.Remove(sessions.ParentsPath())
	})
	hookSessions = func() map[string]sessions.LiveSession { return live }
	proc.PsTree = func() (string, error) { return tree, nil }
	startPPID = func() int { return ppid }
	startTranscript = func(id string) string { return transcripts[id] }
	startFileWait = 0
}

func startHook(id, source string) {
	sessionStartHook(strings.NewReader(fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":%q}`, id, source)), time.Now())
}

func live(name string, pid int) sessions.LiveSession {
	return sessions.LiveSession{PID: pid, Stat: sessions.SessionStat{Name: name}}
}

// A claude run from another session's shell gets that session as its parent,
// from the process tree, as soon as it starts.
func TestSessionStartParentByAncestry(t *testing.T) {
	startFixture(t, map[string]sessions.LiveSession{"orch-id": live("orch", 100), "kid-id": live("kid", 200)},
		"300 200 sh -c ccs hook session-start\n200 150 claude --name kid\n150 100 zsh -c claude --name kid\n100 1 claude", 300, nil)
	startHook("kid-id", "startup")
	r := sessions.LoadParents().Parents["kid"]
	if r.Parent != "orch" || r.ParentID != "orch-id" || r.ChildID != "kid-id" || r.Source != "start" {
		t.Errorf("kid under orch, recorded at start: %+v", r)
	}
}

// One opened in a new tab (session-handoff's open-tab.sh) has no such
// ancestor: its parent is the running session whose transcript just ran the
// command naming it. An old command, or a resume, records nothing.
func TestSessionStartParentByTranscript(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, ago time.Duration, cmd string) string {
		p := filepath.Join(dir, name+".jsonl")
		line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":%q}}]}}`,
			time.Now().Add(-ago).UTC().Format(time.RFC3339), cmd)
		os.WriteFile(p, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"+line+"\n"), 0o600)
		return p
	}
	transcripts := map[string]string{
		"worker-id": write("worker", time.Minute, `${CLAUDE_SKILL_DIR}/open-tab.sh istiod-1-31-upgrade "$S/launch.sh" ~/x istiod-1-31-upgrade "$S/brief.md"`),
		"old-id":    write("old", time.Hour, `open-tab.sh istiod-1-31-upgrade launch.sh`),
		"other-id":  write("other", time.Minute, `open-tab.sh something-else launch.sh`),
	}
	running := map[string]sessions.LiveSession{
		"worker-id": live("istiod-upstream-bug-search", 100), "old-id": live("old", 101), "other-id": live("other", 102),
		"kid-id": live("istiod-1-31-upgrade", 200),
	}
	startFixture(t, running, "300 200 sh\n200 1 claude", 300, transcripts)

	startHook("kid-id", "resume")
	if _, ok := sessions.LoadParents().Parents["istiod-1-31-upgrade"]; ok {
		t.Fatal("a resume keeps the parent it has: nothing recorded")
	}
	startHook("kid-id", "startup")
	if r := sessions.LoadParents().Parents["istiod-1-31-upgrade"]; r.Parent != "istiod-upstream-bug-search" || r.ParentID != "worker-id" || r.Source != "start" {
		t.Errorf("under the session that just ran open-tab.sh for it: %+v", r)
	}
}

// ccs launch or launch_session records the parent itself; the hook never
// replaces that record.
func TestSessionStartLeavesALaunchRecord(t *testing.T) {
	startFixture(t, map[string]sessions.LiveSession{"orch-id": live("orch", 100), "kid-id": live("kid", 200)},
		"300 200 sh\n200 100 claude\n100 1 claude", 300, nil)
	sessions.SetParents([]sessions.ParentRec{{Child: "kid", ChildID: "kid-id", Parent: "launcher", ParentID: "l-id"}})
	startHook("kid-id", "startup")
	if r := sessions.LoadParents().Parents["kid"]; r.Parent != "launcher" {
		t.Errorf("ccs launch's record stays: %+v", r)
	}
}

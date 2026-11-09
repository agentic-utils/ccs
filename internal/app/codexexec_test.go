package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestCodexExecNestsUnderItsParentProcess(t *testing.T) {
	sessions.ExecParents.Lock()
	sessions.ExecParents.Loaded = false
	sessions.ExecParents.Unlock()
	os.Remove(sessions.ExecParentsPath())
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	path := codexFixture(t, cxA, "/w/repo", "exec")
	// the fixture's meta timestamp is 10:00:00, its file written now: after start
	oldTree, oldStarts, oldCwd := proc.PsTree, proc.ProcessStartTimes, transcript.CodexProcCwd
	t.Cleanup(func() { proc.PsTree, proc.ProcessStartTimes, transcript.CodexProcCwd = oldTree, oldStarts, oldCwd })
	// claude 100 -> zsh 200 -> codex exec 300; another codex exec 400 under launchd
	proc.PsTree = func() (string, error) {
		return "100 1 /bin/claude\n200 100 /bin/zsh -c codex exec x\n300 200 /usr/local/bin/codex exec x\n400 1 /usr/local/bin/codex exec y\n", nil
	}
	proc.ProcessStartTimes = func(pids map[string]int) map[int]time.Time {
		out := map[int]time.Time{}
		for _, p := range pids {
			out[p] = start
		}
		return out
	}
	transcript.CodexProcCwd = func(int) string { return "/w/repo" }
	sessions.NoteCodexExecs(map[string]sessions.LiveSession{"claude-session": {Pid: 100}})
	if p := sessions.ExecParentOf(cxA); p != "claude-session" {
		t.Fatalf("exec under claude's process tree: parent %q", p)
	}
	// It survives the run exiting and ccs restarting: read back from disk.
	sessions.ExecParents.Lock()
	sessions.ExecParents.Loaded = false
	sessions.ExecParents.Unlock()
	if p := sessions.ExecParentOf(cxA); p != "claude-session" {
		t.Errorf("after a restart: %q", p)
	}
	m := model{}
	c, _ := transcript.ParseCodexFile(path, time.Time{}, 0)
	if p := m.spawnParent(*c); p != "claude-session" {
		t.Errorf("spawnParent uses the link first: %q", p)
	}
	if _, err := os.Stat(filepath.Dir(sessions.ExecParentsPath())); err != nil {
		t.Error(err)
	}
}

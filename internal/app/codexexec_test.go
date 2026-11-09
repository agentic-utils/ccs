package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexExecArgs(t *testing.T) {
	for _, c := range []struct {
		cmd  []string
		exec bool
		cd   string
	}{
		{[]string{"/usr/local/bin/codex", "exec", "do it"}, true, ""},
		{[]string{"codex", "-c", "model=x", "--cd", "/w", "exec", "-"}, true, "/w"},
		{[]string{"node", "/n/bin/codex.js", "e", "x"}, true, ""},
		{[]string{"/A/codex", "-c", "forced_login_method=chatgpt", "exec-server", "--remote", "x"}, false, ""},
		{[]string{"codex", "-c", "exec"}, false, ""}, // a flag's value, not the subcommand
		{[]string{"codex", "resume", "id"}, false, ""},
		{[]string{"zsh", "-c", "codex exec x"}, false, ""},
	} {
		if exec, cd := codexExecArgs(c.cmd); exec != c.exec || cd != c.cd {
			t.Errorf("%v: %v %q", c.cmd, exec, cd)
		}
	}
}

func TestCodexExecNestsUnderItsParentProcess(t *testing.T) {
	execParents.Lock()
	execParents.loaded = false
	execParents.Unlock()
	os.Remove(execParentsPath())
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	path := codexFixture(t, cxA, "/w/repo", "exec")
	// the fixture's meta timestamp is 10:00:00, its file written now: after start
	oldTree, oldStarts, oldCwd := psTree, processStartTimes, codexProcCwd
	t.Cleanup(func() { psTree, processStartTimes, codexProcCwd = oldTree, oldStarts, oldCwd })
	// claude 100 -> zsh 200 -> codex exec 300; another codex exec 400 under launchd
	psTree = func() (string, error) {
		return "100 1 /bin/claude\n200 100 /bin/zsh -c codex exec x\n300 200 /usr/local/bin/codex exec x\n400 1 /usr/local/bin/codex exec y\n", nil
	}
	processStartTimes = func(pids map[string]int) map[int]time.Time {
		out := map[int]time.Time{}
		for _, p := range pids {
			out[p] = start
		}
		return out
	}
	codexProcCwd = func(int) string { return "/w/repo" }
	noteCodexExecs(map[string]liveSession{"claude-session": {pid: 100}})
	if p := execParentOf(cxA); p != "claude-session" {
		t.Fatalf("exec under claude's process tree: parent %q", p)
	}
	// It survives the run exiting and ccs restarting: read back from disk.
	execParents.Lock()
	execParents.loaded = false
	execParents.Unlock()
	if p := execParentOf(cxA); p != "claude-session" {
		t.Errorf("after a restart: %q", p)
	}
	m := model{}
	c, _ := parseCodexFile(path, time.Time{}, 0)
	if p := m.spawnParent(*c); p != "claude-session" {
		t.Errorf("spawnParent uses the link first: %q", p)
	}
	if _, err := os.Stat(filepath.Dir(execParentsPath())); err != nil {
		t.Error(err)
	}
}

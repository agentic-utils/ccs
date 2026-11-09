package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// sessionsFixture points ccs at a temporary sessions folder; write adds a
// live session file for pid called name.
func sessionsFixture(t *testing.T) (write func(file string, pid int, name string)) {
	t.Helper()
	dir := t.TempDir()
	old, oldStarts := sessions.GetSessionsDir, proc.ProcessStartTimes
	sessions.GetSessionsDir = func() string { return dir }
	proc.ProcessStartTimes = func(map[string]int) map[int]time.Time { return nil } // unknown: not checked
	t.Cleanup(func() { sessions.GetSessionsDir, proc.ProcessStartTimes = old, oldStarts })
	return func(file string, pid int, name string) {
		os.WriteFile(filepath.Join(dir, file), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"id-%s-%d","name":%q}`, pid, name, pid, name)), 0o600)
	}
}

func stubLaunch(t *testing.T) {
	t.Helper()
	t.Setenv("ITERM_SESSION_ID", "w0t0p0")
	oldBin, oldWait := sessions.LookBin, sessions.LaunchWait
	sessions.LookBin = func(name string) (string, error) { return "/bin/" + name, nil }
	sessions.LaunchWait = 300 * time.Millisecond
	oldTab, oldRetype, oldRetypeAt, oldRunning := sessions.OpenLaunchTab, sessions.RetypeLauncher, sessions.LaunchRetype, sessions.LauncherRunning
	t.Cleanup(func() {
		sessions.LookBin, sessions.LaunchWait, sessions.OpenLaunchTab, sessions.RetypeLauncher, sessions.LaunchRetype, sessions.LauncherRunning = oldBin, oldWait, oldTab, oldRetype, oldRetypeAt, oldRunning
	})
}

func TestLaunchAndCloseFromTheList(t *testing.T) {
	stubLaunch(t)
	sessionsFixture(t)
	var script string
	sessions.OpenLaunchTab = func(s string) (string, error) { script = s; return "", errors.New("no tab in tests") }
	cwd := t.TempDir()
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "s1", Cwd: cwd, Title: "one", LastTimestamp: "2026-11-03T10:00:00Z"}}), "", nil)
	m.width, m.height = 120, 30
	m, _ = chord(m, "n")
	if m.launchStep != 1 || m.launchCwd != cwd || !strings.Contains(strip2(m.View()), "New session name:") {
		t.Fatalf("^X N asks for a name, starting in the selected session's folder: step %d, cwd %q", m.launchStep, m.launchCwd)
	}
	for _, r := range "bad name" {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.launchStep != 1 || m.errorMsg == "" {
		t.Fatalf("a name with a space is refused: %d %q", m.launchStep, m.errorMsg)
	}
	m.launchInput.SetValue("helper")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.launchStep != 2 || !strings.Contains(strip2(m.View()), "Brief:") {
		t.Fatalf("then its brief: %d", m.launchStep)
	}
	m.launchInput.SetValue("look into it")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || m.launchStep != 0 {
		t.Fatal("enter starts it")
	}
	res, _ := m.Update(cmd())
	m = res.(model)
	if !strings.Contains(m.errorMsg, "Couldn't start helper") || script != filepath.Join(state.LaunchDir(), "helper", "launch.sh") {
		t.Errorf("the launch ran and its failure shows: %q, %q", m.errorMsg, script)
	}

	// ^X K on a live session in a terminal tab: twice to stop it and close the tab.
	m.live = map[string]bool{"s1": true}
	m.livePIDs = map[string]int{"s1": 999999}
	m, cmd = chord(m, "k")
	if cmd != nil || !strings.Contains(m.errorMsg, "again to stop one and close its tab") {
		t.Fatalf("first ^X K asks again: %q", m.errorMsg)
	}
	m, cmd = chord(m, "k")
	if cmd == nil {
		t.Fatal("second ^X K closes it")
	}
	if msg := cmd().(closeDoneMsg); msg.err == nil || !strings.Contains(msg.err.Error(), "no terminal") {
		t.Errorf("closeSession ran on its pid (no tty in tests): %+v", msg)
	}
}

func TestLaunchResumeNeedsATranscript(t *testing.T) {
	sessionsFixture(t)
	stubLaunch(t)
	opened := false
	sessions.OpenLaunchTab = func(string) (string, error) { opened = true; return "", errors.New("stop here") }
	projects := t.TempDir()
	old := transcript.GetProjectsDir
	transcript.GetProjectsDir = func() string { return projects }
	t.Cleanup(func() { transcript.GetProjectsDir = old })
	os.MkdirAll(filepath.Join(projects, "-w"), 0o755)
	os.WriteFile(filepath.Join(projects, "-w", "known-id.jsonl"), []byte("{}\n"), 0o644)
	day := filepath.Join(transcript.CodexHome(), "sessions", "2026", "11", "03")
	os.MkdirAll(day, 0o755)
	thread := "01a1a197-6287-7f32-8eb6-6cea70678f3f"
	os.WriteFile(filepath.Join(day, "rollout-2026-11-03T10-00-00-"+thread+".jsonl"), []byte("{}\n"), 0o644)

	for _, c := range []struct {
		id    string
		codex bool
	}{{"missing-id", false}, {"01a1a197-0000-0000-0000-000000000000", true}, {thread, false}, {"known-id", true}} {
		opened = false
		_, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "r", Resume: c.id, Codex: c.codex, Cwd: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "to resume") || opened {
			t.Errorf("%+v: refused before any tab opens: %v (opened %v)", c, err, opened)
		}
	}
	for _, c := range []struct {
		id    string
		codex bool
	}{{"known-id", false}, {thread, true}} {
		opened = false
		sessions.LaunchSession(sessions.LaunchOpts{Name: "r", Resume: c.id, Codex: c.codex, Cwd: t.TempDir()})
		if !opened {
			t.Errorf("%+v: a session that exists gets its tab", c)
		}
	}
}

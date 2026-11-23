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
	if m.launchStep != 1 || m.launchCwd != cwd || !strings.Contains(strip2(m.View()), "Agent:") {
		t.Fatalf("^X N asks for the agent first, starting in the selected session's folder: step %d, cwd %q", m.launchStep, m.launchCwd)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.launchStep != 2 || !strings.Contains(strip2(m.View()), "New session name:") {
		t.Fatalf("then its name: step %d", m.launchStep)
	}
	for _, r := range "bad name" {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.launchStep != 2 || m.errorMsg == "" {
		t.Fatalf("a name with a space is refused: %d %q", m.launchStep, m.errorMsg)
	}
	m.launchInput.SetValue("helper")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.launchStep != 3 || !strings.Contains(strip2(m.View()), "Brief:") {
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

	// ^X K on a live session in a terminal tab asks, then y stops it and closes the tab.
	m.live = map[string]bool{"s1": true}
	m.livePIDs = map[string]int{"s1": 999999}
	m, cmd = chord(m, "k")
	if cmd != nil || m.confirmStop != "s1" || !strings.Contains(m.View(), "Stop \"one\" and close its tab?") {
		t.Fatalf("^X K asks first: %q", m.confirmStop)
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || m.confirmStop != "" {
		t.Fatal("esc cancels the stop")
	}
	m, _ = chord(m, "k")
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil || m.confirmStop != "" {
		t.Fatal("y closes it")
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

// ^X R asks first, then y restarts the session by its name; one mid-turn
// asks in red, and y then forces it.
func TestRestartFromTheList(t *testing.T) {
	old := restartSession
	t.Cleanup(func() { restartSession = old })
	var got []sessions.RestartOpts
	restartSession = func(o sessions.RestartOpts) (string, error) {
		got = append(got, o)
		return "restarted " + o.Name, nil
	}
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "s1", Title: "one", LastTimestamp: "2026-11-03T10:00:00Z"}}), "", nil)
	m.width, m.height = 120, 30
	m, cmd := chord(m, "r")
	if cmd != nil || m.confirmRestart != "" || !strings.Contains(m.errorMsg, "^X R restarts") {
		t.Fatalf("a session that isn't running can't be restarted: %q", m.errorMsg)
	}
	m.errorMsg = ""
	m.live = map[string]bool{"s1": true}
	m.livePIDs = map[string]int{"s1": 999999}
	m, cmd = chord(m, "r")
	if cmd != nil || m.confirmRestart != "s1" || m.restartForce || !strings.Contains(m.View(), "Restart \"one\"?") {
		t.Fatalf("^X R asks first: %q", m.confirmRestart)
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || m.confirmRestart != "" || len(got) != 0 {
		t.Fatal("esc cancels it")
	}
	m, _ = chord(m, "r")
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil || m.confirmRestart != "" {
		t.Fatal("y restarts it")
	}
	res, _ := m.Update(cmd())
	m = res.(model)
	if len(got) != 1 || got[0].Name != "one" || got[0].Force || !strings.Contains(m.errorMsg, "restarted one") {
		t.Errorf("restarted by name, not forced: %+v, %q", got, m.errorMsg)
	}

	m.chatStatus = map[string]sessions.SessionStat{"s1": {Status: "busy"}}
	m, _ = chord(m, "r")
	if !m.restartForce || !strings.Contains(m.View(), "mid-turn") {
		t.Fatal("mid-turn, it says so")
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	cmd()
	if len(got) != 2 || !got[1].Force {
		t.Errorf("y forces it: %+v", got)
	}
}

// ^X N's agent row: only the agents on PATH, ←/→ and Tab move the choice,
// the launch runs the chosen one and the choice is saved for next time.
func TestLaunchAgentChoice(t *testing.T) {
	stubLaunch(t)
	sessionsFixture(t)
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	oldSettings := state.SettingsPath
	state.SettingsPath = settingsPath
	t.Cleanup(func() { state.SettingsPath = oldSettings })
	var script string
	sessions.OpenLaunchTab = func(s string) (string, error) {
		b, _ := os.ReadFile(s)
		script = string(b)
		return "", errors.New("no tab in tests")
	}
	cwd := t.TempDir()
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "s1", Cwd: cwd, Title: "one", LastTimestamp: "2026-11-03T10:00:00Z"}}), "", nil)
	m.width, m.height = 160, 30

	m, _ = chord(m, "n")
	view := strip2(m.View())
	if !strings.Contains(view, "Agent: ✻ Claude   ⬡ Codex   ✦ Gemini") || m.launchAgentNow().id != "claude" {
		t.Fatalf("the first default is Claude, all three offered: %q", m.launchAgentNow().id)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRight})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRight})
	if m.launchAgentNow().id != "agy" {
		t.Fatalf("→ twice is Gemini: %q", m.launchAgentNow().id)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.launchAgentNow().id != "claude" {
		t.Fatalf("tab wraps round to Claude: %q", m.launchAgentNow().id)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.launchAgentNow().id != "codex" {
		t.Fatalf("← twice from Claude is Codex: %q", m.launchAgentNow().id)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.launchInput.Value() != "" {
		t.Fatal("the agent row takes no text")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "helper" {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyLeft}) // the name box keeps its arrows
	if m.launchAgentNow().id != "codex" || !strings.Contains(strip2(m.View()), "⬡ Codex") {
		t.Fatalf("← in the name box moves the cursor, not the agent: %q", m.launchAgentNow().id)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.launchInput.SetValue("look into it")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter starts it")
	}
	cmd()
	if !strings.Contains(script, "/bin/codex") || strings.Contains(script, "/bin/claude") {
		t.Errorf("the launcher runs codex: %q", script)
	}
	if s := loadSettings(); s.LaunchAgent != "codex" {
		t.Errorf("the choice is saved: %q", s.LaunchAgent)
	}

	m2 := initialModel(buildItems([]transcript.Conversation{{SessionID: "s1", Cwd: cwd, Title: "one", LastTimestamp: "2026-11-03T10:00:00Z"}}), "", nil)
	m2.width, m2.height = 160, 30
	m2.launchAgent = loadSettings().LaunchAgent
	m2, _ = chord(m2, "n")
	if m2.launchAgentNow().id != "codex" {
		t.Errorf("the next ^X N starts on the last choice: %q", m2.launchAgentNow().id)
	}

	// Only the agents on PATH; with one, nothing to choose.
	sessions.LookBin = func(name string) (string, error) {
		if name == "agy" || name == "claude" {
			return "/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	m2, _ = key(m2, tea.KeyMsg{Type: tea.KeyEsc})
	m2, _ = chord(m2, "n")
	if v := strip2(m2.View()); strings.Contains(v, "Codex") || !strings.Contains(v, "✦ Gemini") || m2.launchAgentNow().id != "claude" {
		t.Errorf("codex isn't offered, and a saved codex falls back to the first: %q", m2.launchAgentNow().id)
	}
	sessions.LookBin = func(name string) (string, error) {
		if name == "agy" {
			return "/bin/agy", nil
		}
		return "", errors.New("not found")
	}
	m2, _ = key(m2, tea.KeyMsg{Type: tea.KeyEsc})
	m2, _ = chord(m2, "n")
	if m2.launchStep != 2 || m2.launchAgentNow().id != "agy" {
		t.Errorf("one agent on PATH skips the row: step %d, %q", m2.launchStep, m2.launchAgentNow().id)
	}
	sessions.LookBin = func(string) (string, error) { return "", errors.New("not found") }
	m2, _ = key(m2, tea.KeyMsg{Type: tea.KeyEsc})
	m2, _ = chord(m2, "n")
	if m2.launchStep != 0 || !strings.Contains(m2.errorMsg, "none is on PATH") {
		t.Errorf("none on PATH says so: %d %q", m2.launchStep, m2.errorMsg)
	}
}

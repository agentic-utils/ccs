package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

func TestLaunchWaitsForTheSession(t *testing.T) {
	write := sessionsFixture(t)
	stubLaunch(t)
	var script string
	sessions.OpenLaunchTab = func(s string) (string, error) {
		script = s
		write("1.json", os.Getpid(), "worker") // claude starts and writes its session file
		return "/dev/ttys042", nil
	}
	cwd := t.TempDir()
	l, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "worker", Brief: "fix the `thing` $NOW", Cwd: cwd})
	if err != nil || l.Pid != os.Getpid() || l.Tty != "/dev/ttys042" {
		t.Fatalf("launched: %+v, %v", l, err)
	}
	if script != filepath.Join(state.LaunchDir(), "worker", "launch.sh") {
		t.Errorf("the tab runs the launcher in its own folder: %s", script)
	}
	brief, _ := os.ReadFile(filepath.Join(state.LaunchDir(), "worker", "brief.md"))
	body, _ := os.ReadFile(script)
	if string(brief) != "fix the `thing` $NOW" || !strings.Contains(string(body), "cd '"+cwd+"'") {
		t.Errorf("brief kept as written, launcher starts in cwd: %q\n%s", brief, body)
	}

	if _, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "worker", Brief: "again", Cwd: cwd}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a name already running is refused: %v", err)
	}
	for _, bad := range []string{"", "a b", "x;rm", "-n", strings.Repeat("a", 65)} {
		if _, err := sessions.LaunchSession(sessions.LaunchOpts{Name: bad, Brief: "x", Cwd: cwd}); err == nil {
			t.Errorf("name %q should be refused", bad)
		}
	}
	if _, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "ok", Cwd: cwd}); err == nil {
		t.Error("nothing to start: no brief and nothing to resume")
	}
}

func TestLaunchTypesTheLauncherAgainWhenTheShellLostIt(t *testing.T) {
	write := sessionsFixture(t)
	stubLaunch(t)
	sessions.LaunchWait, sessions.LaunchRetype = time.Second, 50*time.Millisecond
	sessions.OpenLaunchTab = func(string) (string, error) { return "/dev/ttys044", nil } // typed before the shell read input: lost
	running := false
	sessions.LauncherRunning = func(tty, script string) bool { return running }
	typed := 0
	sessions.RetypeLauncher = func(tty, script string) error {
		typed++
		if tty != "/dev/ttys044" || !strings.HasSuffix(script, "launch.sh") {
			t.Errorf("typed into %s: %s", tty, script)
		}
		running = true
		write("1.json", os.Getpid(), "late") // this time it starts
		return nil
	}
	if l, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "late", Brief: "x", Cwd: t.TempDir()}); err != nil || l.Pid != os.Getpid() || typed != 1 {
		t.Errorf("typed again once and started: %+v %v, typed %d", l, err, typed)
	}

	// Something runs the launcher already (claude at a trust prompt): never typed into.
	sessions.OpenLaunchTab = func(string) (string, error) { return "/dev/ttys045", nil }
	sessions.LauncherRunning = func(string, string) bool { return true }
	typed = 0
	sessions.LaunchWait = 200 * time.Millisecond
	sessions.LaunchSession(sessions.LaunchOpts{Name: "stuck", Brief: "x", Cwd: t.TempDir()})
	if typed != 0 {
		t.Errorf("typed into a tab that runs it: %d", typed)
	}
}

func TestLaunchFailsLoudlyWhenNothingStarts(t *testing.T) {
	sessionsFixture(t)
	stubLaunch(t)
	sessions.OpenLaunchTab = func(string) (string, error) { return "/dev/ttys043", nil }
	_, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "ghost", Brief: "x", Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "didn't start") || !strings.Contains(err.Error(), "ttys043") {
		t.Errorf("says it didn't start and where to look: %v", err)
	}
	sessions.OpenLaunchTab = func(string) (string, error) { return "", errors.New("Automation denied") }
	if _, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "ghost2", Brief: "x", Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "Automation denied") {
		t.Errorf("a tab that couldn't open is an error: %v", err)
	}
	t.Setenv("ITERM_SESSION_ID", "")
	if _, err := sessions.LaunchSession(sessions.LaunchOpts{Name: "ghost3", Brief: "x", Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "iTerm") {
		t.Errorf("outside iTerm: %v", err)
	}
}

func TestCodexSessionsFoundByTheirLaunchFolder(t *testing.T) {
	sessionsFixture(t)
	old := proc.PsAll
	t.Cleanup(func() { proc.PsAll = old })
	d := filepath.Join(state.LaunchDir(), "cx")
	proc.PsAll = func() (string, error) {
		return "  101 ttys001 /bin/codex -s workspace-write -a never --add-dir " + d + " do it\n" +
			"  102 ttys002 /bin/codex --add-dir " + d + "-other x\n" + // another name's folder
			"  103 ttys003 vim " + d + "/brief.md\n", nil
	}
	if pids, _ := sessions.SessionsNamed("cx"); len(pids) != 1 || pids[0] != 101 {
		t.Errorf("only codex with that launch folder: %v", pids)
	}
}

func TestCloseStopsThenClosesTheTab(t *testing.T) {
	write := sessionsFixture(t)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { child.Wait(); close(exited) }()
	pid := child.Process.Pid
	write("a.json", pid, "worker")
	oldTTY, oldSig, oldTab, oldWait := proc.TtyOf, proc.SignalPID, sessions.CloseTab, sessions.CloseWait
	t.Cleanup(func() {
		proc.TtyOf, proc.SignalPID, sessions.CloseTab, sessions.CloseWait = oldTTY, oldSig, oldTab, oldWait
		child.Process.Kill()
	})
	proc.TtyOf = func(int) string { return "/dev/ttys044" }
	sessions.CloseWait = 2 * time.Second
	var closed []string
	sessions.CloseTab = func(tty string) bool {
		if proc.PidAlive(pid) {
			select {
			case <-exited:
			default:
				t.Error("the tab is closed only after the process has exited")
			}
		}
		closed = append(closed, tty)
		return true
	}
	proc.SignalPID = func(p int, sig syscall.Signal) error {
		if p != pid {
			t.Fatalf("signalled %d, not the session's %d", p, pid)
		}
		err := syscall.Kill(p, sig)
		<-exited // reaped, so pidAlive sees it gone
		return err
	}
	msg, err := sessions.CloseByName("worker")
	if err != nil || !strings.Contains(msg, "closed worker") || len(closed) != 1 || closed[0] != "/dev/ttys044" {
		t.Fatalf("closed: %q, %v, tabs %v", msg, err, closed)
	}
}

func TestCloseRefusals(t *testing.T) {
	write := sessionsFixture(t)
	if _, err := sessions.CloseByName("nobody"); err == nil || !strings.Contains(err.Error(), "no running session") {
		t.Errorf("unknown name: %v", err)
	}
	write("a.json", os.Getpid(), "twin")
	write("b.json", os.Getppid(), "twin")
	if _, err := sessions.CloseByName("twin"); err == nil || !strings.Contains(err.Error(), "2 running sessions") {
		t.Errorf("two sessions with one name are left alone: %v", err)
	}
	write("c.json", os.Getpid(), "stuck")
	oldTTY, oldSig, oldTab, oldWait := proc.TtyOf, proc.SignalPID, sessions.CloseTab, sessions.CloseWait
	t.Cleanup(func() {
		proc.TtyOf, proc.SignalPID, sessions.CloseTab, sessions.CloseWait = oldTTY, oldSig, oldTab, oldWait
	})
	if _, err := sessions.CloseSession("stuck", os.Getpid()); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Errorf("no tty (an app or claude --bg): %v", err)
	}
	proc.TtyOf = func(int) string { return "/dev/ttys045" }
	proc.SignalPID = func(int, syscall.Signal) error { return nil } // ignores it
	sessions.CloseWait = 200 * time.Millisecond
	tabClosed := false
	sessions.CloseTab = func(string) bool { tabClosed = true; return true }
	if _, err := sessions.CloseSession("stuck", os.Getpid()); err == nil || !strings.Contains(err.Error(), "still running") || tabClosed {
		t.Errorf("a process that won't exit keeps its tab: %v, closed %v", err, tabClosed)
	}
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

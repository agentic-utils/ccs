package main

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
)

func TestLaunchScript(t *testing.T) {
	dir := "/c/launch/n"
	got := launchScript(launchOpts{name: "n", brief: "do it", cwd: "/w"}, "/bin/claude", dir)
	want := "#!/bin/zsh -l\ncd '/w' || exit 1\n[ -f ~/.claude.secret ] && source ~/.claude.secret\n" +
		`exec '/bin/claude' "$(cat '/c/launch/n/brief.md')" --name 'n' ${=CCS_CLAUDE_ARGS}` + "\n"
	if got != want {
		t.Errorf("the brief goes before CCS_CLAUDE_ARGS (its --channels takes every word after it):\n%s", got)
	}
	got = launchScript(launchOpts{name: "n", resume: "abc", cwd: "/w"}, "/bin/claude", dir)
	if !strings.Contains(got, `exec '/bin/claude' --resume 'abc' --name 'n' ${=CCS_CLAUDE_ARGS}`) {
		t.Errorf("resume, no brief:\n%s", got)
	}
	got = launchScript(launchOpts{name: "n", brief: "x", cwd: "/w", codex: true}, "/bin/codex", dir)
	if !strings.Contains(got, `exec '/bin/codex' -s workspace-write -a never --add-dir '/c/launch/n' "$(cat '/c/launch/n/brief.md')"`) || strings.Contains(got, "claude.secret") {
		t.Errorf("codex, its launch folder writable and marking it:\n%s", got)
	}
	got = launchScript(launchOpts{name: "n", resume: "r1", cwd: "/w", codex: true}, "/bin/codex", dir)
	if !strings.Contains(got, `exec '/bin/codex' resume 'r1' -s`) {
		t.Errorf("codex resume:\n%s", got)
	}
}

// sessionsFixture points ccs at a temporary sessions folder; write adds a
// live session file for pid called name.
func sessionsFixture(t *testing.T) (write func(file string, pid int, name string)) {
	t.Helper()
	dir := t.TempDir()
	old, oldStarts := getSessionsDir, processStartTimes
	getSessionsDir = func() string { return dir }
	processStartTimes = func(map[string]int) map[int]time.Time { return nil } // unknown: not checked
	t.Cleanup(func() { getSessionsDir, processStartTimes = old, oldStarts })
	return func(file string, pid int, name string) {
		os.WriteFile(filepath.Join(dir, file), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"id-%s-%d","name":%q}`, pid, name, pid, name)), 0o600)
	}
}

func stubLaunch(t *testing.T) {
	t.Helper()
	t.Setenv("ITERM_SESSION_ID", "w0t0p0")
	oldBin, oldWait := lookBin, launchWait
	lookBin = func(name string) (string, error) { return "/bin/" + name, nil }
	launchWait = 300 * time.Millisecond
	oldTab := openLaunchTab
	t.Cleanup(func() { lookBin, launchWait, openLaunchTab = oldBin, oldWait, oldTab })
}

func TestLaunchWaitsForTheSession(t *testing.T) {
	write := sessionsFixture(t)
	stubLaunch(t)
	var script string
	openLaunchTab = func(s string) (string, error) {
		script = s
		write("1.json", os.Getpid(), "worker") // claude starts and writes its session file
		return "/dev/ttys042", nil
	}
	cwd := t.TempDir()
	l, err := launchSession(launchOpts{name: "worker", brief: "fix the `thing` $NOW", cwd: cwd})
	if err != nil || l.pid != os.Getpid() || l.tty != "/dev/ttys042" {
		t.Fatalf("launched: %+v, %v", l, err)
	}
	if script != filepath.Join(launchDir(), "worker", "launch.sh") {
		t.Errorf("the tab runs the launcher in its own folder: %s", script)
	}
	brief, _ := os.ReadFile(filepath.Join(launchDir(), "worker", "brief.md"))
	body, _ := os.ReadFile(script)
	if string(brief) != "fix the `thing` $NOW" || !strings.Contains(string(body), "cd '"+cwd+"'") {
		t.Errorf("brief kept as written, launcher starts in cwd: %q\n%s", brief, body)
	}

	if _, err := launchSession(launchOpts{name: "worker", brief: "again", cwd: cwd}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a name already running is refused: %v", err)
	}
	for _, bad := range []string{"", "a b", "x;rm", "-n", strings.Repeat("a", 65)} {
		if _, err := launchSession(launchOpts{name: bad, brief: "x", cwd: cwd}); err == nil {
			t.Errorf("name %q should be refused", bad)
		}
	}
	if _, err := launchSession(launchOpts{name: "ok", cwd: cwd}); err == nil {
		t.Error("nothing to start: no brief and nothing to resume")
	}
}

func TestLaunchFailsLoudlyWhenNothingStarts(t *testing.T) {
	sessionsFixture(t)
	stubLaunch(t)
	openLaunchTab = func(string) (string, error) { return "/dev/ttys043", nil }
	_, err := launchSession(launchOpts{name: "ghost", brief: "x", cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "didn't start") || !strings.Contains(err.Error(), "ttys043") {
		t.Errorf("says it didn't start and where to look: %v", err)
	}
	openLaunchTab = func(string) (string, error) { return "", errors.New("Automation denied") }
	if _, err := launchSession(launchOpts{name: "ghost2", brief: "x", cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "Automation denied") {
		t.Errorf("a tab that couldn't open is an error: %v", err)
	}
	t.Setenv("ITERM_SESSION_ID", "")
	if _, err := launchSession(launchOpts{name: "ghost3", brief: "x", cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "iTerm") {
		t.Errorf("outside iTerm: %v", err)
	}
}

func TestCodexSessionsFoundByTheirLaunchFolder(t *testing.T) {
	sessionsFixture(t)
	old := psAll
	t.Cleanup(func() { psAll = old })
	d := filepath.Join(launchDir(), "cx")
	psAll = func() (string, error) {
		return "  101 ttys001 /bin/codex -s workspace-write -a never --add-dir " + d + " do it\n" +
			"  102 ttys002 /bin/codex --add-dir " + d + "-other x\n" + // another name's folder
			"  103 ttys003 vim " + d + "/brief.md\n", nil
	}
	if pids, _ := sessionsNamed("cx"); len(pids) != 1 || pids[0] != 101 {
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
	oldTTY, oldSig, oldTab, oldWait := ttyOf, signalPID, closeTab, closeWait
	t.Cleanup(func() { ttyOf, signalPID, closeTab, closeWait = oldTTY, oldSig, oldTab, oldWait; child.Process.Kill() })
	ttyOf = func(int) string { return "/dev/ttys044" }
	closeWait = 2 * time.Second
	var closed []string
	closeTab = func(tty string) bool {
		if pidAlive(pid) {
			select {
			case <-exited:
			default:
				t.Error("the tab is closed only after the process has exited")
			}
		}
		closed = append(closed, tty)
		return true
	}
	signalPID = func(p int, sig syscall.Signal) error {
		if p != pid {
			t.Fatalf("signalled %d, not the session's %d", p, pid)
		}
		err := syscall.Kill(p, sig)
		<-exited // reaped, so pidAlive sees it gone
		return err
	}
	msg, err := closeByName("worker")
	if err != nil || !strings.Contains(msg, "closed worker") || len(closed) != 1 || closed[0] != "/dev/ttys044" {
		t.Fatalf("closed: %q, %v, tabs %v", msg, err, closed)
	}
}

func TestCloseRefusals(t *testing.T) {
	write := sessionsFixture(t)
	if _, err := closeByName("nobody"); err == nil || !strings.Contains(err.Error(), "no running session") {
		t.Errorf("unknown name: %v", err)
	}
	write("a.json", os.Getpid(), "twin")
	write("b.json", os.Getppid(), "twin")
	if _, err := closeByName("twin"); err == nil || !strings.Contains(err.Error(), "2 running sessions") {
		t.Errorf("two sessions with one name are left alone: %v", err)
	}
	write("c.json", os.Getpid(), "stuck")
	oldTTY, oldSig, oldTab, oldWait := ttyOf, signalPID, closeTab, closeWait
	t.Cleanup(func() { ttyOf, signalPID, closeTab, closeWait = oldTTY, oldSig, oldTab, oldWait })
	if _, err := closeSession("stuck", os.Getpid()); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Errorf("no tty (an app or claude --bg): %v", err)
	}
	ttyOf = func(int) string { return "/dev/ttys045" }
	signalPID = func(int, syscall.Signal) error { return nil } // ignores it
	closeWait = 200 * time.Millisecond
	tabClosed := false
	closeTab = func(string) bool { tabClosed = true; return true }
	if _, err := closeSession("stuck", os.Getpid()); err == nil || !strings.Contains(err.Error(), "still running") || tabClosed {
		t.Errorf("a process that won't exit keeps its tab: %v, closed %v", err, tabClosed)
	}
}

func TestLaunchAndCloseFromTheList(t *testing.T) {
	stubLaunch(t)
	sessionsFixture(t)
	var script string
	openLaunchTab = func(s string) (string, error) { script = s; return "", errors.New("no tab in tests") }
	cwd := t.TempDir()
	m := initialModel(buildItems([]Conversation{{SessionID: "s1", Cwd: cwd, Title: "one", LastTimestamp: "2026-11-03T10:00:00Z"}}), "", nil)
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
	if !strings.Contains(m.errorMsg, "Couldn't start helper") || script != filepath.Join(launchDir(), "helper", "launch.sh") {
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

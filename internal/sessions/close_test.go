package sessions

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

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// gracefulFixture is a running "session" (a sleep) called worker, with a
// session file and a transcript, a terminal, and a close that really stops
// it. setStatus rewrites its session file; say appends a line to its
// transcript; signalled counts the stops.
type gracefulFixture struct {
	pid        int
	id         string
	transcript string
	signalled  int
	tabs       []string
	setStatus  func(status string, at time.Time)
	say        func(role, text string)
}

func newGracefulFixture(t *testing.T) *gracefulFixture {
	t.Helper()
	sessionsFixture(t)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { child.Wait(); close(exited) }()
	g := &gracefulFixture{pid: child.Process.Pid}
	g.id = fmt.Sprintf("id-worker-%d", g.pid)
	projects := t.TempDir()
	os.MkdirAll(filepath.Join(projects, "-code-app"), 0o700)
	g.transcript = filepath.Join(projects, "-code-app", g.id+".jsonl")
	g.setStatus = func(status string, at time.Time) {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"name":"worker","status":%q,"statusUpdatedAt":%d,"messagingSocketPath":"/tmp/none.sock"}`, g.pid, g.id, status, at.UnixMilli())
		os.WriteFile(filepath.Join(GetSessionsDir(), fmt.Sprintf("%d.json", g.pid)), []byte(body), 0o600)
	}
	g.say = func(role, text string) {
		f, err := os.OpenFile(g.transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fmt.Fprintf(f, `{"type":%q,"sessionId":%q,"cwd":"/code/app","timestamp":%q,"message":{"role":%q,"content":[{"type":"text","text":%q}]}}`+"\n",
			role, g.id, time.Now().UTC().Format(time.RFC3339), role, text)
	}
	g.say("user", "do the thing")
	g.say("assistant", "Working on it.")
	g.setStatus("idle", time.Now().Add(-time.Hour))

	oldProjects, oldTTY, oldSig, oldTab, oldWait, oldPoll, oldSend := transcript.GetProjectsDir, proc.TTYOf, proc.SignalPID, CloseTab, CloseWait, GracefulPoll, SendCloseNotice
	t.Cleanup(func() {
		transcript.GetProjectsDir, proc.TTYOf, proc.SignalPID, CloseTab, CloseWait, GracefulPoll, SendCloseNotice = oldProjects, oldTTY, oldSig, oldTab, oldWait, oldPoll, oldSend
		child.Process.Kill()
	})
	transcript.GetProjectsDir = func() string { return projects }
	proc.TTYOf = func(int) string { return "/dev/ttys050" }
	CloseWait, GracefulPoll = 2*time.Second, 10*time.Millisecond
	CloseTab = func(tty string) bool { g.tabs = append(g.tabs, tty); return true }
	proc.SignalPID = func(p int, sig syscall.Signal) error {
		g.signalled++
		err := syscall.Kill(p, sig)
		<-exited // reaped, so PIDAlive sees it gone
		return err
	}
	SendCloseNotice = func(int, SessionFile, string) error { return errors.New("no notice stubbed") }
	return g
}

func TestGracefulCloseWaitsForTheSessionToWrapUp(t *testing.T) {
	g := newGracefulFixture(t)
	var notice string
	SendCloseNotice = func(pid int, f SessionFile, text string) error {
		if pid != g.pid || f.SessionID != g.id {
			t.Errorf("sent to pid %d, session %s", pid, f.SessionID)
		}
		notice = text
		// It reads the notice, wraps up, says what's left and goes idle.
		g.say("user", "<cross-session-message from-name=\"ccs close\">\n"+CloseNote+text+"\n</cross-session-message>")
		g.say("assistant", "Committed the fix; the docs are left undone.")
		g.setStatus("idle", time.Now().Add(time.Second))
		return nil
	}
	msg, err := CloseGracefully("worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"closed worker", "it went idle", "Its last message:\nCommitted the fix; the docs are left undone."} {
		if !strings.Contains(msg, want) {
			t.Errorf("result lacks %q: %s", want, msg)
		}
	}
	if g.signalled != 1 || len(g.tabs) != 1 || g.tabs[0] != "/dev/ttys050" {
		t.Errorf("stopped once and its tab closed: %d, %v", g.signalled, g.tabs)
	}
	for _, want := range []string{"about to be closed", "worktrees", "what is left undone", "1m0s"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice lacks %q: %s", want, notice)
		}
	}
	if strings.Contains(CloseNote, "typed by the user") && !strings.Contains(CloseNote, "Not typed by the user") {
		t.Error("the note never claims the user typed it")
	}
}

func TestGracefulCloseStopsNothingWhenTheNoticeCantBeSent(t *testing.T) {
	g := newGracefulFixture(t)
	SendCloseNotice = func(int, SessionFile, string) error { return ErrNotConnected }
	_, err := CloseGracefully("worker", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "couldn't ask worker") || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Errorf("says the notice failed and nothing was stopped: %v", err)
	}
	if g.signalled != 0 || len(g.tabs) != 0 || !proc.PIDAlive(g.pid) {
		t.Errorf("the session is left running: signalled %d, tabs %v", g.signalled, g.tabs)
	}
}

func TestGracefulCloseStopsAnywayAtTheTimeout(t *testing.T) {
	// The notice never shows (held for approval, say).
	g := newGracefulFixture(t)
	SendCloseNotice = func(int, SessionFile, string) error { return nil }
	msg, err := CloseGracefully("worker", 60*time.Millisecond)
	if err != nil || !strings.Contains(msg, "never showed in its transcript") || !strings.Contains(msg, "closed worker") || strings.Contains(msg, "Its last message") || g.signalled != 1 {
		t.Errorf("stopped at the timeout, saying the notice never arrived: %q, %v, signalled %d", msg, err, g.signalled)
	}

	// It arrived, but the idle it is in is the one from before the notice.
	g = newGracefulFixture(t)
	SendCloseNotice = func(_ int, _ SessionFile, text string) error {
		g.say("user", text)
		return nil
	}
	msg, err = CloseGracefully("worker", 60*time.Millisecond)
	if err != nil || !strings.Contains(msg, "hadn't gone idle") || g.signalled != 1 {
		t.Errorf("an idle from before the notice doesn't count: %q, %v", msg, err)
	}

	// No status time in its file (older Claude Code): seen busy since, then idle.
	g = newGracefulFixture(t)
	SendCloseNotice = func(_ int, _ SessionFile, text string) error {
		g.say("user", text)
		g.setStatus("busy", time.Time{})
		go func() {
			time.Sleep(40 * time.Millisecond)
			g.setStatus("idle", time.Time{})
		}()
		return nil
	}
	msg, err = CloseGracefully("worker", 5*time.Second)
	if err != nil || !strings.Contains(msg, "it went idle") {
		t.Errorf("busy then idle after the notice: %q, %v", msg, err)
	}
}

func TestGracefulCloseRefusals(t *testing.T) {
	sessionsFixture(t)
	if _, err := CloseGracefully("nobody", time.Second); err == nil || !strings.Contains(err.Error(), "no running session") {
		t.Errorf("unknown name: %v", err)
	}
	// A Codex session ccs launched: found by its launch folder, no session file.
	old, oldSig := proc.PsAll, proc.SignalPID
	t.Cleanup(func() { proc.PsAll, proc.SignalPID = old, oldSig })
	proc.PsAll = func() (string, error) {
		return "  101 ttys001 /bin/codex --add-dir " + filepath.Join(state.LaunchDir(), "cx") + " do it\n", nil
	}
	proc.SignalPID = func(int, syscall.Signal) error { t.Error("nothing is stopped"); return nil }
	if _, err := CloseGracefully("cx", time.Second); err == nil || !strings.Contains(err.Error(), "Codex") || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Errorf("a Codex session can't be asked: %v", err)
	}
}

func TestRunCloseArguments(t *testing.T) {
	sessionsFixture(t)
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "usage: ccs close"},
		{[]string{"--graceful"}, 2, "usage: ccs close"},
		{[]string{"a", "b"}, 2, "usage: ccs close"},
		{[]string{"--nope", "a"}, 2, "nope"},
		{[]string{"ghost"}, 1, "no running session named ghost"},
		{[]string{"--graceful", "ghost"}, 1, "no running session named ghost"},
		{[]string{"ghost", "--graceful", "--timeout", "5s"}, 1, "no running session named ghost"},
	} {
		var out, errs strings.Builder
		if code := RunClose(c.args, &out, &errs); code != c.code || !strings.Contains(errs.String(), c.want) {
			t.Errorf("ccs close %v: exit %d, %q", c.args, code, errs.String())
		}
	}
}

package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
)

// An escaped line is never rewritten in the box: a refusal leaves "!!x",
// so the next Enter sends it again rather than running it.
func TestEscapedLineKeepsItsDraft(t *testing.T) {
	m := boxModel(t, "idle", t.TempDir())
	m.sending = true // the last message still on its way: refused
	m, _ = enter(m, "!!touch x")
	if m.chatInput.Value() != "!!touch x" || m.run != nil {
		t.Fatalf("after a refusal: box %q run %v", m.chatInput.Value(), m.run)
	}
	m.sending = false
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.pending["live"] != "!touch x" || m.run != nil {
		t.Fatalf("the retry sends it: pending %q run %v", m.pending["live"], m.run)
	}
	// A failure gives back the line as typed, escape included.
	m = update(m, sendDoneMsg{id: "live", note: "✗", err: fmt.Errorf("%w: test", sessions.ErrUnsent), text: "!!touch x"})
	if m.chatInput.Value() != "!!touch x" {
		t.Errorf("given back %q", m.chatInput.Value())
	}
}

// With no socket, a message starting / or ! is never typed: Claude Code
// would run it.
func TestCommandLikeMessagesAreNeverTyped(t *testing.T) {
	chatModel(t)
	pid := os.Getpid()
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(`{"sessionId":"live","messagingSocketPath":"/tmp/ccs-no-such.sock"}`), 0o600)
	log := fakeTerminal(t)
	for _, text := range []string{"/compact", "!rm -rf x", "  /model"} {
		note, err := deliverMessage(pid, "live", "busy", text, true)
		if err == nil || !strings.Contains(note, "would run as a command") {
			t.Errorf("%q: note %q err %v", text, note, err)
		}
	}
	if got, _ := os.ReadFile(log); len(got) != 0 {
		t.Errorf("typed: %s", got)
	}
}

// The last checks come after the terminal is found, just before the keys.
func TestSlashChecksAgainAfterFindingTheTerminal(t *testing.T) {
	for _, c := range []struct {
		name, want string
		change     func(file string)
	}{
		{"turned busy", "it's busy", func(file string) {
			os.WriteFile(file, []byte(`{"sessionId":"live","status":"busy"}`), 0o600)
		}},
		{"another session", "session has changed", func(file string) {
			os.WriteFile(file, []byte(`{"sessionId":"other","status":"idle"}`), 0o600)
		}},
		{"a prompt held", "ccs holds a question", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := boxModel(t, "idle", t.TempDir())
			file := filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", os.Getpid()))
			oldFind, oldSend, oldHeld := sessions.FindTerminal, sessions.SendKeys, heldFor
			sessions.FindTerminal = func(int) sessions.Terminal {
				if c.change != nil {
					c.change(file) // while ccs was looking for the terminal
				}
				return sessions.Terminal{Kind: "tmux pane", Target: "%1"}
			}
			typed := false
			sessions.SendKeys = func(sessions.Terminal, string, bool) error { typed = true; return nil }
			heldFor = func(string) bool { return c.change == nil }
			t.Cleanup(func() { sessions.FindTerminal, sessions.SendKeys, heldFor = oldFind, oldSend, oldHeld })
			m, cmd := enter(m, "/compact")
			m = update(m, work(t, cmd))
			if typed || !strings.Contains(m.sendNote["live"], c.want) || m.chatInput.Value() != "/compact" {
				t.Errorf("typed=%v note %q box %q", typed, m.sendNote["live"], m.chatInput.Value())
			}
		})
	}
}

// Quitting (or restarting into an update) stops a running command and what
// it started; automatic updates wait while one runs.
func TestQuitStopsARunningCommand(t *testing.T) {
	dir := t.TempDir()
	m := boxModel(t, "idle", dir)
	m, cmd := enter(m, "!sleep 30 & echo $! > child; wait")
	done := make(chan tea.Msg, 1)
	go func() { done <- work(t, cmd) }()
	for range 100 {
		if _, err := os.Stat(filepath.Join(dir, "child")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !runActive() || m.awayForUpdate() {
		t.Fatal("a running command keeps automatic updates waiting")
	}
	start := time.Now()
	stopActiveRun(3 * time.Second)
	if runActive() || time.Since(start) > 3*time.Second {
		t.Errorf("still running after %s", time.Since(start))
	}
	<-done
	if pidAlive(t, filepath.Join(dir, "child")) {
		t.Error("its child outlived ccs")
	}
	stopActiveRun(time.Second) // nothing running: returns at once
}

// A shell that puts a command in the background and exits takes it with it.
func TestRunShellKillsWhatTheShellLeft(t *testing.T) {
	dir := t.TempDir()
	res, err := proc.RunShell(context.Background(), dir, "/bin/sh", "sleep 30 & echo $! > child", time.Minute, 1000)
	if err != nil || res.Code != 0 {
		t.Fatalf("code %d err %v", res.Code, err)
	}
	if pidAlive(t, filepath.Join(dir, "child")) {
		t.Error("the backgrounded child is left running")
	}
}

func TestCommandOutputIsCleanedStrictly(t *testing.T) {
	in := []byte("a\u009b2Jb\u009d8;;http://x\u0007c\xff\xfed\re\x1b[31mf\x1b[0m")
	got := cleanOutput(in)
	if strings.ContainsFunc(got, func(r rune) bool { return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0) }) || !utf8.ValidString(got) {
		t.Errorf("controls left: %q", got)
	}
	if !strings.Contains(got, "ef") || !strings.Contains(got, "�") {
		t.Errorf("cleaned %q", got)
	}
	// The cap never splits a character. (A clean HOME: a login shell reads the profile.)
	t.Setenv("HOME", t.TempDir())
	res, err := proc.RunShell(context.Background(), t.TempDir(), "/bin/sh", "printf 'aé%.0s' $(seq 1 100)", time.Minute, 50)
	if err != nil || !res.Cut || !utf8.Valid(res.Out) || len(res.Out) != 49 { // 16 × "aé" (3 bytes) + "a", the split é dropped
		t.Errorf("cut output %q (%d bytes) valid=%v err %v", res.Out, len(res.Out), utf8.Valid(res.Out), err)
	}
}

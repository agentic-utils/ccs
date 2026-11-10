package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// restart_session returns once the new tab is open, not when the resumed
// session registers, and passes force and the timeout through.
func TestRestartSessionReturnsOnceTheTabIsOpen(t *testing.T) {
	old := restartSession
	t.Cleanup(func() { restartSession = old })
	registered := make(chan struct{})
	var got sessions.RestartOpts
	restartSession = func(o sessions.RestartOpts) (string, error) {
		got = o
		o.OnTab("/dev/ttys042")
		<-registered // the resumed session takes its time to register
		return "restarted " + o.Name, nil
	}
	done := make(chan string, 1)
	go func() {
		text, err := Call("restart_session", json.RawMessage(`{"name":"worker","force":true,"timeout_seconds":30}`), nil)
		if err != nil {
			t.Error(err)
		}
		done <- text
	}()
	select {
	case text := <-done:
		if !strings.Contains(text, "worker") || !strings.Contains(text, "/dev/ttys042") {
			t.Errorf("says it stopped and where the new tab is: %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waited for the session to register instead of returning when its tab opened")
	}
	close(registered)
	if got.Name != "worker" || !got.Force || got.Timeout != 30*time.Second {
		t.Errorf("passed through: %+v", got)
	}
}

// A restart refused (mid-turn) or one that ends before a tab opens (restarting
// itself) returns what restart said.
func TestRestartSessionRefusalComesBack(t *testing.T) {
	old := restartSession
	t.Cleanup(func() { restartSession = old })
	restartSession = func(o sessions.RestartOpts) (string, error) {
		if o.Timeout != 2*time.Minute || o.Force {
			t.Errorf("defaults: %+v", o)
		}
		return "", errors.New("worker is mid-turn")
	}
	if _, err := Call("restart_session", json.RawMessage(`{"name":"worker"}`), nil); err == nil || !strings.Contains(err.Error(), "mid-turn") {
		t.Errorf("the refusal: %v", err)
	}
	restartSession = func(o sessions.RestartOpts) (string, error) { return "worker restarts once this turn ends", nil }
	if text, err := Call("restart_session", json.RawMessage(`{"name":"worker"}`), nil); err != nil || !strings.Contains(text, "once this turn ends") {
		t.Errorf("restarting itself: %q %v", text, err)
	}
}

package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// switch_session returns once the new tab is open and passes its arguments
// through; a refusal comes back as an error; to is required.
func TestSwitchSessionReturnsOnceTheTabIsOpen(t *testing.T) {
	old := switchSession
	t.Cleanup(func() { switchSession = old })
	registered := make(chan struct{})
	var got sessions.SwitchOpts
	switchSession = func(o sessions.SwitchOpts) (string, error) {
		got = o
		o.OnTab("/dev/ttys043")
		<-registered
		return "switched " + o.Name, nil
	}
	done := make(chan string, 1)
	go func() {
		text, err := Call("switch_session", json.RawMessage(`{"name":"worker","to":"codex","session_id":"s1","force":true,"timeout_seconds":30}`), nil)
		if err != nil {
			t.Error(err)
		}
		done <- text
	}()
	select {
	case text := <-done:
		if !strings.Contains(text, "worker") || !strings.Contains(text, "/dev/ttys043") {
			t.Errorf("says where the new tab is: %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waited for the session to register")
	}
	close(registered)
	if got.Name != "worker" || got.To != "codex" || got.ID != "s1" || !got.Force || got.Timeout != 30*time.Second {
		t.Errorf("passed through: %+v", got)
	}

	switchSession = func(sessions.SwitchOpts) (string, error) { return "", errors.New("worker is already a Codex session") }
	if _, err := Call("switch_session", json.RawMessage(`{"name":"worker","to":"codex"}`), nil); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("a refusal comes back: %v", err)
	}
	if _, err := Call("switch_session", json.RawMessage(`{"name":"worker"}`), nil); err == nil || !strings.Contains(err.Error(), "to (claude") {
		t.Errorf("to is required: %v", err)
	}
}

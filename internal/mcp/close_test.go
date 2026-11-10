package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCloseSessionGraceful(t *testing.T) {
	old := closeGracefully
	t.Cleanup(func() { closeGracefully = old })
	var name string
	var timeout time.Duration
	closeGracefully = func(n string, d time.Duration) (string, error) {
		name, timeout = n, d
		return "closed " + n + ": it went idle", nil
	}
	text, err := Call("close_session", json.RawMessage(`{"name":"worker","graceful":true}`), nil)
	if err != nil || text != "closed worker: it went idle" || name != "worker" || timeout != 2*time.Minute {
		t.Errorf("graceful, two minutes by default: %q %v, %s %s", text, err, name, timeout)
	}
	Call("close_session", json.RawMessage(`{"name":"worker","graceful":true,"timeout_seconds":30}`), nil)
	if timeout != 30*time.Second {
		t.Errorf("its timeout: %s", timeout)
	}
	Call("close_session", json.RawMessage(`{"name":"worker","graceful":true,"timeout_seconds":99999}`), nil)
	if timeout != 15*time.Minute {
		t.Errorf("at most 15 minutes: %s", timeout)
	}
	// Without graceful it closes at once, as before: here, no such session.
	name = ""
	if _, err := Call("close_session", json.RawMessage(`{"name":"nobody"}`), nil); err == nil || !strings.Contains(err.Error(), "no running session named nobody") || name != "" {
		t.Errorf("plain close is unchanged: %v", err)
	}
}

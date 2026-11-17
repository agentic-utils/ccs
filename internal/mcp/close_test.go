package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCloseSessionGraceful(t *testing.T) {
	old := closeGracefully
	t.Cleanup(func() { closeGracefully = old })
	var name string
	var timeout time.Duration
	closeGracefully = func(n string, d time.Duration, _ func(int)) (string, error) {
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
	// The wait outlives a tool call's deadline: once the notice is sent the
	// call answers with the pid, and the close finishes in the background.
	release, finished := make(chan struct{}), make(chan struct{})
	closeGracefully = func(n string, d time.Duration, onSent func(int)) (string, error) {
		onSent(4242)
		<-release
		defer close(finished)
		return "closed " + n, nil
	}
	start := time.Now()
	text, err = Call("close_session", json.RawMessage(`{"name":"worker","graceful":true,"timeout_seconds":600}`), nil)
	if err != nil || !strings.Contains(text, "asked worker (pid 4242) to wrap up") || !strings.Contains(text, "after 10m0s") || time.Since(start) > time.Second {
		t.Errorf("answers once the notice is sent: %q %v after %s", text, err, time.Since(start))
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Error("the close never finished in the background")
	}
	// A notice that can't be sent still fails the call.
	closeGracefully = func(string, time.Duration, func(int)) (string, error) {
		return "", errors.New("couldn't ask worker to wrap up: nothing was stopped")
	}
	if _, err := Call("close_session", json.RawMessage(`{"name":"worker","graceful":true}`), nil); err == nil || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Errorf("an unsent notice is an error: %v", err)
	}

	// Without graceful it closes at once, as before: here, no such session.
	name = ""
	if _, err := Call("close_session", json.RawMessage(`{"name":"nobody"}`), nil); err == nil || !strings.Contains(err.Error(), "no running session named nobody") || name != "" {
		t.Errorf("plain close is unchanged: %v", err)
	}
}

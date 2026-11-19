package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// close_session with handover goes through CloseHandover (graceful never
// runs), answers once the notice is sent, passes a refusal straight back,
// and answers within the cap when nothing has happened by then.
func TestCloseSessionHandover(t *testing.T) {
	oldH, oldG, oldCap := closeHandover, closeGracefully, sessionCallCap
	t.Cleanup(func() { closeHandover, closeGracefully, sessionCallCap = oldH, oldG, oldCap })
	closeGracefully = func(string, string, time.Duration, func(int)) (string, error) {
		t.Error("handover never runs the plain graceful close")
		return "", nil
	}
	var name, id string
	var timeout time.Duration
	release, finished := make(chan struct{}), make(chan struct{})
	closeHandover = func(n, i string, d time.Duration, onSent func(int)) (string, error) {
		name, id, timeout = n, i, d
		onSent(4242)
		<-release
		defer close(finished)
		return "closed " + n + "\nhanded over to lead", nil
	}
	text, err := Call("close_session", json.RawMessage(`{"name":"worker","handover":true,"session_id":"s-1","timeout_seconds":60}`), nil)
	if err != nil || !strings.Contains(text, "asked worker (pid 4242) to hand its leftovers to its parent") || name != "worker" || id != "s-1" || timeout != time.Minute {
		t.Errorf("answers once the notice is sent: %q %v (%s %s %s)", text, err, name, id, timeout)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Error("the handover never finished in the background")
	}

	closeHandover = func(string, string, time.Duration, func(int)) (string, error) {
		return "", errors.New("worker has no running parent: use ^X K to close it, or adopt it first")
	}
	if _, err := Call("close_session", json.RawMessage(`{"name":"worker","handover":true}`), nil); err == nil || !strings.Contains(err.Error(), "no running parent") {
		t.Errorf("a refusal comes straight back: %v", err)
	}

	// Neither sent nor refused within the cap: it says so and carries on.
	sessionCallCap = 50 * time.Millisecond
	release, finished = make(chan struct{}), make(chan struct{})
	closeHandover = func(n, _ string, _ time.Duration, _ func(int)) (string, error) {
		<-release
		defer close(finished)
		return "closed " + n, nil
	}
	start := time.Now()
	text, err = Call("close_session", json.RawMessage(`{"name":"worker","handover":true}`), nil)
	if err != nil || text != "worker is still handing over: check with list_sessions in a minute (the outcome goes to ccs's activity log)" || time.Since(start) > time.Second {
		t.Errorf("answered within the cap: %q %v after %s", text, err, time.Since(start))
	}
	close(release)
	<-finished
}

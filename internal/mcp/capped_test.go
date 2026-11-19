package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// stubSessionCalls makes launch_session and close_session take d, and caps
// a reply at limit.
func stubSessionCalls(t *testing.T, d, limit time.Duration) (launched chan string) {
	t.Helper()
	oldL, oldC, oldCap := launchFn, closeFn, sessionCallCap
	launched = make(chan string, 4)
	// Every call reports when it ends, and the cleanup waits for each one
	// begun: a call answered early still finishes (and logs) before the stubs
	// go back, so it never outlives its test.
	var mu sync.Mutex
	begun := 0
	ended := make(chan struct{}, 16)
	started := func() {
		mu.Lock()
		begun++
		mu.Unlock()
	}
	launchFn = func(o sessions.LaunchOpts) (sessions.Launched, error) {
		started()
		defer func() { ended <- struct{}{} }()
		time.Sleep(d)
		launched <- o.Name
		return sessions.Launched{PID: 42, TTY: "/dev/ttys001"}, nil
	}
	closeFn = func(name, id string) (string, error) {
		started()
		defer func() { ended <- struct{}{} }()
		time.Sleep(d)
		return "closed " + name, nil
	}
	sessionCallCap = limit
	t.Cleanup(func() {
		mu.Lock()
		n := begun
		mu.Unlock()
		for range n {
			<-ended
		}
		launchFn, closeFn, sessionCallCap = oldL, oldC, oldCap
	})
	return launched
}

// A slow launch answers "still starting" within the cap and carries on; a
// fast one answers as before.
func TestSlowLaunchAnswersWithinTheCap(t *testing.T) {
	// Each step stubs afresh in its own subtest, whose cleanup waits for the
	// calls it began before the next step stubs again.
	t.Run("slow launch", func(t *testing.T) {
		launched := stubSessionCalls(t, 500*time.Millisecond, 50*time.Millisecond)
		start := time.Now()
		text, err := Call("launch_session", json.RawMessage(`{"name":"slow","brief":"x","cwd":"/tmp"}`), nil)
		if err != nil || !strings.Contains(text, "slow is still starting") || !strings.Contains(text, "list_sessions") {
			t.Fatalf("still starting: %q %v", text, err)
		}
		if took := time.Since(start); took > 400*time.Millisecond {
			t.Errorf("answered within the cap: %s", took)
		}
		select {
		case name := <-launched:
			if name != "slow" {
				t.Errorf("launched %s", name)
			}
		case <-time.After(2 * time.Second):
			t.Error("the launch carried on after the reply")
		}
	})
	t.Run("fast launch", func(t *testing.T) {
		stubSessionCalls(t, 0, time.Second)
		text, err := Call("launch_session", json.RawMessage(`{"name":"quick","brief":"x","cwd":"/tmp"}`), nil)
		if err != nil || strings.Contains(text, "still starting") || !strings.Contains(text, "quick") {
			t.Errorf("a fast launch answers with its result: %q %v", text, err)
		}
	})
	t.Run("slow close", func(t *testing.T) {
		stubSessionCalls(t, 500*time.Millisecond, 50*time.Millisecond)
		if text, err := Call("close_session", json.RawMessage(`{"name":"w"}`), nil); err != nil || !strings.Contains(text, "w is still closing") {
			t.Errorf("a slow close answers within the cap: %q %v", text, err)
		}
	})
	t.Run("fast close", func(t *testing.T) {
		stubSessionCalls(t, 0, time.Second)
		if text, err := Call("close_session", json.RawMessage(`{"name":"w"}`), nil); err != nil || text != "closed w" {
			t.Errorf("a fast close answers with its result: %q %v", text, err)
		}
	})
}

// A slow launch doesn't hold up the next request on the same server.
func TestLaterCallsArentBlockedBehindASlowLaunch(t *testing.T) {
	stubSessionCalls(t, 2*time.Second, 50*time.Millisecond)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go RunMCP(inR, outW, nil)
	t.Cleanup(func() { inW.Close() })
	replies := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			replies <- sc.Text()
		}
	}()
	start := time.Now()
	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"launch_session","arguments":{"name":"slow","brief":"x","cwd":"/tmp"}}}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n")
	for i := 1; i <= 2; i++ {
		select {
		case r := <-replies:
			if i == 1 && !strings.Contains(r, "still starting") {
				t.Errorf("first reply: %s", r)
			}
			if i == 2 && !strings.Contains(r, `"id":2`) {
				t.Errorf("second reply: %s", r)
			}
		case <-time.After(time.Second):
			t.Fatalf("reply %d held up behind the launch", i)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("both answered well before the launch finished: %s", took)
	}
}

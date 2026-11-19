package mcp

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// ownFile stubs the hook's own session file: s-1 in status since at, both
// changeable while the hook runs.
type ownFile struct {
	mu     sync.Mutex
	status string
	at     int64
}

func (f *ownFile) set(status string, at int64) {
	f.mu.Lock()
	f.status, f.at = status, at
	f.mu.Unlock()
}

func (f *ownFile) file() sessions.SessionFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sessions.SessionFile{SessionID: "s-1", Status: f.status, StatusUpdatedAt: f.at}
}

func stubOwnFile(t *testing.T, status string, at time.Time) *ownFile {
	t.Helper()
	f := &ownFile{status: status, at: at.UnixMilli()}
	testOwnFile.Store(f)
	t.Cleanup(func() { testOwnFile.Store(nil) })
	return f
}

// stubHookWait makes a hook give up d after it starts.
func stubHookWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := hookMaxWait
	hookMaxWait = d
	t.Cleanup(func() { hookMaxWait = old })
}

// handedOff holds the ask prompt in a holder, then hands it off as a
// restart does: the hook is left redialling with no holder.
func handedOff(t *testing.T) <-chan string {
	t.Helper()
	a := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, askHook)
	waitList(t, a)
	a.Handoff("ccs restarting")
	return out
}

func answerBlue(t *testing.T, h *Holder, out <-chan string) {
	t.Helper()
	p := waitList(t, h)[0]
	if _, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Blue"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	d := decisionOf(t, hookOutput(t, out))
	if d["behavior"] != "allow" || d["updatedInput"].(map[string]any)["answers"].(map[string]any)["Which colour?"] != "Blue" {
		t.Errorf("decision %v", d)
	}
}

// A restart keeps a held question: the old holder hands it back, the hook
// dials the new one, which holds it afresh and answers it.
func TestHeldPromptSurvivesARestart(t *testing.T) {
	stubOwnFile(t, "waiting", time.Now())
	out := handedOff(t)
	if log := activityLog(t); !strings.Contains(log, "handed s-1's AskUserQuestion prompt back for a restart") {
		t.Errorf("the log says it was handed back:\n%s", log)
	}
	b := tuiHolder(t, focusedNow(), time.Minute)
	answerBlue(t, b, out)
}

// A holder that crashes (the connection drops with no reply) is treated
// as a restart: the hook dials again.
func TestHookReconnectsAfterACrash(t *testing.T) {
	stubOwnFile(t, "waiting", time.Now())
	stop, err := listenUnix(hookSockPath(), func(c net.Conn) {
		sc := bufio.NewScanner(c)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		var hello hookHello
		if sc.Scan() && json.Unmarshal(sc.Bytes(), &hello) == nil {
			json.NewEncoder(c).Encode(hookReply{Nonce: hello.Nonce, Held: true})
		}
		c.Close() // gone, no reply
	})
	if err != nil {
		t.Fatal(err)
	}
	out := runHook(t, askHook)
	time.Sleep(100 * time.Millisecond)
	stop()
	b := tuiHolder(t, focusedNow(), time.Minute)
	answerBlue(t, b, out)
}

// A final no-decision ends the hook: it never dials again.
func TestFinalNoDecisionDoesntRedial(t *testing.T) {
	stubOwnFile(t, "waiting", time.Now())
	var dials atomic.Int32
	stop, err := listenUnix(hookSockPath(), func(c net.Conn) {
		defer c.Close()
		dials.Add(1)
		sc := bufio.NewScanner(c)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		var hello hookHello
		if sc.Scan() && json.Unmarshal(sc.Bytes(), &hello) == nil {
			json.NewEncoder(c).Encode(hookReply{Nonce: hello.Nonce, Held: true})
			json.NewEncoder(c).Encode(hookReply{Nonce: hello.Nonce})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if got := hookOutput(t, runHook(t, askHook)); got != "" {
		t.Errorf("no decision prints nothing: %q", got)
	}
	time.Sleep(5 * hookRedial)
	if n := dials.Load(); n != 1 {
		t.Errorf("dialled %d times", n)
	}
}

// The hook gives up maxHookWait after it started, redials included, and
// prints nothing. The clock is stubbed: a sleep moves it on.
func TestHookRedialDeadline(t *testing.T) {
	stubOwnFile(t, "waiting", time.Now())
	testClock.Store(time.Now().UnixNano())
	t.Cleanup(func() { testClock.Store(0) })
	stubHookWait(t, time.Second) // 50 redials of hook time
	start := testClock.Load()
	if got := hookOutput(t, handedOff(t)); got != "" {
		t.Errorf("the deadline prints nothing: %q", got)
	}
	if d := time.Duration(testClock.Load() - start); d < time.Second || d > 3*time.Second {
		t.Errorf("gave up after %s of hook time, not at its deadline", d)
	}
}

// Every redial checks the socket and its listener again: one another user
// could use, or one another user listens on, is refused until the
// deadline, and the hook prints nothing. A proper holder is dialled.
func TestRedialRechecksTheSocket(t *testing.T) {
	serveTo := func(c net.Conn) {
		defer c.Close()
		sc := bufio.NewScanner(c)
		var hello hookHello
		if sc.Scan() && json.Unmarshal(sc.Bytes(), &hello) == nil {
			json.NewEncoder(c).Encode(hookReply{Nonce: hello.Nonce, Held: true})
			json.NewEncoder(c).Encode(hookReply{Nonce: hello.Nonce, Behavior: "allow"})
		}
	}
	for _, c := range []struct {
		name string
		bad  func(t *testing.T)
	}{
		{"a 0666 socket", func(t *testing.T) {
			if err := os.Chmod(hookSockPath(), 0o666); err != nil {
				t.Fatal(err)
			}
		}},
		{"another peer", func(t *testing.T) {
			otherPeer.Store(true)
			t.Cleanup(func() { otherPeer.Store(false) })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubOwnFile(t, "waiting", time.Now())
			stubHookWait(t, 1500*time.Millisecond)
			out := handedOff(t)
			var dialled atomic.Int32
			stop, err := listenUnix(hookSockPath(), func(conn net.Conn) { dialled.Add(1); serveTo(conn) })
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			c.bad(t)
			if got := hookOutput(t, out); got != "" {
				t.Errorf("refused until the deadline, printing nothing: %q", got)
			}
		})
		t.Run(c.name+", then a proper holder", func(t *testing.T) {
			stubOwnFile(t, "waiting", time.Now())
			out := handedOff(t)
			stop, err := listenUnix(hookSockPath(), serveTo)
			if err != nil {
				t.Fatal(err)
			}
			c.bad(t)
			time.Sleep(5 * hookRedial)
			select {
			case got := <-out:
				t.Fatalf("the bad socket was used: %q", got)
			default:
			}
			stop()
			b := tuiHolder(t, focusedNow(), time.Minute)
			if os.Chmod(hookSockPath(), 0o600) != nil {
				t.Fatal("no socket")
			}
			otherPeer.Store(false)
			answerBlue(t, b, out)
		})
	}
}

// Answered in its tab while it redials: the hook sees its wait end in its
// session file and stops at once, printing nothing.
func TestTabAnswerDuringRedialEndsTheHook(t *testing.T) {
	f := stubOwnFile(t, "waiting", time.Now())
	out := handedOff(t)
	time.Sleep(3 * hookRedial)
	f.set("busy", time.Now().UnixMilli())
	start := time.Now()
	if got := hookOutput(t, out); got != "" || time.Since(start) > time.Second {
		t.Errorf("%q after %s", got, time.Since(start))
	}
}

// A hook that re-registers after its wait ended (the new holder's scan
// shows the session on), though its own file read hasn't caught up, is
// let go at once and never listed.
func TestReRegistrationAfterItsWaitIsNeverListed(t *testing.T) {
	w := time.Now()
	stubOwnFile(t, "waiting", w)
	out := handedOff(t)
	old := liveSessions
	liveSessions = func() map[string]sessions.LiveSession { return waitingAt("busy", w.Add(time.Second)) }
	t.Cleanup(func() { liveSessions = old })
	b := tuiHolder(t, focusedNow(), time.Minute)
	deadline := time.After(2 * time.Second)
	for {
		if len(b.List()) > 0 {
			t.Fatal("a copy whose wait was over was listed")
		}
		select {
		case got := <-out:
			if got != "" {
				t.Errorf("no decision: %q", got)
			}
			return
		case <-deadline:
			t.Fatal("the hook was never let go")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// A hook from before retries reads the hand-back as it always read a
// release: a reply with no decision, so its tab asks.
func TestOldHookReadsAHandBackAsNoDecision(t *testing.T) {
	a := tuiHolder(t, focusedNow(), time.Minute)
	conn, err := net.Dial("unix", hookSockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var req hookRequest
	json.Unmarshal([]byte(askHook), &req)
	json.NewEncoder(conn).Encode(hookHello{Nonce: "n1", Request: req})
	sc := bufio.NewScanner(conn)
	sc.Scan() // the acknowledgement
	waitList(t, a)
	a.Handoff("ccs restarting")
	if !sc.Scan() {
		t.Fatal("no reply")
	}
	// The old hookReply: no Retry field.
	var old struct {
		Nonce    string            `json:"nonce"`
		Held     bool              `json:"held,omitempty"`
		Behavior string            `json:"behavior,omitempty"`
		Answers  map[string]string `json:"answers,omitempty"`
		Message  string            `json:"message,omitempty"`
		Other    []string          `json:"other,omitempty"`
		Notes    map[string]string `json:"notes,omitempty"`
	}
	if err := json.Unmarshal(sc.Bytes(), &old); err != nil || old.Nonce != "n1" {
		t.Fatalf("%s %v", sc.Bytes(), err)
	}
	r := hookReply{Nonce: old.Nonce, Held: old.Held, Behavior: old.Behavior, Answers: old.Answers, Message: old.Message, Other: old.Other, Notes: old.Notes}
	if d := hookDecision(req, r); d != nil {
		t.Errorf("an old hook would print %v", d)
	}
}

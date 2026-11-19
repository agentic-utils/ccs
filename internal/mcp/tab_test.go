package mcp

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// waitingAt is a scan with session s-1 in the given status since at.
func waitingAt(status string, at time.Time) map[string]sessions.LiveSession {
	return map[string]sessions.LiveSession{"s-1": {PID: 42, Stat: sessions.SessionStat{Status: status, Since: at}}}
}

// Claude Code shows the menu in its tab too and doesn't stop the hook when
// it's answered there: ccs drops its copy on the next scan that shows the
// session no longer waiting on it, with no decision, and logs why.
func TestHeldPromptAnsweredInItsTab(t *testing.T) {
	since := time.Now() // the prompt's own wait, stamped as it arrives
	for _, c := range []struct {
		name  string
		after map[string]sessions.LiveSession
		scans int
	}{
		{"busy", waitingAt("busy", time.Now()), 1},
		{"idle", waitingAt("idle", time.Now()), 1},
		{"a new wait", waitingAt("waiting", since.Add(time.Second)), 1},
		{"the session gone", map[string]sessions.LiveSession{}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubPID(t, c.name != "the session gone") // its claude exited
			h := tuiHolder(t, focusedNow(), time.Minute)
			out := runHook(t, askHook)
			p := waitList(t, h)[0]
			h.Reconcile(waitingAt("waiting", since)) // first seen waiting
			if len(h.List()) != 1 {
				t.Fatal("still waiting: kept")
			}
			for range c.scans {
				h.Reconcile(c.after)
			}
			if len(h.List()) != 0 {
				t.Fatal("dropped from ccs at once")
			}
			if got := hookOutput(t, out); got != "" {
				t.Errorf("the hook prints nothing: %q", got)
			}
			if _, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Red"}}, nil, nil); err == nil {
				t.Error("an answer after that is refused")
			}
			if !strings.Contains(activityLog(t), "answered in its tab") && c.name != "the session gone" {
				t.Error("logged as answered in its tab")
			}
		})
	}
}

// A session that hasn't shown waiting yet (its status is written a moment
// after the hook starts) isn't taken as answered within tabGrace.
func TestHeldPromptBeforeItsSessionWaits(t *testing.T) {
	h := tuiHolder(t, focusedNow(), time.Minute)
	runHook(t, askHook)
	waitList(t, h)
	h.Reconcile(waitingAt("busy", time.Now()))
	if len(h.List()) != 1 {
		t.Error("busy before it was ever seen waiting, within the grace: kept")
	}
	h.Reconcile(waitingAt("", time.Time{}))
	if len(h.List()) != 1 {
		t.Error("an unread status changes nothing")
	}
}

// Just before an answer is sent, the session is checked again: one its tab
// settled since is refused, and nothing reaches the hook.
func TestAnswerAfterTheTabAnswered(t *testing.T) {
	h := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, askHook)
	p := waitList(t, h)[0]
	since := time.Now() // the prompt's own wait, stamped as it arrives
	h.Reconcile(waitingAt("waiting", since))
	old := liveSessions
	liveSessions = func() map[string]sessions.LiveSession { return waitingAt("busy", time.Now()) }
	t.Cleanup(func() { liveSessions = old })
	if _, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Red"}}, nil, nil); err == nil || !strings.Contains(err.Error(), "answered in its tab already") {
		t.Fatalf("refused: %v", err)
	}
	if got := hookOutput(t, out); got != "" {
		t.Errorf("the hook prints nothing: %q", got)
	}
}

// An answer already taken from the book (being delivered) isn't dropped by
// a scan that comes in between: it reaches the hook.
func TestReconcileSparesAnAnswerOnItsWay(t *testing.T) {
	h := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, askHook)
	p := waitList(t, h)[0]
	since := time.Now() // the prompt's own wait, stamped as it arrives
	h.Reconcile(waitingAt("waiting", since))
	hp := h.book.take(p.ID) // what an answer does before it's sent
	h.Reconcile(waitingAt("busy", time.Now()))
	hp.reply <- hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Red"}}
	if d := decisionOf(t, hookOutput(t, out)); d["behavior"] != "allow" {
		t.Errorf("the answer went through: %v", d)
	}
}

// ccs serve's holder does the same: its loop drops a prompt answered in its
// tab, and the page refuses an answer to it.
func TestServeDropsAPromptAnsweredInItsTab(t *testing.T) {
	old := reconcilePeriod
	reconcilePeriod = 10 * time.Millisecond
	t.Cleanup(func() { reconcilePeriod = old })
	s := hookServer(t, time.Minute) // liveSessions: s-1 waiting
	out := runHook(t, askHook)
	p := waitHeld(t, s)
	time.Sleep(50 * time.Millisecond) // a scan sees it waiting
	hookLive.Store(liveFn(func() map[string]sessions.LiveSession { return waitingAt("idle", time.Now()) }))
	for range 200 {
		if len(s.hold.List()) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(s.hold.List()) != 0 {
		t.Fatal("ccs serve dropped it")
	}
	if got := hookOutput(t, out); got != "" {
		t.Errorf("the hook prints nothing: %q", got)
	}
	w := postAct(t, s, url.Values{"action": {"prompt"}, "choice": {"answer"}, "prompt": {p.id}, "id": {"s-1"}, "name": {"worker"}, "q0": {"Red"}, "confirm": {"yes"}})
	if w.Code != http.StatusConflict {
		t.Errorf("an answer after that: %d", w.Code)
	}
}

// stubPID makes every recorded claude pid alive or exited.
func stubPID(t *testing.T, alive bool) {
	t.Helper()
	old := pidAlive
	pidAlive = func(int) bool { return alive }
	t.Cleanup(func() { pidAlive = old })
}

// The measured sequence (Claude Code 2.1.328, two tool calls made together):
// prompt 1 waits; answered in its tab, the status goes busy for about a
// second, then prompt 2 waits with a newer statusUpdatedAt and its own hook.
// Prompt 1's copy goes on the newer wait, even if the scan never caught
// the busy in between; prompt 2's copy, arriving while the file still
// shows prompt 1's older wait, isn't bound to it and so isn't dropped.
func TestParallelPromptsAreAskedOneAtATime(t *testing.T) {
	stubPID(t, true)
	h := tuiHolder(t, focusedNow(), time.Minute)
	runHook(t, askHook)
	p1 := waitList(t, h)[0]
	w1 := time.Now()
	h.Reconcile(waitingAt("waiting", w1))
	if len(h.List()) != 1 {
		t.Fatal("prompt 1 is held while its session waits on it")
	}
	time.Sleep(3 * time.Second) // so prompt 2 arrives well after prompt 1's wait
	out2 := runHook(t, askHook)
	var p2 HeldPrompt
	for range 500 {
		for _, q := range h.List() {
			if q.ID != p1.ID {
				p2 = q
			}
		}
		if p2.ID != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p2.ID == "" {
		t.Fatal("prompt 2 is held")
	}
	// The file hasn't caught up: it still shows prompt 1's wait.
	h.Reconcile(waitingAt("waiting", w1))
	if len(h.List()) != 2 {
		t.Fatalf("an older wait on show drops nothing: %v", h.List())
	}
	// Then prompt 2's own, newer wait: prompt 1 was answered in its tab.
	w2 := time.Now()
	h.Reconcile(waitingAt("waiting", w2))
	if l := h.List(); len(l) != 1 || l[0].ID != p2.ID {
		t.Fatalf("prompt 1 dropped, prompt 2 kept: %v", l)
	}
	h.Reconcile(waitingAt("waiting", w2))
	if len(h.List()) != 1 {
		t.Fatal("its own wait, seen again, keeps prompt 2")
	}
	h.Reconcile(waitingAt("busy", time.Now()))
	if len(h.List()) != 0 {
		t.Fatal("prompt 2 answered in its tab: dropped")
	}
	if got := hookOutput(t, out2); got != "" {
		t.Errorf("the hook prints nothing: %q", got)
	}
}

// A missing or unreadable session file is unknown: the copy stays as long
// as the session's claude process lives, however many scans miss it.
func TestAMissingSessionFileKeepsTheCopy(t *testing.T) {
	stubPID(t, true)
	h := tuiHolder(t, focusedNow(), time.Minute)
	runHook(t, askHook)
	waitList(t, h)
	h.Reconcile(waitingAt("waiting", time.Now()))
	for range 5 {
		h.Reconcile(map[string]sessions.LiveSession{})
	}
	if len(h.List()) != 1 {
		t.Fatal("unreadable while its claude lives: kept")
	}
	stubPID(t, false)
	h.Reconcile(map[string]sessions.LiveSession{})
	if len(h.List()) != 0 {
		t.Fatal("its claude exited: dropped")
	}
}

// A prompt never seen in a scan (no pid known) is kept: nothing says it ended.
func TestAPromptNeverSeenIsKept(t *testing.T) {
	stubPID(t, false)
	h := tuiHolder(t, focusedNow(), time.Minute)
	runHook(t, askHook)
	waitList(t, h)
	for range 3 {
		h.Reconcile(map[string]sessions.LiveSession{})
	}
	if len(h.List()) != 1 {
		t.Fatal("no pid known: kept")
	}
}

// A drop and a release (or the wait running out) at once: the handler
// wakes on the drop rather than waiting for ever for a reply, and the hook
// gets no decision at once. Run under -race.
func TestDropRacesRelease(t *testing.T) {
	stubPID(t, true)
	for range 20 {
		h := tuiHolder(t, focusedNow(), time.Minute)
		out := runHook(t, askHook)
		waitList(t, h)
		h.Reconcile(waitingAt("waiting", time.Now()))
		done := make(chan struct{})
		go func() { h.Reconcile(waitingAt("busy", time.Now())); close(done) }()
		h.ReleaseAll("racing")
		<-done
		select {
		case got := <-out:
			if got != "" {
				t.Fatalf("no decision: %q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the handler hung after a drop raced a release")
		}
		h.Close("done")
	}
}

// A tab answer between the first check and the send: the check made just
// after the prompt is taken refuses it, and the hook gets no decision.
func TestTabAnswerAtTheSameMoment(t *testing.T) {
	stubPID(t, true)
	h := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, askHook)
	p := waitList(t, h)[0]
	h.Reconcile(waitingAt("waiting", time.Now()))
	calls := 0
	old := liveSessions
	liveSessions = func() map[string]sessions.LiveSession {
		calls++
		if calls == 1 {
			return waitingAt("waiting", time.Now().Add(-time.Second)) // the first check: still waiting
		}
		return waitingAt("busy", time.Now()) // the tab answered in between
	}
	t.Cleanup(func() { liveSessions = old })
	if _, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Red"}}, nil, nil); err == nil || !strings.Contains(err.Error(), "answered in its tab already") {
		t.Fatalf("refused at the last check: %v", err)
	}
	if got := hookOutput(t, out); got != "" {
		t.Errorf("the hook prints nothing: %q", got)
	}
}

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
	since := time.Now().Add(-time.Minute)
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
			h := tuiHolder(t, focusedNow(), time.Minute)
			out := runHook(t, askHook)
			p := waitList(t, h)[0]
			h.Reconcile(waitingAt("waiting", since)) // first seen waiting
			if len(h.List()) != 1 {
				t.Fatal("still waiting: kept")
			}
			for i := range c.scans {
				if i == 0 && c.scans > 1 {
					h.Reconcile(c.after)
					if len(h.List()) != 1 {
						t.Fatal("one missed scan isn't the session gone")
					}
					continue
				}
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
	since := time.Now().Add(-time.Minute)
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
	since := time.Now().Add(-time.Minute)
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
	liveSessions = func() map[string]sessions.LiveSession { return waitingAt("idle", time.Now()) }
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

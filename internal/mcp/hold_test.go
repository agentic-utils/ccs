package mcp

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tuiHolder is a TUI-style holder (yields, no key release) that owns the
// hold, focused as focused says.
func tuiHolder(t *testing.T, focused *atomic.Bool, wait time.Duration) *Holder {
	t.Helper()
	h := NewHolder(HoldConfig{Who: "tui", Wait: func() time.Duration { return wait }, MayHold: focused.Load, Yields: true})
	if err := h.Listen(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close("test over") })
	return h
}

func focusedNow() *atomic.Bool {
	var f atomic.Bool
	f.Store(true)
	return &f
}

func waitList(t *testing.T, h *Holder) []HeldPrompt {
	t.Helper()
	for range 500 {
		if ps := h.List(); len(ps) > 0 {
			return ps
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no prompt was held")
	return nil
}

// With no ccs serve, the TUI holds a prompt and answers it by id: the hook
// gets the decision it builds itself.
func TestTUIHolderAnswers(t *testing.T) {
	h := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, askHook)
	p := waitList(t, h)[0]
	if _, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Green"}}, nil); err == nil {
		t.Error("a label that isn't an option is refused")
	}
	if text, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Blue"}}, nil); err != nil || !strings.Contains(text, "Blue") {
		t.Fatalf("%q %v", text, err)
	}
	d := decisionOf(t, hookOutput(t, out))
	if d["behavior"] != "allow" || d["updatedInput"].(map[string]any)["answers"].(map[string]any)["Which colour?"] != "Blue" {
		t.Errorf("decision %v", d)
	}
	if _, err := h.Answer(p.ID, "allow", nil, nil); err == nil {
		t.Error("a second answer to the same prompt")
	}
}

// Not focused: the prompt goes straight to its tab.
func TestTUIHolderUnfocusedLetsGo(t *testing.T) {
	var f atomic.Bool // false: ccs isn't focused
	tuiHolder(t, &f, time.Minute)
	start := time.Now()
	if out := hookOutput(t, runHook(t, bashHook)); out != "" || time.Since(start) > time.Second {
		t.Errorf("unfocused: %q after %s", out, time.Since(start))
	}
}

// Losing focus (ReleaseAll) or the cap lets a held prompt go, undecided.
func TestTUIHolderReleases(t *testing.T) {
	t.Run("focus lost", func(t *testing.T) {
		h := tuiHolder(t, focusedNow(), time.Minute)
		out := runHook(t, bashHook)
		waitList(t, h)
		h.ReleaseAll("ccs lost focus")
		if got := hookOutput(t, out); got != "" {
			t.Errorf("released: %q", got)
		}
	})
	t.Run("the cap", func(t *testing.T) {
		tuiHolder(t, focusedNow(), 50*time.Millisecond)
		if got := hookOutput(t, runHook(t, bashHook)); got != "" {
			t.Errorf("capped: %q", got)
		}
	})
}

// ccs serve takes the hold over from a TUI: the TUI's held prompts go to
// their tabs undecided, and serve binds and holds the next.
func TestServeTakesOverFromTUI(t *testing.T) {
	yielded := make(chan struct{}, 1)
	f := focusedNow()
	tui := NewHolder(HoldConfig{Who: "tui", Wait: func() time.Duration { return time.Minute }, MayHold: f.Load, Yields: true,
		OnYield: func() { yielded <- struct{}{} }})
	if err := tui.Listen(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tui.Close("test over") })
	out := runHook(t, bashHook)
	waitList(t, tui)

	s := hookServer(t, time.Minute) // listenHooks: Listen(true), asks the TUI to yield
	if got := hookOutput(t, out); got != "" {
		t.Errorf("the TUI's held prompt goes to its tab undecided: %q", got)
	}
	select {
	case <-yielded:
	case <-time.After(time.Second):
		t.Error("the TUI was never told it yielded")
	}
	if tui.Owns() || !s.hold.Owns() {
		t.Errorf("owners: tui %v serve %v", tui.Owns(), s.hold.Owns())
	}
	runHook(t, bashHook)
	waitHeld(t, s)
}

// A TUI never takes over: with one owner, a second is a client of it.
func TestTwoTUIs(t *testing.T) {
	first := tuiHolder(t, focusedNow(), time.Minute)
	second := NewHolder(HoldConfig{Who: "tui", Yields: true})
	if err := second.Listen(false); !errors.Is(err, ErrHeldElsewhere) {
		t.Fatalf("the second: %v", err)
	}
	runHook(t, bashHook)
	waitList(t, first)
	ps, err := HeldPrompts() // the second answers through the control socket
	if err != nil || len(ps) != 1 {
		t.Fatalf("through the socket: %v %v", ps, err)
	}
	if text, err := AnswerHeld(ps[0].ID, "deny", nil, nil); err != nil || !strings.Contains(text, "Deny") {
		t.Errorf("%q %v", text, err)
	}
	// Serve won't yield to a TUI: a TUI asking gets nowhere (it never asks,
	// but a yield to serve is refused).
	if _, err := controlCall(controlRequest{Op: "list"}); err != nil {
		t.Error(err)
	}
}

// Closing (ccs quits) lets everything go and removes both sockets.
func TestTUIHolderCloseCleansUp(t *testing.T) {
	h := NewHolder(HoldConfig{Who: "tui", Yields: true})
	if err := h.Listen(false); err != nil {
		t.Fatal(err)
	}
	out := runHook(t, bashHook)
	waitList(t, h)
	h.Close("ccs quit")
	if got := hookOutput(t, out); got != "" {
		t.Errorf("closed: %q", got)
	}
	for _, p := range []string{hookSockPath(), ControlSockPath()} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
	// And the lock is free for the next owner.
	next := NewHolder(HoldConfig{Who: "tui", Yields: true})
	if err := next.Listen(false); err != nil {
		t.Errorf("the next owner: %v", err)
	}
	next.Close("test over")
}

// ccs serve never yields: a second serve is refused, as before.
func TestServeNeverYields(t *testing.T) {
	hookServer(t, time.Minute)
	s2, _ := testServer(t, true, nil)
	if _, err := s2.listenHooks(); err == nil || !strings.Contains(err.Error(), "another ccs serve") {
		t.Errorf("a second ccs serve: %v", err)
	}
}

// A blur between the may-hold check and the registration can't leave the
// prompt held: the release closes the very channel it captured.
func TestHoldBlurDuringRegistration(t *testing.T) {
	f := focusedNow()
	h := tuiHolder(t, f, time.Minute)
	holdSeam = func(h *Holder) {
		f.Store(false)
		go h.ReleaseAll("ccs lost focus") // runs once the registration lets the lock go
	}
	t.Cleanup(func() { holdSeam = nil })
	start := time.Now()
	if out := hookOutput(t, runHook(t, bashHook)); out != "" || time.Since(start) > 2*time.Second {
		t.Errorf("let go undecided at once: %q after %s", out, time.Since(start))
	}
	if len(h.List()) != 0 {
		t.Error("nothing stays held")
	}
}

// An answer confirmed with y (taken from the book) goes through even if the
// prompt is released a moment before it's delivered.
func TestHoldConfirmedAnswerSurvivesRelease(t *testing.T) {
	h := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, bashHook)
	id := waitList(t, h)[0].ID
	p := h.book.take(id) // the answer took it
	h.ReleaseAll("ccs lost focus")
	go func() {
		time.Sleep(30 * time.Millisecond)
		p.reply <- hookReply{Behavior: "allow"}
	}()
	if d := decisionOf(t, hookOutput(t, out)); d["behavior"] != "allow" {
		t.Errorf("the confirmed answer: %v", d)
	}
}

// ccs serve wins against TUIs racing for the hold: while it takes over no
// TUI claims, and it asks whoever owns each round.
func TestServeWinsAgainstRacingTUIs(t *testing.T) {
	first := tuiHolder(t, focusedNow(), time.Minute)
	stop := make(chan struct{})
	done := make(chan struct{})
	var second *Holder
	go func() { // a second TUI claiming as fast as it can
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			h := NewHolder(HoldConfig{Who: "tui", Yields: true})
			if h.Listen(false) == nil {
				second = h
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	s := hookServer(t, time.Minute)
	close(stop)
	<-done
	if !s.hold.Owns() || first.Owns() || (second != nil && second.Owns()) {
		t.Errorf("serve owns: %v; first %v", s.hold.Owns(), first.Owns())
	}
	if second != nil {
		second.Close("test over")
	}
}

// With no MayHold (the TUI's way), a prompt is held and answerable whatever
// the focus.
func TestHolderWithoutMayHoldHolds(t *testing.T) {
	h := NewHolder(HoldConfig{Who: "tui", Wait: func() time.Duration { return time.Minute }, Yields: true})
	if err := h.Listen(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close("test over") })
	out := runHook(t, bashHook)
	p := waitList(t, h)[0]
	if p.At.IsZero() {
		t.Error("a held prompt says when it was held")
	}
	if _, err := h.Answer(p.ID, "allow", nil, nil); err != nil {
		t.Fatal(err)
	}
	if d := decisionOf(t, hookOutput(t, out)); d["behavior"] != "allow" {
		t.Errorf("decision %v", d)
	}
}

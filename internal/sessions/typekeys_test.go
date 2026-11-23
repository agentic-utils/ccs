package sessions

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type keyCall struct {
	at   time.Time
	args []string
}

// stubKeys records every terminal command SendKeys runs; fail, when set,
// answers the n-th call (from 1) with an error.
func stubKeys(t *testing.T, out string, fail int) *[]keyCall {
	t.Helper()
	var calls []keyCall
	old, oldDelay := runKeys, SubmitDelay
	runKeys = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, keyCall{time.Now(), append([]string{name}, args...)})
		if len(calls) == fail {
			return nil, errors.New("boom")
		}
		return []byte(out), nil
	}
	SubmitDelay = 20 * time.Millisecond
	t.Cleanup(func() { runKeys, SubmitDelay = old, oldDelay })
	return &calls
}

// The text and its Enter go separately, SubmitDelay apart: in one burst of
// 64 bytes or more Claude Code reads the Enter as part of a paste, a newline,
// and the message sits in the prompt unsent.
func TestSendKeysPressesEnterApart(t *testing.T) {
	text := strings.Repeat("long message ", 8)
	t.Run("tmux", func(t *testing.T) {
		calls := stubKeys(t, "", 0)
		if err := SendKeys(Terminal{"tmux pane", "%3"}, text, true); err != nil {
			t.Fatal(err)
		}
		c := *calls
		if len(c) != 2 {
			t.Fatalf("want text then Enter, got %v", c)
		}
		if got := strings.Join(c[0].args, " "); got != "tmux send-keys -t %3 -l -- "+text {
			t.Errorf("text call = %q", got)
		}
		if got := strings.Join(c[1].args, " "); got != "tmux send-keys -t %3 Enter" {
			t.Errorf("Enter call = %q", got)
		}
		if gap := c[1].at.Sub(c[0].at); gap < SubmitDelay {
			t.Errorf("Enter came %v after the text, want at least %v", gap, SubmitDelay)
		}
	})
	t.Run("iTerm", func(t *testing.T) {
		calls := stubKeys(t, "typed\n", 0)
		if err := SendKeys(Terminal{"iTerm tab", "/dev/ttys009"}, text, true); err != nil {
			t.Fatal(err)
		}
		c := *calls
		if len(c) != 2 {
			t.Fatalf("want text then Enter, got %d calls", len(c))
		}
		first, second := c[0].args[2], c[1].args[2]
		if !strings.Contains(first, "write text \""+text+"\" newline false") {
			t.Errorf("the text should go without a newline:\n%s", first)
		}
		if !strings.Contains(second, `write text "" newline true`) {
			t.Errorf("Enter should go on its own:\n%s", second)
		}
		if c[1].at.Sub(c[0].at) < SubmitDelay {
			t.Error("Enter came too soon after the text")
		}
	})
	t.Run("no Enter", func(t *testing.T) {
		calls := stubKeys(t, "", 0)
		if err := SendKeys(Terminal{"tmux pane", "%3"}, "2", false); err != nil || len(*calls) != 1 {
			t.Errorf("a menu digit is one call with no Enter: err=%v calls=%d", err, len(*calls))
		}
	})
}

// Once the text is in, a failed Enter must not read as "no terminal", which
// would let the caller send it again over the socket: a second copy.
func TestSendKeysEnterFailureIsNotNoTerminal(t *testing.T) {
	stubKeys(t, "", 2)
	err := SendKeys(Terminal{"tmux pane", "%3"}, "hello", true)
	if !errors.Is(err, ErrEnterNotSent) || errors.Is(err, ErrNoTerminal) {
		t.Errorf("err = %v, want ErrEnterNotSent and not ErrNoTerminal", err)
	}
	stubKeys(t, "", 1)
	if err := SendKeys(Terminal{"tmux pane", "%3"}, "hello", true); err == nil || errors.Is(err, ErrEnterNotSent) {
		t.Errorf("a failed text call is a plain failure, got %v", err)
	}
	calls := stubKeys(t, "not found", 0)
	if err := SendKeys(Terminal{"iTerm tab", "/dev/ttys009"}, "hello", true); !errors.Is(err, ErrNoTerminal) || len(*calls) != 1 {
		t.Errorf("no iTerm session: ErrNoTerminal before any Enter, got %v after %d calls", err, len(*calls))
	}
}

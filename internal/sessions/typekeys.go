package sessions

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentic-utils/ccs/internal/proc"
)

// Terminal is where a session's keys go: a tmux pane by its id, which never
// changes while it lives (a window index can, between the lookup and the
// keys), or an iTerm session by its tty. The zero Terminal is none.
type Terminal struct {
	Kind   string // "tmux pane" or "iTerm tab"
	Target string // the pane id, or the tty
}

// ErrNoTerminal: the iTerm tab with that tty wasn't found when the keys were sent.
var ErrNoTerminal = errors.New("no terminal")

// FindTerminal looks up the terminal running pid; it's separate from
// SendKeys so a caller can check, after the lookup and just before the keys,
// that they're still wanted (ccs serve's answers). A var, as SendKeys, so
// tests never reach a real terminal.
var FindTerminal = func(pid int) Terminal {
	out, err := proc.RunBounded(2*time.Second, nil, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	tty := strings.TrimSpace(string(out))
	if err != nil || tty == "" || tty == "??" {
		return Terminal{}
	}
	tty = "/dev/" + tty
	if os.Getenv("TMUX") != "" {
		panes, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{pane_id}")
		if target := TmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			return Terminal{"tmux pane", target}
		}
	}
	if InITerm() {
		return Terminal{"iTerm tab", tty}
	}
	return Terminal{}
}

// SendKeys types text into t, pressing Enter after it when enter is set. A
// timeout may still have typed it, so it's never retried.
var SendKeys = func(t Terminal, text string, enter bool) error {
	switch t.Kind {
	case "tmux pane":
		if _, err := proc.RunBounded(5*time.Second, nil, "tmux", "send-keys", "-t", t.Target, "-l", "--", text); err != nil || !enter {
			return err
		}
		_, err := proc.RunBounded(5*time.Second, nil, "tmux", "send-keys", "-t", t.Target, "Enter")
		return err
	case "iTerm tab":
		script := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					tell s to write text %q newline %t
					return "typed"
				end if
			end repeat
		end repeat
	end repeat
end tell`, t.Target, text, enter)
		out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-e", script)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "typed" {
			return ErrNoTerminal
		}
		return nil
	}
	return ErrNoTerminal
}

// TypeKeys types text into the terminal running pid (FindTerminal, then
// SendKeys), pressing Enter after it when enter is set (a digit alone picks
// a question menu's option). where names what it used ("" if nothing found
// it).
func TypeKeys(pid int, text string, enter bool) (where string, err error) {
	if strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f || r == utf8.RuneError }) {
		return "terminal", errors.New("the message contains control characters")
	}
	t := FindTerminal(pid)
	if t.Kind == "" {
		return "", nil
	}
	if err := SendKeys(t, text, enter); errors.Is(err, ErrNoTerminal) {
		return "", nil
	} else if err != nil {
		return t.Kind, err
	}
	return t.Kind, nil
}

// KeyIdle is how long since the last key press on this Mac (CoreGraphics:
// kCGEventSourceStateHIDSystemState, kCGEventKeyDown); not ok when it can't
// be read. ^X A auto-focus and ccs serve's held prompts both go by it.
func KeyIdle() (time.Duration, bool) {
	out, err := proc.RunBounded(2*time.Second, nil, "osascript", "-l", "JavaScript", "-e",
		"ObjC.import('CoreGraphics'); $.CGEventSourceSecondsSinceLastEventType(1, 10)")
	secs, perr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || perr != nil {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

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

// SubmitDelay is how long SendKeys waits between the text and its Enter.
// Claude Code reads a burst of 64 bytes or more as a paste, and a paste's
// Enter is a newline: typed with the text in one write, a message of 63
// characters or more sat in the prompt unsent (iTerm's write text sends the
// text and its \r in one write). Codex's paste-burst detection does the same.
// Sent apart, the Enter arrives as a key of its own and submits. A var for
// tests.
var SubmitDelay = 300 * time.Millisecond

// runKeys runs one terminal command for SendKeys; a var so tests see the
// sequence without a terminal.
var runKeys = func(name string, args ...string) ([]byte, error) {
	return proc.RunBounded(5*time.Second, nil, name, args...)
}

// itermWrite is the AppleScript that writes text to the iTerm session on tty,
// with a newline after it when newline is set; it returns "typed" if it found
// the session.
func itermWrite(tty, text string, newline bool) string {
	return fmt.Sprintf(`tell application "iTerm2"
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
end tell`, tty, text, newline)
}

// ErrEnterNotSent: the text was typed but its Enter wasn't, so it may sit in
// the prompt. Not ErrNoTerminal: the text went in, so nothing may send it
// another way.
var ErrEnterNotSent = errors.New("typed, but Enter couldn't be pressed: it may be waiting in the prompt")

// SendKeys types text into t, then, when enter is set, waits SubmitDelay and
// presses Enter on its own (see SubmitDelay). A timeout may still have typed
// it, so it's never retried.
var SendKeys = func(t Terminal, text string, enter bool) error {
	var typeText, pressEnter func() error
	switch t.Kind {
	case "tmux pane":
		typeText = func() error {
			_, err := runKeys("tmux", "send-keys", "-t", t.Target, "-l", "--", text)
			return err
		}
		pressEnter = func() error {
			_, err := runKeys("tmux", "send-keys", "-t", t.Target, "Enter")
			return err
		}
	case "iTerm tab":
		write := func(s string, newline bool) error {
			out, err := runKeys("osascript", "-e", itermWrite(t.Target, s, newline))
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(out)) != "typed" {
				return ErrNoTerminal
			}
			return nil
		}
		typeText = func() error { return write(text, false) }
		pressEnter = func() error { return write("", true) } // write text "" sends \r alone
	default:
		return ErrNoTerminal
	}
	if err := typeText(); err != nil || !enter {
		return err
	}
	time.Sleep(SubmitDelay)
	if err := pressEnter(); err != nil {
		if errors.Is(err, proc.ErrTimedOut) {
			return err
		}
		return fmt.Errorf("%w (%v)", ErrEnterNotSent, err)
	}
	return nil
}

// Key is a key press Claude Code reads from its terminal, as tmux names it
// for send-keys (several for a chord) and as the bytes iTerm writes. Esc is
// written alone: Claude Code reads an escape followed by more bytes in the
// same read as Alt and a key.
type Key struct {
	Name  string   // how ccs names it in notes and logs
	Tmux  []string // send-keys key names, never -l
	Bytes []byte
}

// The keys of Claude Code's own bindings that ccs presses for the user:
// chat:cancel, chat:sendNow (its other binding beside ctrl+enter, which a
// terminal can't tell from enter) and task:background.
var (
	KeyInterrupt  = Key{"Esc", []string{"Escape"}, []byte{0x1b}}
	KeySendNow    = Key{"Ctrl+X Ctrl+S", []string{"C-x", "C-s"}, []byte{0x18, 0x13}}
	KeyBackground = Key{"Ctrl+B", []string{"C-b"}, []byte{0x02}}
)

// SendKey presses k in t. A timeout may still have pressed it, so it's
// never retried. A var, as SendKeys, so tests never reach a real terminal.
var SendKey = func(t Terminal, k Key) error {
	switch t.Kind {
	case "tmux pane":
		_, err := proc.RunBounded(5*time.Second, nil, "tmux", append([]string{"send-keys", "-t", t.Target}, k.Tmux...)...)
		return err
	case "iTerm tab":
		chars := make([]string, len(k.Bytes))
		for i, b := range k.Bytes {
			chars[i] = fmt.Sprintf("(ASCII character %d)", b)
		}
		script := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					tell s to write text (%s) newline NO
					return "typed"
				end if
			end repeat
		end repeat
	end repeat
end tell`, t.Target, strings.Join(chars, " & "))
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
// be read. ^T A auto-focus and ccs serve's held prompts both go by it.
func KeyIdle() (time.Duration, bool) {
	out, err := proc.RunBounded(2*time.Second, nil, "osascript", "-l", "JavaScript", "-e",
		"ObjC.import('CoreGraphics'); $.CGEventSourceSecondsSinceLastEventType(1, 10)")
	secs, perr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || perr != nil {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

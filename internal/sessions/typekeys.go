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

// TypeKeys types text into the terminal running pid, pressing Enter after it
// when enter is set (a digit alone picks a question menu's option): its tmux
// pane, else its iTerm tab. where names what it used ("" if neither found
// it). A tmux pane is targeted by its id, which never changes while it
// lives, not its window index, which can between the lookup and the keys.
// A timeout may still have typed it, so it's never retried. A var so tests
// never type into a real terminal.
var TypeKeys = func(pid int, text string, enter bool) (where string, err error) {
	if strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f || r == utf8.RuneError }) {
		return "terminal", errors.New("the message contains control characters")
	}
	out, err := proc.RunBounded(2*time.Second, nil, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	tty := strings.TrimSpace(string(out))
	if err != nil || tty == "" || tty == "??" {
		return "", nil
	}
	tty = "/dev/" + tty
	if os.Getenv("TMUX") != "" {
		panes, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{pane_id}")
		if target := TmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			if _, err := proc.RunBounded(5*time.Second, nil, "tmux", "send-keys", "-t", target, "-l", "--", text); err != nil || !enter {
				return "tmux pane", err
			}
			_, err := proc.RunBounded(5*time.Second, nil, "tmux", "send-keys", "-t", target, "Enter")
			return "tmux pane", err
		}
	}
	if !InITerm() {
		return "", nil
	}
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
end tell`, tty, text, enter)
	out, err = proc.RunBounded(5*time.Second, nil, "osascript", "-e", script)
	if err != nil {
		return "iTerm tab", err
	}
	if strings.TrimSpace(string(out)) != "typed" {
		return "", nil
	}
	return "iTerm tab", nil
}

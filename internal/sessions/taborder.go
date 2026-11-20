package sessions

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
)

// OrderTabs puts the terminal tabs of the sessions on ttys (in list order) in
// that order: tmux windows within each tmux session when ccs runs in tmux,
// and iTerm tabs within each iTerm window when it runs in iTerm (both in tmux
// inside iTerm). Only the tabs holding a listed session move, among the places
// they already hold; any other tab (ccs's own, a shell) stays where it is. A
// tab with several panes goes where its earliest listed session would. moved
// counts the tabs and windows that changed place. Stubbed in tests.
var OrderTabs = func(ttys []string) (moved int, err error) {
	rank := make(map[string]int, len(ttys))
	for i, t := range ttys {
		if _, seen := rank[t]; !seen && t != "" {
			rank[t] = i
		}
	}
	var errs []error
	tried := false
	if os.Getenv("TMUX") != "" {
		tried = true
		n, err := orderTmux(rank)
		moved += n
		errs = append(errs, err)
	}
	if InITerm() {
		tried = true
		n, err := orderITerm(rank)
		moved += n
		errs = append(errs, err)
	}
	if !tried {
		return 0, errors.New("ordering tabs works in tmux and iTerm, and this is neither")
	}
	return moved, errors.Join(errs...)
}

// slotOrder is the new order of tabs, each given as the ttys in it, as
// indexes into tabs: the tabs holding a listed tty (rank) take the places
// such tabs hold now, sorted by their earliest rank; the rest keep theirs.
// nil when nothing would move.
func slotOrder(tabs [][]string, rank map[string]int) []int {
	first := make([]int, len(tabs))
	var slots []int
	for i, ttys := range tabs {
		first[i] = -1
		for _, t := range ttys {
			if r, ok := rank[t]; ok && (first[i] < 0 || r < first[i]) {
				first[i] = r
			}
		}
		if first[i] >= 0 {
			slots = append(slots, i)
		}
	}
	sorted := slices.Clone(slots)
	slices.SortStableFunc(sorted, func(a, b int) int { return first[a] - first[b] })
	order := make([]int, len(tabs))
	for i := range order {
		order[i] = i
	}
	for j, slot := range slots {
		order[slot] = sorted[j]
	}
	for i, o := range order {
		if o != i {
			return order
		}
	}
	return nil
}

// tmuxWindow is one tmux window: its id (@N), whether it is its session's
// current one, and its panes' ttys.
type tmuxWindow struct {
	id     string
	active bool
	ttys   []string
}

// tmuxPanesFormat is the list-panes format tmuxSessions reads.
const tmuxPanesFormat = "#{session_id} #{window_id} #{window_active} #{pane_tty}"

// tmuxSessions groups `tmux list-panes -a -F tmuxPanesFormat` output into each
// session's windows, in index order (list-panes -a lists them that way). A
// window linked into two sessions is listed under each.
func tmuxSessions(panes string) [][]tmuxWindow {
	var out [][]tmuxWindow
	sess := ""
	for _, line := range strings.Split(panes, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		if f[0] != sess || len(out) == 0 {
			sess = f[0]
			out = append(out, nil)
		}
		ws := out[len(out)-1]
		if n := len(ws); n > 0 && ws[n-1].id == f[1] {
			ws[n-1].ttys = append(ws[n-1].ttys, f[3])
		} else {
			ws = append(ws, tmuxWindow{id: f[1], active: f[2] == "1", ttys: []string{f[3]}})
		}
		out[len(out)-1] = ws
	}
	return out
}

// tmuxSwaps is the swap-window -s/-t pairs that put wins in order (indexes
// into wins, from slotOrder), one place at a time.
func tmuxSwaps(wins []tmuxWindow, order []int) [][2]string {
	cur := make([]string, len(wins))
	for i, w := range wins {
		cur[i] = w.id
	}
	var swaps [][2]string
	for i, o := range order {
		want := wins[o].id
		if cur[i] == want {
			continue
		}
		j := slices.Index(cur, want)
		swaps = append(swaps, [2]string{want, cur[i]})
		cur[i], cur[j] = cur[j], cur[i]
	}
	return swaps
}

// TmuxRun runs one tmux command. Stubbed in tests.
var TmuxRun = func(args ...string) (string, error) {
	out, err := proc.RunBounded(5*time.Second, nil, "tmux", args...)
	return string(out), err
}

func orderTmux(rank map[string]int) (int, error) {
	panes, err := TmuxRun("list-panes", "-a", "-F", tmuxPanesFormat)
	if err != nil {
		return 0, fmt.Errorf("tmux list-panes: %w", err)
	}
	moved := 0
	for _, wins := range tmuxSessions(panes) {
		tabs := make([][]string, len(wins))
		active := ""
		for i, w := range wins {
			tabs[i] = w.ttys
			if w.active {
				active = w.id
			}
		}
		order := slotOrder(tabs, rank)
		if order == nil {
			continue
		}
		// -d keeps the current place selected, which now shows another
		// window, so the window that was current is selected again after.
		for _, s := range tmuxSwaps(wins, order) {
			if _, err := TmuxRun("swap-window", "-d", "-s", s[0], "-t", s[1]); err != nil {
				return moved, fmt.Errorf("tmux swap-window: %w", err)
			}
		}
		for i, o := range order {
			if i != o {
				moved++
			}
		}
		if active != "" {
			if _, err := TmuxRun("select-window", "-t", active); err != nil {
				return moved, fmt.Errorf("tmux select-window: %w", err)
			}
		}
	}
	return moved, nil
}

// ITermSessionTTYs is every iTerm session's unique id and tty, which the API
// doesn't give. Stubbed in tests.
var ITermSessionTTYs = func() (map[string]string, error) {
	out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-e", `tell application "iTerm2"
	set out to ""
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				set out to out & (unique ID of s) & " " & (tty of s) & linefeed
			end repeat
		end repeat
	end repeat
	return out
end tell`)
	if err != nil {
		return nil, iTermError(err)
	}
	ttys := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if id, tty, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			ttys[id] = tty
		}
	}
	return ttys, nil
}

// ITermAPI is the iTerm API calls ordering needs. Stubbed in tests.
var ITermAPI = func() (iTermAPI, error) {
	c, err := dialITermAPI()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// iTermAPI lists iTerm's windows and reorders their tabs. AppleScript can do
// neither: a tab's index is read-only, and `move` on a tab reports success
// and leaves the order as it was (iTerm 3.7.8).
type iTermAPI interface {
	Windows() ([]iTermWindow, error)
	ReorderTabs(windowID string, tabIDs []string) error
	Close() error
}

// iTermWindow is one iTerm window: its id and its tabs in order.
type iTermWindow struct {
	ID   string
	Tabs []iTermTab
}

// iTermTab is one tab: its id and the unique ids of the sessions in it.
type iTermTab struct {
	ID       string
	Sessions []string
}

func orderITerm(rank map[string]int) (int, error) {
	api, err := ITermAPI()
	if err != nil {
		return 0, err
	}
	defer api.Close()
	wins, err := api.Windows()
	if err != nil {
		return 0, err
	}
	ttyOf, err := ITermSessionTTYs()
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, w := range wins {
		tabs := make([][]string, len(w.Tabs))
		for i, t := range w.Tabs {
			for _, s := range t.Sessions {
				tabs[i] = append(tabs[i], ttyOf[s])
			}
		}
		order := slotOrder(tabs, rank)
		if order == nil {
			continue
		}
		ids := make([]string, len(order))
		for i, o := range order {
			ids[i] = w.Tabs[o].ID
			if i != o {
				moved++
			}
		}
		if err := api.ReorderTabs(w.ID, ids); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

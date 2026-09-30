package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// liveInterval is how often ccs re-reads which sessions are live and picks
// up new lines in live (and just-exited) sessions. Cheap: ~10 small files
// plus one stat per live transcript, and only appended lines are parsed.
const liveInterval = 2 * time.Second

type liveTickMsg struct{}

type focusDoneMsg struct {
	pid   int
	found bool
}

// resumeDoneMsg reports how Enter/Ctrl+F opened a conversation: in a new
// tab/window (ccs stays up), or not at all (exec in place after quitting).
type resumeDoneMsg struct {
	conv   Conversation
	fork   bool
	opened bool
	err    error
}

type liveMsg struct {
	live    map[string]bool
	pids    map[string]int
	updated []Conversation // live conversations whose transcript grew
	unknown []string       // live sessions not in the list yet
	gen     int
}

// openedGrace is how long a session ccs opened in a tab counts as live before
// claude has written its session file.
const openedGrace = 15 * time.Second

// liveReads marks transcripts with a live-tick read in flight.
var liveReads sync.Map

func liveTick() tea.Cmd {
	return tea.Tick(liveInterval, func(time.Time) tea.Msg { return liveTickMsg{} })
}

// liveCmd refreshes liveness and the content of live sessions off the UI
// goroutine. Sessions that were live last tick are checked too, so a
// session's final lines land promptly after claude exits.
func (m model) liveCmd() tea.Cmd {
	byID := make(map[string]Conversation, len(m.live))
	for _, item := range m.items {
		byID[item.conv.SessionID] = item.conv
	}
	wasLive, gen := maps.Clone(m.live), m.gen
	return func() tea.Msg {
		pids := liveSessionPIDs()
		live := make(map[string]bool, len(pids))
		for id := range pids {
			live[id] = true
		}
		msg := liveMsg{live: live, pids: pids, gen: gen}
		check := make(map[string]bool, len(live)+len(wasLive))
		for id := range live {
			check[id] = true
		}
		for id := range wasLive {
			check[id] = true
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for id := range check {
			conv, ok := byID[id]
			if !ok {
				if live[id] {
					msg.unknown = append(msg.unknown, id)
				}
				continue
			}
			// A read still stuck from an earlier tick (e.g. a hung mount)
			// isn't started again; the rest carry on.
			if _, busy := liveReads.LoadOrStore(conv.FilePath, true); busy {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer liveReads.Delete(conv.FilePath)
				defer recoverWorker()
				if c, err := parseAppended(&conv); err == nil && c != nil && c.Size != conv.Size {
					mu.Lock()
					msg.updated = append(msg.updated, *c)
					mu.Unlock()
				}
			}()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(liveInterval): // deliver what's ready; a straggler's cached result is picked up next tick
		}
		mu.Lock()
		defer mu.Unlock()
		msg.updated = slices.Clone(msg.updated)
		return msg
	}
}

// applyLive swaps updated conversations into the list, re-sorted by last
// activity, keeping the cursor on the same conversation. Unlike a full
// refresh it only re-matches and re-counts the changed rows, so a streaming
// session doesn't rebuild every cache on the UI goroutine each tick.
func (m *model) applyLive(updated []Conversation) {
	byID := make(map[string]listItem, len(updated))
	for _, item := range buildItems(updated) {
		byID[item.conv.SessionID] = item
	}
	items := make([]listItem, len(m.items))
	newMessages := make(map[string]bool, len(updated))
	for i, item := range m.items {
		// A full scan may have read this file after the live tick did.
		if u, ok := byID[item.conv.SessionID]; ok && !item.conv.readAt.After(u.conv.readAt) {
			newMessages[item.conv.SessionID] = len(u.conv.Messages) != len(item.conv.Messages)
			item = u
		} else {
			delete(byID, item.conv.SessionID)
		}
		items[i] = item
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].conv.LastTimestamp > items[j].conv.LastTimestamp
	})

	var selectedID string
	oldLines := 0
	if len(m.filtered) > 0 {
		selectedID = m.filtered[m.cursor].conv.SessionID
		oldLines = len(m.previewLines())
	}
	shown := make(map[string]bool, len(m.filtered))
	for _, item := range m.filtered {
		shown[item.conv.SessionID] = true
	}
	query := strings.ToLower(m.textInput.Value())
	filtered := make([]listItem, 0, len(m.filtered)+len(updated))
	for _, item := range items {
		match := shown[item.conv.SessionID]
		if _, changed := byID[item.conv.SessionID]; changed {
			match = query == "" || strings.Contains(item.searchLower, query)
			// Most appends are tool calls: HITS only moves with new messages.
			if m.hits != nil && newMessages[item.conv.SessionID] {
				delete(m.hits.byID, item.conv.SessionID)
			}
		}
		if match {
			filtered = append(filtered, item)
		}
	}
	m.items, m.filtered = items, filtered
	m.cursor = min(m.cursor, max(0, len(filtered)-1))
	found := false
	for i, item := range filtered {
		if item.conv.SessionID == selectedID {
			m.cursor, found = i, true
			break
		}
	}
	// The selected preview can shrink (or be a different conversation), so a
	// stored scroll offset past its end would make Ctrl+K seem stuck.
	if _, changed := byID[selectedID]; changed && found && m.previewScroll > 0 {
		// Scrolled back reading: new lines arrive at the bottom, so move the
		// offset by as much, keeping the same lines on screen.
		m.previewScroll += len(m.previewLines()) - oldLines
	}
	if _, changed := byID[selectedID]; changed || !found {
		m.previewScroll = max(0, min(m.previewScroll, m.maxPreviewScroll()))
	}
}

// getSessionsDir returns where Claude Code records running sessions, one
// <pid>.json per process. Declared as a variable so tests can override it.
var getSessionsDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "sessions")
}

// readLiveSessions returns the SessionIDs of conversations currently open in a
// running claude process. A session file whose pid is gone is a stale leftover.
// ponytail: a recycled pid can make a stale file look live until claude cleans it up.
func readLiveSessions() map[string]bool {
	live := make(map[string]bool)
	for id := range liveSessionPIDs() {
		live[id] = true
	}
	return live
}

// liveSessionPIDs maps each live SessionID to the pid of its claude process.
func liveSessionPIDs() map[string]int {
	type candidate struct {
		id, start string
	}
	byPid := make(map[int][]candidate)
	files, _ := filepath.Glob(filepath.Join(getSessionsDir(), "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			Pid       int    `json:"pid"`
			SessionID string `json:"sessionId"`
			ProcStart string `json:"procStart"` // UTC, e.g. "Wed Sep 23 16:52:33 2026"
		}
		if json.Unmarshal(data, &s) != nil || s.SessionID == "" || s.Pid <= 0 {
			continue
		}
		// Signal 0 probes the pid; EPERM still means the process exists.
		if err := syscall.Kill(s.Pid, 0); err == nil || err == syscall.EPERM {
			byPid[s.Pid] = append(byPid[s.Pid], candidate{s.SessionID, s.ProcStart})
		}
	}
	// Check each file's process before mapping sessions: a stale file whose
	// pid was recycled must not overwrite the live file for the same session.
	pids := make(map[string]int, len(byPid))
	for pid := range byPid {
		pids[strconv.Itoa(pid)] = pid
	}
	actual := processStartTimes(pids)
	live := make(map[string]int)
	for pid, cs := range byPid {
		for _, c := range cs {
			if sameStart(c.start, actual[pid]) {
				live[c.id] = pid
			}
		}
	}
	return live
}

// processStartTimes returns each pid's start time from one ps call. Pids ps
// can't report are left out.
var processStartTimes = func(live map[string]int) map[int]time.Time {
	out := make(map[int]time.Time)
	if len(live) == 0 {
		return out
	}
	pids := make([]string, 0, len(live))
	for _, pid := range live {
		pids = append(pids, strconv.Itoa(pid))
	}
	// Fixed lstart layout, in UTC like procStart: local time is ambiguous in
	// the hour clocks go back.
	raw, _ := runBounded(2*time.Second, []string{"LC_ALL=C", "TZ=UTC"}, "ps", "-o", "pid=,lstart=", "-p", strings.Join(pids, ","))
	for _, line := range strings.Split(string(raw), "\n") {
		pidStr, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(pidStr)
		if !ok || err != nil {
			continue
		}
		if t, err := time.Parse(time.ANSIC, strings.Join(strings.Fields(rest), " ")); err == nil {
			out[pid] = t
		}
	}
	return out
}

// sameStart compares the session file's procStart (UTC) with the process's
// actual start. Unknown on either side counts as a match, so a format change
// or ps failure never hides a live session.
func sameStart(recorded string, actual time.Time) bool {
	want, err := time.Parse(time.ANSIC, strings.Join(strings.Fields(recorded), " "))
	if err != nil || actual.IsZero() {
		return true
	}
	d := actual.Sub(want)
	return d > -2*time.Second && d < 2*time.Second
}

// isLive re-reads the session files so a prune/rename decision never trusts a
// liveness snapshot up to a refresh interval old. A session already marked live
// (e.g. one ccs just opened, before claude writes its session file) stays live
// until the next refresh replaces m.live.
func (m *model) isLive(id string) bool {
	fresh := readLiveSessions()
	if m.live[id] {
		fresh[id] = true
	}
	m.live = fresh
	return fresh[id]
}

// openResumeTab tries to resume conv in a new tmux window or iTerm tab. Returns
// false when that isn't possible, leaving the caller to exec claude in place.
func openResumeTab(conv Conversation, claudeFlags []string) (bool, error) {
	cwd := conv.Cwd
	if cwd == "" || cwd == "unknown" {
		cwd = "."
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return false, nil
	}
	args := append([]string{claudePath, "--resume", conv.SessionID}, claudeFlags...)
	// tmux first: inside tmux the iTerm tab would land outside the session.
	if handled, err := resumeInTmuxWindow(cwd, args); handled {
		return true, err
	}
	return resumeInITermTab(cwd, args)
}

// resumeInTmuxWindow opens args in a new background tmux window. handled is
// false outside tmux. An error after trying means the window may or may not
// have opened, so the caller must not launch another copy.
func resumeInTmuxWindow(cwd string, args []string) (handled bool, err error) {
	if os.Getenv("TMUX") == "" {
		return false, nil
	}
	// -d leaves the current window (ccs) focused. tmux expands formats in -c,
	// including #(command), so a '#' in the path must be escaped.
	tmuxArgs := append([]string{"new-window", "-d", "-c", strings.ReplaceAll(cwd, "#", "##")}, args...)
	return tabResult(runBounded(5*time.Second, nil, "tmux", tmuxArgs...))
}

// resumeCmd opens conv off the UI goroutine. Liveness is checked afresh, so
// a session resumed elsewhere since the last live tick is focused, not
// resumed a second time. Forks always open a new session.
func (m *model) resumeCmd(conv Conversation, fork bool) tea.Cmd {
	if m.resuming {
		m.errorMsg = "Still opening the last one..."
		return nil // one at a time: a double Enter mustn't open two tabs
	}
	// Just opened in a tab but claude hasn't written its session file yet:
	// there's nothing to focus, and resuming again would start a second copy.
	if t, ok := m.opened[conv.SessionID]; ok && !fork && time.Since(t) < openedGrace {
		m.errorMsg = "Already opening in another tab"
		return nil
	}
	m.resuming = true
	flags := slices.Clone(m.claudeFlags)
	if fork {
		flags = append(flags, "--fork-session")
	}
	return func() tea.Msg {
		if !fork {
			if pid, ok := liveSessionPIDs()[conv.SessionID]; ok {
				return focusDoneMsg{pid: pid, found: focusSession(pid)}
			}
		}
		opened, err := openResumeTab(conv, flags)
		return resumeDoneMsg{conv: conv, fork: fork, opened: opened, err: err}
	}
}

// focusSession brings the terminal running pid to the front: its tmux pane
// when ccs is in tmux, else its iTerm tab. Matched by the process's tty.
func focusSession(pid int) bool {
	out, err := runBounded(2*time.Second, nil, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	tty := strings.TrimSpace(string(out))
	if err != nil || tty == "" || tty == "??" {
		return false
	}
	tty = "/dev/" + tty
	if os.Getenv("TMUX") != "" {
		panes, err := runBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
		if target := tmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			_, err := runBounded(5*time.Second, nil, "tmux", "switch-client", "-t", target, ";", "select-window", "-t", target, ";", "select-pane", "-t", target)
			return err == nil
		}
	}
	if os.Getenv("TERM_PROGRAM") != "iTerm.app" {
		return false
	}
	script := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					select w
					tell t to select
					tell s to select
					activate
					return "found"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty)
	out, err = runBounded(5*time.Second, nil, "osascript", "-e", script)
	return err == nil && strings.TrimSpace(string(out)) == "found"
}

// tmuxPaneForTTY picks the pane target whose tty matches from
// `tmux list-panes -a -F '#{pane_tty} <target>'` output.
func tmuxPaneForTTY(panes, tty string) string {
	for _, line := range strings.Split(panes, "\n") {
		if t, target, ok := strings.Cut(line, " "); ok && t == tty {
			return target
		}
	}
	return ""
}

// shellQuote wraps s for /bin/sh single-quoted use.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resumeInITermTab opens the resume command in a new iTerm tab. Returns false
// if we're not in iTerm or osascript failed, so the caller can exec in place.
// ponytail: osascript, not the iTerm python API - no deps, no daemon.
func resumeInITermTab(cwd string, args []string) (handled bool, err error) {
	if os.Getenv("TERM_PROGRAM") != "iTerm.app" || !shellSafe(cwd) || slices.IndexFunc(args, func(a string) bool { return !shellSafe(a) }) >= 0 {
		return false, nil // exec in place instead: no shell involved
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	cmd := "cd " + shellQuote(cwd) + " && exec " + strings.Join(quoted, " ")
	// Focus stays on the ccs tab: creating a tab selects it, so select the old
	// one back. No "activate", so iTerm doesn't jump to the front either.
	script := fmt.Sprintf(`tell application "iTerm2"
	tell current window
		set oldTab to current tab
		create tab with default profile
		tell current session to write text %q
		select oldTab
	end tell
end tell`, cmd)
	return tabResult(runBounded(5*time.Second, nil, "osascript", "-e", script))
}

// tabResult maps a tab-open attempt to (handled, err): success is handled; a
// timeout is handled with an error (it may have opened, so launching again
// could run two claudes on one transcript); any other failure (Automation
// denied, stale $TMUX) is unhandled, so the caller resumes in place.
func tabResult(_ []byte, err error) (bool, error) {
	if err == nil || errors.Is(err, errTimedOut) {
		return true, err
	}
	return false, nil
}

// shellSafe reports whether s can go through shellQuote into a shell safely
// whatever that shell is: fish treats backslash as an escape even inside
// single quotes, and control characters break the AppleScript string.
func shellSafe(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f || r == utf8.RuneError })
}

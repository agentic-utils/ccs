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
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// liveInterval is how often ccs re-reads which sessions are live and picks
// up new lines in live (and just-exited) sessions. Cheap: ~10 small files
// plus one stat per live transcript, and only appended lines are parsed.
const liveInterval = 2 * time.Second

type liveTickMsg struct{}

type focusDoneMsg struct {
	pid   int
	found bool
	gone  bool   // the session wasn't running any more, so nothing was opened
	name  string // with gone
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
	live       map[string]bool
	pids       map[string]int
	updated    []Conversation         // live conversations whose transcript grew
	unknown    []string               // live sessions not in the list yet
	stats      map[string]sessionStat // each live session's status, from its session file
	unverified map[string]bool        // live sessions whose pid couldn't be checked against the file's procStart
	scan       map[string]liveSession // this tick's sessions, the next tick's prev
	gen        int
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
	wasLive, prev, gen, record := maps.Clone(m.live), m.lastScan, m.gen, m.reopenChecked
	return func() tea.Msg {
		sessions := scanSessions(prev)
		live := make(map[string]bool, len(sessions))
		msg := liveMsg{live: live, pids: make(map[string]int, len(sessions)), gen: gen, stats: make(map[string]sessionStat, len(sessions)), unverified: make(map[string]bool), scan: sessions}
		for id, s := range sessions {
			live[id], msg.pids[id] = true, s.pid
			msg.stats[id] = s.stat
			if s.unverified {
				msg.unverified[id] = true
			}
		}
		if record { // only once the last boot's record has been read
			recordOpen(live)
		}
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
	out := make(map[string]int)
	for id, s := range scanSessions(nil) {
		out[id] = s.pid
	}
	return out
}

// liveSession is a live session's pid and what its file says, read once.
type liveSession struct {
	pid        int
	stat       sessionStat
	start      string // the file's procStart, so a kept session's pid can be checked later
	unverified bool   // the pid's start time couldn't be checked against the file's: maybe a recycled pid
	misses     int    // ticks in a row it was kept without a readable file
}

// keepTicks is how long a session without a readable file is kept when its
// process's start time was never known, so a recycled pid can't be ruled out.
const keepTicks = 3

// pidAlive probes pid with signal 0; EPERM still means the process exists.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// scanSessions reads ~/.claude/sessions once: each live SessionID's pid and
// status. A session in prev (the last scan) whose file is missing or can't be
// read this time isn't one that ended: it's kept, with no status so nothing
// changes, while its pid is still the process that wrote it (same start time,
// or for keepTicks when that was never known), and not once its pid's file
// names another session (/clear).
func scanSessions(prev map[string]liveSession) map[string]liveSession {
	type candidate struct {
		id, start string
		stat      sessionStat
	}
	byPid := make(map[int][]candidate)
	files, _ := filepath.Glob(filepath.Join(getSessionsDir(), "*.json"))
	for _, f := range files {
		var s struct {
			sessionFile
			Pid       int    `json:"pid"`
			ProcStart string `json:"procStart"` // UTC, e.g. "Wed Sep 23 16:52:33 2026"
		}
		data, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(data, &s) != nil || s.SessionID == "" || s.Pid <= 0 {
			continue
		}
		if pidAlive(s.Pid) {
			byPid[s.Pid] = append(byPid[s.Pid], candidate{s.SessionID, s.ProcStart, s.stat()})
		}
	}
	// Check each process before mapping sessions: a stale file whose pid was
	// recycled must not overwrite the live file for the same session.
	pids := make(map[string]int, len(byPid)+len(prev))
	for pid := range byPid {
		pids[strconv.Itoa(pid)] = pid
	}
	for _, p := range prev {
		pids[strconv.Itoa(p.pid)] = p.pid
	}
	actual := processStartTimes(pids)
	live := make(map[string]liveSession)
	for pid, cs := range byPid {
		for _, c := range cs {
			if match, checked := sameStart(c.start, actual[pid]); match {
				live[c.id] = liveSession{pid: pid, stat: c.stat, start: c.start, unverified: !checked}
			}
		}
	}
	for id, p := range prev {
		if _, ok := live[id]; ok || len(byPid[p.pid]) > 0 || !pidAlive(p.pid) {
			continue
		}
		if match, checked := sameStart(p.start, actual[p.pid]); match && (checked || p.misses < keepTicks) {
			live[id] = liveSession{pid: p.pid, start: p.start, unverified: p.unverified, misses: p.misses + 1}
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
// or ps failure never hides a live session; checked says whether it was known.
func sameStart(recorded string, actual time.Time) (match, checked bool) {
	want, err := time.Parse(time.ANSIC, strings.Join(strings.Fields(recorded), " "))
	if err != nil || actual.IsZero() {
		return true, false
	}
	d := actual.Sub(want)
	return d > -2*time.Second && d < 2*time.Second, true
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

// opening reports (and says why) when id mustn't be opened now: one at a
// time, so a double Enter can't open two tabs, and not while a tab ccs just
// opened is still starting (resuming again would start a second copy). A
// fork is a new session, so only the first applies.
func (m *model) opening(id string, fork bool) bool {
	if m.resuming {
		m.errorMsg = "Still opening the last one..."
		return true
	}
	if t, ok := m.opened[id]; ok && !fork && time.Since(t) < openedGrace {
		m.errorMsg = "Already opening in another tab"
		return true
	}
	return false
}

// focusCmd brings a live session's terminal to the front (Enter on an empty
// message box). Unlike resumeCmd it never starts claude: a session that just
// exited, or whose file can't be read right now, is reported instead of
// resumed a second time.
func (m *model) focusCmd(conv Conversation) tea.Cmd {
	if m.opening(conv.SessionID, false) {
		return nil
	}
	m.resuming = true
	name := m.sessionName(conv)
	return func() tea.Msg {
		pid, ok := liveSessionPIDs()[conv.SessionID]
		if !ok {
			return focusDoneMsg{gone: true, name: name}
		}
		return focusDoneMsg{pid: pid, found: focusSession(pid)}
	}
}

// resumeCmd opens conv off the UI goroutine. Liveness is checked afresh, so
// a session resumed elsewhere since the last live tick is focused, not
// resumed a second time. Forks always open a new session.
func (m *model) resumeCmd(conv Conversation, fork bool) tea.Cmd {
	if m.opening(conv.SessionID, fork) {
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

// openRecordPath is where ccs notes which sessions are open and on which boot,
// so after a restart it can offer to reopen them (Claude Code's own session
// files don't survive it).
var openRecordPath = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccs", "open-sessions.json")
}()

type openRecord struct {
	Boot     string   `json:"boot"`
	Sessions []string `json:"sessions"`
}

// bootID names the current boot: Linux's boot_id, else macOS's boot time.
var bootID = func() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return strings.TrimSpace(string(b))
	}
	out, _ := runBounded(2*time.Second, nil, "sysctl", "-n", "kern.boottime")
	return strings.TrimSpace(string(out))
}

// lastOpenRecord is the record as last written, so an unchanged set isn't
// written again every tick. Only the live tick touches it, one at a time.
var lastOpenRecord string

// recordOpen writes the live sessions with this boot's id when the set changes.
// Atomic, so a restart mid-write leaves the previous record, not half of one.
func recordOpen(live map[string]bool) {
	boot := bootID()
	if boot == "" {
		return
	}
	rec := openRecord{Boot: boot, Sessions: slices.Sorted(maps.Keys(live))}
	data, _ := json.Marshal(rec)
	if string(data) == lastOpenRecord {
		return
	}
	if os.MkdirAll(filepath.Dir(openRecordPath), 0o700) != nil {
		return
	}
	tmp := openRecordPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil && os.Rename(tmp, openRecordPath) == nil {
		lastOpenRecord = string(data)
	}
}

type reopenMsg struct{ ids []string }

// reopenCheckCmd reads the record left by the last boot: its sessions that
// aren't open again already are offered for reopening. Nothing on the same
// boot, or with no record. Runs before the first recordOpen overwrites it.
func reopenCheckCmd() tea.Msg {
	data, err := os.ReadFile(openRecordPath)
	var rec openRecord
	if err != nil || json.Unmarshal(data, &rec) != nil {
		return reopenMsg{}
	}
	if boot := bootID(); boot == "" || rec.Boot == boot {
		return reopenMsg{}
	}
	live := liveSessionPIDs()
	var ids []string
	for _, id := range rec.Sessions {
		if _, ok := live[id]; !ok {
			ids = append(ids, id)
		}
	}
	return reopenMsg{ids: ids}
}

type reopenDoneMsg struct {
	opened []string
	err    error // a tab may or may not have opened: stopped there
	noTabs bool  // neither tmux nor iTerm: nothing opened
}

// reopenCmd resumes each session in its own tab, skipping any open again by
// now. It stops at the first failure rather than resuming in place.
func (m *model) reopenCmd(convs []Conversation) tea.Cmd {
	m.resuming = true
	flags := slices.Clone(m.claudeFlags)
	return func() tea.Msg {
		var done reopenDoneMsg
		live := liveSessionPIDs()
		for _, conv := range convs {
			if _, ok := live[conv.SessionID]; ok {
				continue
			}
			opened, err := openResumeTab(conv, flags)
			if err != nil {
				done.err = err
				break
			}
			if !opened {
				done.noTabs = true
				break
			}
			done.opened = append(done.opened, conv.SessionID)
		}
		return done
	}
}

// maxReopenRows is how many sessions the reopen popup names before "+N more".
const maxReopenRows = 15

func (m model) reopenPopup() string {
	var b strings.Builder
	n := len(m.reopen)
	what := "the session"
	if n > 1 {
		what = fmt.Sprintf("the %d sessions", n)
	}
	fmt.Fprintf(&b, "\033[1mReopen %s open before the restart?\033[0m\n\n", what)
	nameW := 0
	for _, c := range m.reopen[:min(n, maxReopenRows)] {
		nameW = max(nameW, ansi.StringWidth(truncate(m.sessionName(c), 40)))
	}
	for _, c := range m.reopen[:min(n, maxReopenRows)] {
		b.WriteString(padRight(truncate(m.sessionName(c), 40), nameW) + "  \033[90m" + filepath.Base(c.Cwd) + "\033[0m\n")
	}
	if n > maxReopenRows {
		fmt.Fprintf(&b, "\033[90m+%d more\033[0m\n", n-maxReopenRows)
	}
	b.WriteString("\n\033[90m" + hints(keyEnter, "reopen all in new tabs", keyEsc, "not now") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(b.String())
}

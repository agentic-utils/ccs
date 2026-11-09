// Package sessions is the Claude Code and Codex sessions running on this
// machine: which are live and their status (~/.claude/sessions, Codex's
// processes and rollouts), messages to them over their sockets with
// delivery receipts, their terminal tabs (tmux, iTerm, T3 Code, background
// agents), ccs launch and close, and ccs:// links.
package sessions

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// LiveInterval is how often ccs re-reads which sessions are live and picks
// up new lines in live (and just-exited) sessions. Cheap: ~10 small files
// plus one stat per live transcript, and only appended lines are parsed.
const LiveInterval = 2 * time.Second

// OpenedGrace is how long a session ccs opened in a tab counts as live before
// claude has written its session file.
const OpenedGrace = 15 * time.Second

// LiveReads marks transcripts with a live-tick read in flight.
var LiveReads sync.Map

// GetSessionsDir returns where Claude Code records running sessions, one
// <pid>.json per process. Declared as a variable so tests can override it.
var GetSessionsDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "sessions")
}

// ReadLiveSessions returns the SessionIDs of conversations currently open in a
// running claude process. A session file whose pid is gone is a stale leftover.
// ponytail: a recycled pid can make a stale file look live until claude cleans it up.
func ReadLiveSessions() map[string]bool {
	live := make(map[string]bool)
	for id := range LiveSessionPIDs() {
		live[id] = true
	}
	return live
}

// LiveSessionPIDs maps each live SessionID to the pid of its claude process.
func LiveSessionPIDs() map[string]int {
	out := make(map[string]int)
	for id, s := range ScanSessions(nil) {
		out[id] = s.Pid
	}
	return out
}

// LiveSession is a live session's pid and what its file says, read once.
type LiveSession struct {
	Pid        int
	Stat       SessionStat
	Start      string // the file's procStart, so a kept session's pid can be checked later
	Unverified bool   // the pid's start time couldn't be checked against the file's: maybe a recycled pid
	misses     int    // ticks in a row it was kept without a readable file
	Codex      bool   // a Codex session (codexLiveSessions), never kept from an earlier scan
}

// KeepTicks is how long a session without a readable file is kept when its
// process's start time was never known, so a recycled pid can't be ruled out.
const KeepTicks = 3

// ScanSessions reads ~/.claude/sessions once: each live SessionID's pid and
// status. A session in prev (the last scan) whose file is missing or can't be
// read this time isn't one that ended: it's kept, with no status so nothing
// changes, while its pid is still the process that wrote it (same start time,
// or for keepTicks when that was never known), and not once its pid's file
// names another session (/clear).
func ScanSessions(prev map[string]LiveSession) map[string]LiveSession {
	type candidate struct {
		id, start string
		stat      SessionStat
	}
	byPid := make(map[int][]candidate)
	files, _ := filepath.Glob(filepath.Join(GetSessionsDir(), "*.json"))
	for _, f := range files {
		var s struct {
			SessionFile
			Pid       int    `json:"pid"`
			ProcStart string `json:"procStart"` // UTC, e.g. "Wed Sep 23 16:52:33 2026"
		}
		data, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(data, &s) != nil || s.SessionID == "" || s.Pid <= 0 {
			continue
		}
		if proc.PidAlive(s.Pid) {
			byPid[s.Pid] = append(byPid[s.Pid], candidate{s.SessionID, s.ProcStart, s.Stat()})
		}
	}
	// Check each process before mapping sessions: a stale file whose pid was
	// recycled must not overwrite the live file for the same session.
	pids := make(map[string]int, len(byPid)+len(prev))
	for pid := range byPid {
		pids[strconv.Itoa(pid)] = pid
	}
	for _, p := range prev {
		pids[strconv.Itoa(p.Pid)] = p.Pid
	}
	actual := proc.ProcessStartTimes(pids)
	live := make(map[string]LiveSession)
	for pid, cs := range byPid {
		for _, c := range cs {
			if match, checked := sameStart(c.start, actual[pid]); match {
				live[c.id] = LiveSession{Pid: pid, Stat: c.stat, Start: c.start, Unverified: !checked}
			}
		}
	}
	for id, c := range LiveCodexSessions() { // no session file: worked out afresh each time
		if _, ok := live[id]; !ok {
			live[id] = c
		}
	}
	for id, p := range prev {
		if _, ok := live[id]; ok || p.Codex || len(byPid[p.Pid]) > 0 || !proc.PidAlive(p.Pid) {
			continue
		}
		if match, checked := sameStart(p.Start, actual[p.Pid]); match && (checked || p.misses < KeepTicks) {
			live[id] = LiveSession{Pid: p.Pid, Start: p.Start, Unverified: p.Unverified, misses: p.misses + 1}
		}
	}
	return live
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

// OpenResumeTab tries to resume conv in a new tmux window or iTerm tab. Returns
// false when that isn't possible, leaving the caller to exec claude in place.
func OpenResumeTab(conv transcript.Conversation, claudeFlags []string) (bool, error) {
	cwd := conv.Cwd
	if cwd == "" || cwd == "unknown" {
		cwd = "."
	}
	var args []string
	if conv.Kind == transcript.KindCodex { // claude's flags don't apply; a fork asks for one
		codexPath, err := exec.LookPath("codex")
		if err != nil {
			return false, nil
		}
		args = transcript.CodexResumeArgs(codexPath, conv, slices.Contains(claudeFlags, "--fork-session"))
	} else {
		claudePath, err := exec.LookPath("claude")
		if err != nil {
			return false, nil
		}
		args = append([]string{claudePath, "--resume", conv.SessionID}, claudeFlags...)
	}
	// tmux first: inside tmux the iTerm tab would land outside the session.
	if handled, err := ResumeInTmuxWindow(cwd, args); handled {
		return true, err
	}
	return ResumeInITermTab(cwd, args)
}

// ResumeInTmuxWindow opens args in a new background tmux window. handled is
// false outside tmux. An error after trying means the window may or may not
// have opened, so the caller must not launch another copy.
func ResumeInTmuxWindow(cwd string, args []string) (handled bool, err error) {
	if os.Getenv("TMUX") == "" {
		return false, nil
	}
	// -d leaves the current window (ccs) focused. tmux expands formats in -c,
	// including #(command), so a '#' in the path must be escaped.
	tmuxArgs := append([]string{"new-window", "-d", "-c", strings.ReplaceAll(cwd, "#", "##")}, args...)
	return tabResult(proc.RunBounded(5*time.Second, nil, "tmux", tmuxArgs...))
}

// focusSession brings the terminal running pid to the front: its tmux pane
// when ccs is in tmux, else its iTerm tab. Matched by the process's tty.
// InITerm: running in iTerm, directly or in tmux inside it. tmux sets
// TERM_PROGRAM to tmux, but iTerm's LC_TERMINAL and ITERM_SESSION_ID get
// through, so a session in an ordinary iTerm tab can still be found.
func InITerm() bool {
	return os.Getenv("TERM_PROGRAM") == "iTerm.app" || os.Getenv("LC_TERMINAL") == "iTerm2" || os.Getenv("ITERM_SESSION_ID") != ""
}

func FocusSession(pid int, activate bool) bool {
	out, err := proc.RunBounded(2*time.Second, nil, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	tty := strings.TrimSpace(string(out))
	if err != nil || tty == "" || tty == "??" {
		return false
	}
	return FocusTTY("/dev/"+tty, activate)
}

// FocusTTY selects the tmux pane or iTerm tab on tty. Stubbed in tests.
var FocusTTY = func(tty string, activate bool) bool {
	if os.Getenv("TMUX") != "" {
		panes, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
		if target := TmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			_, err := proc.RunBounded(5*time.Second, nil, "tmux", "switch-client", "-t", target, ";", "select-window", "-t", target, ";", "select-pane", "-t", target)
			return err == nil
		}
	}
	if !InITerm() {
		return false
	}
	out, err := iTermSelect(tty, activate)
	return err == nil && strings.TrimSpace(string(out)) == "found"
}

// iTermSelect selects the iTerm session on tty, printing "found" when there is
// one. activate brings iTerm in front of other apps; auto-focus doesn't.
func iTermSelect(tty string, activate bool) ([]byte, error) {
	front := ""
	if activate {
		front = "activate"
	}
	script := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					select w
					tell t to select
					tell s to select
					%s
					return "found"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty, front)
	return proc.RunBounded(5*time.Second, nil, "osascript", "-e", script)
}

// TmuxPaneForTTY picks the pane target whose tty matches from
// `tmux list-panes -a -F '#{pane_tty} <target>'` output.
func TmuxPaneForTTY(panes, tty string) string {
	for _, line := range strings.Split(panes, "\n") {
		if t, target, ok := strings.Cut(line, " "); ok && t == tty {
			return target
		}
	}
	return ""
}

// ResumeInITermTab opens the resume command in a new iTerm tab. Returns false
// if we're not in iTerm or osascript failed, so the caller can exec in place.
// ponytail: osascript, not the iTerm python API - no deps, no daemon.
func ResumeInITermTab(cwd string, args []string) (handled bool, err error) {
	if !InITerm() || !proc.ShellSafe(cwd) || slices.IndexFunc(args, func(a string) bool { return !proc.ShellSafe(a) }) >= 0 {
		return false, nil // exec in place instead: no shell involved
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = proc.ShellQuote(a)
	}
	cmd := "cd " + proc.ShellQuote(cwd) + " && exec " + strings.Join(quoted, " ")
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
	return tabResult(proc.RunBounded(5*time.Second, nil, "osascript", "-e", script))
}

// tabResult maps a tab-open attempt to (handled, err): success is handled; a
// timeout is handled with an error (it may have opened, so launching again
// could run two claudes on one transcript); any other failure (Automation
// denied, stale $TMUX) is unhandled, so the caller resumes in place.
func tabResult(_ []byte, err error) (bool, error) {
	if err == nil || errors.Is(err, proc.ErrTimedOut) {
		return true, err
	}
	return false, nil
}

// OpenRecordPath is where ccs notes which sessions are open and on which boot,
// so after a restart it can offer to reopen them (Claude Code's own session
// files don't survive it).
var OpenRecordPath = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccs", "open-sessions.json")
}()

type OpenRecord struct {
	Boot     string   `json:"boot"`
	Sessions []string `json:"sessions"`
}

// BootID names the current boot: Linux's boot_id, else macOS's boot time.
var BootID = func() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return strings.TrimSpace(string(b))
	}
	out, _ := proc.RunBounded(2*time.Second, nil, "sysctl", "-n", "kern.boottime")
	return strings.TrimSpace(string(out))
}

// LastOpenRecord is the record as last written, so an unchanged set isn't
// written again every tick. Only the live tick touches it, one at a time.
var LastOpenRecord string

// RecordOpen writes the live sessions with this boot's id when the set changes.
// Atomic, so a restart mid-write leaves the previous record, not half of one.
func RecordOpen(live map[string]bool) {
	boot := BootID()
	if boot == "" {
		return
	}
	rec := OpenRecord{Boot: boot, Sessions: slices.Sorted(maps.Keys(live))}
	data, _ := json.Marshal(rec)
	if string(data) == LastOpenRecord {
		return
	}
	if os.MkdirAll(filepath.Dir(OpenRecordPath), 0o700) != nil {
		return
	}
	tmp := OpenRecordPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil && os.Rename(tmp, OpenRecordPath) == nil {
		LastOpenRecord = string(data)
	}
}

// MaxReopenRows is how many sessions the reopen popup names before "+N more".
const MaxReopenRows = 15

// HostOf is the app a claude process runs in when it has no terminal (an app
// such as T3 Code started it with pipes), "" when it has one. ccs can't switch
// to such a session, only message it. Asked once per process (pid and its
// start time, so a recycled pid is asked again).
var HostOf = func(pid int, start string) string {
	key := fmt.Sprint(pid, " ", start)
	if h, ok := hosts.Load(key); ok {
		return h.(string)
	}
	h := findHost(pid)
	hosts.Store(key, h)
	return h
}

// hosts caches hostOf.
var hosts sync.Map

// findHost asks ps for pid's terminal and, without one, walks up its parents
// to the first macOS app bundle, else names its parent command.
func findHost(pid int) string {
	out, err := proc.RunBounded(2*time.Second, nil, "ps", "-o", "tty=,ppid=", "-p", strconv.Itoa(pid))
	f := strings.Fields(string(out))
	if err != nil || len(f) != 2 || f[0] != "??" {
		return "" // a terminal, or unknown: treated as one
	}
	parent, first := f[1], ""
	for range 6 {
		out, err := proc.RunBounded(2*time.Second, nil, "ps", "-o", "ppid=,comm=", "-p", parent)
		pp, comm, ok := strings.Cut(strings.TrimSpace(string(out)), " ")
		if err != nil || !ok {
			break
		}
		comm = strings.TrimSpace(comm)
		if i := strings.Index(comm, ".app/"); i >= 0 {
			return AppName(filepath.Base(comm[:i]))
		}
		if first == "" {
			first = filepath.Base(comm)
		}
		if parent = strings.TrimSpace(pp); parent == "0" || parent == "1" {
			break
		}
	}
	if first == "" {
		return "another app"
	}
	return first
}

// T3App is the name hostOf gives T3 Code, whose threads ccs opens by link.
const T3App = "T3 Code"

// T3Link is the T3 Code link to the thread claude process pid runs in. T3
// Code puts the thread's id and its CLI in the process's environment; the
// CLI's link for the thread names its environment.
var T3Link = func(pid int) (string, error) {
	out, err := proc.RunBounded(2*time.Second, nil, "ps", "-E", "-ww", "-o", "command=", "-p", strconv.Itoa(pid))
	if err != nil {
		return "", err
	}
	var thread, cli string
	for _, f := range strings.Fields(string(out)) { // the environment follows the command, so the last wins
		if v, ok := strings.CutPrefix(f, "T3CODE_THREAD_ID="); ok {
			thread = v
		} else if v, ok := strings.CutPrefix(f, "T3CODE_CLI_PATH="); ok {
			cli = v
		}
	}
	if !T3ID(thread) || !filepath.IsAbs(cli) {
		return "", errors.New("no T3 Code thread in its environment")
	}
	out, err = proc.RunBounded(5*time.Second, nil, cli, "thread", "show", "--json", thread)
	if err != nil {
		return "", err
	}
	var shown struct {
		Thread struct{ Link string } `json:"thread"`
	}
	if err := json.Unmarshal(out, &shown); err != nil {
		return "", err
	}
	u, err := url.Parse(shown.Thread.Link) // http://host:port/<environment>/<thread>
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if env := parts[0]; len(parts) == 2 && T3ID(env) {
		return "t3code://threads/" + env + "/" + thread, nil
	}
	return "", fmt.Errorf("unexpected T3 Code link %q", shown.Thread.Link)
}

// T3ID: a T3 Code id (a uuid, or mcp:… for threads an agent started), safe
// to put in a link.
func T3ID(s string) bool {
	return s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789:-") == ""
}

// OpenApp hands an app's own link to the system. A variable so tests never
// open anything.
var OpenApp = func(link string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	_, err := proc.RunBounded(5*time.Second, nil, opener, link)
	return err
}

// BgSession is a background session (claude --bg) as claude agents lists it:
// the short id attach, logs and stop take, and its state (working, blocked,
// done, failed, stopped).
type BgSession struct{ ID, State string }

// AgentsInterval is how often the live tick asks claude agents.
const AgentsInterval = 10 * time.Second

// ListAgents is the background sessions by SessionID, from claude agents
// --json. A variable so tests never run the real one.
var ListAgents = realListAgents

func realListAgents() (map[string]BgSession, error) {
	out, err := proc.RunBounded(5*time.Second, nil, "claude", "agents", "--json")
	if err != nil {
		return nil, err
	}
	var all []struct {
		Kind      string `json:"kind"`
		ID        string `json:"id"`
		State     string `json:"state"`
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, err
	}
	bg := map[string]BgSession{}
	for _, a := range all {
		if a.Kind == "background" && a.ID != "" && a.SessionID != "" {
			bg[a.SessionID] = BgSession{a.ID, a.State}
		}
	}
	return bg, nil
}

// AppName is an app's name without a parenthesised edition: "T3 Code (Beta)"
// reads "T3 Code", short enough for the list's grey row.
func AppName(s string) string {
	if i := strings.Index(s, " ("); i > 0 {
		return s[:i]
	}
	return s
}

// ccs://session/<name> switches to that session's tab. macOS delivers a URL as
// an Apple event, not an argument, and a cgo-free Go binary can't receive one,
// so a small AppleScript app in ~/Applications takes it and runs `ccs link open`.
const (
	linkPrefix   = "ccs://session/"
	linkBundleID = "io.github.agentic-utils.ccs-links"
	// linkAppVersion changes when linkApplet or the app's plist does; a change
	// rebuilds the app, and macOS then asks again for permission to control iTerm.
	linkAppVersion = "1"
	LinkVersionKey = "CCSLinkVersion"
	LinkTargetKey  = "CCSLinkTarget"
)

var (
	LinkAppPath = func() string {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Applications", "CCS Links.app")
	}
	Lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

	// Stubbed in tests.
	namedSessions = SessionsNamed
	FocusLinkTTY  = RealFocusLinkTTY
	ITermRunning  = realITermRunning
	UrlHandlerApp = realURLHandlerApp
)

// parseSessionLink is the session a link names. The link comes from anywhere,
// so it's matched whole against one exact shape, nothing decoded: no query,
// fragment, user, extra path or percent-escapes. Errors don't repeat it.
func parseSessionLink(raw string) (string, error) {
	name, ok := strings.CutPrefix(raw, linkPrefix)
	if !ok || !state.SessionName.MatchString(name) {
		return "", errors.New("that isn't a valid ccs link, so nothing was opened")
	}
	return name, nil
}

// openLink switches to the running session a link names. It only ever focuses:
// a session that isn't running is reported, never started or resumed.
func openLink(raw string) (string, error) {
	name, err := parseSessionLink(raw)
	if err != nil {
		return "", err
	}
	pids, err := namedSessions(name)
	if err != nil {
		return "", err
	}
	switch len(pids) {
	case 0:
		return "", fmt.Errorf("no session named %s is running, so nothing was opened", name)
	case 1:
	default:
		return "", fmt.Errorf("%d running sessions are named %s, so there's no telling which you meant", len(pids), name)
	}
	tty := proc.TtyOf(pids[0])
	if tty == "" {
		return "", fmt.Errorf("%s has no terminal tab to switch to: an app or claude --bg started it", name)
	}
	if err := FocusLinkTTY(tty); err != nil {
		return "", fmt.Errorf("couldn't switch to %s: %v", name, err)
	}
	return "switched to " + name, nil
}

// RealFocusLinkTTY selects the tmux window or iTerm tab on tty and brings it to
// the front. The handler app starts ccs with neither $TMUX nor $TERM_PROGRAM,
// so unlike focusTTY this asks tmux and iTerm directly. It never starts iTerm.
func RealFocusLinkTTY(tty string) error {
	panes, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
	if target := TmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
		return focusTmuxTarget(target)
	}
	if !ITermRunning() {
		return errors.New("it isn't in a tmux window or an iTerm tab")
	}
	out, err := iTermSelect(tty, true)
	if err != nil {
		return iTermError(err)
	}
	if strings.TrimSpace(string(out)) != "found" {
		return errors.New("it isn't in a tmux window or an iTerm tab")
	}
	return nil
}

// focusTmuxTarget points tmux's most recently active client at target, then
// raises that client's iTerm tab if it has one. A client in another terminal
// switches but its window stays where it is.
func focusTmuxTarget(target string) error {
	out, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-clients", "-F", "#{client_activity} #{client_tty}")
	client := latestTmuxClient(string(out))
	if err != nil || client == "" {
		return errors.New("its tmux session isn't attached to a terminal")
	}
	if _, err := proc.RunBounded(5*time.Second, nil, "tmux", "switch-client", "-c", client, "-t", target, ";", "select-window", "-t", target, ";", "select-pane", "-t", target); err != nil {
		return fmt.Errorf("tmux wouldn't switch: %v", err)
	}
	if ITermRunning() {
		iTermSelect(client, true)
	}
	return nil
}

// latestTmuxClient picks the tty from `#{client_activity} #{client_tty}` lines.
func latestTmuxClient(lines string) string {
	best, bestAt := "", int64(-1)
	for _, line := range strings.Split(lines, "\n") {
		at, tty, ok := strings.Cut(strings.TrimSpace(line), " ")
		if n, err := strconv.ParseInt(at, 10, 64); ok && err == nil && n > bestAt {
			best, bestAt = tty, n
		}
	}
	return best
}

// iTermError says what to do about a failed osascript call. -1743 is macOS
// refusing the app permission to control iTerm.
func iTermError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "-1743") {
		return errors.New("macOS hasn't allowed it to control iTerm: turn on CCS Links under iTerm in System Settings, Privacy & Security, Automation")
	}
	return fmt.Errorf("iTerm wouldn't switch: %v", err)
}

// realITermRunning looks in the process list, since naming iTerm in AppleScript
// would start it, and opening a link must not start anything.
func realITermRunning() bool {
	out, err := proc.RunBounded(3*time.Second, nil, "ps", "-axo", "comm=")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "/MacOS/iTerm2") {
			return true
		}
	}
	return false
}

// realURLHandlerApp is the app macOS would open a ccs link with, "" for none.
func realURLHandlerApp() string {
	script := `ObjC.import("AppKit");
var app = $.NSWorkspace.sharedWorkspace.URLForApplicationToOpenURL($.NSURL.URLWithString("` + linkPrefix + `x"));
app.isNil() ? "" : app.path.js`
	out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-l", "JavaScript", "-e", script)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// linkTarget is the ccs the app runs. A Homebrew keg's folder changes with every
// upgrade but its bin symlink doesn't, so the app keeps working across upgrades.
func linkTarget(exe string) string {
	if prefix, _, ok := strings.Cut(exe, "/Cellar/"); ok {
		bin := filepath.Join(prefix, "bin", "ccs")
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}
	return exe
}

// appleScriptString is s as an AppleScript string literal.
func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// linkApplet is the handler app's source. The URL only ever reaches the shell
// through `quoted form of`, and ccs validates it before using it.
func linkApplet(target string) string {
	return fmt.Sprintf(`on open location theURL
	try
		do shell script (quoted form of %s) & " link open " & quoted form of theURL
	on error errorText
		activate
		display alert "ccs link" message errorText as warning giving up after 15
	end try
end open location

on run
	activate
	display alert "CCS Links" message "This app opens ccs://session/<name> links by switching to that session's tab. There's nothing to do here." giving up after 15
end run
`, appleScriptString(target))
}

// LinkPlistValue reads one key of the app's Info.plist, "" when it can't.
func LinkPlistValue(app, key string) string {
	out, err := proc.RunBounded(5*time.Second, nil, "plutil", "-extract", key, "raw", "-o", "-", filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// linkAppCurrent reports whether the app at path is this version's, running target.
func linkAppCurrent(app, target string) bool {
	return LinkPlistValue(app, LinkVersionKey) == linkAppVersion && LinkPlistValue(app, LinkTargetKey) == target
}

// InstallLinkApp builds the handler app and registers it. An app already right
// is only registered again: rebuilding would make macOS ask for permission again.
func InstallLinkApp(target string) error {
	if !proc.ShellSafe(target) {
		return fmt.Errorf("can't use ccs at %q: the path has characters the handler can't quote", target)
	}
	app := LinkAppPath()
	if _, err := os.Stat(app); err == nil {
		if LinkPlistValue(app, "CFBundleIdentifier") != linkBundleID {
			return fmt.Errorf("%s exists and isn't ccs's: move it first", app)
		}
		if linkAppCurrent(app, target) {
			return registerLinkApp(app)
		}
	}
	if err := buildLinkApp(app, target); err != nil {
		return err
	}
	return registerLinkApp(app)
}

// buildLinkApp compiles the app in a temp folder, then moves it into place.
func buildLinkApp(app, target string) error {
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(cache, "ccs-link-*") // beside home, so the renames stay on one volume
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	source := filepath.Join(tmp, "handler.applescript")
	if err := os.WriteFile(source, []byte(linkApplet(target)), 0o600); err != nil {
		return err
	}
	built := filepath.Join(tmp, filepath.Base(app))
	run := func(name string, args ...string) error {
		if _, err := proc.RunBounded(30*time.Second, nil, name, args...); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		return nil
	}
	if err := run("osacompile", "-o", built, source); err != nil {
		return err
	}
	plist := filepath.Join(built, "Contents", "Info.plist")
	for _, edit := range [][]string{
		{"CFBundleIdentifier", "-string", linkBundleID},
		{"CFBundleURLTypes", "-json", `[{"CFBundleURLName":"ccs session link","CFBundleURLSchemes":["ccs"]}]`},
		{"LSUIElement", "-bool", "true"}, // no Dock icon for a click
		{"NSAppleEventsUsageDescription", "-string", "CCS Links switches to a session's iTerm tab when you open a ccs:// link."},
		{LinkVersionKey, "-string", linkAppVersion},
		{LinkTargetKey, "-string", target},
	} {
		if err := run("plutil", append([]string{"-replace"}, append(edit, plist)...)...); err != nil {
			return err
		}
	}
	// The plist edits broke the signature osacompile gave it.
	if err := run("codesign", "--force", "--sign", "-", built); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(app), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(app); err == nil {
		if err := os.Rename(app, filepath.Join(tmp, "old.app")); err != nil {
			return err
		}
	}
	return os.Rename(built, app)
}

func registerLinkApp(app string) error {
	if _, err := proc.RunBounded(30*time.Second, nil, Lsregister, "-f", app); err != nil {
		return fmt.Errorf("couldn't register %s with macOS: %v", app, err)
	}
	return nil
}

// RefreshLinkApp rebuilds an installed handler after an update changed it, or
// when the ccs it points at has gone. It never installs one that isn't there.
func RefreshLinkApp() {
	if runtime.GOOS != "darwin" || state.Version == "dev" {
		return
	}
	app := LinkAppPath()
	if _, err := os.Stat(app); err != nil || LinkPlistValue(app, "CFBundleIdentifier") != linkBundleID {
		return
	}
	target := LinkPlistValue(app, LinkTargetKey)
	if _, err := os.Stat(target); err == nil && LinkPlistValue(app, LinkVersionKey) == linkAppVersion {
		return
	}
	exe, err := os.Executable()
	if err == nil {
		err = InstallLinkApp(linkTarget(exe))
	}
	state.LogActivity("link handler refreshed: %v", state.ErrOrOK(err))
}

// RunLink is `ccs link`: open is what the handler app runs, the others manage it.
func RunLink(args []string, stdout, stderr io.Writer) int {
	usage := "usage: ccs link install | uninstall | status | open ccs://session/<name>"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if args[0] == "open" {
		if len(args) != 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		// The handler app's PATH is only the system's, without Homebrew's tmux.
		os.Setenv("PATH", os.Getenv("PATH")+":/opt/homebrew/bin:/usr/local/bin")
		msg, err := openLink(args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(stderr, "ccs links are macOS only")
		return 1
	}
	switch args[0] {
	case "install":
		return LinkInstall(stdout, stderr)
	case "uninstall":
		return LinkUninstall(stdout, stderr)
	case "status":
		return LinkStatus(stdout)
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

func LinkInstall(stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err == nil {
		err = InstallLinkApp(linkTarget(exe))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	app := LinkAppPath()
	handler := ""
	for range 10 { // macOS can take a moment to notice the new app
		if handler = UrlHandlerApp(); handler == app {
			fmt.Fprintf(stdout, "ccs:// links now open with %s\n", app)
			return 0
		}
		Sleep(300 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "installed %s, but macOS opens ccs:// links with %q\n", app, handler)
	return 1
}

func LinkUninstall(stdout, stderr io.Writer) int {
	app := LinkAppPath()
	if _, err := os.Stat(app); err != nil {
		fmt.Fprintln(stdout, "no handler installed")
		return 0
	}
	if LinkPlistValue(app, "CFBundleIdentifier") != linkBundleID {
		fmt.Fprintf(stderr, "%s isn't ccs's, so it was left alone\n", app)
		return 1
	}
	if _, err := proc.RunBounded(30*time.Second, nil, Lsregister, "-u", app); err != nil {
		fmt.Fprintf(stderr, "couldn't unregister %s: %v\n", app, err)
		return 1
	}
	if err := os.RemoveAll(app); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "removed %s\n", app)
	return 0
}

// LinkStatus says whether a link would work; 0 only when ours handles them.
func LinkStatus(stdout io.Writer) int {
	app := LinkAppPath()
	handler := UrlHandlerApp()
	if handler == "" {
		fmt.Fprintln(stdout, "nothing handles ccs:// links: run ccs link install")
		return 1
	}
	fmt.Fprintf(stdout, "ccs:// links open with %s\n", handler)
	if handler != app {
		fmt.Fprintln(stdout, "that isn't ccs's handler: run ccs link install")
		return 1
	}
	fmt.Fprintf(stdout, "which runs %s\n", LinkPlistValue(app, LinkTargetKey))
	return 0
}

// LaunchOpts says what to start: a brief (the first prompt), or a session to
// resume, under a name, in a folder.
type LaunchOpts struct {
	Name, Brief, Resume, Cwd string
	Codex                    bool
	SessionID                string        // claude's id for a new session, known before it starts
	Focus                    bool          // bring its tab to the front: it needs an answer there
	Wait                     time.Duration // how long it may take to start; launchWait if zero
	InPlace                  bool          // no tab: replace this process with it
}

// LaunchWait is how long launch waits for the session to show up, and
// CloseWait how long close waits for its process to exit.
var (
	LaunchWait = 20 * time.Second
	CloseWait  = 10 * time.Second
)

// LookBin finds claude or codex (ccs isn't a shell, so no alias or function
// stands in the way). Stubbed in tests.
var LookBin = exec.LookPath

// launchScript is the launcher a new tab runs: a login zsh, so PATH and
// CCS_CLAUDE_ARGS are the user's; the secrets file the user's claude()
// wrapper sources; then claude with the brief first, since --channels (in
// CCS_CLAUDE_ARGS) takes every word after it as a channel.
func launchScript(o LaunchOpts, bin, dir string) string {
	var b strings.Builder
	b.WriteString("#!/bin/zsh -l\n")
	b.WriteString("cd " + proc.ShellQuote(o.Cwd) + " || exit 1\n")
	brief := `"$(cat ` + proc.ShellQuote(filepath.Join(dir, "brief.md")) + `)"`
	if o.Codex {
		args := []string{proc.ShellQuote(bin)}
		if o.Resume != "" {
			args = append(args, "resume", proc.ShellQuote(o.Resume))
		}
		// Its launch folder first among the writable ones: sessionsNamed finds
		// it by that.
		flags := transcript.CodexSandboxArgs(dir)
		for i, a := range flags {
			if i > 0 && flags[i-1] == "--add-dir" { // the folders; the flags themselves are plain words
				a = proc.ShellQuote(a)
			}
			args = append(args, a)
		}
		if o.Brief != "" {
			args = append(args, brief)
		}
		b.WriteString("exec " + strings.Join(args, " ") + "\n")
		return b.String()
	}
	b.WriteString("[ -f ~/.claude.secret ] && source ~/.claude.secret\n")
	args := []string{proc.ShellQuote(bin)}
	if o.Resume != "" {
		args = append(args, "--resume", proc.ShellQuote(o.Resume))
	}
	if o.SessionID != "" {
		args = append(args, "--session-id", proc.ShellQuote(o.SessionID))
	}
	if o.Brief != "" {
		args = append(args, brief)
	}
	args = append(args, "--name", proc.ShellQuote(o.Name), "${=CCS_CLAUDE_ARGS}")
	b.WriteString("exec " + strings.Join(args, " ") + "\n")
	return b.String()
}

// Launched is a session launch started: its process and terminal, and a
// Codex session's thread.
type Launched struct {
	Pid    int
	Tty    string
	thread string
}

// Resumable: id has a transcript to resume, a Claude Code session under
// the projects folder or, with codex, a Codex thread's rollout.
func Resumable(id string, codex bool) bool {
	if codex {
		return transcript.CodexRollout(id) != ""
	}
	if strings.ContainsAny(id, `/\*?[`) {
		return false
	}
	paths, _ := filepath.Glob(filepath.Join(transcript.GetProjectsDir(), "*", id+".jsonl"))
	return len(paths) > 0
}

// LaunchSession starts o in a new iTerm tab and waits until it's running.
func LaunchSession(o LaunchOpts) (Launched, error) {
	if !state.SessionName.MatchString(o.Name) {
		return Launched{}, fmt.Errorf("name %q: use letters, digits, '.', '_' and '-' (at most 64)", o.Name)
	}
	if o.Brief == "" && o.Resume == "" {
		return Launched{}, errors.New("give a brief or a session to resume")
	}
	if o.Resume != "" && !proc.ShellSafe(o.Resume) {
		return Launched{}, fmt.Errorf("bad session id %q", o.Resume)
	}
	if o.Resume != "" && !Resumable(o.Resume, o.Codex) {
		kind := "Claude Code session"
		if o.Codex {
			kind = "Codex thread"
		}
		return Launched{}, fmt.Errorf("no %s %q to resume", kind, o.Resume)
	}
	if o.Cwd == "" {
		o.Cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(o.Cwd); err == nil {
		o.Cwd = abs
	}
	if info, err := os.Stat(o.Cwd); err != nil || !info.IsDir() {
		return Launched{}, fmt.Errorf("no folder %s", o.Cwd)
	}
	if !proc.ShellSafe(o.Cwd) {
		return Launched{}, fmt.Errorf("folder %q can't go into a shell command", o.Cwd)
	}
	if !CanOpenTabs() && !o.InPlace {
		return Launched{}, errors.New("not in iTerm or tmux: launch opens a tab there")
	}
	if err := CheckLaunchShell(); err != nil {
		return Launched{}, err
	}
	if pids, err := SessionsNamed(o.Name); err != nil {
		return Launched{}, err
	} else if len(pids) > 0 {
		return Launched{}, fmt.Errorf("a session named %s is already running (pid %d)", o.Name, pids[0])
	}
	binName := "claude"
	if o.Codex {
		binName = "codex"
	}
	bin, err := LookBin(binName)
	if err != nil {
		return Launched{}, fmt.Errorf("%s isn't installed: %v", binName, err)
	}
	dir := filepath.Join(state.LaunchDir(), o.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Launched{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte(o.Brief), 0o600); err != nil {
		return Launched{}, err
	}
	script := filepath.Join(dir, "launch.sh")
	if err := os.WriteFile(script, []byte(launchScript(o, bin, dir)), 0o700); err != nil {
		return Launched{}, err
	}
	if o.InPlace {
		return Launched{}, ExecScript(script)
	}
	tty, err := OpenLaunchTab(script)
	if err != nil {
		return Launched{}, fmt.Errorf("couldn't open a tab: %v", err)
	}
	if o.Focus {
		FocusTTY(tty, true)
	}
	wait := LaunchWait
	if o.Wait > 0 {
		wait = o.Wait
	}
	deadline := time.Now().Add(wait)
	retypeAt := time.Now().Add(LaunchRetype)
	for {
		if !retypeAt.IsZero() && time.Now().After(retypeAt) {
			retypeAt = time.Time{}
			// The shell can draw (a "Last login" line) before it reads input,
			// and what was typed then is lost: type it again, once, if
			// nothing on the tab runs the launcher yet.
			if !LauncherRunning(tty, script) {
				state.LogActivity("launch %s: nothing ran the launcher on %s, typing it again", o.Name, tty)
				if err := RetypeLauncher(tty, script); err != nil {
					state.LogActivity("launch %s: couldn't type it again: %v", o.Name, err)
				}
			}
		}
		pids, _ := SessionsNamed(o.Name)
		thread := ""
		if o.Codex {
			thread = CodexThreadFor(o.Name) // queue and claim need it
		}
		if len(pids) > 0 && (!o.Codex || thread != "" || o.Resume != "") {
			return Launched{pids[0], tty, thread}, nil
		}
		if time.Now().After(deadline) {
			if len(pids) > 0 {
				return Launched{pids[0], tty, ""}, fmt.Errorf("%s started (pid %d) but Codex wrote no transcript for it within %s, so it can't be messaged: see the tab on %s", o.Name, pids[0], wait, tty)
			}
			return Launched{Tty: tty}, fmt.Errorf("%s didn't start within %s: see the tab on %s, where a folder-trust question may be waiting (launcher %s)", o.Name, wait, tty, script)
		}
		Sleep(250 * time.Millisecond)
	}
}

// LaunchRetype is how long after typing the launcher ccs checks that
// something on the tab runs it.
var LaunchRetype = 4 * time.Second

// LauncherRunning: a process on tty runs the launcher, or the claude or
// codex it execs.
var LauncherRunning = func(tty, script string) bool {
	out, err := proc.PsAll()
	if err != nil {
		return true // can't tell: never type into what may be running
	}
	short := strings.TrimPrefix(tty, "/dev/")
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != short {
			continue
		}
		cmd := strings.Join(f[2:], " ")
		if strings.Contains(cmd, script) || strings.Contains(f[2], "claude") || strings.Contains(f[2], "codex") {
			return true
		}
	}
	return false
}

// RetypeLauncher types ctrl-U and the launcher into the iTerm session on tty.
var RetypeLauncher = func(tty, script string) error {
	as := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					tell s to write text ((character id 21) & %q)
					return "typed"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty, proc.ShellQuote(script))
	out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-e", as)
	if err == nil && strings.TrimSpace(string(out)) != "typed" {
		err = errors.New("no iTerm session on " + tty)
	}
	return err
}

// Sleep is time.Sleep, stubbed in tests.
var Sleep = time.Sleep

// launchShell runs every launcher: launchScript needs zsh's word splitting.
const launchShell = "/bin/zsh"

// CheckLaunchShell says what's wrong before anything is changed or opened, since
// a launcher that can't run only shows up as a session that never starts.
// Stubbed in tests.
var CheckLaunchShell = func() error {
	if _, err := os.Stat(launchShell); err != nil {
		return fmt.Errorf("sessions are started with zsh, and there's none at %s", launchShell)
	}
	return nil
}

// tmuxWindowArgs opens a background window on the launcher. Several words, so
// tmux runs them directly: one word would go through a shell, which splits a
// path with a space in it.
func tmuxWindowArgs(script string) []string {
	return []string{"new-window", "-d", "-P", "-F", "#{pane_tty}", launchShell, "-l", script}
}

// CanOpenTabs: launch has somewhere to open a tab.
func CanOpenTabs() bool { return os.Getenv("TMUX") != "" || InITerm() }

// ExecScript replaces this process with a launcher. Stubbed in tests.
var ExecScript = func(script string) error {
	return syscall.Exec(script, []string{script}, os.Environ())
}

// OpenLaunchTab opens a background tmux window running the launcher or, outside
// tmux (an iTerm tab there would land outside the session), an iTerm tab (at
// the end of the strip if iTerm's "new tabs at the end" is on), waits for its
// shell to draw something, then types ctrl-U (any stray input gone) and the
// launcher. The tab that had focus keeps it. Returns the new tab's tty.
var OpenLaunchTab = func(script string) (string, error) {
	if os.Getenv("TMUX") != "" {
		out, err := proc.RunBounded(5*time.Second, nil, "tmux", tmuxWindowArgs(script)...)
		tty := strings.TrimSpace(string(out))
		if err == nil && tty == "" {
			err = errors.New("tmux returned no tty")
		}
		return tty, err
	}
	as := fmt.Sprintf(`tell application "iTerm2"
	if (count of windows) = 0 then
		create window with default profile
		set s to current session of current window
	else
		tell current window
			set oldTab to current tab
			set newTab to (create tab with default profile)
			set s to current session of newTab
			select oldTab
		end tell
	end if
	repeat 50 times
		if length of (contents of s as text) > 0 then exit repeat
		delay 0.1
	end repeat
	delay 0.3
	tell s to write text ((character id 21) & %q)
	return tty of s
end tell`, proc.ShellQuote(script))
	out, err := proc.RunBounded(15*time.Second, nil, "osascript", "-e", as)
	tty := strings.TrimSpace(string(out))
	if err == nil && tty == "" {
		err = errors.New("iTerm returned no tty")
	}
	return tty, err
}

// SessionsNamed is the pids of running sessions called name: claude sessions
// whose session file says so (its pid checked against the file's start
// time), and codex sessions ccs launched (their --add-dir is the name's
// launch folder).
func SessionsNamed(name string) ([]int, error) {
	var pids []int
	for _, s := range ScanSessions(nil) {
		if s.Stat.Name == name {
			pids = append(pids, s.Pid)
		}
	}
	out, err := proc.PsAll()
	if err != nil {
		return pids, nil // claude's sessions are still known
	}
	marker := "--add-dir " + filepath.Join(state.LaunchDir(), name) + " " // ps shows arguments unquoted
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		cmd := strings.Join(f[2:], " ") + " "
		if !strings.Contains(cmd, "codex") || !strings.Contains(cmd, marker) {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil && !containsInt(pids, pid) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// CloseByName closes the one running session called name.
func CloseByName(name string) (string, error) {
	pids, err := SessionsNamed(name)
	if err != nil {
		return "", err
	}
	switch len(pids) {
	case 0:
		return "", fmt.Errorf("no running session named %s", name)
	case 1:
	default:
		return "", fmt.Errorf("%d running sessions are named %s (pids %v): close one by hand", len(pids), name, pids)
	}
	return CloseSession(name, pids[0])
}

// CloseSession stops pid (SIGTERM, as quitting does), confirms it exited,
// then closes its tab: the tmux pane, or the iTerm session on its tty, one
// osascript call per tab (closing several in one loop shifts the indexes).
// Stopping first means iTerm never asks whether to close a running job.
func CloseSession(name string, pid int) (string, error) {
	tty := proc.TtyOf(pid)
	if tty == "" {
		return "", fmt.Errorf("%s (pid %d) has no terminal: an app or claude --bg started it, close it there", name, pid)
	}
	if err := proc.SignalPID(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return "", fmt.Errorf("couldn't stop %s (pid %d): %v", name, pid, err)
	}
	deadline := time.Now().Add(CloseWait)
	for proc.PidAlive(pid) {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s (pid %d) still running %s after SIGTERM; its tab on %s is left open", name, pid, CloseWait, tty)
		}
		Sleep(100 * time.Millisecond)
	}
	if !CloseTab(tty) {
		return "", fmt.Errorf("stopped %s (pid %d), but found no tab on %s to close", name, pid, tty)
	}
	return fmt.Sprintf("closed %s (pid %d, %s)", name, pid, tty), nil
}

// CloseTab closes the tmux pane or iTerm session on tty.
var CloseTab = realCloseTab

func realCloseTab(tty string) bool {
	if os.Getenv("TMUX") != "" {
		panes, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
		if target := TmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			_, err := proc.RunBounded(5*time.Second, nil, "tmux", "kill-pane", "-t", target)
			return err == nil
		}
		// A window launch opened runs the launcher itself, so it went with the session.
		if err == nil && !InITerm() {
			return true
		}
	}
	as := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					tell s to close
					return "closed"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty)
	out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-e", as)
	return err == nil && strings.TrimSpace(string(out)) == "closed"
}

// RunLaunch is `ccs launch`.
func RunLaunch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o LaunchOpts
	var briefFile string
	fs.StringVar(&o.Name, "name", "", "the session's name (required)")
	fs.StringVar(&briefFile, "brief", "", "a file whose text is the session's first prompt")
	fs.StringVar(&o.Resume, "resume", "", "a session id to resume instead (or as well)")
	fs.StringVar(&o.Cwd, "cwd", "", "the folder to start in (default: this one)")
	fs.BoolVar(&o.Codex, "codex", false, "start codex instead of claude")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs launch --name <name> (--brief <file> | --resume <id>) [--codex] [--cwd <dir>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.Name == "" || (briefFile == "" && o.Resume == "") || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	if briefFile != "" {
		data, err := os.ReadFile(briefFile)
		if err != nil {
			fmt.Fprintln(stderr, "ccs launch:", err)
			return 1
		}
		o.Brief = strings.TrimRight(string(data), "\n")
	}
	l, err := LaunchSession(o)
	if err != nil {
		fmt.Fprintln(stderr, "ccs launch:", err)
		return 1
	}
	if l.thread != "" {
		fmt.Fprintf(stdout, "launched %s: pid %d, %s, codex thread %s\n", o.Name, l.Pid, l.Tty, l.thread)
	} else {
		fmt.Fprintf(stdout, "launched %s: pid %d, %s\n", o.Name, l.Pid, l.Tty)
	}
	return 0
}

// RunClose is `ccs close`.
func RunClose(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: ccs close <name>")
		return 2
	}
	msg, err := CloseByName(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "ccs close:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// LaunchPrompt is ^X N's one-line box.
func LaunchPrompt(prompt, value string) textinput.Model {
	in := textinput.New()
	in.Prompt = prompt
	in.Width = 60
	in.SetValue(value)
	in.Focus()
	in.Cursor.SetMode(cursor.CursorStatic)
	return in
}

// TildePath is p with the home folder as ~.
func TildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// SessionStat is what a live session's file says about it right now.
type SessionStat struct {
	Status     string    // "busy", "idle", "waiting" (on the user) or "shell"
	WaitingFor string    // with "waiting": "permission prompt", "input needed"
	Since      time.Time // statusUpdatedAt; zero when the file has none
	Name       string
}

// SessionFile is the part of ~/.claude/sessions/<pid>.json ccs uses.
type SessionFile struct {
	SessionID       string `json:"sessionId"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"` // epoch ms
	WaitingFor      string `json:"waitingFor"`
	Socket          string `json:"messagingSocketPath"`
}

func (f SessionFile) Stat() SessionStat {
	st := SessionStat{Status: f.Status, WaitingFor: f.WaitingFor, Name: f.Name}
	if f.StatusUpdatedAt > 0 {
		st.Since = time.UnixMilli(f.StatusUpdatedAt)
	}
	return st
}

func ReadSessionFile(pid int) (SessionFile, error) {
	var f SessionFile
	data, err := os.ReadFile(filepath.Join(GetSessionsDir(), strconv.Itoa(pid)+".json"))
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(data, &f)
}

// ErrNotConnected means the socket couldn't be reached at all, so nothing
// was sent and the terminal fallback is safe.
var ErrNotConnected = errors.New("not connected")

// ErrUnsent means nothing reached the session, so the text goes back to its box.
var ErrUnsent = errors.New("not sent")

// SendEnvelope is sendViaSocket with its own sender name and leading note;
// replyTo gives a return address for receipts (only a process that serves
// them should ask for them).
func SendEnvelope(path, token, sender, note, text string, replyTo bool) (string, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotConnected, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	var out bytes.Buffer
	if token != "" {
		line, _ := json.Marshal(map[string]string{"type": "auth", "token": token})
		out.Write(append(line, '\n'))
	}
	body := strings.ReplaceAll(text, "</cross-session-message>", "</ cross-session-message>")
	msgID := newUUID()
	msg := map[string]any{
		"type":    "user",
		"message": map[string]string{"role": "user", "content": "<cross-session-message from-name=\"" + sender + "\">\n" + note + body + "\n</cross-session-message>"},
		"msg_id":  msgID,
		"msgV":    1,
	}
	// A return address lets the session send receipts (held, refused…); it
	// only replies to one in its own socket folder.
	if replyTo {
		if inbox := receiptInbox(filepath.Dir(path)); inbox != "" {
			msg["from"] = "uds:" + inbox
		}
	}
	line, _ := json.Marshal(msg)
	out.Write(append(line, '\n'))
	if _, err := conn.Write(out.Bytes()); err != nil {
		return msgID, err
	}
	time.Sleep(150 * time.Millisecond) // as Claude Code's own sender does before ending
	if uc, ok := conn.(*net.UnixConn); ok {
		return msgID, uc.CloseWrite()
	}
	return msgID, nil
}

// Delivery receipts. A Claude Code session sends one back, as a
// {"type":"control","action":"peer_message_status"} line, only when a
// message doesn't go straight in: held for approval (bypass-permissions
// sessions), then delivered, denied or expired; refused; or dropped. It
// connects to the "from" address, which must be a .sock in the recipient's
// own socket folder, and checks the listener is the process that sent.
var (
	inboxMu    sync.Mutex
	inboxes    = map[string]net.Listener{} // socket folder -> ccs's listener there
	SentMsgs   sync.Map                    // msg_id -> SessionID
	ReceiptsCh = make(chan ReceiptMsg, 16)
)

type ReceiptMsg struct {
	Id, Status, Reason string // id is the SessionID the message went to
}

// receiptInbox returns ccs's listening socket in dir, starting it on first
// use; "" if it can't (the send then just goes without a return address).
func receiptInbox(dir string) string {
	inboxMu.Lock()
	defer inboxMu.Unlock()
	if l, ok := inboxes[dir]; ok {
		return l.Addr().String()
	}
	path := filepath.Join(dir, fmt.Sprintf("ccs-%d-inbox.sock", os.Getpid()))
	if len(path) > 100 { // unix socket paths are limited to ~104 bytes
		return ""
	}
	reapInboxes(dir)
	os.Remove(path) // our own pid's leftover, if any
	l, err := net.Listen("unix", path)
	if err != nil {
		return ""
	}
	os.Chmod(path, 0o600)
	inboxes[dir] = l
	go serveReceipts(l)
	return path
}

// reapInboxes removes inbox sockets left by ccs processes that have exited.
func reapInboxes(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "ccs-*-inbox.sock"))
	for _, m := range matches {
		var pid int
		if _, err := fmt.Sscanf(filepath.Base(m), "ccs-%d-inbox.sock", &pid); err != nil || pid == os.Getpid() {
			continue
		}
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			os.Remove(m)
		}
	}
}

func serveReceipts(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return // closed
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			sc := bufio.NewScanner(conn)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				if r, ok := ParseReceipt(sc.Bytes()); ok {
					select {
					case ReceiptsCh <- r:
					default: // nobody's reading fast enough; the heuristic still covers it
					}
				}
			}
		}()
	}
}

// ParseReceipt reads one line sent to ccs's inbox; anything but a receipt
// for a message ccs sent (an auth line, a reply, an idle notice) is ignored.
func ParseReceipt(line []byte) (ReceiptMsg, bool) {
	var r struct {
		Type, Action, Status, Reason string
		StatusDetail                 string   `json:"status_detail"`
		OrigMsgID                    string   `json:"orig_msg_id"`
		DropReason                   string   `json:"drop_reason"`
		DroppedMsgIDs                []string `json:"dropped_msg_ids"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "control" || r.Action != "peer_message_status" {
		return ReceiptMsg{}, false
	}
	if r.Status == "expired" && r.StatusDetail == "refused" {
		r.Status = "refused"
	}
	if r.Status == "dropped" && r.DropReason != "" {
		r.Reason = r.DropReason
	}
	for _, id := range append([]string{r.OrigMsgID}, r.DroppedMsgIDs...) {
		if sid, ok := SentMsgs.Load(id); ok && id != "" {
			return ReceiptMsg{Id: sid.(string), Status: r.Status, Reason: r.Reason}, true
		}
	}
	return ReceiptMsg{}, false
}

// CloseInboxes stops ccs's receipt sockets and removes their files.
func CloseInboxes() {
	inboxMu.Lock()
	defer inboxMu.Unlock()
	for dir, l := range inboxes {
		l.Close() // Go removes the socket file on Close
		delete(inboxes, dir)
	}
}

// PeerToken reads the recipient's peer token from
// ~/.claude/sessions/<pid>.<sha256(socket path)>.key, "" if there isn't one
// (macOS sessions don't require it by default).
func PeerToken(pid int, socket string) string {
	paths := []string{socket}
	if real, err := filepath.EvalSymlinks(socket); err == nil && real != socket {
		paths = append(paths, real)
	}
	for _, p := range paths {
		sum := sha256.Sum256([]byte(p))
		data, err := os.ReadFile(filepath.Join(GetSessionsDir(), fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:]))))
		if err != nil {
			continue
		}
		var k struct {
			PeerToken string `json:"peerToken"`
		}
		if json.Unmarshal(data, &k) == nil && k.PeerToken != "" {
			return k.PeerToken
		}
	}
	return ""
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func ExecParentsPath() string {
	return filepath.Join(filepath.Dir(state.MarksPath), "codex-exec-parents.json")
}

// ExecParents maps a codex exec thread to the Claude session that ran it.
// seen remembers each exec process already linked (by pid and start), so a
// long run isn't looked up again every tick.
var ExecParents = struct {
	sync.Mutex
	Loaded bool
	m      map[string]string
	seen   map[string]bool
}{}

func loadExecParents() {
	if ExecParents.Loaded {
		return
	}
	ExecParents.Loaded, ExecParents.m, ExecParents.seen = true, map[string]string{}, map[string]bool{}
	if data, err := os.ReadFile(ExecParentsPath()); err == nil {
		json.Unmarshal(data, &ExecParents.m)
	}
}

// ExecParentOf is the Claude session that ran codex thread, "" if none was seen.
func ExecParentOf(thread string) string {
	ExecParents.Lock()
	defer ExecParents.Unlock()
	loadExecParents()
	return ExecParents.m[thread]
}

// codexValueFlags take a value, so the word after one isn't the subcommand.
var codexValueFlags = map[string]bool{
	"-c": true, "--config": true, "--enable": true, "--disable": true, "-p": true, "--profile": true,
	"-m": true, "--model": true, "-s": true, "--sandbox": true, "-a": true, "--ask-for-approval": true,
	"-C": true, "--cd": true, "--add-dir": true, "-i": true, "--image": true, "--remote": true,
	"--remote-auth-token-env": true, "--local-provider": true,
}

// codexExecArgs: a command line that is `codex [options] exec ...`, and the
// folder it works in when -C/--cd says ("" otherwise).
func codexExecArgs(fields []string) (bool, string) {
	for i, f := range fields {
		if b := filepath.Base(f); b != "codex" && b != "codex.js" {
			continue
		}
		cd := ""
		for j := i + 1; j < len(fields); j++ {
			a := fields[j]
			if codexValueFlags[a] && j+1 < len(fields) {
				if a == "-C" || a == "--cd" {
					cd = fields[j+1]
				}
				j++
				continue
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			return a == "exec" || a == "e", cd
		}
		return false, ""
	}
	return false, ""
}

// A headless `codex exec` run records nothing about who started it, so its
// parent is caught while it runs: on each live tick, a running `codex exec`
// whose process descends from a live claude process belongs to that
// claude's session. The link is kept on disk (ExecParentsPath), so the run
// stays nested after it exits and after ccs restarts. Runs ccs never saw
// alive keep the prompt match (findSpawnParent).

// NoteCodexExecs links the running codex exec runs to the live Claude
// session whose process they descend from. sessions is the live tick's scan.
func NoteCodexExecs(sessions map[string]LiveSession) {
	claude := map[int]string{}
	for id, s := range sessions {
		if !s.Codex {
			claude[s.Pid] = id
		}
	}
	if len(claude) == 0 {
		return
	}
	out, err := proc.PsTree()
	if err != nil {
		return
	}
	parent := map[int]int{}
	type run struct {
		pid int
		cd  string
	}
	var runs []run
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		parent[pid] = ppid
		if ok, cd := codexExecArgs(f[2:]); ok {
			runs = append(runs, run{pid, cd})
		}
	}
	if len(runs) == 0 {
		return
	}
	pids := map[string]int{}
	for _, r := range runs {
		pids[strconv.Itoa(r.pid)] = r.pid
	}
	starts := proc.ProcessStartTimes(pids)

	ExecParents.Lock()
	defer ExecParents.Unlock()
	loadExecParents()
	changed := false
	var rollouts []string
	for _, r := range runs {
		start := starts[r.pid]
		if start.IsZero() {
			continue // a recycled pid can't be ruled out
		}
		key := strconv.Itoa(r.pid) + " " + start.UTC().Format(time.RFC3339)
		if ExecParents.seen[key] {
			continue
		}
		session := ""
		for p, n := parent[r.pid], 0; p > 1 && n < 30; p, n = parent[p], n+1 {
			if id, ok := claude[p]; ok {
				session = id
				break
			}
		}
		if session == "" {
			ExecParents.seen[key] = true // not started by a Claude session
			continue
		}
		cwd := r.cd
		if cwd == "" {
			cwd = transcript.CodexProcCwd(r.pid)
		} else if !filepath.IsAbs(cwd) {
			if base := transcript.CodexProcCwd(r.pid); base != "" {
				cwd = filepath.Join(base, cwd)
			}
		}
		if cwd == "" {
			continue
		}
		if rollouts == nil {
			rollouts = transcript.CodexRollouts()
		}
		thread := execThread(rollouts, cwd, start)
		if thread == "" {
			continue // no rollout yet: tried again next tick
		}
		ExecParents.seen[key] = true
		if ExecParents.m[thread] != session {
			ExecParents.m[thread] = session
			changed = true
		}
	}
	if changed {
		saveExecParents()
	}
}

// execThread is the exec run's thread: the newest exec rollout in cwd that
// began at or after the process did.
func execThread(rollouts []string, cwd string, start time.Time) string {
	best, bestAt := "", time.Time{}
	for _, p := range rollouts {
		info, err := os.Stat(p)
		if err != nil || info.ModTime().Before(start) {
			continue
		}
		m, ok := transcript.ReadCodexMeta(p)
		if !ok || m.Cwd != cwd || codexSourceName(m.Source) != "exec" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m.Timestamp)
		if err != nil || at.Before(start.Add(-2*time.Second)) || at.Before(bestAt) {
			continue
		}
		best, bestAt = m.ID, at
	}
	return best
}

// codexSourceName is a session_meta source when it's a plain string ("exec",
// "cli"...), "" for a subagent's object.
func codexSourceName(raw json.RawMessage) string {
	var s string
	json.Unmarshal(raw, &s)
	return s
}

func saveExecParents() {
	data, err := json.Marshal(ExecParents.m)
	if err != nil {
		return
	}
	path := ExecParentsPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-exec-parents-*")
	if err != nil {
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
	}
}

// CodexIdle is how long a Codex thread nobody launched through ccs still
// counts as live after its transcript was last written: without a process
// to check, an idle session and a closed one look the same.
const CodexIdle = 12 * time.Hour

// CodexThreadName is the title Codex gave thread, "" when it has none.
func CodexThreadName(thread string) string {
	f, err := os.Open(filepath.Join(transcript.CodexHome(), "session_index.jsonl"))
	if err != nil {
		return ""
	}
	defer f.Close()
	name := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var rec struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.ID == thread && rec.Name != "" {
			name = rec.Name // later lines rename it
		}
	}
	return name
}

// CodexThreadFor is the thread ccs launch started under name: the newest
// transcript with the name's launch folder among its workspace roots.
func CodexThreadFor(name string) string {
	dir := filepath.Join(state.LaunchDir(), name)
	for _, p := range transcript.CodexRollouts() {
		if m, ok := transcript.ReadCodexMeta(p); ok && containsString(m.Roots, dir) {
			return m.ID
		}
	}
	return ""
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// CodexLive says whether a Codex claim's session is still open: for a thread
// ccs launched, a codex process with its launch folder is running; for any
// other, its transcript was written within codexIdle. Stubbed in tests.
var CodexLive = func(thread string) bool {
	path := transcript.CodexRollout(thread)
	if path == "" {
		return false
	}
	if m, ok := transcript.ReadCodexMeta(path); ok {
		if name := transcript.CodexLaunchName(m); name != "" {
			pids, err := SessionsNamed(name)
			if err == nil {
				return len(pids) > 0
			}
		}
	}
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) < CodexIdle
}

// CodexNudgeNote opens a nudge queued into a Codex session: Codex records it
// as a user turn, so it says plainly who sent it.
const CodexNudgeNote = "[Automatic status notice from ccs watch, the user's PR watcher. Not typed by the user: no reply is needed.]\n\n"

// CodexQueue queues text into a Codex thread; Codex delivers it to the
// running session at its next turn. Stubbed in tests.
var CodexQueue = func(thread, text string) error {
	out, err := proc.RunBounded(15*time.Second, nil, "codex", "queue", "--thread", thread, "--message", text)
	if err != nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return errorString(s)
		}
		return err
	}
	return nil
}

type errorString string

func (e errorString) Error() string { return string(e) }

var CodexLiveCache struct {
	sync.Mutex
	at  time.Time
	Out map[string]LiveSession
}

// LiveCodexSessions is codexLiveSessions; stubbed in tests, which must
// never see the machine's own Codex tabs.
var LiveCodexSessions = CodexLiveSessions

// CodexLiveSessions is the Codex sessions open in a terminal: a codex TUI
// (a codex process with a tty, not one of its servers or headless
// commands) and the thread it's on. Codex writes a rollout without holding
// it open, so the thread is the one its arguments name (resume or fork), or
// for a session ccs launched its launch folder's, or else the latest
// rollout in its folder written since it started. A TUI that hasn't had a
// turn yet has no thread, and isn't a session.
func CodexLiveSessions() map[string]LiveSession {
	CodexLiveCache.Lock()
	defer CodexLiveCache.Unlock()
	if CodexLiveCache.Out != nil && time.Since(CodexLiveCache.at) < transcript.CodexLiveTTL {
		return CodexLiveCache.Out
	}
	out := map[string]LiveSession{}
	ps, err := proc.PsAll()
	if err != nil {
		CodexLiveCache.at, CodexLiveCache.Out = time.Now(), out
		return out
	}
	type tui struct {
		pid  int
		args []string
	}
	var tuis []tui
	pids := map[string]int{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] == "??" || filepath.Base(f[2]) != "codex" {
			continue
		}
		args := f[3:]
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") && args[0] != "resume" && args[0] != "fork" {
			continue // a subcommand (exec, queue, app-server...) or a prompt given as the first word
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		tuis = append(tuis, tui{pid, args})
		pids[f[0]] = pid
	}
	if len(tuis) == 0 {
		CodexLiveCache.at, CodexLiveCache.Out = time.Now(), out
		return out
	}
	starts := proc.ProcessStartTimes(pids)
	rollouts := transcript.CodexRollouts()
	for _, t := range tuis {
		start := starts[t.pid]
		thread := codexThreadOf(t.args, rollouts, start, t.pid)
		if thread == "" {
			continue
		}
		s := LiveSession{Pid: t.pid, Unverified: start.IsZero(), Codex: true}
		if !start.IsZero() {
			s.Start = start.UTC().Format(time.ANSIC)
		}
		s.Stat = CodexStat(transcript.CodexRollout(thread))
		out[thread] = s
	}
	CodexLiveCache.at, CodexLiveCache.Out = time.Now(), out
	return out
}

// codexThreadOf is the thread a codex TUI started with args is on.
func codexThreadOf(args, rollouts []string, start time.Time, pid int) string {
	for i, a := range args {
		if a == "resume" && i+1 < len(args) && transcript.CodexThreadID.MatchString(args[i+1]) { // a fork starts a new thread
			return args[i+1]
		}
		if a == "--add-dir" && i+1 < len(args) {
			if name, ok := strings.CutPrefix(args[i+1], state.LaunchDir()+string(filepath.Separator)); ok && state.SessionName.MatchString(name) {
				if th := CodexThreadFor(name); th != "" {
					return th
				}
			}
		}
	}
	if start.IsZero() {
		return ""
	}
	cwd := transcript.CodexProcCwd(pid)
	if cwd == "" {
		return ""
	}
	best, bestMod := "", time.Time{}
	for _, p := range rollouts {
		info, err := os.Stat(p)
		if err != nil || info.ModTime().Before(start) || !info.ModTime().After(bestMod) {
			continue
		}
		if m, ok := transcript.ReadCodexMeta(p); ok && m.Cwd == cwd {
			best, bestMod = m.ID, info.ModTime()
		}
	}
	return best
}

// CodexStat is a Codex session's status from its rollout's last turn
// event: busy from task_started until task_complete or turn_aborted.
func CodexStat(path string) SessionStat {
	st := SessionStat{Status: "idle"}
	f, err := os.Open(path)
	if err != nil {
		return st
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 256<<10 {
		f.Seek(info.Size()-256<<10, 0)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		b := sc.Bytes()
		for _, ev := range []string{"task_started", "task_complete", "turn_aborted"} {
			if strings.Contains(string(b), `"type":"`+ev+`"`) {
				var rec transcript.CodexRecord
				if json.Unmarshal(b, &rec) == nil {
					if ev == "task_started" {
						st.Status = "busy"
					} else {
						st.Status = "idle"
					}
					if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
						st.Since = t
					}
				}
			}
		}
	}
	return st
}

// DeliverCodex sends the message box's text to a live Codex session: Codex
// queues it and hands it over at the session's next turn, as the user's.
func DeliverCodex(id, name, text string) (string, error) {
	if err := CodexQueue(id, text); err != nil {
		return "✗ Couldn't queue it for " + name + ": " + err.Error(), fmt.Errorf("%w: %v", ErrUnsent, err)
	}
	return "✓ Queued for " + name + " with codex queue (as you)", nil
}

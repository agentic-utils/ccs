package mcp

// `ccs hook stop` is a Claude Code Stop hook: Claude Code runs it the moment
// a session's turn ends. When the session has a recorded parent (it was
// launched or adopted by an orchestrator), the hook tells that parent, if it
// is running, what the child is doing now: a child_turn notice over the
// parent's socket in ccs's own automatic-notice envelope (never the user's),
// and a child_turn line in pr-events.jsonl either way. It never prints
// anything, so it can't stop the turn from ending.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// The envelope child-turn notices carry: not the user's (ccsSender), which
// says the user typed it.
const (
	childTurnSender = "ccs hook"
	ChildTurnNote   = "[Automatic notice from ccs: a session you launched or adopted finished its turn. Not typed by the user and not from another Claude session: there is no one to reply to. Act on it as that session's orchestrator.]\n\n"
)

// stopHookTimeout is the timeout `ccs hook install` gives the Stop hook
// (seconds); the hook itself gives up well before it.
const stopHookTimeout = 15

// childTurnDebounce: a notice for the same child within this long of the
// last one, in the same bucket, is skipped; the next notice carries its
// state anyway.
const childTurnDebounce = 30 * time.Second

// childTurnTail is how much of the end of a transcript the hook reads: the
// turn that just ended and its last message, not the whole history.
const childTurnTail = 1 << 20

// autoNotice is a prompt that only an automatic ccs notice wrote: a child
// turn notice, a ccs watch nudge, ccs reorg or ccs close.
var autoNotice = regexp.MustCompile(`^(Another Claude session sent a message:\s*)?<cross-session-message from-name="ccs (hook|watch|reorg|close)"`)

// stopRequest is the part of Claude Code's Stop hook input ccs uses.
type stopRequest struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Event          string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// ChildTurnEvent is a child_turn line in pr-events.jsonl.
type ChildTurnEvent struct {
	At        string     `json:"at"`
	Event     string     `json:"event"`   // child_turn
	Session   string     `json:"session"` // the child's name
	SessionID string     `json:"session_id"`
	Parent    string     `json:"parent"`
	ParentID  string     `json:"parent_id,omitempty"`
	Bucket    string     `json:"bucket"`
	Reason    string     `json:"reason,omitempty"`
	Last      string     `json:"last,omitempty"`
	PRs       []TriagePR `json:"prs,omitempty"`
	Delivered bool       `json:"delivered"`
	Error     string     `json:"error,omitempty"` // why it wasn't delivered
}

// childNotices is child-notices.json beside the settings: the orchestrators
// (by name) that turned the notices off, and the last notice per child.
type childNotices struct {
	Off  map[string]time.Time      `json:"off,omitempty"`
	Last map[string]childNoticeRec `json:"last,omitempty"`
}

type childNoticeRec struct {
	At     time.Time `json:"at"`
	Bucket string    `json:"bucket"`
}

var childNoticesPath = func() string { return filepath.Join(filepath.Dir(state.SettingsPath), "child-notices.json") }

// withChildNotices reads and, when f says so, writes child-notices.json
// under a flock.
func withChildNotices(f func(n *childNotices) bool) error {
	path := childNoticesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	var n childNotices
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &n)
	}
	if n.Off == nil {
		n.Off = map[string]time.Time{}
	}
	if n.Last == nil {
		n.Last = map[string]childNoticeRec{}
	}
	if !f(&n) {
		return nil
	}
	for id, r := range n.Last { // a day-old record can't debounce anything
		if time.Since(r.At) > 24*time.Hour {
			delete(n.Last, id)
		}
	}
	return github.WriteJSON(path, n)
}

// SetChildNotices turns an orchestrator's child-turn notices on or off.
func SetChildNotices(parent string, on bool, now time.Time) error {
	if strings.TrimSpace(parent) == "" {
		return errors.New("no orchestrator named")
	}
	return withChildNotices(func(n *childNotices) bool {
		if on {
			delete(n.Off, parent)
		} else {
			n.Off[parent] = now
		}
		return true
	})
}

// turnTail is what the hook reads from the end of the transcript: the
// prompt that started the last turn, the turn's last tool call, and the
// conversation as far as the tail goes.
type turnTail struct {
	Prompt   string // the last prompt's text (typed, queued or a cross-session message)
	LastTool string // the name of the last tool call after it
	LastTo   string // with SendMessage, its "to"
	Conv     transcript.Conversation
}

// readTurnTail reads the last childTurnTail bytes of path.
func readTurnTail(path, id string) (turnTail, error) {
	t := turnTail{Conv: transcript.Conversation{SessionID: id}}
	f, err := os.Open(path)
	if err != nil {
		return t, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return t, err
	}
	start := max(info.Size()-childTurnTail, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return t, err
	}
	r := bufio.NewReaderSize(f, 1<<16)
	if start > 0 {
		r.ReadBytes('\n') // a line cut by the seek
	}
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			transcript.ParseLine(&t.Conv, line)
			t.note(line)
		}
		if err != nil {
			break
		}
	}
	transcript.FinishParse(&t.Conv)
	return t, nil
}

// note follows the prompts and tool calls in one line.
func (t *turnTail) note(line []byte) {
	var l struct {
		Type    string `json:"type"`
		IsSide  bool   `json:"isSidechain"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Attachment struct {
			Type   string `json:"type"`
			Prompt string `json:"prompt"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &l) != nil || l.IsSide {
		return
	}
	prompt := func(s string) { t.Prompt, t.LastTool, t.LastTo = strings.TrimSpace(s), "", "" }
	switch l.Type {
	case "attachment":
		if l.Attachment.Type == "queued_command" {
			prompt(l.Attachment.Prompt)
		}
	case "user":
		var s string
		if json.Unmarshal(l.Message.Content, &s) == nil {
			prompt(s)
			return
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		json.Unmarshal(l.Message.Content, &blocks)
		var text []string
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return
			}
			if b.Type == "text" {
				text = append(text, b.Text)
			}
		}
		if len(text) > 0 {
			prompt(strings.Join(text, "\n"))
		}
	case "assistant":
		var blocks []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				To string `json:"to"`
			} `json:"input"`
		}
		json.Unmarshal(l.Message.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "tool_use" {
				t.LastTool, t.LastTo = b.Name, b.Input.To
			}
		}
	}
}

// toParent: a SendMessage "to" that names the parent, by name (with or
// without a [ref]) or by its socket.
func toParent(to, parent, socket string) bool {
	to = strings.TrimSpace(to)
	if to == "" {
		return false
	}
	if socket != "" && strings.TrimPrefix(to, "uds:") == socket {
		return true
	}
	name, _, _ := strings.Cut(to, " [")
	return parent != "" && name == parent
}

// childTurnText is the notice the parent gets.
func childTurnText(e ChildTurnEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s finished a turn · now %s", e.Session, e.Bucket)
	if e.Reason != "" {
		fmt.Fprintf(&b, " (%s)", e.Reason)
	}
	b.WriteString("\n")
	if e.Last != "" {
		fmt.Fprintf(&b, "last: %q\n", e.Last)
	}
	if len(e.PRs) > 0 {
		var prs []string
		for _, p := range e.PRs {
			prs = append(prs, github.PRName(p.PR)+" "+p.Status)
		}
		fmt.Fprintf(&b, "PRs: %s\n", strings.Join(prs, "; "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// sendChildTurn delivers a notice to the parent's live Claude session;
// stubbed in tests.
var sendChildTurn = func(pid int, parentID, text string) error {
	f, err := sessions.ReadSessionFile(pid)
	if err != nil {
		return fmt.Errorf("session file: %w", err)
	}
	if f.SessionID != parentID {
		return fmt.Errorf("pid %d is now another session", pid)
	}
	if f.Socket == "" {
		return errors.New("it has no message socket")
	}
	_, err = sessions.SendEnvelope(f.Socket, sessions.PeerToken(pid, f.Socket), childTurnSender, ChildTurnNote, text, false)
	return err
}

// parentSocket is the live parent's socket, for matching a SendMessage to
// it; stubbed in tests.
var parentSocket = func(pid int) string {
	if f, err := sessions.ReadSessionFile(pid); err == nil {
		return f.Socket
	}
	return ""
}

// hookCallerName is the name of the session the hook runs for, from its
// session file: the claude process that ran the hook (its parent) first,
// else the file that names this session id (Claude Code may run the hook
// through a shell). Reading files, never ps, keeps the hook quick for the
// many sessions that have no parent. "" when none says; stubbed in tests.
var hookCallerName = func(id string) string {
	if f, err := sessions.ReadSessionFile(os.Getppid()); err == nil && f.SessionID == id {
		return f.Name
	}
	files, _ := filepath.Glob(filepath.Join(sessions.GetSessionsDir(), "*.json"))
	for _, p := range files {
		var f sessions.SessionFile
		if data, err := os.ReadFile(p); err == nil && json.Unmarshal(data, &f) == nil && f.SessionID == id {
			return f.Name
		}
	}
	return ""
}

// stopHook reads the Stop hook's input and, when the session has a running
// parent and the turn is worth telling, tells it. Every failure is logged
// and swallowed: a Stop hook must never get in the turn's way.
func stopHook(stdin io.Reader, now time.Time) {
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return
	}
	var req stopRequest
	if json.Unmarshal(raw, &req) != nil || req.Event != "Stop" || req.SessionID == "" || req.StopHookActive {
		return
	}
	type result struct {
		e  ChildTurnEvent
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		e, ok := childTurn(req, now)
		if ok {
			if err := github.WriteEventLines([]any{e}); err != nil {
				state.LogActivity("ccs hook stop: couldn't write child_turn for %s: %v", e.Session, err)
			}
		}
		done <- result{e, ok}
	}()
	select {
	case <-done:
	case <-time.After(stopHookDeadline):
		// A slow disk or a stuck socket must not hold the turn's end: give up.
		state.LogActivity("ccs hook stop: gave up on session %s after %s", req.SessionID, stopHookDeadline)
	}
}

// stopHookDeadline is the most the Stop hook spends; Claude Code's own
// timeout for it (stopHookTimeout) is longer, so the hook always ends first.
var stopHookDeadline = 5 * time.Second

// hookSessions is the running Claude sessions as their session files say,
// keyed by session id: no ps, only the files and a signal-0 check that each
// pid is alive (sendChildTurn checks the pid still runs that session before
// sending). Stubbed in tests.
var hookSessions = func() map[string]sessions.LiveSession {
	out := map[string]sessions.LiveSession{}
	files, _ := filepath.Glob(filepath.Join(sessions.GetSessionsDir(), "*.json"))
	for _, p := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err != nil || pid <= 0 {
			continue
		}
		var f sessions.SessionFile
		data, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(data, &f) != nil || f.SessionID == "" {
			continue
		}
		if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
			continue // a file left behind by a process that has gone
		}
		out[f.SessionID] = sessions.LiveSession{PID: pid, Stat: sessions.SessionStat{Status: f.Status, Name: f.Name, Cwd: f.Cwd}}
	}
	return out
}

// childTurn decides and sends the notice for one finished turn, and is the
// event to log; false when there is nothing to tell (no parent, a skipped
// turn, notices off).
func childTurn(req stopRequest, now time.Time) (ChildTurnEvent, bool) {
	// Most sessions have no parent: decide that from the records and the
	// hook's own claude process (its parent) before scanning every session.
	parents := sessions.LoadParents().Parents
	if len(parents) == 0 {
		return ChildTurnEvent{}, false
	}
	findRec := func(name string) (sessions.ParentRec, bool) {
		var rec sessions.ParentRec
		found := false
		for key, r := range parents {
			if r.ChildID == req.SessionID {
				return r, r.Parent != ""
			}
			if name != "" && (r.Child == name || key == name) {
				rec, found = r, r.Parent != ""
			}
		}
		return rec, found
	}
	name := hookCallerName(req.SessionID)
	rec, found := findRec(name)
	nameOnly := false // a record only a name can match (no child id yet), and no name known here
	for _, r := range parents {
		nameOnly = nameOnly || r.ChildID == ""
	}
	if !found && (name != "" || !nameOnly) {
		return ChildTurnEvent{}, false
	}
	live := hookSessions()
	me, ok := live[req.SessionID]
	if !ok || me.Codex {
		return ChildTurnEvent{}, false
	}
	if r, ok := findRec(me.Stat.Name); ok {
		rec, found = r, true
	}
	if !found {
		return ChildTurnEvent{}, false
	}
	// The parent: running under its recorded id, else the one session by its name.
	ppid, pid := 0, ""
	if p, ok := live[rec.ParentID]; ok && rec.ParentID != "" && !p.Codex {
		ppid, pid = p.PID, rec.ParentID
	} else {
		for id, p := range live {
			if !p.Codex && p.Stat.Name == rec.Parent {
				if ppid != 0 {
					ppid, pid = 0, "" // two by that name: never guess
					break
				}
				ppid, pid = p.PID, id
			}
		}
	}
	if pid == req.SessionID {
		return ChildTurnEvent{}, false
	}
	path := cmpOr(req.TranscriptPath, transcript.TranscriptPath(req.SessionID))
	tail, err := readTurnTail(path, req.SessionID)
	if err != nil {
		state.LogActivity("ccs hook stop: %s: %v", me.Stat.Name, err)
		return ChildTurnEvent{}, false
	}
	if autoNotice.MatchString(tail.Prompt) {
		return ChildTurnEvent{}, false // a notice started it: telling the parent would cascade up the tree
	}
	if tail.LastTool == "SendMessage" && ppid != 0 && toParent(tail.LastTo, rec.Parent, parentSocket(ppid)) {
		return ChildTurnEvent{}, false // it just told the parent itself
	}
	tail.Conv.Title = me.Stat.Name
	me.Stat.Status, me.Stat.Since = "idle", now // the turn has just ended, whatever the file says yet
	prs := ownedPRs(now)[ownerKey("claude", req.SessionID)]
	row := triageOf(tail.Conv, me, prs, now, DefaultStallAfter)
	e := ChildTurnEvent{At: now.UTC().Format(time.RFC3339), Event: "child_turn", Session: cmpOr(me.Stat.Name, rec.Child), SessionID: req.SessionID, Parent: rec.Parent, ParentID: cmpOr(pid, rec.ParentID), Bucket: row.Bucket, Reason: row.Reason, Last: row.LastSaid, PRs: row.OpenPRs}

	skip := false
	withChildNotices(func(n *childNotices) bool {
		if _, off := n.Off[rec.Parent]; off {
			skip = true
			return false
		}
		if last, ok := n.Last[req.SessionID]; ok && now.Sub(last.At) < childTurnDebounce && last.Bucket == e.Bucket {
			skip = true
			return false
		}
		n.Last[req.SessionID] = childNoticeRec{At: now, Bucket: e.Bucket}
		return true
	})
	if skip {
		return ChildTurnEvent{}, false
	}
	if ppid == 0 {
		e.Error = "parent not running"
		return e, true
	}
	if err := sendChildTurn(ppid, pid, childTurnText(e)); err != nil {
		e.Error = err.Error()
		state.LogActivity("ccs hook stop: couldn't tell %s about %s: %v", rec.Parent, e.Session, err)
		return e, true
	}
	e.Delivered = true
	return e, true
}

package mcp

import (
	"bufio"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// `ccs hook session-start` is Claude Code's SessionStart hook. When a new
// session starts (source "startup"), it records the session's parent at
// once, however it was started, so it never shows up without one:
//
//   - a claude run from another session's shell descends from that session's
//     process, so the first running session among its ancestors is the parent;
//   - one opened in a new tab or window (session-handoff's open-tab.sh, or
//     osascript or tmux by hand) has no such ancestor, so the parent is the
//     running session whose transcript, in the last startEvidenceWindow, ran a
//     command that names it (open-tab.sh <name>, claude --name <name>,
//     ccs launch --name <name>) or called launch_session with its name.
//
// ccs launch and launch_session record their own parent just after, and win.
// An env var wouldn't do: a tab opened through osascript or tmux starts a
// fresh login shell, so nothing set in the parent reaches it.
//
// It prints nothing: SessionStart output becomes the session's context.

type sessionStartRequest struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
}

var (
	// sessionStartDeadline is the most the hook spends; its timeout in the
	// settings (sessionStartTimeout) is longer, so the hook always ends first.
	sessionStartDeadline = 4 * time.Second
	// startFileWait is how long it waits for the new session's own file,
	// which Claude Code may write just after the hook starts.
	startFileWait = 2 * time.Second
	// startEvidenceWindow is how far back a launching command counts.
	startEvidenceWindow = 15 * time.Minute
	// startPPID and startTranscript are stubbed in tests.
	startPPID       = os.Getppid
	startTranscript = transcript.TranscriptPath
)

func sessionStartHook(stdin io.Reader, now time.Time) {
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return
	}
	var req sessionStartRequest
	if json.Unmarshal(raw, &req) != nil || req.Event != "SessionStart" || req.SessionID == "" || req.Source != "startup" {
		return // a resume, /clear or compaction keeps the parent it has
	}
	done := make(chan struct{})
	go func() {
		recordStartParent(req.SessionID, now)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(sessionStartDeadline):
		state.LogActivity("ccs hook session-start: gave up on session %s after %s", req.SessionID, sessionStartDeadline)
	}
}

// recordStartParent works out and records the parent of the session id that
// just started, if it can tell.
func recordStartParent(id string, now time.Time) {
	var live map[string]sessions.LiveSession
	me, found := sessions.LiveSession{}, false
	for start := time.Now(); ; time.Sleep(100 * time.Millisecond) {
		live = hookSessions()
		if me, found = live[id]; found || time.Since(start) >= startFileWait {
			break
		}
	}
	name := me.Stat.Name
	key := cmp.Or(name, id)
	if r, ok := sessions.LoadParents().Parents[key]; ok && r.ChildID == id {
		return // launch_session or ccs launch got there first
	}
	parent, how := startParent(id, name, me.PID, live, now)
	if parent.ID == "" {
		return
	}
	rec := sessions.ParentRec{Child: name, ChildID: id, Parent: parent.Name, ParentID: parent.ID, ParentKind: parent.Kind, Source: "start"}
	if err := sessions.SetParents([]sessions.ParentRec{rec}); err != nil {
		state.LogActivity("ccs hook session-start: couldn't record %s under %s: %v", key, parent.Name, err)
		return
	}
	state.LogActivity("ccs hook session-start: %s is under %s (%s)", key, parent.Name, how)
}

// startParent is the session that started id: by process ancestry, else by
// the newest command in a running session's transcript naming it.
func startParent(id, name string, myPID int, live map[string]sessions.LiveSession, now time.Time) (sessions.Caller, string) {
	byPID := map[int]string{}
	for sid, s := range live {
		if !s.Codex && sid != id {
			byPID[s.PID] = sid
		}
	}
	if tree, err := proc.PsTree(); err == nil {
		ppid := map[int]int{}
		for _, line := range strings.Split(tree, "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			pid, e1 := strconv.Atoi(f[0])
			pp, e2 := strconv.Atoi(f[1])
			if e1 == nil && e2 == nil {
				ppid[pid] = pp
			}
		}
		pid := startPPID()
		for range 40 {
			if pid <= 1 {
				break
			}
			if sid, ok := byPID[pid]; ok && pid != myPID {
				return sessions.Caller{ID: sid, Name: live[sid].Stat.Name}, "its process runs under that session's"
			}
			pid = ppid[pid]
		}
	}
	if name == "" {
		return sessions.Caller{}, ""
	}
	var best sessions.Caller
	var bestAt time.Time
	for sid, s := range live {
		if sid == id || s.Codex {
			continue
		}
		if at, ok := launchedIn(startTranscript(sid), name, now); ok && at.After(bestAt) {
			best, bestAt = sessions.Caller{ID: sid, Name: s.Stat.Name}, at
		}
	}
	if best.ID == "" {
		return best, ""
	}
	return best, "its transcript ran the command that started it"
}

// startTailBytes is how much of a transcript's end launchedIn reads.
const startTailBytes = 2 << 20

// launchedIn is when the transcript at path last started a session called
// name within startEvidenceWindow: a Bash command transcript.LaunchNames
// finds it in, or a launch_session call with that name.
func launchedIn(path, name string, now time.Time) (time.Time, bool) {
	f, err := os.Open(path)
	if path == "" || err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > startTailBytes {
		f.Seek(info.Size()-startTailBytes, io.SeekStart)
	}
	var last time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), name) || !strings.Contains(string(line), `"tool_use"`) {
			continue
		}
		var rec struct {
			Timestamp string `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil || now.Sub(at) > startEvidenceWindow || at.After(now.Add(time.Minute)) {
			continue
		}
		var blocks []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Command string `json:"command"`
				Name    string `json:"name"`
			} `json:"input"`
		}
		if json.Unmarshal(rec.Message.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type != "tool_use" {
				continue
			}
			named := strings.HasSuffix(b.Name, "launch_session") && b.Input.Name == name ||
				b.Name == "Bash" && slices.Contains(transcript.LaunchNames(b.Input.Command), name)
			if named && at.After(last) {
				last = at
			}
		}
	}
	return last, !last.IsZero()
}

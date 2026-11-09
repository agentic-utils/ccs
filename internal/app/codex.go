package app

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Codex sessions: ccs launch --codex starts one, ccs claim works inside one
// (Codex gives every command it runs CODEX_THREAD_ID), and ccs watch nudges
// one with `codex queue`, which delivers into a running Codex tab. Codex's
// sandbox lets a command neither run ps nor reach a socket, so a Codex
// claim is keyed by thread id, never by process, and its liveness is worked
// out by ccs watch from outside the sandbox (codexLive).

// codexIdle is how long a Codex thread nobody launched through ccs still
// counts as live after its transcript was last written: without a process
// to check, an idle session and a closed one look the same.
const codexIdle = 12 * time.Hour

// codexThreadName is the title Codex gave thread, "" when it has none.
func codexThreadName(thread string) string {
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

// codexSession is the Codex session this process runs in, from the
// CODEX_THREAD_ID Codex gives every command; ok false outside Codex.
func codexSession() (prClaim, bool) {
	thread := os.Getenv("CODEX_THREAD_ID")
	if !transcript.CodexThreadID.MatchString(thread) {
		return prClaim{}, false
	}
	c := prClaim{Session: thread, Kind: "codex", Thread: thread}
	if m, ok := transcript.ReadCodexMeta(transcript.CodexRollout(thread)); ok {
		c.Name = transcript.CodexLaunchName(m)
	}
	if c.Name == "" {
		c.Name = codexThreadName(thread)
	}
	if c.Name == "" {
		c.Name = thread
	}
	return c, true
}

// codexThreadFor is the thread ccs launch started under name: the newest
// transcript with the name's launch folder among its workspace roots.
func codexThreadFor(name string) string {
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

// codexLive says whether a Codex claim's session is still open: for a thread
// ccs launched, a codex process with its launch folder is running; for any
// other, its transcript was written within codexIdle. Stubbed in tests.
var codexLive = func(thread string) bool {
	path := transcript.CodexRollout(thread)
	if path == "" {
		return false
	}
	if m, ok := transcript.ReadCodexMeta(path); ok {
		if name := transcript.CodexLaunchName(m); name != "" {
			pids, err := sessionsNamed(name)
			if err == nil {
				return len(pids) > 0
			}
		}
	}
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) < codexIdle
}

// codexNudgeNote opens a nudge queued into a Codex session: Codex records it
// as a user turn, so it says plainly who sent it.
const codexNudgeNote = "[Automatic status notice from ccs watch, the user's PR watcher. Not typed by the user: no reply is needed.]\n\n"

// codexQueue queues text into a Codex thread; Codex delivers it to the
// running session at its next turn. Stubbed in tests.
var codexQueue = func(thread, text string) error {
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

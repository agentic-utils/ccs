package app

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
)

// Codex sessions: ccs launch --codex starts one, ccs claim works inside one
// (Codex gives every command it runs CODEX_THREAD_ID), and ccs watch nudges
// one with `codex queue`, which delivers into a running Codex tab. Codex's
// sandbox lets a command neither run ps nor reach a socket, so a Codex
// claim is keyed by thread id, never by process, and its liveness is worked
// out by ccs watch from outside the sandbox (codexLive).

// codexHome is Codex's data folder. Stubbed in tests.
var codexHome = func() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// codexThreadID is a Codex thread id: a uuid.
var codexThreadID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// codexIdle is how long a Codex thread nobody launched through ccs still
// counts as live after its transcript was last written: without a process
// to check, an idle session and a closed one look the same.
const codexIdle = 12 * time.Hour

// codexMeta is the first line of a Codex transcript (its session_meta).
type codexMeta struct {
	ID        string          `json:"id"`
	Cwd       string          `json:"cwd"`
	Roots     []string        `json:"runtime_workspace_roots"`
	Source    json.RawMessage `json:"source"`
	Timestamp string          `json:"timestamp"`
}

// codexRollouts is Codex's transcripts, newest first by name (each starts
// rollout-<start time>-<thread id>).
func codexRollouts() []string {
	paths, _ := filepath.Glob(filepath.Join(codexHome(), "sessions", "*", "*", "*", "rollout-*.jsonl"))
	for i, j := 0, len(paths)-1; i < j; i, j = i+1, j-1 {
		paths[i], paths[j] = paths[j], paths[i]
	}
	return paths
}

// codexRollout is thread's transcript, "" when there's none.
func codexRollout(thread string) string {
	if !codexThreadID.MatchString(thread) {
		return ""
	}
	paths, _ := filepath.Glob(filepath.Join(codexHome(), "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if len(paths) == 0 {
		return ""
	}
	return paths[len(paths)-1]
}

// readCodexMeta reads a transcript's session_meta.
func readCodexMeta(path string) (codexMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return codexMeta{}, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	line, _ := r.ReadBytes('\n')
	var rec struct {
		Type    string    `json:"type"`
		Payload codexMeta `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "session_meta" || rec.Payload.ID == "" {
		return codexMeta{}, false
	}
	return rec.Payload, true
}

// codexLaunchName is the name ccs launch gave a thread: its launch folder is
// one of the thread's workspace roots. "" for a thread ccs didn't launch.
func codexLaunchName(m codexMeta) string {
	base := launchDir() + string(filepath.Separator)
	for _, r := range m.Roots {
		if name, ok := strings.CutPrefix(r, base); ok && sessionName.MatchString(name) {
			return name
		}
	}
	return ""
}

// codexThreadName is the title Codex gave thread, "" when it has none.
func codexThreadName(thread string) string {
	f, err := os.Open(filepath.Join(codexHome(), "session_index.jsonl"))
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
	if !codexThreadID.MatchString(thread) {
		return prClaim{}, false
	}
	c := prClaim{Session: thread, Kind: "codex", Thread: thread}
	if m, ok := readCodexMeta(codexRollout(thread)); ok {
		c.Name = codexLaunchName(m)
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
	dir := filepath.Join(launchDir(), name)
	for _, p := range codexRollouts() {
		if m, ok := readCodexMeta(p); ok && containsString(m.Roots, dir) {
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
	path := codexRollout(thread)
	if path == "" {
		return false
	}
	if m, ok := readCodexMeta(path); ok {
		if name := codexLaunchName(m); name != "" {
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

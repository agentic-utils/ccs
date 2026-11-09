package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Codex sessions in the list: each transcript (a "rollout" under
// $CODEX_HOME/sessions/YYYY/MM/DD) parses into a transcript.Conversation with Kind
// "codex", sorted, searched and previewed with Claude's. A Codex rollout is
// small next to a Claude transcript and isn't appended to in place by a
// resume, so a changed one is simply parsed again (parseCache keeps the
// unchanged ones).

var codexLiveCache struct {
	sync.Mutex
	at  time.Time
	out map[string]liveSession
}

// liveCodexSessions is codexLiveSessions; stubbed in tests, which must
// never see the machine's own Codex tabs.
var liveCodexSessions = codexLiveSessions

// codexLiveSessions is the Codex sessions open in a terminal: a codex TUI
// (a codex process with a tty, not one of its servers or headless
// commands) and the thread it's on. Codex writes a rollout without holding
// it open, so the thread is the one its arguments name (resume or fork), or
// for a session ccs launched its launch folder's, or else the latest
// rollout in its folder written since it started. A TUI that hasn't had a
// turn yet has no thread, and isn't a session.
func codexLiveSessions() map[string]liveSession {
	codexLiveCache.Lock()
	defer codexLiveCache.Unlock()
	if codexLiveCache.out != nil && time.Since(codexLiveCache.at) < transcript.CodexLiveTTL {
		return codexLiveCache.out
	}
	out := map[string]liveSession{}
	ps, err := proc.PsAll()
	if err != nil {
		codexLiveCache.at, codexLiveCache.out = time.Now(), out
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
		codexLiveCache.at, codexLiveCache.out = time.Now(), out
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
		s := liveSession{pid: t.pid, unverified: start.IsZero(), codex: true}
		if !start.IsZero() {
			s.start = start.UTC().Format(time.ANSIC)
		}
		s.stat = codexStat(transcript.CodexRollout(thread))
		out[thread] = s
	}
	codexLiveCache.at, codexLiveCache.out = time.Now(), out
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
				if th := codexThreadFor(name); th != "" {
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

// codexStat is a Codex session's status from its rollout's last turn
// event: busy from task_started until task_complete or turn_aborted.
func codexStat(path string) sessionStat {
	st := sessionStat{status: "idle"}
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
						st.status = "busy"
					} else {
						st.status = "idle"
					}
					if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
						st.since = t
					}
				}
			}
		}
	}
	return st
}

// deliverCodex sends the message box's text to a live Codex session: Codex
// queues it and hands it over at the session's next turn, as the user's.
func deliverCodex(id, name, text string) (string, error) {
	if err := codexQueue(id, text); err != nil {
		return "✗ Couldn't queue it for " + name + ": " + err.Error(), fmt.Errorf("%w: %v", errUnsent, err)
	}
	return "✓ Queued for " + name + " with codex queue (as you)", nil
}

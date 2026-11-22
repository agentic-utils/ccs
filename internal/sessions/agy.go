package sessions

import (
	"bufio"
	"encoding/json"
	"errors"
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

// Antigravity CLI (agy) live sessions, launch discovery, liveness and delivery.

// AgyIdle is how long an agy conversation nobody launched through ccs and with
// no detected live process still counts as live after its transcript was last
// written.
const AgyIdle = 12 * time.Hour

// AgyLiveCache caches live agy sessions for transcript.AgyLiveTTL.
var AgyLiveCache struct {
	sync.Mutex
	at  time.Time
	Out map[string]LiveSession
}

// AgyPresenceLocks maps each agy pid to the conversation UUID whose presence
// lock file it holds open under <AgyHome>/presence/<id>.lock. Stubbed in tests.
var AgyPresenceLocks = func() map[int]string {
	locks, _ := filepath.Glob(filepath.Join(transcript.AgyHome(), "presence", "*.lock"))
	if len(locks) == 0 {
		return nil
	}
	args := append([]string{"-Fpn"}, locks...)
	out, err := proc.RunBounded(2*time.Second, nil, "lsof", args...)
	if err != nil && len(out) == 0 {
		return nil
	}
	res := map[int]string{}
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "p"); ok {
			pid, _ = strconv.Atoi(p)
		} else if n, ok := strings.CutPrefix(line, "n"); ok && pid > 0 {
			id := strings.TrimSuffix(filepath.Base(n), ".lock")
			if transcript.AgyConvID.MatchString(id) {
				res[pid] = id
			}
		}
	}
	return res
}

// agySubcommands are non-interactive subcommands of `agy` that aren't TUI sessions.
var agySubcommands = map[string]bool{
	"acp": true, "agent": true, "agents": true, "changelog": true,
	"help": true, "install": true, "mcp": true, "mic-serve": true,
	"models": true, "plugin": true, "plugins": true, "remote-control": true,
	"update": true,
}

// LiveAgySessions is AgyLiveSessions; stubbed in tests.
var LiveAgySessions = AgyLiveSessions

// AgyLiveSessions is the agy sessions open in a terminal: an agy TUI (an agy
// process with a tty, not a subcommand or print mode) and the conversation it
// has open (from its presence lock, --conversation arg, ccs launch folder, or
// latest transcript in its cwd written since it started).
func AgyLiveSessions() map[string]LiveSession {
	AgyLiveCache.Lock()
	defer AgyLiveCache.Unlock()
	if AgyLiveCache.Out != nil && time.Since(AgyLiveCache.at) < transcript.AgyLiveTTL {
		return AgyLiveCache.Out
	}
	out := map[string]LiveSession{}
	ps, err := proc.PsAll()
	if err != nil {
		AgyLiveCache.at, AgyLiveCache.Out = time.Now(), out
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
		if len(f) < 3 || f[1] == "??" || filepath.Base(f[2]) != "agy" {
			continue
		}
		args := f[3:]
		if isAgyNonTUI(args) {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		tuis = append(tuis, tui{pid, args})
		pids[f[0]] = pid
	}
	if len(tuis) == 0 {
		AgyLiveCache.at, AgyLiveCache.Out = time.Now(), out
		return out
	}
	starts := proc.ProcessStartTimes(pids)
	locks := AgyPresenceLocks()
	transcripts := transcript.AgyTranscripts()
	for _, t := range tuis {
		start := starts[t.pid]
		convID := agyConvOf(t.pid, t.args, locks, transcripts, start)
		if convID == "" {
			continue
		}
		s := LiveSession{PID: t.pid, Unverified: start.IsZero(), Agy: true}
		if !start.IsZero() {
			s.Start = start.UTC().Format(time.ANSIC)
		}
		s.Stat = AgyStat(convID, transcript.AgyTranscript(convID))
		out[convID] = s
	}
	AgyLiveCache.at, AgyLiveCache.Out = time.Now(), out
	return out
}

func isAgyNonTUI(args []string) bool {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && agySubcommands[args[0]] {
		return true
	}
	for _, a := range args {
		if a == "-p" || a == "--print" || a == "--prompt" || strings.HasPrefix(a, "--print=") || strings.HasPrefix(a, "--prompt=") {
			return true
		}
	}
	return false
}

func agyConvOf(pid int, args []string, locks map[int]string, transcripts []string, start time.Time) string {
	if id := locks[pid]; id != "" {
		return id
	}
	for i, a := range args {
		if a == "--conversation" && i+1 < len(args) && transcript.AgyConvID.MatchString(args[i+1]) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--conversation="); ok && transcript.AgyConvID.MatchString(v) {
			return v
		}
		if a == "--add-dir" && i+1 < len(args) {
			if name, ok := strings.CutPrefix(args[i+1], state.LaunchDir()+string(filepath.Separator)); ok && state.SessionName.MatchString(name) {
				if id := AgyConvFor(name); id != "" {
					return id
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
	sums := transcript.AgySummaries()
	cwds := transcript.AgyHistoryWorkspaces()
	best, bestMod := "", time.Time{}
	for _, p := range transcripts {
		info, err := os.Stat(p)
		if err != nil || info.ModTime().Before(start) || !info.ModTime().After(bestMod) {
			continue
		}
		id := transcript.AgyConvIDFromPath(p)
		if id == "" {
			continue
		}
		tCwd := ""
		if sum, ok := sums[id]; ok {
			tCwd = transcript.AgyPrimaryCwd(transcript.AgyWorkspacePaths(sum.WorkspaceURIs))
		}
		if tCwd == "" {
			tCwd = cwds[id]
		}
		if tCwd == cwd {
			best, bestMod = id, info.ModTime()
		}
	}
	return best
}

// AgyStat determines an agy session's current status (idle, busy, waiting)
// from conversation_summaries.db and the tail of its transcript.jsonl.
func AgyStat(id, path string) SessionStat {
	st := SessionStat{Status: "idle"}
	if sum, ok := transcript.AgySummaries()[id]; ok {
		if name := transcript.AgyLaunchName(transcript.AgyWorkspacePaths(sum.WorkspaceURIs)); name != "" {
			st.Name = name
		} else if sum.Title != "" {
			st.Name = sum.Title
		}
		switch {
		case strings.Contains(sum.Status, "WAITING"):
			st.Status = "waiting"
		case sum.Status == "CASCADE_RUN_STATUS_RUNNING":
			st.Status = "busy"
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return st
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 256<<10 {
		f.Seek(info.Size()-256<<10, 0)
	}
	var last transcript.AgyRecord
	hasLast := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var rec transcript.AgyRecord
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Type != "" {
			last, hasLast = rec, true
		}
	}
	if hasLast {
		if t, err := time.Parse(time.RFC3339Nano, last.CreatedAt); err == nil {
			st.Since = t
		}
		if st.Status == "idle" && transcript.AgyRecordBusy(last) {
			st.Status = "busy"
		}
	}
	return st
}

// AgyConvFor finds the conversation id ccs launch started under name.
func AgyConvFor(name string) string {
	dir := filepath.Join(state.LaunchDir(), name)
	for id, sum := range transcript.AgySummaries() {
		if containsString(transcript.AgyWorkspacePaths(sum.WorkspaceURIs), dir) {
			return id
		}
	}
	return ""
}

// AgyName returns the display name of agy conversation id: its ccs launch
// name, else its title in conversation_summaries.db, else "".
func AgyName(id string) string {
	if sum, ok := transcript.AgySummaries()[id]; ok {
		if name := transcript.AgyLaunchName(transcript.AgyWorkspacePaths(sum.WorkspaceURIs)); name != "" {
			return name
		}
		return sum.Title
	}
	return ""
}

// AgyLive says whether an agy claim's session is still open: if it's in
// LiveAgySessions, or (for one ccs launched) its agy process is running, or
// else its transcript was written within AgyIdle. Stubbed in tests.
var AgyLive = func(id string) bool {
	if _, ok := LiveAgySessions()[id]; ok {
		return true
	}
	if sum, ok := transcript.AgySummaries()[id]; ok {
		if name := transcript.AgyLaunchName(transcript.AgyWorkspacePaths(sum.WorkspaceURIs)); name != "" {
			if pids, err := Named(name); err == nil {
				return len(pids) > 0
			}
		}
	}
	path := transcript.AgyTranscript(id)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) < AgyIdle
}

// AgyNudgeNote opens a nudge typed into an agy session: agy sees it as user
// input, so it says plainly who sent it.
const AgyNudgeNote = "[Automatic status notice from ccs watch, the user's PR watcher. Not typed by the user: no reply is needed.]\n\n"

// AgyDeliver sends text to a running agy conversation by typing into its
// terminal. Stubbed in tests.
var AgyDeliver = func(id, text string) error {
	s, ok := LiveAgySessions()[id]
	if !ok {
		return errors.New("agy session isn't running in a terminal")
	}
	if s.Unverified {
		return errors.New("agy process start time couldn't be verified")
	}
	if s.Stat.Status == "waiting" {
		return errors.New("agy session is waiting on a prompt")
	}
	term := FindTerminal(s.PID)
	if term.Kind == "" {
		return errors.New("agy session isn't in iTerm or tmux")
	}
	return SendKeys(term, text, true)
}

// DeliverAgy sends the message box's text to a live agy session by typing
// into its terminal.
func DeliverAgy(pid int, id, name, text string, verified bool) (string, error) {
	if !verified {
		return "✗ Not typed into " + name + ": its process couldn't be verified", fmt.Errorf("%w: unverified pid", ErrUnsent)
	}
	if AgyStat(id, transcript.AgyTranscript(id)).Status == "waiting" {
		return "✗ Not typed into " + name + ": it's waiting on a prompt, answer it in its tab", fmt.Errorf("%w: waiting", ErrUnsent)
	}
	where, err := TypeKeys(pid, text, true)
	switch {
	case where == "":
		return "✗ Couldn't reach " + name + ": it isn't in iTerm or tmux", fmt.Errorf("%w: unreachable", ErrUnsent)
	case errors.Is(err, proc.ErrTimedOut):
		return "⚠ Typing into " + name + "'s " + where + " timed out; it may have been typed", err
	case err != nil:
		return "✗ Couldn't type into " + name + "'s " + where + ": " + err.Error(), err
	}
	return "✓ Typed into " + name + "'s " + where + " (as you)", nil
}

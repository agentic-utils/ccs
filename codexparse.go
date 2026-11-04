package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Codex sessions in the list: each transcript (a "rollout" under
// $CODEX_HOME/sessions/YYYY/MM/DD) parses into a Conversation with Kind
// "codex", sorted, searched and previewed with Claude's. A Codex rollout is
// small next to a Claude transcript and isn't appended to in place by a
// resume, so a changed one is simply parsed again (parseCache keeps the
// unchanged ones).

// kindCodex marks a Codex session; Claude's leave Kind empty.
const kindCodex = "codex"

// agentGlyph marks every row with the agent behind it, one cell wide: ✻,
// the asterisk Claude Code draws, in Claude's orange; ⬡ for Codex.
func agentGlyph(conv Conversation) (glyph, colour string) {
	if conv.Kind == kindCodex {
		return "⬡", "37"
	}
	return "✻", "38;5;173"
}

// isCodexRollout: path is a Codex transcript, not a Claude one.
func isCodexRollout(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
}

// codexRecord is one line of a rollout, decoded only as far as ccs needs.
type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload is the union of the payload fields ccs reads.
type codexPayload struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Cwd       string          `json:"cwd"`
	Timestamp string          `json:"timestamp"`
	Source    json.RawMessage `json:"source"`
	Roots     []string        `json:"runtime_workspace_roots"`
	Model     string          `json:"model"`
	Role      string          `json:"role"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Info *struct {
		Total struct {
			Input  int64 `json:"input_tokens"`
			Cached int64 `json:"cached_input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"total_token_usage"`
		Last struct {
			Input     int   `json:"input_tokens"`
			Cached    int64 `json:"cached_input_tokens"`
			Write     int64 `json:"cache_write_input_tokens"`
			Output    int64 `json:"output_tokens"`
			Reasoning int64 `json:"reasoning_output_tokens"`
		} `json:"last_token_usage"`
		Window int `json:"model_context_window"`
	} `json:"info"`
	Item *struct {
		Type    string          `json:"type"`
		Command json.RawMessage `json:"command"`
		Output  string          `json:"aggregated_output"`
		// McpToolCall
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
		// FileChange: path to its change
		Changes map[string]json.RawMessage `json:"changes"`
		// Extension: kind web.search
		Kind  string `json:"kind"`
		Query string `json:"query"`
	} `json:"item"`
}

// codexSource is a session_meta's source: "cli", "vscode", "exec", or a
// subagent object naming the thread that spawned it.
func codexSource(raw json.RawMessage) (source, parent string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, ""
	}
	var o struct {
		Subagent struct {
			ThreadSpawn struct {
				Parent string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return "subagent", o.Subagent.ThreadSpawn.Parent
	}
	return "", ""
}

// codexHarness: user-role text Codex adds itself (AGENTS.md, the
// environment, retained instructions, app context), never typed.
func codexHarness(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<") || strings.HasPrefix(t, "# AGENTS.md instructions") || strings.HasPrefix(t, ">>> ")
}

// parseCodexFile parses one rollout, with the cutoff, size limit and cache
// parseConversationFile applies to Claude's transcripts.
func parseCodexFile(path string, cutoff time.Time, maxSize int64) (*Conversation, error) {
	readAt := time.Now()
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if maxSize > 0 && info.Size() > maxSize || !cutoff.IsZero() && info.ModTime().Before(cutoff) {
		return nil, nil
	}
	parseCacheMu.Lock()
	cached, ok := parseCache[path]
	parseCacheMu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		if cached.conv == nil {
			return nil, nil
		}
		c := *cached.conv
		c.readAt = readAt
		return &c, nil
	}
	conv, err := parseCodexUncached(path, info)
	if conv != nil {
		conv.readAt = readAt
	}
	if err == nil {
		parseCacheMu.Lock()
		parseCache[path] = parsedFile{info.Size(), info.ModTime(), conv}
		parseCacheMu.Unlock()
	}
	return conv, err
}

func parseCodexUncached(path string, info os.FileInfo) (*Conversation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	conv := &Conversation{Kind: kindCodex, FilePath: path, Size: info.Size()}
	var meta codexMeta
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var rec codexRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		var p codexPayload
		if json.Unmarshal(rec.Payload, &p) != nil {
			continue
		}
		ts := rec.Timestamp
		switch {
		case rec.Type == "session_meta":
			if conv.SessionID != "" { // a fork or resume carries its parent's meta too
				continue
			}
			conv.SessionID, conv.Cwd, conv.FirstTimestamp = p.ID, p.Cwd, p.Timestamp
			meta = codexMeta{ID: p.ID, Cwd: p.Cwd, Roots: p.Roots}
			source, parent := codexSource(p.Source)
			conv.Spawned = source == "exec" || source == "subagent"
			conv.codexExec = source == "exec"
			conv.ParentID = parent
		case rec.Type == "turn_context":
			if p.Model != "" {
				conv.Model, conv.ActiveModel = p.Model, p.Model
			}
		case rec.Type == "response_item" && p.Type == "message" && (p.Role == "user" || p.Role == "assistant"):
			var parts []string
			for _, c := range p.Content {
				if c.Text != "" && !(p.Role == "user" && codexHarness(c.Text)) {
					parts = append(parts, c.Text)
				}
			}
			if text := strings.TrimSpace(strings.Join(parts, "\n\n")); text != "" {
				conv.Messages = append(conv.Messages, Message{Role: p.Role, Text: text, Ts: ts})
			}
		case rec.Type == "event_msg" && p.Type == "token_count" && p.Info != nil:
			conv.ContextTokens = p.Info.Last.Input
			conv.PeakContext = max(conv.PeakContext, conv.ContextTokens)
			// Each event is one request (they sum to the running total), so
			// each is priced on its own tier.
			l := p.Info.Last
			in := int64(l.Input)
			conv.Usage.Input += max(in-l.Cached-l.Write, 0)
			conv.Usage.CacheRead += l.Cached
			conv.Usage.Cache5m += l.Write
			conv.Usage.Output += l.Output
			conv.Reasoning += l.Reasoning
			if c, ok := openAICost(conv.ActiveModel, in, l.Cached, l.Write, l.Output); ok {
				conv.Cost += c
			} else if in+l.Output > 0 {
				conv.Unpriced = true
			}
			if p.Info.Window > 0 {
				conv.Window = p.Info.Window
			}
		case rec.Type == "event_msg" && (p.Type == "task_started" || p.Type == "task_complete" || p.Type == "turn_aborted"):
			conv.codexBusy = p.Type == "task_started"
		case rec.Type == "event_msg" && p.Type == "item_completed" && p.Item != nil:
			if p.Item.Type == "CommandExecution" {
				if cmd := codexCommand(p.Item.Command); ghPRCall(cmd) {
					for _, u := range ghOutputPRs(p.Item.Output) {
						addPR(conv, u)
					}
				}
			}
			noteCodexTool(conv, p, ts)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if conv.SessionID == "" || len(conv.Messages) == 0 {
		return nil, nil
	}
	if name := codexLaunchName(meta); name != "" {
		conv.Title, conv.IsCustomTitle = name, true
	} else {
		conv.Title = codexThreadNames()[conv.SessionID]
	}
	conv.parsedBytes = conv.Size
	finishParse(conv)
	conv.searchText = searchTextOf(*conv)
	conv.searchLower = strings.ToLower(conv.searchText)
	return conv, nil
}

// noteCodexTool records an item that is a tool call, named as Claude's are.
func noteCodexTool(conv *Conversation, p codexPayload, ts string) {
	switch it := p.Item; it.Type {
	case "CommandExecution":
		noteTool(conv, "Shell", strings.TrimSpace(codexShellText(it.Command)), ts)
	case "McpToolCall":
		noteTool(conv, "mcp__"+it.Server+"__"+it.Tool, toolArg(it.Arguments), ts)
	case "FileChange":
		noteTool(conv, "Edit", oneLine(strings.Join(slices.Sorted(maps.Keys(it.Changes)), " ")), ts)
	case "Extension":
		if it.Kind == "web.search" {
			noteTool(conv, "WebSearch", oneLine(it.Query), ts)
		}
	}
}

// codexShellText is the script of a command run as [shell, -lc, script], else
// the whole command line.
func codexShellText(raw json.RawMessage) string {
	var argv []string
	if json.Unmarshal(raw, &argv) == nil && len(argv) >= 3 && (argv[len(argv)-2] == "-lc" || argv[len(argv)-2] == "-c") {
		return argv[len(argv)-1]
	}
	return codexCommand(raw)
}

// codexCommand is a CommandExecution's command line: Codex records it as an
// argv (["/bin/zsh","-lc","gh pr create ..."]) or a string.
func codexCommand(raw json.RawMessage) string {
	var argv []string
	if json.Unmarshal(raw, &argv) == nil {
		return strings.Join(argv, " ")
	}
	var s string
	json.Unmarshal(raw, &s)
	return s
}

// codexIndex caches session_index.jsonl (each thread's title, renamed by
// later lines), reread only when it changes.
var codexIndex struct {
	sync.Mutex
	size  int64
	mod   time.Time
	names map[string]string
}

// codexThreadNames is every thread's title from session_index.jsonl.
func codexThreadNames() map[string]string {
	path := filepath.Join(codexHome(), "session_index.jsonl")
	codexIndex.Lock()
	defer codexIndex.Unlock()
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if codexIndex.names != nil && info.Size() == codexIndex.size && info.ModTime().Equal(codexIndex.mod) {
		return codexIndex.names
	}
	names := map[string]string{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var rec struct {
				ID   string `json:"id"`
				Name string `json:"thread_name"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.ID != "" && rec.Name != "" {
				names[rec.ID] = rec.Name
			}
		}
		f.Close()
	}
	codexIndex.size, codexIndex.mod, codexIndex.names = info.Size(), info.ModTime(), names
	return names
}

// codexRolloutID is the thread id a rollout's file name ends with.
var codexRolloutID = regexp.MustCompile(`-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// codexLiveTTL is how long one look at the running Codex processes is
// trusted: the live tick and every liveness check share it.
const codexLiveTTL = 2 * time.Second

var codexLiveCache struct {
	sync.Mutex
	at  time.Time
	out map[string]liveSession
}

// codexProcCwd is a process's working folder. Stubbed in tests.
var codexProcCwd = func(pid int) string {
	out, err := runBounded(2*time.Second, nil, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(l, "n"); ok {
			return p
		}
	}
	return ""
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
	if codexLiveCache.out != nil && time.Since(codexLiveCache.at) < codexLiveTTL {
		return codexLiveCache.out
	}
	out := map[string]liveSession{}
	ps, err := psAll()
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
	starts := processStartTimes(pids)
	rollouts := codexRollouts()
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
		s.stat = codexStat(codexRollout(thread))
		out[thread] = s
	}
	codexLiveCache.at, codexLiveCache.out = time.Now(), out
	return out
}

// codexThreadOf is the thread a codex TUI started with args is on.
func codexThreadOf(args, rollouts []string, start time.Time, pid int) string {
	for i, a := range args {
		if a == "resume" && i+1 < len(args) && codexThreadID.MatchString(args[i+1]) { // a fork starts a new thread
			return args[i+1]
		}
		if a == "--add-dir" && i+1 < len(args) {
			if name, ok := strings.CutPrefix(args[i+1], launchDir()+string(filepath.Separator)); ok && sessionName.MatchString(name) {
				if th := codexThreadFor(name); th != "" {
					return th
				}
			}
		}
	}
	if start.IsZero() {
		return ""
	}
	cwd := codexProcCwd(pid)
	if cwd == "" {
		return ""
	}
	best, bestMod := "", time.Time{}
	for _, p := range rollouts {
		info, err := os.Stat(p)
		if err != nil || info.ModTime().Before(start) || !info.ModTime().After(bestMod) {
			continue
		}
		if m, ok := readCodexMeta(p); ok && m.Cwd == cwd {
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
				var rec codexRecord
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

// codexResumeArgs is the codex command line that opens conv again: resume
// it, or fork it into a new thread, with ccs launch's sandbox (network on,
// the user's approval mode, ccs's state writable).
func codexResumeArgs(bin string, conv Conversation, fork bool) []string {
	sub := "resume"
	if fork {
		sub = "fork"
	}
	return append([]string{bin, sub, conv.SessionID}, codexSandboxArgs()...)
}

// codexSandboxArgs is how ccs starts Codex: the user's own approval mode;
// network on (Codex's sandbox has none by default, so git and gh fail);
// ccs's state writable, for ccs claim; then any launch folders.
func codexSandboxArgs(dirs ...string) []string {
	args := []string{"-s", "workspace-write", "-a", "on-request", "-c", "sandbox_workspace_write.network_access=true"}
	for _, d := range dirs {
		args = append(args, "--add-dir", d)
	}
	return append(args, "--add-dir", filepath.Dir(settingsPath))
}

// deliverCodex sends the message box's text to a live Codex session: Codex
// queues it and hands it over at the session's next turn, as the user's.
func deliverCodex(id, name, text string) (string, error) {
	if err := codexQueue(id, text); err != nil {
		return "✗ Couldn't queue it for " + name + ": " + err.Error(), fmt.Errorf("%w: %v", errUnsent, err)
	}
	return "✓ Queued for " + name + " with codex queue (as you)", nil
}

package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
)

// Antigravity CLI (agy) sessions: transcripts live under
// $ANTIGRAVITY_APP_DATA_DIR/brain/<id>/.system_generated/logs/transcript.jsonl
// (default ~/.gemini/antigravity-cli), with metadata in
// conversation_summaries.db and workspace fallback in history.jsonl.

// KindAgy marks an agy CLI session; Claude's leave Kind empty.
const KindAgy = "agy"

// AgyLiveTTL is how long one look at the running agy processes is trusted.
const AgyLiveTTL = 2 * time.Second

// AgyHome is agy's data folder. Stubbed in tests.
var AgyHome = func() string {
	if h := os.Getenv("ANTIGRAVITY_APP_DATA_DIR"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

// AgyConvID is an agy conversation id: a uuid.
var AgyConvID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// AgySummary is the metadata agy keeps in conversation_summaries.db for one
// conversation.
type AgySummary struct {
	ID            string `json:"conversation_id"`
	Title         string `json:"title"`
	WorkspaceURIs string `json:"workspace_uris"`
	Status        string `json:"status"`
	ParentID      string `json:"parent_conversation_id"`
	Source        string `json:"source"`
	// LastUserInput is when the latest prompt arrived, as sqlite prints it
	// ("2026-01-02 15:04:05.123456+00:00"): the running turn's start.
	LastUserInput string `json:"last_user_input_time"`
}

// AgyRecord is one line of an agy transcript.jsonl.
type AgyRecord struct {
	StepIndex       int    `json:"step_index"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	Model           string `json:"model"`
	InputTokens     int    `json:"input_tokens"`
	CacheReadTokens int    `json:"cache_read_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	Content         string `json:"content"`
	Thinking        string `json:"thinking"`
	Error           string `json:"error"`
	ToolCalls       []struct {
		Name string            `json:"name"`
		Args map[string]string `json:"args"`
	} `json:"tool_calls"`
}

// What agy writes while it works, measured with agy 1.5.18: a step's record
// is written once the step is done, its created_at being when it began. A
// PLANNER_RESPONSE (the model's step: thinking, a reply, tool calls) is
// followed by one record per tool call, in the calls' order (GENERIC, or
// RUN_COMMAND, VIEW_FILE and the like), status ERROR when the call failed and
// "Command exited with code N" in a command's content; a background command's
// record is RUNNING. A turn ends with a PLANNER_RESPONSE that has no tool
// calls, written when the turn finishes, so the file's time is the turn's end.
// Nothing records a step's or a turn's end otherwise.

// AgyFinal: rec is the last step of a turn, a reply with no tool calls.
func AgyFinal(rec AgyRecord) bool {
	return rec.Type == "PLANNER_RESPONSE" && len(rec.ToolCalls) == 0 && rec.Status != "RUNNING"
}

// AgyAsking: rec asks the user a question (agy's ask_question tool), whose
// result is written only once it is answered.
func AgyAsking(rec AgyRecord) bool {
	if rec.Type != "PLANNER_RESPONSE" {
		return false
	}
	for _, tc := range rec.ToolCalls {
		if tc.Name == "ask_question" {
			return true
		}
	}
	return false
}

// agyExitCode matches a command's exit in its result record.
var agyExitCode = regexp.MustCompile(`(?m)^Command exited with code (-?\d+)\.`)

// agyFailed: a tool call's result record says it failed.
func agyFailed(rec AgyRecord) bool {
	if rec.Status == "ERROR" {
		return true
	}
	m := agyExitCode.FindStringSubmatch(rec.Content)
	return m != nil && m[1] != "0"
}

// agyToolNames are agy's tools under the names the preview gives Claude
// Code's, with the argument that says what each call is for. A file's
// content or an edit's strings are never shown.
var agyToolNames = map[string]struct{ name, arg string }{
	"run_command":          {"Bash", "CommandLine"},
	"read_file":            {"Read", "Path"},
	"view_file":            {"Read", "AbsolutePath"},
	"edit_file":            {"Edit", "Path"},
	"replace_file_content": {"Edit", "TargetFile"},
	"write_file":           {"Write", "Path"},
	"write_to_file":        {"Write", "TargetFile"},
	"grep_search":          {"Grep", "Query"},
	"list_dir":             {"List", "DirectoryPath"},
}

// agyTool is a tool call as the preview draws it: its name and argument.
func agyTool(name string, args map[string]string) (string, string) {
	if t, ok := agyToolNames[name]; ok {
		arg := agyUnquoteArg(args[t.arg])
		if t.name != "Bash" {
			arg = oneLine(arg)
		}
		return t.name, arg
	}
	return name, oneLine(agyUnquoteArg(args["toolSummary"]))
}

// agyTopic is the heading agy gives its thinking ("**Analyzing the
// Request**"), which its terminal shows beside "Thought for"; else its first
// line.
func agyTopic(thinking string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(thinking), "\n")
	return oneLine(strings.Trim(strings.TrimSpace(line), "*"))
}

// agySteps follows one transcript's steps for the preview: tool calls waiting
// for their results, the thinking waiting for its step's end, and the turn.
type agySteps struct {
	results   []int     // conv.Tools indexes of calls whose results are still to come, in order
	thought   int       // conv.Tools index of a Thought whose duration is still to come; -1 for none
	thoughtAt time.Time // when that step began
	topic     string
	turnAt    time.Time // when the turn began: its USER_INPUT
	last      AgyRecord // the last record, if any
}

// stepEnd: a record began at at, so the step before it ended then, which
// gives a waiting Thought its duration.
func (s *agySteps) stepEnd(conv *Conversation, at time.Time) {
	if s.thought < 0 || at.IsZero() || s.thoughtAt.IsZero() {
		return
	}
	t := &conv.Tools[s.thought]
	t.Arg = agyThoughtArg(at.Sub(s.thoughtAt), s.topic)
	s.thought = -1
}

func agyThoughtArg(d time.Duration, topic string) string {
	took := FormatDuration(d)
	if topic == "" {
		return took
	}
	return took + " · " + topic
}

// FormatDuration is d as the preview's turn ends say it: 9s, 6m 53s, 1h 4m.
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", max(int(d.Seconds()), 1))
}

// turnEnded records the end of the turn s follows at end.
func (s *agySteps) turnEnded(conv *Conversation, end time.Time) {
	if s.turnAt.IsZero() || end.Before(s.turnAt) {
		return
	}
	noteTurnEnd(conv, max(end.Sub(s.turnAt).Milliseconds(), 1), end.UTC().Format(time.RFC3339Nano))
	s.turnAt = time.Time{}
}

func agyTime(ts string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, ts)
	return t
}

// AgyTranscripts returns all agy conversation transcripts.
func AgyTranscripts() []string {
	paths, _ := filepath.Glob(filepath.Join(AgyHome(), "brain", "*", ".system_generated", "logs", "transcript.jsonl"))
	return paths
}

// AgyTranscript is id's transcript path, "" when there's none.
func AgyTranscript(id string) string {
	if !AgyConvID.MatchString(id) {
		return ""
	}
	p := filepath.Join(AgyHome(), "brain", id, ".system_generated", "logs", "transcript.jsonl")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// IsAgyTranscript reports whether path is an agy transcript.jsonl under brain/.
func IsAgyTranscript(path string) bool {
	return strings.HasSuffix(filepath.ToSlash(path), "/.system_generated/logs/transcript.jsonl")
}

// AgyConvIDFromPath extracts the conversation UUID from a brain transcript path.
func AgyConvIDFromPath(path string) string {
	id := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	if AgyConvID.MatchString(id) {
		return id
	}
	return ""
}

// AgyWorkspacePaths decodes workspace_uris (JSON array of file:// URIs or
// plain paths) into local filesystem paths.
func AgyWorkspacePaths(raw string) []string {
	if raw == "" {
		return nil
	}
	var uris []string
	if json.Unmarshal([]byte(raw), &uris) != nil {
		return nil
	}
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		if strings.HasPrefix(u, "file://") {
			if parsed, err := url.Parse(u); err == nil && parsed.Path != "" {
				out = append(out, parsed.Path)
				continue
			}
			out = append(out, strings.TrimPrefix(u, "file://"))
		} else if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// AgySummaryCache caches conversation_summaries.db rows, invalidated when the
// db or its WAL file changes.
var AgySummaryCache struct {
	sync.Mutex
	dbSize, walSize int64
	dbMod, walMod   time.Time
	Rows            map[string]AgySummary
}

// QueryAgySummaries reads all rows from conversation_summaries.db using
// sqlite3. Stubbed in tests.
var QueryAgySummaries = func(dbPath string) (map[string]AgySummary, error) {
	out, err := proc.RunBounded(3*time.Second, nil, "sqlite3", "-json", dbPath,
		"SELECT conversation_id, title, workspace_uris, status, parent_conversation_id, source, last_user_input_time FROM conversation_summaries;")
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return map[string]AgySummary{}, nil
	}
	var list []AgySummary
	if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
		return nil, err
	}
	m := make(map[string]AgySummary, len(list))
	for _, r := range list {
		if r.ID != "" {
			m[r.ID] = r
		}
	}
	return m, nil
}

// AgySummaries returns cached metadata from conversation_summaries.db.
func AgySummaries() map[string]AgySummary {
	dbPath := filepath.Join(AgyHome(), "conversation_summaries.db")
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil
	}
	var walSize int64
	var walMod time.Time
	if wi, err := os.Stat(dbPath + "-wal"); err == nil {
		walSize, walMod = wi.Size(), wi.ModTime()
	}
	AgySummaryCache.Lock()
	defer AgySummaryCache.Unlock()
	if AgySummaryCache.Rows != nil &&
		info.Size() == AgySummaryCache.dbSize && info.ModTime().Equal(AgySummaryCache.dbMod) &&
		walSize == AgySummaryCache.walSize && walMod.Equal(AgySummaryCache.walMod) {
		return AgySummaryCache.Rows
	}
	rows, err := QueryAgySummaries(dbPath)
	if err != nil {
		return AgySummaryCache.Rows
	}
	AgySummaryCache.dbSize, AgySummaryCache.dbMod = info.Size(), info.ModTime()
	AgySummaryCache.walSize, AgySummaryCache.walMod = walSize, walMod
	AgySummaryCache.Rows = rows
	return rows
}

// AgyHistoryCache caches workspace paths from history.jsonl as a fallback
// when conversation_summaries.db is unavailable or doesn't have a row yet.
var AgyHistoryCache struct {
	sync.Mutex
	size int64
	mod  time.Time
	Cwds map[string]string
}

// AgyHistoryWorkspaces returns conversation_id -> workspace from history.jsonl.
func AgyHistoryWorkspaces() map[string]string {
	path := filepath.Join(AgyHome(), "history.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	AgyHistoryCache.Lock()
	defer AgyHistoryCache.Unlock()
	if AgyHistoryCache.Cwds != nil && info.Size() == AgyHistoryCache.size && info.ModTime().Equal(AgyHistoryCache.mod) {
		return AgyHistoryCache.Cwds
	}
	cwds := map[string]string{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var rec struct {
				ConversationID string `json:"conversationId"`
				Workspace      string `json:"workspace"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.ConversationID != "" && rec.Workspace != "" {
				cwds[rec.ConversationID] = rec.Workspace
			}
		}
		f.Close()
	}
	AgyHistoryCache.size, AgyHistoryCache.mod, AgyHistoryCache.Cwds = info.Size(), info.ModTime(), cwds
	return cwds
}

// AgyLaunchName is the name ccs launch gave a conversation: its launch folder
// is one of the conversation's workspace paths. "" for one ccs didn't launch.
func AgyLaunchName(paths []string) string {
	base := state.LaunchDir() + string(filepath.Separator)
	for _, p := range paths {
		if name, ok := strings.CutPrefix(p, base); ok && state.SessionName.MatchString(name) {
			return name
		}
	}
	return ""
}

// AgyPrimaryCwd returns the first workspace path that isn't a ccs launch or
// settings directory.
//
// agy lists only the --add-dir folders as a conversation's workspaces, so a
// session ccs launched (both its folders ccs's own) is in the folder its
// launch recorded in LaunchCwdFile; with neither, "" (the caller then tries
// agy's history).
func AgyPrimaryCwd(paths []string) string {
	lBase := state.LaunchDir() + string(filepath.Separator)
	sDir := filepath.Dir(state.SettingsPath)
	launch := ""
	for _, p := range paths {
		if strings.HasPrefix(p, lBase) {
			launch = p
			continue
		}
		if p == sDir {
			continue
		}
		return p
	}
	if launch != "" {
		if b, err := os.ReadFile(filepath.Join(launch, LaunchCwdFile)); err == nil {
			if cwd := strings.TrimSpace(string(b)); filepath.IsAbs(cwd) {
				return cwd
			}
		}
	}
	return ""
}

// LaunchCwdFile, in a ccs launch folder, holds the folder the session was
// started in.
const LaunchCwdFile = "cwd"

// AgyRecordBusy reports whether the last transcript record indicates an
// active/unfinished turn.
func AgyRecordBusy(rec AgyRecord) bool {
	if rec.Status == "RUNNING" {
		return true
	}
	if rec.Type == "USER_INPUT" {
		return true
	}
	if rec.Type == "PLANNER_RESPONSE" && len(rec.ToolCalls) > 0 {
		return true
	}
	return false
}

// AgyResumeArgs is the agy command line that reopens conv.
func AgyResumeArgs(bin string, conv Conversation) []string {
	return []string{bin, "--conversation", conv.SessionID}
}

func agyUserText(content string) string {
	if req := tagText(content, "USER_REQUEST"); req != "" {
		return req
	}
	return strings.TrimSpace(content)
}

func agyUnquoteArg(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}

// ParseAgyFile parses one agy transcript, with the cutoff, size limit and cache
// ParseConversationFile applies to Claude's transcripts.
func ParseAgyFile(path string, cutoff time.Time, maxSize int64) (*Conversation, error) {
	readAt := time.Now()
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if maxSize > 0 && info.Size() > maxSize || !cutoff.IsZero() && info.ModTime().Before(cutoff) {
		return nil, nil
	}
	ParseCacheMu.Lock()
	cached, ok := ParseCache[path]
	ParseCacheMu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		if cached.conv == nil {
			return nil, nil
		}
		c := *cached.conv
		c.ReadAt = readAt
		return &c, nil
	}
	conv, err := parseAgyUncached(path, info)
	if conv != nil {
		conv.ReadAt = readAt
	}
	if err == nil {
		ParseCacheMu.Lock()
		ParseCache[path] = parsedFile{info.Size(), info.ModTime(), conv}
		ParseCacheMu.Unlock()
	}
	return conv, err
}

func parseAgyUncached(path string, info os.FileInfo) (*Conversation, error) {
	id := AgyConvIDFromPath(path)
	if id == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	conv := &Conversation{
		Kind:      KindAgy,
		SessionID: id,
		FilePath:  path,
		Size:      info.Size(),
	}

	if sum, ok := AgySummaries()[id]; ok {
		paths := AgyWorkspacePaths(sum.WorkspaceURIs)
		conv.Cwd = AgyPrimaryCwd(paths)
		if name := AgyLaunchName(paths); name != "" {
			conv.Title, conv.IsCustomTitle = name, true
		} else if sum.Title != "" {
			conv.Title = sum.Title
		}
		if sum.ParentID != "" {
			conv.ParentID = sum.ParentID
			conv.Spawned = true
		}
		if sum.Status == "CASCADE_RUN_STATUS_RUNNING" {
			conv.AgyBusy = true
		}
	}
	if conv.Cwd == "" {
		if cwd := AgyHistoryWorkspaces()[id]; cwd != "" {
			conv.Cwd = cwd
		}
	}

	pendingGHPR := 0
	var lastRec AgyRecord
	hasRec := false
	steps := agySteps{thought: -1}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	m := markOf(conv)
	for sc.Scan() {
		cleanSince(conv, m)
		m = markOf(conv)
		var rec AgyRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		at := agyTime(rec.CreatedAt)
		if hasRec && AgyFinal(lastRec) && rec.Type == "USER_INPUT" {
			// An earlier turn: nothing says when its last reply was done,
			// so it ends where that reply began, a few seconds early, and
			// that reply's thinking has no duration (the next prompt came
			// whenever the user typed it).
			steps.thought = -1
			steps.turnEnded(conv, agyTime(lastRec.CreatedAt))
		}
		steps.stepEnd(conv, at)
		hasRec = true
		lastRec = rec
		if conv.FirstTimestamp == "" && rec.CreatedAt != "" {
			conv.FirstTimestamp = rec.CreatedAt
		}
		switch rec.Type {
		case "USER_INPUT":
			steps.results, steps.turnAt = nil, at
			if text := agyUserText(rec.Content); text != "" {
				conv.Messages = append(conv.Messages, Message{Role: "user", Text: text, Ts: rec.CreatedAt})
			}
		case "PLANNER_RESPONSE":
			if rec.Model != "" {
				conv.Model, conv.ActiveModel = rec.Model, rec.Model
			}
			if rec.InputTokens > 0 || rec.CacheReadTokens > 0 || rec.OutputTokens > 0 {
				conv.ContextTokens = rec.InputTokens + rec.CacheReadTokens
				conv.PeakContext = max(conv.PeakContext, conv.ContextTokens)
				conv.Usage.Input += int64(rec.InputTokens)
				conv.Usage.CacheRead += int64(rec.CacheReadTokens)
				conv.Usage.Output += int64(rec.OutputTokens)
				NoteOutput(conv, rec.CreatedAt, int64(rec.OutputTokens))
			}
			if strings.TrimSpace(rec.Thinking) != "" {
				steps.topic = CleanText(agyTopic(rec.Thinking)) // stepEnd writes it after this line's cleaning
				NoteTool(conv, "Thought", steps.topic, rec.CreatedAt)
				steps.thought, steps.thoughtAt = len(conv.Tools)-1, at
			}
			if text := strings.TrimSpace(rec.Content); text != "" {
				conv.Messages = append(conv.Messages, Message{Role: "assistant", Text: text, Ts: rec.CreatedAt})
				conv.LastError, conv.LastErrorTs = "", ""
			}
			steps.results = steps.results[:0]
			for _, tc := range rec.ToolCalls {
				name, arg := agyTool(tc.Name, tc.Args)
				NoteTool(conv, name, arg, rec.CreatedAt)
				steps.results = append(steps.results, len(conv.Tools)-1)
				if tc.Name == "run_command" {
					cmd := agyUnquoteArg(tc.Args["CommandLine"])
					if GHPRCall(cmd) {
						pendingGHPR++
					}
					if IsSpawn(cmd) {
						conv.Spawns = append(conv.Spawns, SpawnCmd{Ts: rec.CreatedAt, Text: SpawnText(cmd)})
					}
				}
			}
		case "ERROR_MESSAGE":
			if rec.Error != "" {
				line, _, _ := strings.Cut(strings.TrimSpace(rec.Error), "\n")
				conv.LastError, conv.LastErrorTs = line, rec.CreatedAt
			}
		case "SYSTEM_MESSAGE", "CONVERSATION_HISTORY":
		default: // a tool call's result
			if len(steps.results) > 0 {
				if agyFailed(rec) {
					conv.Tools[steps.results[0]].Failed = true
				}
				steps.results = steps.results[1:]
			}
			if (rec.Type == "GENERIC" || rec.Type == "RUN_COMMAND") && pendingGHPR > 0 {
				for _, u := range GHOutputPRs(rec.Content) {
					addPR(conv, u)
				}
				pendingGHPR--
			}
		}
	}
	if hasRec && AgyFinal(lastRec) {
		// The last turn is over: its final record was written as it ended.
		steps.stepEnd(conv, info.ModTime())
		steps.turnEnded(conv, info.ModTime())
	}
	cleanSince(conv, m)
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(conv.Messages) == 0 {
		return nil, nil
	}
	if hasRec && !conv.AgyBusy {
		conv.AgyBusy = AgyRecordBusy(lastRec)
	}
	conv.ParsedBytes = conv.Size
	FinishParse(conv)
	conv.SearchText = SearchTextOf(*conv)
	conv.SearchLower = strings.ToLower(conv.SearchText)
	return conv, nil
}

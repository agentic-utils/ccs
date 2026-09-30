package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Message represents a conversation message
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Ts   string `json:"ts"`
}

// Conversation represents a parsed conversation
type Conversation struct {
	SessionID      string     `json:"session_id"`
	Title          string     `json:"title"`           // custom-title (user-set) or ai-title
	IsCustomTitle  bool       `json:"is_custom_title"` // true only when Title came from a user-set custom-title
	Spawned        bool       `json:"spawned"`         // started by a script/another session (sdk-cli) or a team lead, not typed by you
	Cwd            string     `json:"cwd"`
	FirstTimestamp string     `json:"first_timestamp"`
	LastTimestamp  string     `json:"last_timestamp"`
	Messages       []Message  `json:"messages"`
	FilePath       string     `json:"file_path"`      // Full path to the .jsonl file
	Size           int64      `json:"size"`           // .jsonl file size in bytes
	ContextTokens  int        `json:"context_tokens"` // conversation size as of the last reply (input + cache reads/writes)
	PeakContext    int        `json:"peak_context"`   // largest context seen; over 200k proves a 1M window
	Model          string     `json:"model"`          // model of the last real reply
	ActiveModel    string     `json:"active_model"`   // the model in use now: last reply's, or a later /model switch
	Model1M        bool       `json:"model_1m"`       // the last /model switch chose a 1M-context model
	LastError      string     `json:"last_error"`     // latest surfaced API error, cleared by a later successful reply
	LastErrorTs    string     `json:"last_error_ts"`
	Usage          tokenUsage `json:"usage"` // summed over replies (main transcript, not subagents)

	// Built once at parse and shared through parseCache, so a refresh
	// doesn't rebuild the search text of unchanged conversations.
	searchText, searchLower string

	// Incremental parsing state: bytes consumed up to the last complete line,
	// and whether a user line was seen (it decides Spawned).
	parsedBytes int64
	sawUser     bool
	peerQueued  []string // recent messages from other sessions/ccs the session has queued, for delivery status
	lastUsageID string   // one reply spans several lines with the same id and usage; count it once
	tailApplied bool     // an unterminated last line parsed as a record, so resuming at parsedBytes would repeat it

	// readAt is when this copy was read from disk (the stat before the read).
	// Between a full scan and the live tick, the later read wins; file size
	// can't decide it, since a prune legitimately shrinks a file.
	readAt time.Time
}

// RawMessage represents the JSON structure in conversation files
type RawMessage struct {
	Type    string `json:"type"`
	Cwd     string `json:"cwd"`
	Message struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreation            struct {
				Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
	IsAPIError     bool   `json:"isApiErrorMessage"` // a surfaced API failure (synthetic reply)
	APIErrorStatus int    `json:"apiErrorStatus"`
	Error          string `json:"error"`
	Timestamp      string `json:"timestamp"`
	IsMeta         bool   `json:"isMeta"`     // harness-injected (e.g. the local-command caveat), not typed
	Entrypoint     string `json:"entrypoint"` // cli / claude-desktop = interactive, sdk-cli = claude -p or SDK
	TeamName       string `json:"teamName"`   // set on teammate transcripts spawned by a team lead
	CustomTitle    string `json:"customTitle"`
	AiTitle        string `json:"aiTitle"`
	// A message that arrived mid-turn (typed by the user, or from another
	// session or ccs) is logged as an attachment, not a user line.
	// Raw, and decoded only for those record types, so an unexpected shape
	// elsewhere can't make a whole line unparseable.
	Attachment json.RawMessage `json:"attachment"`
	Operation  string          `json:"operation"` // queue-operation: enqueue, dequeue, remove
	Content    json.RawMessage `json:"content"`   // queue-operation: what was queued
}

// queuedCommand is an attachment record for a message that arrived mid-turn.
type queuedCommand struct {
	Type        string          `json:"type"`
	Prompt      json.RawMessage `json:"prompt"`
	CommandMode string          `json:"commandMode"`
	IsMeta      bool            `json:"isMeta"`
}

// tokenUsage sums a conversation's per-reply usage.
type tokenUsage struct {
	Input, Cache5m, Cache1h, CacheRead, Output int64
}

// effective is the usage in base-input-token equivalents, using Anthropic's
// price multipliers: 5m cache write 1.25x, 1h write 2x, cache read 0.1x,
// output 5x (same definition as claude-dashboard).
func (u tokenUsage) effective() float64 {
	return float64(u.Input) + 1.25*float64(u.Cache5m) + 2*float64(u.Cache1h) + 0.1*float64(u.CacheRead) + 5*float64(u.Output)
}

// pricePerMTok converts effective tokens to an estimated cost: the base input
// price, $5/MTok (Opus-class; claude-dashboard's default).
const pricePerMTok = 5.0

// contextWindow is the context a model can hold. The 1M window is a per-request
// option not recorded in transcripts, so grade against capability: Haiku caps
// at 200k, other Claude models can do 1M, and a context over 200k proves 1M.
func contextWindow(model string, peak int) int {
	m := strings.ToLower(model)
	switch {
	case peak > 200_000, strings.Contains(m, "1m context"):
		return 1_000_000
	case strings.Contains(m, "haiku") || m == "":
		return 200_000
	case strings.Contains(m, "opus"), strings.Contains(m, "sonnet"), strings.Contains(m, "fable"):
		return 1_000_000
	}
	return 200_000
}

// ctxColour grades a context size against its window like claude-dashboard's
// traffic light: green, yellow, amber, red, then flashing red near the limit.
func ctxColour(size, window int) (code string, flash bool) {
	g, y, a, r := 100_000, 125_000, 150_000, 175_000
	if window >= 1_000_000 {
		g, y, a, r = 150_000, 300_000, 450_000, 600_000
	}
	switch {
	case size > r:
		return "1;31", true
	case size > a:
		return "31", false
	case size > y:
		return "38;5;208", false
	case size > g:
		return "33", false
	}
	return "32", false
}

// modelSwitch matches /model's logged result, e.g. "Set model to Opus 5
// (1M context) and saved as your default"; ansiCodes strips its styling.
var (
	modelSwitch = regexp.MustCompile(`Set model to (.+?)(?: and saved|</local-command-stdout>|$)`)
	ansiCodes   = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	modelParts  = regexp.MustCompile(`(?i)(opus|sonnet|haiku|fable)[- ]?(\d+)(?:[-.](\d{1,2}))?\b`)
)

// shortModel renders a model for the list: "claude-opus-5-5" or "Opus 5.5
// (1M context)" become "opus 5.5", with "1M" added when the session is known
// to run the 1M-context version.
func shortModel(conv Conversation) string {
	name := conv.ActiveModel
	if name == "" {
		return ""
	}
	short := strings.ToLower(name)
	if m := modelParts.FindStringSubmatch(name); m != nil {
		short = strings.ToLower(m[1]) + " " + m[2]
		if m[3] != "" {
			short += "." + m[3]
		}
	}
	if conv.Model1M || conv.PeakContext > 200_000 {
		short += " 1M"
	}
	return short
}

// TextContent for parsing content arrays
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// refreshInterval is how often ccs re-scans conversations and live sessions.
// Changed files are reparsed whole, and active sessions are often 100MB+.
const refreshInterval = time.Minute

type refreshTickMsg struct{}

type refreshMsg struct {
	items []listItem
	err   error // scan failed: keep the current list
	live  map[string]bool
	gen   int  // m.gen when the scan started
	early bool // started by the live tick, outside the one-minute schedule
}

func refreshTick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

// refreshStalled reports a scan running far longer than normal (e.g. blocked
// on a hung network mount under ~/.claude/projects). It can't be cancelled,
// so the header just says so rather than showing a silently frozen list.
// refreshNote says what the background refresh is doing, for the header:
// "refreshing…", "refreshed 20s ago", "refresh failed 3m ago". Empty when
// auto-refresh is off, or when a stall is reported instead.
func (m model) refreshNote() string {
	switch {
	case m.reload == nil || m.refreshStalled():
		return ""
	case !m.refreshStarted.IsZero():
		return " · refreshing…"
	case m.lastRefresh.IsZero():
		return ""
	}
	// Minute granularity: a note that changes every second would make an
	// idle ccs redraw constantly (terminals show that as tab activity).
	when := "just now"
	if ago := time.Since(m.lastRefresh); ago >= time.Minute {
		when = fmt.Sprintf("%dm ago", int(ago.Minutes()))
	}
	if m.refreshFailed {
		return " · refresh failed, list from " + when
	}
	return " · refreshed " + when
}

func (m model) refreshStalled() bool {
	return !m.refreshStarted.IsZero() && time.Since(m.refreshStarted) > 5*time.Minute
}

// keepNewer returns scanned, but where the list holds a copy of a
// conversation read from disk after the scan read it (the live tick got
// there later), keeps that copy, so a slow scan can't roll it back.
func keepNewer(current, scanned []listItem) []listItem {
	have := make(map[string]listItem, len(current))
	for _, item := range current {
		have[item.conv.SessionID] = item
	}
	out := make([]listItem, len(scanned))
	for i, item := range scanned {
		if cur, ok := have[item.conv.SessionID]; ok && cur.conv.FilePath == item.conv.FilePath && cur.conv.readAt.After(item.conv.readAt) {
			item = cur
		}
		out[i] = item
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].conv.LastTimestamp > out[j].conv.LastTimestamp
	})
	return out
}

// startRefresh begins a full scan off the UI goroutine.
func (m *model) startRefresh(early bool) tea.Cmd {
	m.refreshStarted = time.Now()
	reload, gen := m.reload, m.gen
	return func() tea.Msg {
		items, err := reload()
		return refreshMsg{items: items, err: err, live: readLiveSessions(), gen: gen, early: early}
	}
}

// applyRefresh swaps in freshly loaded items, keeping the cursor on the same
// conversation and re-running the current filter.
func (m *model) applyRefresh(msg refreshMsg) {
	if msg.err != nil {
		return
	}
	var selectedID string
	if len(m.filtered) > 0 {
		selectedID = m.filtered[m.cursor].conv.SessionID
	}
	prevScroll := m.previewScroll
	m.items = msg.items
	m.lastFilterQuery = "" // force a full rescan, not incremental narrowing
	m.hits = &hitCounter{byID: make(map[string]int)}
	m.preview = &previewCache{}
	m.updateFilter()
	for i, item := range m.filtered {
		if item.conv.SessionID == selectedID {
			m.cursor = i
			m.previewScroll = min(prevScroll, m.maxPreviewScroll())
			break
		}
	}
}

// ============================================================================
// Data loading (preserved from original)
// ============================================================================

// getProjectsDir returns the path to the Claude projects directory
// Declared as a variable so it can be overridden in tests
var getProjectsDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func extractText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str
	}

	var arr []TextContent
	if err := json.Unmarshal(content, &arr); err == nil {
		var parts []string
		for _, item := range arr {
			if item.Type == "text" && item.Text != "" {
				parts = append(parts, item.Text)
			}
		}
		return strings.Join(parts, " ")
	}

	return ""
}

func parseConversationFile(path string, cutoff time.Time, maxSize int64) (*Conversation, error) {
	readAt := time.Now()
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(info.Name(), "agent-") {
		return nil, nil
	}

	// Skip files larger than maxSize (0 means no limit)
	if maxSize > 0 && info.Size() > maxSize {
		return nil, nil
	}

	// Skip files not modified since cutoff (file mtime check)
	if !cutoff.IsZero() && info.ModTime().Before(cutoff) {
		return nil, nil
	}

	parseCacheMu.Lock()
	cached, ok := parseCache[path]
	parseCacheMu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		if cached.conv == nil { // cached as "no conversation" (no messages yet)
			return nil, nil
		}
		c := *cached.conv // copy: the cached one is shared across goroutines
		c.readAt = readAt
		return &c, nil
	}
	conv, err := parseConversationUncached(path, info)
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

// parseCache lets auto-refresh reparse only files that changed since the last
// scan. ponytail: entries for files deleted outside ccs are never evicted;
// negligible for a session-long TUI.
type parsedFile struct {
	size    int64
	modTime time.Time
	conv    *Conversation
}

var (
	parseCacheMu sync.Mutex
	parseCache   = make(map[string]parsedFile)
)

func parseConversationUncached(path string, info os.FileInfo) (*Conversation, error) {
	conv := &Conversation{
		SessionID: strings.TrimSuffix(info.Name(), ".jsonl"),
		FilePath:  path,
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	complete, total, tail, err := consumeLines(conv, file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	conv.parsedBytes, conv.Size, conv.tailApplied = complete, total, tail
	if len(conv.Messages) == 0 {
		return nil, nil
	}
	finishParse(conv)
	conv.searchText = searchTextOf(*conv)
	conv.searchLower = strings.ToLower(conv.searchText)
	return conv, nil
}

// parseAppended brings prev up to date with its file, parsing only the lines
// appended since prev was parsed, so a live 100MB transcript costs only its
// new lines. A line caught mid-write wasn't applied, so parsing resumes at it.
// Falls back to a full parse when resuming isn't safe: the file shrank (e.g.
// a prune) or an unterminated last line was applied as a record. Returns prev
// itself when the file hasn't grown.
func parseAppended(prev *Conversation) (*Conversation, error) {
	readAt := time.Now()
	info, err := os.Stat(prev.FilePath)
	if err != nil {
		return nil, err
	}
	if info.Size() == prev.Size {
		return prev, nil
	}
	// A read that missed an earlier tick's deadline still cached its result;
	// continue from it rather than re-reading the same bytes every tick.
	parseCacheMu.Lock()
	if cached, ok := parseCache[prev.FilePath]; ok && cached.conv != nil && cached.conv.parsedBytes > prev.parsedBytes && cached.conv.readAt.After(prev.readAt) && cached.conv.parsedBytes <= info.Size() {
		prev = cached.conv
	}
	parseCacheMu.Unlock()
	if info.Size() == prev.Size {
		c := *prev
		c.readAt = readAt
		return &c, nil
	}
	if info.Size() < prev.Size || prev.tailApplied || prev.parsedBytes > info.Size() {
		return parseConversationFile(prev.FilePath, time.Time{}, 0)
	}
	file, err := os.Open(prev.FilePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(prev.parsedBytes, io.SeekStart); err != nil {
		return nil, err
	}
	c := *prev
	c.Messages = slices.Clip(prev.Messages) // appends must not write into prev's array
	if c.Cwd == "unknown" {
		c.Cwd = ""
	}
	complete, total, tail, err := consumeLines(&c, file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", prev.FilePath, err)
	}
	c.parsedBytes = prev.parsedBytes + complete
	c.Size = prev.parsedBytes + total
	c.tailApplied = tail
	c.readAt = readAt
	finishParse(&c)
	// Rebuilt exactly as a full parse would, and only when something it
	// covers changed: most appends are tool calls with no searchable text.
	c.searchText, c.searchLower = appendedSearchText(prev, &c)
	if info, err := file.Stat(); err == nil {
		parseCacheMu.Lock()
		parseCache[c.FilePath] = parsedFile{c.Size, info.ModTime(), &c}
		parseCacheMu.Unlock()
	}
	return &c, nil
}

// appendedSearchText returns c's search text, identical to searchTextOf(c).
// When only messages were added it extends prev's instead: searchTextOf ends
// with the last timestamp, so swap that suffix for the new messages and the
// new timestamp, lowercasing just the new part. Anything else rebuilds.
func appendedSearchText(prev, c *Conversation) (string, string) {
	if len(c.Messages) == len(prev.Messages) && c.Title == prev.Title && c.Cwd == prev.Cwd && c.FirstTimestamp == prev.FirstTimestamp {
		return prev.searchText, prev.searchLower
	}
	oldTail := " " + formatTimestamp(prev.LastTimestamp)
	oldTailLower := strings.ToLower(oldTail)
	if len(c.Messages) > len(prev.Messages) && c.Title == prev.Title && c.Cwd == prev.Cwd && c.FirstTimestamp == prev.FirstTimestamp &&
		strings.HasSuffix(prev.searchText, oldTail) && strings.HasSuffix(prev.searchLower, oldTailLower) {
		parts := make([]string, 0, len(c.Messages)-len(prev.Messages)+1)
		for _, msg := range c.Messages[len(prev.Messages):] {
			parts = append(parts, msg.Text)
		}
		parts = append(parts, formatTimestamp(c.LastTimestamp))
		add := " " + strings.Join(parts, " ")
		return strings.TrimSuffix(prev.searchText, oldTail) + add,
			strings.TrimSuffix(prev.searchLower, oldTailLower) + strings.ToLower(add)
	}
	text := searchTextOf(*c)
	return text, strings.ToLower(text)
}

// consumeLines parses JSONL records from r into conv. complete counts bytes
// up to the last newline; total includes a trailing unterminated line. That
// line is still parsed (a finished file may lack the final newline); tail
// reports whether it held a whole record, as opposed to a write in progress.
func consumeLines(conv *Conversation, r io.Reader) (complete, total int64, tail bool, err error) {
	size := 1 << 20
	if f, ok := r.(*os.File); ok {
		// Appends are often a few hundred bytes; don't allocate 1MB for them.
		if info, err := f.Stat(); err == nil {
			if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
				size = int(min(max(info.Size()-pos, 4096), 1<<20))
			}
		}
	}
	br := bufio.NewReaderSize(r, size)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			total += int64(len(line))
			applied := parseLine(conv, line)
			if line[len(line)-1] == '\n' {
				complete = total
			} else {
				tail = applied
			}
		}
		if err == io.EOF {
			return complete, total, tail, nil
		}
		if err != nil {
			return complete, total, tail, err
		}
	}
}

// parseLine applies one JSONL record to conv; false if it isn't valid JSON.
func parseLine(conv *Conversation, line []byte) bool {
	var raw RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return false
	}
	switch raw.Type {
	case "custom-title":
		conv.Title = raw.CustomTitle // user-set name wins over ai-title
		conv.IsCustomTitle = raw.CustomTitle != ""
	case "ai-title":
		if conv.Title == "" {
			conv.Title = raw.AiTitle
		}
	case "user":
		if !conv.sawUser {
			// The first user line says how the session was started.
			conv.sawUser = true
			conv.Spawned = raw.Entrypoint == "sdk-cli" || raw.TeamName != ""
		}
		if conv.Cwd == "" {
			conv.Cwd = raw.Cwd
		}
		// isMeta lines are harness-injected (e.g. the local-command caveat).
		text := extractText(raw.Message.Content)
		// "/model" logs its result as command output; a switch after the last
		// reply is the model the session will use next.
		if m := modelSwitch.FindStringSubmatch(ansiCodes.ReplaceAllString(text, "")); m != nil {
			name := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(m[1], "`", ""), "(default)", ""))
			conv.ActiveModel, conv.Model1M = name, strings.Contains(name, "1M")
		}
		// Messages from other sessions (and from ccs) are harness-marked but
		// are real conversation turns, so keep them.
		if (!raw.IsMeta || peerMessage.MatchString(text)) && strings.TrimSpace(text) != "" {
			if conv.FirstTimestamp == "" {
				conv.FirstTimestamp = raw.Timestamp
			}
			conv.Messages = append(conv.Messages, Message{Role: "user", Text: text, Ts: raw.Timestamp})
		}
	case "attachment":
		var a queuedCommand
		if json.Unmarshal(raw.Attachment, &a) != nil || a.Type != "queued_command" || a.CommandMode != "prompt" {
			break
		}
		text := extractText(a.Prompt)
		if (!a.IsMeta || peerMessage.MatchString(text)) && strings.TrimSpace(text) != "" {
			conv.Messages = append(conv.Messages, Message{Role: "user", Text: text, Ts: raw.Timestamp})
		}
	case "queue-operation":
		if text := extractText(raw.Content); raw.Operation == "enqueue" && peerMessage.MatchString(text) {
			if len(conv.peerQueued) >= 20 {
				conv.peerQueued = conv.peerQueued[1:]
			}
			conv.peerQueued = append(conv.peerQueued, text)
		}
	case "assistant":
		if raw.IsAPIError {
			conv.LastError = strings.TrimSpace(fmt.Sprintf("%s %s", strconv.Itoa(raw.APIErrorStatus), raw.Error))
			conv.LastErrorTs = raw.Timestamp
		}
		// Each reply's usage counts the whole conversation it was sent, so
		// the last one is the current context. Zero-usage lines are
		// placeholders (e.g. API errors) and would read as an empty context.
		u := raw.Message.Usage
		if ctx := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens; ctx > 0 {
			conv.ContextTokens = ctx
			conv.PeakContext = max(conv.PeakContext, ctx)
			conv.LastError, conv.LastErrorTs = "", "" // a later successful reply clears it
			if raw.Message.Model != "" && raw.Message.Model != "<synthetic>" {
				conv.Model = raw.Message.Model
				conv.ActiveModel = raw.Message.Model
			}
			// Consecutive lines of one reply repeat its id and usage.
			if raw.Message.ID == "" || raw.Message.ID != conv.lastUsageID {
				conv.lastUsageID = raw.Message.ID
				c5, c1 := u.CacheCreation.Ephemeral5m, u.CacheCreation.Ephemeral1h
				if c5+c1 == 0 { // older replies don't split cache writes: count them as 5m
					c5 = u.CacheCreationInputTokens
				}
				conv.Usage.Input += int64(u.InputTokens)
				conv.Usage.Cache5m += int64(c5)
				conv.Usage.Cache1h += int64(c1)
				conv.Usage.CacheRead += int64(u.CacheReadInputTokens)
				conv.Usage.Output += int64(u.OutputTokens)
			}
		}
		if text := extractText(raw.Message.Content); strings.TrimSpace(text) != "" {
			conv.Messages = append(conv.Messages, Message{Role: "assistant", Text: text, Ts: raw.Timestamp})
		}
	}
	return true
}

func finishParse(conv *Conversation) {
	if len(conv.Messages) > 0 {
		conv.LastTimestamp = conv.Messages[len(conv.Messages)-1].Ts
	}
	if conv.Cwd == "" {
		conv.Cwd = "unknown"
	}
}

// parseForScan parses one file for a full scan. A panic skips that file
// rather than killing ccs from a worker goroutine.
func parseForScan(path string, cutoff time.Time, maxSize int64) (conv *Conversation) {
	defer recoverWorker()
	conv, err := parseConversationFile(path, cutoff, maxSize)
	if err != nil {
		return nil
	}
	return conv
}

// workerPanicLog is where recoverWorker records panics; workerPanicked makes
// the header say so.
var (
	workerPanicLog = filepath.Join(os.TempDir(), "ccs-panic.log")
	workerPanicked atomic.Bool
)

// recoverWorker, deferred in goroutines ccs starts itself, stops a panic there
// from killing the process. bubbletea only recovers its own event loop, so an
// unrecovered worker panic exits without restoring the terminal and leaves it
// garbled. The stack is appended to workerPanicLog.
func recoverWorker() {
	if r := recover(); r != nil {
		recoverWorkerValue(r)
	}
}

// recoverWorkerValue records a recovered panic value (see recoverWorker).
func recoverWorkerValue(r any) {
	workerPanicked.Store(true)
	if f, err := os.OpenFile(workerPanicLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s ccs %s: panic: %v\n%s\n", time.Now().Format(time.RFC3339), version, r, debug.Stack())
		f.Close()
	}
}

func getConversations(cutoff time.Time, maxSize int64, excludeDirs []string) ([]Conversation, error) {
	projectsDir := getProjectsDir()
	// Walk swallows a root error, so a vanished dir would read as "no
	// conversations" and a refresh would wipe the list.
	if _, err := os.Stat(projectsDir); err != nil {
		return nil, err
	}

	var files []string
	err := filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && info.Name() == "subagents" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			for _, exc := range excludeDirs {
				if strings.Contains(info.Name(), exc) {
					return filepath.SkipDir
				}
			}
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") && !strings.HasPrefix(info.Name(), "agent-") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Worker pool to limit concurrent file operations
	const numWorkers = 8
	jobs := make(chan string, len(files))
	results := make(chan *Conversation, len(files))

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				if conv := parseForScan(path, cutoff, maxSize); conv != nil {
					results <- conv
				}
			}
		}()
	}

	for _, file := range files {
		jobs <- file
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	var conversations []Conversation
	for conv := range results {
		conversations = append(conversations, *conv)
	}

	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].LastTimestamp > conversations[j].LastTimestamp
	})

	return conversations, nil
}

func formatTimestamp(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if len(ts) >= 16 {
			return ts[:16]
		}
		return ts
	}
	return t.Local().Format("2006-01-02 15:04")
}

// formatAgo renders how long before now ts was, for the WHEN column:
// "now", "5m ago", "3h ago", "2d ago", "3w ago", "4mo ago", "1y ago". The
// full timestamp stays searchable and shows on each message in the preview.
func formatAgo(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	var n int
	var unit string
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		n, unit = int(d.Minutes()), "m"
	case d < 24*time.Hour:
		n, unit = int(d.Hours()), "h"
	case d < 14*24*time.Hour:
		n, unit = int(d.Hours()/24), "d"
	case d < 60*24*time.Hour:
		n, unit = int(d.Hours()/24/7), "w"
	case d < 365*24*time.Hour:
		n, unit = int(d.Hours()/24/30), "mo"
	default:
		n, unit = int(d.Hours()/24/365), "y"
	}
	return fmt.Sprintf("%d%s ago", n, unit)
}

// formatBytes renders a byte count compactly (fits the 6-wide SIZE column).
// formatTokens renders a token count for the 5-wide CTX column: 950, 12k,
// 281k, 1.2M. Blank when unknown (no reply with usage yet).
func formatTokens(n int) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dMB", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%dKB", n/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func truncate(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	if maxLen < 3 {
		return string(r[:maxLen]) // no room for the ellipsis
	}
	return string(r[:maxLen-3]) + "..."
}

// getTopic returns the session name (custom/ai title), else first user message, else session ID
func getTopic(conv Conversation) string {
	if conv.Title != "" {
		return conv.Title
	}
	for _, msg := range conv.Messages {
		if msg.Role != "user" {
			continue
		}
		// Harness text (command echoes and output, task notifications, teammate
		// messages) is tag-wrapped. ponytail: a real prompt starting with "<"
		// is treated as harness text too.
		if !strings.HasPrefix(msg.Text, "<") {
			return msg.Text
		}
		// Slash and ! commands: show the command itself.
		if cmd := tagText(msg.Text, "command-name"); cmd != "" {
			return cmd
		}
		if cmd := tagText(msg.Text, "bash-input"); cmd != "" {
			return "! " + cmd
		}
	}
	return conv.SessionID
}

// tagText returns the trimmed text inside the first <tag>...</tag> of s, or "".
func tagText(s, tag string) string {
	_, rest, ok := strings.Cut(s, "<"+tag+">")
	if !ok {
		return ""
	}
	inner, _, ok := strings.Cut(rest, "</"+tag+">")
	if !ok {
		return ""
	}
	return strings.TrimSpace(inner)
}

// buildItems creates list items from conversations
func buildItems(conversations []Conversation) []listItem {
	items := make([]listItem, 0, len(conversations))
	for _, conv := range conversations {
		if conv.searchText == "" { // not from parseConversationFile (e.g. tests)
			conv.searchText = searchTextOf(conv)
			conv.searchLower = strings.ToLower(conv.searchText)
		}
		items = append(items, listItem{conv: conv, searchText: conv.searchText, searchLower: conv.searchLower})
	}
	return items
}

// searchTextOf joins everything a conversation can be found by.
func searchTextOf(conv Conversation) string {
	parts := []string{conv.SessionID, conv.Title, conv.Cwd, formatTimestamp(conv.FirstTimestamp)}
	// Include assistant messages too so a conversation is findable by what
	// Claude said, matching the HITS column and preview.
	for _, msg := range conv.Messages {
		parts = append(parts, msg.Text)
	}
	// Last, so new messages can be appended without rebuilding (parseAppended).
	parts = append(parts, formatTimestamp(conv.LastTimestamp))
	return strings.Join(parts, " ")
}

// Package transcript reads Claude Code and Codex session transcripts into
// Conversations: full and incremental parsing behind a cache, search text
// and the search rule, background tasks and spawned sessions, PR links in
// gh output, messages between sessions, token figures, and lossless pruning.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
)

// Message represents a conversation message
type Message struct {
	Role string `json:"role"` // user, assistant, or sent: a SendMessage to another session
	Text string `json:"text"`
	Ts   string `json:"ts"`
	To   string `json:"to,omitempty"`   // sent: the address (uds:…sock or an agent id)
	Note string `json:"note,omitempty"` // sent: its summary
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
	Usage          TokenUsage `json:"usage"`               // summed over replies (main transcript, not subagents)
	Kind           string     `json:"kind,omitempty"`      // "codex" for a Codex session; empty for Claude's
	Reasoning      int64      `json:"reasoning,omitempty"` // Codex: reasoning tokens, part of Usage.Output
	Cost           float64    `json:"cost,omitempty"`      // Codex: USD at OpenAI's prices (openAIPrices); 0 when a model isn't in the table
	Unpriced       bool       `json:"unpriced,omitempty"`  // Codex: some usage came from a model openAIPrices doesn't have
	ParentID       string     `json:"parent_id,omitempty"` // a Codex subagent's parent thread
	Window         int        `json:"-"`                   // the context window the transcript states (Codex does); 0: inferred from the model
	CodexBusy      bool       // a Codex session's last turn hasn't finished

	// Built once at parse and shared through parseCache, so a refresh
	// doesn't rebuild the search text of unchanged conversations.
	SearchText, SearchLower string

	// Incremental parsing state: bytes consumed up to the last complete line,
	// and whether a user line was seen (it decides Spawned).
	ParsedBytes int64
	sawUser     bool
	sdkCLI      bool           // its first user line's entrypoint is sdk-cli (claude -p)
	firstPrompt string         // the first promptId seen
	morePrompts bool           // a second promptId: something talked to it after its first prompt
	CodexExec   bool           // a Codex session started by codex exec
	PeerQueued  []string       // recent messages from other sessions/ccs the session has queued, for delivery status
	lastUsageID string         // one reply spans several lines with the same id and usage; count it once
	TailApplied bool           // an unterminated last line parsed as a record, so resuming at ParsedBytes would repeat it
	Pending     *toolCall      // the last tool call still without a result: what a waiting session asks about
	PRs         []string       // its PRs, first touched first: Claude Code's pr-link records, and those gh pr create/merge printed (ghPRCall)
	GHCalls     []string       // gh pr create/merge calls awaiting their output
	Spawns      []SpawnCmd     // claude -p commands it ran, to find the sessions they started
	Peers       []PeerRef      // its messages to and from other sessions, to link the two ends
	outputs     []replyOutput  // each reply's output tokens, for the working line's count this turn
	Tools       []ToolUse      // its tool calls and turn ends, for the preview; not messages, so counts, times and search ignore them
	PeerNotes   map[int]string // set on the copy the preview draws: each peer message's other end (peerLabels)
	calls       []BgTask       // Bash/Agent/Monitor calls awaiting the result that says whether they went to the background
	Tasks       []BgTask       // its background tasks (tasks.go)

	// ReadAt is when this copy was read from disk (the stat before the read).
	// Between a full scan and the live tick, the later read wins; file size
	// can't decide it, since a prune legitimately shrinks a file.
	ReadAt time.Time
}

// RawMessage represents the JSON structure in conversation files
type RawMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"` // system lines: turn_duration
	// DurationMs is a turn_duration line's: how long the turn took, the figure
	// Claude Code's own "worked for" line shows.
	DurationMs int64  `json:"durationMs"`
	Cwd        string `json:"cwd"`
	Message    struct {
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
	Entrypoint     string `json:"entrypoint"` // cli, claude-desktop, sdk-ts (T3 Code) = interactive; sdk-cli = claude -p
	PromptID       string `json:"promptId"`   // one per prompt the session was given
	TeamName       string `json:"teamName"`   // set on teammate transcripts spawned by a team lead
	CustomTitle    string `json:"customTitle"`
	PRURL          string `json:"prUrl"` // pr-link: a PR a gh command in the session touched
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

// TokenUsage sums a conversation's per-reply usage.
type TokenUsage struct {
	Input, Cache5m, Cache1h, CacheRead, Output int64
}

// Effective is the usage in base-input-token equivalents, using Anthropic's
// price multipliers: 5m cache write 1.25x, 1h write 2x, cache read 0.1x,
// output 5x (same definition as claude-dashboard).
func (u TokenUsage) Effective() float64 {
	return float64(u.Input) + 1.25*float64(u.Cache5m) + 2*float64(u.Cache1h) + 0.1*float64(u.CacheRead) + 5*float64(u.Output)
}

// PricePerMTok converts effective tokens to an estimated cost: the base input
// price, $5/MTok (Opus-class; claude-dashboard's default).
const PricePerMTok = 5.0

// openAIPrice is USD per 1M tokens: uncached input, cached input, cache
// write (30 minutes) and output (reasoning included).
type openAIPrice struct{ in, cached, write, out float64 }

// openAIPrices is OpenAI's standard tier for the models Codex sessions use,
// from https://developers.openai.com/api/docs/pricing (read 2026-11-03). A
// request whose input is over longAbove tokens is priced at long. A model
// not here shows its tokens with no cost; never guess one.
var openAIPrices = map[string]struct {
	std, long openAIPrice
	longAbove int64
}{
	"gpt-6.1-sol": {openAIPrice{2, 0.10, 2.50, 10}, openAIPrice{4, 0.20, 5, 15}, 272_000},
}

// openAICost is one request's cost and whether model has a price.
func openAICost(model string, input, cached, write, output int64) (float64, bool) {
	p, ok := openAIPrices[model]
	if !ok {
		return 0, false
	}
	r := p.std
	if p.longAbove > 0 && input > p.longAbove {
		r = p.long
	}
	fresh := max(input-cached-write, 0)
	return (float64(fresh)*r.in + float64(cached)*r.cached + float64(write)*r.write + float64(output)*r.out) / 1e6, true
}

// ContextWindow is the context a model can hold. The 1M window is a per-request
// option not recorded in transcripts, so grade against capability: Haiku caps
// at 200k, other Claude models can do 1M, and a context over 200k proves 1M.
func ContextWindow(model string, peak int) int {
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

// WindowSize is conv's context WindowSize: the one its transcript states, else
// contextWindow's estimate from the model.
func (conv Conversation) WindowSize() int {
	if conv.Window > 0 {
		return conv.Window
	}
	return ContextWindow(conv.ActiveModel, conv.PeakContext)
}

// CtxColour grades a context size against its window like claude-dashboard's
// traffic light: green, yellow, amber, red, then flashing red near the limit.
func CtxColour(size, window int) (code string, flash bool) {
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
// (1M context) and saved as your default"; AnsiCodes strips its styling.
var (
	modelSwitch = regexp.MustCompile(`Set model to (.+?)(?: and saved|</local-command-stdout>|$)`)
	AnsiCodes   = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	modelParts  = regexp.MustCompile(`(?i)(opus|sonnet|haiku|fable)[- ]?(\d+)(?:[-.](\d{1,2}))?\b`)
)

// ShortModel renders a model for the list: "claude-opus-5-5" or "Opus 5.5
// (1M context)" become "opus 5.5", with "1M" added when the session is known
// to run the 1M-context version.
func ShortModel(conv Conversation) string {
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

// RefreshInterval is how often ccs re-scans conversations and live sessions.
// Changed files are reparsed whole, and active sessions are often 100MB+.
const RefreshInterval = time.Minute

type RefreshTickMsg struct{}

func RefreshTick() tea.Cmd {
	return tea.Tick(RefreshInterval, func(time.Time) tea.Msg { return RefreshTickMsg{} })
}

// GetProjectsDir returns the path to the Claude projects directory
// Declared as a variable so it can be overridden in tests
var GetProjectsDir = func() string {
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

func ParseConversationFile(path string, cutoff time.Time, maxSize int64) (*Conversation, error) {
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

	ParseCacheMu.Lock()
	cached, ok := ParseCache[path]
	ParseCacheMu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		if cached.conv == nil { // cached as "no conversation" (no messages yet)
			return nil, nil
		}
		c := *cached.conv // copy: the cached one is shared across goroutines
		c.ReadAt = readAt
		return &c, nil
	}
	conv, err := ParseConversationUncached(path, info)
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

// parseCache lets auto-refresh reparse only files that changed since the last
// scan. ponytail: entries for files deleted outside ccs are never evicted;
// negligible for a session-long TUI.
type parsedFile struct {
	size    int64
	modTime time.Time
	conv    *Conversation
}

var (
	ParseCacheMu sync.Mutex
	ParseCache   = make(map[string]parsedFile)
)

func ParseConversationUncached(path string, info os.FileInfo) (*Conversation, error) {
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
	conv.ParsedBytes, conv.Size, conv.TailApplied = complete, total, tail
	if len(conv.Messages) == 0 {
		return nil, nil
	}
	FinishParse(conv)
	conv.SearchText = SearchTextOf(*conv)
	conv.SearchLower = strings.ToLower(conv.SearchText)
	return conv, nil
}

// ParseAppended brings prev up to date with its file, parsing only the lines
// appended since prev was parsed, so a live 100MB transcript costs only its
// new lines. A line caught mid-write wasn't applied, so parsing resumes at it.
// Falls back to a full parse when resuming isn't safe: the file shrank (e.g.
// a prune) or an unterminated last line was applied as a record. Returns prev
// itself when the file hasn't grown.
func ParseAppended(prev *Conversation) (*Conversation, error) {
	if prev.Kind == KindCodex { // small, and a resume rewrites it: parsed again whole
		return ParseCodexFile(prev.FilePath, time.Time{}, 0)
	}
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
	ParseCacheMu.Lock()
	if cached, ok := ParseCache[prev.FilePath]; ok && cached.conv != nil && cached.conv.ParsedBytes > prev.ParsedBytes && cached.conv.ReadAt.After(prev.ReadAt) && cached.conv.ParsedBytes <= info.Size() {
		prev = cached.conv
	}
	ParseCacheMu.Unlock()
	if info.Size() == prev.Size {
		c := *prev
		c.ReadAt = readAt
		return &c, nil
	}
	if info.Size() < prev.Size || prev.TailApplied || prev.ParsedBytes > info.Size() {
		return ParseConversationFile(prev.FilePath, time.Time{}, 0)
	}
	file, err := os.Open(prev.FilePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(prev.ParsedBytes, io.SeekStart); err != nil {
		return nil, err
	}
	c := *prev
	c.Messages = slices.Clip(prev.Messages) // appends must not write into prev's array
	c.Tools = slices.Clip(prev.Tools)
	c.outputs = slices.Clip(prev.outputs)
	if c.Cwd == "unknown" {
		c.Cwd = ""
	}
	complete, total, tail, err := consumeLines(&c, file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", prev.FilePath, err)
	}
	c.ParsedBytes = prev.ParsedBytes + complete
	c.Size = prev.ParsedBytes + total
	c.TailApplied = tail
	c.ReadAt = readAt
	FinishParse(&c)
	// Rebuilt exactly as a full parse would, and only when something it
	// covers changed: most appends are tool calls with no searchable text.
	c.SearchText, c.SearchLower = appendedSearchText(prev, &c)
	if info, err := file.Stat(); err == nil {
		ParseCacheMu.Lock()
		ParseCache[c.FilePath] = parsedFile{c.Size, info.ModTime(), &c}
		ParseCacheMu.Unlock()
	}
	return &c, nil
}

// appendedSearchText returns c's search text, identical to searchTextOf(c).
// When only messages were added it extends prev's instead: searchTextOf ends
// with the last timestamp, so swap that suffix for the new messages and the
// new timestamp, lowercasing just the new part. Anything else rebuilds.
func appendedSearchText(prev, c *Conversation) (string, string) {
	if len(c.Messages) == len(prev.Messages) && c.Title == prev.Title && c.Cwd == prev.Cwd && c.FirstTimestamp == prev.FirstTimestamp {
		return prev.SearchText, prev.SearchLower
	}
	oldTail := SearchSep + FormatTimestamp(prev.LastTimestamp)
	oldTailLower := strings.ToLower(oldTail)
	if len(c.Messages) > len(prev.Messages) && c.Title == prev.Title && c.Cwd == prev.Cwd && c.FirstTimestamp == prev.FirstTimestamp &&
		strings.HasSuffix(prev.SearchText, oldTail) && strings.HasSuffix(prev.SearchLower, oldTailLower) {
		parts := make([]string, 0, len(c.Messages)-len(prev.Messages)+1)
		for _, msg := range c.Messages[len(prev.Messages):] {
			parts = append(parts, msg.Text)
		}
		parts = append(parts, FormatTimestamp(c.LastTimestamp))
		add := SearchSep + strings.Join(parts, SearchSep)
		return strings.TrimSuffix(prev.SearchText, oldTail) + add,
			strings.TrimSuffix(prev.SearchLower, oldTailLower) + strings.ToLower(add)
	}
	text := SearchTextOf(*c)
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
			applied := ParseLine(conv, line)
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

// ParseLine applies one JSONL record to conv; false if it isn't valid JSON.
func ParseLine(conv *Conversation, line []byte) bool {
	var raw RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return false
	}
	noteEnds(conv, line)
	switch raw.Type {
	case "custom-title":
		conv.Title = raw.CustomTitle // user-set name wins over ai-title
		conv.IsCustomTitle = raw.CustomTitle != ""
	case "pr-link":
		addPR(conv, raw.PRURL)
	case "system":
		if raw.Subtype == "turn_duration" {
			noteTurnEnd(conv, raw.DurationMs, raw.Timestamp)
		}
	case "ai-title":
		if conv.Title == "" {
			conv.Title = raw.AiTitle
		}
	case "user":
		if !conv.sawUser {
			// The first user line says how the session was started.
			conv.sawUser = true
			conv.Spawned = raw.Entrypoint == "sdk-cli" || raw.TeamName != ""
			conv.sdkCLI = raw.Entrypoint == "sdk-cli"
		}
		if raw.PromptID != "" {
			if conv.firstPrompt == "" {
				conv.firstPrompt = raw.PromptID
			} else if raw.PromptID != conv.firstPrompt {
				conv.morePrompts = true
			}
		}
		if conv.Cwd == "" {
			conv.Cwd = raw.Cwd
		}
		trackTools(conv, raw.Message.Content, raw.Timestamp)
		// isMeta lines are harness-injected (e.g. the local-command caveat).
		text := extractText(raw.Message.Content)
		// "/model" logs its result as command output; a switch after the last
		// reply is the model the session will use next.
		if m := modelSwitch.FindStringSubmatch(AnsiCodes.ReplaceAllString(text, "")); m != nil {
			name := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(m[1], "`", ""), "(default)", ""))
			conv.ActiveModel, conv.Model1M = name, strings.Contains(name, "1M")
		}
		if isChannelEvent(text) {
			conv.Messages = append(conv.Messages, Message{Role: "channel", Text: text, Ts: raw.Timestamp})
			break
		}
		// Messages from other sessions (and from ccs) are harness-marked but
		// are real conversation turns, so keep them.
		if (!raw.IsMeta || PeerMessage.MatchString(text)) && strings.TrimSpace(text) != "" {
			if conv.FirstTimestamp == "" {
				conv.FirstTimestamp = raw.Timestamp
			}
			conv.Messages = append(conv.Messages, Message{Role: "user", Text: text, Ts: raw.Timestamp})
			noteReceived(conv, text)
		}
	case "attachment":
		var a queuedCommand
		if json.Unmarshal(raw.Attachment, &a) != nil || a.Type != "queued_command" || a.CommandMode != "prompt" {
			break
		}
		text := extractText(a.Prompt)
		if isChannelEvent(text) {
			conv.Messages = append(conv.Messages, Message{Role: "channel", Text: text, Ts: raw.Timestamp})
			break
		}
		if (!a.IsMeta || PeerMessage.MatchString(text)) && strings.TrimSpace(text) != "" {
			conv.Messages = append(conv.Messages, Message{Role: "user", Text: text, Ts: raw.Timestamp})
			noteReceived(conv, text)
		}
	case "queue-operation":
		if text := extractText(raw.Content); raw.Operation == "enqueue" && PeerMessage.MatchString(text) {
			if len(conv.PeerQueued) >= 20 {
				conv.PeerQueued = conv.PeerQueued[1:]
			}
			conv.PeerQueued = append(conv.PeerQueued, text)
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
				NoteOutput(conv, raw.Timestamp, int64(u.OutputTokens))
			}
		}
		if text := extractText(raw.Message.Content); strings.TrimSpace(text) != "" {
			conv.Messages = append(conv.Messages, Message{Role: "assistant", Text: text, Ts: raw.Timestamp})
		}
		trackTools(conv, raw.Message.Content, raw.Timestamp) // after the text: a SendMessage follows what it says
	}
	return true
}

// toolCall is a tool_use block: a permission prompt asks about it, and an
// AskUserQuestion carries the question menu, both only on screen in claude.
type toolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// askQuestion is one question of an AskUserQuestion call.
type askQuestion struct {
	Question    string `json:"question"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

// trackTools keeps conv.pending in step with a line's content: a tool call
// becomes pending, its result clears it.
func trackTools(conv *Conversation, content json.RawMessage, ts string) {
	if len(content) == 0 || content[0] != '[' {
		return
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		toolCall
	}
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			noteResult(conv, b.ToolUseID, b.Content)
			if i := slices.Index(conv.GHCalls, b.ToolUseID); i >= 0 {
				conv.GHCalls = slices.Delete(slices.Clone(conv.GHCalls), i, i+1)
				for _, u := range GHOutputPRs(ResultText(b.Content)) {
					addPR(conv, u)
				}
			}
		}
		switch {
		case b.Type == "tool_use":
			call := b.toolCall
			conv.Pending = &call
			noteCall(conv, call, ts)
			if call.Name == "SendMessage" {
				noteSent(conv, call.Input, ts) // drawn as a "To" message, not a tool call
			} else {
				NoteTool(conv, call.Name, toolArg(call.Input), ts)
			}
			if call.Name == "Bash" {
				var in struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(call.Input, &in) == nil && IsSpawn(in.Command) {
					conv.Spawns = append(conv.Spawns, SpawnCmd{ts, SpawnText(in.Command)})
				}
				if GHPRCall(in.Command) {
					conv.GHCalls = append(slices.Clone(conv.GHCalls), call.ID)
				}
			}
		case b.Type == "tool_result" && conv.Pending != nil && b.ToolUseID == conv.Pending.ID:
			conv.Pending = nil
		}
	}
}

// replyOutput is one reply's output tokens and when it was written.
type replyOutput struct {
	at     time.Time
	tokens int64
}

func NoteOutput(conv *Conversation, ts string, tokens int64) {
	if at, err := time.Parse(time.RFC3339Nano, ts); err == nil && tokens > 0 {
		conv.outputs = append(conv.outputs, replyOutput{at, tokens})
	}
}

// OutputSince sums the output tokens of replies written from since on. A reply
// still being written isn't in the transcript, so this runs behind Claude Code's
// own count until it's finished.
func (c Conversation) OutputSince(since time.Time) int64 {
	var n int64
	for i := len(c.outputs) - 1; i >= 0 && !c.outputs[i].at.Before(since); i-- {
		n += c.outputs[i].tokens
	}
	return n
}

// ToolUse is a tool call as the preview draws it, after message at-1; or, with
// took set and no name, the end of a turn, in the same list to keep their order.
type ToolUse struct {
	At        int
	Name, Arg string
	Ts        string
	Took      time.Duration
}

// noteTurnEnd records a finished turn, from the duration the agent recorded:
// first to last message would count the idle hours before a late notification.
func noteTurnEnd(conv *Conversation, ms int64, ts string) {
	if ms > 0 {
		conv.Tools = append(conv.Tools, ToolUse{At: len(conv.Messages), Ts: ts, Took: time.Duration(ms) * time.Millisecond})
	}
}

// maxToolArg caps a tool call's one-line argument (a command keeps its lines
// and is capped at maxMessageRunes).
const maxToolArg = 200

func NoteTool(conv *Conversation, name, arg, ts string) {
	if server, tool, ok := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__"); ok && strings.HasPrefix(name, "mcp__") {
		name = server + " - " + tool + " (MCP)"
	}
	if r := []rune(arg); len(r) > MaxMessageRunes {
		arg = string(r[:MaxMessageRunes]) + fmt.Sprintf("\n… (%d more characters)", len(r)-MaxMessageRunes)
	}
	conv.Tools = append(conv.Tools, ToolUse{At: len(conv.Messages), Name: name, Arg: arg, Ts: ts})
}

// toolArgKeys are the inputs that say what a tool call is for, most telling
// first; a Write's content or an Edit's strings never reach the preview.
var toolArgKeys = []string{"command", "file_path", "notebook_path", "pattern", "query", "url", "description", "skill", "path", "subject", "name"}

// toolArg is what a tool call shows in its brackets: a command verbatim, else
// one line of its most telling input, else its input as compact JSON.
func toolArg(input json.RawMessage) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return ""
	}
	for _, k := range toolArgKeys {
		var s string
		if json.Unmarshal(fields[k], &s) != nil || strings.TrimSpace(s) == "" {
			continue
		}
		if k == "command" {
			return strings.TrimSpace(s)
		}
		return oneLine(s)
	}
	if len(fields) == 0 {
		return ""
	}
	return oneLine(string(input))
}

// oneLine is s on a single line, cut to maxToolArg.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxToolArg {
		return string(r[:maxToolArg]) + "…"
	}
	return s
}

// SpawnCmd is a claude -p command a session ran: when, and its text as
// spawnText leaves it, to match against a spawned session's first prompt.
type SpawnCmd struct{ Ts, Text string }

// IsSpawn: a command that starts a headless or background claude (claude -p,
// claude --bg, cswap run … -p).
func IsSpawn(cmd string) bool {
	return strings.Contains(cmd, "claude") && (strings.Contains(cmd, " -p") || strings.Contains(cmd, "--print") || strings.Contains(cmd, "--bg") || strings.Contains(cmd, "--background")) ||
		strings.Contains(cmd, "cswap run") || strings.Contains(cmd, "codex exec ") || strings.Contains(cmd, "codex e ")
}

// SpawnText is s lowercased, without quotes or backslashes and with runs of
// whitespace as one space, so a prompt matches however the shell quoted it.
func SpawnText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '"' || r == '\'' || r == '\\' {
			return -1
		}
		return r
	}, strings.ToLower(s))
	return strings.Join(strings.Fields(s), " ")
}

// Questions is the pending AskUserQuestion's Questions, if that's what's pending.
func (c Conversation) Questions() []askQuestion {
	if c.Pending == nil || c.Pending.Name != "AskUserQuestion" {
		return nil
	}
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	json.Unmarshal(c.Pending.Input, &in)
	return in.Questions
}

// Headless: a one-shot run nobody talks to (claude -p given a single prompt,
// or codex exec) and nobody named; hidden from the list and ccs mcp unless
// --all (showHeadless). Apps you type in start sessions as cli,
// claude-desktop or sdk-ts, never sdk-cli.
func (c Conversation) Headless() bool {
	if c.Kind == KindCodex {
		return c.CodexExec && !c.IsCustomTitle
	}
	return c.sdkCLI && !c.morePrompts && !c.IsCustomTitle
}

func FinishParse(conv *Conversation) {
	if len(conv.Messages) > 0 {
		conv.LastTimestamp = conv.Messages[len(conv.Messages)-1].Ts
		// An event arriving isn't the session saying anything: it doesn't make
		// a session newer, unread or awake.
		for i := len(conv.Messages) - 1; i >= 0; i-- {
			if conv.Messages[i].Role != "channel" {
				conv.LastTimestamp = conv.Messages[i].Ts
				break
			}
		}
	}
	if conv.Cwd == "" {
		conv.Cwd = "unknown"
	}
}

// parseForScan parses one file for a full scan. A panic skips that file
// rather than killing ccs from a worker goroutine.
func parseForScan(path string, cutoff time.Time, maxSize int64) (conv *Conversation) {
	defer RecoverWorker()
	parse := ParseConversationFile
	if isCodexRollout(path) {
		parse = ParseCodexFile
	}
	conv, err := parse(path, cutoff, maxSize)
	if err != nil {
		return nil
	}
	return conv
}

// WorkerPanicLog is where recoverWorker records panics; WorkerPanicked makes
// the header say so.
var (
	WorkerPanicLog = filepath.Join(os.TempDir(), "ccs-panic.log")
	WorkerPanicked atomic.Bool
)

// RecoverWorker, deferred in goroutines ccs starts itself, stops a panic there
// from killing the process. bubbletea only recovers its own event loop, so an
// unrecovered worker panic exits without restoring the terminal and leaves it
// garbled. The stack is appended to workerPanicLog.
func RecoverWorker() {
	if r := recover(); r != nil {
		RecoverWorkerValue(r)
	}
}

// RecoverWorkerValue records a recovered panic value (see recoverWorker).
func RecoverWorkerValue(r any) {
	WorkerPanicked.Store(true)
	if f, err := os.OpenFile(WorkerPanicLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s ccs %s: panic: %v\n%s\n", time.Now().Format(time.RFC3339), state.Version, r, debug.Stack())
		f.Close()
	}
}

func GetConversations(cutoff time.Time, maxSize int64, excludeDirs []string) ([]Conversation, error) {
	projectsDir := GetProjectsDir()
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
	files = append(files, CodexRollouts()...) // Codex's sessions sit in the same list
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

func FormatTimestamp(ts string) string {
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

// FormatAgo renders how long before now ts was, for the WHEN column:
// "now", "5m ago", "3h ago", "2d ago", "3w ago", "4mo ago", "1y ago". The
// full timestamp stays searchable and shows on each message in the preview.
func FormatAgo(ts string, now time.Time) string {
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

// FormatTokens renders a token count for the 5-wide CTX column: 950, 12k,
// 281k, 1.2M. Blank when unknown (no reply with usage yet).
func FormatTokens(n int) string {
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

// FormatBytes renders a byte count compactly (fits the 6-wide SIZE column).
func FormatBytes(n int64) string {
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

func Truncate(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if ansi.StringWidth(s) <= maxLen { // screen cells, so emoji and CJK count double
		return s
	}
	if maxLen < 3 {
		return ansi.Truncate(s, max(maxLen, 0), "") // no room for the ellipsis
	}
	return ansi.Truncate(s, maxLen, "...")
}

// GetTopic returns the session name (custom/ai title), else first user message, else session ID
func GetTopic(conv Conversation) string {
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

// SearchTextOf joins everything a conversation can be found by.
func SearchTextOf(conv Conversation) string {
	parts := []string{conv.SessionID, conv.Title, conv.Cwd, FormatTimestamp(conv.FirstTimestamp)}
	if conv.Kind != "" {
		parts[0] += " " + conv.Kind // "codex" finds them all
	}
	// Include assistant messages too so a conversation is findable by what
	// Claude said, matching the HITS column and preview.
	for _, msg := range conv.Messages {
		parts = append(parts, msg.Text)
	}
	// Last, so new messages can be appended without rebuilding (parseAppended).
	parts = append(parts, FormatTimestamp(conv.LastTimestamp))
	return strings.Join(parts, SearchSep)
}

// SearchSep separates the parts of a session's search text (its id/title/
// project, each message, the last timestamp), so a multi-word query can
// require every word in the same part, as HITS counts them.
const SearchSep = "\x1e"

// nameWords lowercases a session name and treats - and _ as spaces, so
// "checkout pdf gate" finds the session named checkout-pdf-gate.
func nameWords(s string) string {
	return strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(s))
}

// ContainsAll reports whether every term starts a word somewhere in s (so
// "self" matches "self-revoke" and "selfish" but not "itself"; "revoke"
// matches "revoked"). s and terms are lowercase.
func ContainsAll(s string, terms []string) bool {
	for _, t := range terms {
		if wordStartIndex(s, t, 0) < 0 {
			return false
		}
	}
	return true
}

// wordStartIndex is the first index at or after from where t starts a word
// in s, or -1.
func wordStartIndex(s, t string, from int) int {
	for from <= len(s) {
		i := strings.Index(s[from:], t)
		if i < 0 {
			return -1
		}
		i += from
		if i == 0 || !isWordByte(s, i) {
			return i
		}
		from = i + 1
	}
	return -1
}

// isWordByte reports whether the character just before index i is a letter
// or digit.
func isWordByte(s string, i int) bool {
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// hasPhrase reports whether the terms appear in s next to each other, in
// order, separated only by spaces, dashes or underscores ("self revoke",
// "self-revoke"): a much stronger signal than the words appearing apart.
func hasPhrase(s string, terms []string) bool {
	if len(terms) < 2 {
		return false
	}
	for at := wordStartIndex(s, terms[0], 0); at >= 0; at = wordStartIndex(s, terms[0], at+1) {
		i, ok := at+len(terms[0]), true
		for _, t := range terms[1:] {
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '-' || s[j] == '_') {
				j++
			}
			if j == i || !strings.HasPrefix(s[j:], t) {
				ok = false
				break
			}
			i = j + len(t)
		}
		if ok {
			return true
		}
	}
	return false
}

// MatchRank orders search results: 0 the session's name has every word,
// 1 the words appear together as a phrase, 2 anything else that matches.
func MatchRank(lower, name string, terms []string) int {
	switch {
	case name != "" && ContainsAll(nameWords(name), terms):
		return 0
	case hasPhrase(lower, terms):
		return 1
	}
	return 2
}

// QueryMatches is the one search rule, shared by the list, HITS, the preview
// and ccs mcp: every word of the query appears in one part of the search text
// (the same message, or the id/title/project), or every word appears in the
// session's name with its dashes read as spaces.
func QueryMatches(lower, name string, terms []string) bool {
	if name != "" && ContainsAll(nameWords(name), terms) {
		return true
	}
	if !ContainsAll(lower, terms) {
		return false // fast reject: some word appears nowhere
	}
	if len(terms) < 2 {
		return true
	}
	for rest := lower; ; {
		part, after, more := strings.Cut(rest, SearchSep)
		if ContainsAll(part, terms) {
			return true
		}
		if !more {
			return false
		}
		rest = after
	}
}

// PRURL is a github.com PR address; anything else in a pr-link is ignored,
// since its parts go into a GraphQL query.
var PRURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)$`)

var (
	prInOutput  = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+\b`)
	prMergedOut = regexp.MustCompile(`(?:Merged pull request|Pull request) ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)`)
)

// addPR adds u to conv's PRs once, if it's a PR address.
func addPR(conv *Conversation, u string) {
	if PRURL.MatchString(u) && !slices.Contains(conv.PRs, u) {
		conv.PRs = append(slices.Clone(conv.PRs), u)
	}
}

// GHPRCall: a Bash command whose output names the PRs it opened or merged.
func GHPRCall(cmd string) bool {
	return strings.Contains(cmd, "gh pr create") || strings.Contains(cmd, "gh pr merge")
}

// GHOutputPRs is the PRs a ghPRCall printed: create prints the new PR's
// address, merge "Merged pull request owner/repo#N".
func GHOutputPRs(out string) []string {
	urls := prInOutput.FindAllString(out, -1)
	for _, m := range prMergedOut.FindAllStringSubmatch(out, -1) {
		urls = append(urls, "https://github.com/"+m[1]+"/pull/"+m[2])
	}
	return urls
}

// KindCodex marks a Codex session; Claude's leave Kind empty.
const KindCodex = "codex"

// AgentGlyph marks every row with the agent behind it, one cell wide: ✻,
// the asterisk Claude Code draws, in Claude's orange; ⬡ for Codex.
func AgentGlyph(conv Conversation) (glyph, colour string) {
	if conv.Kind == KindCodex {
		return "⬡", "37"
	}
	return "✻", "38;5;173"
}

// isCodexRollout: path is a Codex transcript, not a Claude one.
func isCodexRollout(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
}

// CodexRecord is one line of a rollout, decoded only as far as ccs needs.
type CodexRecord struct {
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
	// DurationMs is a task_complete's: how long the turn took.
	DurationMs int64 `json:"duration_ms"`
	Content    []struct {
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

// Codex sessions in the list: each transcript (a "rollout" under
// $CODEX_HOME/sessions/YYYY/MM/DD) parses into a Conversation with Kind
// "codex", sorted, searched and previewed with Claude's. A Codex rollout is
// small next to a Claude transcript and isn't appended to in place by a
// resume, so a changed one is simply parsed again (ParseCache keeps the
// unchanged ones).

// ParseCodexFile parses one rollout, with the cutoff, size limit and cache
// parseConversationFile applies to Claude's transcripts.
func ParseCodexFile(path string, cutoff time.Time, maxSize int64) (*Conversation, error) {
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
	conv, err := parseCodexUncached(path, info)
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

func parseCodexUncached(path string, info os.FileInfo) (*Conversation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	conv := &Conversation{Kind: KindCodex, FilePath: path, Size: info.Size()}
	var meta codexMeta
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var rec CodexRecord
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
			conv.CodexExec = source == "exec"
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
			NoteOutput(conv, ts, l.Output)
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
			conv.CodexBusy = p.Type == "task_started"
			if p.Type == "task_complete" {
				noteTurnEnd(conv, p.DurationMs, ts)
			}
		case rec.Type == "event_msg" && p.Type == "item_completed" && p.Item != nil:
			if p.Item.Type == "CommandExecution" {
				if cmd := codexCommand(p.Item.Command); GHPRCall(cmd) {
					for _, u := range GHOutputPRs(p.Item.Output) {
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
	if name := CodexLaunchName(meta); name != "" {
		conv.Title, conv.IsCustomTitle = name, true
	} else {
		conv.Title = codexThreadNames()[conv.SessionID]
	}
	conv.ParsedBytes = conv.Size
	FinishParse(conv)
	conv.SearchText = SearchTextOf(*conv)
	conv.SearchLower = strings.ToLower(conv.SearchText)
	return conv, nil
}

// noteCodexTool records an item that is a tool call, named as Claude's are.
func noteCodexTool(conv *Conversation, p codexPayload, ts string) {
	switch it := p.Item; it.Type {
	case "CommandExecution":
		NoteTool(conv, "Shell", strings.TrimSpace(codexShellText(it.Command)), ts)
	case "McpToolCall":
		NoteTool(conv, "mcp__"+it.Server+"__"+it.Tool, toolArg(it.Arguments), ts)
	case "FileChange":
		NoteTool(conv, "Edit", oneLine(strings.Join(slices.Sorted(maps.Keys(it.Changes)), " ")), ts)
	case "Extension":
		if it.Kind == "web.search" {
			NoteTool(conv, "WebSearch", oneLine(it.Query), ts)
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
	path := filepath.Join(CodexHome(), "session_index.jsonl")
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

// CodexLiveTTL is how long one look at the running Codex processes is
// trusted: the live tick and every liveness check share it.
const CodexLiveTTL = 2 * time.Second

// CodexProcCwd is a process's working folder. Stubbed in tests.
var CodexProcCwd = func(pid int) string {
	out, err := proc.RunBounded(2*time.Second, nil, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
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

// CodexResumeArgs is the codex command line that opens conv again: resume
// it, or fork it into a new thread, with ccs launch's sandbox (network on,
// the user's approval mode, ccs's state writable).
func CodexResumeArgs(bin string, conv Conversation, fork bool) []string {
	sub := "resume"
	if fork {
		sub = "fork"
	}
	return append([]string{bin, sub, conv.SessionID}, CodexSandboxArgs()...)
}

// CodexSandboxArgs is how ccs starts Codex: the user's own approval mode;
// network on (Codex's sandbox has none by default, so git and gh fail);
// ccs's state writable, for ccs claim; then any launch folders.
func CodexSandboxArgs(dirs ...string) []string {
	args := []string{"-s", "workspace-write", "-a", "on-request", "-c", "sandbox_workspace_write.network_access=true"}
	for _, d := range dirs {
		args = append(args, "--add-dir", d)
	}
	return append(args, "--add-dir", filepath.Dir(state.SettingsPath))
}

// BgTask is a background task a session started: a Bash command or a
// Monitor. Subagents, async or teammates, are counted apart from their own
// meta.json (subagentsOf), so they never show twice.
type BgTask struct {
	ID, Kind, Desc, Started string
	Status                  string    // "" while running; completed, failed or stopped from its notification
	Expires                 time.Time // a monitor's own end, if it never reports one
}

var (
	taskEnd     = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>.*?<status>([^<]+)</status>`)
	monitorLife = regexp.MustCompile(`expires in (\d+)m`)
)

// noteCall remembers a call that may start a background task; its result decides.
func noteCall(conv *Conversation, call toolCall, ts string) {
	if call.Name != "Bash" && call.Name != "Monitor" {
		return
	}
	var in struct{ Description, Command string }
	json.Unmarshal(call.Input, &in)
	desc := in.Description
	if desc == "" {
		desc = in.Command
	}
	conv.calls = append(slices.Clone(conv.calls), BgTask{ID: call.ID, Kind: call.Name, Desc: strings.Join(strings.Fields(desc), " "), Started: ts})
}

// noteResult makes a call a task when its result says it went to the background.
func noteResult(conv *Conversation, id string, content json.RawMessage) {
	i := slices.IndexFunc(conv.calls, func(t BgTask) bool { return t.ID == id })
	if i < 0 {
		return
	}
	t := conv.calls[i]
	conv.calls = slices.Delete(slices.Clone(conv.calls), i, i+1)
	// Claude Code starts the result with it; anywhere else it's only output
	// that mentions it.
	s := ResultText(content)
	switch {
	case t.Kind == "Bash" && strings.HasPrefix(s, "Command running in background"):
		t.Kind = "bash"
	case t.Kind == "Monitor" && strings.HasPrefix(s, "Monitor started"):
		t.Kind = "monitor"
		if m := monitorLife.FindStringSubmatch(s); m != nil {
			if start, err := time.Parse(time.RFC3339, t.Started); err == nil {
				mins, _ := strconv.Atoi(m[1])
				t.Expires = start.Add(time.Duration(mins) * time.Minute)
			}
		}
	default:
		return
	}
	conv.Tasks = append(slices.Clone(conv.Tasks), t)
}

// ResultText is a tool result's text: a string, or its first text block.
func ResultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct{ Text string }
	if json.Unmarshal(content, &blocks) == nil && len(blocks) > 0 {
		return blocks[0].Text
	}
	return ""
}

// noteEnds marks the tasks a line's <task-notification> reports as ended.
func noteEnds(conv *Conversation, line []byte) {
	if len(conv.Tasks) == 0 || !bytes.Contains(line, []byte("<task-notification>")) {
		return
	}
	for _, m := range taskEnd.FindAllSubmatch(line, -1) {
		if i := slices.IndexFunc(conv.Tasks, func(t BgTask) bool { return t.ID == string(m[1]) }); i >= 0 && conv.Tasks[i].Status == "" {
			conv.Tasks = slices.Clone(conv.Tasks)
			conv.Tasks[i].Status = string(m[2])
		}
	}
}

// TaskColour is the tasks' own colour, a light blue no other mark uses
// (PRs are green, amber, red and purple; HITS cyan; busy amber), and
// SubagentColour, pink, theirs.
const (
	TaskColour     = "38;5;75"
	SubagentColour = "38;5;211"
)

// MaxTaskLines is how many running tasks the preview header lists.
const MaxTaskLines = 5

// Subagent is one of a session's subagents (Agent tool, teammates
// included), from the meta.json Claude Code writes beside its transcript in
// <session>/subagents/.
type Subagent struct {
	Name      string
	Started   time.Time
	LastWrite time.Time // its transcript's: a working subagent writes every few seconds
	Finished  bool
}

// SubagentIdle is how long without a transcript write before an unfinished
// subagent counts as idle (a teammate waiting to be given work or shut
// down), not running.
const SubagentIdle = 10 * time.Minute

// subagentResumed is how long after its meta.json says finished a
// subagent's transcript may still be written before it counts as resumed.
const subagentResumed = 5 * time.Second

// subagentsTTL is how long subagentsOf trusts what it last read.
const subagentsTTL = 2 * time.Second

var subagentCache = struct {
	sync.Mutex
	m map[string]subagentsRead
}{m: map[string]subagentsRead{}}

type subagentsRead struct {
	at   time.Time
	subs []Subagent
}

// SubagentsOf is conv's subagents, running first then newest, read at most
// every subagentsTTL per session (the list asks on every frame).
func SubagentsOf(conv Conversation) []Subagent {
	dir := filepath.Join(strings.TrimSuffix(conv.FilePath, ".jsonl"), "subagents")
	subagentCache.Lock()
	defer subagentCache.Unlock()
	if r, ok := subagentCache.m[dir]; ok && time.Since(r.at) < subagentsTTL {
		return r.subs
	}
	metas, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
	var subs []Subagent
	for _, path := range metas {
		var meta struct {
			Name, Description string
			Finished          bool
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &meta) != nil {
			continue
		}
		s := Subagent{Name: meta.Name, Finished: meta.Finished}
		if s.Name == "" {
			s.Name = meta.Description
		}
		transcript := strings.TrimSuffix(path, ".meta.json") + ".jsonl"
		s.Started = firstTimestamp(transcript)
		if info, err := os.Stat(transcript); err == nil {
			s.LastWrite = info.ModTime()
			// Resumed (SendMessage to a finished subagent): Claude Code leaves
			// finished set, but the transcript goes on past the meta.json write.
			if mi, err := os.Stat(path); err == nil && s.Finished && s.LastWrite.Sub(mi.ModTime()) > subagentResumed {
				s.Finished = false
			}
		}
		subs = append(subs, s)
	}
	slices.SortStableFunc(subs, func(a, b Subagent) int {
		if a.Finished != b.Finished {
			if a.Finished {
				return 1
			}
			return -1
		}
		return b.Started.Compare(a.Started)
	})
	subagentCache.m[dir] = subagentsRead{time.Now(), subs}
	return subs
}

// firstTimestamp is when a transcript began: the first of its first lines
// with a timestamp, else when the file was last written.
func firstTimestamp(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for range 20 {
		line, err := r.ReadBytes('\n')
		var rec struct{ Timestamp time.Time }
		if json.Unmarshal(line, &rec) == nil && !rec.Timestamp.IsZero() {
			return rec.Timestamp
		}
		if err != nil {
			break
		}
	}
	if info, err := f.Stat(); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// CcsNote is the leading note on a message ccs sends for the user (see
// ccsSender).
const CcsNote = "[Typed by the user in ccs, their session browser. Not from another Claude session: reply here as you would to any message from the user; there is no session to send a reply to.]\n\n"

// PeerMessage matches a message another session (or ccs) sent through the
// message socket; peerParts pulls out who it's from and the text.
// Claude Code may add its own paragraph after the closing tag (a note that
// peers can't grant permissions), so anything after it is allowed.
var PeerMessage = regexp.MustCompile(`(?s)^(?:Another Claude session sent a message:\n)?<cross-session-message([^>]*)>\n(.*?)\n</cross-session-message>(?:\s.*)?$`)

var PeerFrom = regexp.MustCompile(`from-name="([^"]*)"`)

// PeerRef is one of a session's messages to or from another session: which
// message, the start of its text (to find the other end's copy), and the
// address it went to or came from.
type PeerRef struct {
	Idx  int    // in conv.Messages
	Key  string // peerKey of its text
	Sent bool
	Addr string // sent: SendMessage's to; received: the sender's from="uds:…"
	Name string // received: the sender's from-name
}

// peerKey is the start of a message's text, whitespace evened out, so the
// sender's copy and the recipient's match.
func peerKey(text string) string {
	k := strings.Join(strings.Fields(text), " ")
	if r := []rune(k); len(r) > 120 {
		k = string(r[:120])
	}
	return k
}

// peerAddr is a cross-session message's from="…" attribute.
var peerAddr = regexp.MustCompile(`\bfrom="([^"]*)"`)

// noteSent records a SendMessage call as a "sent" message.
func noteSent(conv *Conversation, input json.RawMessage, ts string) {
	var in struct {
		To      string `json:"to"`
		Summary string `json:"summary"`
		Message any    `json:"message"`
	}
	if json.Unmarshal(input, &in) != nil || in.To == "" {
		return
	}
	text, ok := in.Message.(string)
	if !ok { // a structured message (shutdown, plan approval): its JSON
		b, _ := json.Marshal(in.Message)
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	conv.Messages = append(conv.Messages, Message{Role: "sent", Text: text, Ts: ts, To: in.To, Note: in.Summary})
	conv.Peers = append(conv.Peers, PeerRef{Idx: len(conv.Messages) - 1, Key: peerKey(text), Sent: true, Addr: in.To})
}

// noteReceived records the message just appended when another session sent it.
func noteReceived(conv *Conversation, text string) {
	if !strings.Contains(text[:min(len(text), 200)], "<cross-session-message") {
		return
	}
	m := PeerMessage.FindStringSubmatch(text)
	if m == nil {
		return
	}
	ref := PeerRef{Idx: len(conv.Messages) - 1, Key: peerKey(strings.TrimPrefix(m[2], CcsNote))}
	if f := peerAddr.FindStringSubmatch(m[1]); f != nil {
		ref.Addr = f[1]
	}
	if f := PeerFrom.FindStringSubmatch(m[1]); f != nil {
		ref.Name = f[1]
	}
	conv.Peers = append(conv.Peers, ref)
}

var ChannelMessage = regexp.MustCompile(`(?s)^<channel\s([^>]*)>\n?(.*?)\n?</channel>\s*$`)

func isChannelEvent(text string) bool {
	return strings.HasPrefix(text, "<channel ") && ChannelMessage.MatchString(text)
}

// MaxMessageRunes caps one message in the preview; beyond it the rest is
// summarised, so a pasted log can't swamp the conversation.
const MaxMessageRunes = 20000

// CodexHome is Codex's data folder. Stubbed in tests.
var CodexHome = func() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// CodexThreadID is a Codex thread id: a uuid.
var CodexThreadID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// codexMeta is the first line of a Codex transcript (its session_meta).
type codexMeta struct {
	ID        string          `json:"id"`
	Cwd       string          `json:"cwd"`
	Roots     []string        `json:"runtime_workspace_roots"`
	Source    json.RawMessage `json:"source"`
	Timestamp string          `json:"timestamp"`
}

// CodexRollouts is Codex's transcripts, newest first by name (each starts
// rollout-<start time>-<thread id>).
func CodexRollouts() []string {
	paths, _ := filepath.Glob(filepath.Join(CodexHome(), "sessions", "*", "*", "*", "rollout-*.jsonl"))
	for i, j := 0, len(paths)-1; i < j; i, j = i+1, j-1 {
		paths[i], paths[j] = paths[j], paths[i]
	}
	return paths
}

// CodexRollout is thread's transcript, "" when there's none.
func CodexRollout(thread string) string {
	if !CodexThreadID.MatchString(thread) {
		return ""
	}
	paths, _ := filepath.Glob(filepath.Join(CodexHome(), "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if len(paths) == 0 {
		return ""
	}
	return paths[len(paths)-1]
}

// ReadCodexMeta reads a transcript's session_meta.
func ReadCodexMeta(path string) (codexMeta, bool) {
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

// CodexLaunchName is the name ccs launch gave a thread: its launch folder is
// one of the thread's workspace roots. "" for a thread ccs didn't launch.
func CodexLaunchName(m codexMeta) string {
	base := state.LaunchDir() + string(filepath.Separator)
	for _, r := range m.Roots {
		if name, ok := strings.CutPrefix(r, base); ok && state.SessionName.MatchString(name) {
			return name
		}
	}
	return ""
}

// PruneOpts selects which categories of redundant data to remove. Conversation
// (user/assistant) messages are never touched.
type PruneOpts struct {
	DropSnapshots    bool // drop file-history-snapshot lines (rewind/checkpoint backups)
	StripToolResults bool // remove the toolUseResult field (a copy of the tool_result already in message.content)
}

type pruneStats struct {
	BytesIn          int64
	BytesOut         int64
	droppedSnapshots int
	strippedResults  int
	convLinesIn      int // user/assistant lines seen
	convLinesOut     int // ... and kept (invariant: must equal convLinesIn)
}

// pruneLine applies the transforms to one JSONL line. It returns the output
// bytes (nil = drop the line), the line's "type", and whether it was
// dropped/stripped. Unparseable lines pass through verbatim.
func pruneLine(line []byte, opts PruneOpts) (out []byte, typ string, dropped, stripped bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		return line, "", false, false
	}
	if raw, ok := obj["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}
	if opts.DropSnapshots && typ == "file-history-snapshot" {
		return nil, typ, true, false
	}
	if opts.StripToolResults {
		if _, ok := obj["toolUseResult"]; ok {
			delete(obj, "toolUseResult")
			b, err := json.Marshal(obj)
			if err != nil {
				return line, typ, false, false // keep original on marshal error
			}
			return b, typ, false, true
		}
	}
	return line, typ, false, false
}

// pruneStream reads JSONL from r and writes the pruned version to w (w may be
// nil to only measure). It never drops or modifies user/assistant lines.
func pruneStream(r io.Reader, w io.Writer, opts PruneOpts) (pruneStats, error) {
	var st pruneStats
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		st.BytesIn += int64(len(line)) + 1
		out, typ, dropped, stripped := pruneLine(line, opts)
		isConv := typ == "user" || typ == "assistant"
		if isConv {
			st.convLinesIn++
		}
		if dropped {
			st.droppedSnapshots++
			continue
		}
		if stripped {
			st.strippedResults++
		}
		if isConv {
			st.convLinesOut++
		}
		if w != nil {
			if _, err := w.Write(out); err != nil {
				return st, err
			}
			if _, err := w.Write([]byte{'\n'}); err != nil {
				return st, err
			}
		}
		st.BytesOut += int64(len(out)) + 1
	}
	return st, scanner.Err()
}

// PruneFile prunes one conversation file. With write=true it streams to
// <path>.pruned and atomically replaces path, aborting (no replace) if the
// conversation line count would change. With write=false it only measures.
func PruneFile(path string, write bool, opts PruneOpts) (pruneStats, error) {
	in, err := os.Open(path)
	if err != nil {
		return pruneStats{}, err
	}
	defer in.Close()

	if !write {
		return pruneStream(in, nil, opts)
	}

	tmpPath := path + ".pruned"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return pruneStats{}, err
	}
	bw := bufio.NewWriter(tmp)
	st, err := pruneStream(in, bw, opts)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && st.convLinesIn != st.convLinesOut {
		err = fmt.Errorf("integrity check failed: %d conversation lines in, %d out", st.convLinesIn, st.convLinesOut)
	}
	if err != nil {
		os.Remove(tmpPath)
		return st, err
	}
	return st, os.Rename(tmpPath, path)
}

// findPrunableFiles returns .jsonl files at or above minSize, largest first.
func findPrunableFiles(minSize int64) ([]string, error) {
	type fileSize struct {
		path string
		size int64
	}
	var found []fileSize
	err := filepath.Walk(GetProjectsDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") && info.Size() >= minSize {
			found = append(found, fileSize{path, info.Size()})
		}
		return nil
	})
	sort.Slice(found, func(i, j int) bool { return found[i].size > found[j].size })
	paths := make([]string, len(found))
	for i, f := range found {
		paths[i] = f.path
	}
	return paths, err
}

// shortPath shows the project dir + filename for readable reporting.
func shortPath(p string) string {
	return filepath.Join(filepath.Base(filepath.Dir(p)), filepath.Base(p))
}

func RunPrune(args []string) {
	apply, yes := false, false // dry run by default; --apply to actually rewrite
	minSizeMB := int64(50)
	opts := PruneOpts{DropSnapshots: true, StripToolResults: true}
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			printPruneHelp()
			return
		case a == "--apply":
			apply = true
		case a == "--dry-run":
			apply = false // explicit; this is already the default
		case a == "-y" || a == "--yes":
			yes = true
		case a == "--no-snapshots":
			opts.DropSnapshots = false
		case a == "--no-tool-results":
			opts.StripToolResults = false
		case strings.HasPrefix(a, "--min-size="):
			fmt.Sscanf(strings.TrimPrefix(a, "--min-size="), "%d", &minSizeMB)
		default:
			fmt.Fprintf(os.Stderr, "unknown prune flag: %s (try ccs prune --help)\n", a)
			os.Exit(2)
		}
	}
	if !opts.DropSnapshots && !opts.StripToolResults {
		fmt.Fprintln(os.Stderr, "nothing to prune: both categories disabled")
		os.Exit(2)
	}

	files, err := findPrunableFiles(minSizeMB * 1024 * 1024)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error scanning conversations: %v\n", err)
		os.Exit(1)
	}
	if len(files) == 0 {
		fmt.Printf("No conversations >= %dMB to prune.\n", minSizeMB)
		return
	}

	report := func(st pruneStats, path string) {
		saved := st.BytesIn - st.BytesOut
		fmt.Printf("  %-46s %8s -> %8s  (-%s)\n", shortPath(path), FormatBytes(st.BytesIn), FormatBytes(st.BytesOut), FormatBytes(saved))
	}

	if !apply {
		fmt.Printf("Dry run (no changes). Conversations >= %dMB:\n\n", minSizeMB)
		var in, out int64
		for _, f := range files {
			st, err := PruneFile(f, false, opts)
			if err != nil {
				fmt.Printf("  %-46s error: %v\n", shortPath(f), err)
				continue
			}
			report(st, f)
			in += st.BytesIn
			out += st.BytesOut
		}
		fmt.Printf("\nWould reclaim %s across %d files. Re-run with --apply to prune.\n", FormatBytes(in-out), len(files))
		return
	}

	if !yes {
		var total int64
		for _, f := range files {
			if info, e := os.Stat(f); e == nil {
				total += info.Size()
			}
		}
		fmt.Printf("Prune %d conversations (%s)? Rewrites them in place, removing duplicate tool\nresults and snapshot backups - dialogue is preserved. [y/N] ", len(files), FormatBytes(total))
		var resp string
		fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" {
			fmt.Println("Aborted.")
			return
		}
	}

	var in, out int64
	for _, f := range files {
		st, err := PruneFile(f, true, opts)
		if err != nil {
			fmt.Printf("  %-46s FAILED: %v\n", shortPath(f), err)
			continue
		}
		report(st, f)
		in += st.BytesIn
		out += st.BytesOut
	}
	fmt.Printf("\nReclaimed %s across %d files.\n", FormatBytes(in-out), len(files))
}

func printPruneHelp() {
	fmt.Print(`ccs prune - shrink conversation files by removing redundant data

Removes data that duplicates content kept elsewhere, so pruned conversations
still resume with full dialogue:
  - toolUseResult fields (a copy of the tool_result already in message.content)
  - file-history-snapshot lines (rewind/checkpoint backups; pruning loses
    rewind history, not the conversation)

User and assistant messages are never modified. Each file is rewritten only if
its conversation line count is unchanged.

By default this is a dry run that only previews savings - pass --apply to
actually rewrite the files.

Usage: ccs prune [flags]

Flags:
  --apply              Actually rewrite files (default is a dry-run preview)
  --min-size=N         Only consider files >= N MB (default: 50)
  --no-tool-results    Keep toolUseResult fields
  --no-snapshots       Keep file-history-snapshot lines
  -y, --yes            Skip the confirmation prompt (with --apply)
  -h, --help           Show this help

Examples:
  ccs prune                        Preview savings across files >= 50MB
  ccs prune --apply                Prune files >= 50MB (after confirmation)
  ccs prune --apply --min-size=200 Prune files >= 200MB
  ccs prune --apply --no-tool-results -y   Only drop snapshot backups, no prompt
`)
}

// CleanText drops terminal control codes that transcripts can carry (coloured
// command output, carriage returns): printed raw they'd move the cursor or
// leave stray "[" on screen.
func CleanText(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\t' || r == 0x7f }) {
		return s
	}
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

var (
	harnessTag  = regexp.MustCompile(`^<([a-z][a-z0-9-]*)[ >]`)
	harnessSumm = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	anyTag      = regexp.MustCompile(`</?[a-z][a-z0-9-]*[^>]*>`)
)

// HarnessNote recognises a user-role message that the harness injected
// (starts with a tag like <task-notification> or <system-reminder>) and
// returns its tag and a one-line summary: its <summary> if it has one, else
// its text with the tags removed.
func HarnessNote(text string) (tag, summary string, ok bool) {
	m := harnessTag.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	if sm := harnessSumm.FindStringSubmatch(text); sm != nil {
		summary = sm[1]
	} else {
		summary = anyTag.ReplaceAllString(text, " ")
	}
	return m[1], strings.Join(strings.Fields(summary), " "), true
}

// channelEvent is one event a channel plugin pushed into a session:
// <channel source="plugin:name:server" event="…" …>, then a line saying what
// happened, usually its address, then any body (a comment, a review).
type channelEvent struct {
	source, summary, Link, Detail string
}

var (
	channelAttr = regexp.MustCompile(`\b(source|event)="([^"]*)"`)
	bareURL     = regexp.MustCompile(`^https?://\S+$`)
)

func ChannelParts(text string) (channelEvent, bool) {
	m := ChannelMessage.FindStringSubmatch(text)
	if m == nil {
		return channelEvent{}, false
	}
	var ev channelEvent
	event := ""
	for _, a := range channelAttr.FindAllStringSubmatch(m[1], -1) {
		if a[1] == "source" {
			ev.source = a[2][strings.LastIndex(a[2], ":")+1:] // plugin:agentic-utils-github:pr-events
		} else {
			event = a[2]
		}
	}
	lines := strings.Split(strings.TrimSpace(m[2]), "\n")
	ev.summary = strings.TrimPrefix(lines[0], ev.source+": ")
	lines = lines[1:]
	if len(lines) > 0 && bareURL.MatchString(strings.TrimSpace(lines[0])) {
		ev.Link = strings.TrimSpace(lines[0])
		lines = lines[1:]
	}
	ev.Detail = strings.TrimSpace(strings.Join(lines, "\n"))
	if ev.summary == "" {
		ev.summary = event
	}
	return ev, true
}

// Line is the event as Claude Code's terminal shows it.
func (ev channelEvent) Line() string {
	return "← " + ev.source + ": " + ev.summary
}

// ImageNoteStart begins the note listing a message's pasted images.
const ImageNoteStart = "[Pasted images, read each file to see it: "

// imageNote matches that note at the end of a message.
var imageNote = regexp.MustCompile(`(?s)\s*` + regexp.QuoteMeta(ImageNoteStart) + `.*\]\s*$`)

// StripImageNote is a sent message as it was typed, without the paths behind
// its placeholders.
func StripImageNote(text string) string {
	if !strings.Contains(text, ImageNoteStart) {
		return text
	}
	return imageNote.ReplaceAllString(text, "")
}

func PeerParts(text string) (from, body string, ok bool) {
	m := PeerMessage.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	from = "another session"
	if f := PeerFrom.FindStringSubmatch(m[1]); f != nil {
		from = f[1]
	}
	return from, StripImageNote(strings.TrimPrefix(m[2], CcsNote)), true
}

// ShortAddr is a SendMessage address for show: "session 123" for a socket,
// an agent's id as it is.
func ShortAddr(a string) string {
	if strings.HasPrefix(a, "uds:") {
		return "session " + strings.TrimSuffix(filepath.Base(a), ".sock")
	}
	return a
}

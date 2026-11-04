package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPadRight(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		length   int
		expected string
	}{
		{"short string", "hello", 10, "hello     "},
		{"exact length", "hello", 5, "hello"},
		{"needs truncation", "hello world", 8, "hello wo"},
		{"wide chars pad by screen width", "世界", 5, "世界 "},
		{"wide chars cut by screen width", "世界世界世界", 4, "世界"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := padRight(tt.input, tt.length)
			if result != tt.expected {
				t.Errorf("padRight(%q, %d) = %q, want %q", tt.input, tt.length, result, tt.expected)
			}
		})
	}
}

func TestRenderingNeverCorruptsUTF8(t *testing.T) {
	// These would emit invalid UTF-8 (mid-rune byte cuts) under byte-based slicing.
	multibyte := []string{"世界世界世界", "😀😀😀😀", "café résumé naïve", "İstanbul Kelvin K"}
	for _, s := range multibyte {
		for n := 0; n <= len([]rune(s))+2; n++ {
			if got := truncate(s, n); !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) = %q: invalid UTF-8", s, n, got)
			}
			if got := padRight(s, n); !utf8.ValidString(got) {
				t.Errorf("padRight(%q, %d) = %q: invalid UTF-8", s, n, got)
			}
		}
		for _, q := range []string{"世", "😀", "é", "i", "k"} {
			if got := highlight(s, q); !utf8.ValidString(got) {
				t.Errorf("highlight(%q, %q) = %q: invalid UTF-8", s, q, got)
			}
		}
	}
}

func TestHighlightMatchesMultibyte(t *testing.T) {
	got := highlight("héllo wörld héllo", "héllo")
	if n := strings.Count(got, "\033[43;30m"); n != 2 {
		t.Errorf("expected 2 highlights of multibyte query, got %d in %q", n, got)
	}
	// The visible text must be preserved exactly once ANSI codes are stripped.
	stripped := strings.NewReplacer("\033[43;30m", "", "\033[0m", "", "\033[49;39m", "").Replace(got)
	if stripped != "héllo wörld héllo" {
		t.Errorf("highlight altered visible text: %q", stripped)
	}
}

func TestHighlight(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		query    string
		contains string
	}{
		{"empty query", "hello world", "", "hello world"},
		{"matching query", "hello world", "world", "world"},
		{"case insensitive", "Hello World", "world", "World"},
		{"no match", "hello world", "foo", "hello world"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := highlight(tt.text, tt.query)
			if !strings.Contains(result, tt.contains) {
				t.Errorf("highlight(%q, %q) = %q, want to contain %q", tt.text, tt.query, result, tt.contains)
			}
		})
	}
}

func TestPreviewLinesCachedUntilSelectionOrQueryChanges(t *testing.T) {
	items := []listItem{
		{conv: Conversation{SessionID: "s1", Messages: []Message{{Role: "user", Text: "alpha"}}}, searchText: "alpha"},
		{conv: Conversation{SessionID: "s2", Messages: []Message{{Role: "user", Text: "beta"}}}, searchText: "beta"},
	}
	m := initialModel(items, "", nil)

	_ = m.previewLines() // build + cache for s1
	// Poison the cache; a cached call must return it without rebuilding.
	m.preview.lines = []string{"CACHED"}
	if got := m.previewLines(); len(got) != 1 || got[0] != "CACHED" {
		t.Errorf("expected cached value, got %v", got)
	}

	// Changing the query invalidates the cache → rebuild (not the poison).
	m.textInput.SetValue("alpha")
	if got := m.previewLines(); len(got) == 1 && got[0] == "CACHED" {
		t.Error("changing query should rebuild the preview, not return stale cache")
	}

	// Moving the cursor to a different conversation also rebuilds.
	m.textInput.SetValue("")
	_ = m.previewLines()
	m.preview.lines = []string{"CACHED"}
	m.cursor = 1
	if got := m.previewLines(); len(got) == 1 && got[0] == "CACHED" {
		t.Error("moving the cursor should rebuild the preview")
	}
}

func TestHitCountCachedPerQuery(t *testing.T) {
	conv := Conversation{SessionID: "s1", Messages: []Message{
		{Role: "user", Text: "alpha beta"},
		{Role: "assistant", Text: "beta gamma"},
	}}
	item := listItem{conv: conv}
	m := initialModel([]listItem{item}, "beta", nil)

	if got := m.hitCount(item); got != 2 {
		t.Fatalf("hitCount = %d, want 2", got)
	}
	// Poison the cache; a cached call must return it (no rescan).
	m.hits.byID["s1"] = 99
	if got := m.hitCount(item); got != 99 {
		t.Errorf("expected cached value 99, got %d", got)
	}
	// Changing the query invalidates the cache → recompute.
	m.textInput.SetValue("gamma")
	if got := m.hitCount(item); got != 1 {
		t.Errorf("query change should recompute hits: got %d, want 1", got)
	}
}

func TestRenderPreview(t *testing.T) {
	conv := Conversation{
		SessionID: "test-123",
		Cwd:       "/test/project",
		Messages: []Message{
			{Role: "user", Text: "first message", Ts: "2024-01-15T10:00:00Z"},
			{Role: "assistant", Text: "response", Ts: "2024-01-15T10:01:00Z"},
			{Role: "user", Text: "second message with query term", Ts: "2024-01-15T10:02:00Z"},
		},
	}

	item := listItem{conv: conv, searchText: "test"}
	m := initialModel([]listItem{item}, "query", nil)

	preview := m.renderPreview(item, 20)

	// Check preview contains key elements
	if !strings.Contains(preview, "Project:") {
		t.Error("preview should contain 'Project:' header")
	}
	if !strings.Contains(preview, "Session:") {
		t.Error("preview should contain 'Session:' header")
	}
	if !strings.Contains(preview, "/test/project") {
		t.Error("preview should contain project path")
	}
	if !strings.Contains(preview, "test-123") {
		t.Error("preview should contain session ID")
	}
}

func TestRenderPreviewLongMultibyteMessageStaysValidUTF8(t *testing.T) {
	// A message longer than the 500-char preview cap, all multibyte: byte
	// slicing would cut mid-rune at byte 500 and emit invalid UTF-8.
	conv := Conversation{
		SessionID: "s1",
		Cwd:       "/p",
		Messages:  []Message{{Role: "user", Text: strings.Repeat("世", 800), Ts: "2024-01-15T10:00:00Z"}},
	}
	item := listItem{conv: conv}
	m := initialModel([]listItem{item}, "", nil)

	if got := m.renderPreview(item, 40); !utf8.ValidString(got) {
		t.Error("preview of a long multibyte message produced invalid UTF-8")
	}
}
func TestPreviewScrollClampedToContent(t *testing.T) {
	conv := Conversation{SessionID: "s1", Messages: []Message{
		{Role: "user", Text: "only message", Ts: "2024-01-15T10:00:00Z"},
	}}
	m := initialModel([]listItem{{conv: conv}}, "", nil)

	maxScroll := m.maxPreviewScroll()

	// Hammer pgup (back through history) far past the start; previewScroll
	// must never exceed max.
	for i := 0; i < 100; i++ {
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		m = result.(model)
		if m.previewScroll > maxScroll {
			t.Fatalf("previewScroll %d exceeded max %d after pgup", m.previewScroll, maxScroll)
		}
	}
	if m.previewScroll != maxScroll {
		t.Errorf("previewScroll should settle at max %d, got %d", maxScroll, m.previewScroll)
	}

	// A single pgdown from the oldest end must visibly move (no dead zone).
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = result.(model)
	if maxScroll > 0 && m.previewScroll >= maxScroll {
		t.Errorf("pgdown should move forward from max; stuck at %d", m.previewScroll)
	}
}

func TestApplyLiveKeepsUnrelatedCaches(t *testing.T) {
	sel := Conversation{SessionID: "sel", Size: 1, LastTimestamp: "2026-09-25T09:00:00Z", Messages: []Message{{Role: "user", Text: "selected"}}}
	other := Conversation{SessionID: "other", Size: 1, LastTimestamp: "2026-09-25T08:00:00Z", Messages: []Message{{Role: "user", Text: "other"}}}
	m := initialModel(buildItems([]Conversation{sel, other}), "", nil)
	lines := m.previewLines()
	grown := other
	grown.Size = 2
	grown.Messages = append(slices.Clone(other.Messages), Message{Role: "assistant", Text: "more"})
	m.applyLive([]Conversation{grown})
	if m.filtered[m.cursor].conv.SessionID != "sel" {
		t.Fatal("cursor moved")
	}
	if got := m.previewLines(); &got[0] != &lines[0] {
		t.Error("selected preview should not be rebuilt when another session changed")
	}
}

func TestPreviewShowsWholeConversationWithoutSearch(t *testing.T) {
	var conv Conversation
	for i := 0; i < 300; i++ {
		conv.Messages = append(conv.Messages, Message{Role: "user", Text: fmt.Sprint("msg-", i)})
	}
	all := strings.Join(buildPreviewLines(conv, "", 0), "\n")
	if strings.Contains(all, "messages ...") || !strings.Contains(all, "msg-150") {
		t.Error("without a search every message should be scrollable")
	}
	found := strings.Join(buildPreviewLines(conv, "msg-150", 0), "\n")
	if !strings.Contains(found, "messages ...") || strings.Contains(found, "msg-100\n") {
		t.Error("with a search the preview should collapse to matches in context")
	}
}

func BenchmarkFullPreview7500(b *testing.B) {
	var conv Conversation
	for i := 0; i < 7500; i++ {
		conv.Messages = append(conv.Messages, Message{Role: "assistant", Text: strings.Repeat("some reply text ", 30), Ts: "2026-09-29T10:00:00Z"})
	}
	for i := 0; i < b.N; i++ {
		buildPreviewLines(conv, "", 0)
	}
}

func TestPreviewOpensAtNewestAndScrollsBackToFirst(t *testing.T) {
	var conv Conversation
	conv.SessionID, conv.Cwd = "s", "/p"
	for i := 0; i < 60; i++ {
		conv.Messages = append(conv.Messages, Message{Role: "user", Text: fmt.Sprint("line-", i)})
	}
	m := initialModel(buildItems([]Conversation{conv}), "", nil)
	m.width, m.height = 120, 40
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	screen := func() []string { return strings.Split(strip.ReplaceAllString(m.View(), ""), "\n") }

	// Opens with the newest message at the bottom.
	lines := screen()
	if len(lines) != m.height || !strings.Contains(strings.Join(lines[len(lines)-2:], "\n"), "line-59") {
		t.Fatalf("should open at the newest message; last rows:\n%s", strings.Join(lines[len(lines)-3:], "\n"))
	}
	// Scrolling back stops with the first message just under the header.
	_, _, previewTop := m.listLayout()
	for i := 0; i < 100; i++ {
		m = m.handleMouse(tea.MouseMsg{X: m.width - 10, Y: previewTop + 1, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	}
	if m.previewScroll != m.maxPreviewScroll() {
		t.Errorf("scroll %d should stop at the max %d", m.previewScroll, m.maxPreviewScroll())
	}
	if !regexp.MustCompile(`line-0\b`).MatchString(strings.Join(screen(), "\n")) {
		t.Error("scrolled all the way back, the first message should be visible")
	}
}

func TestPreviewReadable(t *testing.T) {
	long := strings.Repeat("word ", 60)
	conv := Conversation{Messages: []Message{
		{Role: "user", Text: "<task-notification>\n<task-id>x</task-id>\n<summary>Monitor event: CI on #336</summary>\n<event>check build: fail</event>\n</task-notification>"},
		{Role: "assistant", Text: "## Result\nThe **build** failed in `docker build`.\n- " + long + "\n```\ncode line\n```\n" + strings.Repeat("x", 600)},
	}}
	lines := buildPreviewLines(conv, "", 80)
	plain := strip2(strings.Join(lines, "\n"))
	for i, l := range strings.Split(plain, "\n") {
		if w := utf8.RuneCountInString(l); w > 80 {
			t.Errorf("line %d is %d wide, over 80: %q", i, w, l)
		}
	}
	if !strings.Contains(plain, "▸ task-notification · Monitor event: CI on #336") || strings.Contains(plain, "<task-id>") {
		t.Errorf("harness message should collapse to one summary line:\n%s", plain)
	}
	if strings.Contains(plain, "**") || strings.Contains(plain, "`") || strings.Contains(plain, "## ") {
		t.Errorf("markdown markers should be rendered, not shown:\n%s", plain)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "\033[1mbuild\033[22m") || !strings.Contains(joined, "\033[36mdocker build\033[39m") {
		t.Error("bold and inline code should be styled")
	}
	// A wrapped bullet's continuation lines line up with its text.
	var bullet []string
	for _, l := range strings.Split(plain, "\n") {
		if strings.HasPrefix(l, "  - word") || (len(bullet) > 0 && strings.HasPrefix(l, "    word")) {
			bullet = append(bullet, l)
		}
	}
	if len(bullet) < 2 {
		t.Errorf("long bullet should wrap with a hanging indent:\n%s", plain)
	}
	// No more 500-character cut.
	if strings.Contains(plain, "(truncated)") || strings.Count(plain, "x") < 600 {
		t.Error("messages should be shown in full")
	}
}

func BenchmarkReadablePreview7500(b *testing.B) {
	var conv Conversation
	for i := 0; i < 7500; i++ {
		conv.Messages = append(conv.Messages, Message{Role: "assistant", Text: "Some **bold** reply with `code` and a long line " + strings.Repeat("of words ", 30), Ts: "2026-09-29T10:00:00Z"})
	}
	for i := 0; i < b.N; i++ {
		buildPreviewLines(conv, "", 160)
	}
}

func TestLongCodeBlocksCollapse(t *testing.T) {
	diff := "```diff\n" + strings.Repeat("+ added line\n", 40) + "- needle removed\n```"
	conv := Conversation{Messages: []Message{{Role: "assistant", Text: "Here's the change:\n" + diff + "\nShort one:\n```go\nx := 1\n```"}}}
	plain := strip2(strings.Join(buildPreviewLines(conv, "", 100), "\n"))
	if !strings.Contains(plain, "▸ diff · 41 lines") || strings.Contains(plain, "+ added line") {
		t.Errorf("a long diff should collapse to one line:\n%s", plain)
	}
	if !strings.Contains(plain, "x := 1") {
		t.Error("short code blocks stay inline")
	}
	// A search that matches inside keeps it expanded.
	found := strip2(strings.Join(buildPreviewLines(conv, "needle", 100), "\n"))
	if !strings.Contains(found, "needle removed") {
		t.Error("a block containing the search match should stay expanded")
	}
}

func TestLinksAndCollapsedIndent(t *testing.T) {
	conv := Conversation{Messages: []Message{
		{Role: "user", Text: "<task-notification><summary>CI done</summary></task-notification>", Ts: "2026-09-29T10:00:00Z"},
		{Role: "assistant", Text: "See [slack-bot#336](https://github.com/acme/slack-bot/pull/336) and [https://x.io/a](https://x.io/a)."},
	}}
	plain := strip2(strings.Join(buildPreviewLines(conv, "", 120), "\n"))
	if !regexp.MustCompile(`(?m)^  ▸ task-notification · CI done$`).MatchString(plain) {
		t.Errorf("collapsed line should keep its indent:\n%s", plain)
	}
	if !strings.Contains(plain, "slack-bot#336 (github.com/acme/slack-bot/pull/336)") || strings.Contains(plain, "](") {
		t.Errorf("links should render as text plus a short address:\n%s", plain)
	}
	if !strings.Contains(plain, "and x.io/a.") {
		t.Errorf("a link whose text is its URL shows once:\n%s", plain)
	}
}

func TestPreviewDrawnAsClaudeCode(t *testing.T) {
	conv := Conversation{Messages: []Message{
		{Role: "user", Text: "merge it", Ts: "2026-09-29T10:00:00Z"},
		{Role: "assistant", Text: "Merged.", Ts: "2026-09-29T10:01:00Z"},
		{Role: "assistant", Text: "The reply didn't post.", Ts: "2026-09-29T10:02:00Z"},
	}}
	lines := buildPreviewLines(conv, "", 40)
	plain := strip2(strings.Join(lines, "\n"))
	when := formatTimestamp("2026-09-29T10:00:00Z")
	if strings.Contains(plain, "Claude\n") || strings.Contains(plain, "User\n") || strings.Contains(plain, when[:10]) || strings.Contains(plain, when[11:]) {
		t.Errorf("no speaker headers, and no date or time among the messages:\n%s", plain)
	}
	for _, want := range []string{"\n● Merged.\n", "\n● The reply didn't post.\n", "❯ merge it"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q:\n%s", want, plain)
		}
	}
	// The user's row is painted the full width, background to the last cell.
	row := lines[slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "merge it") })]
	if !strings.HasPrefix(row, userBg) || ansi.StringWidth(row) != 40 {
		t.Errorf("user row should be painted 40 wide, got %d: %q", ansi.StringWidth(row), row)
	}
	// A search match is marked in place of its bullet, and stays findable.
	found := buildPreviewLines(conv, "reply", 40)
	if !strings.Contains(strip2(strings.Join(found, "\n")), "▶ The reply didn't post.") || len(hitLinesIn(found)) != 1 {
		t.Errorf("a match should be marked:\n%s", strip2(strings.Join(found, "\n")))
	}
	// A match in the user's row: findable, and the background survives the highlight.
	found = buildPreviewLines(conv, "merge it", 40)
	if len(hitLinesIn(found)) != 1 {
		t.Errorf("a user match should be a hit: %q", found)
	}
	if !strings.Contains(strings.Join(found, "\n"), "\033[49;39m"+userBg) {
		t.Errorf("the highlight should hand back to the row's background: %q", found)
	}
}

func TestUserRowWrapsWithBackground(t *testing.T) {
	conv := Conversation{Messages: []Message{{Role: "user", Text: strings.Repeat("word ", 30) + "\n\nsecond **bold** paragraph"}}}
	lines := buildPreviewLines(conv, "", 40)
	lines = lines[:len(lines)-1] // the blank after it
	if len(lines) < 4 {
		t.Fatalf("should wrap: %q", lines)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, userBg) || ansi.StringWidth(l) != 40 {
			t.Errorf("line %d should be painted full width (%d): %q", i, ansi.StringWidth(l), l)
		}
	}
	if !strings.HasPrefix(strip2(lines[1]), "  word") {
		t.Errorf("wrapped text should hang under the first line's: %q", strip2(lines[1]))
	}
}

func TestToolCallsInPreview(t *testing.T) {
	conv := &Conversation{}
	line := func(content string) {
		parseLine(conv, []byte(`{"type":"assistant","timestamp":"2026-09-29T10:00:00Z","message":{"content":`+content+`}}`))
	}
	line(`[{"type":"text","text":"Checking."}]`)
	line(`[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"cd /x && ls\n  indented **stars**\n\tcat y","description":"list"}}]`)
	line(`[{"type":"tool_use","id":"2","name":"Write","input":{"file_path":"/a/b.go","content":"secret body"}}]`)
	line(`[{"type":"tool_use","id":"3","name":"mcp__ccs__list_prs","input":{}}]`)
	line(`[{"type":"tool_use","id":"4","name":"SendMessage","input":{"to":"uds:/tmp/9.sock","message":"hi there"}}]`)
	if len(conv.Messages) != 2 { // the text and the SendMessage: tool calls are never messages
		t.Fatalf("tool calls must stay out of Messages: %+v", conv.Messages)
	}
	plain := strip2(strings.Join(buildPreviewLines(*conv, "", 60), "\n"))
	for _, want := range []string{
		"● Checking.\n\n● Bash(cd /x && ls\n    indented **stars**\n      cat y)\n\n",
		"● Write(/a/b.go)\n",
		"● ccs - list_prs (MCP)\n",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "secret body") || strings.Count(plain, "hi there") != 1 {
		t.Errorf("a file's content isn't shown, and a SendMessage only as its message:\n%s", plain)
	}
	// A long command wraps inside the width; a search shows matches and context only.
	conv.tools = append(conv.tools, toolUse{at: 2, name: "Bash", arg: strings.Repeat("longword ", 30), ts: "2026-09-29T10:00:00Z"})
	for i, l := range strings.Split(strip2(strings.Join(buildPreviewLines(*conv, "", 60), "\n")), "\n") {
		if w := ansi.StringWidth(l); w > 60 {
			t.Errorf("line %d is %d wide: %q", i, w, l)
		}
	}
	if found := strip2(strings.Join(buildPreviewLines(*conv, "checking", 60), "\n")); strings.Contains(found, "Bash(") {
		t.Errorf("tool calls aren't search context:\n%s", found)
	}
	// One arriving redraws the cached preview.
	c := &previewCache{}
	before := len(cachedPreview(c, *conv, "", 60))
	noteTool(conv, "Read", "/z", "2026-09-29T10:00:00Z")
	if len(cachedPreview(c, *conv, "", 60)) == before {
		t.Error("a new tool call should redraw the preview")
	}
}

func TestToolArgControlCodesCleaned(t *testing.T) {
	lines := renderTool(toolUse{name: "Bash", arg: "echo \x1b[31mred\x1b[0m\rdone"}, 60)
	if strings.Contains(strings.Join(lines, ""), "\x1b[31m") || strings.Contains(strings.Join(lines, ""), "\r") {
		t.Errorf("a transcript's control codes must not reach the terminal: %q", lines)
	}
}

func TestHarnessNotesStack(t *testing.T) {
	note := "<task-notification>\n<summary>Monitor event: CI</summary>\n</task-notification>"
	conv := Conversation{Messages: []Message{
		{Role: "assistant", Text: "watching", Ts: "2026-09-11T17:02:00Z"},
		{Role: "user", Text: note, Ts: "2026-09-11T17:03:00Z"},
		{Role: "user", Text: note, Ts: "2026-09-11T17:03:30Z"},
		{Role: "assistant", Text: "done", Ts: "2026-09-11T17:04:00Z"},
	}}
	plain := strip2(strings.Join(buildPreviewLines(conv, "", 100), "\n"))
	if want := "  ▸ task-notification · Monitor event: CI\n  ▸ task-notification"; !strings.Contains(plain, want) {
		t.Errorf("notes should stack without a blank line:\n%s", plain)
	}
}

func TestHeaderCollapses(t *testing.T) {
	conv := Conversation{SessionID: "abc", Title: "fix login", Cwd: "/code/api", ActiveModel: "claude-opus-5-5", ContextTokens: 350000, Model1M: true, PeakContext: 350000, Size: 2048,
		Messages: []Message{{Role: "assistant", Text: "hello", Ts: "2026-09-11T10:00:00Z"}}}
	item := listItem{conv: conv}
	m := initialModel([]listItem{item}, "", nil)
	m.width, m.height = 140, 30
	full := len(m.previewHeader(conv, ""))

	// ^X D folds it to one line that still has the key facts, and more rows for messages.
	rows := m.previewMessageRows(conv)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if !m.headerCollapsed {
		t.Fatal("^X D should fold the header")
	}
	if loadSettings().HeaderOpen {
		t.Error("the choice should be saved")
	}
	head := m.previewHeader(conv, "")
	if len(head) != 2 || full <= 2 || m.previewMessageRows(conv) <= rows {
		t.Errorf("should be one line and a blank row, got %d of %d", len(head), full)
	}
	s := strip2(head[0])
	for _, want := range []string{"fix login", "api", "35% ctx", "^X D"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary should say %q: %q", want, s)
		}
	}
	if strings.Contains(strip2(m.renderPreview(item, 20)), "Session:") {
		t.Error("the full header shouldn't show when folded")
	}

	// A click on it unfolds, and again folds; clicks on messages do neither.
	_, _, top := m.listLayout()
	x := m.width - 5
	m = m.handleMouse(tea.MouseMsg{X: x, Y: top, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.headerCollapsed {
		t.Error("a click on the line should unfold it")
	}
	m = m.handleMouse(tea.MouseMsg{X: x, Y: top + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.headerCollapsed {
		t.Error("a click on the full header should fold it")
	}
	m = m.handleMouse(tea.MouseMsg{X: x, Y: top + 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.headerCollapsed {
		t.Error("a click below the header shouldn't toggle it")
	}
	if !slices.ContainsFunc(m.shortcuts(), func(s [2]string) bool { return s[0] == "^X D" }) {
		t.Error("^X D should be in the help popup")
	}
}

func TestHeaderSummaryFitsAndCounts(t *testing.T) {
	conv := Conversation{SessionID: "abc", Cwd: "/code/" + strings.Repeat("long", 40), Size: 1, LastError: "529 overloaded",
		Messages: []Message{{Role: "assistant", Text: "x"}}}
	m := initialModel([]listItem{{conv: conv}}, "", nil)
	m.width, m.height = 60, 20
	m.headerCollapsed = true
	if w := ansi.StringWidth(m.headerSummary(conv, "")); w > m.paneWidth()-1 {
		t.Errorf("summary is %d wide in a pane of %d", w, m.paneWidth())
	}
	m.width = 200
	if !strings.Contains(strip2(m.headerSummary(conv, "")), "! 529 overloaded") {
		t.Errorf("an API error should show: %q", strip2(m.headerSummary(conv, "")))
	}
}

func TestHeaderSaysWhenTheTopMessageHappened(t *testing.T) {
	var msgs []Message
	for i := range 20 {
		msgs = append(msgs, Message{Role: "assistant", Text: fmt.Sprintf("reply %d", i), Ts: fmt.Sprintf("2026-09-11T10:%02d:00Z", i)})
	}
	conv := Conversation{SessionID: "s", Cwd: "/p", Messages: msgs}
	item := listItem{conv: conv}
	m := initialModel([]listItem{item}, "", nil)
	m.width, m.height = 100, 40
	m.headerCollapsed = true
	check := func() {
		t.Helper()
		lines := strings.Split(m.renderPreview(item, 12), "\n")
		n := len(m.previewHeader(conv, ""))
		i, _ := strconv.Atoi(strings.Fields(strip2(lines[n]))[2]) // "● reply 16 │"
		want := humanWhen(formatTimestamp(msgs[i].Ts), "Mon 2 Jan 15:04")
		if !strings.HasSuffix(strip2(lines[0]), " "+want) {
			t.Errorf("the top message's date and time should end the first line, want %q: %q", want, strip2(lines[0]))
		}
		if w := ansi.StringWidth(lines[0]); w != m.paneWidth()-1 {
			t.Errorf("the first line should reach the pane's edge, %d of %d: %q", w, m.paneWidth()-1, strip2(lines[0]))
		}
	}
	check() // at the newest
	m.previewScroll = 8
	check() // it follows the scroll
	m.headerCollapsed = false
	if first := strip2(strings.Split(m.renderPreview(item, 30), "\n")[0]); !strings.HasPrefix(first, "Project:") || !strings.Contains(first, "Sep") {
		t.Errorf("the full header carries it too: %q", first)
	}
}

func TestSummaryKeepsItsFactsBesideTheTime(t *testing.T) {
	conv := Conversation{SessionID: "s", Title: strings.Repeat("a long topic ", 8), Cwd: "/code/api", ActiveModel: "claude-opus-5-5", ContextTokens: 90000, Size: 2048,
		Messages: []Message{{Role: "assistant", Text: "x", Ts: "2026-09-11T10:00:00Z"}}}
	item := listItem{conv: conv}
	m := initialModel([]listItem{item}, "", nil)
	m.width, m.height, m.headerCollapsed = 100, 30, true
	first := strip2(strings.Split(m.renderPreview(item, 20), "\n")[0])
	for _, want := range []string{"a long", "9% ctx", humanWhen(formatTimestamp(conv.Messages[0].Ts), "Mon 2 Jan 15:04")} {
		if !strings.Contains(first, want) {
			t.Errorf("a %d-wide pane should keep %q: %q", m.paneWidth(), want, first)
		}
	}
}

func TestWithWhen(t *testing.T) {
	if got := withWhen("short", "Wed 28 Oct 21:51", 60); ansi.StringWidth(got) != 60 || !strings.HasPrefix(strip2(got), "short ") {
		t.Errorf("padded to the edge: %q", got)
	}
	long := strings.Repeat("x", 80)
	if got := strip2(withWhen(long, "Wed 28 Oct 21:51", 60)); ansi.StringWidth(got) != 60 || !strings.Contains(got, "…") || !strings.HasSuffix(got, "21:51") {
		t.Errorf("a long line is cut to fit: %q", got)
	}
	if got := withWhen(long, "Wed 28 Oct 21:51", 40); got != long {
		t.Errorf("too narrow for both keeps the line: %q", got)
	}
}

func TestHumanWhen(t *testing.T) {
	this := time.Now().Year()
	if got, want := humanWhen(fmt.Sprintf("%d-10-28 21:51", this), "Jan 2 15:04"), "Oct 28 21:51"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := humanWhen("2020-10-28 21:51", "Mon 2 Jan 15:04"); got != "Wed 28 Oct 2020 21:51" {
		t.Errorf("another year says so: %q", got)
	}
	if got := humanWhen("2020-10-28", "Mon 2 Jan"); got != "Wed 28 Oct 2020" {
		t.Errorf("a date alone: %q", got)
	}
	if got := humanWhen("not a date", "Mon 2 Jan"); got != "not a date" {
		t.Errorf("anything else is left as it is: %q", got)
	}
}

func TestTurnEndsAndNewDays(t *testing.T) {
	conv := &Conversation{}
	parseLine(conv, []byte(`{"type":"user","timestamp":"2026-09-29T10:00:00Z","message":{"content":"go"}}`))
	parseLine(conv, []byte(`{"type":"assistant","timestamp":"2026-09-29T10:01:00Z","message":{"content":[{"type":"text","text":"Done."}]}}`))
	parseLine(conv, []byte(`{"type":"system","subtype":"turn_duration","durationMs":413185,"timestamp":"2026-09-29T10:07:00Z"}`))
	parseLine(conv, []byte(`{"type":"system","subtype":"stop_hook_summary","timestamp":"2026-09-29T10:07:01Z"}`))
	parseLine(conv, []byte(`{"type":"system","subtype":"turn_duration","durationMs":0,"timestamp":"2026-09-29T10:07:02Z"}`))
	parseLine(conv, []byte(`{"type":"user","timestamp":"2026-10-02T10:00:00Z","message":{"content":"again"}}`))
	if len(conv.Messages) != 3 {
		t.Fatalf("turn ends aren't messages: %+v", conv.Messages)
	}
	plain := strip2(strings.Join(buildPreviewLines(*conv, "", 60), "\n"))
	clock := formatTimestamp("2026-09-29T10:07:00Z")[11:]
	if want := "● Done.\n\n  ✻ 6m 53s · " + clock + "\n\n"; !strings.Contains(plain, want) {
		t.Errorf("missing %q:\n%s", want, plain)
	}
	if strings.Count(plain, "✻") != 1 {
		t.Errorf("only recorded, non-zero durations make a line:\n%s", plain)
	}
	if day := humanWhen(formatTimestamp("2026-10-02T10:00:00Z")[:10], "Mon 2 Jan"); !strings.Contains(plain, "  "+day+"\n\n") {
		t.Errorf("a new day gets a row saying so (%q):\n%s", day, plain)
	}
	if first := humanWhen(formatTimestamp("2026-09-29T10:00:00Z")[:10], "Mon 2 Jan"); strings.Contains(plain, first) {
		t.Errorf("the first day is the header's to say:\n%s", plain)
	}
	if found := strip2(strings.Join(buildPreviewLines(*conv, "done", 60), "\n")); strings.Contains(found, "✻") {
		t.Errorf("a search shows matches and context, not turn ends:\n%s", found)
	}
}

func TestTurnEndFormats(t *testing.T) {
	for took, want := range map[time.Duration]string{400 * time.Millisecond: "1s", 42 * time.Second: "42s", 65 * time.Second: "1m 5s", 3900 * time.Second: "1h 5m"} {
		if got := strip2(turnEnd(Conversation{}, toolUse{took: took})); got != "  ✻ "+want {
			t.Errorf("%v: got %q", took, got)
		}
	}
}

func TestMarkdownTable(t *testing.T) {
	text := "Results:\n| PR | State | Note |\n|---|:---:|---|\n| #70 | **merged** | changelog popup |\n| #71 | open | " + strings.Repeat("long ", 30) + "|\nafter"
	plain := strip2(strings.Join(renderBody(text, "", 60), "\n"))
	for _, want := range []string{"PR  │ State  │ Note", "────┼────────┼", "#70 │ merged │ changelog popup", "…", "after"} {
		if !strings.Contains(plain, want) {
			t.Errorf("table should render as aligned columns, missing %q:\n%s", want, plain)
		}
	}
	for i, l := range strings.Split(plain, "\n") {
		if w := ansi.StringWidth(l); w > 60 {
			t.Errorf("line %d is %d wide, over 60: %q", i, w, l)
		}
	}
	if strings.Contains(plain, "|---") || strings.Contains(plain, "**") {
		t.Errorf("markdown table syntax should not show:\n%s", plain)
	}
}

func TestLinksClickable(t *testing.T) {
	text := "See [the PR](https://github.com/a/b/pull/7) and https://example.com/x?y=1. Also a long [link text that wraps across lines](https://e.io/z)."
	joined := strings.Join(renderBody(text, "", 0), "\n")
	lines := renderBody(text, "", 30)
	for _, want := range []string{
		"\033]8;;https://github.com/a/b/pull/7\033\\the\033]8;;\033\\",
		"\033]8;;https://example.com/x?y=1\033\\example.com/x?y=1\033]8;;\033\\\033[24m.",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing clickable link %q in:\n%q", want, joined)
		}
	}
	for i, l := range lines {
		if strings.Count(l, "\033]8;;https") != strings.Count(l, "\033]8;;\033\\") {
			t.Errorf("line %d leaves a link open: %q", i, l)
		}
	}
	if plain := strip2(joined); !strings.Contains(plain, "the PR (github.com/a/b/pull/7)") {
		t.Errorf("links should still read as text plus a short address:\n%s", plain)
	}
}

func TestTeammateMessagesReadable(t *testing.T) {
	report := `{"type":"idle_notification","from":"sonnet-alerts-c","timestamp":"2026-09-30T09:33:22.836Z","result":"I only got part of this batch done.\n\n**Decision for you**: merge 189.","idleReason":"available"}`
	ping := `{"type":"idle_notification","from":"opus-x","timestamp":"2026-09-30T09:34:00Z","idleReason":"available"}`
	text := "Another Claude session sent a message:\n<teammate-message teammate_id=\"sonnet-alerts-c\" color=\"red\">\n" + report + "\n</teammate-message>\n\n" +
		"<teammate-message teammate_id=\"opus-x\" color=\"blue\">\n" + ping + "\n</teammate-message>\n\n" +
		"<teammate-message teammate_id=\"team-lead\" summary=\"Round 2\">\nPlease attack the revised proposal.\n</teammate-message>"
	conv := Conversation{Messages: []Message{{Role: "user", Text: text, Ts: "2026-09-30T09:34:00Z"}}}
	plain := strip2(strings.Join(buildPreviewLines(conv, "", 100), "\n"))
	for _, want := range []string{
		"From sonnet-alerts-c · idle notification",
		"I only got part of this batch done.",
		"Decision for you: merge 189.",
		"▸ opus-x · idle notification · available",
		"From team-lead · Round 2",
		"Please attack the revised proposal.",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in:\n%s", want, plain)
		}
	}
	for _, bad := range []string{"<teammate-message", `{"type"`, `\n`, "Another Claude session"} {
		if strings.Contains(plain, bad) {
			t.Errorf("raw %q should not show:\n%s", bad, plain)
		}
	}
}

func TestWrappedLinkStaysContainedAndClickable(t *testing.T) {
	text := "The PR is still waiting for review: https://github.com/acme/infra/pull/4369"
	lines := renderBody(text, "", 50)
	if len(lines) < 2 {
		t.Fatalf("expected a wrap: %q", lines)
	}
	for i, l := range lines {
		if strings.Count(l, "\033]8;;https") != strings.Count(l, "\033]8;;\033\\") {
			t.Errorf("line %d leaves the link open: %q", i, l)
		}
		if strings.Contains(l, "\033[4m") && !strings.HasSuffix(l, "\033[0m") && !strings.HasSuffix(l, "\033[24m") {
			t.Errorf("line %d leaves the underline open: %q", i, l)
		}
	}
	last := lines[len(lines)-1]
	x := ansi.StringWidth(strings.TrimRight(ansiSeq.ReplaceAllString(last, ""), " ")) - 1
	if got := linkAt(last, x); got != "https://github.com/acme/infra/pull/4369" {
		t.Errorf("clicking the wrapped part should find the link, got %q in %q", got, last)
	}
	if got := linkAt(lines[0], 0); got != "" {
		t.Errorf("plain text isn't a link, got %q", got)
	}
}

func TestClickOpensLink(t *testing.T) {
	var opened string
	defer func(f func(string) error) { openURL = f }(openURL)
	openURL = func(u string) error { opened = u; return nil }
	conv := Conversation{SessionID: "s", Cwd: "/p", Messages: []Message{{Role: "assistant", Text: "see https://example.com/a", Ts: "2026-09-30T10:00:00Z"}}}
	m := initialModel([]listItem{{conv: conv}}, "", nil)
	m.width, m.height = 100, 40
	_, _, previewTop := m.listLayout()
	lines := strings.Split(m.renderPreview(m.filtered[0], m.previewRenderHeight()), "\n")
	for row, l := range lines {
		plain := ansiSeq.ReplaceAllString(l, "")
		if x := strings.Index(plain, "example.com"); x >= 0 {
			nm, cmd := m.Update(tea.MouseMsg{X: m.sidebarWidth() + 1 + ansi.StringWidth(plain[:x]) + 2, Y: previewTop + row, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			m = nm.(model)
			if cmd == nil {
				t.Fatal("clicking a link should open it")
			}
			cmd()
			if opened != "https://example.com/a" {
				t.Errorf("opened %q", opened)
			}
			return
		}
	}
	t.Fatal("link not on screen")
}

func TestControlCodesStripped(t *testing.T) {
	conv := Conversation{Messages: []Message{{Role: "assistant", Text: "ok \x1b[1;35mcoloured\x1b[0m\rdone\x07", Ts: "2026-09-30T10:00:00Z"}}}
	joined := strings.Join(buildPreviewLines(conv, "", 80), "\n")
	if strings.Contains(joined, "\x1b[1;35m") || strings.Contains(joined, "\r") || strings.Contains(joined, "\x07") {
		t.Errorf("control codes from the transcript should be dropped: %q", joined)
	}
	if !strings.Contains(strip2(joined), "ok coloureddone") {
		t.Errorf("text should survive: %q", strip2(joined))
	}
}

func TestJumpBetweenSearchHits(t *testing.T) {
	var msgs []Message
	for i := range 40 {
		text := fmt.Sprintf("filler %d\nmore", i)
		if i == 5 || i == 20 || i == 38 {
			text = fmt.Sprintf("needle %d\nmore", i)
		}
		msgs = append(msgs, Message{Role: []string{"user", "assistant"}[i%2], Text: text, Ts: "2026-09-30T10:00:00Z"})
	}
	conv := Conversation{SessionID: "s", Cwd: "/p", Messages: msgs}
	m := initialModel(buildItems([]Conversation{conv}), "needle", nil)
	m.width, m.height = 100, 30
	m.table = true // the hit positions below assume the table's preview height
	hits := m.hitLines()
	if len(hits) != 3 {
		t.Fatalf("want 3 hits, got %v", hits)
	}
	top := func() int {
		lines := m.previewLines()
		rows := m.previewMessageRows(m.filtered[m.cursor].conv)
		return max(0, len(lines)-rows-min(m.previewScroll, max(0, len(lines)-rows)))
	}
	// Opens at the bottom; previous goes back through the hits, then wraps.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlBackslash})
	if top() != hits[1] {
		t.Errorf("previous from the bottom should reach the middle hit (%d), top=%d", hits[1], top())
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlBackslash})
	if top() != hits[0] {
		t.Errorf("previous again should reach the first hit (%d), top=%d", hits[0], top())
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	if top() != hits[1] {
		t.Errorf("next should go to the middle hit, top=%d", top())
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlCloseBracket}) // last hit sits in the final screen
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	if top() != hits[0] {
		t.Errorf("next past the end should wrap to the first hit, top=%d", top())
	}
	if v := strip2(m.View()); !strings.Contains(v, "hit 1/3") {
		t.Errorf("preview should show the hit position:\n%s", v)
	}
	m.textInput.SetValue("")
	m.updateFilter()
	if v := strip2(m.View()); strings.Contains(v, "hit ") {
		t.Errorf("no hit position without a search:\n%s", v)
	}
}

func TestLinksPopup(t *testing.T) {
	var opened []string
	defer func(f func(string) error) { openURL = f }(openURL)
	openURL = func(u string) error { opened = append(opened, u); return nil }
	conv := Conversation{SessionID: "s", Cwd: "/p", Messages: []Message{
		{Role: "assistant", Text: "See [the release notes](https://github.com/a/b/releases) and https://example.com/x.", Ts: "2026-09-30T10:00:00Z"},
		{Role: "assistant", Text: "Again https://example.com/x and a long [link text that wraps across more than one line here](https://e.io/z)", Ts: "2026-09-30T10:01:00Z"},
	}}
	m := initialModel([]listItem{{conv: conv}}, "", nil)
	m.width, m.height = 60, 40
	want := []linkItem{
		{"https://github.com/a/b/releases", "the release notes"},
		{"https://example.com/x", "example.com/x"},
		{"https://e.io/z", "link text that wraps across more than one line here"},
	}
	if got := m.visibleLinks(); !reflect.DeepEqual(got, want) {
		t.Errorf("visible links = %q, want %q", got, want)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.linksOpen {
		t.Fatal("^O should open the links popup")
	}
	if v := strip2(m.View()); !strings.Contains(v, "› the release notes github.com/a/b/releases") || !strings.Contains(v, "link text") {
		t.Errorf("popup should list the links:\n%s", v)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.linksOpen || cmd == nil {
		t.Fatal("enter should open the selected link and leave the popup open")
	}
	cmd()
	for _, r := range "wraps" {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if v := strip2(m.View()); strings.Contains(v, "› See the release notes") || !strings.Contains(v, "links in view 1/1") || !strings.Contains(v, "› link text") {
		t.Errorf("typing filters the links, cursor on the first match:\n%s", v)
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	cmd()
	if want := []string{"https://example.com/x", "https://e.io/z"}; !reflect.DeepEqual(opened, want) {
		t.Errorf("opened %q, want %q", opened, want)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zzz")})
	if v := strip2(m.View()); !strings.Contains(v, "no links match") {
		t.Errorf("nothing matching says so:\n%s", v)
	}
	if _, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Error("enter with nothing matching opens nothing")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if v := strip2(m.View()); !m.linksOpen || !strings.Contains(v, "release notes") {
		t.Errorf("reopening starts with an empty filter:\n%s", v)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.linksOpen || m.textInput.Value() != "" {
		t.Error("esc should close the popup without touching the search")
	}
}

func TestLinksPopupEmpty(t *testing.T) {
	conv := Conversation{SessionID: "s", Cwd: "/p", Messages: []Message{{Role: "assistant", Text: "no links here", Ts: "2026-09-30T10:00:00Z"}}}
	m := initialModel([]listItem{{conv: conv}}, "", nil)
	m.width, m.height = 100, 40
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.linksOpen || !strings.Contains(strip2(m.View()), "No links in view") {
		t.Errorf("with no links, ^O should say so instead of opening an empty popup:\n%s", strip2(m.View()))
	}
}

func TestLinksNeverCarryStyleCodes(t *testing.T) {
	osc := regexp.MustCompile(`\x1b\]8;;([^\x1b]*)(\x1b.)`)
	for _, in := range []string{
		"**https://github.com/o/r/pull/144** (plugin 2.2.0)",
		"see **[the PR](https://github.com/o/r/pull/1)** and `https://example.com/x`",
		"**bold** https://example.com/a*b*, then `code`",
	} {
		out := inlineMarkdown(in)
		for _, m := range osc.FindAllStringSubmatch(out, -1) {
			if m[2] != "\x1b\\" {
				t.Errorf("%q: a link's sequence must hold only its address, got %q", in, m[0])
			}
		}
	}
	if out := ansi.Strip(inlineMarkdown("**https://github.com/o/r/pull/144**")); out != "github.com/o/r/pull/144" {
		t.Errorf("a bold bare address should show as the address alone, got %q", out)
	}
}

func TestPreviewWaitsForTheSelectionToRest(t *testing.T) {
	defer func(d time.Duration) { previewDebounce = d }(previewDebounce)
	previewDebounce = 50 * time.Millisecond
	var convs []Conversation
	for i := range 3 {
		convs = append(convs, Conversation{SessionID: fmt.Sprint("s", i), Title: fmt.Sprint("t", i),
			LastTimestamp: time.Now().Add(-time.Duration(i) * time.Minute).Format(time.RFC3339),
			Messages:      []Message{{Role: "user", Text: fmt.Sprint("body of s", i)}}})
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 120, 30
	res, _ := m.Update(struct{}{}) // the first selection shows at once
	m = res.(model)
	if len(m.previewLines()) == 0 {
		t.Fatal("the first selection isn't scrolling past anything")
	}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = res.(model)
	if len(m.previewLines()) != 0 || strings.Contains(strip2(m.View()), "body of s1") || cmd == nil {
		t.Error("just after moving, only the header: the conversation waits for the selection to rest")
	}
	time.Sleep(60 * time.Millisecond)
	if !strings.Contains(strip2(m.View()), "body of s1") {
		t.Error("once it has rested, the conversation shows")
	}
}

func TestLinksPopupFiltersPRsByStatus(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.prs = map[string]prInfo{}
	var prs []string
	for i := 1; i <= 25; i++ {
		u := fmt.Sprintf("https://github.com/o/r/pull/%d", i)
		prs = append(prs, u)
		m.prs[u] = prInfo{State: "MERGED", Title: strings.Repeat("a very long title ", 10)}
	}
	m.prs[prs[24]] = prInfo{State: "OPEN", MergeState: "CLEAN", Title: "go"}
	conv := Conversation{SessionID: "s", Cwd: "/p", PRs: prs, Messages: []Message{{Role: "assistant", Text: "hi", Ts: "2026-09-30T10:00:00Z"}}}
	m.items = []listItem{{conv: conv}}
	m.filtered = m.items
	m.width, m.height = 100, 20
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	v := strip2(m.View())
	if !strings.Contains(v, "› ✓ r#25 ready to merge · go") || !strings.Contains(v, "● r#1 merged · a very long") {
		t.Errorf("each PR's status before its title, ready first:\n%s", v)
	}
	if strings.Contains(v, "r#24 ") || !strings.Contains(v, "1/25") || !strings.Contains(v, "┃") {
		t.Errorf("a list taller than the screen scrolls, with a scrollbar:\n%s", v)
	}
	for _, r := range "ready" {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if v := strip2(m.View()); !strings.Contains(v, "1/1") || strings.Contains(v, "r#1 merged") {
		t.Errorf("the status is searchable too:\n%s", v)
	}
	m.prs[prs[24]] = prInfo{State: "MERGED", Title: "go"}
	if v := strip2(m.View()); !strings.Contains(v, "no links match") {
		t.Errorf("rows follow the latest status:\n%s", v)
	}
}

const (
	channelPR    = "<channel source=\"plugin:agentic-utils-github:pr-events\" event=\"pull_request\" repo=\"two-inc/twoctl-cli\" pr=\"14\">\ntwo-inc/twoctl-cli#14: PR merged by brtkwr\nhttps://github.com/two-inc/twoctl-cli/pull/14\n(still watching for late comments)\n</channel>"
	channelSlack = "<channel source=\"plugin:agentic-utils-slack-inbox:slack-events\" event=\"message\" via=\"inbox\" trigger=\"dm\" channel=\"D02\" ts=\"1.2\">\ndm from jk (U01) in D02: limits api, thanks\nhttps://two-inc.slack.com/archives/D02/p12\n</channel>"
)

func TestChannelParts(t *testing.T) {
	ev, ok := channelParts(channelPR)
	if !ok || ev.line() != "← pr-events: two-inc/twoctl-cli#14: PR merged by brtkwr" ||
		ev.link != "https://github.com/two-inc/twoctl-cli/pull/14" || ev.detail != "(still watching for late comments)" {
		t.Errorf("got %+v, %v", ev, ok)
	}
	// the plugin's own name at the start of its text isn't said twice
	ev, _ = channelParts("<channel source=\"plugin:x:pr-events\" event=\"polling\">\npr-events: polling a/b#1 every 60s\n</channel>")
	if ev.line() != "← pr-events: polling a/b#1 every 60s" || ev.link != "" {
		t.Errorf("got %+v", ev)
	}
	// nothing but the event's name
	ev, _ = channelParts("<channel source=\"plugin:x:y\" event=\"ping\"></channel>")
	if ev.line() != "← y: ping" {
		t.Errorf("got %q", ev.line())
	}
	if _, ok := channelParts("<system-reminder>hello</system-reminder>"); ok {
		t.Error("only a channel tag is a channel event")
	}
}

// A meta channel line, or one queued mid-turn, is kept as an event; other meta
// lines stay dropped; neither end of the session's clock moves for one.
func TestParseKeepsChannelEvents(t *testing.T) {
	line := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) + "\n" }
	body := line(map[string]any{"type": "user", "timestamp": "2026-11-03T10:00:00Z", "message": map[string]any{"role": "user", "content": "hello"}}) +
		line(map[string]any{"type": "assistant", "timestamp": "2026-11-03T10:01:00Z", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "shall i merge?"}}}}) +
		line(map[string]any{"type": "user", "isMeta": true, "timestamp": "2026-11-03T10:02:00Z", "message": map[string]any{"role": "user", "content": "<system-reminder>not shown</system-reminder>"}}) +
		line(map[string]any{"type": "user", "isMeta": true, "timestamp": "2026-11-03T10:03:00Z", "message": map[string]any{"role": "user", "content": channelPR}}) +
		line(map[string]any{"type": "attachment", "timestamp": "2026-11-03T10:04:00Z", "attachment": map[string]any{"type": "queued_command", "commandMode": "prompt", "isMeta": true, "prompt": channelSlack}})
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	conv, err := parseConversationFile(path, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range conv.Messages {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, " "); got != "user assistant channel channel" {
		t.Fatalf("roles: %s", got)
	}
	if conv.LastTimestamp != "2026-11-03T10:01:00Z" {
		t.Errorf("an event moved the session's last message time: %s", conv.LastTimestamp)
	}
	if n := lastSaid(conv.Messages); n != 1 {
		t.Errorf("an event hid the question it asked: lastSaid %d", n)
	}
	if !strings.Contains(searchTextOf(*conv), "merged by brtkwr") {
		t.Error("events should be searchable")
	}
}

func TestChannelEventDrawnDim(t *testing.T) {
	conv := Conversation{Messages: []Message{
		{Role: "user", Text: "hello", Ts: "2026-11-03T10:00:00Z"},
		{Role: "channel", Text: channelPR, Ts: "2026-11-03T10:01:00Z"},
		{Role: "channel", Text: channelSlack, Ts: "2026-11-03T10:02:00Z"},
		{Role: "assistant", Text: "noted", Ts: "2026-11-03T10:03:00Z"},
	}}
	raw := strings.Join(buildPreviewLines(conv, "", 80), "\n")
	plain := strip2(raw)
	want := "  ← pr-events: two-inc/twoctl-cli#14: PR merged by brtkwr\n  ← slack-events: dm from jk (U01) in D02: limits api, thanks"
	if !strings.Contains(plain, want) {
		t.Errorf("want two stacked one-line events:\n%s", plain)
	}
	if strings.Contains(plain, "still watching") || strings.Contains(plain, "<channel") {
		t.Errorf("the body stays folded:\n%s", plain)
	}
	if !strings.Contains(raw, "\033[90m  \033]8;;https://github.com/two-inc/twoctl-cli/pull/14\033\\←") {
		t.Errorf("should be dim and link to the PR:\n%q", raw)
	}
	// A search that hits the body unfolds it, and the line is a hit to jump to.
	hit := buildPreviewLines(conv, "watching", 80)
	if !strings.Contains(strip2(strings.Join(hit, "\n")), "still watching") || len(hitLinesIn(hit)) != 1 {
		t.Errorf("a search hit should unfold it:\n%s", strip2(strings.Join(hit, "\n")))
	}
}

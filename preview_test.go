package main

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
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
		{"multibyte pads by rune count", "世界", 5, "世界   "},
		{"multibyte truncates on rune boundary", "世界世界世界", 4, "世界世界"},
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
		m = m.handleMouse(tea.MouseMsg{Y: previewTop + 1, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	}
	if m.previewScroll != m.maxPreviewScroll() {
		t.Errorf("scroll %d should stop at the max %d", m.previewScroll, m.maxPreviewScroll())
	}
	if !strings.Contains(strings.Join(screen(), "\n"), "line-0\n") {
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
		if strings.HasPrefix(l, "        - word") || (len(bullet) > 0 && strings.HasPrefix(l, "          word")) {
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
		{Role: "assistant", Text: "See [slack-bot#336](https://github.com/two-inc/slack-bot/pull/336) and [https://x.io/a](https://x.io/a)."},
	}}
	plain := strip2(strings.Join(buildPreviewLines(conv, "", 120), "\n"))
	if !regexp.MustCompile(`(?m)^  \S.* ▸ task-notification · CI done$`).MatchString(plain) {
		t.Errorf("collapsed line should keep its indent:\n%s", plain)
	}
	if !strings.Contains(plain, "slack-bot#336 (github.com/two-inc/slack-bot/pull/336)") || strings.Contains(plain, "](") {
		t.Errorf("links should render as text plus a short address:\n%s", plain)
	}
	if !strings.Contains(plain, "and x.io/a.") {
		t.Errorf("a link whose text is its URL shows once:\n%s", plain)
	}
}

func TestPreviewSpeakerRuns(t *testing.T) {
	conv := Conversation{Messages: []Message{
		{Role: "user", Text: "merge it", Ts: "2026-09-29T10:00:00Z"},
		{Role: "assistant", Text: "Merged.", Ts: "2026-09-29T10:01:00Z"},
		{Role: "assistant", Text: "The reply didn't post.", Ts: "2026-09-29T10:02:00Z"},
		{Role: "assistant", Text: "Posted now.", Ts: "2026-09-29T10:03:00Z"},
	}}
	plain := strip2(strings.Join(buildPreviewLines(conv, "", 80), "\n"))
	if n := strings.Count(plain, "── "+formatTimestamp("2026-09-29T10:02:00Z")[:10]+" ──"); n != 1 {
		t.Errorf("the date should get one row of its own, got %d:\n%s", n, plain)
	}
	if n := strings.Count(plain, "Claude\n"); n != 1 {
		t.Errorf("a run of Claude messages should share one header, got %d:\n%s", n, plain)
	}
	clock := formatTimestamp("2026-09-29T10:02:00Z")[11:]
	if !strings.Contains(plain, "  "+clock+" The reply didn't post.") {
		t.Errorf("each message should start with its time:\n%s", plain)
	}
	// A search match gets its own marked header even mid-run.
	plain = strip2(strings.Join(buildPreviewLines(conv, "reply", 80), "\n"))
	if !strings.Contains(plain, "▶ Claude") {
		t.Errorf("a match should be marked:\n%s", plain)
	}
}

func TestHarnessNotesUseTimeGutter(t *testing.T) {
	note := "<task-notification>\n<summary>Monitor event: CI</summary>\n</task-notification>"
	conv := Conversation{Messages: []Message{
		{Role: "assistant", Text: "watching", Ts: "2026-09-11T17:02:00Z"},
		{Role: "user", Text: note, Ts: "2026-09-11T17:03:00Z"},
		{Role: "user", Text: note, Ts: "2026-09-11T17:03:30Z"},
		{Role: "assistant", Text: "done", Ts: "2026-09-11T17:04:00Z"},
	}}
	plain := strip2(strings.Join(buildPreviewLines(conv, "", 100), "\n"))
	day := formatTimestamp("2026-09-11T17:03:00Z")[:10]
	if strings.Count(plain, day) != 1 {
		t.Errorf("the date should appear only on its own row:\n%s", plain)
	}
	clock := formatTimestamp("2026-09-11T17:03:00Z")[11:]
	want := "  " + clock + " ▸ task-notification · Monitor event: CI\n  "
	if !strings.Contains(plain, want) {
		t.Errorf("notes should sit in the time gutter and stack without a blank line:\n%s", plain)
	}
}

func TestPinnedDate(t *testing.T) {
	var msgs []Message
	for i := range 20 {
		msgs = append(msgs, Message{Role: "assistant", Text: fmt.Sprintf("reply %d", i), Ts: fmt.Sprintf("2026-09-11T10:%02d:00Z", i)})
	}
	conv := Conversation{SessionID: "s", Cwd: "/p", Messages: msgs}
	item := listItem{conv: conv}
	m := initialModel([]listItem{item}, "", nil)
	m.width, m.height = 100, 40
	day := dateRow(formatTimestamp(msgs[0].Ts)[:10])
	// At the bottom, the day's own row has scrolled away, so it's pinned.
	if lines := strings.Split(m.renderPreview(item, 12), "\n"); !slices.Contains(lines[:len(previewHeader(conv, ""))], day) {
		t.Errorf("date should be pinned above the messages:\n%s", strings.Join(lines, "\n"))
	}
	// At the top, the date row itself is shown, not pinned twice.
	m.previewScroll = 1000
	if got := m.renderPreview(item, 12); strings.Count(got, day) != 1 {
		t.Errorf("date should appear once at the top:\n%s", got)
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
	text := "The PR is still waiting for review: https://github.com/two-inc/infra/pull/4369"
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
	if got := linkAt(last, x); got != "https://github.com/two-inc/infra/pull/4369" {
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
			nm, cmd := m.Update(tea.MouseMsg{X: ansi.StringWidth(plain[:x]) + 2, Y: previewTop + row, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
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
		text := fmt.Sprintf("filler %d", i)
		if i == 5 || i == 20 || i == 38 {
			text = fmt.Sprintf("needle %d", i)
		}
		msgs = append(msgs, Message{Role: []string{"user", "assistant"}[i%2], Text: text, Ts: "2026-09-30T10:00:00Z"})
	}
	conv := Conversation{SessionID: "s", Cwd: "/p", Messages: msgs}
	m := initialModel(buildItems([]Conversation{conv}), "needle", nil)
	m.width, m.height = 100, 30
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
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if !m.linksOpen {
		t.Fatal("^T should open the links popup")
	}
	if v := strip2(m.View()); !strings.Contains(v, "1 the release notes github.com/a/b/releases") || !strings.Contains(v, "3 link text") {
		t.Errorf("popup should number the links:\n%s", v)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.linksOpen || cmd == nil {
		t.Fatal("enter should open the selected link and close the popup")
	}
	cmd()
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	cmd()
	if want := []string{"https://example.com/x", "https://e.io/z"}; !reflect.DeepEqual(opened, want) {
		t.Errorf("opened %q, want %q", opened, want)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.linksOpen || m.textInput.Value() != "" {
		t.Error("esc should close the popup without touching the search")
	}
}

func TestLinksPopupEmpty(t *testing.T) {
	conv := Conversation{SessionID: "s", Cwd: "/p", Messages: []Message{{Role: "assistant", Text: "no links here", Ts: "2026-09-30T10:00:00Z"}}}
	m := initialModel([]listItem{{conv: conv}}, "", nil)
	m.width, m.height = 100, 40
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.linksOpen || !strings.Contains(strip2(m.View()), "No links in view") {
		t.Errorf("with no links, ^T should say so instead of opening an empty popup:\n%s", strip2(m.View()))
	}
}

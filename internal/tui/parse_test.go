package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestBuildItems(t *testing.T) {
	conversations := []transcript.Conversation{
		{
			SessionID:      "session1",
			Cwd:            "/home/user/project1",
			FirstTimestamp: "2024-01-15T10:00:00Z",
			LastTimestamp:  "2024-01-15T10:03:00Z",
			Messages: []transcript.Message{
				{Role: "user", Text: "first message", Ts: "2024-01-15T10:00:00Z"},
				{Role: "assistant", Text: "response 1", Ts: "2024-01-15T10:01:00Z"},
				{Role: "user", Text: "second message", Ts: "2024-01-15T10:02:00Z"},
				{Role: "assistant", Text: "response 2", Ts: "2024-01-15T10:03:00Z"},
			},
		},
		{
			SessionID:      "session2",
			Cwd:            "/home/user/project2",
			FirstTimestamp: "2024-01-16T10:00:00Z",
			LastTimestamp:  "2024-01-16T10:01:00Z",
			Messages: []transcript.Message{
				{Role: "user", Text: "hello world", Ts: "2024-01-16T10:00:00Z"},
				{Role: "assistant", Text: "hi there", Ts: "2024-01-16T10:01:00Z"},
			},
		},
	}

	items := buildItems(conversations)

	// Should have exactly one item per conversation
	if len(items) != 2 {
		t.Errorf("buildItems returned %d items, want 2", len(items))
	}

	// First item should be for session1
	if items[0].conv.SessionID != "session1" {
		t.Errorf("first item should be session1, got %q", items[0].conv.SessionID)
	}

	// Search text should contain all user messages
	if !strings.Contains(items[0].searchText, "first message") || !strings.Contains(items[0].searchText, "second message") {
		t.Errorf("search text should contain all user messages, got %q", items[0].searchText)
	}

	// Search text should also contain assistant messages (findable by what Claude said)
	if !strings.Contains(items[0].searchText, "response 1") || !strings.Contains(items[0].searchText, "response 2") {
		t.Errorf("search text should contain assistant messages, got %q", items[0].searchText)
	}

	// Search text should contain session ID
	if !strings.Contains(items[0].searchText, "session1") {
		t.Errorf("search text should contain session ID, got %q", items[0].searchText)
	}

	// Search text should contain cwd
	if !strings.Contains(items[0].searchText, "/home/user/project1") {
		t.Errorf("search text should contain cwd, got %q", items[0].searchText)
	}

	// searchLower must be the lowercased searchText, precomputed for filtering
	if items[0].searchLower != strings.ToLower(items[0].searchText) {
		t.Errorf("searchLower = %q, want lowercased searchText %q", items[0].searchLower, strings.ToLower(items[0].searchText))
	}

	// Second item should be for session2
	if items[1].conv.SessionID != "session2" {
		t.Errorf("second item should be session2, got %q", items[1].conv.SessionID)
	}
}

func TestBuildItemsNoUserMessages(t *testing.T) {
	conversations := []transcript.Conversation{
		{
			SessionID:      "session1",
			Cwd:            "/home/user/project1",
			FirstTimestamp: "2024-01-15T10:00:00Z",
			LastTimestamp:  "2024-01-15T10:00:00Z",
			Messages: []transcript.Message{
				{Role: "assistant", Text: "only assistant", Ts: "2024-01-15T10:00:00Z"},
			},
		},
	}

	items := buildItems(conversations)

	// Should still have one item (we include all conversations now)
	if len(items) != 1 {
		t.Errorf("buildItems returned %d items, want 1", len(items))
	}
}

func TestBuildItemsProjectExtraction(t *testing.T) {
	conversations := []transcript.Conversation{
		{
			SessionID:      "session1",
			Cwd:            "/home/user/my-project",
			FirstTimestamp: "2024-01-15T10:00:00Z",
			LastTimestamp:  "2024-01-15T10:00:00Z",
			Messages: []transcript.Message{
				{Role: "user", Text: "test", Ts: "2024-01-15T10:00:00Z"},
			},
		},
	}

	items := buildItems(conversations)

	// Search text should contain full path
	if !strings.Contains(items[0].searchText, "/home/user/my-project") {
		t.Errorf("search text should contain full path, got %q", items[0].searchText)
	}
}

func TestRefreshKeepsSelectionAndFilter(t *testing.T) {
	mk := func(id, text string) listItem {
		return buildItems([]transcript.Conversation{{SessionID: id, Messages: []transcript.Message{{Role: "user", Text: text}}}})[0]
	}
	m := initialModel([]listItem{mk("a", "apple"), mk("b", "banana")}, "ban", nil)
	m.cursor = 0 // "b" is the only match
	next, cmd := m.Update(refreshMsg{
		items: []listItem{mk("c", "bandit"), mk("a", "apple"), mk("b", "banana")},
		live:  map[string]bool{"b": true},
	})
	m = next.(model)
	if cmd == nil {
		t.Error("refresh should schedule the next tick")
	}
	if len(m.filtered) != 2 || m.filtered[m.cursor].conv.SessionID != "b" {
		t.Errorf("cursor should stay on b within refiltered list, got %+v at %d", m.filtered, m.cursor)
	}
	if !m.live["b"] {
		t.Error("live set not applied")
	}

	// A pending delete confirmation must not have its index shifted.
	m.confirmDelete = true
	next, _ = m.Update(refreshMsg{items: []listItem{mk("z", "zzz")}})
	if len(next.(model).items) != 3 {
		t.Error("refresh should be skipped while confirming a delete")
	}
}

func TestParsedSearchTextSharedThroughCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"/p","message":{"content":"needle"},"timestamp":"t"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conv, err := transcript.ParseConversationFile(path, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	item := buildItems([]transcript.Conversation{*conv})[0]
	if !strings.Contains(item.searchLower, "needle") || item.searchText != conv.SearchText {
		t.Errorf("item should carry the parsed search text, got %q", item.searchText)
	}
}

func TestFormatAgo(t *testing.T) {
	now := time.Date(2026, 9, 23, 18, 30, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		10 * time.Second:     "now",
		2 * time.Minute:      "2m ago",
		59 * time.Minute:     "59m ago",
		3 * time.Hour:        "3h ago",
		23 * time.Hour:       "23h ago",
		36 * time.Hour:       "1d ago",
		13 * 24 * time.Hour:  "13d ago",
		20 * 24 * time.Hour:  "2w ago",
		59 * 24 * time.Hour:  "8w ago",
		90 * 24 * time.Hour:  "3mo ago",
		364 * 24 * time.Hour: "12mo ago",
		800 * 24 * time.Hour: "2y ago",
	}
	for d, want := range cases {
		ts := now.Add(-d).Format(time.RFC3339)
		if got := transcript.FormatAgo(ts, now); got != want || len(got) > colWhen {
			t.Errorf("formatAgo(-%v) = %q, want %q (max %d wide)", d, got, want, colWhen)
		}
	}
	if transcript.FormatAgo("garbage", now) != "" {
		t.Error("unparseable timestamp should render empty")
	}
}

func TestFormatTokens(t *testing.T) {
	for n, want := range map[int]string{0: "", 950: "950", 12_345: "12k", 281_080: "281k", 1_234_567: "1.2M", 99_900_000: "99.9M", 4_096_900_000: "4.1B"} {
		if got := transcript.FormatTokens(n); got != want || len(got) > colCtx {
			t.Errorf("formatTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRefreshNote(t *testing.T) {
	m := initialModel(nil, "", nil)
	if m.refreshNote() != "" {
		t.Error("no auto-refresh, no note")
	}
	m.reload = func() ([]listItem, error) { return nil, nil }
	m.lastRefresh = time.Now().Add(-20 * time.Second)
	if got := m.refreshNote(); got != " · refreshed just now" {
		t.Errorf("got %q", got)
	}
	m.lastRefresh = time.Now().Add(-3 * time.Minute)
	if got := m.refreshNote(); got != " · refreshed 3m ago" {
		t.Errorf("got %q", got)
	}

	res, _ := m.Update(transcript.RefreshTickMsg{})
	if m = res.(model); m.refreshNote() != " · refreshing…" {
		t.Errorf("in-flight scan should say refreshing, got %q", m.refreshNote())
	}
	res, _ = m.Update(refreshMsg{err: os.ErrPermission})
	if m = res.(model); !strings.HasPrefix(m.refreshNote(), " · refresh failed") {
		t.Errorf("failed scan should say so, got %q", m.refreshNote())
	}
	res, _ = m.Update(refreshMsg{items: []listItem{}})
	if m = res.(model); m.refreshNote() != " · refreshed just now" {
		t.Errorf("successful scan resets the counter, got %q", m.refreshNote())
	}
	m.width, m.height = 160, 30
	if !strings.Contains(m.View(), "refreshed just now") {
		t.Error("note should render in the header")
	}
}

func TestStaleFullScanDoesNotRollBackLiveContent(t *testing.T) {
	scanStart := time.Now()
	old := transcript.Conversation{SessionID: "b", FilePath: "/b", Size: 100, LastTimestamp: "2026-09-25T09:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "hi"}}, ReadAt: scanStart}
	newer := old
	newer.Size, newer.LastTimestamp, newer.ReadAt = 200, "2026-09-25T11:00:00Z", scanStart.Add(time.Second)
	newer.Messages = append(slices.Clone(old.Messages), transcript.Message{Role: "assistant", Text: "needle"})
	a := transcript.Conversation{SessionID: "a", FilePath: "/a", Size: 10, LastTimestamp: "2026-09-25T10:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "a"}}}

	m := initialModel(buildItems([]transcript.Conversation{newer, a}), "needle", nil)
	if len(m.filtered) != 1 || m.filtered[0].conv.SessionID != "b" {
		t.Fatal("setup: b should match the query")
	}
	// A scan that read b before its latest reply lands afterwards.
	res, _ := m.Update(refreshMsg{items: buildItems([]transcript.Conversation{a, old})})
	m = res.(model)
	if len(m.filtered) != 1 || m.filtered[m.cursor].conv.SessionID != "b" || m.filtered[0].conv.Size != 200 {
		t.Errorf("stale scan rolled b back or moved the selection: %+v", m.filtered)
	}
}

func TestRenameRebuildsSearchTextAndDropsCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"t"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conv, _ := transcript.ParseConversationFile(path, time.Time{}, 0)
	m := initialModel(buildItems([]transcript.Conversation{*conv}), "", nil)
	m.renameIndex = 0
	m.renameConversation("Shiny Name")
	if !strings.Contains(m.items[0].conv.SearchLower, "shiny name") || !strings.Contains(m.items[0].searchLower, "shiny name") {
		t.Error("rename should rebuild the conversation's own search text")
	}
	transcript.ParseCacheMu.Lock()
	_, cached := transcript.ParseCache[path]
	transcript.ParseCacheMu.Unlock()
	if cached {
		t.Error("rename should drop the stale parse cache entry")
	}
	// The next live read builds on the renamed conversation and keeps the name searchable.
	c, err := transcript.ParseAppended(&m.items[0].conv)
	if err != nil || !strings.Contains(c.SearchLower, "shiny name") {
		t.Errorf("renamed session lost its name from search: %v", err)
	}
}

func TestExternallyPrunedFileIsShownNotResurrected(t *testing.T) {
	// ccs prune ran in another terminal: the scan reads a smaller file later.
	big := transcript.Conversation{SessionID: "s", FilePath: "/s", Size: 2000, LastTimestamp: "2026-09-25T09:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "x"}}, ReadAt: time.Now()}
	small := big
	small.Size, small.ReadAt = 90, big.ReadAt.Add(time.Minute)
	m := initialModel(buildItems([]transcript.Conversation{big}), "", nil)
	res, _ := m.Update(refreshMsg{items: buildItems([]transcript.Conversation{small})})
	if m = res.(model); m.items[0].conv.Size != 90 {
		t.Errorf("a later read of a shrunk file must win, shown size %d", m.items[0].conv.Size)
	}
}

func TestOwnPruneResetsParseState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"user","cwd":"/p","message":{"content":"keep"},"timestamp":"t1"}` + "\n" +
		`{"type":"file-history-snapshot","snapshot":{"data":"` + strings.Repeat("x", 3000) + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	conv, _ := transcript.ParseConversationFile(path, time.Time{}, 0)
	m := initialModel(buildItems([]transcript.Conversation{*conv}), "", nil)
	m.pruneIndex = 0
	m.pruneConversation()
	if m.errorMsg != "" {
		t.Fatal(m.errorMsg)
	}
	// Claude resumes and appends: the next live read must see it.
	appendTo(t, path, `{"type":"user","cwd":"/p","message":{"content":"after prune"},"timestamp":"t2"}`+"\n")
	c, err := transcript.ParseAppended(&m.items[0].conv)
	if err != nil || len(c.Messages) != 2 {
		t.Errorf("message appended after a prune was missed: %+v, %v", c, err)
	}
}

func TestRecoverWorkerKeepsProcessAlive(t *testing.T) {
	old := transcript.WorkerPanicLog
	transcript.WorkerPanicLog = filepath.Join(t.TempDir(), "panic.log")
	defer func() { transcript.WorkerPanicLog = old; transcript.WorkerPanicked.Store(false) }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer transcript.RecoverWorker()
		var p *transcript.Conversation
		_ = p.Size // nil dereference
	}()
	<-done
	logged, _ := os.ReadFile(transcript.WorkerPanicLog)
	if !transcript.WorkerPanicked.Load() || !strings.Contains(string(logged), "nil pointer") {
		t.Errorf("panic should be recovered and logged, got %q", logged)
	}
	m := initialModel(nil, "", nil)
	m.width, m.height = 200, 30
	if !strings.Contains(m.View(), "internal error") {
		t.Error("header should mention the logged error")
	}
}

func TestActiveModelFromRepliesAndSwitches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	reply := func(model string) string {
		return `{"type":"assistant","message":{"id":"` + model + `","model":"` + model + `","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10}},"timestamp":"2026-09-29T10:00:00Z"}` + "\n"
	}
	sw := func(stdout string) string {
		b, _ := json.Marshal("<local-command-stdout>" + stdout + "</local-command-stdout>")
		return `{"type":"user","cwd":"/p","message":{"content":` + string(b) + `},"timestamp":"2026-09-29T10:01:00Z"}` + "\n"
	}
	body := `{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2026-09-29T09:59:00Z"}` + "\n" + reply("claude-sonnet-5")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := transcript.ParseConversationFile(path, time.Time{}, 0)
	if c.ActiveModel != "claude-sonnet-5" || transcript.ShortModel(*c) != "sonnet 5" {
		t.Errorf("from a reply: %q / %q", c.ActiveModel, transcript.ShortModel(*c))
	}
	// /model after the last reply: that's the active model now.
	appendTo(t, path, sw("Set model to \x1b[1mOpus 5.5 (1M context) (default)\x1b[22m and saved as your default for new sessions"))
	c, _ = transcript.ParseAppended(c)
	if c.ActiveModel != "Opus 5.5 (1M context)" || !c.Model1M || transcript.ShortModel(*c) != "opus 5.5 1M" {
		t.Errorf("after /model: %q 1M=%v short=%q", c.ActiveModel, c.Model1M, transcript.ShortModel(*c))
	}
	if !strings.Contains(strings.Join(sessionStats(*c), "\n"), "switched; last reply claude-sonnet-5") {
		t.Error("preview should say the model was switched since the last reply")
	}
	// A reply from the new model takes over, keeping the known 1M window.
	appendTo(t, path, reply("claude-opus-5-5"))
	c, _ = transcript.ParseAppended(c)
	if c.ActiveModel != "claude-opus-5-5" || transcript.ShortModel(*c) != "opus 5.5 1M" || transcript.ContextWindow(c.ActiveModel, c.PeakContext) != 1_000_000 {
		t.Errorf("after the next reply: %q short=%q", c.ActiveModel, transcript.ShortModel(*c))
	}
	// The backtick form some versions log.
	appendTo(t, path, sw("Set model to `Haiku 4.5` and saved as your default for new sessions"))
	c, _ = transcript.ParseAppended(c)
	if c.ActiveModel != "Haiku 4.5" || c.Model1M || transcript.ShortModel(*c) != "haiku 4.5" {
		t.Errorf("backtick form: %q 1M=%v short=%q", c.ActiveModel, c.Model1M, transcript.ShortModel(*c))
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"needs truncation", "hello world", 8, "hello..."},
		{"with newlines", "hello\nworld", 20, "hello world"},
		{"multiple spaces", "hello   world", 20, "hello world"},
		{"multibyte fits", "世界", 5, "世界"},
		{"multibyte truncates on rune boundary", "世界世界世界", 5, "世界..."},
		{"tiny maxLen no ellipsis", "世界世界", 2, "世界"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncate(tt.input, tt.maxLen)
			if result != tt.expected {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{2048, "2KB"},
		{75 * 1 << 20, "75MB"},
		{1 << 30, "1.0GB"},
		{3*(1<<30) + (1 << 29), "3.5GB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.n)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
		if len(got) > 6 {
			t.Errorf("formatBytes(%d) = %q exceeds the 6-wide SIZE column", tt.n, got)
		}
	}
}

func TestFormatTimestamp(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty", "", ""},
		{"invalid short", "abc", "abc"},
		{"valid RFC3339", "2024-01-15T10:30:00Z", "2024-01-15"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatTimestamp(tt.input)
			if tt.name == "valid RFC3339" {
				// Just check it starts with the date (time zone varies)
				if !strings.HasPrefix(result, "2024-01-1") {
					t.Errorf("formatTimestamp(%q) = %q, want prefix '2024-01-1'", tt.input, result)
				}
			} else if result != tt.expected {
				t.Errorf("formatTimestamp(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestExtractText(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple string", `"hello world"`, "hello world"},
		{"empty", ``, ""},
		{"array with text", `[{"type":"text","text":"hello"},{"type":"text","text":"world"}]`, "hello world"},
		{"array with non-text", `[{"type":"image","text":"ignore"},{"type":"text","text":"hello"}]`, "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractText(json.RawMessage(tt.input))
			if result != tt.expected {
				t.Errorf("extractText(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestBuildItems(t *testing.T) {
	conversations := []Conversation{
		{
			SessionID:      "session1",
			Cwd:            "/home/user/project1",
			FirstTimestamp: "2024-01-15T10:00:00Z",
			LastTimestamp:  "2024-01-15T10:03:00Z",
			Messages: []Message{
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
			Messages: []Message{
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
	conversations := []Conversation{
		{
			SessionID:      "session1",
			Cwd:            "/home/user/project1",
			FirstTimestamp: "2024-01-15T10:00:00Z",
			LastTimestamp:  "2024-01-15T10:00:00Z",
			Messages: []Message{
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
	conversations := []Conversation{
		{
			SessionID:      "session1",
			Cwd:            "/home/user/my-project",
			FirstTimestamp: "2024-01-15T10:00:00Z",
			LastTimestamp:  "2024-01-15T10:00:00Z",
			Messages: []Message{
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

func TestParseConversationFile(t *testing.T) {
	// Create a temp file with test conversation
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test-session.jsonl")

	content := `{"type":"user","cwd":"/test/project","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}
{"type":"assistant","message":{"content":"hi there"},"timestamp":"2024-01-15T10:01:00Z"}
{"type":"user","message":{"content":"goodbye"},"timestamp":"2024-01-15T10:02:00Z"}
`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	conv, err := parseConversationFile(testFile, time.Time{}, 0) // No cutoff, no size limit
	if err != nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}

	if conv == nil {
		t.Fatal("parseConversationFile returned nil")
	}

	if conv.SessionID != "test-session" {
		t.Errorf("SessionID = %q, want %q", conv.SessionID, "test-session")
	}

	if conv.Cwd != "/test/project" {
		t.Errorf("Cwd = %q, want %q", conv.Cwd, "/test/project")
	}

	if len(conv.Messages) != 3 {
		t.Errorf("len(Messages) = %d, want 3", len(conv.Messages))
	}

	if conv.FirstTimestamp != "2024-01-15T10:00:00Z" {
		t.Errorf("FirstTimestamp = %q, want %q", conv.FirstTimestamp, "2024-01-15T10:00:00Z")
	}

	if conv.LastTimestamp != "2024-01-15T10:02:00Z" {
		t.Errorf("LastTimestamp = %q, want %q", conv.LastTimestamp, "2024-01-15T10:02:00Z")
	}
}

func TestParseConversationFileTitle(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "titled-session.jsonl")

	// ai-title appears first, custom-title (user-set) must win
	content := `{"type":"ai-title","aiTitle":"auto generated name","sessionId":"titled-session"}
{"type":"user","cwd":"/test/project","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}
{"type":"custom-title","customTitle":"my custom name","sessionId":"titled-session"}
`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	conv, err := parseConversationFile(testFile, time.Time{}, 0)
	if err != nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}
	if conv == nil {
		t.Fatal("parseConversationFile returned nil")
	}
	if conv.Title != "my custom name" {
		t.Errorf("Title = %q, want %q", conv.Title, "my custom name")
	}
	if !conv.IsCustomTitle {
		t.Error("IsCustomTitle should be true when a custom-title is present")
	}
	if got := getTopic(*conv); got != "my custom name" {
		t.Errorf("getTopic = %q, want title %q", got, "my custom name")
	}
}

func TestParseConversationFileAiTitleNotCustom(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "ai-titled.jsonl")
	content := `{"type":"ai-title","aiTitle":"auto name","sessionId":"ai-titled"}
{"type":"user","cwd":"/test","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}
`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	conv, err := parseConversationFile(testFile, time.Time{}, 0)
	if err != nil || conv == nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}
	if conv.Title != "auto name" {
		t.Errorf("Title = %q, want %q", conv.Title, "auto name")
	}
	if conv.IsCustomTitle {
		t.Error("IsCustomTitle must be false for an ai-title (auto-generated)")
	}
}

func TestGetTopicFallsBackToFirstMessage(t *testing.T) {
	conv := Conversation{Messages: []Message{{Role: "user", Text: "first msg"}}}
	if got := getTopic(conv); got != "first msg" {
		t.Errorf("getTopic = %q, want %q", got, "first msg")
	}
}

func TestParseConversationFileSkipsAgentFiles(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "agent-test.jsonl")

	content := `{"type":"user","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	conv, err := parseConversationFile(testFile, time.Time{}, 0) // No cutoff, no size limit
	if err != nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}

	if conv != nil {
		t.Error("parseConversationFile should return nil for agent- prefixed files")
	}
}

func TestParseConversationFileEmptyMessages(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "empty-session.jsonl")

	content := `{"type":"summary","message":{"content":"summary only"}}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	conv, err := parseConversationFile(testFile, time.Time{}, 0) // No cutoff, no size limit
	if err != nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}

	if conv != nil {
		t.Error("parseConversationFile should return nil for files with no user/assistant messages")
	}
}

func TestParseConversationFileSkipsOldFiles(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "old-session.jsonl")

	content := `{"type":"user","cwd":"/test","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Set file mtime to 60 days ago
	oldTime := time.Now().AddDate(0, 0, -60)
	if err := os.Chtimes(testFile, oldTime, oldTime); err != nil {
		t.Fatalf("failed to set file mtime: %v", err)
	}

	// Cutoff is 30 days ago - file should be skipped
	cutoff := time.Now().AddDate(0, 0, -30)
	conv, err := parseConversationFile(testFile, cutoff, 0)
	if err != nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}

	if conv != nil {
		t.Error("parseConversationFile should return nil for files older than cutoff")
	}
}

func TestParseConversationFileIncludesRecentFiles(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "recent-session.jsonl")

	content := `{"type":"user","cwd":"/test","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// File mtime is now (recent) - cutoff is 30 days ago
	cutoff := time.Now().AddDate(0, 0, -30)
	conv, err := parseConversationFile(testFile, cutoff, 0)
	if err != nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}

	if conv == nil {
		t.Error("parseConversationFile should include files newer than cutoff")
	}
}

func TestParseConversationFileSkipsLargeFiles(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "large-session.jsonl")

	content := `{"type":"user","cwd":"/test","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// maxSize of 10 bytes - file should be skipped
	conv, err := parseConversationFile(testFile, time.Time{}, 10)
	if err != nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}

	if conv != nil {
		t.Error("parseConversationFile should return nil for files larger than maxSize")
	}
}

func TestGetTopic(t *testing.T) {
	tests := []struct {
		name     string
		conv     Conversation
		expected string
	}{
		{
			name: "with user message",
			conv: Conversation{
				SessionID: "test-123",
				Messages: []Message{
					{Role: "user", Text: "Hello world"},
					{Role: "assistant", Text: "Hi there"},
				},
			},
			expected: "Hello world",
		},
		{
			name: "no user message",
			conv: Conversation{
				SessionID: "test-456",
				Messages: []Message{
					{Role: "assistant", Text: "Hi there"},
				},
			},
			expected: "test-456",
		},
		{
			name: "empty messages",
			conv: Conversation{
				SessionID: "test-789",
				Messages:  []Message{},
			},
			expected: "test-789",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getTopic(tt.conv)
			if result != tt.expected {
				t.Errorf("getTopic() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestGetProjectsDir(t *testing.T) {
	dir := getProjectsDir()
	if !strings.Contains(dir, ".claude") || !strings.Contains(dir, "projects") {
		t.Errorf("getProjectsDir should return path containing .claude/projects, got %s", dir)
	}
}

func TestGetConversations(t *testing.T) {
	// Create temp directory with test conversations
	tmpDir := t.TempDir()

	// Create test files
	file1 := filepath.Join(tmpDir, "session-1.jsonl")
	file2 := filepath.Join(tmpDir, "session-2.jsonl")
	agentFile := filepath.Join(tmpDir, "agent-test.jsonl")

	content1 := `{"type":"user","cwd":"/test1","message":{"content":"first"},"timestamp":"2024-01-15T10:00:00Z"}`
	content2 := `{"type":"user","cwd":"/test2","message":{"content":"second"},"timestamp":"2024-01-15T11:00:00Z"}`

	if err := os.WriteFile(file1, []byte(content1), 0644); err != nil {
		t.Fatalf("failed to write file1: %v", err)
	}
	if err := os.WriteFile(file2, []byte(content2), 0644); err != nil {
		t.Fatalf("failed to write file2: %v", err)
	}
	if err := os.WriteFile(agentFile, []byte(content1), 0644); err != nil {
		t.Fatalf("failed to write agent file: %v", err)
	}

	// Save and override getProjectsDir
	oldGetProjectsDir := getProjectsDir
	getProjectsDir = func() string { return tmpDir }
	defer func() { getProjectsDir = oldGetProjectsDir }()

	// Get conversations
	convs, err := getConversations(time.Time{}, 0, nil)
	if err != nil {
		t.Fatalf("getConversations failed: %v", err)
	}

	// Should have 2 conversations (agent file should be skipped)
	if len(convs) != 2 {
		t.Errorf("expected 2 conversations, got %d", len(convs))
	}

	// Verify sorted by timestamp (newest first)
	if convs[0].SessionID != "session-2" {
		t.Errorf("first conversation should be session-2, got %s", convs[0].SessionID)
	}
	if convs[1].SessionID != "session-1" {
		t.Errorf("second conversation should be session-1, got %s", convs[1].SessionID)
	}
}

func TestGetConversationsSkipsSubagents(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a regular session file
	sessionFile := filepath.Join(tmpDir, "session-1.jsonl")
	content := `{"type":"user","cwd":"/test","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(sessionFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write session file: %v", err)
	}

	// Create a subagents directory with a session file inside
	subagentsDir := filepath.Join(tmpDir, "abc123", "subagents")
	if err := os.MkdirAll(subagentsDir, 0755); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}
	subagentFile := filepath.Join(subagentsDir, "agent-test.jsonl")
	if err := os.WriteFile(subagentFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write subagent file: %v", err)
	}
	// Also put a non-agent-prefixed file in subagents to confirm the directory is skipped entirely
	sneakyFile := filepath.Join(subagentsDir, "sneaky-session.jsonl")
	if err := os.WriteFile(sneakyFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write sneaky file: %v", err)
	}

	oldGetProjectsDir := getProjectsDir
	getProjectsDir = func() string { return tmpDir }
	defer func() { getProjectsDir = oldGetProjectsDir }()

	convs, err := getConversations(time.Time{}, 0, nil)
	if err != nil {
		t.Fatalf("getConversations failed: %v", err)
	}

	// Should only have 1 conversation (subagents dir should be skipped entirely)
	if len(convs) != 1 {
		t.Errorf("expected 1 conversation, got %d", len(convs))
		for _, c := range convs {
			t.Logf("  found: %s (file: %s)", c.SessionID, c.FilePath)
		}
	}

	if len(convs) > 0 && convs[0].SessionID != "session-1" {
		t.Errorf("expected session-1, got %s", convs[0].SessionID)
	}
}

func TestRefreshKeepsSelectionAndFilter(t *testing.T) {
	mk := func(id, text string) listItem {
		return buildItems([]Conversation{{SessionID: id, Messages: []Message{{Role: "user", Text: text}}}})[0]
	}
	m := initialModel([]listItem{mk("a", "apple"), mk("b", "banana")}, "an", nil)
	m.cursor = 0 // "b" is the only match
	next, cmd := m.Update(refreshMsg{
		items: []listItem{mk("c", "mango"), mk("a", "apple"), mk("b", "banana")},
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

func TestParseConversationFileReusesUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2024-01-01T00:00:00Z"}` + "\n"
	os.WriteFile(path, []byte(line), 0o644)
	a, _ := parseConversationFile(path, time.Time{}, 0)
	b, _ := parseConversationFile(path, time.Time{}, 0)
	if &a.Messages[0] != &b.Messages[0] || !b.readAt.After(a.readAt) && !b.readAt.Equal(a.readAt) {
		t.Error("unchanged file should come from the cache (with a fresh read time)")
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(line)
	f.Close()
	c, _ := parseConversationFile(path, time.Time{}, 0)
	if c == a || len(c.Messages) != 2 {
		t.Error("appended file should be reparsed")
	}
}

func TestGetTopicSkipsHarnessText(t *testing.T) {
	msgs := func(texts ...string) Conversation {
		c := Conversation{SessionID: "sid"}
		for _, x := range texts {
			c.Messages = append(c.Messages, Message{Role: "user", Text: x})
		}
		return c
	}
	cases := []struct {
		want string
		conv Conversation
	}{
		{"/model", msgs("<command-name>/model</command-name> <command-message>model</command-message>", "<local-command-stdout>ok</local-command-stdout>")},
		{"real prompt", msgs("<task-notification>done</task-notification>", "real prompt")},
		{"sid", msgs("<local-command-stdout>ok</local-command-stdout>")},
		{"/grill", msgs("<command-message>grill</command-message>\n<command-name>/grill</command-name>")},
		{"! glogin -S", msgs("<bash-input>glogin -S</bash-input>")},
		{"plain first", msgs("plain first", "<b>html</b>")},
		{"why <command-name>x</command-name>", msgs("why <command-name>x</command-name>")},
	}
	for _, c := range cases {
		if got := getTopic(c.conv); got != c.want {
			t.Errorf("getTopic = %q, want %q", got, c.want)
		}
	}
}

func TestParseSkipsMetaAndDetectsSpawned(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	human := write("h.jsonl", `{"type":"user","isMeta":true,"entrypoint":"cli","cwd":"/p","message":{"content":"<local-command-caveat>x</local-command-caveat>"},"timestamp":"t1"}
{"type":"user","entrypoint":"cli","cwd":"/p","message":{"content":"hello"},"timestamp":"t2"}
`)
	sdk := write("s.jsonl", `{"type":"user","entrypoint":"sdk-cli","cwd":"/p","message":{"content":"do it"},"timestamp":"t"}
`)
	team := write("t.jsonl", `{"type":"user","entrypoint":"cli","teamName":"session-x","cwd":"/p","message":{"content":"<teammate-message>go</teammate-message>"},"timestamp":"t"}
`)
	c, _ := parseConversationFile(human, time.Time{}, 0)
	if len(c.Messages) != 1 || c.Messages[0].Text != "hello" || c.Spawned {
		t.Errorf("meta line should be dropped and cli is not spawned: %+v", c)
	}
	for _, p := range []string{sdk, team} {
		if c, _ := parseConversationFile(p, time.Time{}, 0); !c.Spawned {
			t.Errorf("%s should be marked spawned", p)
		}
	}
}

func TestGetConversationsMissingDirErrors(t *testing.T) {
	old := getProjectsDir
	getProjectsDir = func() string { return filepath.Join(t.TempDir(), "gone") }
	defer func() { getProjectsDir = old }()
	if _, err := getConversations(time.Time{}, 0, nil); err == nil {
		t.Error("missing projects dir should be an error, not an empty list")
	}
}

func TestParsedSearchTextSharedThroughCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"/p","message":{"content":"needle"},"timestamp":"t"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conv, err := parseConversationFile(path, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	item := buildItems([]Conversation{*conv})[0]
	if !strings.Contains(item.searchLower, "needle") || item.searchText != conv.searchText {
		t.Errorf("item should carry the parsed search text, got %q", item.searchText)
	}
}

func TestMetaOnlySessionKeepsCwdAndOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"user","isMeta":true,"entrypoint":"sdk-cli","cwd":"/real","message":{"content":"<local-command-caveat>x</local-command-caveat>"},"timestamp":"t1"}
{"type":"user","cwd":"/real","message":{"content":"<command-name>/model</command-name>"},"timestamp":"t2"}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := parseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	if c.Cwd != "/real" || !c.Spawned || len(c.Messages) != 1 {
		t.Errorf("cwd/origin should come from the meta line, messages should skip it: %+v", c)
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
		if got := formatAgo(ts, now); got != want || len(got) > colWhen {
			t.Errorf("formatAgo(-%v) = %q, want %q (max %d wide)", d, got, want, colWhen)
		}
	}
	if formatAgo("garbage", now) != "" {
		t.Error("unparseable timestamp should render empty")
	}
}

func TestFormatTokens(t *testing.T) {
	for n, want := range map[int]string{0: "", 950: "950", 12_345: "12k", 281_080: "281k", 1_234_567: "1.2M", 99_900_000: "99.9M", 4_096_900_000: "4.1B"} {
		if got := formatTokens(n); got != want || len(got) > colCtx {
			t.Errorf("formatTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestParseContextTokensFromLastReply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"t1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"a"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":1000,"cache_read_input_tokens":500,"output_tokens":9}},"timestamp":"t2"}
{"type":"assistant","message":{"content":[{"type":"text","text":"b"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":200,"cache_read_input_tokens":1500,"output_tokens":9}},"timestamp":"t3"}
{"type":"assistant","message":{"content":[{"type":"text","text":"API Error"}],"usage":{"input_tokens":0,"output_tokens":0}},"timestamp":"t4"}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := parseConversationFile(path, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.ContextTokens != 1703 {
		t.Errorf("ContextTokens = %d, want 1703 (last non-zero reply)", c.ContextTokens)
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

	res, _ := m.Update(refreshTickMsg{})
	if m = res.(model); m.refreshNote() != " · refreshing…" {
		t.Errorf("in-flight scan should say refreshing, got %q", m.refreshNote())
	}
	res, _ = m.Update(refreshMsg{err: os.ErrNotExist})
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

func TestParseAppendedReadsOnlyNewLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	first := `{"type":"user","cwd":"/p","message":{"content":"hello"},"timestamp":"2026-09-25T10:00:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	prev, err := parseConversationFile(path, time.Time{}, 0)
	if err != nil || prev == nil {
		t.Fatal(prev, err)
	}
	if same, _ := parseAppended(prev); same != prev {
		t.Error("unchanged file should return prev as is")
	}

	appendTo(t, path, `{"type":"assistant","message":{"content":[{"type":"text","text":"needle reply"}],"usage":{"input_tokens":5,"cache_read_input_tokens":995}},"timestamp":"2026-09-25T10:01:00Z"}`+"\n")
	c, err := parseAppended(prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 2 || c.ContextTokens != 1000 || c.LastTimestamp != "2026-09-25T10:01:00Z" || c.Cwd != "/p" {
		t.Errorf("appended parse wrong: %+v", c)
	}
	if !strings.Contains(c.searchLower, "needle reply") || !strings.Contains(c.searchLower, "hello") {
		t.Error("search text should cover old and new messages")
	}
	if len(prev.Messages) != 1 {
		t.Error("prev must not be modified")
	}
	full, _ := parseConversationUncached(path, mustStat(t, path))
	if full.searchText == "" || len(full.Messages) != len(c.Messages) || full.parsedBytes != c.parsedBytes {
		t.Errorf("incremental and full parse disagree: %d/%d msgs, %d/%d bytes", len(c.Messages), len(full.Messages), c.parsedBytes, full.parsedBytes)
	}

	// A title arriving later rebuilds the search text.
	appendTo(t, path, `{"type":"custom-title","customTitle":"Renamed"}`+"\n")
	c2, _ := parseAppended(c)
	if c2.Title != "Renamed" || !strings.Contains(c2.searchLower, "renamed") {
		t.Errorf("title update missed: %q", c2.Title)
	}

	// A shrunk file (e.g. pruned) is reparsed in full.
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	c3, _ := parseAppended(c2)
	if c3 == nil || len(c3.Messages) != 1 {
		t.Errorf("shrunk file should fully reparse, got %+v", c3)
	}
}

func TestParseAppendedUnterminatedTailNoDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	// Ends mid-line: the last record is parsed but the next parse must be full.
	body := `{"type":"user","cwd":"/p","message":{"content":"a"},"timestamp":"t1"}` + "\n" +
		`{"type":"user","cwd":"/p","message":{"content":"b"},"timestamp":"t2"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	prev, _ := parseConversationFile(path, time.Time{}, 0)
	appendTo(t, path, "\n"+`{"type":"user","cwd":"/p","message":{"content":"c"},"timestamp":"t3"}`+"\n")
	c, _ := parseAppended(prev)
	if len(c.Messages) != 3 {
		t.Errorf("want a, b, c exactly once, got %d messages", len(c.Messages))
	}
}

func TestStaleFullScanDoesNotRollBackLiveContent(t *testing.T) {
	scanStart := time.Now()
	old := Conversation{SessionID: "b", FilePath: "/b", Size: 100, LastTimestamp: "2026-09-25T09:00:00Z", Messages: []Message{{Role: "user", Text: "hi"}}, readAt: scanStart}
	newer := old
	newer.Size, newer.LastTimestamp, newer.readAt = 200, "2026-09-25T11:00:00Z", scanStart.Add(time.Second)
	newer.Messages = append(slices.Clone(old.Messages), Message{Role: "assistant", Text: "needle"})
	a := Conversation{SessionID: "a", FilePath: "/a", Size: 10, LastTimestamp: "2026-09-25T10:00:00Z", Messages: []Message{{Role: "user", Text: "a"}}}

	m := initialModel(buildItems([]Conversation{newer, a}), "needle", nil)
	if len(m.filtered) != 1 || m.filtered[0].conv.SessionID != "b" {
		t.Fatal("setup: b should match the query")
	}
	// A scan that read b before its latest reply lands afterwards.
	res, _ := m.Update(refreshMsg{items: buildItems([]Conversation{a, old})})
	m = res.(model)
	if len(m.filtered) != 1 || m.filtered[m.cursor].conv.SessionID != "b" || m.filtered[0].conv.Size != 200 {
		t.Errorf("stale scan rolled b back or moved the selection: %+v", m.filtered)
	}
}

func TestParseAppendedResumesAfterLineCaughtMidWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := func(text string) string {
		return `{"type":"user","cwd":"/p","message":{"content":"` + text + `"},"timestamp":"t"}` + "\n"
	}
	partial := line("beta")[:20] // a write in progress: not valid JSON yet
	if err := os.WriteFile(path, []byte(line("alpha")+partial), 0o644); err != nil {
		t.Fatal(err)
	}
	prev, _ := parseConversationFile(path, time.Time{}, 0)
	if prev.tailApplied || prev.parsedBytes == prev.Size {
		t.Fatal("setup: partial tail should be read but not applied")
	}
	appendTo(t, path, line("beta")[20:])
	c, err := parseAppended(prev)
	if err != nil || len(c.Messages) != 2 || c.Messages[1].Text != "beta" {
		t.Fatalf("should resume at the partial line, got %+v, %v", c, err)
	}
	full, _ := parseConversationUncached(path, mustStat(t, path))
	if c.searchText != full.searchText {
		t.Errorf("incremental search text differs from a full parse:\n%q\n%q", c.searchText, full.searchText)
	}
}

func TestParseAppendedSearchTextMatchesFullParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	// Starts with no cwd; a later meta line supplies it. Messages arrive over
	// two ticks, so a phrase spans them.
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"content":"alpha"},"timestamp":"2026-09-25T10:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := parseConversationFile(path, time.Time{}, 0)
	appendTo(t, path, `{"type":"user","isMeta":true,"cwd":"/newproject","message":{"content":"x"},"timestamp":"2026-09-25T10:01:00Z"}`+"\n")
	c, _ = parseAppended(c)
	appendTo(t, path, `{"type":"assistant","message":{"content":[{"type":"text","text":"beta"}]},"timestamp":"2026-09-25T10:02:00Z"}`+"\n")
	c, _ = parseAppended(c)
	full, _ := parseConversationUncached(path, mustStat(t, path))
	if c.searchText != full.searchText || !strings.Contains(c.searchLower, "/newproject") {
		t.Errorf("drift from full parse:\n%q\n%q", c.searchText, full.searchText)
	}
}

func TestRenameRebuildsSearchTextAndDropsCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"t"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conv, _ := parseConversationFile(path, time.Time{}, 0)
	m := initialModel(buildItems([]Conversation{*conv}), "", nil)
	m.renameIndex = 0
	m.renameConversation("Shiny Name")
	if !strings.Contains(m.items[0].conv.searchLower, "shiny name") || !strings.Contains(m.items[0].searchLower, "shiny name") {
		t.Error("rename should rebuild the conversation's own search text")
	}
	parseCacheMu.Lock()
	_, cached := parseCache[path]
	parseCacheMu.Unlock()
	if cached {
		t.Error("rename should drop the stale parse cache entry")
	}
	// The next live read builds on the renamed conversation and keeps the name searchable.
	c, err := parseAppended(&m.items[0].conv)
	if err != nil || !strings.Contains(c.searchLower, "shiny name") {
		t.Errorf("renamed session lost its name from search: %v", err)
	}
}

func TestExternallyPrunedFileIsShownNotResurrected(t *testing.T) {
	// ccs prune ran in another terminal: the scan reads a smaller file later.
	big := Conversation{SessionID: "s", FilePath: "/s", Size: 2000, LastTimestamp: "2026-09-25T09:00:00Z", Messages: []Message{{Role: "user", Text: "x"}}, readAt: time.Now()}
	small := big
	small.Size, small.readAt = 90, big.readAt.Add(time.Minute)
	m := initialModel(buildItems([]Conversation{big}), "", nil)
	res, _ := m.Update(refreshMsg{items: buildItems([]Conversation{small})})
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
	conv, _ := parseConversationFile(path, time.Time{}, 0)
	m := initialModel(buildItems([]Conversation{*conv}), "", nil)
	m.pruneIndex = 0
	m.pruneConversation()
	if m.errorMsg != "" {
		t.Fatal(m.errorMsg)
	}
	// Claude resumes and appends: the next live read must see it.
	appendTo(t, path, `{"type":"user","cwd":"/p","message":{"content":"after prune"},"timestamp":"t2"}`+"\n")
	c, err := parseAppended(&m.items[0].conv)
	if err != nil || len(c.Messages) != 2 {
		t.Errorf("message appended after a prune was missed: %+v, %v", c, err)
	}
}

func TestAppendedSearchTextEqualsFullParseAcrossManyTicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"/p","message":{"content":"Alpha"},"timestamp":"2026-09-25T10:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := parseConversationFile(path, time.Time{}, 0)
	for i := 0; i < 5; i++ {
		appendTo(t, path, fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":"Reply %d İ"}]},"timestamp":"2026-09-25T10:0%d:00Z"}`, i, i+1)+"\n")
		appendTo(t, path, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]},"timestamp":"x"}`+"\n")
		c, _ = parseAppended(c)
	}
	full, _ := parseConversationUncached(path, mustStat(t, path))
	if c.searchText != full.searchText || c.searchLower != full.searchLower {
		t.Errorf("drift after appends:\n%q\n%q", c.searchLower, full.searchLower)
	}
}

func TestParseAppendedContinuesFromLateRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := func(s string) string {
		return `{"type":"user","cwd":"/p","message":{"content":"` + s + `"},"timestamp":"t"}` + "\n"
	}
	if err := os.WriteFile(path, []byte(line("a")), 0o644); err != nil {
		t.Fatal(err)
	}
	listCopy, _ := parseConversationFile(path, time.Time{}, 0)
	appendTo(t, path, line("b"))
	late, _ := parseAppended(listCopy) // missed the deadline: result only in the cache
	appendTo(t, path, line("c"))
	c, err := parseAppended(listCopy) // next tick, still from the stale list copy
	if err != nil || len(c.Messages) != 3 || c.parsedBytes <= late.parsedBytes {
		t.Errorf("should continue from the late read: %d messages, %v", len(c.Messages), err)
	}
}

func TestCachedEmptyFileDoesNotCrash(t *testing.T) {
	// A transcript with no messages yet (e.g. a claude opened but unused) is
	// cached as "no conversation"; reading it from the cache must not panic.
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"permission-mode","mode":"default"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if c, err := parseConversationFile(path, time.Time{}, 0); c != nil || err != nil {
			t.Fatalf("pass %d: want no conversation, got %v %v", i, c, err)
		}
	}
	// And the live tick's cache lookup for the same path is safe too.
	prev := &Conversation{SessionID: "empty", FilePath: path, Size: 1}
	if _, err := parseAppended(prev); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverWorkerKeepsProcessAlive(t *testing.T) {
	old := workerPanicLog
	workerPanicLog = filepath.Join(t.TempDir(), "panic.log")
	defer func() { workerPanicLog = old; workerPanicked.Store(false) }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverWorker()
		var p *Conversation
		_ = p.Size // nil dereference
	}()
	<-done
	logged, _ := os.ReadFile(workerPanicLog)
	if !workerPanicked.Load() || !strings.Contains(string(logged), "nil pointer") {
		t.Errorf("panic should be recovered and logged, got %q", logged)
	}
	m := initialModel(nil, "", nil)
	m.width, m.height = 200, 30
	if !strings.Contains(m.View(), "internal error") {
		t.Error("header should mention the logged error")
	}
}

func TestParseUsageModelAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	reply := func(id string, in, c5, c1, read, out int) string {
		return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":"claude-opus-5-5","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}},"timestamp":"2026-09-29T10:00:00Z"}`,
			id, in, c5+c1, read, out, c5, c1) + "\n"
	}
	body := `{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2026-09-29T09:59:00Z"}` + "\n" +
		reply("m1", 10, 100, 1000, 0, 50) +
		reply("m1", 10, 100, 1000, 0, 50) + // same reply, second content block: counted once
		reply("m2", 5, 0, 200, 300_000, 70) +
		`{"type":"assistant","isApiErrorMessage":true,"apiErrorStatus":429,"error":"rate_limit","message":{"id":"e1","model":"<synthetic>","content":[{"type":"text","text":"API Error"}],"usage":{"input_tokens":0,"output_tokens":0}},"timestamp":"2026-09-29T10:05:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := parseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	want := tokenUsage{Input: 15, Cache5m: 100, Cache1h: 1200, CacheRead: 300_000, Output: 120}
	if c.Usage != want {
		t.Errorf("usage = %+v, want %+v", c.Usage, want)
	}
	if c.Model != "claude-opus-5-5" || c.PeakContext != 300_205 || c.LastError != "429 rate_limit" {
		t.Errorf("model=%q peak=%d err=%q", c.Model, c.PeakContext, c.LastError)
	}
	// A successful reply afterwards clears the error.
	appendTo(t, path, reply("m3", 1, 0, 0, 300_300, 5))
	c2, _ := parseAppended(c)
	if c2.LastError != "" {
		t.Errorf("error should clear after a successful reply, got %q", c2.LastError)
	}
}

func TestContextWindowAndColour(t *testing.T) {
	if contextWindow("claude-haiku-4-5", 0) != 200_000 || contextWindow("claude-opus-5-5", 0) != 1_000_000 || contextWindow("", 250_000) != 1_000_000 {
		t.Error("context window inference wrong")
	}
	for _, c := range []struct {
		size, window int
		code         string
		flash        bool
	}{
		{50_000, 200_000, "32", false}, {110_000, 200_000, "33", false}, {140_000, 200_000, "38;5;208", false},
		{160_000, 200_000, "31", false}, {190_000, 200_000, "1;31", true}, {190_000, 1_000_000, "33", false},
		{700_000, 1_000_000, "1;31", true},
	} {
		if code, flash := ctxColour(c.size, c.window); code != c.code || flash != c.flash {
			t.Errorf("ctxColour(%d, %d) = %q %v, want %q %v", c.size, c.window, code, flash, c.code, c.flash)
		}
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
	c, _ := parseConversationFile(path, time.Time{}, 0)
	if c.ActiveModel != "claude-sonnet-5" || shortModel(*c) != "sonnet 5" {
		t.Errorf("from a reply: %q / %q", c.ActiveModel, shortModel(*c))
	}
	// /model after the last reply: that's the active model now.
	appendTo(t, path, sw("Set model to \x1b[1mOpus 5.5 (1M context) (default)\x1b[22m and saved as your default for new sessions"))
	c, _ = parseAppended(c)
	if c.ActiveModel != "Opus 5.5 (1M context)" || !c.Model1M || shortModel(*c) != "opus 5.5 1M" {
		t.Errorf("after /model: %q 1M=%v short=%q", c.ActiveModel, c.Model1M, shortModel(*c))
	}
	if !strings.Contains(strings.Join(sessionStats(*c), "\n"), "switched; last reply claude-sonnet-5") {
		t.Error("preview should say the model was switched since the last reply")
	}
	// A reply from the new model takes over, keeping the known 1M window.
	appendTo(t, path, reply("claude-opus-5-5"))
	c, _ = parseAppended(c)
	if c.ActiveModel != "claude-opus-5-5" || shortModel(*c) != "opus 5.5 1M" || contextWindow(c.ActiveModel, c.PeakContext) != 1_000_000 {
		t.Errorf("after the next reply: %q short=%q", c.ActiveModel, shortModel(*c))
	}
	// The backtick form some versions log.
	appendTo(t, path, sw("Set model to `Haiku 4.5` and saved as your default for new sessions"))
	c, _ = parseAppended(c)
	if c.ActiveModel != "Haiku 4.5" || c.Model1M || shortModel(*c) != "haiku 4.5" {
		t.Errorf("backtick form: %q 1M=%v short=%q", c.ActiveModel, c.Model1M, shortModel(*c))
	}
}

func TestShortModelNames(t *testing.T) {
	for name, want := range map[string]string{
		"claude-opus-5-5": "opus 5.5", "claude-haiku-4-5-20251001": "haiku 4.5", "claude-fable-5-1": "fable 5.1",
		"Fable 5": "fable 5", "Opus 4.8 (1M context)": "opus 4.8", "something-else": "something-else",
	} {
		if got := shortModel(Conversation{ActiveModel: name}); got != want {
			t.Errorf("shortModel(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestMidTurnAndPrefixedPeerMessagesParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	wrap := "<cross-session-message from-name=\"the user, via ccs\">\n" + ccsNote + "check CI\n</cross-session-message>"
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	lines := []string{
		`{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2026-09-30T10:00:00Z"}`,
		// idle: a meta user line, with Claude Code's prefix
		`{"type":"user","isMeta":true,"message":{"content":` + j("Another Claude session sent a message:\n"+wrap) + `},"timestamp":"2026-09-30T10:01:00Z"}`,
		// busy: queued, then attached mid-turn
		`{"type":"queue-operation","operation":"enqueue","content":` + j(strings.Replace(wrap, "check CI", "and the logs", 1)) + `,"timestamp":"2026-09-30T10:02:00Z"}`,
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":` + j(strings.Replace(wrap, "check CI", "and the logs", 1)) + `,"commandMode":"prompt","isMeta":true},"timestamp":"2026-09-30T10:02:05Z"}`,
		// the user typing mid-turn
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"also bump node","commandMode":"prompt"},"timestamp":"2026-09-30T10:02:06Z"}`,
		// other attachments are ignored
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"<task-notification>x</task-notification>","commandMode":"task-notification"},"timestamp":"2026-09-30T10:02:07Z"}`,
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	c, _ := parseConversationFile(path, time.Time{}, 0)
	var got []string
	for _, msg := range c.Messages {
		body := msg.Text
		if _, b, ok := peerParts(body); ok {
			body = "peer:" + b
		}
		got = append(got, body)
	}
	want := []string{"hi", "peer:check CI", "peer:and the logs", "also bump node"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("messages = %q, want %q", got, want)
	}
	if len(c.peerQueued) != 1 {
		t.Errorf("the enqueue should be recorded, got %d", len(c.peerQueued))
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
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
	stripped := strings.NewReplacer("\033[43;30m", "", "\033[0m", "").Replace(got)
	if stripped != "héllo wörld héllo" {
		t.Errorf("highlight altered visible text: %q", stripped)
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
	if got := getTopic(*conv); got != "my custom name" {
		t.Errorf("getTopic = %q, want title %q", got, "my custom name")
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

func TestDeleteConversation(t *testing.T) {
	// Create a temp directory to simulate projects dir
	tmpDir := t.TempDir()

	// Override getProjectsDir for this test
	origGetProjectsDir := getProjectsDir
	defer func() {
		// Can't actually override without refactoring, so just test the logic
	}()
	_ = origGetProjectsDir

	// Create test conversations
	conv1 := Conversation{
		SessionID: "test-session-1",
		Cwd:       "/test/project1",
		Messages: []Message{
			{Role: "user", Text: "First message"},
		},
	}
	conv2 := Conversation{
		SessionID: "test-session-2",
		Cwd:       "/test/project2",
		Messages: []Message{
			{Role: "user", Text: "Second message"},
		},
	}

	items := []listItem{
		{conv: conv1, searchText: "test1"},
		{conv: conv2, searchText: "test2"},
	}

	// Create model with test data
	m := model{
		items:         items,
		filtered:      items,
		cursor:        0,
		deleteIndex:   0,
		confirmDelete: true,
	}

	// Create a temporary file for the conversation
	testFile := filepath.Join(tmpDir, conv1.SessionID+".jsonl")
	content := `{"type":"user","cwd":"/test/project1","message":{"content":"First message"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Note: We can't fully test deleteConversation without dependency injection
	// but we can verify the file operations and state management logic separately

	// Test file deletion
	if err := os.Remove(testFile); err != nil {
		t.Errorf("failed to remove file: %v", err)
	}

	// Verify file was deleted
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Error("file should be deleted but still exists")
	}

	// Test cursor adjustment logic
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}

	// Simulate deletion from filtered slice
	m.filtered = append(m.filtered[:m.deleteIndex], m.filtered[m.deleteIndex+1:]...)

	if len(m.filtered) != 1 {
		t.Errorf("filtered should have 1 item after deletion, got %d", len(m.filtered))
	}

	if m.filtered[0].conv.SessionID != "test-session-2" {
		t.Errorf("remaining conversation should be test-session-2, got %s", m.filtered[0].conv.SessionID)
	}
}

func TestDeleteConversationInvalidIndex(t *testing.T) {
	m := model{
		items:       []listItem{},
		filtered:    []listItem{},
		deleteIndex: 10, // Invalid index
	}

	// Should not panic when index is out of bounds
	m.deleteConversation()

	// Verify error is set or state is unchanged
	// (deleteConversation returns early for invalid index)
}

func TestDeleteConversationErrorHandling(t *testing.T) {
	// Test deletion of non-existent file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "nonexistent.jsonl")

	// Try to delete non-existent file
	err := os.Remove(testFile)
	if err == nil {
		t.Error("expected error when deleting non-existent file")
	}

	// Verify it's the right kind of error
	if !os.IsNotExist(err) {
		t.Errorf("expected IsNotExist error, got: %v", err)
	}
}

func TestUpdateFilter(t *testing.T) {
	items := []listItem{
		{conv: Conversation{SessionID: "test-1"}, searchText: "Hello World foo", searchLower: "hello world foo"},
		{conv: Conversation{SessionID: "test-2"}, searchText: "Goodbye World bar", searchLower: "goodbye world bar"},
		{conv: Conversation{SessionID: "test-3"}, searchText: "Hello bar baz", searchLower: "hello bar baz"},
	}

	m := initialModel(items, "", nil)

	// Test: empty query returns all items
	m.textInput.SetValue("")
	m.updateFilter()
	if len(m.filtered) != 3 {
		t.Errorf("empty query should return all items, got %d", len(m.filtered))
	}

	// Test: query matches subset
	m.textInput.SetValue("hello")
	m.updateFilter()
	if len(m.filtered) != 2 {
		t.Errorf("'hello' query should return 2 items, got %d", len(m.filtered))
	}

	// Test: case insensitive
	m.textInput.SetValue("WORLD")
	m.updateFilter()
	if len(m.filtered) != 2 {
		t.Errorf("'WORLD' query should return 2 items (case insensitive), got %d", len(m.filtered))
	}

	// Test: no matches
	m.textInput.SetValue("xyz")
	m.updateFilter()
	if len(m.filtered) != 0 {
		t.Errorf("'xyz' query should return 0 items, got %d", len(m.filtered))
	}

	// Test: cursor adjustment when filtered list shrinks
	m.cursor = 2
	m.textInput.SetValue("hello")
	m.updateFilter()
	if m.cursor >= len(m.filtered) {
		t.Errorf("cursor should be adjusted to bounds, cursor=%d, filtered=%d", m.cursor, len(m.filtered))
	}
}

func TestFormatListItem(t *testing.T) {
	conv := Conversation{
		SessionID:     "test-123",
		Cwd:           "/home/user/very-long-project-name-that-exceeds-column-width",
		LastTimestamp: "2024-01-15T10:30:00Z",
		Messages: []Message{
			{Role: "user", Text: "This is a very long first message that should be truncated to fit in the column"},
			{Role: "assistant", Text: "Response"},
			{Role: "user", Text: "Second message"},
		},
	}

	item := listItem{conv: conv, searchText: ""}
	m := initialModel([]listItem{item}, "", nil)

	// Test selected formatting
	result := m.formatListItem(item, true)
	if !strings.Contains(result, "2024-01-15 10:30") {
		t.Errorf("formatted item should contain timestamp")
	}
	if !strings.Contains(result, "3") { // message count
		t.Errorf("formatted item should contain message count")
	}

	// Test non-selected formatting (has ANSI codes)
	result = m.formatListItem(item, false)
	if !strings.Contains(result, "\033[") {
		t.Errorf("non-selected item should contain ANSI color codes")
	}

	// Test hit count with query
	m.textInput.SetValue("message")
	result = m.formatListItem(item, false)
	// Should show 2 hits (both user messages contain "message")
	if !strings.Contains(result, "2") {
		t.Errorf("should show hit count when query matches")
	}
}

func TestFormatListItemNamedSessionMarker(t *testing.T) {
	named := listItem{conv: Conversation{
		SessionID:     "s1",
		Title:         "Refactor auth flow",
		LastTimestamp: "2024-01-15T10:30:00Z",
		Messages:      []Message{{Role: "user", Text: "hi"}},
	}}
	unnamed := listItem{conv: Conversation{
		SessionID:     "s2",
		LastTimestamp: "2024-01-15T10:30:00Z",
		Messages:      []Message{{Role: "user", Text: "just a first message"}},
	}}
	m := initialModel([]listItem{named, unnamed}, "", nil)

	if got := m.formatListItem(named, false); !strings.Contains(got, "✎ Refactor auth flow") {
		t.Errorf("named session should show the marker, got %q", got)
	}
	if got := m.formatListItem(unnamed, false); strings.Contains(got, "✎") {
		t.Errorf("first-message fallback should not show the marker, got %q", got)
	}
}

func TestUpdateKeyboardNavigation(t *testing.T) {
	items := []listItem{
		{conv: Conversation{SessionID: "test-1"}, searchText: "first"},
		{conv: Conversation{SessionID: "test-2"}, searchText: "second"},
		{conv: Conversation{SessionID: "test-3"}, searchText: "third"},
	}

	m := initialModel(items, "", nil)
	m.width = 100
	m.height = 30

	// Test down navigation
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = result.(model)
	if m.cursor != 1 {
		t.Errorf("down key should move cursor to 1, got %d", m.cursor)
	}

	// Test up navigation
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = result.(model)
	if m.cursor != 0 {
		t.Errorf("up key should move cursor to 0, got %d", m.cursor)
	}

	// Test up at boundary (should stay at 0)
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = result.(model)
	if m.cursor != 0 {
		t.Errorf("up at boundary should stay at 0, got %d", m.cursor)
	}

	// Test down to end
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = result.(model)
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = result.(model)
	if m.cursor != 2 {
		t.Errorf("should be at last item, got %d", m.cursor)
	}

	// Test down at boundary (should stay at 2)
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = result.(model)
	if m.cursor != 2 {
		t.Errorf("down at boundary should stay at 2, got %d", m.cursor)
	}
}

func TestUpdateDeleteConfirmation(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a real conversation file for testing
	sessionID := "delete-test-session"
	testFile := filepath.Join(tmpDir, sessionID+".jsonl")
	content := `{"type":"user","cwd":"/test","message":{"content":"test message"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Override getProjectsDir temporarily
	originalGetProjectsDir := getProjectsDir
	defer func() {
		// Can't easily override without refactoring, so we'll test the flow differently
		_ = originalGetProjectsDir
	}()

	items := []listItem{
		{
			conv: Conversation{
				SessionID: sessionID,
				Cwd:       "/test",
				Messages:  []Message{{Role: "user", Text: "test message"}},
			},
			searchText: "test",
		},
		{
			conv: Conversation{
				SessionID: "keep-this",
				Cwd:       "/test2",
				Messages:  []Message{{Role: "user", Text: "keep message"}},
			},
			searchText: "keep",
		},
	}

	m := initialModel(items, "", nil)
	m.width = 100
	m.height = 30

	// Trigger delete mode with Ctrl+D
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = result.(model)
	if !m.confirmDelete {
		t.Error("Ctrl+D should enter delete confirmation mode")
	}
	if m.deleteIndex != 0 {
		t.Errorf("deleteIndex should be 0, got %d", m.deleteIndex)
	}

	// Test cancellation with 'n'
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = result.(model)
	if m.confirmDelete {
		t.Error("'n' should exit delete confirmation mode")
	}

	// Test cancellation with Esc
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = result.(model)
	if !m.confirmDelete {
		t.Error("should be in delete confirmation mode")
	}
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = result.(model)
	if m.confirmDelete {
		t.Error("Esc should exit delete confirmation mode")
	}

	// Test that other keys are ignored in delete mode
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = result.(model)
	originalCursor := m.cursor
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = result.(model)
	if m.cursor != originalCursor {
		t.Error("arrow keys should be ignored in delete confirmation mode")
	}
}

func TestUpdateCtrlU(t *testing.T) {
	items := []listItem{
		{conv: Conversation{SessionID: "test-1"}, searchText: "hello"},
	}

	m := initialModel(items, "initial query", nil)

	// Verify initial query is set
	if m.textInput.Value() != "initial query" {
		t.Errorf("initial query should be set")
	}

	// Test Ctrl+U clears search
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = result.(model)
	if m.textInput.Value() != "" {
		t.Errorf("Ctrl+U should clear search, got %q", m.textInput.Value())
	}
}

func TestViewRendering(t *testing.T) {
	items := []listItem{
		{
			conv: Conversation{
				SessionID:     "test-1",
				Cwd:           "/test/project",
				LastTimestamp: "2024-01-15T10:00:00Z",
				Messages: []Message{
					{Role: "user", Text: "test message", Ts: "2024-01-15T10:00:00Z"},
				},
			},
			searchText: "test",
		},
	}

	m := initialModel(items, "", nil)
	m.width = 120
	m.height = 30

	output := m.View()

	// Check for key UI elements
	if !strings.Contains(output, "ccs") {
		t.Error("output should contain 'ccs' title")
	}
	if !strings.Contains(output, "type to search") {
		t.Error("output should contain search prompt")
	}
	if !strings.Contains(output, "DATE") || !strings.Contains(output, "PROJECT") {
		t.Error("output should contain column headers")
	}
	if !strings.Contains(output, "/test/project") {
		t.Error("output should contain project path in preview")
	}

	// Test delete confirmation view
	m.confirmDelete = true
	m.deleteIndex = 0
	output = m.View()
	if !strings.Contains(output, "Delete conversation") {
		t.Error("delete confirmation should be shown")
	}
	if !strings.Contains(output, "[y/N]") {
		t.Error("delete confirmation should show [y/N] prompt")
	}

	// Test error message display
	m.confirmDelete = false
	m.errorMsg = "Test error message"
	output = m.View()
	if !strings.Contains(output, "Test error message") {
		t.Error("error message should be displayed")
	}
}

func TestInit(t *testing.T) {
	m := initialModel([]listItem{}, "", nil)
	cmd := m.Init()
	if cmd == nil {
		t.Error("Init should return a command")
	}
}

func TestGetProjectsDir(t *testing.T) {
	dir := getProjectsDir()
	if !strings.Contains(dir, ".claude") || !strings.Contains(dir, "projects") {
		t.Errorf("getProjectsDir should return path containing .claude/projects, got %s", dir)
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

func TestUpdateMouseScroll(t *testing.T) {
	items := []listItem{
		{conv: Conversation{SessionID: "test-1"}, searchText: "first"},
		{conv: Conversation{SessionID: "test-2"}, searchText: "second"},
		{conv: Conversation{SessionID: "test-3"}, searchText: "third"},
	}

	m := initialModel(items, "", nil)

	// Set window size first to initialize mouse tracking
	result, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = result.(model)

	// Mouse wheel up in list area (Y=5, which should be in list)
	m.mouseInPreview = false // Explicitly set for test
	result, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Y: 5})
	m = result.(model)
	// Should not move cursor from 0
	if m.cursor != 0 {
		t.Errorf("wheel up at top should keep cursor at 0, got %d", m.cursor)
	}

	// Move to second item
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = result.(model)
	if m.cursor != 1 {
		t.Errorf("should be at cursor 1, got %d", m.cursor)
	}

	// Mouse wheel down in list area should move cursor forward
	m.mouseInPreview = false // Ensure we're scrolling list not preview
	result, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Y: 5})
	m = result.(model)
	if m.cursor != 2 {
		t.Errorf("wheel down should move to cursor 2, got %d", m.cursor)
	}

	// Mouse wheel in preview area
	m.previewScroll = 0
	m.mouseInPreview = true
	result, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Y: 20})
	m = result.(model)
	if m.previewScroll != 3 {
		t.Errorf("wheel down in preview should scroll preview, got %d", m.previewScroll)
	}
}

func TestDeleteConversationFullFlow(t *testing.T) {
	// Create temp directory that will act as projects dir
	tmpDir := t.TempDir()

	// Create test conversation files
	session1 := "delete-me"
	session2 := "keep-me"

	file1 := filepath.Join(tmpDir, session1+".jsonl")
	file2 := filepath.Join(tmpDir, session2+".jsonl")

	content := `{"type":"user","cwd":"/test","message":{"content":"test"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(file1, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	if err := os.WriteFile(file2, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Save and override getProjectsDir
	oldGetProjectsDir := getProjectsDir
	getProjectsDir = func() string { return tmpDir }
	defer func() { getProjectsDir = oldGetProjectsDir }()

	// Create items
	items := []listItem{
		{
			conv: Conversation{
				SessionID: session1,
				FilePath:  file1,
				Messages:  []Message{{Role: "user", Text: "delete this"}},
			},
			searchText: "delete",
		},
		{
			conv: Conversation{
				SessionID: session2,
				FilePath:  file2,
				Messages:  []Message{{Role: "user", Text: "keep this"}},
			},
			searchText: "keep",
		},
	}

	m := initialModel(items, "", nil)

	// Verify initial state
	if len(m.items) != 2 {
		t.Fatalf("initial items should be 2, got %d", len(m.items))
	}
	if len(m.filtered) != 2 {
		t.Fatalf("initial filtered should be 2, got %d", len(m.filtered))
	}

	m.deleteIndex = 0
	m.confirmDelete = true

	// Execute deletion (needs pointer receiver)
	(&m).deleteConversation()

	// Verify file was deleted
	if _, err := os.Stat(file1); !os.IsNotExist(err) {
		t.Error("file1 should be deleted")
	}

	// Verify second file still exists
	if _, err := os.Stat(file2); err != nil {
		t.Error("file2 should still exist")
	}

	// Verify filtered list was updated
	if len(m.filtered) != 1 {
		t.Errorf("filtered should have 1 item, got %d", len(m.filtered))
	}

	// Verify items list was updated
	if len(m.items) != 1 {
		t.Errorf("items should have 1 item, got %d", len(m.items))
	}

	// Verify remaining item is session2
	if m.items[0].conv.SessionID != session2 {
		t.Errorf("remaining item should be %s, got %s", session2, m.items[0].conv.SessionID)
	}

	// Verify confirmation mode exited
	if m.confirmDelete {
		t.Error("confirmDelete should be false after deletion")
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

func TestPrintHelp(t *testing.T) {
	// Just call it to ensure no panics - we can't easily test stdout
	// but this at least ensures the function doesn't crash
	printHelp()
}


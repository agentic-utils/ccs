package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	// Search text should contain session ID
	if !strings.Contains(items[0].searchText, "session1") {
		t.Errorf("search text should contain session ID, got %q", items[0].searchText)
	}

	// Search text should contain cwd
	if !strings.Contains(items[0].searchText, "/home/user/project1") {
		t.Errorf("search text should contain cwd, got %q", items[0].searchText)
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

	conv, err := parseConversationFile(testFile)
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

func TestParseConversationFileSkipsAgentFiles(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "agent-test.jsonl")

	content := `{"type":"user","message":{"content":"hello"},"timestamp":"2024-01-15T10:00:00Z"}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	conv, err := parseConversationFile(testFile)
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

	conv, err := parseConversationFile(testFile)
	if err != nil {
		t.Fatalf("parseConversationFile failed: %v", err)
	}

	if conv != nil {
		t.Error("parseConversationFile should return nil for files with no user/assistant messages")
	}
}


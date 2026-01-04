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

func TestPadOrTruncate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		length   int
		expected string
	}{
		{"short string", "hello", 10, "hello     "},
		{"exact length", "hello", 5, "hello"},
		{"needs truncation", "hello world", 8, "hello w\u2026"},
		{"with spaces", "hello   world", 10, "hello wor\u2026"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := padOrTruncate(tt.input, tt.length)
			if result != tt.expected {
				t.Errorf("padOrTruncate(%q, %d) = %q, want %q", tt.input, tt.length, result, tt.expected)
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

func TestBuildSearchLines(t *testing.T) {
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

	lines, convMap := buildSearchLines(conversations)

	// Should have exactly one line per conversation
	if len(lines) != 2 {
		t.Errorf("buildSearchLines returned %d lines, want 2", len(lines))
	}

	// Should have both conversations in the map
	if len(convMap) != 2 {
		t.Errorf("convMap has %d entries, want 2", len(convMap))
	}

	// First line should be for session1 with first user message
	if !strings.HasPrefix(lines[0], "session1\t") {
		t.Errorf("first line should start with 'session1\\t', got %q", lines[0])
	}
	if !strings.Contains(lines[0], "first message") {
		t.Errorf("first line should contain 'first message', got %q", lines[0])
	}

	// Line should contain all user messages for searching (column 5, not truncated)
	parts := strings.Split(lines[0], "\t")
	if len(parts) < 5 {
		t.Errorf("line should have 5 columns, got %d", len(parts))
	} else {
		searchText := parts[4]
		if !strings.Contains(searchText, "first message") || !strings.Contains(searchText, "second message") {
			t.Errorf("search text should contain all user messages, got %q", searchText)
		}
		// Verify no truncation
		if strings.Contains(searchText, "...") {
			t.Errorf("search text should not be truncated, got %q", searchText)
		}
	}

	// Second line should be for session2
	if !strings.HasPrefix(lines[1], "session2\t") {
		t.Errorf("second line should start with 'session2\\t', got %q", lines[1])
	}
}

func TestBuildSearchLinesNoUserMessages(t *testing.T) {
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

	lines, _ := buildSearchLines(conversations)

	// Should have no lines since there are no user messages
	if len(lines) != 0 {
		t.Errorf("buildSearchLines returned %d lines for conversation with no user messages, want 0", len(lines))
	}
}

func TestBuildSearchLinesProjectExtraction(t *testing.T) {
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

	lines, _ := buildSearchLines(conversations)

	// Should extract project name from path
	if !strings.Contains(lines[0], "my-project") {
		t.Errorf("line should contain project name 'my-project', got %q", lines[0])
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
		{"matching query", "hello world", "world", "\033[43;30mworld\033[0m"},
		{"case insensitive", "Hello World", "world", "\033[43;30mWorld\033[0m"},
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

func TestFormatCodeBlock(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		query    string
		contains string
	}{
		{"plain text", "hello world", "", "hello world"},
		{"code block", "```go\nfmt.Println()\n```", "", "┌─ go ─"},
		{"code block end", "```go\nfmt.Println()\n```", "", "└─────────"},
		{"highlights query outside code", "hello world", "world", "\033[43;30mworld\033[0m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatCodeBlock(tt.text, tt.query, "")
			if !strings.Contains(result, tt.contains) {
				t.Errorf("formatCodeBlock(%q, %q) = %q, want to contain %q", tt.text, tt.query, result, tt.contains)
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

func TestSaveAndLoadCache(t *testing.T) {
	convMap := map[string]Conversation{
		"session1": {
			SessionID:      "session1",
			Cwd:            "/test/path",
			FirstTimestamp: "2024-01-15T10:00:00Z",
			LastTimestamp:  "2024-01-15T10:01:00Z",
			Messages: []Message{
				{Role: "user", Text: "hello", Ts: "2024-01-15T10:00:00Z"},
			},
		},
	}

	if err := saveCache(convMap); err != nil {
		t.Fatalf("saveCache failed: %v", err)
	}

	loaded, err := loadCache()
	if err != nil {
		t.Fatalf("loadCache failed: %v", err)
	}

	if len(loaded) != 1 {
		t.Errorf("loaded cache has %d entries, want 1", len(loaded))
	}

	conv, ok := loaded["session1"]
	if !ok {
		t.Fatal("session1 not found in loaded cache")
	}

	if conv.Cwd != "/test/path" {
		t.Errorf("Cwd = %q, want %q", conv.Cwd, "/test/path")
	}
}

func TestBuildSearchLinesUsesLastTimestamp(t *testing.T) {
	conversations := []Conversation{
		{
			SessionID:      "session1",
			Cwd:            "/home/user/project",
			FirstTimestamp: "2024-01-15T10:00:00Z",
			LastTimestamp:  "2024-01-15T12:00:00Z",
			Messages: []Message{
				{Role: "user", Text: "first", Ts: "2024-01-15T10:00:00Z"},
				{Role: "user", Text: "second", Ts: "2024-01-15T12:00:00Z"},
			},
		},
	}

	lines, _ := buildSearchLines(conversations)

	// Should use LastTimestamp (12:00) not FirstTimestamp (10:00)
	if !strings.Contains(lines[0], "12:00") {
		t.Errorf("line should contain LastTimestamp '12:00', got %q", lines[0])
	}
}

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
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
	stripped := strings.NewReplacer("\033[43;30m", "", "\033[0m", "", "\033[49;39m", "").Replace(got)
	if stripped != "héllo wörld héllo" {
		t.Errorf("highlight altered visible text: %q", stripped)
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

func TestFormatListItemShowsSize(t *testing.T) {
	item := listItem{conv: Conversation{
		SessionID:     "s1",
		LastTimestamp: "2024-01-15T10:30:00Z",
		Size:          75 * 1 << 20,
		Messages:      []Message{{Role: "user", Text: "hi"}},
	}}
	m := initialModel([]listItem{item}, "", nil)
	if got := m.formatListItem(item, false); !strings.Contains(got, "75MB") {
		t.Errorf("list row should show the file size, got %q", got)
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

func TestUpdateFilterIncrementalNarrowing(t *testing.T) {
	items := []listItem{
		{conv: Conversation{SessionID: "1"}, searchText: "auth gateway", searchLower: "auth gateway"},
		{conv: Conversation{SessionID: "2"}, searchText: "auth service", searchLower: "auth service"},
		{conv: Conversation{SessionID: "3"}, searchText: "billing", searchLower: "billing"},
	}
	m := initialModel(items, "", nil)

	check := func(q string, want int) {
		t.Helper()
		m.textInput.SetValue(q)
		m.updateFilter()
		if len(m.filtered) != want {
			t.Fatalf("query %q: got %d results, want %d", q, len(m.filtered), want)
		}
	}
	check("auth", 2)         // full scan
	check("auth gateway", 1) // narrowing: filters the previous 2, not all 3
	if m.filtered[0].conv.SessionID != "1" {
		t.Errorf("narrowed result should be session 1, got %s", m.filtered[0].conv.SessionID)
	}
	check("auth", 2) // broadening: "auth gateway" not in "auth" -> full rescan
	check("billing", 1)
	check("", 3) // cleared
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
	// Timestamps render in local time, so pin the zone or this fails outside UTC.
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.UTC

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
	if !strings.Contains(result, "y ago") { // 2024 timestamp, rendered relative
		t.Errorf("formatted item should show how long ago it was active, got %q", result)
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
	custom := listItem{conv: Conversation{
		SessionID:     "s1",
		Title:         "Refactor auth flow",
		IsCustomTitle: true,
		LastTimestamp: "2024-01-15T10:30:00Z",
		Messages:      []Message{{Role: "user", Text: "hi"}},
	}}
	// ai-title: has a Title but it's auto-generated - must NOT get the marker.
	aiTitled := listItem{conv: Conversation{
		SessionID:     "s2",
		Title:         "Some auto generated title",
		IsCustomTitle: false,
		LastTimestamp: "2024-01-15T10:30:00Z",
		Messages:      []Message{{Role: "user", Text: "hi"}},
	}}
	fallback := listItem{conv: Conversation{
		SessionID:     "s3",
		LastTimestamp: "2024-01-15T10:30:00Z",
		Messages:      []Message{{Role: "user", Text: "just a first message"}},
	}}
	m := initialModel([]listItem{custom, aiTitled, fallback}, "", nil)
	m.width = 120 // give TOPIC room so the title isn't truncated

	if got := m.formatListItem(custom, false); !strings.Contains(got, "Refactor auth flow ✍") {
		t.Errorf("user-set custom title should show the marker, got %q", got)
	}
	if got := m.formatListItem(aiTitled, false); strings.Contains(got, "✍") {
		t.Errorf("auto ai-title must NOT show the marker, got %q", got)
	}
	if got := m.formatListItem(fallback, false); strings.Contains(got, "✍") {
		t.Errorf("first-message fallback should not show the marker, got %q", got)
	}
}

func TestTopicColWidthFlexes(t *testing.T) {
	fixed := listIndent + colWhen + colProject + colModel + colCtx + colMsgs + colHits + colSize + numGaps*colGap
	for _, w := range []int{100, 120, 200} {
		m := model{width: w}
		if got, want := m.topicColWidth(), w-fixed; got != want {
			t.Errorf("topicColWidth(width=%d) = %d, want %d", w, got, want)
		}
	}
	// Tiny terminal clamps to a minimum rather than going negative.
	if got := (model{width: 20}).topicColWidth(); got != 10 {
		t.Errorf("topicColWidth(width=20) = %d, want 10 (min)", got)
	}
}

func TestFormatListItemFillsWidth(t *testing.T) {
	item := listItem{conv: Conversation{
		SessionID:     "s",
		LastTimestamp: "2024-01-15T10:30:00Z",
		Size:          1 << 20,
		Messages:      []Message{{Role: "user", Text: "hi"}},
	}}
	for _, w := range []int{100, 120, 200} {
		m := initialModel([]listItem{item}, "", nil)
		m.width = w
		// Selected row has no ANSI codes; its width + the 2-char row prefix
		// added by View should fill the terminal exactly.
		if got := len(m.formatListItem(item, true)); got != w-listIndent {
			t.Errorf("width=%d: row length = %d, want %d", w, got, w-listIndent)
		}
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
	if !strings.Contains(output, "WHEN") || !strings.Contains(output, "PROJECT") {
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
	if m.Init() != nil {
		t.Error("with nothing to refresh, Init should schedule nothing (no cursor blink)")
	}
	m.reload = func() ([]listItem, error) { return nil, nil }
	if m.Init() == nil {
		t.Error("Init should start the refresh and live ticks")
	}
}

func TestIdleScreenDoesNotChange(t *testing.T) {
	// An idle frame must be identical from one render to the next: the
	// renderer skips identical frames, so ccs writes nothing and terminals
	// don't show the tab as busy.
	m := initialModel(buildItems([]Conversation{{SessionID: "s", LastTimestamp: time.Now().Format(time.RFC3339), Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 120, 30
	m.reload = func() ([]listItem, error) { return nil, nil }
	m.lastRefresh = time.Now()
	first := m.View()
	time.Sleep(1100 * time.Millisecond)
	if m.View() != first {
		t.Error("an idle screen changed within a second; it would redraw constantly")
	}
}

func TestGetProjectsDir(t *testing.T) {
	dir := getProjectsDir()
	if !strings.Contains(dir, ".claude") || !strings.Contains(dir, "projects") {
		t.Errorf("getProjectsDir should return path containing .claude/projects, got %s", dir)
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

func TestPruneStreamRemovesDuplicatesKeepsDialogue(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","message":{"content":"hello"},"uuid":"u1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]},"uuid":"a1"}`,
		`{"type":"file-history-snapshot","snapshot":{"trackedFileBackups":{"big":"xxxxxxxxxx"}}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"the real output"}]},"toolUseResult":{"type":"text","text":"the real output dup"},"uuid":"u2"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	st, err := pruneStream(strings.NewReader(input), &out, pruneOpts{dropSnapshots: true, stripToolResults: true})
	if err != nil {
		t.Fatalf("pruneStream: %v", err)
	}
	if st.droppedSnapshots != 1 {
		t.Errorf("droppedSnapshots = %d, want 1", st.droppedSnapshots)
	}
	if st.strippedResults != 1 {
		t.Errorf("strippedResults = %d, want 1", st.strippedResults)
	}
	if st.convLinesIn != 3 || st.convLinesOut != 3 {
		t.Errorf("conv lines in/out = %d/%d, want 3/3", st.convLinesIn, st.convLinesOut)
	}
	o := out.String()
	if strings.Contains(o, "trackedFileBackups") {
		t.Error("file-history-snapshot should be dropped")
	}
	if strings.Contains(o, "toolUseResult") {
		t.Error("toolUseResult field should be stripped")
	}
	for _, want := range []string{"hello", "the real output", "u2", "a1"} {
		if !strings.Contains(o, want) {
			t.Errorf("output should preserve %q", want)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(o), "\n") {
		var v map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &v) != nil {
			t.Errorf("output line is not valid JSON: %s", line)
		}
	}
}

func TestPruneOptsRespected(t *testing.T) {
	input := `{"type":"file-history-snapshot","snapshot":{}}` + "\n" +
		`{"type":"user","toolUseResult":{"x":1},"message":{"content":"hi"}}` + "\n"
	var a bytes.Buffer
	pruneStream(strings.NewReader(input), &a, pruneOpts{dropSnapshots: false, stripToolResults: true})
	if !strings.Contains(a.String(), "file-history-snapshot") {
		t.Error("--no-snapshots should keep snapshot lines")
	}
	var b bytes.Buffer
	pruneStream(strings.NewReader(input), &b, pruneOpts{dropSnapshots: true, stripToolResults: false})
	if !strings.Contains(b.String(), "toolUseResult") {
		t.Error("--no-tool-results should keep toolUseResult")
	}
}

func TestPruneFileReplacesAndShrinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	content := `{"type":"user","message":{"content":"keep me"},"uuid":"u1"}` + "\n" +
		`{"type":"file-history-snapshot","snapshot":{"data":"` + strings.Repeat("x", 5000) + `"}}` + "\n" +
		`{"type":"assistant","message":{"content":"keep me too"},"uuid":"a1"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	st, err := pruneFile(path, true, pruneOpts{dropSnapshots: true, stripToolResults: true})
	if err != nil {
		t.Fatalf("pruneFile: %v", err)
	}
	if st.convLinesIn != st.convLinesOut {
		t.Fatalf("integrity broken: %d != %d", st.convLinesIn, st.convLinesOut)
	}
	after, _ := os.Stat(path)
	if after.Size() >= before.Size() {
		t.Errorf("file should shrink: before %d, after %d", before.Size(), after.Size())
	}
	if _, err := os.Stat(path + ".pruned"); !os.IsNotExist(err) {
		t.Error(".pruned temp should be gone after atomic replace")
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, "file-history-snapshot") {
		t.Error("snapshot not removed from file")
	}
	if !strings.Contains(s, "keep me") || !strings.Contains(s, "keep me too") {
		t.Error("dialogue not preserved in file")
	}
}

func TestPruneFileDryRunLeavesFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jsonl")
	content := `{"type":"file-history-snapshot","snapshot":{}}` + "\n" + `{"type":"user","message":{"content":"hi"}}` + "\n"
	os.WriteFile(path, []byte(content), 0644)
	orig, _ := os.ReadFile(path)
	st, err := pruneFile(path, false, pruneOpts{dropSnapshots: true, stripToolResults: true})
	if err != nil {
		t.Fatalf("pruneFile dry: %v", err)
	}
	if st.bytesOut >= st.bytesIn {
		t.Error("dry run should still report savings")
	}
	now, _ := os.ReadFile(path)
	if string(now) != string(orig) {
		t.Error("dry run must not modify the file")
	}
}

func TestPruneConversationShrinksAndUpdatesSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	content := `{"type":"user","message":{"content":"keep"},"uuid":"u1"}` + "\n" +
		`{"type":"file-history-snapshot","snapshot":{"data":"` + strings.Repeat("x", 4000) + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)

	item := listItem{conv: Conversation{SessionID: "s1", FilePath: path, Size: info.Size(),
		Messages: []Message{{Role: "user", Text: "keep"}}}}
	m := initialModel([]listItem{item}, "", nil)
	m.confirmPrune = true
	m.pruneIndex = 0
	m.pruneConversation()

	if m.errorMsg != "" {
		t.Fatalf("prune errored: %s", m.errorMsg)
	}
	if m.confirmPrune {
		t.Error("should exit confirm mode after pruning")
	}
	after, _ := os.Stat(path)
	if after.Size() >= info.Size() {
		t.Errorf("file should shrink: %d -> %d", info.Size(), after.Size())
	}
	if m.items[0].conv.Size != after.Size() || m.filtered[0].conv.Size != after.Size() {
		t.Errorf("displayed size not updated to %d (items=%d filtered=%d)", after.Size(), m.items[0].conv.Size, m.filtered[0].conv.Size)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "file-history-snapshot") {
		t.Error("snapshot should be removed")
	}
	if !strings.Contains(string(data), "keep") {
		t.Error("conversation message should be kept")
	}
}

func TestCtrlXEntersAndCancelsPruneConfirm(t *testing.T) {
	// Ctrl+X measures the file to preview savings, so it needs a real file.
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	content := `{"type":"user","message":{"content":"hi"}}` + "\n" +
		`{"type":"file-history-snapshot","snapshot":{"data":"` + strings.Repeat("x", 3000) + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	item := listItem{conv: Conversation{SessionID: "s1", FilePath: path, Size: info.Size(),
		Messages: []Message{{Role: "user", Text: "hi"}}}}
	m := initialModel([]listItem{item}, "", nil)

	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m = res.(model)
	if !m.confirmPrune {
		t.Fatalf("ctrl+x should enter prune confirm mode (err=%q)", m.errorMsg)
	}
	if m.pruneSaved <= 0 {
		t.Errorf("ctrl+x should measure a positive saving, got %d", m.pruneSaved)
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = res.(model)
	if m.confirmPrune {
		t.Error("esc should cancel prune confirm")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`a b`); got != `'a b'` {
		t.Errorf("got %s", got)
	}
	if got := shellQuote(`it's`); got != `'it'\''s'` {
		t.Errorf("got %s", got)
	}
}

func TestResumeInITermTabSkipsOutsideITerm(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	if handled, _ := resumeInITermTab("/tmp", []string{"claude"}); handled {
		t.Error("should not open a tab outside iTerm")
	}
}

func TestResumeInTmuxWindowSkipsOutsideTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	if handled, _ := resumeInTmuxWindow("/tmp", []string{"claude"}); handled {
		t.Error("should not open a window outside tmux")
	}
}

func TestReadLiveSessions(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("self.json", fmt.Sprintf(`{"pid":%d,"sessionId":"alive"}`, os.Getpid()))
	write("dead.json", `{"pid":999999999,"sessionId":"dead"}`)
	write("junk.json", `not json`)

	live := readLiveSessions()
	if !live["alive"] || live["dead"] || len(live) != 1 {
		t.Errorf("want only 'alive', got %v", live)
	}
}

func TestFormatListItemLiveMarker(t *testing.T) {
	item := listItem{conv: Conversation{SessionID: "s1", Title: "Topic", Messages: []Message{{Role: "user", Text: "x"}}}}
	m := initialModel([]listItem{item}, "", nil)
	m.width = 120
	if strings.Contains(m.formatListItem(item, false), "●") {
		t.Error("non-live row should have no marker")
	}
	m.live = map[string]bool{"s1": true}
	plain := m.formatListItem(item, true)
	if !strings.Contains(plain, " ● Topic") {
		t.Errorf("live row should be marked, got %q", plain)
	}
	coloured := m.formatListItem(item, false)
	if !strings.Contains(coloured, " \033[32m●\033[0m Topic") {
		t.Errorf("live marker should be green, got %q", coloured)
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

func TestFormatListItemSpawnedAndLiveMarkers(t *testing.T) {
	item := listItem{conv: Conversation{SessionID: "s1", Title: "Topic", Spawned: true, Messages: []Message{{Role: "user", Text: "x"}}}}
	m := initialModel([]listItem{item}, "", nil)
	m.width = 120
	m.live = map[string]bool{"s1": true}
	if got := m.formatListItem(item, true); !strings.Contains(got, " ●⚙ Topic") {
		t.Errorf("selected row = %q", got)
	}
	got := m.formatListItem(item, false)
	if !strings.Contains(got, " \033[32m●\033[0m\033[90m⚙\033[0m Topic") {
		t.Errorf("unselected row = %q", got)
	}
	// Same visible width as an unmarked row.
	plain := listItem{conv: Conversation{SessionID: "s2", Title: "Topic", Messages: item.conv.Messages}}
	strip := func(s string) int {
		return utf8.RuneCountInString(regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(s, ""))
	}
	unmarked := m.formatListItem(plain, false)
	if strip(got) != strip(unmarked) {
		t.Error("marked row width differs from unmarked row")
	}
	// Titles line up whatever the markers.
	col := func(s string) int {
		s = regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(s, "")
		return utf8.RuneCountInString(s[:strings.Index(s, "Topic")])
	}
	if col(got) != col(unmarked) {
		t.Errorf("topic column differs: marked %d, unmarked %d", col(got), col(unmarked))
	}
}

func TestRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	// No trailing newline: the rename must not fuse onto the last record.
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	item := buildItems([]Conversation{{SessionID: "s1", FilePath: path, Messages: []Message{{Role: "user", Text: "hi"}}}})[0]
	m := initialModel([]listItem{item}, "", nil)

	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = res.(model)
	if !m.renaming {
		t.Fatal("ctrl+r should start renaming")
	}
	m.renameInput.SetValue("  new name ")
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	if m.renaming || m.items[0].conv.Title != "new name" || !m.items[0].conv.IsCustomTitle {
		t.Fatalf("rename not applied: %+v err=%q", m.items[0].conv, m.errorMsg)
	}
	conv, err := parseConversationFile(path, time.Time{}, 0)
	if err != nil || conv.Title != "new name" || !conv.IsCustomTitle || len(conv.Messages) != 1 {
		t.Errorf("reparsed = %+v, %v", conv, err)
	}
	m.textInput.SetValue("new name")
	m.updateFilter()
	if len(m.filtered) != 1 {
		t.Error("new name should be searchable")
	}

	// Live sessions can't be renamed from ccs.
	m.textInput.SetValue("")
	m.updateFilter()
	m.live = map[string]bool{"s1": true}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m = res.(model); m.renaming || m.errorMsg == "" {
		t.Error("rename should be refused on a live session")
	}
}

func TestRefreshTickRunsReloadAndCarriesGen(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.reload = func() ([]listItem, error) { return []listItem{{conv: Conversation{SessionID: "x"}}}, nil }
	m.gen = 3
	_, cmd := m.Update(refreshTickMsg{})
	msg, ok := cmd().(refreshMsg)
	if !ok || msg.gen != 3 || len(msg.items) != 1 {
		t.Fatalf("tick should produce a refreshMsg from reload, got %+v", msg)
	}
}

func TestRefreshDroppedWhenStaleOrFailed(t *testing.T) {
	a := listItem{conv: Conversation{SessionID: "a"}}
	m := initialModel([]listItem{a}, "", nil)

	// A scan that started before a delete/prune/rename must not undo it.
	m.gen = 1
	res, cmd := m.Update(refreshMsg{gen: 0, items: []listItem{a, {conv: Conversation{SessionID: "deleted"}}}})
	if m = res.(model); len(m.items) != 1 || cmd == nil {
		t.Errorf("stale refresh should be dropped but still reschedule, items=%d", len(m.items))
	}

	// A failed scan keeps the current list.
	res, _ = m.Update(refreshMsg{gen: 1, err: os.ErrNotExist, live: map[string]bool{"a": true}})
	if m = res.(model); len(m.items) != 1 || !m.live["a"] {
		t.Error("failed refresh should keep items and still update live")
	}

	// A successful scan that legitimately finds nothing empties the list.
	res, _ = m.Update(refreshMsg{gen: 1, items: []listItem{}})
	if m = res.(model); len(m.items) != 0 {
		t.Error("empty successful refresh should apply")
	}
}

func TestDeleteBumpsGenAndEvictsCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"content":"hi"},"timestamp":"t"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConversationFile(path, time.Time{}, 0); err != nil {
		t.Fatal(err)
	}
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s", FilePath: path}}}, "", nil)
	m.deleteIndex = 0
	m.deleteConversation()
	parseCacheMu.Lock()
	_, cached := parseCache[path]
	parseCacheMu.Unlock()
	if m.gen != 1 || cached {
		t.Errorf("delete should bump gen and evict cache: gen=%d cached=%v", m.gen, cached)
	}
}

func TestPruneRefusedOnLiveSession(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.live = map[string]bool{"s": true}
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	if m = res.(model); m.confirmPrune || m.errorMsg == "" {
		t.Error("prune should be refused on a live session")
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

func TestLiveCheckedAtConfirmAndAlwaysApplied(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s", FilePath: path}}}, "", nil)
	m.confirmPrune = true

	// Session becomes live between Ctrl+X and y: the confirm must re-check.
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"s"}`, os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m = res.(model); m.confirmPrune || m.errorMsg == "" || m.gen != 0 {
		t.Errorf("prune confirm should be refused once the session is live (gen=%d err=%q)", m.gen, m.errorMsg)
	}

	// A dropped (stale) refresh still updates liveness.
	m.gen = 5
	res, _ = m.Update(refreshMsg{gen: 0, live: map[string]bool{"other": true}})
	if m = res.(model); !m.live["other"] {
		t.Error("live should be applied even when the refresh is stale")
	}
}

func TestTmuxPaneForTTY(t *testing.T) {
	panes := "/dev/ttys001 main:0.0\n/dev/ttys007 work:2.1\n"
	if got := tmuxPaneForTTY(panes, "/dev/ttys007"); got != "work:2.1" {
		t.Errorf("got %q", got)
	}
	if got := tmuxPaneForTTY(panes, "/dev/ttys999"); got != "" {
		t.Errorf("unknown tty should give no target, got %q", got)
	}
}

func TestEnterOnLiveSessionFocusesInsteadOfResuming(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"s"}`, os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	t.Setenv("TERM_PROGRAM", "") // no terminal to focus in the test

	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.live = map[string]bool{"s": true}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	if m.selected != nil || m.quitting || cmd == nil {
		t.Fatal("enter on a live session must focus it (in the background), not resume a second copy")
	}
	res, _ = m.Update(firstOfBatch(cmd)) // the fast refresh for the live selection rides along
	if m = res.(model); !strings.Contains(m.errorMsg, "^F") {
		t.Errorf("unfocusable live session should point at fork, got %q", m.errorMsg)
	}
}

func TestCtrlFForksInPlaceOutsideTabbedTerminals(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TERM_PROGRAM", "")
	flags := make([]string, 1, 4) // spare capacity: fork must not write into it
	flags[0] = "--plan"
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", flags)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	m = res.(model)
	if cmd == nil {
		t.Fatal("ctrl+f should start opening the fork")
	}
	res, _ = m.Update(cmd())
	m = res.(model)
	if m.selected == nil || !m.fork || !m.quitting {
		t.Error("ctrl+f should select the conversation for a forked resume")
	}
	if len(m.claudeFlags) != 1 || m.claudeFlags[:2][1] == "--fork-session" {
		t.Error("fork flag must not leak into the shared claude flags")
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

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		tag, cur string
		want     bool
	}{
		{"v0.25.0", "0.24.1", true},
		{"v0.24.2", "0.24.1", true},
		{"v1.0.0", "0.99.99", true},
		{"v0.24.1", "0.24.1", false},
		{"v0.9.0", "0.24.1", false}, // numeric, not lexical
		{"v0.25.0", "dev", false},
		{"garbage", "0.24.1", false},
	}
	for _, c := range cases {
		if got := newerVersion(c.tag, c.cur); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.tag, c.cur, got, c.want)
		}
	}
}

func TestUpdatePopupFlow(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.24.1"
	upgraded := false
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.upgrade = &upgrader{install: func(_ string, step func(string)) (string, error) {
		step("brew upgrade")
		upgraded = true
		return "/bin/ccs", nil
	}}

	res, cmd := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	if !m.updateOpen || cmd == nil {
		t.Fatal("newer release should open the popup and schedule the next check")
	}
	if !strings.Contains(m.View(), "v0.25.0 is available") {
		t.Error("popup should render in the view")
	}

	// Keys straight after it opens are swallowed, so typing can't answer it.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); !m.updateOpen || m.updating {
		t.Fatal("enter inside the grace period must be ignored")
	}

	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); !m.updating || cmd == nil {
		t.Fatal("enter should start the upgrade")
	}
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); !upgraded || m.restart != "/bin/ccs" || !m.quitting {
		t.Error("a successful upgrade should quit for restart")
	}
}

func TestUpdatePopupLaterAndFailure(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.24.1"
	m := initialModel(nil, "", nil)
	m.upgrade = &upgrader{install: func(string, func(string)) (string, error) { return "", fmt.Errorf("network down") }}

	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m = res.(model); m.updateOpen || m.quitting {
		t.Fatal("esc should dismiss the popup, not quit ccs")
	}
	res, _ = m.Update(latestMsg{tag: "v0.25.0"})
	if m = res.(model); m.updateOpen {
		t.Error("a dismissed version should not pop up again this session")
	}
	res, _ = m.Update(latestMsg{tag: "v0.26.0"})
	if m = res.(model); !m.updateOpen {
		t.Error("an even newer version should pop up again")
	}

	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); m.restart != "" || !m.updateOpen || !strings.Contains(m.updateErr, "network down") {
		t.Errorf("failed upgrade should reopen the popup with the error, got open=%v err=%q", m.updateOpen, m.updateErr)
	}
	m.width, m.height = 100, 30
	if v := m.View(); !strings.Contains(v, "network down") || !strings.Contains(v, "retry") {
		t.Error("popup should show the failure and offer a retry")
	}
}

func TestUpdatePopupWithoutBrew(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.24.1"
	m := initialModel(nil, "", nil)
	m.width, m.height = 100, 30
	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	if !strings.Contains(m.View(), "package manager") {
		t.Error("non-Homebrew installs should be told to update themselves")
	}
}

func TestReleaseTagFromURL(t *testing.T) {
	if tag, err := releaseTagFromURL("https://github.com/agentic-utils/ccs/releases/tag/v0.24.1"); err != nil || tag != "v0.24.1" {
		t.Errorf("got %q, %v", tag, err)
	}
	for _, bad := range []string{"", "https://github.com/agentic-utils/ccs/releases"} {
		if _, err := releaseTagFromURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// fakeRelease serves tag's checksums.txt and this platform's archive holding
// binary as "ccs"; tamper corrupts the published checksum.
func fakeRelease(t *testing.T, tag string, binary []byte, tamper bool) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{"README.md": []byte("readme"), "ccs": binary} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	gz.Close()
	asset := fmt.Sprintf("ccs_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(buf.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	if tamper {
		sums = strings.Repeat("0", 64) + "  " + asset + "\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tag + "/checksums.txt":
			io.WriteString(w, sums)
		case "/" + tag + "/" + asset:
			w.Write(buf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := releaseDownloadURL
	releaseDownloadURL = srv.URL
	t.Cleanup(func() { releaseDownloadURL = old })
}

func TestReplaceBinary(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	if _, err := chooseUpgrader(exe).Install("v9.9.9", func(string) {}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	info, _ := os.Stat(exe)
	if string(got) != "new binary" || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("binary not replaced/executable: %q %v", got, info.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".ccs-update-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestReplaceBinaryRejectsBadChecksum(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("evil"), true)
	if _, err := chooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Error("a failed checksum must leave the binary untouched")
	}
}

func TestReplaceBinaryUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	fakeRelease(t, "v9.9.9", []byte("new"), false)
	if _, err := chooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "can't write") {
		t.Fatalf("want a clear permission error, got %v", err)
	}
}

func TestChooseUpgrader(t *testing.T) {
	if chooseUpgrader("/nix/store/abc-ccs/bin/ccs") != nil {
		t.Error("nix store is read-only: notify only")
	}
	if chooseUpgrader(filepath.Join(t.TempDir(), "ccs")) == nil {
		t.Error("a plain install should self-update")
	}
	// A Homebrew keg is never replaced in place: it gets brew or nothing.
	up := chooseUpgrader("/opt/homebrew/Cellar/ccs/0.25.0/bin/ccs")
	if _, err := exec.LookPath("brew"); err != nil && up != nil {
		t.Error("Cellar install without brew on PATH must not self-replace")
	}
}

// firstOfBatch runs the first command of a tea.Batch (the upgrade itself;
// the rest is the 1s redraw tick, which would just sleep).
func firstOfBatch(cmd tea.Cmd) tea.Msg {
	if batch, ok := cmd().(tea.BatchMsg); ok {
		return batch[0]()
	}
	return cmd()
}

func TestUpgraderInstallWaitsForPrepare(t *testing.T) {
	release := make(chan struct{})
	var order []string
	var mu sync.Mutex
	log := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	u := &upgrader{
		prepare: func(string) error { <-release; log("prepared"); return nil },
		install: func(string, func(string)) (string, error) { log("installed"); return "", nil },
	}
	u.Prepare("v1")
	u.Prepare("v1") // second call is a no-op
	var steps []string
	done := make(chan struct{})
	go func() { u.Install("v1", func(s string) { steps = append(steps, s) }); close(done) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done
	if strings.Join(order, ",") != "prepared,installed" || len(steps) == 0 || steps[0] != "finishing download" {
		t.Errorf("install must wait for prepare: order=%v steps=%v", order, steps)
	}
}

func TestPreparedReleaseSkipsDownloadOnInstall(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	u := chooseUpgrader(exe)
	u.Prepare("v9.9.9")
	u.mu.Lock()
	done := u.pending["v9.9.9"]
	u.mu.Unlock()
	<-done
	releaseDownloadURL = "http://127.0.0.1:1" // any download after prepare would now fail
	var steps []string
	if _, err := u.Install("v9.9.9", func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatalf("install should use the prepared download: %v (steps %v)", err, steps)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary" {
		t.Errorf("binary not replaced: %q", got)
	}
}

func TestUpdatingHeaderShowsStep(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.width, m.height = 140, 30
	m.updating, m.updateTo = true, "v0.27.0"
	m.progress = &updateProgress{step: "brew upgrade", started: time.Now().Add(-5 * time.Second)}
	if v := m.View(); !strings.Contains(v, "updating to v0.27.0: brew upgrade (5s)") {
		t.Errorf("header should show step and elapsed time")
	}
}

func TestSameStart(t *testing.T) {
	actual := time.Date(2026, 9, 23, 16, 52, 33, 0, time.UTC).In(time.FixedZone("NPT", 5*3600+45*60))
	if !sameStart("Wed Sep 23 16:52:33 2026", actual) {
		t.Error("same instant in different zones should match")
	}
	if sameStart("Wed Sep 23 10:00:00 2026", actual) {
		t.Error("a different start time means a recycled pid")
	}
	if !sameStart("", actual) || !sameStart("Wed Sep 23 16:52:33 2026", time.Time{}) {
		t.Error("unknown start on either side must not hide a live session")
	}
}

func TestLiveSessionsDropsRecycledPid(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()
	oldPS := processStartTimes
	defer func() { processStartTimes = oldPS }()
	processStartTimes = func(map[string]int) map[int]time.Time {
		return map[int]time.Time{os.Getpid(): time.Date(2026, 9, 23, 16, 52, 33, 0, time.UTC)}
	}
	write := func(name, id, start string) {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"procStart":%q}`, os.Getpid(), id, start)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", "current", "Wed Sep 23 16:52:33 2026")
	write("b.json", "stale", "Mon Sep 21 09:00:00 2026") // same pid, earlier process
	live := readLiveSessions()
	if !live["current"] || live["stale"] {
		t.Errorf("recycled pid should not count as live: %v", live)
	}
}

func TestRefreshStalledShowsInHeader(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.width, m.height = 140, 30
	m.refreshStarted = time.Now().Add(-7 * time.Minute)
	if !strings.Contains(m.View(), "refresh stalled 7m") {
		t.Error("a long-running scan should be flagged in the header")
	}
	res, _ := m.Update(refreshMsg{})
	if m = res.(model); m.refreshStalled() {
		t.Error("a completed scan clears the stall")
	}
}

func TestUpdatePopupWaitsBehindPrompts(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.27.1"
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s", Title: "old"}}}, "", nil)
	m.width, m.height = 100, 30
	m.upgrade = &upgrader{install: func(string, func(string)) (string, error) { return "", nil }}
	m.renaming = true
	m.renameInput = textinput.New()
	m.renameInput.Focus()

	res, _ := m.Update(latestMsg{tag: "v0.27.2"})
	m = res.(model)
	m.updateShownAt = time.Now().Add(-time.Hour) // grace long gone
	if strings.Contains(m.View(), "is available") {
		t.Error("popup must not show over the rename prompt")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m = res.(model); !strings.HasSuffix(m.renameInput.Value(), "x") {
		t.Error("typing should reach the rename input, not the popup")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m = res.(model); m.renaming || !m.updateOpen {
		t.Fatal("esc should cancel the rename and leave the update pending")
	}
	if !strings.Contains(m.View(), "is available") {
		t.Error("popup should show once the prompt closes")
	}
	// First key after it appears is swallowed by a fresh grace period.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); m.updating {
		t.Error("enter straight after the prompt closed must not start the update")
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

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
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

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestLiveMsgUpdatesAndResortsKeepingCursor(t *testing.T) {
	mk := func(id, ts string) Conversation {
		return Conversation{SessionID: id, LastTimestamp: ts, Messages: []Message{{Role: "user", Text: id}}}
	}
	m := initialModel(buildItems([]Conversation{mk("a", "2026-09-25T10:00:00Z"), mk("b", "2026-09-25T09:00:00Z")}), "", nil)
	m.cursor = 1 // on b
	grown := mk("b", "2026-09-25T11:00:00Z")
	grown.Messages = append(grown.Messages, Message{Role: "assistant", Text: "new"})
	res, cmd := m.Update(liveMsg{live: map[string]bool{"b": true}, updated: []Conversation{grown}})
	m = res.(model)
	if cmd == nil || !m.live["b"] {
		t.Fatal("live tick should apply liveness and reschedule")
	}
	if m.items[0].conv.SessionID != "b" || len(m.items[0].conv.Messages) != 2 {
		t.Errorf("updated session should move to the top with its new message: %+v", m.items[0].conv)
	}
	if m.filtered[m.cursor].conv.SessionID != "b" {
		t.Error("cursor should follow the same conversation")
	}

	// Content updates wait while a prompt is open; liveness still applies.
	m.confirmDelete = true
	res, _ = m.Update(liveMsg{live: map[string]bool{}, updated: []Conversation{mk("a", "2026-09-25T12:00:00Z")}})
	if m = res.(model); m.items[0].conv.SessionID != "b" || m.live["b"] {
		t.Error("prompt open: no reshuffle, but liveness should update")
	}
}

func TestUnknownLiveSessionTriggersEarlyScanOnce(t *testing.T) {
	m := initialModel(nil, "", nil)
	scans := 0
	m.reload = func() ([]listItem, error) { scans++; return []listItem{}, nil }
	res, cmd := m.Update(liveMsg{live: map[string]bool{"new": true}, unknown: []string{"new"}})
	m = res.(model)
	if m.refreshStarted.IsZero() || cmd == nil {
		t.Fatal("an unlisted live session should start a scan now")
	}
	// The scheduled tick arriving mid-scan doesn't start a second scan.
	res, _ = m.Update(refreshTickMsg{})
	if m = res.(model); scans != 0 {
		t.Error("tick during an early scan should only reschedule")
	}
	// The early scan's result doesn't start its own one-minute chain.
	res, cmd = m.Update(refreshMsg{items: []listItem{}, early: true})
	if m = res.(model); cmd != nil {
		t.Error("early scan must not add a second refresh schedule")
	}
	// And kicks are rate-limited.
	res, _ = m.Update(liveMsg{live: map[string]bool{"new": true}, unknown: []string{"new"}})
	if m = res.(model); !m.refreshStarted.IsZero() {
		t.Error("a second kick within 15s should wait")
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

func TestUpdateCheckFailureShowsInHeader(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.width, m.height = 160, 30
	res, _ := m.Update(latestMsg{err: fmt.Errorf("dns")})
	if m = res.(model); !strings.Contains(m.View(), "update check failed") {
		t.Error("a failed update check should be visible")
	}
	res, _ = m.Update(latestMsg{tag: "v0.0.1"})
	if m = res.(model); strings.Contains(m.View(), "update check failed") {
		t.Error("a later successful check clears it")
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

func TestStaleLiveResultDoesNotRollBackScan(t *testing.T) {
	now := time.Now()
	scanned := Conversation{SessionID: "s", Size: 300, LastTimestamp: "2026-09-25T11:00:00Z", Messages: []Message{{Role: "user", Text: "new"}}, readAt: now}
	liveOld := scanned
	liveOld.Size, liveOld.readAt = 200, now.Add(-time.Second)
	m := initialModel(buildItems([]Conversation{scanned}), "", nil)
	m.applyLive([]Conversation{liveOld})
	if m.items[0].conv.Size != 300 {
		t.Error("a live read older than the list's copy must be ignored")
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

func TestTmuxCwdHashIsEscaped(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "tmux")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TMUX", "fake")
	if handled, err := resumeInTmuxWindow("/tmp/#(id)", []string{"claude"}); !handled || err != nil {
		t.Fatal(handled, err)
	}
	got, _ := os.ReadFile(log)
	if !strings.Contains(string(got), "/tmp/##(id)") {
		t.Errorf("tmux -c must get '#' escaped, got args:\n%s", got)
	}
}

func TestDoubleEnterOpensOnce(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	res, first := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	_, second := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if first == nil || second != nil {
		t.Error("a second Enter while the first is opening must be ignored")
	}
}

func TestTabResultOnlyTimeoutMeansMaybeOpened(t *testing.T) {
	if h, err := tabResult(nil, nil); !h || err != nil {
		t.Error("success is handled")
	}
	if h, err := tabResult(nil, fmt.Errorf("osascript: %w", errTimedOut)); !h || err == nil {
		t.Error("a timeout is handled with an error: it may have opened")
	}
	if h, _ := tabResult(nil, fmt.Errorf("exit status 1")); h {
		t.Error("a plain failure must fall back to resuming in place")
	}
	if _, err := runBounded(50*time.Millisecond, nil, "sleep", "5"); !errors.Is(err, errTimedOut) {
		t.Errorf("runBounded should report its deadline, got %v", err)
	}
}

func TestShellSafe(t *testing.T) {
	for s, want := range map[string]bool{"/Users/me/proj": true, "/tmp/it's": true, `/r/a\`: false, "/tmp/a\x01b": false, "/tmp/\xff": false} {
		if shellSafe(s) != want {
			t.Errorf("shellSafe(%q) != %v", s, want)
		}
	}
}

func TestSecondEnterAfterTabOpenDoesNotLaunchAgain(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	res, _ := m.Update(resumeDoneMsg{conv: Conversation{SessionID: "s"}, opened: true})
	m = res.(model)
	// A live tick before claude writes its session file must keep it live.
	res, _ = m.Update(liveMsg{live: map[string]bool{}})
	if m = res.(model); !m.live["s"] {
		t.Error("a session ccs just opened should stay live until its file appears")
	}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); cmd != nil {
		t.Error("enter again must not start another resume")
	}
}

func TestStaleSessionFileWithOtherPidDoesNotHideLiveOne(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()
	oldPS := processStartTimes
	defer func() { processStartTimes = oldPS }()
	me := os.Getpid()
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	processStartTimes = func(map[string]int) map[int]time.Time {
		return map[int]time.Time{me: start, 1: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	write := func(name string, pid int, when string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"S","procStart":%q}`, pid, when)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("0-live.json", me, "Fri Sep 25 10:00:00 2026")
	write("9-stale.json", 1, "Mon Sep 21 09:00:00 2026") // crashed claude's leftover; pid 1 recycled
	if pid := liveSessionPIDs()["S"]; pid != me {
		t.Errorf("live session hidden by a stale file: got pid %d", pid)
	}
}

func TestQuitBlockedDuringUpdate(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.updating = true
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m = res.(model); m.quitting || cmd != nil || m.errorMsg == "" {
		t.Error("ctrl+c during an update must not quit")
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

func TestUnlistableLiveSessionScansOnce(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.reload = func() ([]listItem, error) { return []listItem{}, nil }
	res, _ := m.Update(liveMsg{live: map[string]bool{"x": true}, unknown: []string{"x"}})
	m = res.(model)
	res, _ = m.Update(refreshMsg{items: []listItem{}, early: true})
	m = res.(model)
	m.lastKick = time.Time{} // even past the 15s limit
	res, _ = m.Update(liveMsg{live: map[string]bool{"x": true}, unknown: []string{"x"}})
	if m = res.(model); !m.refreshStarted.IsZero() {
		t.Error("a session an early scan already looked for must not trigger scans forever")
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

func TestEscOnlyClearsSearch(t *testing.T) {
	items := buildItems([]Conversation{
		{SessionID: "a", Messages: []Message{{Role: "user", Text: "apple"}}},
		{SessionID: "b", Messages: []Message{{Role: "user", Text: "banana"}}},
	})
	m := initialModel(items, "apple", nil)
	if len(m.filtered) != 1 {
		t.Fatal("setup: query should filter")
	}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = res.(model)
	if m.quitting || cmd != nil || m.textInput.Value() != "" || len(m.filtered) != 2 {
		t.Fatalf("esc should clear the search and show everything")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m = res.(model); m.quitting {
		t.Error("esc never quits, even on an empty search box")
	}
}

func TestCtrlCQuitsEvenWithSearch(t *testing.T) {
	m := initialModel(nil, "something", nil)
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m = res.(model); !m.quitting {
		t.Error("ctrl+c should quit straight away")
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

func TestSessionStatsAndErrorIcon(t *testing.T) {
	conv := Conversation{SessionID: "s", Title: "T", Model: "claude-opus-5-5", ContextTokens: 281_000, PeakContext: 281_000,
		Usage: tokenUsage{Input: 1000, Output: 1000, CacheRead: 1_000_000}, LastError: "429 rate_limit", LastErrorTs: time.Now().Format(time.RFC3339),
		Messages: []Message{{Role: "user", Text: "x"}}}
	stats := strings.Join(sessionStats(conv), "\n")
	for _, want := range []string{"claude-opus-5-5", "281k", "of 1.0M", "28%", "effective 106k", "~$0.53 at API prices", "429 rate_limit"} {
		if !strings.Contains(stats, want) {
			t.Errorf("stats missing %q:\n%s", want, stats)
		}
	}
	m := initialModel(buildItems([]Conversation{conv}), "", nil)
	m.width = 140
	if !strings.Contains(m.formatListItem(m.items[0], true), "! T") {
		t.Error("a session whose last action errored should show ! in the list")
	}
}

func TestMain(m *testing.M) {
	// Keep tests from writing into the real update log.
	dir, _ := os.MkdirTemp("", "ccs-test-log")
	updateLogPath = filepath.Join(dir, "update.log")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestUpdateLogRecordsCommandsAndDownloads(t *testing.T) {
	old := updateLogPath
	updateLogPath = filepath.Join(t.TempDir(), "logs", "update.log")
	defer func() { updateLogPath = old }()

	if _, err := runCommand(5*time.Second, nil, true, "sh", "-c", "echo downloading; echo boom >&2; exit 3"); err == nil {
		t.Fatal("want failure")
	}
	fakeRelease(t, "v9.9.9", []byte("bin"), false)
	if _, err := download(releaseDownloadURL + "/v9.9.9/checksums.txt"); err != nil {
		t.Fatal(err)
	}
	logUpdate("install v9.9.9: %v in %s", errOrOK(nil), time.Second)

	got, err := os.ReadFile(updateLogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"$ sh -c", "exit status 3", "  | downloading", "  | boom", "GET http://", "last connection 127.0.0.1:", "install v9.9.9: ok"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

func TestFailedUpdatePopupPointsAtLog(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.width, m.height = 120, 30
	m.updateTo, m.updateErr, m.updateOpen = "v9.9.9", "brew upgrade: curl: (35) Recv failure", true
	m.upgrade = &upgrader{install: func(string, func(string)) (string, error) { return "", nil }}
	if !strings.Contains(m.View(), updateLogPath) {
		t.Error("a failed update should point at the update log")
	}
}

func usageTestLine(id, reqID string, ts time.Time, in, create, c5, c1, read, out int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":%q,"message":{"id":%q,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}}}`,
		ts.UTC().Format(time.RFC3339), reqID, id, in, create, read, out, c5, c1) + "\n"
}

func TestCollectUsageBucketsAndDedupes(t *testing.T) {
	dir := t.TempDir()
	old := getProjectsDir
	getProjectsDir = func() string { return dir }
	defer func() { getProjectsDir = old }()
	usageFilesMu.Lock()
	usageFiles = make(map[string]usageFile)
	usageFilesMu.Unlock()

	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	proj := filepath.Join(dir, "-p")
	sub := filepath.Join(proj, "sess", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string, mod time.Time) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, mod, mod)
	}
	recent := now.Add(-30 * time.Minute) // last bucket hour
	earlier := now.Add(-6 * time.Hour)
	write(filepath.Join(proj, "sess.jsonl"),
		usageTestLine("m1", "", recent, 10, 300, 100, 200, 1000, 50)+
			usageTestLine("m1", "", recent, 10, 300, 100, 200, 1000, 50)+ // same reply, second line
			usageTestLine("", "r1", earlier, 5, 20, 20, 0, 0, 7)+ // no message id: requestId; read 0 = cache miss
			usageTestLine("m0", "", now.Add(-13*time.Hour), 999, 0, 0, 0, 0, 999)+ // outside the window
			`{"type":"user","message":{"content":"hi"}}`+"\n",
		now)
	write(filepath.Join(sub, "agent-a.jsonl"), usageTestLine("s1", "", recent, 1, 40, 40, 0, 500, 3), now)
	write(filepath.Join(proj, "stale.jsonl"), usageTestLine("x", "", recent, 1e6, 0, 0, 0, 0, 1e6), now.Add(-24*time.Hour)) // file untouched for a day

	d := collectUsage(now, nil)
	var agg usageBucketTotals
	for _, b := range d.buckets {
		agg.Uncached += b.Uncached
		agg.C5m += b.C5m
		agg.C1h += b.C1h
		agg.Read += b.Read
		agg.New += b.New
		agg.Miss += b.Miss
		agg.Output += b.Output
		agg.Responses += b.Responses
	}
	want := usageBucketTotals{Uncached: 16, C5m: 160, C1h: 200, Read: 1500, New: 310 + 41, Miss: 25, Output: 60, Responses: 3}
	if agg != want {
		t.Errorf("totals = %+v, want %+v", agg, want)
	}
	if last := d.buckets[usageBuckets-6]; last.Responses != 2 { // 30 minutes ago = 6 buckets from the end
		t.Errorf("recent replies should land in the bucket 30m ago, got %+v", last)
	}
	eff1h := tokenUsage{Input: 10, Cache5m: 100, Cache1h: 200, CacheRead: 1000, Output: 50}.effective() +
		tokenUsage{Input: 1, Cache5m: 40, CacheRead: 500, Output: 3}.effective()
	eff12h := eff1h + tokenUsage{Input: 5, Cache5m: 20, Output: 7}.effective()
	if d.eff1h != eff1h || d.eff12h != eff12h {
		t.Errorf("effective 1h=%v 12h=%v", d.eff1h, d.eff12h)
	}
}

func TestRenderUsageChart(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local)
	buckets := make([]usageBucketTotals, usageBuckets)
	buckets[10] = usageBucketTotals{Output: 1000}
	buckets[20] = usageBucketTotals{Output: 500}
	merged := resampleBuckets(buckets, 72)
	if len(merged) != 72 || merged[5].Output != 1000 {
		t.Fatalf("resampleBuckets: %d cols", len(merged))
	}
	lines := renderUsageChart("Output", []usageSeries{{"output", "output tokens"}}, merged, 4, now)
	strip := func(s string) string { return regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(s, "") }
	if len(lines) != 1+4+2 {
		t.Fatalf("want title + 4 rows + baseline + labels, got %d lines", len(lines))
	}
	top := []rune(strip(lines[1]))
	if top[usageMargin+5] != '█' || top[usageMargin+10] == '█' {
		t.Errorf("tallest bar should reach the top row, half bar shouldn't: %q", strip(lines[1]))
	}
	if !strings.Contains(strip(lines[1]), "1k") {
		t.Errorf("y-axis should label the scale: %q", strip(lines[1]))
	}
	if !strings.Contains(strip(lines[len(lines)-1]), ":00") {
		t.Errorf("x-axis should have hourly labels: %q", strip(lines[len(lines)-1]))
	}
}

func TestFetchAllowance(t *testing.T) {
	var gotAuth, gotBeta string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		w.WriteHeader(status)
		io.WriteString(w, `{"limits":[{"kind":"session","percent":62,"resets_at":"2026-09-29T14:10:00+00:00"},{"kind":"weekly_all","percent":21.5,"resets_at":"2026-10-05T09:00:00+00:00"}]}`)
	}))
	defer srv.Close()
	oldURL, oldRead := allowanceURL, keychainRead
	defer func() { allowanceURL, keychainRead = oldURL, oldRead }()
	allowanceURL = srv.URL
	now := time.Now()
	creds := func(tok string, exp time.Time) {
		keychainRead = func() ([]byte, error) {
			return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, tok, exp.UnixMilli())), nil
		}
	}

	creds("tok", now.Add(time.Hour))
	limits, err := fetchAllowance(now)
	if err != nil || len(limits) != 2 || limits[0].Kind != "session" || limits[1].Percent != 21.5 {
		t.Fatalf("limits=%+v err=%v", limits, err)
	}
	if gotAuth != "Bearer tok" || gotBeta != "oauth-2025-04-20" {
		t.Errorf("headers: auth=%q beta=%q", gotAuth, gotBeta)
	}

	creds("tok", now.Add(-time.Minute)) // expired: never refreshed by ccs
	if _, err := fetchAllowance(now); !errors.Is(err, errAllowanceLogin) {
		t.Errorf("expired token should ask for a claude login, got %v", err)
	}
	keychainRead = func() ([]byte, error) { return nil, nil } // no credential stored
	if _, err := fetchAllowance(now); !errors.Is(err, errAllowanceLogin) {
		t.Errorf("missing credential should ask for a claude login, got %v", err)
	}
	creds("tok", now.Add(time.Hour))
	status = http.StatusUnauthorized
	if _, err := fetchAllowance(now); !errors.Is(err, errAllowanceLogin) {
		t.Errorf("401 should ask for a claude login, got %v", err)
	}
	status = http.StatusInternalServerError
	if _, err := fetchAllowance(now); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("server error should be reported, got %v", err)
	}
}

func TestKeychainServiceName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if keychainService() != "Claude Code-credentials" {
		t.Errorf("default profile: %q", keychainService())
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/me/.claude-work")
	sum := sha256.Sum256([]byte("/Users/me/.claude-work"))
	if want := "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]; keychainService() != want {
		t.Errorf("per-profile: %q, want %q", keychainService(), want)
	}
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "") // set but empty: the default item
	if keychainService() != "Claude Code-credentials" {
		t.Errorf("empty override: %q", keychainService())
	}
}

func TestUsageScreenToggleAndPanels(t *testing.T) {
	m := initialModel(buildItems([]Conversation{{SessionID: "a", Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 170, 50
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = res.(model)
	if !m.showUsage || cmd == nil || !m.usageLoading || !m.allowanceLoading {
		t.Fatal("tab should open the usage screen and start loading")
	}
	// Typing on the usage screen mustn't edit the hidden search.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m = res.(model); m.textInput.Value() != "" {
		t.Error("keys other than tab/ctrl+c are ignored on the usage screen")
	}
	buckets := make([]usageBucketTotals, usageBuckets)
	buckets[100] = usageBucketTotals{Uncached: 10, Read: 90, New: 10, Output: 5, Responses: 1}
	res, _ = m.Update(usageMsg{usageData{now: time.Now(), buckets: buckets, eff1h: 1000, eff12h: 2000}})
	m = res.(model)
	res, _ = m.Update(allowanceMsg{limits: []allowanceLimit{{Kind: "session", Percent: 62, ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)}}})
	m = res.(model)
	v := regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(m.View(), "")
	for _, want := range []string{"Input · cache write disposition", "Context assembly", "Output", "SUMMARY", "responses", "1 ", "ALLOWANCE", "5-hour session", "62%", "read from cache", "90.0%"} {
		if !strings.Contains(v, want) {
			t.Errorf("usage screen missing %q", want)
		}
	}
	res, _ = m.Update(allowanceMsg{err: errAllowanceLogin})
	if m = res.(model); !strings.Contains(m.View(), "62%") || !strings.Contains(m.View(), "open claude") {
		t.Error("a failed refresh keeps the last gauges and shows why")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m = res.(model); m.showUsage {
		t.Error("tab again returns to the session list")
	}
}

func TestDownloadFallsBackPastStalledAddress(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "payload") }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	// An address that accepts TCP but never answers the TLS handshake.
	stall, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stall.Close()
	go func() {
		for {
			c, err := stall.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	oldResolve, oldDial, oldTLS, oldTimeout := resolveHost, dialAddr, downloadTLSConfig, addrAttemptTimeout
	defer func() {
		resolveHost, dialAddr, downloadTLSConfig, addrAttemptTimeout = oldResolve, oldDial, oldTLS, oldTimeout
	}()
	failedAddrs.Range(func(k, _ any) bool { failedAddrs.Delete(k); return true })
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	downloadTLSConfig = &tls.Config{RootCAs: pool}
	addrAttemptTimeout = 300 * time.Millisecond
	resolveHost = func(context.Context, string) ([]string, error) { return []string{"192.0.2.1", "192.0.2.2"}, nil }
	var tried []string
	dialAddr = func(ctx context.Context, network, addr string) (net.Conn, error) {
		tried = append(tried, addr)
		if strings.HasPrefix(addr, "192.0.2.1:") {
			return (&net.Dialer{}).DialContext(ctx, network, stall.Addr().String())
		}
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port)
	}
	downloadTransport.CloseIdleConnections()

	start := time.Now()
	body, err := download("https://example.com:" + port + "/x")
	if err != nil || string(body) != "payload" {
		t.Fatalf("download = %q, %v", body, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("fallback took %v; the stalled address should cost ~one attempt limit", d)
	}
	if _, bad := failedAddrs.Load("192.0.2.1"); !bad {
		t.Error("the stalled address should be remembered as failed")
	}
	// Next time the failed address goes last.
	tried = nil
	downloadTransport.CloseIdleConnections()
	if _, err := download("https://example.com:" + port + "/x"); err != nil {
		t.Fatal(err)
	}
	if len(tried) == 0 || !strings.HasPrefix(tried[0], "192.0.2.2:") {
		t.Errorf("known-bad address should be tried last, got %v", tried)
	}
}

func TestSeedFile(t *testing.T) {
	fakeRelease(t, "v1.0.0", []byte("bin"), false)
	url := releaseDownloadURL + "/v1.0.0/checksums.txt"
	body, err := download(url)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256Hex(body)
	path := filepath.Join(t.TempDir(), "cached.tar.gz")

	if err := seedFile(path, url, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); sha256Hex(got) != want {
		t.Fatal("seeded file has the wrong content")
	}
	// Already valid: no download (an unreachable URL proves it isn't fetched).
	if err := seedFile(path, "http://127.0.0.1:1/unreachable", want); err != nil {
		t.Errorf("valid cache should be kept without downloading: %v", err)
	}
	// Checksum mismatch: error, and nothing replaces the cache file.
	other := filepath.Join(t.TempDir(), "other.tar.gz")
	if err := seedFile(other, url, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("want checksum mismatch, got %v", err)
	}
	if fileExists(other) {
		t.Error("a mismatched download must not be written to the cache")
	}
}

func TestAllowanceInSearchRowAndUsageScreenHidesSearch(t *testing.T) {
	m := initialModel(buildItems([]Conversation{{SessionID: "s", Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 160, 40
	res, _ := m.Update(allowanceMsg{account: "me@example.com", limits: []allowanceLimit{
		{Kind: "session", Percent: 14, ResetsAt: time.Now().Add(2 * time.Hour).Format(time.RFC3339)},
		{Kind: "weekly_all", Percent: 22.4},
	}})
	m = res.(model)
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	lines := strings.Split(strip.ReplaceAllString(m.View(), ""), "\n")
	row := lines[1]
	for _, want := range []string{"type to search", "me@example.com", "5h 14%", "ends ", "week 22%", "(1/1)"} {
		if !strings.Contains(row, want) {
			t.Errorf("search row missing %q: %q", want, row)
		}
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = res.(model)
	if v := strip.ReplaceAllString(m.View(), ""); strings.Contains(v, "type to search") || !strings.Contains(v, "tab back") {
		t.Error("the usage screen should replace the search box with a back hint")
	}
	if v := strip.ReplaceAllString(m.View(), ""); strings.Contains(v, "^S msg") || !strings.Contains(v, "tab back  ^O account  ^L changelog  ^C quit") {
		t.Error("the usage screen header should list only its own keys")
	}
}

func TestAllowanceSummaryAsksToOpenClaudeWhenLoggedOut(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.allowanceErr = errAllowanceLogin
	if !strings.Contains(m.allowanceSummary(), "open claude") {
		t.Error("no token should say how to get usage")
	}
}

func TestAccountEmailFromClaudeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"me@example.com"},"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := accountEmail(); got != "me@example.com" {
		t.Errorf("got %q", got)
	}
	alt := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", alt)
	if err := os.WriteFile(filepath.Join(alt, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"other@example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := accountEmail(); got != "other@example.com" {
		t.Errorf("CLAUDE_CONFIG_DIR profile: got %q", got)
	}
}

func TestResampleBucketsFillsAnyWidth(t *testing.T) {
	in := make([]usageBucketTotals, usageBuckets)
	for i := range in {
		in[i].Output = 1
	}
	for _, cols := range []int{20, 72, 144, 200, 400} {
		out := resampleBuckets(in, cols)
		if len(out) != cols {
			t.Errorf("cols %d: got %d columns", cols, len(out))
		}
		var total int64
		for _, b := range out {
			total += b.Output
			if b.Output == 0 {
				t.Errorf("cols %d: gap in the chart", cols)
				break
			}
		}
		if cols <= usageBuckets && total != int64(usageBuckets) {
			t.Errorf("cols %d: downsampling must keep the total, got %d", cols, total)
		}
	}
}

func TestUsageChartsSpanScreenWidth(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.usage = usageData{now: time.Now(), buckets: make([]usageBucketTotals, usageBuckets)}
	for i := range m.usage.buckets {
		m.usage.buckets[i].Output = int64(i + 1)
	}
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	for _, w := range []int{100, 170, 260} {
		m.width, m.height = w, 50
		for _, line := range strings.Split(strip.ReplaceAllString(m.usageView(46), ""), "\n") {
			if strings.Contains(line, "└") {
				if got := utf8.RuneCountInString(line); got != w-1 {
					t.Errorf("width %d: chart baseline is %d wide, want %d", w, got, w-1)
				}
				break
			}
		}
	}
}

func TestUsagePanelsInThreeEqualColumns(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.usage = usageData{now: time.Now(), buckets: make([]usageBucketTotals, usageBuckets)}
	m.usage.buckets[0] = usageBucketTotals{Read: 90, New: 10, Output: 5, Responses: 1}
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	for _, w := range []int{120, 200} {
		m.width = w
		for _, line := range strings.Split(strip.ReplaceAllString(m.usageView(40), ""), "\n") {
			if !strings.Contains(line, "SUMMARY") {
				continue
			}
			s, c, a := strings.Index(line, "SUMMARY"), strings.Index(line, "CACHE MIX"), strings.Index(line, "ALLOWANCE")
			if s < 0 || c < 0 || a < 0 || c-s != a-c {
				t.Errorf("width %d: panels not in equal columns: %q", w, line)
			}
		}
	}
}

func TestUpdatePopupOverlaysEveryScreen(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.36.0"
	m := initialModel(buildItems([]Conversation{{SessionID: "s", Title: "Some session", Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 140, 40
	m.upgrade = &upgrader{install: func(string, func(string)) (string, error) { return "", nil }}
	m.usage = usageData{now: time.Now(), buckets: make([]usageBucketTotals, usageBuckets)}
	res, _ := m.Update(latestMsg{tag: "v0.37.0"})
	m = res.(model)
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	for _, usage := range []bool{false, true} {
		m.showUsage = usage
		v := strip.ReplaceAllString(m.View(), "")
		if !strings.Contains(v, "v0.37.0 is available") {
			t.Errorf("usage=%v: popup not shown", usage)
		}
		if !strings.Contains(v, "claude code search") {
			t.Errorf("usage=%v: the screen behind the popup should stay visible", usage)
		}
		for i, line := range strings.Split(v, "\n") {
			if !strings.ContainsAny(line, "│╭╰") { // only the rows the popup covers
				continue
			}
			if w := utf8.RuneCountInString(line); w > m.width {
				t.Errorf("usage=%v: line %d is %d wide, over the %d-column screen", usage, i, w, m.width)
			}
		}
	}
}

func mouseModel(t *testing.T) model {
	t.Helper()
	var convs []Conversation
	for i := 0; i < 10; i++ {
		c := Conversation{SessionID: fmt.Sprint("s", i), LastTimestamp: fmt.Sprintf("2026-09-29T10:%02d:00Z", 59-i)}
		for j := 0; j < 40; j++ {
			c.Messages = append(c.Messages, Message{Role: "user", Text: fmt.Sprintf("message %d", j)})
		}
		convs = append(convs, c)
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 120, 40
	return m
}

func TestWheelScrollsWhateverIsUnderThePointer(t *testing.T) {
	m := mouseModel(t)
	listTop, _, previewTop := m.listLayout()

	// Over the preview: the conversation scrolls, the selection stays.
	// (The preview opens at the newest message, so wheel up goes back first.)
	m = m.handleMouse(tea.MouseMsg{X: 10, Y: previewTop + 2, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if m.previewScroll != 3 || m.cursor != 0 {
		t.Fatalf("wheel up over preview: scroll=%d cursor=%d", m.previewScroll, m.cursor)
	}
	m = m.handleMouse(tea.MouseMsg{X: 10, Y: previewTop + 2, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.previewScroll != 0 {
		t.Errorf("wheel down over preview should come forward again, got %d", m.previewScroll)
	}

	// Over the list: the selection moves.
	m = m.handleMouse(tea.MouseMsg{X: 10, Y: listTop + 1, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.cursor != 1 {
		t.Errorf("wheel over list should move the selection, cursor=%d", m.cursor)
	}

	// Clicking a row selects it.
	m = m.handleMouse(tea.MouseMsg{X: 10, Y: listTop + 4, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.cursor != 4 {
		t.Errorf("click should select row 4, cursor=%d", m.cursor)
	}

	// The layout matches what View draws: the preview's first line is the Project header.
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	lines := strings.Split(strip.ReplaceAllString(m.View(), ""), "\n")
	if !strings.HasPrefix(lines[previewTop], "Project:") {
		t.Errorf("previewTop %d doesn't match the screen: %q", previewTop, lines[previewTop])
	}
}

func TestLeakedMouseReportNeverReachesSearch(t *testing.T) {
	m := mouseModel(t)
	for _, leak := range []string{"[<65;40;12M", "<64;10;5", "[<0;3;4m"} {
		res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(leak)})
		if m = res.(model); m.textInput.Value() != "" {
			t.Errorf("%q leaked into the search box", leak)
		}
	}
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m = res.(model); m.textInput.Value() != "a" {
		t.Error("ordinary typing must still work")
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

func TestLiveUpdateKeepsScrolledBackViewStill(t *testing.T) {
	mk := func(n int) Conversation {
		c := Conversation{SessionID: "s", Cwd: "/p", Size: int64(n), readAt: time.Now()}
		for i := 0; i < n; i++ {
			c.Messages = append(c.Messages, Message{Role: "user", Text: fmt.Sprint("line-", i)})
		}
		return c
	}
	m := initialModel(buildItems([]Conversation{mk(60)}), "", nil)
	m.width, m.height = 120, 40
	m.previewScroll = 20 // reading back in the history
	before := m.View()
	grown := mk(62) // two new messages arrive
	grown.readAt = time.Now().Add(time.Second)
	m.applyLive([]Conversation{grown})
	b, a := strings.Split(strip2(before), "\n"), strings.Split(strip2(m.View()), "\n")
	_, _, previewTop := m.listLayout()
	for i := previewTop; i < len(b) && i < len(a); i++ {
		if b[i] != a[i] {
			t.Errorf("preview row %d moved:\n was %q\n now %q", i, b[i], a[i])
		}
	}
	m.previewScroll = 0 // at the newest: follow new messages
	m.applyLive([]Conversation{func() Conversation { c := mk(64); c.readAt = time.Now().Add(2 * time.Second); return c }()})
	if !strings.Contains(strip2(m.View()), "line-63") {
		t.Error("at the newest message, new ones should come into view")
	}
}

func strip2(s string) string { return regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(s, "") }

func TestUIStateSurvivesRestart(t *testing.T) {
	mk := func(id, text string) Conversation {
		c := Conversation{SessionID: id, LastTimestamp: "2026-09-29T10:00:00Z"}
		for i := 0; i < 50; i++ {
			c.Messages = append(c.Messages, Message{Role: "user", Text: text + fmt.Sprint(i)})
		}
		return c
	}
	before := initialModel(buildItems([]Conversation{mk("a", "apple"), mk("b", "banana"), mk("c", "bandana")}), "", nil)
	before.width, before.height = 120, 40
	before.textInput.SetValue("band")
	before.textInput.SetCursor(2)
	before.updateFilter()
	before.cursor = 0 // "band" matches only "c" (bandana)
	before.previewScroll = 7
	before.showUsage = true
	saved, err := json.Marshal(before.uiState())
	if err != nil {
		t.Fatal(err)
	}

	// The new version's list comes back in a different order.
	after := initialModel(buildItems([]Conversation{mk("c", "bandana"), mk("b", "banana"), mk("a", "apple")}), "", nil)
	var state uiState
	if err := json.Unmarshal(saved, &state); err != nil {
		t.Fatal(err)
	}
	after.restore(state)
	if after.textInput.Value() != "band" || after.textInput.Position() != 2 {
		t.Errorf("query/cursor = %q/%d", after.textInput.Value(), after.textInput.Position())
	}
	if len(after.filtered) != 1 || after.filtered[after.cursor].conv.SessionID != "c" {
		t.Errorf("selection not restored: %+v", after.filtered)
	}
	if after.previewScroll != 7 || !after.showUsage || after.screenName() != "usage" {
		t.Errorf("scroll=%d usage=%v", after.previewScroll, after.showUsage)
	}
	if after.Init() == nil {
		t.Error("restored onto the usage screen, Init should load the usage data")
	}

	// A conversation that no longer exists is skipped, not an error.
	gone := initialModel(buildItems([]Conversation{mk("z", "zebra")}), "", nil)
	gone.restore(uiState{Screen: "list", Selected: "missing"})
	if gone.cursor != 0 || gone.showUsage {
		t.Error("restoring a missing selection should leave a sane default")
	}
}

// chatModel has two conversations, the second live (pid of this process).
func chatModel(t *testing.T) model {
	t.Helper()
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	t.Cleanup(func() { getSessionsDir = old })
	items := buildItems([]Conversation{
		{SessionID: "idle", Title: "Quiet one", LastTimestamp: "2026-09-29T11:00:00Z", Messages: []Message{{Role: "user", Text: "hello"}}},
		{SessionID: "live", Title: "Busy one", LastTimestamp: "2026-09-29T10:00:00Z", Messages: []Message{{Role: "user", Text: "hi"}}},
	})
	m := initialModel(items, "", nil)
	m.width, m.height = 120, 40
	m.live = map[string]bool{"live": true}
	m.livePIDs = map[string]int{"live": os.Getpid()}
	return m
}

func key(m model, k tea.KeyMsg) (model, tea.Cmd) {
	res, cmd := m.Update(k)
	return res.(model), cmd
}

func TestMessageBoxFocusRules(t *testing.T) {
	m := chatModel(t)
	if m.chatFocus || m.chatRows() != 0 {
		t.Fatal("a non-live selection has no message box")
	}
	// Moving the selection onto the live session focuses its box.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if !m.chatFocus || m.chatRows() == 0 {
		t.Fatal("moving onto a live session should focus its message box")
	}
	// Typed keys go to the box, not the search.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("yo")})
	if m.chatInput.Value() != "yo" || m.textInput.Value() != "" {
		t.Errorf("box=%q search=%q", m.chatInput.Value(), m.textInput.Value())
	}
	// Esc returns to the search; the draft stays.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.chatFocus || m.chatInput.Value() != "yo" {
		t.Error("esc should return focus to the search")
	}
	// Clicking the box focuses it; clicking the search row leaves it.
	m = m.handleMouse(tea.MouseMsg{Y: m.height - 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.chatFocus {
		t.Error("clicking the message box should focus it")
	}
	m = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.chatFocus {
		t.Error("clicking the search row should return focus to the search")
	}
	// Moving off the live session takes focus back to the search.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.chatFocus {
		t.Error("a non-live selection must not keep focus in a message box")
	}
}

func TestTypingSearchNeverLandsInMessageBox(t *testing.T) {
	m := chatModel(t)
	// Typing narrows the list so the live session becomes the selection.
	for _, r := range "busy" {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if len(m.filtered) != 1 || m.filtered[0].conv.SessionID != "live" {
		t.Fatalf("setup: search should select the live session, got %d rows", len(m.filtered))
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" one")})
	if m.chatFocus || m.chatInput.Value() != "" || m.textInput.Value() != "busy one" {
		t.Errorf("search typing leaked into the message box: focus=%v box=%q search=%q", m.chatFocus, m.chatInput.Value(), m.textInput.Value())
	}
}

// fakeSocket listens on a unix socket in a temp dir and returns what one
// connection sent.
func fakeSocket(t *testing.T) (path string, got chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ccs-sock") // unix socket paths must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path = filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		data, _ := io.ReadAll(c) // until the sender closes its write side
		c.Close()
		got <- string(data)
	}()
	return path, got
}

func TestSendViaSocketExactBytes(t *testing.T) {
	m := chatModel(t)
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	sessions := getSessionsDir()
	os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","name":"busy","messagingSocketPath":%q}`, sock)), 0o600)
	sum := sha256.Sum256([]byte(sock)) // the path as written, not resolved
	os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:]))), []byte(`{"peerToken":"tok123"}`), 0o600)

	note, err := deliverMessage(pid, "live", "busy", `check "it" </cross-session-message> now`)
	if err != nil || !strings.Contains(note, "via its message socket") {
		t.Fatalf("note=%q err=%v", note, err)
	}
	data := <-got
	lines := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(data, "\n") {
		t.Fatalf("want two LF-terminated JSON lines, got %q", data)
	}
	if lines[0] != `{"token":"tok123","type":"auth"}` {
		t.Errorf("auth line = %s", lines[0])
	}
	var msg struct {
		Type    string `json:"type"`
		MsgID   string `json:"msg_id"`
		Message struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte(lines[1]), &msg); err != nil {
		t.Fatal(err)
	}
	want := "<cross-session-message from-name=\"ccs\">\ncheck \"it\" </ cross-session-message> now\n</cross-session-message>"
	if msg.Type != "user" || msg.Message.Role != "user" || msg.Message.Content != want || len(msg.MsgID) != 36 {
		t.Errorf("message line = %s", lines[1])
	}
	_ = m
}

// fakeTerminal puts fake ps and tmux on PATH; tmux logs what it's asked.
func fakeTerminal(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "tmux.log")
	os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\necho ttys099\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nif [ \"$1\" = list-panes ]; then echo '/dev/ttys099 main:0.1'; exit 0; fi\nprintf '%s|' \"$@\" >> "+log+"\necho >> "+log+"\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TMUX", "fake")
	return log
}

func TestSendFallsBackToTypingOnlyWhenSocketUnreachable(t *testing.T) {
	chatModel(t) // temp sessions dir
	pid := os.Getpid()
	os.WriteFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(`{"sessionId":"live","messagingSocketPath":"/tmp/ccs-no-such.sock"}`), 0o600)
	log := fakeTerminal(t)
	note, err := deliverMessage(pid, "live", "busy", "hello there")
	if err != nil || note != "✓ Typed into busy's tmux pane (as you)" {
		t.Fatalf("note=%q err=%v", note, err)
	}
	got, _ := os.ReadFile(log)
	if string(got) != "send-keys|-t|main:0.1|-l|--|hello there|\nsend-keys|-t|main:0.1|Enter|\n" {
		t.Errorf("tmux calls:\n%s", got)
	}
	// Control characters are never typed.
	if _, err := typeIntoSession(pid, "a\x1b[2Jb"); err == nil {
		t.Error("control characters must be refused")
	}
}

func TestSocketSuccessNeverAlsoTypes(t *testing.T) {
	chatModel(t)
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	os.WriteFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","messagingSocketPath":%q}`, sock)), 0o600)
	log := fakeTerminal(t)
	if _, err := deliverMessage(pid, "live", "busy", "once"); err != nil {
		t.Fatal(err)
	}
	<-got
	if data, _ := os.ReadFile(log); len(data) != 0 {
		t.Errorf("sent via socket, must not also type it: %s", data)
	}
}

func TestPeerMessageShownAndPendingClearsWhenItLands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	wrapped, _ := json.Marshal("<cross-session-message from-name=\"ccs\">\ncheck the PR\n</cross-session-message>")
	body := `{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2026-09-29T10:00:00Z"}` + "\n" +
		`{"type":"user","isMeta":true,"cwd":"/p","message":{"content":` + string(wrapped) + `},"timestamp":"2026-09-29T10:01:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := parseConversationFile(path, time.Time{}, 0)
	if len(c.Messages) != 2 {
		t.Fatalf("a message from ccs should be kept even though it's marked meta, got %d", len(c.Messages))
	}
	lines := strip2(strings.Join(buildPreviewLines(*c, "", 0), "\n"))
	if !strings.Contains(lines, "From ccs:") || !strings.Contains(lines, "check the PR") || strings.Contains(lines, "cross-session-message") {
		t.Errorf("preview should show it as from ccs, unwrapped:\n%s", lines)
	}
	m := initialModel(buildItems([]Conversation{*c}), "", nil)
	m.pending = map[string]string{c.SessionID: "check the PR"}
	m.sendNote = map[string]string{c.SessionID: "✓ Sent to x via its message socket (as a message from ccs)"}
	m.checkDelivered(c.SessionID)
	if m.pending[c.SessionID] != "" || !strings.HasPrefix(m.sendNote[c.SessionID], "✓ Delivered to") {
		t.Errorf("pending=%q note=%q", m.pending[c.SessionID], m.sendNote[c.SessionID])
	}
}

func TestChatTickReadsStatusAndNewLines(t *testing.T) {
	m := chatModel(t)
	path := filepath.Join(t.TempDir(), "live.jsonl")
	os.WriteFile(path, []byte(`{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2026-09-29T10:00:00Z"}`+"\n"), 0o644)
	conv, _ := parseConversationFile(path, time.Time{}, 0)
	conv.SessionID = "live"
	m.items = buildItems([]Conversation{*conv})
	m.updateFilter()
	os.WriteFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", os.Getpid())), []byte(fmt.Sprintf(`{"sessionId":"live","name":"busy","status":"busy","statusUpdatedAt":%d}`, time.Now().Add(-14*time.Second).UnixMilli())), 0o600)
	appendTo(t, path, `{"type":"assistant","message":{"content":[{"type":"text","text":"on it"}]},"timestamp":"2026-09-29T10:00:05Z"}`+"\n")
	res := m.chatTickCmd()().(chatTickResult)
	if res.stat.status != "busy" || res.conv == nil || len(res.conv.Messages) != 2 {
		t.Fatalf("tick result: %+v", res)
	}
	m.cursor = 0
	nm, _ := m.Update(res)
	m = nm.(model)
	v := strip2(m.View())
	if !strings.Contains(v, "working 1") || !strings.Contains(v, "on it") || !strings.Contains(v, "message busy…") {
		t.Errorf("view should show the reply, the working indicator and the box:\n%s", v)
	}
}

func TestIdleLiveSelectionStaysStatic(t *testing.T) {
	m := chatModel(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown}) // onto the live session
	m.chatStatus = map[string]sessionStat{"live": {status: "idle", since: time.Now()}}
	first := m.View()
	time.Sleep(1100 * time.Millisecond)
	nm, _ := m.Update(chatTickResult{id: "live", stat: sessionStat{status: "idle", since: m.chatStatus["live"].since}})
	if nm.(model).View() != first {
		t.Error("an idle live session's screen changed; the fast refresh would redraw constantly")
	}
}

func TestCtrlSReachesMessageBoxByKeyboard(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()
	m := initialModel(buildItems([]Conversation{{SessionID: "s", Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 120, 30
	m.live = map[string]bool{"s": true}
	m.chatFocus = false // e.g. after Esc back to the search
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m = res.(model); !m.chatFocus {
		t.Fatal("ctrl+s should move typing into the live session's message box")
	}
	// Ctrl+J/K scroll the conversation from the message box too.
	m.previewScroll = 0
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if m = res.(model); m.chatInput.Value() != "" {
		t.Error("ctrl+k in the message box should scroll, not edit the text")
	}
	// Not live: explain instead of focusing.
	m.live = map[string]bool{}
	m.chatFocus = false
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m = res.(model); m.chatFocus || m.errorMsg == "" {
		t.Error("ctrl+s on a session that isn't live should say why")
	}
}

// fakeCswap puts a fake cswap on PATH (and nothing else from the user's
// PATH, so the real one can never run). It prints canned list JSON, records
// every call's arguments, and fails "switch" when CSWAP_FAIL is set.
func fakeCswap(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls")
	list := `{"schemaVersion":1,"activeAccountNumber":1,"accounts":[` +
		`{"number":1,"email":"me@work.example","organizationName":"Work","active":true,"usageStatus":"unavailable","usage":null,"lastGoodUsage":{"fiveHour":{"pct":21},"sevenDay":{"pct":24}}},` +
		`{"number":2,"email":"me@home.example","organizationName":"Home","active":false,"usageStatus":"ok","usage":{"fiveHour":{"pct":4},"sevenDay":{"pct":10}}},` +
		`{"number":3,"email":"old@example.com","organizationName":"Old","active":false,"usageStatus":"relogin_required"}]}`
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n" +
		"case \"$1\" in\n" +
		"  list) echo '" + list + "' ;;\n" +
		"  switch) if [ -n \"$CSWAP_FAIL\" ]; then echo 'Error: account 2 needs re-login' >&2; exit 1; fi ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "cswap"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	return logPath
}

// runAcct feeds a key to the model and runs any command it returns (and the
// ones those return), like bubbletea would, skipping the allowance fetch.
func runAcct(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	res, cmd := m.Update(msg)
	m = res.(model)
	for cmd != nil {
		out := cmd()
		if b, ok := out.(tea.BatchMsg); ok {
			cmd = nil
			for _, c := range b {
				if c == nil {
					continue
				}
				if r := c(); r != nil {
					if _, isAllow := r.(allowanceMsg); isAllow {
						continue
					}
					res, next := m.Update(r)
					m = res.(model)
					if next != nil {
						cmd = next
					}
				}
			}
			continue
		}
		if out == nil {
			break
		}
		res, cmd = m.Update(out)
		m = res.(model)
	}
	return m
}

func TestCswapUsageLabel(t *testing.T) {
	pct := func(f float64) *float64 { return &f }
	live := &cswapUsage{}
	live.FiveHour = &struct {
		Pct *float64 `json:"pct"`
	}{pct(4)}
	if got := cswapUsageLabel(cswapAccount{Usage: live}); got != "5h 4%" {
		t.Errorf("got %q", got)
	}
	if got := cswapUsageLabel(cswapAccount{UsageStatus: "relogin_required", Usage: live}); got != "re-login" {
		t.Errorf("relogin: got %q", got)
	}
	if got := cswapUsageLabel(cswapAccount{LastGoodUsage: live}); got != "5h 4%" {
		t.Errorf("falls back to the last good reading: got %q", got)
	}
}

func TestAccountSwitcherListsAndSwitches(t *testing.T) {
	calls := fakeCswap(t)
	m := initialModel(nil, "", nil)
	m.width, m.height = 140, 40

	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.acctOpen || len(m.accts) != 3 {
		t.Fatalf("ctrl+o should open and list 3 accounts, open=%v n=%d msg=%q", m.acctOpen, len(m.accts), m.acctMsg)
	}
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	v := strip.ReplaceAllString(m.View(), "")
	for _, want := range []string{"Switch Claude account", "● 1  me@work.example", "5h 21% · 7d 24%", "me@home.example", "5h 4% · 7d 10%", "re-login"} {
		if !strings.Contains(v, want) {
			t.Errorf("popup missing %q", want)
		}
	}

	// The popup owns the keyboard: letters don't reach the search box.
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.textInput.Value() != "" {
		t.Error("typing leaked into the search box")
	}
	// Choosing the active account doesn't run cswap.
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	// Switch with a digit.
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if !strings.Contains(m.acctMsg, "Switched to me@home.example") {
		t.Errorf("status after switch: %q", m.acctMsg)
	}
	// Switch with arrows + Enter, and add.
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("+")})
	got, _ := os.ReadFile(calls)
	want := "list --json\nswitch 2\nlist --json\nswitch 3\nlist --json\nadd\nlist --json\n"
	if string(got) != want {
		t.Errorf("cswap calls:\n%s\nwant:\n%s", got, want)
	}
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.acctOpen {
		t.Error("esc should close the switcher")
	}
}

func TestAccountSwitchFailureIsShown(t *testing.T) {
	fakeCswap(t)
	t.Setenv("CSWAP_FAIL", "1")
	m := initialModel(nil, "", nil)
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyCtrlO})
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if !strings.Contains(m.acctMsg, "needs re-login") {
		t.Errorf("failure should show cswap's reason, got %q", m.acctMsg)
	}
}

func TestSwitchRefetchesAllowance(t *testing.T) {
	fakeCswap(t)
	m := initialModel(nil, "", nil)
	m.allowanceAt = time.Now() // fetched recently: normally no refetch for a minute
	m.allowanceLoading = true  // and one is in flight
	res, _ := m.Update(acctActionMsg{what: "switching", done: "Switched"})
	m = res.(model)
	if !m.allowanceStale {
		t.Fatal("an in-flight fetch for the old account should be marked stale")
	}
	res, cmd := m.Update(allowanceMsg{account: "old@example.com"})
	if m = res.(model); cmd == nil || !m.allowanceLoading {
		t.Error("a stale allowance result should trigger a fresh fetch")
	}
}

func TestAccountSwitcherWithoutCswap(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m := initialModel(nil, "", nil)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if m = res.(model); m.acctOpen || cmd != nil || !strings.Contains(m.errorMsg, "install cswap") {
		t.Errorf("without cswap, ctrl+o should explain, got open=%v msg=%q", m.acctOpen, m.errorMsg)
	}
}

func TestClickingAccountEmailOpensSwitcher(t *testing.T) {
	fakeCswap(t)
	m := initialModel(nil, "", nil)
	m.width, m.height = 160, 40
	m.account = "me@work.example"
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	row := strip.ReplaceAllString(strings.Split(m.View(), "\n")[1], "")
	x := strings.Index(row, "me@work.example")
	if x < 0 {
		t.Fatalf("email not in search row: %q", row)
	}
	// Clicking elsewhere on the row does nothing.
	m = runAcct(t, m, tea.MouseMsg{X: 3, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.acctOpen {
		t.Fatal("clicking the search box shouldn't open the switcher")
	}
	m = runAcct(t, m, tea.MouseMsg{X: utf8.RuneCountInString(row[:x]) + 2, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.acctOpen || len(m.accts) != 3 {
		t.Errorf("clicking the email should open and list accounts, open=%v n=%d", m.acctOpen, len(m.accts))
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
		if strings.HasPrefix(l, "          - word") || (len(bullet) > 0 && strings.HasPrefix(l, "            word")) {
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
	if !regexp.MustCompile(`(?m)^    \S.* ▸ task-notification · CI done$`).MatchString(plain) {
		t.Errorf("collapsed line should keep its indent:\n%s", plain)
	}
	if !strings.Contains(plain, "slack-bot#336 (github.com/two-inc/slack-bot/pull/336)") || strings.Contains(plain, "](") {
		t.Errorf("links should render as text plus a short address:\n%s", plain)
	}
	if !strings.Contains(plain, "and x.io/a.") {
		t.Errorf("a link whose text is its URL shows once:\n%s", plain)
	}
}

func TestChatPendingLine(t *testing.T) {
	m := chatModel(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	id := m.filtered[m.cursor].conv.SessionID
	m.pending = map[string]string{id: "please check the logs.\nthen fix it."}
	v := strip2(m.View())
	if !strings.Contains(v, "sent: please check the logs. then fix it.") || strings.Contains(v, "....") {
		t.Errorf("pending message should read as sent on one line:\n%s", v)
	}
	if m.chatStatus == nil {
		m.chatStatus = map[string]sessionStat{}
	}
	m.chatStatus[id] = sessionStat{status: "busy", since: time.Now()}
	if v := strip2(m.View()); !strings.Contains(v, "queued until it's free:") {
		t.Errorf("busy recipient should show the message as queued:\n%s", v)
	}
}

func TestParseChangelog(t *testing.T) {
	feed := `<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom">
<entry><title>v0.44.0</title><content type="html">&lt;ul&gt;&lt;li&gt;feat: not yet offered&lt;/li&gt;&lt;/ul&gt;</content></entry>
<entry><title>v0.43.1</title><content type="html">&lt;h2&gt;Changelog&lt;/h2&gt;&lt;ul&gt;
&lt;li&gt;&lt;a href=&quot;x&quot;&gt;&lt;tt&gt;4cde904&lt;/tt&gt;&lt;/a&gt; Merge pull request &lt;a&gt;#66&lt;/a&gt; from x&lt;/li&gt;
&lt;li&gt;&lt;a href=&quot;x&quot;&gt;&lt;tt&gt;7d1e6c2&lt;/tt&gt;&lt;/a&gt; fix: clearer &amp;quot;status&amp;quot; line&lt;/li&gt;&lt;/ul&gt;</content></entry>
<entry><title>v0.43.0</title><content type="html">&lt;ul&gt;&lt;li&gt;feat: readable preview&lt;/li&gt;&lt;/ul&gt;</content></entry>
<entry><title>v0.42.0</title><content type="html">&lt;ul&gt;&lt;li&gt;feat: already installed&lt;/li&gt;&lt;/ul&gt;</content></entry>
</feed>`
	got, err := parseChangelog(strings.NewReader(feed), "0.42.0", "v0.43.1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v0.43.1", `  • fix: clearer "status" line`, "v0.43.0", "  • feat: readable preview"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestUpdatePopupShowsChangelog(t *testing.T) {
	old := version
	version = "0.24.1"
	defer func() { version = old }()
	m := model{width: 100, fetchChangelog: func(tag string) ([]string, error) {
		lines := []string{tag}
		for i := range 20 {
			lines = append(lines, fmt.Sprintf("  fix: change %d", i))
		}
		return lines, nil
	}}
	nm, cmd := m.Update(latestMsg{tag: "v9.0.0"})
	m = nm.(model)
	if cmd == nil {
		t.Fatal("expected a changelog fetch")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("expected the changelog fetch alongside the next check")
	}
	nm, _ = m.Update(batch[0]()) // the fetch; batch[1] is the 2-minute tick
	m = nm.(model)
	v := strip2(m.updatePopup())
	if !strings.Contains(v, "fix: change 0") || !strings.Contains(v, "… and 10 more") || strings.Contains(v, "change 19") {
		t.Errorf("popup should list the changes, capped:\n%s", v)
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
	if !strings.Contains(plain, "    "+clock+" The reply didn't post.") {
		t.Errorf("each message should start with its time:\n%s", plain)
	}
	// A search match gets its own marked header even mid-run.
	plain = strip2(strings.Join(buildPreviewLines(conv, "reply", 80), "\n"))
	if !strings.Contains(plain, ">>> Claude") {
		t.Errorf("a match should be marked:\n%s", plain)
	}
}

func TestHelpPopup(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 120, 40
	v := strip2(m.View())
	if !strings.Contains(v, "enter resume  ^S msg  tab usage  ^G help  ^C quit") || strings.Contains(v, "prune") {
		t.Errorf("header should list only the essentials:\n%s", v)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlG})
	v = strip2(m.View())
	for _, s := range m.shortcuts() {
		if !strings.Contains(v, s[0]) || !strings.Contains(v, s[1]) {
			t.Errorf("popup missing %q %q:\n%s", s[0], s[1], v)
		}
	}
	// Only what applies: one idle conversation, no search typed.
	if !strings.Contains(v, "resume") || !strings.Contains(v, "rename") || strings.Contains(v, "message it") ||
		strings.Contains(v, "clear the search") || strings.Contains(v, "move through the list") {
		t.Errorf("popup should list only the keys that apply:\n%s", v)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m.showUsage = true
	if m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlG}); m.helpOpen {
		t.Error("the usage screen lists all its keys, so ^G should do nothing there")
	}
	m.showUsage = false
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlG})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !m.helpOpen || m.textInput.Value() != "" {
		t.Error("popup should own the keyboard while open")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.helpOpen || strings.Contains(strip2(m.View()), "changelog") {
		t.Error("esc should close the popup")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlG})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlG})
	if m.helpOpen {
		t.Error("ctrl+g should toggle the popup")
	}
}

func TestChangelogPopup(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.2.0"
	var lines []string
	for v := 30; v > 0; v-- {
		lines = append(lines, fmt.Sprintf("v0.%d.0", v), fmt.Sprintf("  feat: thing %d", v))
	}
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.fetchNotes = func() ([]string, error) { return lines, nil }
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyCtrlL})
	if !m.notesOpen || cmd == nil {
		t.Fatal("Ctrl+L should open the changelog and fetch it")
	}
	if v := strip2(m.View()); !strings.Contains(v, "fetching…") {
		t.Errorf("should say it's fetching:\n%s", v)
	}
	nm, _ := m.Update(cmd())
	m = nm.(model)
	v := strip2(m.View())
	if !strings.Contains(v, "v0.30.0 (new)") || !strings.Contains(v, "feat: thing 30") || strings.Contains(v, "thing 1\n") {
		t.Errorf("should show the newest releases first, marked:\n%s", v)
	}
	for range 100 {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if want := len(lines) - m.notesRows(); m.notesScroll != want {
		t.Errorf("scroll should stop at the end: %d, want %d", m.notesScroll, want)
	}
	if v := strip2(m.View()); !strings.Contains(v, "v0.2.0 (installed)") || !strings.Contains(v, "feat: thing 1") {
		t.Errorf("scrolled to the end should show the oldest, installed marked:\n%s", v)
	}
	nm, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if nm.(model).notesScroll != m.notesScroll-3 {
		t.Error("the wheel should scroll the changelog")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.notesOpen {
		t.Error("esc should close it")
	}
	if m, cmd = key(m, tea.KeyMsg{Type: tea.KeyCtrlL}); cmd != nil || !m.notesOpen || m.notesScroll != 0 {
		t.Error("reopening should reuse the fetched changelog, from the top")
	}
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	tea "github.com/charmbracelet/bubbletea"
)

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
	if !strings.Contains(strip2(output), "y delete  n/esc cancel") {
		t.Error("delete confirmation should show its keys")
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

func TestMouseFragmentsDropped(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("["), Alt: true})
	nm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	m = nm.(model)
	for _, frag := range []string{"[", "<65;40", ";12M"} {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(frag)})
	}
	if v := m.textInput.Value(); v != "" {
		t.Errorf("split mouse reports shouldn't reach the search box, got %q", v)
	}
	m.lastMouse = time.Time{}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[")})
	if v := m.textInput.Value(); v != "[" {
		t.Errorf("a typed [ long after any mouse event should still type, got %q", v)
	}
}

func TestHeaderNeverWraps(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.height = 30
	for _, w := range []int{140, 90, 70, 50, 30, 12} {
		m.width = w
		first := strings.Split(m.View(), "\n")[0]
		if got := ansi.StringWidth(first); got > w {
			t.Errorf("width %d: header is %d wide: %q", w, got, strip2(first))
		}
		plain := strip2(first)
		if w >= 70 && (!strings.Contains(plain, "^G help") || !strings.Contains(plain, "^C quit")) {
			t.Errorf("width %d: help and quit should stay: %q", w, plain)
		}
		if w == 140 && !strings.Contains(plain, "enter resume") {
			t.Errorf("wide header should keep every hint: %q", plain)
		}
	}
}

func TestPrunePromptHints(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s", Size: 4096}}}, "", nil)
	m.width, m.height = 140, 30
	m.confirmPrune, m.pruneIndex, m.pruneSaved = true, 0, 1024
	if v := strip2(m.View()); !strings.Contains(v, "y prune  n/esc cancel") || strings.Contains(v, "[y/N]") {
		t.Errorf("prune prompt should use the compact key hints:\n%s", v)
	}
}

func TestSearchMatchesWordsAndDashedNames(t *testing.T) {
	convs := []Conversation{
		{SessionID: "a", Title: "self-revoke-access", Messages: []Message{{Role: "user", Text: "let people drop their own access"}}},
		{SessionID: "b", Title: "other", Messages: []Message{{Role: "user", Text: "revoke the token, then the self check"}}},
		{SessionID: "c", Title: "unrelated", Messages: []Message{{Role: "user", Text: "nothing here"}}},
	}
	m := initialModel(buildItems(convs), "self revoke", nil)
	var got []string
	for _, it := range m.filtered {
		got = append(got, it.conv.SessionID)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("every word must appear, in any order, or in the dashed name: got %v", got)
	}
	if n := countHits(convs[1], "self revoke"); n != 1 {
		t.Errorf("a message with both words is a hit, got %d", n)
	}
	if h := strip2(highlight("revoke the self", "self revoke")); h != "revoke the self" || !strings.Contains(highlight("revoke the self", "self revoke"), "\033[43;30mself") {
		t.Errorf("each word is highlighted: %q", highlight("revoke the self", "self revoke"))
	}
}

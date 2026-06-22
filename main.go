package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var version = "dev"

// Message represents a conversation message
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Ts   string `json:"ts"`
}

// Conversation represents a parsed conversation
type Conversation struct {
	SessionID      string    `json:"session_id"`
	Title          string    `json:"title"`           // custom-title (user-set) or ai-title
	IsCustomTitle  bool      `json:"is_custom_title"` // true only when Title came from a user-set custom-title
	Cwd            string    `json:"cwd"`
	FirstTimestamp string    `json:"first_timestamp"`
	LastTimestamp  string    `json:"last_timestamp"`
	Messages       []Message `json:"messages"`
	FilePath       string    `json:"file_path"` // Full path to the .jsonl file
	Size           int64     `json:"size"`      // .jsonl file size in bytes
}

// RawMessage represents the JSON structure in conversation files
type RawMessage struct {
	Type    string `json:"type"`
	Cwd     string `json:"cwd"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Timestamp   string `json:"timestamp"`
	CustomTitle string `json:"customTitle"`
	AiTitle     string `json:"aiTitle"`
}

// TextContent for parsing content arrays
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// listItem holds display and search data for a conversation
type listItem struct {
	conv        Conversation
	searchText  string // All searchable content
	searchLower string // searchText lowercased once, for case-insensitive filtering
}

// selectedStyle highlights the cursor row. The rest of the UI is rendered with
// raw ANSI escapes in View/formatListItem/renderPreview.
var selectedStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("62")).
	Foreground(lipgloss.Color("230")).
	Bold(true)

// model is the bubbletea application state
type model struct {
	items          []listItem
	filtered       []listItem
	textInput      textinput.Model
	cursor         int
	previewScroll  int
	width          int
	height         int
	listHeight     int // Calculated list height for mouse detection
	selected       *Conversation
	quitting       bool
	claudeFlags    []string
	mouseInPreview bool // Track if mouse is in preview area
	confirmDelete  bool   // Are we in delete confirmation mode?
	deleteIndex    int    // Index of item to delete
	errorMsg       string // Show deletion errors
}

func initialModel(items []listItem, filterQuery string, claudeFlags []string) model {
	ti := textinput.New()
	ti.Placeholder = "type to search..."
	ti.Prompt = "> "
	ti.Focus()
	ti.SetValue(filterQuery)
	ti.Width = 40

	m := model{
		items:       items,
		textInput:   ti,
		claudeFlags: claudeFlags,
	}
	m.updateFilter()
	return m
}

func (m *model) updateFilter() {
	query := m.textInput.Value()
	if query == "" {
		// Make a copy to avoid sharing backing array with m.items
		m.filtered = make([]listItem, len(m.items))
		copy(m.filtered, m.items)
	} else {
		// Exact substring matching (case-insensitive)
		queryLower := strings.ToLower(query)
		m.filtered = make([]listItem, 0)
		for _, item := range m.items {
			if strings.Contains(item.searchLower, queryLower) {
				m.filtered = append(m.filtered, item)
			}
		}
	}
	// Keep cursor in bounds
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.previewScroll = 0
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Calculate list height for mouse detection
		m.listHeight = m.height * 30 / 100
		if m.listHeight < 3 {
			m.listHeight = 3
		}
		// Clear so a shrink doesn't leave wider stale rows behind.
		return m, tea.ClearScreen

	case tea.MouseMsg:
		// Determine if mouse is in preview area (below list + separator)
		listAreaHeight := 2 + m.listHeight // search line + separator + list
		m.mouseInPreview = msg.Y > listAreaHeight

		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.mouseInPreview {
				m.previewScroll = max(0, m.previewScroll-3)
			} else {
				if m.cursor > 0 {
					m.cursor--
					m.previewScroll = 0
				}
			}
			return m, nil
		case tea.MouseButtonWheelDown:
			if m.mouseInPreview {
				m.previewScroll = min(m.previewScroll+3, m.maxPreviewScroll())
			} else {
				if m.cursor < len(m.filtered)-1 {
					m.cursor++
					m.previewScroll = 0
				}
			}
			return m, nil
		}
		return m, nil

	case tea.KeyMsg:
		// Handle delete confirmation mode
		if m.confirmDelete {
			switch msg.String() {
			case "y", "Y":
				m.deleteConversation()
				return m, nil
			case "n", "N", "esc":
				m.confirmDelete = false
				return m, nil
			}
			return m, nil // Ignore all other keys
		}

		// Clear error message on any keypress in normal mode
		if m.errorMsg != "" {
			m.errorMsg = ""
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit

		case "enter":
			if len(m.filtered) > 0 {
				m.selected = &m.filtered[m.cursor].conv
			}
			m.quitting = true
			return m, tea.Quit

		case "ctrl+d":
			if len(m.filtered) > 0 {
				m.confirmDelete = true
				m.deleteIndex = m.cursor
			}
			return m, nil

		case "up", "ctrl+p":
			if m.cursor > 0 {
				m.cursor--
				m.previewScroll = 0
			}
			return m, nil

		case "down", "ctrl+n":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m.previewScroll = 0
			}
			return m, nil

		case "pgup", "ctrl+k":
			m.previewScroll = max(0, m.previewScroll-10)
			return m, nil

		case "pgdown", "ctrl+j":
			m.previewScroll = min(m.previewScroll+10, m.maxPreviewScroll())
			return m, nil

		case "ctrl+u":
			m.textInput.SetValue("")
			m.updateFilter()
			return m, nil
		}
	}

	// Update text input
	var cmd tea.Cmd
	prevValue := m.textInput.Value()
	m.textInput, cmd = m.textInput.Update(msg)
	if m.textInput.Value() != prevValue {
		m.updateFilter()
	}
	return m, cmd
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	var b strings.Builder

	// The list spans the full terminal width; TOPIC flexes to fill it.
	tableWidth := m.width

	// Title line with help right-aligned
	title := fmt.Sprintf("ccs · claude code search · %s", version)
	help := "Resume:Enter Delete:Ctrl+D Scroll:Ctrl+J/K Exit:Esc"
	titlePadding := tableWidth - 2 - len(title) - len(help)
	if titlePadding < 1 {
		titlePadding = 1
	}
	b.WriteString(fmt.Sprintf("  \033[1;36mccs\033[0m \033[90m· claude code search · %s%s%s\033[0m\n",
		version, strings.Repeat(" ", titlePadding), help))

	// Search line or delete confirmation
	var sections []string
	var inputSection string
	if m.confirmDelete {
		topic := getTopic(m.filtered[m.deleteIndex].conv)
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // Red
			Render(fmt.Sprintf("Delete conversation \"%s\"? [y/N]", truncate(topic, 50)))
		sections = append(sections, "  "+inputSection)
	} else {
		count := fmt.Sprintf("(%d/%d)", len(m.filtered), len(m.items))
		searchPadding := tableWidth - 2 - 2 - 40 - len(count) - 1 // 2 for indent, 2 for "> ", 40 for textInput, -1 to shift left
		if searchPadding < 1 {
			searchPadding = 1
		}
		inputSection = fmt.Sprintf("  %s%s\033[90m%s\033[0m", m.textInput.View(), strings.Repeat(" ", searchPadding), count)
		sections = append(sections, inputSection)
	}

	// Show error message if set
	if m.errorMsg != "" {
		errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
		sections = append(sections, "  "+errorStyle.Render(m.errorMsg))
	}

	b.WriteString(strings.Join(sections, "\n"))
	b.WriteString("\n\n")

	// Calculate heights
	listHeight := m.height * 30 / 100
	if listHeight < 3 {
		listHeight = 3
	}
	previewHeight := m.height - listHeight - 6 // 6 for title + search + blank + header + borders

	// Column headers
	b.WriteString(fmt.Sprintf("  \033[90m%-*s  %-*s  %-*s  %*s  %*s  %*s\033[0m\n",
		colDate, "DATE", colProject, "PROJECT", m.topicColWidth(), "TOPIC", colMsgs, "MSGS", colHits, "HITS", colSize, "SIZE"))
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	visibleItems := listHeight
	start := 0
	if m.cursor >= visibleItems {
		start = m.cursor - visibleItems + 1
	}

	for i := start; i < min(start+visibleItems, len(m.filtered)); i++ {
		item := m.filtered[i]
		isSelected := i == m.cursor
		line := m.formatListItem(item, isSelected)

		if isSelected {
			// Pad to full width for selection highlight
			line = padRight("> "+line, m.width)
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString("  " + line)
		}
		b.WriteString("\n")
	}

	// Fill remaining list space
	for i := len(m.filtered) - start; i < visibleItems; i++ {
		b.WriteString("\n")
	}

	// Preview section
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	if len(m.filtered) > 0 {
		preview := m.renderPreview(m.filtered[m.cursor], previewHeight)
		b.WriteString(preview)
	}

	return b.String()
}

// Fixed list column widths. TOPIC is the flex column - it absorbs the rest of
// the terminal width (see topicColWidth).
const (
	colDate    = 16
	colProject = 22
	colMsgs    = 5
	colHits    = 4
	colSize    = 6
	colGap     = 2 // spaces between columns
	listIndent = 2 // leading "  " / "> " on each row
	numGaps    = 5
)

// topicColWidth flexes the TOPIC column to fill the terminal width.
func (m model) topicColWidth() int {
	used := listIndent + colDate + colProject + colMsgs + colHits + colSize + numGaps*colGap
	if w := m.width - used; w > 10 {
		return w
	}
	return 10
}

func (m model) formatListItem(item listItem, selected bool) string {
	ts := formatTimestamp(item.conv.LastTimestamp)
	project := item.conv.Cwd
	if idx := strings.LastIndex(project, "/"); idx >= 0 {
		project = project[idx+1:]
	}
	project = truncate(project, colProject)

	// Mark only user-set custom titles. Claude auto-generates an ai-title for
	// almost every session, so marking any title would flag nearly every row;
	// the ✎ should mean "you named this". ponytail: the glyph is ambiguous-width,
	// so a marked row may sit one cell narrow on CJK-width terminals - cosmetic
	// only, truncate is rune-safe.
	topic := getTopic(item.conv)
	if item.conv.IsCustomTitle {
		topic = "✎ " + topic
	}
	tw := m.topicColWidth()
	topic = truncate(topic, tw)

	// Message count
	msgs := len(item.conv.Messages)

	// Count messages containing the query
	query := m.textInput.Value()
	hits := 0
	if query != "" {
		queryLower := strings.ToLower(query)
		for _, msg := range item.conv.Messages {
			if strings.Contains(strings.ToLower(msg.Text), queryLower) {
				hits++
			}
		}
	}

	size := formatBytes(item.conv.Size)

	// Format: date | project | topic | msgs | hits | size (aligned columns)
	if selected {
		return fmt.Sprintf("%-*s  %-*s  %-*s  %*d  %*d  %*s",
			colDate, ts, colProject, project, tw, topic, colMsgs, msgs, colHits, hits, colSize, size)
	}
	return fmt.Sprintf("\033[90m%-*s\033[0m  \033[1;33m%-*s\033[0m  %-*s  %*d  \033[36m%*d\033[0m  \033[35m%*s\033[0m",
		colDate, ts, colProject, project, tw, topic, colMsgs, msgs, colHits, hits, colSize, size)
}

// buildPreviewLines builds the scrollable message lines of a conversation
// preview (everything below the fixed header). Shared by renderPreview and
// maxPreviewScroll so the render and the scroll-clamp can never disagree on how
// far the preview can scroll.
func buildPreviewLines(conv Conversation, query string) []string {
	var msgLines []string

	// Find messages containing the query
	queryLower := strings.ToLower(query)
	matchSet := make(map[int]bool)
	if query != "" {
		for i, msg := range conv.Messages {
			if strings.Contains(strings.ToLower(msg.Text), queryLower) {
				matchSet[i] = true
			}
		}
	}

	// Build set of indices to show
	showSet := make(map[int]bool)

	// Always show first 2 and last 2 messages
	for i := 0; i < 2 && i < len(conv.Messages); i++ {
		showSet[i] = true
	}
	for i := len(conv.Messages) - 2; i < len(conv.Messages); i++ {
		if i >= 0 {
			showSet[i] = true
		}
	}

	// Add matches with context
	for idx := range matchSet {
		if idx > 0 {
			showSet[idx-1] = true
		}
		showSet[idx] = true
		if idx < len(conv.Messages)-1 {
			showSet[idx+1] = true
		}
	}

	// Display messages with gaps
	lastShown := -1
	for i := 0; i < len(conv.Messages); i++ {
		if !showSet[i] {
			continue
		}

		if lastShown >= 0 && i > lastShown+1 {
			skipped := i - lastShown - 1
			msgLines = append(msgLines, fmt.Sprintf("\033[90m    ... %d messages ...\033[0m", skipped))
			msgLines = append(msgLines, "")
		} else if lastShown == -1 && i > 0 {
			msgLines = append(msgLines, fmt.Sprintf("\033[90m    ... %d earlier messages\033[0m", i))
			msgLines = append(msgLines, "")
		}

		msg := conv.Messages[i]
		ts := formatTimestamp(msg.Ts)
		var prefix string
		if matchSet[i] {
			if msg.Role == "user" {
				prefix = fmt.Sprintf("\033[1;32m>>> %s User:\033[0m", ts) // Bold green
			} else {
				prefix = fmt.Sprintf("\033[1;34m>>> %s Claude:\033[0m", ts) // Bold blue
			}
		} else {
			if msg.Role == "user" {
				prefix = fmt.Sprintf("\033[32m    %s User:\033[0m", ts) // Green
			} else {
				prefix = fmt.Sprintf("\033[34m    %s Claude:\033[0m", ts) // Blue
			}
		}

		msgLines = append(msgLines, prefix)
		text := msg.Text
		if r := []rune(text); len(r) > 500 {
			text = string(r[:500]) + "... (truncated)" // slice on runes, not bytes
		}
		for _, line := range strings.Split(text, "\n") {
			msgLines = append(msgLines, "    "+highlight(line, query))
		}
		msgLines = append(msgLines, "")

		lastShown = i
	}

	if lastShown < len(conv.Messages)-1 {
		remaining := len(conv.Messages) - lastShown - 1
		msgLines = append(msgLines, fmt.Sprintf("\033[90m    ... %d more messages\033[0m", remaining))
	}

	return msgLines
}

// maxPreviewScroll is the furthest the preview of the current selection can
// scroll - one line short of the rendered message-line count.
func (m model) maxPreviewScroll() int {
	if len(m.filtered) == 0 {
		return 0
	}
	lines := buildPreviewLines(m.filtered[m.cursor].conv, m.textInput.Value())
	return max(0, len(lines)-1)
}

func (m model) renderPreview(item listItem, height int) string {
	query := m.textInput.Value()
	conv := item.conv

	// Fixed header (always visible)
	var header []string
	header = append(header, "\033[1;33mProject:\033[0m "+highlight(conv.Cwd, query))
	if conv.Title != "" {
		header = append(header, "\033[1;33mName:\033[0m    "+highlight(conv.Title, query))
	}
	header = append(header, "\033[1;33mSession:\033[0m "+highlight(conv.SessionID, query))
	header = append(header, "")

	msgLines := buildPreviewLines(conv, query)

	// Apply scroll to messages only (header stays fixed). Clamp locally for this
	// render; the persisted m.previewScroll is bounded in Update via
	// maxPreviewScroll (this method has a value receiver, so a write here would
	// be discarded).
	msgHeight := height - len(header)
	if msgHeight < 1 {
		msgHeight = 1
	}
	scroll := min(m.previewScroll, max(0, len(msgLines)-1))
	end := min(scroll+msgHeight, len(msgLines))
	visibleMsgLines := msgLines[scroll:end]

	// Combine header + scrolled messages
	allLines := append(header, visibleMsgLines...)
	return strings.Join(allLines, "\n")
}

func highlight(text, query string) string {
	if query == "" {
		return text
	}
	tr := []rune(text)
	lr := []rune(strings.ToLower(text))
	queryLower := strings.ToLower(query)
	qr := []rune(queryLower)

	// Match on runes so multibyte text (CJK, emoji) is never sliced mid-rune.
	// ponytail: a handful of runes change length when lowercased (İ, Kelvin K),
	// which breaks the lr/tr index alignment - bail to plain text rather than
	// emit corrupted bytes. Highlighting those is not worth the complexity.
	if len(lr) != len(tr) || len(qr) == 0 {
		return text
	}

	var result strings.Builder
	for i := 0; i < len(tr); {
		if i+len(qr) <= len(tr) && string(lr[i:i+len(qr)]) == queryLower {
			// Yellow background, black text for highlight
			result.WriteString("\033[43;30m")
			result.WriteString(string(tr[i : i+len(qr)]))
			result.WriteString("\033[0m")
			i += len(qr)
		} else {
			result.WriteRune(tr[i])
			i++
		}
	}
	return result.String()
}

func padRight(s string, length int) string {
	r := []rune(s)
	if len(r) >= length {
		return string(r[:length])
	}
	// ponytail: pads by rune count, not display width; CJK/emoji rows can still
	// look a cell narrow. Swap in go-runewidth if column alignment matters.
	return s + strings.Repeat(" ", length-len(r))
}

// ============================================================================
// Data loading (preserved from original)
// ============================================================================

// getProjectsDir returns the path to the Claude projects directory
// Declared as a variable so it can be overridden in tests
var getProjectsDir = func() string {
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





func parseConversationFile(path string, cutoff time.Time, maxSize int64) (*Conversation, error) {
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

	sessionID := strings.TrimSuffix(info.Name(), ".jsonl")
	conv := &Conversation{
		SessionID: sessionID,
		FilePath:  path,
		Size:      info.Size(),
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// A single JSONL line holds a whole turn - a big tool result or a base64
	// image can be tens of MB. ponytail: 64MB ceiling; if a line ever exceeds
	// it the scanner.Err() check below skips the file rather than silently
	// truncating the parse.
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)

	for scanner.Scan() {
		lineBytes := scanner.Bytes()

		var raw RawMessage
		if err := json.Unmarshal(lineBytes, &raw); err != nil {
			continue
		}

		if raw.Type == "custom-title" {
			conv.Title = raw.CustomTitle // user-set name wins over ai-title
			conv.IsCustomTitle = raw.CustomTitle != ""
		} else if raw.Type == "ai-title" {
			if conv.Title == "" {
				conv.Title = raw.AiTitle
			}
		} else if raw.Type == "user" {
			if conv.Cwd == "" {
				conv.Cwd = raw.Cwd
			}
			text := extractText(raw.Message.Content)
			if strings.TrimSpace(text) != "" {
				if conv.FirstTimestamp == "" {
					conv.FirstTimestamp = raw.Timestamp
				}
				conv.Messages = append(conv.Messages, Message{
					Role: "user",
					Text: text,
					Ts:   raw.Timestamp,
				})
			}
		} else if raw.Type == "assistant" {
			text := extractText(raw.Message.Content)
			if strings.TrimSpace(text) != "" {
				conv.Messages = append(conv.Messages, Message{
					Role: "assistant",
					Text: text,
					Ts:   raw.Timestamp,
				})
			}
		}
	}

	// A scan error (e.g. a line over the buffer cap) leaves the parse partial.
	// Surface it instead of trusting a silently-truncated conversation.
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	if len(conv.Messages) == 0 {
		return nil, nil
	}

	conv.LastTimestamp = conv.Messages[len(conv.Messages)-1].Ts

	if conv.Cwd == "" {
		conv.Cwd = "unknown"
	}

	return conv, nil
}

func getConversations(cutoff time.Time, maxSize int64, excludeDirs []string) ([]Conversation, error) {
	projectsDir := getProjectsDir()

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
	jobs := make(chan string, len(files))
	results := make(chan *Conversation, len(files))

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				conv, err := parseConversationFile(path, cutoff, maxSize)
				if err == nil && conv != nil {
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

func formatTimestamp(ts string) string {
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

// formatBytes renders a byte count compactly (fits the 6-wide SIZE column).
func formatBytes(n int64) string {
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

func truncate(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	if maxLen < 3 {
		return string(r[:maxLen]) // no room for the ellipsis
	}
	return string(r[:maxLen-3]) + "..."
}

// getTopic returns the session name (custom/ai title), else first user message, else session ID
func getTopic(conv Conversation) string {
	if conv.Title != "" {
		return conv.Title
	}
	for _, msg := range conv.Messages {
		if msg.Role == "user" {
			return msg.Text
		}
	}
	return conv.SessionID
}

// deleteConversation removes the selected conversation from disk and UI
func (m *model) deleteConversation() {
	if m.deleteIndex >= len(m.filtered) {
		return
	}

	conv := m.filtered[m.deleteIndex].conv

	// Delete the file (ignore if already deleted)
	if err := os.Remove(conv.FilePath); err != nil && !os.IsNotExist(err) {
		m.errorMsg = fmt.Sprintf("Delete failed: %v", err)
		m.confirmDelete = false
		return
	}

	// Remove from filtered slice
	m.filtered = append(m.filtered[:m.deleteIndex], m.filtered[m.deleteIndex+1:]...)

	// Remove from items slice (find by SessionID)
	for i, item := range m.items {
		if item.conv.SessionID == conv.SessionID {
			m.items = append(m.items[:i], m.items[i+1:]...)
			break
		}
	}

	// Adjust cursor
	if len(m.filtered) == 0 {
		m.cursor = 0
	} else if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	// Otherwise cursor stays at same position (shows next item)

	// Exit confirmation mode
	m.confirmDelete = false
	m.errorMsg = ""
}

// buildItems creates list items from conversations
func buildItems(conversations []Conversation) []listItem {
	items := make([]listItem, 0, len(conversations))

	for _, conv := range conversations {
		// Build search text from all content
		var searchParts []string
		searchParts = append(searchParts, conv.SessionID)
		searchParts = append(searchParts, conv.Title)
		searchParts = append(searchParts, conv.Cwd)
		searchParts = append(searchParts, formatTimestamp(conv.FirstTimestamp))
		searchParts = append(searchParts, formatTimestamp(conv.LastTimestamp))

		// Include assistant messages too so a conversation is findable by
		// what Claude said, matching the HITS column and preview which already
		// count all messages.
		for _, msg := range conv.Messages {
			searchParts = append(searchParts, msg.Text)
		}

		searchText := strings.Join(searchParts, " ")
		items = append(items, listItem{
			conv:        conv,
			searchText:  searchText,
			searchLower: strings.ToLower(searchText),
		})
	}

	return items
}

// ============================================================================
// Prune - shrink conversation files by removing duplicate / redundant data
// ============================================================================

// pruneOpts selects which categories of redundant data to remove. Conversation
// (user/assistant) messages are never touched.
type pruneOpts struct {
	dropSnapshots    bool // drop file-history-snapshot lines (rewind/checkpoint backups)
	stripToolResults bool // remove the toolUseResult field (a copy of the tool_result already in message.content)
}

type pruneStats struct {
	bytesIn          int64
	bytesOut         int64
	droppedSnapshots int
	strippedResults  int
	convLinesIn      int // user/assistant lines seen
	convLinesOut     int // ... and kept (invariant: must equal convLinesIn)
}

// pruneLine applies the transforms to one JSONL line. It returns the output
// bytes (nil = drop the line), the line's "type", and whether it was
// dropped/stripped. Unparseable lines pass through verbatim.
func pruneLine(line []byte, opts pruneOpts) (out []byte, typ string, dropped, stripped bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		return line, "", false, false
	}
	if raw, ok := obj["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}
	if opts.dropSnapshots && typ == "file-history-snapshot" {
		return nil, typ, true, false
	}
	if opts.stripToolResults {
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
func pruneStream(r io.Reader, w io.Writer, opts pruneOpts) (pruneStats, error) {
	var st pruneStats
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		st.bytesIn += int64(len(line)) + 1
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
		st.bytesOut += int64(len(out)) + 1
	}
	return st, scanner.Err()
}

// pruneFile prunes one conversation file. With write=true it streams to
// <path>.pruned and atomically replaces path, aborting (no replace) if the
// conversation line count would change. With write=false it only measures.
func pruneFile(path string, write bool, opts pruneOpts) (pruneStats, error) {
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
	err := filepath.Walk(getProjectsDir(), func(path string, info os.FileInfo, err error) error {
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

func runPrune(args []string) {
	apply, yes := false, false // dry run by default; --apply to actually rewrite
	minSizeMB := int64(50)
	opts := pruneOpts{dropSnapshots: true, stripToolResults: true}
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
			opts.dropSnapshots = false
		case a == "--no-tool-results":
			opts.stripToolResults = false
		case strings.HasPrefix(a, "--min-size="):
			fmt.Sscanf(strings.TrimPrefix(a, "--min-size="), "%d", &minSizeMB)
		default:
			fmt.Fprintf(os.Stderr, "unknown prune flag: %s (try ccs prune --help)\n", a)
			os.Exit(2)
		}
	}
	if !opts.dropSnapshots && !opts.stripToolResults {
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
		saved := st.bytesIn - st.bytesOut
		fmt.Printf("  %-46s %8s -> %8s  (-%s)\n", shortPath(path), formatBytes(st.bytesIn), formatBytes(st.bytesOut), formatBytes(saved))
	}

	if !apply {
		fmt.Printf("Dry run (no changes). Conversations >= %dMB:\n\n", minSizeMB)
		var in, out int64
		for _, f := range files {
			st, err := pruneFile(f, false, opts)
			if err != nil {
				fmt.Printf("  %-46s error: %v\n", shortPath(f), err)
				continue
			}
			report(st, f)
			in += st.bytesIn
			out += st.bytesOut
		}
		fmt.Printf("\nWould reclaim %s across %d files. Re-run with --apply to prune.\n", formatBytes(in-out), len(files))
		return
	}

	if !yes {
		var total int64
		for _, f := range files {
			if info, e := os.Stat(f); e == nil {
				total += info.Size()
			}
		}
		fmt.Printf("Prune %d conversations (%s)? Rewrites them in place, removing duplicate tool\nresults and snapshot backups - dialogue is preserved. [y/N] ", len(files), formatBytes(total))
		var resp string
		fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" {
			fmt.Println("Aborted.")
			return
		}
	}

	var in, out int64
	for _, f := range files {
		st, err := pruneFile(f, true, opts)
		if err != nil {
			fmt.Printf("  %-46s FAILED: %v\n", shortPath(f), err)
			continue
		}
		report(st, f)
		in += st.bytesIn
		out += st.bytesOut
	}
	fmt.Printf("\nReclaimed %s across %d files.\n", formatBytes(in-out), len(files))
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

func printHelp() {
	fmt.Printf(`ccs v%s - Claude Code Search

Search and resume Claude Code conversations.

Usage: ccs [filter] [-- claude-flags...]
       ccs prune [flags]    Shrink large conversations (see ccs prune --help)

Arguments:
  filter           Initial search query (optional)
  -- claude-flags  Flags to pass to 'claude --resume' (after --)

Flags:
  -h, --help       Show this help message
  -v, --version    Show version
  --max-age=N      Only search last N days (default: 60, 0 = no limit)
  --max-size=N     Max file size in MB (default: 1024, 0 = no limit)
  --all            Include everything (same as --max-age=0 --max-size=0)
  --exclude=a,b    Exclude dirs containing these strings (default: observer-sessions)
  --dump [query]   Debug: print all search items (with optional highlighting)

Examples:
  ccs                                Search last 60 days, files <1GB (default)
  ccs --max-age=7                    Search last 7 days only
  ccs --all                          Search everything (all time, all files)
  ccs buyer                          Search with initial query "buyer"
  ccs -- --plan                      Resume with plan mode
  ccs buyer -- --plan                Search "buyer", resume with plan mode

Key bindings:
  ↑/↓, Ctrl+P/N   Navigate list
  Enter           Select and resume conversation
  Ctrl+D          Delete conversation (with confirmation)
  Ctrl+J/K        Scroll preview
  Mouse wheel     Scroll list or preview (based on position)
  Ctrl+U          Clear search
  Esc, Ctrl+C     Quit

`, version)
}

func main() {
	args := os.Args[1:]

	if len(args) > 0 && args[0] == "prune" {
		runPrune(args[1:])
		return
	}

	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			printHelp()
			return
		}
		if arg == "-v" || arg == "--version" {
			fmt.Printf("ccs v%s\n", version)
			return
		}
	}

	// Parse flags
	maxAgeDays := 60        // Default to 60 days
	maxSizeMB := int64(1024) // Default to 1GB
	excludeDirs := []string{"observer-sessions"}
	for _, arg := range args {
		if arg == "--all" {
			maxAgeDays = 0
			maxSizeMB = 0
		} else if strings.HasPrefix(arg, "--max-age=") {
			val := strings.TrimPrefix(arg, "--max-age=")
			fmt.Sscanf(val, "%d", &maxAgeDays)
		} else if strings.HasPrefix(arg, "--max-size=") {
			val := strings.TrimPrefix(arg, "--max-size=")
			fmt.Sscanf(val, "%d", &maxSizeMB)
		} else if strings.HasPrefix(arg, "--exclude=") {
			val := strings.TrimPrefix(arg, "--exclude=")
			excludeDirs = strings.Split(val, ",")
		}
	}

	// Convert to bytes (0 means no limit)
	maxSize := maxSizeMB * 1024 * 1024

	// Calculate cutoff time (0 means no limit)
	var cutoff time.Time
	if maxAgeDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -maxAgeDays)
	}

	// Debug mode - dump search lines
	for i, arg := range args {
		if arg == "--dump" {
			filter := ""
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				filter = args[i+1]
			}
			conversations, _ := getConversations(cutoff, maxSize, excludeDirs)
			items := buildItems(conversations)
			for _, item := range items {
				line := item.searchText
				if filter != "" {
					line = highlight(line, filter)
				}
				fmt.Println(line)
			}
			return
		}
	}

	// Parse args: positional arg is filter, args after -- go to claude
	var claudeFlags []string
	var filterQuery string
	for i, arg := range args {
		if arg == "--" {
			claudeFlags = args[i+1:]
			break
		}
		// Skip our flags when looking for filter query
		if arg == "--all" || strings.HasPrefix(arg, "--max-age=") || strings.HasPrefix(arg, "--max-size=") || strings.HasPrefix(arg, "--exclude=") {
			continue
		}
		if !strings.HasPrefix(arg, "-") && filterQuery == "" {
			filterQuery = arg
		}
	}

	projectsDir := getProjectsDir()
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Projects directory not found: %s\n", projectsDir)
		fmt.Fprintf(os.Stderr, "Make sure Claude Code is installed and has been used at least once.\n")
		os.Exit(1)
	}

	fmt.Fprint(os.Stderr, "Loading conversations...")
	conversations, err := getConversations(cutoff, maxSize, excludeDirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\rError loading conversations: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprint(os.Stderr, "\r                         \r")

	if len(conversations) == 0 {
		fmt.Fprintf(os.Stderr, "No conversations found\n")
		os.Exit(1)
	}

	items := buildItems(conversations)
	if len(items) == 0 {
		fmt.Fprintf(os.Stderr, "No searchable messages found\n")
		os.Exit(1)
	}

	// Run TUI
	m := initialModel(items, filterQuery, claudeFlags)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	final := finalModel.(model)
	if final.selected == nil {
		return
	}

	conv := final.selected
	cwd := conv.Cwd
	if cwd == "" || cwd == "unknown" {
		cwd = "."
	}

	// Change directory before announcing the resume, and fail loudly rather
	// than launching claude in the wrong directory (which silently gives it the
	// wrong project config / MCP servers).
	if err := os.Chdir(cwd); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot resume in %s: %v\n", cwd, err)
		os.Exit(1)
	}

	fmt.Printf("\033[1mResuming conversation %s in %s...\033[0m\n", conv.SessionID, cwd)
	if len(claudeFlags) > 0 {
		fmt.Printf("\033[90mFlags: %s\033[0m\n", strings.Join(claudeFlags, " "))
	}
	fmt.Println()

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude not found in PATH\n")
		os.Exit(1)
	}

	execArgs := []string{"claude", "--resume", conv.SessionID}
	execArgs = append(execArgs, claudeFlags...)

	syscall.Exec(claudePath, execArgs, os.Environ())
}

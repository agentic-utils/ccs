package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	Cwd            string    `json:"cwd"`
	FirstTimestamp string    `json:"first_timestamp"`
	LastTimestamp  string    `json:"last_timestamp"`
	Messages       []Message `json:"messages"`
}

// RawMessage represents the JSON structure in conversation files
type RawMessage struct {
	Type    string `json:"type"`
	Cwd     string `json:"cwd"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Timestamp string `json:"timestamp"`
}

// TextContent for parsing content arrays
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// listItem holds display and search data for a conversation
type listItem struct {
	conv       Conversation
	searchText string // All searchable content
	display    string // What to show in the list
}

// Styles
var (
	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("62")).
			Foreground(lipgloss.Color("230")).
			Bold(true)

	normalStyle = lipgloss.NewStyle()

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	projectStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")).
			Bold(true)

	headerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")).
			Bold(true)

	userStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("70"))

	assistantStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("68"))

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))
)

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
		m.filtered = m.items
	} else {
		// Exact substring matching (case-insensitive)
		queryLower := strings.ToLower(query)
		m.filtered = make([]listItem, 0)
		for _, item := range m.items {
			if strings.Contains(strings.ToLower(item.searchText), queryLower) {
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
		return m, nil

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
				m.previewScroll += 3
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
			m.previewScroll += 10
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

	// Table width: 2 + 16 + 2 + 22 + 2 + 40 + 2 + 5 + 2 + 4 = 97
	tableWidth := 97

	// Title line with help right-aligned
	title := fmt.Sprintf("ccs · claude code search · %s", version)
	help := "↑/↓ Enter Ctrl+J/K Esc"
	titlePadding := tableWidth - 2 - len(title) - len(help)
	if titlePadding < 1 {
		titlePadding = 1
	}
	b.WriteString(fmt.Sprintf("  \033[1;36mccs\033[0m \033[90m· claude code search · %s%s%s\033[0m\n",
		version, strings.Repeat(" ", titlePadding), help))

	// Search line with count right-aligned
	count := fmt.Sprintf("(%d/%d)", len(m.filtered), len(m.items))
	searchPadding := tableWidth - 2 - 2 - 40 - len(count) - 1 // 2 for indent, 2 for "> ", 40 for textInput, -1 to shift left
	if searchPadding < 1 {
		searchPadding = 1
	}
	b.WriteString(fmt.Sprintf("  %s%s\033[90m%s\033[0m\n\n",
		m.textInput.View(), strings.Repeat(" ", searchPadding), count))

	// Calculate heights
	listHeight := m.height * 30 / 100
	if listHeight < 3 {
		listHeight = 3
	}
	previewHeight := m.height - listHeight - 6 // 6 for title + search + blank + header + borders

	// Column headers
	b.WriteString(fmt.Sprintf("  \033[90m%-16s  %-22s  %-40s  %5s  %4s\033[0m\n", "DATE", "PROJECT", "TOPIC", "MSGS", "HITS"))
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

func (m model) formatListItem(item listItem, selected bool) string {
	ts := formatTimestamp(item.conv.LastTimestamp)
	project := item.conv.Cwd
	if idx := strings.LastIndex(project, "/"); idx >= 0 {
		project = project[idx+1:]
	}
	// Truncate project name to fit column
	if len(project) > 22 {
		project = project[:19] + "..."
	}

	// Use first user message as topic
	topic := ""
	for _, msg := range item.conv.Messages {
		if msg.Role == "user" {
			topic = truncate(msg.Text, 40)
			break
		}
	}

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

	// Format: date | project | topic | msgs | hits (aligned columns)
	if selected {
		return fmt.Sprintf("%-16s  %-22s  %-40s  %5d  %4d", ts, project, topic, msgs, hits)
	}
	return fmt.Sprintf("\033[90m%-16s\033[0m  \033[1;33m%-22s\033[0m  %-40s  %5d  \033[36m%4d\033[0m",
		ts, project, topic, msgs, hits)
}

func (m model) renderPreview(item listItem, height int) string {
	query := m.textInput.Value()
	conv := item.conv

	// Fixed header (always visible)
	var header []string
	header = append(header, "\033[1;33mProject:\033[0m "+highlight(conv.Cwd, query))
	header = append(header, "\033[1;33mSession:\033[0m "+highlight(conv.SessionID, query))
	header = append(header, "")

	// Build message lines (scrollable)
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
		if len(text) > 500 {
			text = text[:500] + "... (truncated)"
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

	// Apply scroll to messages only (header stays fixed)
	msgHeight := height - len(header)
	if msgHeight < 1 {
		msgHeight = 1
	}
	if m.previewScroll >= len(msgLines) {
		m.previewScroll = max(0, len(msgLines)-1)
	}
	end := min(m.previewScroll+msgHeight, len(msgLines))
	visibleMsgLines := msgLines[m.previewScroll:end]

	// Combine header + scrolled messages
	allLines := append(header, visibleMsgLines...)
	return strings.Join(allLines, "\n")
}

func highlight(text, query string) string {
	if query == "" {
		return text
	}
	lower := strings.ToLower(text)
	queryLower := strings.ToLower(query)

	// Find all occurrences and highlight them
	var result strings.Builder
	lastEnd := 0
	for {
		idx := strings.Index(lower[lastEnd:], queryLower)
		if idx == -1 {
			result.WriteString(text[lastEnd:])
			break
		}
		idx += lastEnd
		result.WriteString(text[lastEnd:idx])
		// Yellow background, black text for highlight
		result.WriteString("\033[43;30m")
		result.WriteString(text[idx : idx+len(query)])
		result.WriteString("\033[0m")
		lastEnd = idx + len(query)
	}
	return result.String()
}

func padRight(s string, length int) string {
	if len(s) >= length {
		return s[:length]
	}
	return s + strings.Repeat(" ", length-len(s))
}

// ============================================================================
// Data loading (preserved from original)
// ============================================================================

func getProjectsDir() string {
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
	conv := &Conversation{SessionID: sessionID}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		lineBytes := scanner.Bytes()

		var raw RawMessage
		if err := json.Unmarshal(lineBytes, &raw); err != nil {
			continue
		}

		if raw.Type == "user" {
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

	if len(conv.Messages) == 0 {
		return nil, nil
	}

	conv.LastTimestamp = conv.Messages[len(conv.Messages)-1].Ts

	if conv.Cwd == "" {
		conv.Cwd = "unknown"
	}

	return conv, nil
}

func getConversations(cutoff time.Time, maxSize int64) ([]Conversation, error) {
	projectsDir := getProjectsDir()

	var files []string
	err := filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
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

func truncate(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// buildItems creates list items from conversations
func buildItems(conversations []Conversation) []listItem {
	items := make([]listItem, 0, len(conversations))

	for _, conv := range conversations {
		// Build search text from all content
		var searchParts []string
		searchParts = append(searchParts, conv.SessionID)
		searchParts = append(searchParts, conv.Cwd)
		searchParts = append(searchParts, formatTimestamp(conv.FirstTimestamp))
		searchParts = append(searchParts, formatTimestamp(conv.LastTimestamp))

		for _, msg := range conv.Messages {
			if msg.Role == "user" {
				searchParts = append(searchParts, msg.Text)
			}
		}

		items = append(items, listItem{
			conv:       conv,
			searchText: strings.Join(searchParts, " "),
		})
	}

	return items
}

func printHelp() {
	fmt.Printf(`ccs v%s - Claude Code Search

Search and resume Claude Code conversations.

Usage: ccs [filter] [-- claude-flags...]

Arguments:
  filter           Initial search query (optional)
  -- claude-flags  Flags to pass to 'claude --resume' (after --)

Flags:
  -h, --help       Show this help message
  -v, --version    Show version
  --max-age=N      Only search last N days (default: 60, 0 = no limit)
  --max-size=N     Max file size in MB (default: 1024, 0 = no limit)
  --all            Include everything (same as --max-age=0 --max-size=0)
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
  Ctrl+J/K        Scroll preview
  Mouse wheel     Scroll list or preview (based on position)
  Ctrl+U          Clear search
  Esc, Ctrl+C     Quit

`, version)
}

func main() {
	args := os.Args[1:]

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
			conversations, _ := getConversations(cutoff, maxSize)
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
		if arg == "--all" || strings.HasPrefix(arg, "--max-age=") || strings.HasPrefix(arg, "--max-size=") {
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
	conversations, err := getConversations(cutoff, maxSize)
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

	fmt.Printf("\033[1mResuming conversation %s in %s...\033[0m\n", conv.SessionID, cwd)
	if len(claudeFlags) > 0 {
		fmt.Printf("\033[90mFlags: %s\033[0m\n", strings.Join(claudeFlags, " "))
	}
	fmt.Println()

	if err := os.Chdir(cwd); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not change to directory %s: %v\n", cwd, err)
	}

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude not found in PATH\n")
		os.Exit(1)
	}

	execArgs := []string{"claude", "--resume", conv.SessionID}
	execArgs = append(execArgs, claudeFlags...)

	syscall.Exec(claudePath, execArgs, os.Environ())
}

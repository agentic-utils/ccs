package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.3.0"

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

// CacheData holds all data needed for preview
type CacheData struct {
	Conversations map[string]Conversation `json:"conversations"`
}

func getProjectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func getCachePath() string {
	return filepath.Join(os.TempDir(), "ccs-cache.json")
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

func parseConversationFile(path string) (*Conversation, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(info.Name(), "agent-") {
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
		var raw RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
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

	// Set LastTimestamp from the last message
	conv.LastTimestamp = conv.Messages[len(conv.Messages)-1].Ts

	if conv.Cwd == "" {
		conv.Cwd = "unknown"
	}

	return conv, nil
}

func getConversations() ([]Conversation, error) {
	projectsDir := getProjectsDir()

	// Collect all jsonl files
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

	// Parse files in parallel
	var wg sync.WaitGroup
	results := make(chan *Conversation, len(files))

	// Limit concurrency
	sem := make(chan struct{}, 20)

	for _, file := range files {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			conv, err := parseConversationFile(path)
			if err == nil && conv != nil {
				results <- conv
			}
		}(file)
	}

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

func padOrTruncate(s string, length int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > length {
		return s[:length-1] + "…"
	}
	return fmt.Sprintf("%-*s", length, s)
}

func buildSearchLines(conversations []Conversation) ([]string, map[string]Conversation) {
	var lines []string
	convMap := make(map[string]Conversation)

	for _, conv := range conversations {
		convMap[conv.SessionID] = conv
		project := conv.Cwd
		if idx := strings.LastIndex(conv.Cwd, "/"); idx >= 0 {
			project = conv.Cwd[idx+1:]
		}

		// Collect all user messages for search, display first one
		var firstUserMsg *Message
		var allUserText []string
		for i := range conv.Messages {
			if conv.Messages[i].Role == "user" {
				if firstUserMsg == nil {
					firstUserMsg = &conv.Messages[i]
				}
				allUserText = append(allUserText, conv.Messages[i].Text)
			}
		}

		if firstUserMsg == nil {
			continue
		}

		displayText := truncate(firstUserMsg.Text, 100)
		searchText := strings.Join(strings.Fields(strings.Join(allUserText, " ")), " ")
		ts := formatTimestamp(conv.LastTimestamp)
		projectPad := padOrTruncate(project, 25)

		// Format: id \t date \t project \t display_message \t search_text
		// Colors: date=dim, project=yellow/bold, message=white
		// search_text is hidden (column 5) but used for matching
		line := fmt.Sprintf("%s\t\033[90m%s\033[0m\t\033[1;33m%s\033[0m\t%s\t%s",
			conv.SessionID, ts, projectPad, displayText, searchText)
		lines = append(lines, line)
	}

	return lines, convMap
}

func saveCache(convMap map[string]Conversation) error {
	data := CacheData{Conversations: convMap}
	file, err := os.Create(getCachePath())
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(data)
}

func loadCache() (map[string]Conversation, error) {
	file, err := os.Open(getCachePath())
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var data CacheData
	if err := json.NewDecoder(file).Decode(&data); err != nil {
		return nil, err
	}
	return data.Conversations, nil
}

func highlight(text, query string) string {
	if query == "" {
		return text
	}
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(query))
	return re.ReplaceAllStringFunc(text, func(match string) string {
		return fmt.Sprintf("\033[43;30m%s\033[0m", match)
	})
}

func formatCodeBlock(text, query, indent string) string {
	lines := strings.Split(text, "\n")
	var result []string
	inCodeBlock := false
	codeLang := ""

	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			if !inCodeBlock {
				inCodeBlock = true
				codeLang = strings.TrimPrefix(line, "```")
				if codeLang == "" {
					codeLang = "code"
				}
				result = append(result, fmt.Sprintf("%s\033[90m┌─ %s ─\033[0m", indent, codeLang))
			} else {
				inCodeBlock = false
				result = append(result, fmt.Sprintf("%s\033[90m└─────────\033[0m", indent))
			}
		} else if inCodeBlock {
			result = append(result, fmt.Sprintf("%s\033[90m│\033[0m \033[36m%s\033[0m", indent, line))
		} else {
			result = append(result, fmt.Sprintf("%s%s", indent, highlight(line, query)))
		}
	}

	return strings.Join(result, "\n")
}

func showPreview(line, query string) {
	convMap, err := loadCache()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cache not found: %v\n", err)
		return
	}

	parts := strings.Split(line, "\t")
	if len(parts) == 0 {
		return
	}

	sessionID := parts[0]

	conv, ok := convMap[sessionID]
	if !ok {
		fmt.Println("Conversation not found")
		return
	}

	fmt.Printf("\033[1;33mProject:\033[0m %s\n", conv.Cwd)
	fmt.Printf("\033[1;33mSession:\033[0m %s\n", sessionID)
	fmt.Printf("\033[1;33mTotal messages:\033[0m %d\n\n", len(conv.Messages))

	// Find all messages containing the query
	var matchIndices []int
	matchSet := make(map[int]bool)
	if query != "" {
		queryLower := strings.ToLower(query)
		for i, msg := range conv.Messages {
			if strings.Contains(strings.ToLower(msg.Text), queryLower) {
				matchIndices = append(matchIndices, i)
				matchSet[i] = true
			}
		}
	}

	// Build set of indices to show (matches + 1 context on each side)
	showSet := make(map[int]bool)
	if len(matchIndices) > 0 {
		for _, idx := range matchIndices {
			if idx > 0 {
				showSet[idx-1] = true
			}
			showSet[idx] = true
			if idx < len(conv.Messages)-1 {
				showSet[idx+1] = true
			}
		}
	} else {
		// No matches, show first 6 messages
		for i := 0; i < 6 && i < len(conv.Messages); i++ {
			showSet[i] = true
		}
	}

	// Display messages with gaps
	lastShown := -1
	for i := 0; i < len(conv.Messages); i++ {
		if !showSet[i] {
			continue
		}

		// Show gap indicator if we skipped messages
		if lastShown >= 0 && i > lastShown+1 {
			skipped := i - lastShown - 1
			fmt.Printf("\033[90m    ... %d messages ...\033[0m\n\n", skipped)
		} else if lastShown == -1 && i > 0 {
			fmt.Printf("\033[90m    ... %d earlier messages\033[0m\n\n", i)
		}

		msg := conv.Messages[i]
		var prefix string
		if matchSet[i] {
			if msg.Role == "user" {
				prefix = "\033[1;32m>>> User:\033[0m"
			} else {
				prefix = "\033[1;34m>>> Claude:\033[0m"
			}
		} else {
			if msg.Role == "user" {
				prefix = "\033[32m    User:\033[0m"
			} else {
				prefix = "\033[34m    Claude:\033[0m"
			}
		}

		text := msg.Text
		if len(text) > 2000 {
			text = text[:2000] + "\n... (truncated)"
		}

		fmt.Println(prefix)
		fmt.Println(formatCodeBlock(text, query, "    "))
		fmt.Println()

		lastShown = i
	}

	if lastShown < len(conv.Messages)-1 {
		remaining := len(conv.Messages) - lastShown - 1
		fmt.Printf("\033[90m    ... %d more messages\033[0m\n", remaining)
	}
}

func printHelp() {
	fmt.Printf(`ccs v%s - Claude Code Search

Search and resume Claude Code conversations using fzf.

Usage: ccs [flags]

Flags:
  -h, --help      Show this help message
  -v, --version   Show version

Any other flags are passed through to 'claude --resume'.

Examples:
  ccs                                 Search and resume a conversation
  ccs --dangerously-skip-permissions  Resume with auto-accept permissions

Requirements:
  - fzf (brew install fzf)
  - claude (Claude Code CLI)

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

	// Preview mode - reads from cache (fast!)
	if len(args) >= 2 && args[0] == "--preview" {
		line := args[1]
		query := ""
		if len(args) >= 3 {
			query = args[2]
		}
		showPreview(line, query)
		return
	}

	var claudeFlags []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			claudeFlags = append(claudeFlags, arg)
		}
	}

	projectsDir := getProjectsDir()
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Projects directory not found: %s\n", projectsDir)
		fmt.Fprintf(os.Stderr, "Make sure Claude Code is installed and has been used at least once.\n")
		os.Exit(1)
	}

	if _, err := exec.LookPath("fzf"); err != nil {
		fmt.Fprintf(os.Stderr, "fzf not found. Install with: brew install fzf\n")
		os.Exit(1)
	}

	fmt.Fprint(os.Stderr, "Loading conversations...")
	conversations, err := getConversations()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\rError loading conversations: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprint(os.Stderr, "\r                         \r")

	if len(conversations) == 0 {
		fmt.Fprintf(os.Stderr, "No conversations found\n")
		os.Exit(1)
	}

	lines, convMap := buildSearchLines(conversations)
	if len(lines) == 0 {
		fmt.Fprintf(os.Stderr, "No searchable messages found\n")
		os.Exit(1)
	}

	// Save cache for preview
	if err := saveCache(convMap); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save cache: %v\n", err)
	}

	self, _ := os.Executable()

	cmd := exec.Command("fzf",
		"--ansi",
		"--delimiter=\t",
		"--with-nth=2,3,4",
		"--nth=2..",
		"--no-sort",
		"--tabstop=4",
		"--preview", fmt.Sprintf("%s --preview {} {q}", self),
		"--preview-window=bottom:70%:wrap:+5",
		"--header=Search conversations | Enter to resume, Esc to quit",
		"--prompt=Search: ",
		"--height=90%",
		"--layout=reverse",
		"--border=rounded",
		"--info=inline",
	)

	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	cmd.Stderr = os.Stderr

	output, err := cmd.Output()
	if err != nil {
		return
	}

	selected := strings.TrimSpace(string(output))
	if selected == "" {
		return
	}

	parts := strings.Split(selected, "\t")
	sessionID := parts[0]

	conv, ok := convMap[sessionID]
	if !ok {
		fmt.Fprintf(os.Stderr, "Conversation not found\n")
		os.Exit(1)
	}

	cwd := conv.Cwd
	if cwd == "" || cwd == "unknown" {
		cwd = "."
	}

	fmt.Printf("\033[1mResuming conversation %s in %s...\033[0m\n", sessionID, cwd)
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

	execArgs := []string{"claude", "--resume", sessionID}
	execArgs = append(execArgs, claudeFlags...)

	syscall.Exec(claudePath, execArgs, os.Environ())
}

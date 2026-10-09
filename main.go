package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var version = "dev"

func printHelp() {
	fmt.Printf(`ccs v%s - Claude Code Search

Search and resume Claude Code conversations.

Usage: ccs [filter] [-- claude-flags...]
       ccs prune [flags]    Shrink large conversations (see ccs prune --help)
       ccs mcp [flags]      MCP server for Claude Code (search earlier sessions); add with
                            claude mcp add --scope user ccs -- ccs mcp

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
  ↑/↓, Ctrl+P/N   Move through the list
  Enter           Resume the conversation, or focus it if it's open in claude
  Ctrl+S          Message the selected live session (Enter sends, Esc back)
  Ctrl+F          Fork conversation (resume into a new session id)
  Ctrl+R          Rename conversation (not while it's open in claude)
  Ctrl+D          Delete conversation (with confirmation)
  Ctrl+X          Prune conversation - shrink it losslessly (with confirmation)
  Ctrl+J/K        Scroll the conversation preview (also the mouse wheel)
  Esc, Ctrl+U     Clear the search
  Tab             Usage screen (Tab again to go back)
  Ctrl+O          Switch Claude account (needs cswap)
  Ctrl+L          Changelog
  Ctrl+T          Open a link in view
  Ctrl+] / Ctrl+\  Next / previous search hit
  Ctrl+G          Show the shortcuts that apply right now
  Ctrl+C          Quit

  Click a row to select it, click a link in the preview to open it.

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
	maxAgeDays := 60         // Default to 60 days
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

	if len(args) > 0 && args[0] == "mcp" {
		mcpMain(cutoff, maxSize, excludeDirs)
		return
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
				line := strings.ReplaceAll(item.searchText, searchSep, " ")
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

	// Run TUI with mouse reporting, so the wheel scrolls whatever is under the
	// pointer (see listLayout). A report that arrives fragmented can reach
	// Update as typed text; mouseLeak drops those before the search box.
	m := initialModel(items, filterQuery, claudeFlags)
	if saved := os.Getenv(restoreEnv); saved != "" {
		os.Unsetenv(restoreEnv) // once only: not inherited by claude or a later ccs
		var state uiState
		if json.Unmarshal([]byte(saved), &state) == nil {
			m.restore(state)
		}
	}
	m.livePIDs = liveSessionPIDs()
	m.live = make(map[string]bool, len(m.livePIDs))
	for id := range m.livePIDs {
		m.live[id] = true
	}
	m.lastRefresh = time.Now()
	if version != "dev" {
		m.checkLatest = latestRelease
		m.fetchChangelog = func(tag string) ([]string, error) { return releaseChangelog(version, tag) }
		m.fetchNotes = func() ([]string, error) { return releaseChangelog("0.0.0", "v99999.0.0") }
		m.notesFetching = true // Init starts it
		m.allowanceEnabled = true
		if exe, err := os.Executable(); err == nil {
			m.upgrade = chooseUpgrader(exe)
		}
	}
	m.usageExclude = excludeDirs
	m.reload = func() ([]listItem, error) {
		convs, err := getConversations(cutoff, maxSize, excludeDirs)
		if err != nil {
			return nil, err
		}
		return buildItems(convs), nil
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	finalModel, err := p.Run()
	closeInboxes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	final := finalModel.(model)
	if final.restart != "" {
		// Hand the new version where the user was, so the update is seamless.
		env := os.Environ()
		if state, err := json.Marshal(final.uiState()); err == nil {
			env = append(env, restoreEnv+"="+string(state))
		}
		err := syscall.Exec(final.restart, append([]string{"ccs"}, os.Args[1:]...), env)
		fmt.Fprintf(os.Stderr, "Updated; run ccs again (%v)\n", err)
		os.Exit(1)
	}
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
	if final.fork {
		execArgs = append(execArgs, "--fork-session")
	}

	syscall.Exec(claudePath, execArgs, os.Environ())
}

// runBounded runs an external command with a deadline, so a hung ps, tmux
// server or unresponsive iTerm can't stall ccs indefinitely.
func runBounded(timeout time.Duration, env []string, name string, args ...string) ([]byte, error) {
	return runCommand(timeout, env, false, name, args...)
}

// errTimedOut marks a command that hit its deadline, as opposed to one that
// failed: a timed-out tab open may still have opened, a failed one didn't.
var errTimedOut = errors.New("timed out")

// runCommand runs name with a deadline. detach puts it in its own session
// with no controlling terminal, so it can't prompt (git credentials, ssh
// passphrases) behind the TUI and swallow keystrokes. Output is stdout, or
// stdout+stderr when detached (for error messages).
func runCommand(timeout time.Duration, env []string, detach bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Without this, a child that inherits the output pipe keeps Output()
	// waiting past the deadline.
	cmd.WaitDelay = time.Second
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out []byte
	var err error
	start := time.Now()
	if detach {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("%s: %w after %s", name, errTimedOut, timeout)
	}
	if detach { // git/brew during self-update: keep a record to diagnose slow or failed updates
		logUpdate("$ %s %s\n  -> %v in %s\n%s", name, strings.Join(args, " "), errOrOK(err), time.Since(start).Round(time.Millisecond), indent(out))
	}
	return out, err
}

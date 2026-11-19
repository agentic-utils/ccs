// Package tui is ccs's terminal UI, the bubbletea model with its list,
// preview, message box, popups and usage screen, and its command line
// (Main), which dispatches the subcommands to the other internal packages.
package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime/pprof"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/selfupdate"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"

	tea "github.com/charmbracelet/bubbletea"
)

// startProfile records a CPU profile to path (CCS_CPUPROFILE, or ^X R),
// for chasing slowness: go tool pprof -top it. Returns the stop.
func startProfile(path string) (func(), error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() { pprof.StopCPUProfile(); f.Close() }, nil
}

func printHelp() {
	fmt.Printf(`ccs v%s - Coding CLI Sessions

Search and resume Claude Code conversations, and Codex sessions beside them.

Usage: ccs [filter] [-- claude-flags...]
       ccs prune [flags]    Shrink large conversations (see ccs prune --help)
       ccs orchestrator [--yes] [--no-mcp] [--adopt <session> | --release]
                            Open the session that runs your other sessions: its tab, resumed
                            if it isn't running, set up (with the ccs MCP server) if there is none;
                            ccs coordinator, its old name, still works
       ccs launch --name <name> (--brief <file> | --resume <id>) [--codex] [--cwd <dir>] [--no-preamble]
                            Start a named claude (or codex) session in a new iTerm tab or tmux window;
                            a new session's brief starts with launch-preamble.md beside ccs's settings, if you keep one;
                            the session that ran it becomes its parent, and the list draws it under that one
       ccs adopt <name> [--to <parent> | --to none]
                            Move the named session under the calling session (or --to's) in the list;
                            --to none detaches it
       ccs reorg [--dry-run] [--yes] | --undo
                            Move sessions into topics so no orchestrator looks after more than 3:
                            one Sonnet call plans it, you see the plan and answer y/N (--yes for scripts);
                            then it launches the orchestrators it needs and tells live sessions;
                            --undo puts back the records the last one wrote, after asking
       ccs close <name>     Stop the named session and close its tab (--graceful asks it to
                            finish and say what's left first)
       ccs restart [--force] <name>
                            Stop the named session and resume the same conversation in a new tab, with
                            the same name, folder and flags; refused mid-turn unless --force. A session
                            restarting itself restarts once its turn ends
       ccs sessions [--json] [--all] [--triage [--stall-after 15m]]
                            The sessions running now and what each is doing: busy, idle or waiting on you
                            and since when, its question, last API error, PRs, background tasks and subagents
                            --triage: each in one bucket (waiting_on_user, working, waiting_on_external,
                            done, stalled, idle) with why, its open PRs and next wake-up
       ccs link install     Make ccs://session/<name> links switch to that session's tab (macOS);
                            also: ccs link status, ccs link uninstall
       ccs watch [flags]    Report changes to your open PRs as JSON lines, and tell the sessions
                            that claimed them what's failing (see ccs watch --help)
       ccs claim <PR URL>   Record that the session running it owns the PR (--release drops it);
                            ccs watch does it for the session that opened a PR
                            (a Claude Code or Codex session)
       ccs prs [flags]      Your open PRs with their owning sessions (--json, --statusline, --claude-session-id / --codex-session-id <id>);
                            --merged lists what merged and which session merged it
       ccs depends <PR> --after <PR>   Record that a PR must not merge before another has; ccs watch
                            and ccs prs then show it blocked and say if it merges early
                            (ccs depends lists them, --drop forgets one)
       ccs orchestrator-report [--since 24h] [--json]   Each orchestrator's scorecard against
                            the previous saved day: response time, stalled and done minutes,
                            your nudges and corrections, hands-on work, merges, span
       ccs serve --host <h> --team <t> --aud <tag> --email <e> [--allow-write]
                            the sessions as a web page, behind Cloudflare Access (docs/remote.md)
       ccs hook install|uninstall [--print]   ccs's Claude Code hooks: PermissionRequest (while
                            ccs serve --allow-write runs, prompts wait there to be answered),
                            Stop (an orchestrator is told each time a session under it ends a turn;
                            ccs hook child-notices on|off turns that off for one) and SessionStart
                            (a new session's parent is recorded as it starts, however it was started)
       ccs mcp [flags]      MCP server for Claude Code (search earlier sessions); add with
                            claude mcp add --scope user ccs -- ccs mcp
       ccs stats [--json]   How often you use each ccs feature, and the ones never used;
                            recorded on this machine only (--off / --on, or CCS_NO_STATS=1)

Arguments:
  filter           Initial search query (optional)
  -- claude-flags  Flags to pass to 'claude --resume' (after --)

Environment:
  CCS_CLAUDE_ARGS  Flags passed to every claude ccs starts, before any -- claude-flags
                   (whitespace-separated, no quoting)

Flags:
  -h, --help       Show this help message
  -v, --version    Show version
  --max-age=N      Only search last N days (default: 60, 0 = no limit)
  --max-size=N     Max file size in MB (default: 1024, 0 = no limit)
  --all            Include everything: --max-age=0 --max-size=0, and headless
                   runs (claude -p with a single prompt, codex exec), hidden
                   otherwise
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
  Single keys act now and leave nothing behind; Ctrl+X then a letter changes
  something that stays (a mark, a setting, a file, whether it runs), and the
  ones that can't be undone ask first.

 Moving around
  ↑/↓, Ctrl+P/N   Move through the list
  PgUp/PgDn       Scroll the conversation (the mouse wheel too)
  Ctrl+] / Ctrl+\  Jump to the next / previous search hit
  Esc             Clear the search and go back to the top

 This session
  Enter           Resume it in claude, in a new tab in tmux or iTerm; on one
                  open in claude, type in its message box, and Enter again on
                  the empty box goes to its tab
  Ctrl+V          Paste a screenshot from the clipboard into a message to it
  Ctrl+F          Fork it: a new session with the same history, in a new tab
  Ctrl+O          Open a link in view or one of its PRs

 Ctrl+X then a letter: this session
  Ctrl+X P        Pin it to the top of the list (again to unpin)
  Alt+Up/Down     Move it among the pinned sessions
  Ctrl+X S        Put it away: settle until its next message, or snooze it
                  (again to bring it back)
  Ctrl+X E        Edit its name, as /rename does (not while it's open in claude)
  Ctrl+X F        Show or hide its details under the summary line (clicking the line does too)
  Ctrl+X X        Shrink its file, keeping the dialogue; asks first, loses
                  rewind history
  Ctrl+X K        Stop it and close its tab, or stop a background session;
                  asks first
  Ctrl+X D        Delete its transcript file; asks first, can't be undone

 Ctrl+X then a letter: view and tools
  Ctrl+X N        Start a new session in an iTerm tab or tmux window, in this one's folder
  Ctrl+X O        Open the orchestrator, setting one up if there is none
  Ctrl+X C        Every orchestrator tree: show closed sessions, or hide them again
  Ctrl+X G        Group the list by folder (again to stop)
  Ctrl+X M        Move sessions into topics (ccs reorg): see Sonnet's plan, enter applies it
  Ctrl+X T        The full table, every column, instead of the sidebar
  Ctrl+X L        Hide or show the session list, the preview taking its place
  Ctrl+X H        Show or hide headless runs (claude -p, codex exec); --all
                  starts shown
  Ctrl+X A        When a session starts waiting on you, select it in ccs (and focus a prompt ccs
                  serve holds, to answer it in the bottom pane) and switch to its tab
  Ctrl+X B        Set up ccs serve (try it on this Mac, or a form for your phone), start its launchd agent, or show its address and QR code (s stops it, asking y/n; e edits its setup)
  Ctrl+X I        Install ccs updates on their own once you're away (off by
                  default)
  Ctrl+X R        Record a CPU profile, for when ccs feels slow; again to stop
                  (or set CCS_CPUPROFILE=path)
  Ctrl+X U        Switch Claude account (needs cswap)

 ccs
  Ctrl+R          Refresh sessions, PRs and allowance now; check for an update
  Tab             Token use of every session in the last 12h, and your
                  allowance (Tab again to go back)
  Ctrl+L          What changed in each ccs release
  ?               Show the shortcuts that apply right now (on an empty search or
                  message box; after text it types a ?)
  Ctrl+C          Quit

  ccs's own actions are Ctrl+X then a letter (Ctrl+X Ctrl+A works too), so
  the search and message boxes edit like Claude Code's prompt: Ctrl+A/E start
  and end, Ctrl+K/U delete to the end or start, Ctrl+W the word before,
  Ctrl+Y puts back what they deleted, Alt+B/F move by word.

  Click a row to select it, click a link in the preview to open it.

`, state.Version)
}

// Main runs ccs with the command line in os.Args, as release v.
func Main(v string) {
	state.Version = v
	args := os.Args[1:]
	recordCLI(args)

	if len(args) > 0 && args[0] == "stats" {
		os.Exit(stats.Run(args[1:], os.Stdout, os.Stderr))
	}

	if len(args) > 0 && args[0] == "prune" {
		transcript.RunPrune(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "launch" {
		os.Exit(sessions.RunLaunch(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && isOrchestratorCmd(args[0]) {
		os.Exit(runOrchestrator(args[1:], os.Stdin, os.Stdout, os.Stderr, stdinIsTerminal()))
	}
	if len(args) > 0 && args[0] == "reorg" {
		os.Exit(runReorgCLI(args[1:], os.Stdin, os.Stdout, os.Stderr, stdinIsTerminal()))
	}
	if len(args) > 0 && args[0] == "adopt" {
		os.Exit(sessions.RunAdopt(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "close" {
		os.Exit(sessions.RunClose(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "restart" {
		os.Exit(sessions.RunRestart(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "link" {
		os.Exit(sessions.RunLink(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "watch" {
		github.SessionTick = mcp.WatchSessions
		os.Exit(github.RunWatch(args[1:]))
	}
	if len(args) > 0 && args[0] == "claim" {
		os.Exit(github.RunClaim(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "review" {
		fmt.Fprintln(os.Stderr, "ccs review is gone:", github.ReviewMoved)
		os.Exit(2)
	}
	if len(args) > 0 && args[0] == "prs" {
		os.Exit(github.RunPRs(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "orchestrator-report" {
		os.Exit(mcp.RunReport(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "depends" {
		os.Exit(github.RunDepends(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "sessions" {
		// Its --all is its own (sessions not running too), over the list's
		// usual window: the list's flags aren't its flags.
		load := func() ([]transcript.Conversation, error) {
			return transcript.GetConversations(time.Now().AddDate(0, 0, -60), 1024*1024*1024, []string{"observer-sessions"})
		}
		mcp.UseLiveLoads()
		os.Exit(mcp.RunSessions(args[1:], os.Stdout, os.Stderr, load))
	}

	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			printHelp()
			return
		}
		if arg == "-v" || arg == "--version" {
			fmt.Printf("ccs v%s\n", state.Version)
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
			showHeadless = true
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

	if len(args) > 0 && args[0] == "hook" {
		os.Exit(mcp.RunHook(args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "serve" {
		load := func() ([]transcript.Conversation, error) {
			// long-running and keeps its cache: it never holds a scan slot for good
			defer transcript.ReleaseScanSlot()
			return transcript.GetConversations(time.Now().AddDate(0, 0, -60), 1024*1024*1024, []string{"observer-sessions"})
		}
		mcp.ServeMain(args[1:], load)
	}

	if len(args) > 0 && args[0] == "mcp" {
		mcp.Main(cutoff, maxSize, excludeDirs)
		return
	}

	// Debug mode - dump search lines
	for i, arg := range args {
		if arg == "--dump" {
			filter := ""
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				filter = args[i+1]
			}
			conversations, _ := transcript.GetConversations(cutoff, maxSize, excludeDirs)
			items := buildItems(conversations)
			for _, item := range items {
				line := strings.ReplaceAll(item.searchText, transcript.SearchSep, " ")
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
	claudeFlags = withEnvClaudeArgs(os.Getenv("CCS_CLAUDE_ARGS"), claudeFlags)

	projectsDir := transcript.GetProjectsDir()
	var conversations []transcript.Conversation
	if _, err := os.Stat(projectsDir); !os.IsNotExist(err) {
		fmt.Fprint(os.Stderr, "Loading conversations...")
		conversations, err = transcript.GetConversations(cutoff, maxSize, excludeDirs)
		transcript.ReleaseScanSlot() // the list keeps its cache for good: it never holds a slot for good
		if err != nil {
			fmt.Fprintf(os.Stderr, "\rError loading conversations: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprint(os.Stderr, "\r                         \r")
	}

	items := buildItems(conversations)
	// Nothing to list: open only if there's an orchestrator to offer.
	if len(items) == 0 && !orchestratorOfferable() {
		fmt.Fprintf(os.Stderr, "No conversations found in %s\n", projectsDir)
		fmt.Fprintf(os.Stderr, "Start one with claude, or set up an orchestrator with ccs orchestrator.\n")
		os.Exit(1)
	}

	// Run TUI with mouse reporting, so the wheel scrolls whatever is under the
	// pointer (see listLayout). A report that arrives fragmented can reach
	// Update as typed text; mouseLeak drops those before the search box.
	go sessions.RefreshLinkApp()
	m := initialModel(items, filterQuery, claudeFlags)
	s := loadSettings() // the view it was left in
	m.table, m.grouped, m.autoFocus, m.hideSidebar = s.Table, s.Grouped, s.AutoFocus, s.HideSidebar
	m.treeView = s.treeView()
	m.headerCollapsed = !s.HeaderOpen
	m.autoUpdate = s.autoUpdateOn() // off unless turned on with ^X I
	if s.AutoUpdate == nil && s.found {
		saveSettings(m.settings()) // pin what an older install had
	}
	if from := os.Getenv(updatedEnv); from != "" {
		os.Unsetenv(updatedEnv) // once only
		m.updatedFrom, m.updatedOpen = from, true
	}
	m.marks = loadMarks()                     // when each session was last looked at
	m.orchID = loadOrchestrator().SessionID   // listed above everything
	if handled, ok := loadFocusRecord(); ok { // waits it switched to before a restart aren't switched to again
		m.focusHandled, m.focusNoted = handled, map[string]bool{}
	}
	m.sortList()
	if saved := os.Getenv(restoreEnv); saved != "" {
		os.Unsetenv(restoreEnv) // once only: not inherited by claude or a later ccs
		var state uiState
		if json.Unmarshal([]byte(saved), &state) == nil {
			m.restore(state)
		}
	}
	m.livePIDs = sessions.LiveSessionPIDs()
	m.live = make(map[string]bool, len(m.livePIDs))
	for id := range m.livePIDs {
		m.live[id] = true
	}
	m.lastRefresh = time.Now()
	if state.Version != "dev" {
		m.checkLatest = selfupdate.LatestRelease
		m.fetchChangelog = func(tag string) ([]string, error) { return selfupdate.ReleaseChangelog(state.Version, tag) }
		m.fetchNotes = func() ([]string, error) { return selfupdate.ReleaseChangelog("0.0.0", "v99999.0.0") }
		m.notesFetching = true // Init starts it
		m.allowanceEnabled = true
		if exe, err := os.Executable(); err == nil {
			m.upgrade = selfupdate.ChooseUpgrader(exe)
		}
	}
	m.usageExclude = excludeDirs
	m.fetchPRs = github.FetchPRs()
	m.reload = func() ([]listItem, error) {
		convs, err := transcript.GetConversations(cutoff, maxSize, excludeDirs)
		transcript.ReleaseScanSlot()
		if err != nil {
			return nil, err
		}
		return buildItems(convs), nil
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus(), tea.WithOutput(eraseSavedLines{os.Stdout}))

	if path := os.Getenv("CCS_CPUPROFILE"); path != "" {
		stop, err := startProfile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "CCS_CPUPROFILE: %v\n", err)
			os.Exit(1)
		}
		m.profileStop, m.profilePath = stop, path
	}
	finalModel, err := p.Run()
	stats.Flush() // before any exec or exit below, which skip the background flush
	// After a panic bubbletea hands back no model, only the error.
	final, _ := finalModel.(model)
	if stop := final.profileStop; stop != nil {
		stop()
	}
	sessions.CloseInboxes()
	releaseHold("ccs quit") // prompts it held are handed back: their hooks dial the next ccs
	stopPreview()           // a preview started from ^X B lives only as long as this ccs
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if final.restart != "" {
		// Hand the new version where the user was, so the update is seamless.
		env := os.Environ()
		if state, err := json.Marshal(final.uiState()); err == nil {
			env = append(env, restoreEnv+"="+string(state))
		}
		if final.autoInstalling { // the new ccs says it updated itself
			env = append(env, updatedEnv+"="+state.Version)
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

	if conv.Kind == transcript.KindCodex {
		codexPath, err := exec.LookPath("codex")
		if err != nil {
			fmt.Fprintf(os.Stderr, "codex not found in PATH\n")
			os.Exit(1)
		}
		args := transcript.CodexResumeArgs("codex", *conv, final.fork)
		syscall.Exec(codexPath, args, os.Environ())
		return
	}

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

// withEnvClaudeArgs puts CCS_CLAUDE_ARGS ahead of the -- flags, so the command line wins where claude takes the last value.
func withEnvClaudeArgs(env string, flags []string) []string {
	return append(strings.Fields(env), flags...)
}

// eraseSavedLines follows every screen clear with an erase of the saved
// lines. iTerm keeps lines cleared off the alternate screen as scrollback,
// and the wheel then scrolls the window into those old frames: the title row
// goes off the top and stale rows show. Embedding the *os.File keeps it a
// terminal to bubbletea (Fd, raw mode, size).
type eraseSavedLines struct{ *os.File }

func (e eraseSavedLines) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte(ansi.EraseEntireScreen)) {
		return e.File.Write(p)
	}
	_, err := e.File.Write(bytes.ReplaceAll(p, []byte(ansi.EraseEntireScreen), []byte(ansi.EraseEntireScreen+ansi.EraseEntireDisplay)))
	return len(p), err
}

// WriteString too: io.WriteString (bubbletea's clears) would otherwise use
// the embedded file's WriteString and skip Write.
func (e eraseSavedLines) WriteString(s string) (int, error) { return e.Write([]byte(s)) }

// cliCommands are the subcommands counted by name; any other first word is a
// search, so it counts as the list.
var cliCommands = map[string]stats.Feature{
	"prune": "cli.prune", "launch": "cli.launch", orchestratorName: "cli.orchestrator",
	state.LegacyOrchestratorName: "cli.orchestrator", "reorg": "cli.reorg", "adopt": "cli.adopt",
	"close": "cli.close", "restart": "cli.restart", "link": "cli.link", "watch": "cli.watch",
	"claim": "cli.claim", "review": "cli.review", "prs": "cli.prs",
	"orchestrator-report": "cli.orchestrator_report", "depends": "cli.depends",
	"sessions": "cli.sessions", "mcp": "cli.mcp", "stats": "cli.stats",
}

// recordCLI counts the command (and, for the list, its flags, never their
// values) and writes it straight away, since most commands end in os.Exit.
// A status line's call isn't a use, and runs every few seconds, so it's left
// out.
func recordCLI(args []string) {
	if slices.Contains(args, "--statusline") {
		return
	}
	defer stats.Flush()
	if len(args) > 0 {
		if f, ok := cliCommands[args[0]]; ok {
			stats.Record(f, "")
			return
		}
	}
	for _, arg := range args {
		switch {
		case arg == "-h" || arg == "--help":
			stats.Record("cli.help", "")
			return
		case arg == "-v" || arg == "--version":
			stats.Record("cli.version", "")
			return
		}
	}
	stats.Record("cli.tui", "")
	for _, arg := range args {
		switch {
		case arg == "--":
			stats.Record("cli.claude_args", "")
			return // the rest are claude's
		case arg == "--all":
			stats.Record("cli.all", "")
		case strings.HasPrefix(arg, "--max-age="):
			stats.Record("cli.max_age", "")
		case arg == "--dump":
			stats.Record("cli.dump", "")
		}
	}
}

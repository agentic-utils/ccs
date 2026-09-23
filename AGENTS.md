# CCS - Claude Code Search

## Overview

CLI tool to search and resume Claude Code conversations using a bubbletea TUI.

## Development

### Build & Test

```bash
go build
go test -v -cover
```

### Test Coverage

- **Target**: 60%+ statement coverage
- **Current**: ~69%
- All new features must include tests
- Break logic into testable functions when possible
- Refactor for testability (e.g., use variables instead of functions for dependency injection)

### Run locally

```bash
./ccs                    # search recent (60 days, <1GB files)
./ccs <query>            # search with initial query
./ccs --max-age=7        # last 7 days only
./ccs --all              # include everything
./ccs -- --plan          # pass flags to claude
./ccs prune --dry-run    # preview lossless size reduction of large files
```

## Release Process

Merging to `main` releases automatically: `.github/workflows/ci.yaml` runs tests, bumps and pushes the tag from the conventional-commit prefixes (`feat:` minor, `fix:`/other patch, breaking-change major), then GoReleaser publishes and updates the Homebrew tap. Never tag by hand; `version` in `main.go` is injected at build time.

Install the release locally: `brew update && brew upgrade ccs`.

## Architecture

- `main.go` - Single file containing all logic
- `main_test.go` - Unit tests
- `.goreleaser.yaml` - Release configuration
- `.github/workflows/test.yaml` - CI test workflow (reusable)
- `.github/workflows/release.yaml` - Release workflow (calls test.yaml)

### Dependencies

- `github.com/charmbracelet/bubbletea` - TUI framework
- `github.com/charmbracelet/bubbles/textinput` - Text input component
- `github.com/charmbracelet/lipgloss` - Styling

### Key Types

- `Conversation` - Parsed conversation with messages, timestamps, cwd
- `Message` - Single message (role, text, timestamp)
- `listItem` - Display item with conversation and search text
- `model` - Bubbletea application state (includes delete confirmation state)

### Key Functions

- `getConversations(cutoff, maxSize)` - Loads conversations from `~/.claude/projects/` with filters
- `parseConversationFile(path, cutoff, maxSize)` - Parses JSONL files, skips by mtime/size
- `buildItems()` - Creates list items with searchable text
- `initialModel()` - Sets up bubbletea TUI
- `Update()` - Handles keyboard/mouse input, including delete confirmation
- `View()` - Renders the TUI with delete confirmation prompt
- `renderPreview()` - Renders conversation preview with highlights
- `previewLines()` - Memoised `buildPreviewLines` for the selected conversation (rebuilt only when selection/query changes), avoids per-frame rescans of huge conversations
- `hitCount()` / `countHits()` - Memoised per-query HITS count (messages containing the query), keyed by SessionID, so `formatListItem` doesn't rescan every visible row each frame
- `formatAgo()` - AGO column: compact time since last message (`now`, `5m`, `3h`, `2d`, `3w`, `4mo`, `1y`), recomputed each frame
- `formatListItem()` - Formats a single list row; `●` live / `⚙` spawned are packed right into a 2-cell column (`colMarks`) just before the title, so titles stay aligned; `✍` trails a user-set name
- `readLiveSessions()` - SessionIDs open in a running claude, from `~/.claude/sessions/<pid>.json` where the pid is alive
- `refreshTick()` / `applyRefresh()` - Background re-scan every `refreshInterval` (1 min), keeping the cursor on the same conversation; skipped while a delete/prune/rename prompt is open, and dropped if `m.gen` moved (a delete/prune/rename happened while the scan ran); search text is built once at parse and shared through `parseCache`, so unchanged conversations cost nothing on refresh
- `parseCache` - Reuses parsed conversations whose file size+mtime are unchanged, so refreshes only reparse changed files
- `openResumeTab()` / `resumeInTmuxWindow()` / `resumeInITermTab()` - Opens the selected conversation in a new tmux window or iTerm tab (focus stays on ccs); falls back to exec-in-place elsewhere
- `focusSession()` / `tmuxPaneForTTY()` - Enter on a live session: find its terminal by the claude pid's tty (`liveSessionPIDs()`) and select that tmux pane or iTerm tab instead of resuming a second copy
- Ctrl+F forks: `openResumeTab` / exec with `--fork-session`
- `deleteConversation()` - Removes conversation file and updates UI state
- `pruneConversation()` - Prunes the selected conversation in place (Ctrl+X) and refreshes its size
- `getTopic()` - Title, else first real user message (skips tag-wrapped harness text, shows `/cmd` or `! cmd` for command-only sessions), else session id
- `renameConversation()` / `appendLine()` - Ctrl+R rename: appends a `custom-title` line like `/rename`; refused on live sessions since a running claude re-appends its own title
- `Conversation.Spawned` - first user line has `entrypoint: sdk-cli` or a `teamName`; shown as `⚙`
- `pruneFile()` / `pruneStream()` - Shrink a conversation by dropping duplicate/redundant data (never touches user/assistant lines)
- `runPrune()` - `ccs prune` subcommand driver

### TUI Layout

```
  ccs · claude code search    Resume:Enter Fork:Ctrl+F Rename:Ctrl+R Delete:Ctrl+D Prune:Ctrl+X Scroll:Ctrl+J/K Exit:Esc
  > type to search...                                                     (N/total)

  DATE              AGO   PROJECT               TOPIC                       MSGS  HITS    SIZE
──────────────────────────────────────────────────────────────────────────────────────
  2024-01-08 15:04   2h   project-name          ● Refactor auth flow ✍        42     3   1.2GB
> 2024-01-08 14:30   3h   selected              This one is selected          28     1    12MB
──────────────────────────────────────────────────────────────────────────────────────
Project: /path/to/project
Session: abc123...

    2024-01-08 12:00 User:
    message text here...

    2024-01-08 12:01 Claude:
    response text here...
```

## Conventions

- Use conventional commits (feat:, fix:, docs:, etc.)
- Run tests before releasing
- Keep it simple - single file is fine for this project

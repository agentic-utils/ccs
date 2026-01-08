# CCS - Claude Code Search

## Overview

CLI tool to search and resume Claude Code conversations using a bubbletea TUI.

## Development

### Build & Test

```bash
go build
go test -v -cover
```

### Run locally

```bash
./ccs
./ccs <query>
./ccs -- --plan  # pass flags to claude
```

## Release Process

1. Update version in `main.go`:
   ```go
   const version = "X.Y.Z"
   ```

2. Commit changes:
   ```bash
   git add -A && git commit -m "feat/fix: description"
   ```

3. Push and tag:
   ```bash
   git push
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

4. CI will run tests, then release via GoReleaser and update Homebrew tap

## Version Bumping

- **Patch** (0.0.X): Bug fixes, minor tweaks
- **Minor** (0.X.0): New features, backwards compatible
- **Major** (X.0.0): Breaking changes

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
- `model` - Bubbletea application state

### Key Functions

- `getConversations()` - Loads all conversations from `~/.claude/projects/`
- `parseConversationFile()` - Parses JSONL conversation files
- `buildItems()` - Creates list items with searchable text
- `initialModel()` - Sets up bubbletea TUI
- `Update()` - Handles keyboard/mouse input
- `View()` - Renders the TUI
- `renderPreview()` - Renders conversation preview with highlights
- `formatListItem()` - Formats a single list row

### TUI Layout

```
  ccs · claude code search                    ↑/↓ Enter Ctrl+J/K Esc
  > type to search...                                       (N/total)

  DATE              PROJECT               TOPIC                 MSGS  HITS
────────────────────────────────────────────────────────────────────────────
  2024-01-08 15:04  project-name          First user message     42     3
> 2024-01-08 14:30  selected              This one is selected   28     1
────────────────────────────────────────────────────────────────────────────
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

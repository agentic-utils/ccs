# ccs - Claude Code Search

[![Tests](https://github.com/agentic-utils/ccs/actions/workflows/test.yaml/badge.svg)](https://github.com/agentic-utils/ccs/actions/workflows/test.yaml)
[![Release](https://img.shields.io/github/v/release/agentic-utils/ccs)](https://github.com/agentic-utils/ccs/releases/latest)
[![License](https://img.shields.io/github/license/agentic-utils/ccs)](LICENSE)

Globally search and resume [Claude Code](https://claude.ai/claude-code) conversations.

[![Demo](demo.gif)](https://asciinema.org/a/JXHQVf8PGBG2Orsl)

## Features

- Search through all your Claude Code conversations
- See session names (your custom titles or Claude's auto-generated ones) in the list
- Preview conversation context with search term highlighting
- See message counts, hit counts, and file size per conversation
- Resume conversations directly from the search interface
- Delete conversations with confirmation prompt
- Prune bloated conversations losslessly (`ccs prune`)
- Pass flags through to `claude` (e.g., `--plan`)

## Installation

### Homebrew (macOS and Linux)

```bash
brew install agentic-utils/tap/ccs
```

### From source

Requires [Go](https://go.dev/doc/install) 1.24+.

```bash
go install github.com/agentic-utils/ccs@latest
```

### Manual

Download the binary from [releases](https://github.com/agentic-utils/ccs/releases) and add to your PATH.

## Requirements

- [Claude Code](https://claude.ai/claude-code) - must be installed and used at least once

## Usage

```bash
# Search recent conversations (last 60 days, files <1GB)
ccs

# Search with initial query
ccs buyer

# Search last 7 days only
ccs --max-age=7

# Search everything (all time, all files)
ccs --all

# Resume with plan mode
ccs -- --plan

# Combined: search "buyer", resume with plan mode
ccs buyer -- --plan
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--max-age=N` | 60 | Only search files modified in the last N days (0 = no limit) |
| `--max-size=N` | 1024 | Max file size in MB to include (0 = no limit) |
| `--all` | - | Include everything (same as `--max-age=0 --max-size=0`) |
| `--exclude=a,b` | observer-sessions | Exclude project dirs whose path contains any of these substrings |

### Keybindings

- `↑/↓` or `Ctrl+P/N` - Navigate list
- `Enter` - Resume selected conversation
- `Ctrl+D` - Delete selected conversation (with confirmation)
- `Ctrl+R` - Prune selected conversation - shrink it losslessly (with confirmation)
- `Ctrl+J/K` - Scroll preview
- `Ctrl+U` - Clear search
- `Esc` / `Ctrl+C` - Quit

## Pruning

Conversation files grow large over time. `ccs prune` shrinks them by removing data that duplicates content kept elsewhere, so pruned conversations still resume with their full dialogue intact:

- `toolUseResult` fields - a copy of the tool result already present in `message.content`
- `file-history-snapshot` lines - rewind/checkpoint backups (pruning loses rewind history, not the conversation)

User and assistant messages are never modified, and a file is only rewritten if its conversation line count is unchanged.

`ccs prune` is a dry-run preview by default - it only reports what it would reclaim. Pass `--apply` to actually rewrite the files.

```bash
ccs prune                       # preview savings, change nothing (files >= 50MB)
ccs prune --apply               # prune after a confirmation prompt
ccs prune --apply --min-size=200  # only files >= 200MB
ccs prune --apply --no-tool-results  # keep tool results, only drop snapshot backups
```

Run `ccs prune --help` for all flags.

You can also prune a single conversation from the search interface: select it and press `Ctrl+R` (with confirmation).

## How it works

ccs reads conversation history from `~/.claude/projects/` and presents them in an interactive TUI. When you select a conversation, it changes to the original project directory and runs `claude --resume <session-id>`.

## License

MIT

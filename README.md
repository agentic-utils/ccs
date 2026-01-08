# ccs - Claude Code Search

[![Tests](https://github.com/agentic-utils/ccs/actions/workflows/test.yaml/badge.svg)](https://github.com/agentic-utils/ccs/actions/workflows/test.yaml)
[![Release](https://img.shields.io/github/v/release/agentic-utils/ccs)](https://github.com/agentic-utils/ccs/releases/latest)
[![License](https://img.shields.io/github/license/agentic-utils/ccs)](LICENSE)

Globally search and resume [Claude Code](https://claude.ai/claude-code) conversations.

[![Demo](demo.gif)](https://asciinema.org/a/JXHQVf8PGBG2Orsl)

## Features

- Search through all your Claude Code conversations
- Preview conversation context with search term highlighting
- See message counts and hit counts per conversation
- Resume conversations directly from the search interface
- Pass flags through to `claude` (e.g., `--plan`)
- Mouse wheel scrolling support

## Installation

### Homebrew (macOS and Linux)

```bash
brew install agentic-utils/tap/ccs
```

### From source

Requires [Go](https://go.dev/doc/install) 1.21+.

```bash
go install github.com/agentic-utils/ccs@latest
```

### Manual

Download the binary from [releases](https://github.com/agentic-utils/ccs/releases) and add to your PATH.

## Requirements

- [Claude Code](https://claude.ai/claude-code) - must be installed and used at least once

## Usage

```bash
# Search and resume a conversation
ccs

# Search with initial query
ccs buyer

# Resume with plan mode
ccs -- --plan

# Combined: search "buyer", resume with plan mode
ccs buyer -- --plan
```

### Keybindings

- `↑/↓` or `Ctrl+P/N` - Navigate list
- `Enter` - Resume selected conversation
- `Ctrl+J/K` - Scroll preview
- `Mouse wheel` - Scroll list or preview (context-aware)
- `Ctrl+U` - Clear search
- `Esc` / `Ctrl+C` - Quit

## How it works

ccs reads conversation history from `~/.claude/projects/` and presents them in an interactive TUI. When you select a conversation, it changes to the original project directory and runs `claude --resume <session-id>`.

## License

MIT

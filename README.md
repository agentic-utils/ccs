# ccs - Claude Code Search

Search and resume [Claude Code](https://claude.ai/claude-code) conversations using fzf.

![Demo](demo.gif)

## Features

- Fuzzy search through all your Claude Code conversations
- Preview conversation context with syntax highlighting
- Search term highlighting in preview
- Code block rendering
- Resume conversations directly from the search interface
- Pass flags through to `claude` (e.g., `--dangerously-skip-permissions`)

## Installation

### Homebrew

```bash
brew install agentic-cli/tap/ccs
```

### From source

```bash
go install github.com/agentic-cli/ccs@latest
```

### Manual

Download the binary from [releases](https://github.com/agentic-cli/ccs/releases) and add to your PATH.

## Requirements

- [fzf](https://github.com/junegunn/fzf) - `brew install fzf`
- [Claude Code](https://claude.ai/claude-code) - must be installed and used at least once

## Usage

```bash
# Search and resume a conversation
ccs

# Resume with auto-accept permissions
ccs --dangerously-skip-permissions
```

### Keybindings

- `Enter` - Resume selected conversation
- `Esc` / `Ctrl+C` - Quit
- Type to fuzzy search

## How it works

ccs reads conversation history from `~/.claude/projects/` and presents them in fzf. When you select a conversation, it changes to the original project directory and runs `claude --resume <session-id>`.

## License

MIT

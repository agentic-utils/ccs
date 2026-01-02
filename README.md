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

- [fzf](https://github.com/junegunn/fzf)

  ```bash
  # macOS
  brew install fzf

  # Debian/Ubuntu
  sudo apt install fzf

  # Fedora
  sudo dnf install fzf

  # Arch
  sudo pacman -S fzf
  ```

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

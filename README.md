# ccs - Claude Code Search

[![Tests](https://github.com/agentic-utils/ccs/actions/workflows/test.yaml/badge.svg)](https://github.com/agentic-utils/ccs/actions/workflows/test.yaml)
[![Release](https://img.shields.io/github/v/release/agentic-utils/ccs)](https://github.com/agentic-utils/ccs/releases/latest)
[![License](https://img.shields.io/github/license/agentic-utils/ccs)](LICENSE)

Globally search and resume [Claude Code](https://claude.ai/claude-code) conversations.

![Demo: searching, jumping between hits, the usage screen, help and the changelog](demo.gif)

## Features

- Search through all your Claude Code conversations
- See session names (your custom titles or Claude's auto-generated ones) in the list
- Preview conversation context with search term highlighting
- See context size (tokens as of the last reply), message counts, hit counts, and file size per conversation
- Resume conversations directly from the search interface
- Background tasks and subagents: a session open in `claude` shows `⧗N` for its running background commands and monitors and a pink `◆N` for its running subagents, and the preview lists both with how long each has run
- Auto-refreshes every minute; a green `●` marks conversations open in a running `claude`, and ones started by another session's `claude -p` sit `└` under it
- Rename conversations (`Ctrl+X E`)
- Delete conversations with confirmation prompt
- Prune bloated conversations losslessly (`ccs prune`)
- Pass flags through to `claude` (e.g., `--plan`)
- Usage screen (`Tab`): the last 12 hours of token usage across all sessions (including subagents) as three charts (cache-write disposition, context assembly, output), a summary with effective tokens, and your live 5-hour and weekly allowance. The allowance reads Claude Code's stored login read-only (macOS Keychain, else `~/.claude/.credentials.json`); ccs never refreshes or writes it, so if it has expired, open `claude` to refresh it
- Checks for new releases at startup and every 2 minutes (update steps are logged to `~/Library/Logs/ccs/update.log`) and offers to update and restart from a popup (via `brew upgrade` for Homebrew installs, otherwise by replacing the binary with the checksum-verified release)


## Let Claude search your sessions (MCP)

`ccs mcp` is an MCP server that lets Claude find and read your earlier Claude Code sessions. Add it once:

```bash
claude mcp add --scope user ccs -- ccs mcp
```

Claude starts it when needed (no ccs window has to be open, no network port). It has three tools: `search_sessions` (every word must appear; sessions named after the query first, then by matching messages and recency, each with a snippet and its resume command), `list_sessions` (most recent first) and `read_session` (a session's messages, paged, optionally only those around a search term). It searches the same window as the TUI (60 days, files under 1GB); use `ccs mcp --all` or `--max-age=N` to change it.

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

Set `CCS_CLAUDE_ARGS` to pass flags to every `claude` ccs starts, ahead of any `--` flags. It is split on whitespace with no quoting:

```bash
export CCS_CLAUDE_ARGS="--permission-mode auto"
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--max-age=N` | 60 | Only search files modified in the last N days (0 = no limit) |
| `--max-size=N` | 1024 | Max file size in MB to include (0 = no limit) |
| `--all` | - | Include everything (same as `--max-age=0 --max-size=0`) |
| `--exclude=a,b` | observer-sessions | Exclude project dirs whose path contains any of these substrings |

### Keybindings

- Text boxes (the search, the message box) edit like Claude Code's prompt: `Ctrl+A`/`Ctrl+E` start/end, `Ctrl+K`/`Ctrl+U` delete to the end/start, `Ctrl+W` the word before, `Ctrl+Y` puts back what they deleted, `Alt+B`/`Alt+F` move by word. So ccs's own actions are `Ctrl+X` then a letter (`Ctrl+X Ctrl+W` works too)
- `↑/↓` or `Ctrl+P/N` - Navigate list
- The list is in sections by state, each with a header: waiting on you, asked you something, done (finished since you last looked at it in ccs, a cyan `●`), idle, busy, not open in `claude`
- `Ctrl+X W` - Sessions waiting on you: their question, a box to answer, Enter opens the session's tab, Esc snoozes
- `Ctrl+X A` - Auto-focus: when a session starts waiting on a question or permission prompt, switch to its iTerm tab or tmux pane (without pulling iTerm in front of another app), once you've stopped typing for 2 seconds (moving the mouse doesn't count), and not again until that question is answered. Questions already waiting when ccs starts count too; one it already switched to before a restart doesn't. Off by default, remembered across restarts
- `Ctrl+X P` - Pin the selected session: pinned sessions sit in their own section at the top, whatever their state, in the order you pinned them; `Alt+↑`/`Alt+↓` move one among them. Again to unpin. Kept across restarts
- `Ctrl+X C` - With folders grouped, collapse the selected session's folder to its most urgent session, its header counting the rest (`▸ api 5 · 4 more`). Again to expand. Kept across restarts
- `Ctrl+X S` - Put the selected session away: settle it (until its next message) or snooze it (an hour, 4 hours, tomorrow 9:00). It moves to the settled or snoozed section at the bottom, and comes back early if it waits on you. Again to bring it back
- `Ctrl+X G` - Group sessions by folder within each state section (a worktree under `.claude/worktrees` counts as its repo), the most urgent group first. Again to ungroup. ccs remembers this and the ^X T layout, and reopens the way you left it
- `Ctrl+X T` - Switch between the sidebar (sessions in a narrow column beside the conversation, the default; each on two rows: the title, then a grey line of when it was last active, its context, project and PR counts) and the full-width table with every column (when, project, model, size, context, messages, hits, PRs). The list and the conversation each have a scrollbar whose thumb is the share in view
- `Enter` - Resume the selected conversation. On one already open in `claude` (`●`), Enter jumps to its iTerm tab / tmux pane instead
- `Ctrl+F` - Fork selected conversation (`claude --resume <id> --fork-session`)
- `Ctrl+D` - Delete selected conversation (with confirmation)
- `Ctrl+X E` - Edit the selected conversation's name (not while it's open in `claude`; use `/rename` there)
- `Ctrl+R` - Refresh now: rescan sessions, re-check PRs and the usage allowance, and check for a ccs update
- `Ctrl+X X` - Prune selected conversation - shrink it losslessly (with confirmation; not while it's open in `claude`)
- `PgUp`/`PgDn` - Scroll preview (also the mouse wheel)
- Mouse wheel - scrolls whatever is under the pointer (the conversation or the list); click a session to select it, click a link in the conversation to open it. Hold `⌥ Option` while dragging to select text in iTerm
- `Esc` - Clear the search and go back to the top of the list
- Message box - select an open (`●`) session and press `Ctrl+S` (or click the box) to type in the box under its conversation; `Enter` sends (through Claude Code's message socket, or typed into its iTerm tab / tmux pane if the socket isn't reachable), `Esc` returns to the search
- `Tab` - Switch between the session list and the usage screen
- `Ctrl+X U` - Switch Claude account (needs [cswap](https://github.com/realiti4/claude-swap), e.g. `uv tool install claude-swap`); clicking the account email does the same
- `?` - Show the shortcuts that apply right now (the header lists only the common ones, in compact form: `^S` for Ctrl+S, and drops hints on narrow terminals rather than wrapping)
- `Ctrl+L` - Changelog: what changed in every release, newest first, the installed and newer ones marked; scroll with ↑↓, PgUp/PgDn or the wheel
- `Ctrl+O` - Links: the selected session's PRs, then the links in the visible part of the conversation, each PR with its status; type to filter them (text, address or status, e.g. `merged`), ↑↓ and Enter to open one in the browser, staying open for the next (clicking a link works too)
- `Ctrl+]` / `Ctrl+\` - With a search typed, jump to the next / previous matching message in the conversation (wraps round); the preview shows `hit 2/5`
- `Ctrl+C` - Quit

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

You can also prune a single conversation from the search interface: select it and press `Ctrl+X X` (with confirmation).

## How it works

ccs reads conversation history from `~/.claude/projects/` and presents them in an interactive TUI. Live sessions come from `~/.claude/sessions/<pid>.json`, counted only while that pid is still running. When you select a conversation, it changes to the original project directory and runs `claude --resume <session-id>`.

## License

MIT

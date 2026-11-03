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
- Start and close named sessions in iTerm tabs (`ccs launch`, `ccs close`, or `Ctrl+X N` / `Ctrl+X K` in the list)
- Watch your open PRs and get one JSON line per change (`ccs watch`)
- Record which session owns each PR, and list your open PRs with their owners (`ccs claim`, `ccs prs`)
- Ask for reviews on green PRs in batches, routed by a headless Claude (`ccs watch --review`, `ccs review`)
- Tell the session that claimed a PR when its checks fail, it conflicts or it gains unresolved threads (`ccs watch`)
- Pass flags through to `claude` (e.g., `--plan`)
- Usage screen (`Tab`): the last 12 hours of token usage across all sessions (including subagents) as three charts (cache-write disposition, context assembly, output), a summary with effective tokens, and your live 5-hour and weekly allowance. The allowance reads Claude Code's stored login read-only (macOS Keychain, else `~/.claude/.credentials.json`); ccs never refreshes or writes it, so if it has expired, open `claude` to refresh it
- Checks for new releases at startup and every 2 minutes (update steps are logged to `~/Library/Logs/ccs/update.log`) and offers to update and restart from a popup (via `brew upgrade` for Homebrew installs, otherwise by replacing the binary with the checksum-verified release). Every update, Homebrew included, is checked against a signature that only ccs's release pipeline on `main` can make, so a release uploaded any other way is refused


## Let Claude search your sessions (MCP)

`ccs mcp` is an MCP server that lets Claude find and read your earlier Claude Code sessions. Add it once:

```bash
claude mcp add --scope user ccs -- ccs mcp
```

Claude starts it when needed (no ccs window has to be open, no network port). It has six tools: `search_sessions` (every word must appear; sessions named after the query first, then by matching messages and recency, each with a snippet and its resume command), `list_sessions` (most recent first) `read_session` (a session's messages, paged, optionally only those around a search term), `launch_session` / `close_session`, which do what `ccs launch` and `ccs close` do (below), and `list_prs`, which is `ccs prs --json`. It searches the same window as the TUI (60 days, files under 1GB); use `ccs mcp --all` or `--max-age=N` to change it.

## Start and close sessions

```bash
ccs launch --name fix-login --brief brief.md --cwd ~/code/app   # claude, the brief as its first prompt
ccs launch --name fix-login --resume <session-id>               # resume one under a name
ccs launch --name tests --brief brief.md --codex                # codex instead
ccs close fix-login
```

`ccs launch` opens an iTerm tab (at the end of the tab strip if iTerm's "new tabs at the end" setting is on, focus staying where it was), clears the prompt and runs a launcher kept in `~/Library/Caches/ccs/launch/<name>/`: a login zsh that sources `~/.claude.secret` when there is one and runs `claude "<brief>" --name <name> $CCS_CLAUDE_ARGS`, the brief first since `--channels` takes every word after it. It then waits up to 20 seconds for a session with that name to start, printing its pid and tty, and fails if none does. A name already running is refused. With `--codex` it runs `codex -s workspace-write -a never --add-dir <that folder>`, the folder being how ccs finds it again.

`ccs close <name>` finds the one running session with that name (two with the same name are left alone), stops it, confirms its process has exited, then closes its iTerm tab (or tmux pane). A session with no terminal (an app's, or `claude --bg`) is refused.

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
- Background sessions (`claude --bg`) show `bg <state>` on the grey row: Enter opens one with `claude attach` in a new tmux window or iTerm tab, and `Ctrl+X K` twice stops it (on a session in a terminal tab, `Ctrl+X K` twice stops it and closes the tab). One started from another session's `claude --bg` sits under it
- Messages between sessions show at both ends: the sender's preview shows each `SendMessage` as `To <session>` (`✓ arrived` once it's in that session's transcript), the recipient's `From <session>`
- A live session run by an app without a terminal shows `in <app>`. One in T3 Code opens in T3 Code with Enter, at its thread; any other app gets a hollow `○`, since ccs can't switch to it, and Enter says to open it there. Messaging it (`Ctrl+S`) and forking it (`Ctrl+F`) work either way
- Each open session has a circle, `●` (or `○` in an app ccs can't open), coloured by its state: magenta waiting on you, light magenta asked you something, cyan done, green idle, amber busy
- The list is in sections by state, each with a header: waiting on you, asked you something, idle (a cyan `●` marks one finished since you last looked at it in ccs; it turns green once you select it and move on, without moving), busy, not open in `claude`
- `Ctrl+X W` - Sessions waiting on you: their question, a box to answer, Enter opens the session's tab, Esc snoozes
- `Ctrl+X R` - Record a CPU profile while you do whatever feels slow, then `Ctrl+X R` again to save it beside the update log (`~/Library/Logs/ccs/cpu-<time>.pprof` on macOS; read it with `go tool pprof -top`). The header says `recording profile` until you stop it. `CCS_CPUPROFILE=path ccs` records the whole run instead
- `Ctrl+X I` - Automatic updates, on by default: a new ccs release downloads in the background and installs once you're away (ccs not focused, or no key for 3 minutes, with nothing unsent), then ccs restarts where you were and says it updated itself, with what changed. Off, new versions are offered in a popup instead. The header shows which (`auto-update on`), remembered across restarts
- `Ctrl+X A` - Auto-focus: when a session starts waiting on a question or permission prompt, switch to its iTerm tab or tmux pane (without pulling iTerm in front of another app), once you've stopped typing for 2 seconds (moving the mouse doesn't count), and not again until that question is answered. Questions already waiting when ccs starts count too; one it already switched to before a restart doesn't. Off by default, remembered across restarts
- `Ctrl+X P` - Pin the selected session: pinned sessions sit in their own section at the top, whatever their state, in the order you pinned them; `Alt+↑`/`Alt+↓` move one among them. Again to unpin. Kept across restarts
- `Ctrl+X C` - With folders grouped, collapse the selected session's folder to its most urgent session, its header counting the rest (`▸ api 5 · 4 more`). Again to expand. Kept across restarts
- `Ctrl+X S` - Put the selected session away: settle it (until its next message) or snooze it (an hour, 4 hours, tomorrow 9:00). It moves to the settled or snoozed section at the bottom, and comes back early if it waits on you. Again to bring it back
- `Ctrl+X G` - Group sessions by folder within each state section (a worktree under `.claude/worktrees` counts as its repo), the most urgent group first. Again to ungroup. ccs remembers this and the ^X T layout, and reopens the way you left it
- `Ctrl+X T` - Switch between the sidebar (sessions in a narrow column beside the conversation, the default; each on two rows: the title, then a grey line of when it was last active, its context, project and PR counts) and the full-width table with every column (when, project, model, size, context, messages, hits, PRs). The list and the conversation each have a scrollbar whose thumb is the share in view
- `Enter` - Resume the selected conversation. On one already open in `claude` (`●`), Enter jumps to its iTerm tab / tmux pane instead
- `Ctrl+F` - Fork selected conversation (`claude --resume <id> --fork-session`)
- `Ctrl+D` - Delete selected conversation (with confirmation)
- `Ctrl+X N` - Start a new session in an iTerm tab, in the selected session's folder: type its name, Enter, then its brief, Enter (`ccs launch`)
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
- `Ctrl+O` - Links: the sessions the selected one messaged or heard from (Enter selects that session in the list), its PRs, then the links in the visible part of the conversation, each PR with its status; type to filter them (text, address or status, e.g. `merged`), ↑↓ and Enter to open one in the browser, staying open for the next (clicking a link works too)
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

## Watching your PRs

`ccs watch` checks one author's open PRs in one GitHub owner every 3 minutes and appends a JSON line for each change to `~/Library/Logs/ccs/pr-events.jsonl` (and prints it), for a script or a coordinating Claude session to follow with `tail -F`. It uses gh and GitHub's REST API.

```bash
ccs watch                                  # brtkwr's PRs in two-inc, every 3 minutes
ccs watch --author me --owner my-org --interval 5m
ccs watch --once                           # one pass, then exit
```

Events: `new`, `checks_failing` (with the failing checks), `checks_passed`, `needs_review` (checks green, not a draft, no approval: once per head commit), `ready` (approved, checks green, not a draft), `conflicts`, `threads_unresolved` (with `unresolved`, when the count of unresolved review threads rises), `threads_resolved` (back to none), `merged`, `closed`. Any failed check, optional or not, counts as failing, and so does an unresolved review thread: such a PR is never `ready` or `needs_review`. Threads are counted only for PRs whose checks pass, with one small GraphQL query each (REST doesn't say whether a thread is resolved). Each line has `at`, `event`, `pr`, `repo`, `number`, `title` and `head`.

```json
{"at":"2026-11-03T09:12:51Z","event":"checks_failing","pr":"https://github.com/o/r/pull/220","repo":"o/r","number":220,"title":"...","head":"0fc4f95...","checks":["setup"]}
```

The first run only records what it finds and writes one `watch_started` line, so it doesn't report every PR as news. Its state is kept beside ccs's settings (`pr-watch.json`), so a restart reports what changed while it was stopped, without repeating anything. Only one `ccs watch` runs at a time.

## Asking for reviews

`ccs watch --review` also asks for reviews, as you, on PRs that are green, not drafts and waiting for an approval, once per head commit (PRs already waiting when it starts included):

- PRs are batched: the batch goes once no PR has joined it for 10 minutes (at most 30), and never between 22:00 and 07:00 London time, when it waits for one morning batch.
- A headless `claude -p --model sonnet` routes the batch by your routing guide (`--review-guide`, a file saying who reviews what, which channels, who gets a DM), with the PRs' requested reviewers and owning sessions. It can only read: it posts nothing itself.
- ccs checks its plan (every PR placed once, links only to the batch's own PRs, `--review-mention` present, one line per PR plus one) and posts the one message it sends on its own: the review request in `--review-channel`, through `SLACK_USER_TOKEN` (from the environment or `~/.claude.secret`), ending with `_✨ via ccs watch · claude sonnet_`.
- Everything else, such as a DM to a reviewer the channel doesn't reach, is a draft that waits for you. A PR whose prerequisite hasn't merged is held and planned again after an hour.

```bash
ccs watch --review --review-channel C0123ABCD --review-mention '<!subteam^S0123ABCD>' --review-guide ~/review-guide.md
ccs watch --review ... --dry-run  # print the plan for the PRs waiting now; posts and records nothing
ccs review list                   # drafts waiting for you
ccs review send r3                # post draft r3 as you
ccs review drop r3                # discard it
```

Events: `review_requested` (channel, PRs, permalink), `review_dm_pending` (id, recipient, text, PRs), `review_held` (PR, reason), `review_plan_failed` (PRs, reason; the batch is tried again after the wait). What was asked for, per PR and head commit, is kept in `review.json` beside ccs's settings.

## Telling sessions about their PRs

After each pass, `ccs watch` tells the session that claimed a PR (`ccs claim`, below) what's wrong with it: checks failing (by name), conflicts with its base, or more unresolved review threads than it was last told of. Each running owner is told once per PR, head commit and reason, so it isn't nagged every pass; a new commit that still fails is told again. A session that isn't running, or a PR nobody claimed, gets nothing beyond the usual events.

The notice goes over the session's own message socket, as `ccs watch`, and says it's an automatic status notice, not typed by you, so the session treats it as information rather than your instruction:

```
<PR URL> (<title>) has 2 unresolved review threads; checks failing: setup.
This session claimed it with ccs claim. If it's yours, deal with it; if not, run: ccs claim <PR URL> --release
```

Events: `nudged` (pr, head, session, reasons) and `nudge_failed` (the same plus the error; tried again next pass). What was sent is kept in `nudges.json` beside ccs's settings, so a restart never sends it again. `--no-nudge` turns this off.

## Who owns each PR

A session that owns a PR says so by running `ccs claim <PR URL>` (`--release` to give it up). ccs works out which session ran it from the claude process it runs under, and records the session's id and name in `claims.json` beside its settings. A claim ends by itself when the PR merges or closes, or the session stops.

`ccs prs` lists your open PRs with their owners, needing you first, and flags any PR nobody has claimed (`UNOWNED`) and any PR two running sessions both claim (`CONTESTED`). It uses `ccs watch`'s state when that is under 10 minutes old, and otherwise asks GitHub (REST) itself. A PR with a failed check, optional or not, needs you.

```bash
ccs claim https://github.com/o/r/pull/12
ccs prs                  # one line per PR
ccs prs --json           # the same, for scripts
ccs prs --statusline     # e.g. "PRs 3 ready · 1 need you · 2 unowned", from saved state only
```

`--statusline` never asks GitHub, so it is quick enough for Claude Code's status line; it prints nothing when there is nothing to count, and adds `(stale)` when `ccs watch` hasn't refreshed the state for 10 minutes.

## How it works

ccs reads conversation history from `~/.claude/projects/` and presents them in an interactive TUI. Live sessions come from `~/.claude/sessions/<pid>.json`, counted only while that pid is still running. When you select a conversation, it changes to the original project directory and runs `claude --resume <session-id>`.

## License

MIT

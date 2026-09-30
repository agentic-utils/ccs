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

If a merge's push event is lost and no release runs, start it by hand: `gh workflow run CI --ref main`.

Install the release locally: `brew update && brew upgrade ccs`.

## Architecture

- `main.go` - Single file containing all logic
- `main_test.go` - Unit tests
- `.goreleaser.yaml` - Release configuration
- `.github/workflows/test.yaml` - CI test workflow (reusable)
- `.github/workflows/ci.yaml` - On push to main: runs test.yaml, then tags and releases

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
- `handleMouse()` / `listLayout()` / `mouseLeak` - mouse reporting is on: the wheel scrolls whatever is under the pointer (conversation preview vs session list, by row from `listLayout`, which must match `View`), a click selects a list row; a mouse report that arrives split and reads as typed text is dropped before the search box
- `buildPreviewLines()` - with no search, the whole conversation; with a search, the first/last two messages plus matches with one message of context, gaps marked. `previewScroll` counts lines back from the newest message, so the preview opens (and any reset lands) at the latest message; `applyLive` shifts it by the lines added so a scrolled-back view stays still while new messages arrive
- `renderBody()` / `harnessNote()` - preview readability: each message is wrapped to the preview width with a hanging indent (`ansi.Wrap`, ANSI-aware), markdown rendered (bold, inline code, headings, bullets, `[text](url)` and bare addresses as underlined text plus a short grey address, both clickable OSC 8 terminal links (`hyperlink`, one per word so a wrapped link never stays open), tables as aligned columns via `renderTable`, widest columns cut with … to fit), messages shown in full up to `maxMessageRunes`. Fenced code blocks over `maxCodeLines` collapse to `▸ lang · N lines` unless the search matches inside. Harness-injected user messages (`<task-notification>`, `<system-reminder>`, command output…) collapse to one dim `▸ tag · summary` line; messages from ccs/other sessions stay expanded
- `timeGutter()` - consecutive messages from one speaker share a `User` / `Claude` header (a new one on a speaker change, after a gap or harness line, and for each search match, marked `▶`); the date is a dim `── 2026-09-29 ──` row (`dateRow`), once per day, and `renderPreview` pins the top visible message's date in the header's last line unless its own row is already at the top; harness notes and messages from other sessions use the same time gutter, and consecutive notes stack without blank lines; every message starts with its time (`21:45`) and the text lines up after it
- `linkAt()` / `openURL` / `carryStyles()` - ccs holds the mouse, so the terminal never sees a click on a link: a left click in the preview finds the OSC 8 link under the pointer in the rendered line and opens it (`open`/`xdg-open`, http(s) only). Wrapped lines are self-contained (`carryStyles` closes open styles/links at each line end and reopens them on the next), so an underline never bleeds across the screen
- `teammateBlocks()` - messages from teammate agents (`<teammate-message teammate_id=… summary=…>`, several per message, optionally after "Another Claude session sent a message:") show one `From <id> · <summary or status>` block each; a JSON status report is unpacked (its `result`/`message` rendered as markdown), and a status with nothing to read (idle, shutdown) is one dim `▸ id · status` line
- `previewLines()` - Memoised `buildPreviewLines` for the selected conversation (rebuilt only when selection, query, message count or width changes), avoids per-frame rescans of huge conversations
- `hitCount()` / `countHits()` - Memoised per-query HITS count (messages containing the query), keyed by SessionID, so `formatListItem` doesn't rescan every visible row each frame
- `formatAgo()` - WHEN column: time since the last message (`now`, `5m ago` ... `1y ago`), recomputed each frame; replaces the absolute date in the list (full timestamps stay searchable and show per message in the preview)
- `formatTokens()` / `Conversation.ContextTokens` - CTX column: context size as of the last reply (input + cache reads + cache writes from its `usage`; zero-usage placeholder replies skipped)
- `contextWindow()` / `ctxColour()` - CTX column traffic light (green/yellow/amber/red/flashing, thresholds scaled to a 200k or 1M window inferred from the model, or proven 1M by a context over 200k)
- `Conversation.ActiveModel` / `shortModel()` - the model in use: the last reply's `message.model`, or a later `/model` switch parsed from its logged `Set model to …` output (`Model1M` when it names a 1M-context model). MODEL list column shows it short (`opus 5.5 1M`); the preview shows it exactly, noting a switch since the last reply
- `sessionStats()` / `tokenUsage.effective()` - preview header: model, context of window, summed usage (one reply spans several lines with the same `message.id`; counted once), effective tokens (1x input, 1.25x 5m write, 2x 1h write, 0.1x read, 5x output) and cost at `pricePerMTok`; the last surfaced API error (`isApiErrorMessage`) until a later successful reply, also shown as a red `!` in the list
- `formatListItem()` - Formats a single list row; `●` live / `⚙` spawned / `!` API error are packed right into a 3-cell column (`colMarks`) just before the title, so titles stay aligned; `✍` trails a user-set name
- `readLiveSessions()` / `liveSessionPIDs()` - SessionIDs open in a running claude, from `~/.claude/sessions/<pid>.json` where the pid is alive and its start time (`ps lstart`, read in UTC) matches the file's `procStart` (UTC), so a recycled pid doesn't count. Files are checked per pid before mapping to sessions, so a stale leftover can't hide the live file
- `liveTick()` / `liveCmd()` / `applyLive()` - every `liveInterval` (2s): re-read `~/.claude/sessions` and, for live (and just-exited) sessions, `parseAppended()` their transcripts; updated rows re-sort in place, keeping the cursor; `applyLive` re-matches and re-counts only changed rows (preview cache is keyed by SessionID+Size+query, so it rebuilds only if the selected conversation changed). A live session not in the list yet triggers an early full scan (`startRefresh(true)`, at most every 15s; early scans don't reschedule, so the one-minute chain stays single)
- `parseAppended()` / `consumeLines()` - incremental parse: seek to `parsedBytes` (end of the last complete line) and parse only appended lines. A line caught mid-write isn't applied, so parsing resumes at it; full reparse only if the file shrank (prune) or an unterminated last line *was* applied as a record (`tailApplied`). Search text stays identical to `searchTextOf` (which ends with the last timestamp): new messages swap that suffix and lowercase only the new part (`appendedSearchText`); anything else rebuilds. Live reads run in parallel with a per-tick deadline; a read still stuck from an earlier tick isn't restarted (`liveReads`)
- `Conversation.readAt` / `keepNewer()` - every parsed copy records when it was read from disk; between a full scan and the live tick the later read wins (in both directions). Size can't decide it: a prune legitimately shrinks a file
- `runBounded()` / `runCommand()` - every external command has a deadline (and `WaitDelay`, so an inherited pipe can't outlive it) and reports a timeout as `errTimedOut`. git/brew run detached (`Setsid`, `GIT_TERMINAL_PROMPT=0`, batch-mode ssh), so they can't prompt behind the TUI
- `resumeCmd()` - Enter/Ctrl+F run off the UI goroutine: fresh liveness check, then focus (`focusDoneMsg`), a new tab/window, or exec in place (`resumeDoneMsg`). `tabResult`: only a timeout counts as "may have opened" (reported, no second launch); any other failure (Automation denied, stale `$TMUX`) resumes in place, as does a cwd/arg that isn't `shellSafe` (backslash, control chars). One open at a time; a session opened in a tab stays live via `m.opened` for `openedGrace` until claude's own session file appears. tmux `-c` gets `#` escaped (tmux expands `#(cmd)` there)
- `refreshNote()` - header shows what the background refresh is doing: `refreshing…`, `refreshed just now` / `3m ago`, or `refresh failed, list from 3m ago`. Minute granularity and a static (non-blinking) cursor keep the idle frame identical, so bubbletea writes nothing and terminals don't show the tab as busy; don't add anything that changes every second while idle
- `refreshStalled()` - header shows "refresh stalled Nm" when a scan has run over 5 min (e.g. hung network mount); it can't be cancelled
- `refreshTick()` / `applyRefresh()` - Background re-scan every `refreshInterval` (1 min), keeping the cursor on the same conversation; skipped while a delete/prune/rename prompt is open, and dropped if `m.gen` moved (a delete/prune/rename happened while the scan ran); search text is built once at parse and shared through `parseCache`, so unchanged conversations cost nothing on refresh
- `parseCache` - Reuses parsed conversations whose file size+mtime are unchanged, so refreshes only reparse changed files
- `openResumeTab()` / `resumeInTmuxWindow()` / `resumeInITermTab()` - Opens the selected conversation in a new tmux window or iTerm tab (focus stays on ccs); falls back to exec-in-place elsewhere
- `focusSession()` / `tmuxPaneForTTY()` - Enter on a live session: find its terminal by the claude pid's tty (`liveSessionPIDs()`) and select that tmux pane or iTerm tab instead of resuming a second copy
- Ctrl+F forks: `openResumeTab` / exec with `--fork-session`
- `latestRelease()` / `newerVersion()` / `chooseUpgrader()` / `upgrader` - Self-update: at startup and every `updateCheckInterval` (2 min; `updateCheckBackoff` 10 min after a failed check), read the newest tag from the `github.com/.../releases/latest` redirect (not the REST API, whose 60/h unauthenticated limit shared IPs exhaust). If newer, `upgrader.Prepare` starts the slow, side-effect-free part in the background (Homebrew: `git pull` of just the ccs tap + `brew fetch`; otherwise download + verify against `checksums.txt`) while a popup over the preview offers Enter = install and re-exec, Esc = later (for that version, this session). `Install` waits for an in-flight prepare, then: Homebrew kegs (`/Cellar/`) only ever `brew upgrade agentic-utils/tap/ccs` (never overwritten); `/nix/` notify only; anything else atomically renames the verified binary over the old. The header shows the current step and elapsed seconds (`updateProgress`). Keys are ignored for `updateKeyGrace` after the popup opens. `dev` builds never check
- `releaseChangelog()` / `parseChangelog()` / `changelogBlock()` - when an update is offered, the popup lists what changed in every release after the running version up to the new one (the `releases.atom` feed, not the REST API; it holds the latest 10 releases), commit subjects under each version, capped at `maxChangelogLines`. GoReleaser drops merge commits and hashes from the notes. A failed fetch is logged and the popup shows no list
- `notesPopup()` / `scrollNotes()` - Ctrl+L changelog, any time: every release in the feed (`releaseChangelog("0.0.0", …)`, fetched once per run, refetched only after a failure), the installed version and newer ones marked; owns the keyboard (↑↓ ^P/N ^J/K, PgUp/PgDn, the wheel; ^L/esc close) and stops at the end
- `downloadTransport` / `dialTLSAnyAddr()` - every self-update download tries each address the host resolves to, with `addrAttemptTimeout` (5s) for TCP + TLS, moving on when one fails; failed addresses go last for the rest of the process. Each attempt is logged
- `preseedBrewCache()` / `seedFile()` - Homebrew installs: take the formula's URL and sha256 from `brew info --json=v2` and the path from `brew --cache`, download with `downloadTransport`, verify, and write atomically into brew's cache so `brew fetch`/`upgrade` find it there (brew verifies again). Any problem is logged and brew downloads as usual; nothing outside the cache is touched
- `logUpdate()` / `updateLogPath` - self-update log (`~/Library/Logs/ccs/update.log` on macOS, capped at 1MB): every git/brew command with full output, exit and duration (brew fetch/upgrade run with `HOMEBREW_CURL_VERBOSE=1`, so each address curl tried is visible), every download with the address it connected to, and each prepare/install outcome. A failed update's popup points at it
- `collectUsage()` / `usageView()` / `renderUsageChart()` - Usage screen (Tab), ported from claude-dashboard: every transcript (subagents included) modified in the last 12h, replies de-duplicated by `message.id` (else `requestId`), into 144 five-minute buckets: chart 1 uncached/5m write/1h write, chart 2 cache read/new input/cache miss (fresh input on a turn that read 0 from cache), chart 3 output; buckets merge to fit narrow terminals. Per-file records cached by size+mtime (`usageFiles`). SUMMARY: 12h totals, effective tokens 1h/12h, cache mix. Collected off the UI goroutine when the screen opens and on each 1-minute refresh while it's open
- `usageSummary()` / `usageView()` panels - below the charts, SUMMARY, CACHE MIX and ALLOWANCE sit in three equal columns across the screen, values right-aligned
- `allowanceSummary()` / `accountEmail()` - the search row shows the Claude account (`oauthAccount.emailAddress` from `.claude.json`) and 5h/weekly % with the 5h reset; the allowance is fetched at startup and each minute. On the usage screen the search box and list-only shortcuts are hidden (`tab back  ^O account  ^L changelog  ^C quit`, every key it has, so Ctrl+G does nothing there)
- `fetchAllowance()` / `keychainRead` - ALLOWANCE panel: Claude Code's OAuth token read-only (`/usr/bin/security find-generic-password` for `keychainService()`, which hashes CLAUDE_CONFIG_DIR like Claude Code, else `.credentials.json`), then GET `api/oauth/usage` (`limits`: `session`, `weekly_all`). An expired/missing token or a 401 shows "open claude to refresh the allowance"; ccs never refreshes or writes the credential. At most once a minute, only while the usage screen is open
- `shortcuts()` / `hints()` / `helpPopup()` - key hints everywhere use the compact labels (`keyEnter` enter, `keyTab` tab, `keyEsc` esc, `^S` for Ctrl+S; keys bold) via `hints()`. The header shows only the common keys; Ctrl+G opens a popup of the keys that work right now, from `shortcuts()` (screen, selection, live or not, message box focus, a search typed, cswap installed; add new keys there with their condition). The popup owns the keyboard (Ctrl+G/Esc close, Ctrl+C quits) and is overlaid last
- `View()` / `viewScreen()` / `overlayCentre()` - `viewScreen` draws the current screen (list or usage); `View` then overlays popups (the update offer) centred on top, ANSI-aware so the screen stays visible around them. Screens never draw popups themselves, so any new screen gets them for free
- `uiState` / `restore()` / `restoreEnv` - before the post-update restart, `uiState()` (screen name, search text and cursor position, selected SessionID, preview scroll) goes into `CCS_RESTORE_STATE` for the new process only; `main` reads it once, unsets it, and `restore()` puts the user back (a missing conversation is skipped). A new screen needs a case in `screenName()` and `restore()`
- Message box (`chat*`, `deliverMessage()`) - when the selected conversation is live, its preview gets a status line and a one-line message box. Focus moves to the box only when the user moves the selection onto a live session (`selectionMoved`: keys, wheel, click), never through search filtering; Esc or clicking the search row returns it. Enter sends via `sendViaSocket` (the session file's `messagingSocketPath`: newline-delimited JSON, an `auth` line with the `peerToken` from `<pid>.<sha256(socket path as written)>.key`, then a `user` message wrapped in `<cross-session-message from-name="the user, via ccs">`, starting with `ccsNote`, which tells the recipient it was typed by the user, not another Claude session, so it answers in its own conversation instead of messaging back; `peerParts` strips the note for the preview); only if the socket can't be reached at all (`errNotConnected`) does it type into the terminal instead (`typeIntoSession`: tmux `send-keys -l`, or iTerm `write text`; control characters refused). Never both. The socket gives no acknowledgement, so a message is only called delivered once `checkDelivered` sees it in the transcript. Messages from ccs or other sessions (`peerMessage`, kept even though marked meta) show in the preview under a `From <name>` header. While a live session is selected, `chatTickCmd` checks its transcript and busy/idle status every `chatFastRefresh` (250ms); an idle frame stays static, and `✻ working Ns` only ticks while busy
- `acctKey()` / `acctPopup()` / `cswapActionCmd()` - account switcher (Ctrl+O, or click the account email in the search row via `accountSpan()`), backed by the cswap CLI (`cswapPath`, absent means Ctrl+O just says to install it): rows from `cswap list --json` (active marked, 5h/7d from `usage` else `lastGoodUsage`, `re-login` when `relogin_required`, as claude-dashboard). Digits or ↑↓ Enter run `cswap switch <n>`, `+` runs `cswap add`, both detached with a deadline, one at a time; the popup owns the keyboard and is overlaid last in `View`. After a change the list and the allowance/account are re-read (an in-flight allowance fetch is marked `allowanceStale` and repeated). Tests use a fake cswap on PATH; never run switch/add against real accounts in tests
- `deleteConversation()` - Removes conversation file and updates UI state
- `pruneConversation()` - Prunes the selected conversation in place (Ctrl+X) and refreshes its size
- `getTopic()` - Title, else first real user message (skips tag-wrapped harness text, shows `/cmd` or `! cmd` for command-only sessions), else session id
- `renameConversation()` / `appendLine()` - Ctrl+R rename: appends a `custom-title` line like `/rename`; refused on live sessions since a running claude re-appends its own title
- `Conversation.Spawned` - first user line has `entrypoint: sdk-cli` or a `teamName`; shown as `⚙`
- `pruneFile()` / `pruneStream()` - Shrink a conversation by dropping duplicate/redundant data (never touches user/assistant lines)
- `runPrune()` - `ccs prune` subcommand driver

### TUI Layout

```
  ccs · claude code search · v0.44.0 · refreshed just now          enter resume  ^S msg  tab usage  ^G help  ^C quit
  > type to search...                                                     (N/total)

  WHEN      PROJECT               TOPIC                    MODEL          SIZE   CTX  MSGS  HITS
──────────────────────────────────────────────────────────────────────────────────────
  2h ago    project-name          ● Refactor auth flow ✍  opus 5.5 1M   1.2GB  281k    42     3
> 3h ago    selected              This one is selected     sonnet 5       12MB   92k    28     1
──────────────────────────────────────────────────────────────────────────────────────
Project: /path/to/project
Session: abc123...

  ── 2024-01-08 ──

  User
  12:00 message text here...

  Claude
  12:01 response text here...
  12:02 a second reply, no repeated header
```

## Conventions

- Every interaction must be possible from the keyboard; the mouse (click, wheel) is only ever a shortcut for something a key already does

- Use conventional commits (feat:, fix:, docs:, etc.)
- Run tests before releasing
- Keep it simple - single file is fine for this project

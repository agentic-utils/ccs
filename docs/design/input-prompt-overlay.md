# Input-prompt overlay

Status: approved; first version built in wait.go.

## Problem

When several Claude sessions run in different tabs, a session that stops to ask you something sits idle until you happen to look at its tab. ccs already knows which sessions are live and can message them, but it gives no sign that one is waiting on you. The idea: when a live session needs input, ccs pops an overlay over its own screen, showing that session's transcript (scrollable, for context) above a message box, so you can answer or defer without hunting for the tab.

Decided already: the overlay lives inside the ccs TUI, so there's no separate window and no background watcher. It shows only while ccs is open. Defer means snooze for 15 minutes (configurable), after which the overlay pops up again. ccs reads only Claude Code's own files, never another tool's database.

## Detecting "waiting"

**Signal 1: the session file.** Claude Code writes `status` and `statusUpdatedAt` into `~/.claude/sessions/<pid>.json`. A snapshot of the 10 live session files on this Mac showed four status values: `busy` (1), `idle` (4), `shell` (4) and `waiting` (1). Only the waiting file had `waitingFor`, set to `"input needed"`; it's absent otherwise. ccs already parses `status` and `statusUpdatedAt` (chat.go:59-65), but only for the selected session, every 250ms (chat.go:111-135), and only acts on `busy` (chat.go:162, 170). The live tick already opens every session file every 2s (live.go:216-255) and could read the same three fields there, so all live sessions get a status at no extra cost.

What triggers `waiting` was tested (see Findings): a permission prompt and an AskUserQuestion menu both give `waiting`, each with its own `waitingFor`; a plain question in Claude's text ends the turn as `idle`. The transcript alone still can't say what a prompt is, since the pending tool call isn't parsed, so the overlay shows `waitingFor` and the last messages and doesn't claim more.

**Signal 2: the last turn asks you something.** A live session that's `idle` and whose last assistant message ends in `?` is probably waiting on you too. It's a guess, so it gets a separate, dimmer marker and never pops the overlay on its own; it only appears in the overlay's queue when you open it by hand.

**Later: tool calls.** Parsing AskUserQuestion calls with no result yet would give the real question and its options. ccs drops tool calls when parsing today, so this is separate work.

The list gets a marker in the existing marks column. A yellow `?` for `waiting` and a dim `?` for signal 2 take the place of the green `●`, since a waiting session is live by definition. That keeps the column at its three cells (`colMarks`, ui.go:1309). The header shows a count, such as `2 waiting`.

## Overlay layout

Drawn with `overlayCentre` like the other popups (ui.go:1011-1023, 1127), at about 90% of the screen so the transcript has room:

- **Title row:** `waiting 1 of 3 · <session name> · input needed · 4m`.
- **Transcript:** the existing preview rendering (`previewLines`), opened at the newest message like the main preview.
- **Message box:** the existing chat box and delivery status (chat.go:150-190).
- **Hints:** one row of key hints, via `hints()`.

**Order: oldest waiting first**, by `statusUpdatedAt`. The wheelhouse inbox shows newest first within each band. That's good for "what did I just trigger", but with snoozes re-popping, newest-first lets an old prompt starve behind fresh ones. Oldest-first gets the longest-blocked session moving first. Signal 2 sessions go after all `waiting` ones.

**When it pops up on its own:** when a session newly enters `waiting`, and at startup if any sessions are already waiting (oldest first, since the longest-waiting one is the one you most likely want). Never while another popup is open, a prompt is active (ui.go:244), or the message box or search has had a keystroke in the last few seconds. A blocked pop-up waits for the next quiet tick.

## Keys

Like the other popups (ui.go:660-735), the overlay owns the keyboard. Every key below is checked against ccs's bindings and the text box's own (bubbles v0.21.0 textinput).

| Key | Action | Notes |
|---|---|---|
| Ctrl+Y | open the overlay by hand | Also closes it. It's the only Ctrl+letter that neither ccs nor the text box uses (Ctrl+Q and Ctrl+Z belong to the terminal). Ctrl+W would delete a word in the search box. |
| Enter | send the typed message | As today. |
| Enter on an empty box | go to the session's tab | Reuses `focusSession`, the same rule as the main screen. |
| Esc | defer: snooze this session, show the next | Closes the overlay when none are left. |
| Ctrl+N / Ctrl+P | next / previous waiting session | Doesn't snooze. Same keys as list navigation. |
| PgUp / PgDn, Ctrl+K / Ctrl+J, wheel | scroll the transcript | As the main preview. |
| Ctrl+C | quit | As everywhere. |

Esc snoozes because closing the overlay without answering means you aren't answering now. Ctrl+Y closes without snoozing; the overlay then pops up again only for a new wait. Ctrl+N skips without snoozing, and ↑↓ do the same as Ctrl+P/N.

## Defer

A snooze holds a session out of the queue and stops it popping up until the time is up. Then, if it's still `waiting`, it can pop up again. Leaving `waiting` (answered in its own tab, or busy again) clears the snooze. The length is `CCS_SNOOZE` (default `15m`).

**The state lives in memory, not in a file.** Weighing the two:
- **A file** survives a restart and could be shared by two ccs windows. But it adds stale state to clean up, and it raises the question of whether one window's snooze should silence another.
- **Memory** is lost on restart, so a snoozed session pops up again when ccs starts. That's acceptable: restarting ccs means you're looking. Snoozes are carried across ccs's own post-update restart by adding them to `uiState`/`restore()`, which exist for exactly that.

## Answering menus: the main risk

Permission prompts and AskUserQuestion are menus inside the session's terminal, not text prompts. ccs sends through the session's message socket and only falls back to typing into the terminal when the socket can't be reached, never both (chat.go:229-249, 444). *Hypothesis (not tested):* a socket message sent to a session sitting on a menu is queued as a message for later (as ccs already sees with busy sessions), so it doesn't pick an option. Typing would land keys in the menu, but ccs can't know which key does what.

So the first version answers only by message. For anything it can't answer, it points at Enter-on-empty to jump to the tab. Its title row says plainly that a menu may need answering in the session itself. Answering menu options from the overlay waits until tool-call parsing shows what the menu is.

## First version

- Read `status`, `waitingFor` and `statusUpdatedAt` for every live session in the live tick.
- The `?` list marker and the header count, for both signals.
- The overlay: open by hand, pop on entering `waiting` and at startup, oldest-first queue, the keys above, snooze in memory carried through `uiState`.
- Keys listed in `shortcuts()` so Ctrl+G shows them, and the AGENTS.md key list updated.

Not in it: tool-call parsing, picking menu options, and sharing snoozes between ccs windows.

## Rough size

About 250 lines of Go, plus about 150 lines of tests:
- Status in the live tick: 40 lines.
- Markers and count: 20 lines.
- Overlay view and keys: 150 lines.
- Snooze and restore: 40 lines.
- Tests: about 150 lines, using the fake session files and sockets already in helpers_test.go.

## Findings

Tested with a throwaway interactive claude (Haiku, default permission mode) in a detached tmux session, reading its `~/.claude/sessions/<pid>.json` after each case:

| Case | `status` | `waitingFor` |
|---|---|---|
| Permission prompt (a Bash command needing approval) | `waiting` | `permission prompt` |
| AskUserQuestion menu | `waiting` | `input needed` |
| Plain question at the end of a turn, no tool | `idle` | absent |
| Each prompt dismissed with Esc | `idle` | absent |

So both menus pop the overlay, and signal 2 stays: a plain question never shows as `waiting`.

## Settled

- **Pop at startup:** yes, for sessions already waiting, oldest first.
- **`shell` status:** not waiting. Four of the ten live files said `shell`, presumably a `!` command or shell mode, which is you mid-command rather than Claude asking.
- **Two ccs windows:** both may pop in the first version. ccs can't tell which window has focus; a focus check or lock file can come later.

# Input-prompt overlay

Status: approved; built in wait.go, then reworked after two rounds of adversarial review.

## Invariant

Any change to the overlay must keep these true:

- **A wait starts when a session enters `waiting`**, from any other status, or gets a new `statusUpdatedAt` while waiting (when the file has one). It ends when the session leaves `waiting`. A new prompt that arrives within one tick is a new wait, and so is busy-then-waiting in a file with no timestamp.
- **A snooze, and having been seen, belong to one wait** and end with it. Opening the overlay by hand shows snoozed sessions without ending their snoozes. A snooze survives ccs restarting itself after an update (matched to the wait by `statusUpdatedAt`).
- **A session is forgotten only once its process is gone.** A session file that can't be read or parsed for a tick, while its pid is alive, keeps the last known state; nothing is pruned, snoozed or popped because of it.
- **The overlay never takes keys meant for another input.** When it pops up on its own it takes only Ctrl+Y (to answer there) and Esc (snooze); every other key goes where it would without the overlay. Opened by hand, or after Ctrl+Y, it owns the keyboard until closed.
- **Text is only ever delivered to the session shown when Enter was pressed.** Each session keeps its own draft, and text that wasn't sent goes back to the box it came from.
- **ccs never types into a session that is showing a menu.** Typed keys could pick an option, so the typing fallback re-reads the session file just before typing and refuses a `waiting` session.
- **The popups are listed in one place** (`popups`, bottom first). `View` draws them in that order, and `popupAboveWait` (the ones over the overlay) feeds key routing, the mouse and `canPop`. Nothing opens over the overlay from the keyboard, so in practice only the update offer sits above it.
- **A pid that couldn't be checked still pops, marked unverified.** One whose start time doesn't match the session file's is a recycled pid's leftover: not live at all.

## Problem

When several Claude sessions run in different tabs, a session that stops to ask you something sits idle until you happen to look at its tab. ccs already knows which sessions are live and can message them, but it gives no sign that one is waiting on you. The idea: when a live session needs input, ccs pops an overlay over its own screen, showing that session's transcript (scrollable, for context) above a message box, so you can answer or defer without hunting for the tab.

Decided already: the overlay lives inside the ccs TUI, so there's no separate window and no background watcher. It shows only while ccs is open. Defer means snooze for 15 minutes (configurable), after which the overlay pops up again. ccs reads only Claude Code's own files, never another tool's database.

## Detecting "waiting"

**Signal 1: the session file.** Claude Code writes `status` and `statusUpdatedAt` into `~/.claude/sessions/<pid>.json`. A snapshot of the 10 live session files on this Mac showed four status values: `busy` (1), `idle` (4), `shell` (4) and `waiting` (1). Only the waiting file had `waitingFor`, set to `"input needed"`; it's absent otherwise. ccs used to parse `status` and `statusUpdatedAt` (`sessionFile`) only for the selected session, every 250ms (`chatTickCmd`), and only acted on `busy`. The live tick already opens every session file every 2s (`scanSessions`), so it reads the same fields there and every live session gets a status at no extra cost.

What triggers `waiting` was tested (see Findings): a permission prompt and an AskUserQuestion menu both give `waiting`, each with its own `waitingFor`; a plain question in Claude's text ends the turn as `idle`. The transcript alone still can't say what a prompt is, since the pending tool call isn't parsed, so the overlay shows `waitingFor` and the last messages and doesn't claim more.

**Signal 2: the last turn asks you something.** A live session that's `idle` and whose last assistant message ends in `?` is probably waiting on you too. It's a guess, so it gets a separate, dimmer marker and never pops the overlay on its own; it only appears in the overlay's queue when you open it by hand.

**Later: tool calls.** Parsing AskUserQuestion calls with no result yet would give the real question and its options. ccs drops tool calls when parsing today, so this is separate work.

The list gets a marker in the existing marks column. A yellow `?` for `waiting` and a dim `?` for signal 2 take the place of the green `●`, since a waiting session is live by definition. That keeps the column at its three cells (`colMarks`). The header shows a count, such as `2 waiting`.

## Overlay layout

Drawn with `overlayCentre` like the other popups, at about 90% of the screen (clamped to the terminal; on a short screen the transcript shrinks first, then the menu note goes, then the hints) so the transcript has room:

- **Title row:** `waiting 1 of 3 · <session name> · input needed · 4m`.
- **Transcript:** the existing preview rendering (`previewLines`), opened at the newest message like the main preview.
- **Message box:** the existing chat box and delivery status (`chatBox`).
- **Hints:** one row listing every key, via `hints()`. Before Ctrl+Y it shows how to answer or snooze, and that other keys work as usual.

**Order: oldest waiting first**, by `statusUpdatedAt`. The wheelhouse inbox shows newest first within each band. That's good for "what did I just trigger", but with snoozes re-popping, newest-first lets an old prompt starve behind fresh ones. Oldest-first gets the longest-blocked session moving first. Signal 2 sessions go after all `waiting` ones.

**When it pops up on its own:** when a new wait starts, and at startup if any sessions are already waiting (oldest first, since the longest-waiting one is the one you most likely want). Never while another popup or a rename/delete/prune prompt is open; a blocked pop-up waits for the next tick. It doesn't take the keyboard, so typing in the search or the main message box carries on and it never has to wait for you to stop.

## Keys

Opened by hand, or after Ctrl+Y on a pop-up, the overlay owns the keyboard like the other popups. Before that only Ctrl+Y and Esc are its. Either way Ctrl+L, Ctrl+T, Ctrl+G and Ctrl+O don't open over it. Every key below is checked against ccs's bindings and the text box's own (bubbles v0.21.0 textinput).

| Key | Action | Notes |
|---|---|---|
| Ctrl+Y | open the overlay by hand; on a pop-up, answer there | Again closes it. Works on the usage screen. By hand it also lists snoozed sessions, marked snoozed, without ending their snoozes. It's the only Ctrl+letter that neither ccs nor the text box uses (Ctrl+Q and Ctrl+Z belong to the terminal). Ctrl+W would delete a word in the search box. |
| Enter | send the typed message | As today. |
| Enter on an empty box | go to the session's tab | `focusCmd`, shared with the main message box. It never resumes a second claude: a session that can't be found right now is reported, not resumed. |
| Esc | defer: snooze this session, show the next | Also on a pop-up before Ctrl+Y. Closes the overlay when none are left. |
| Ctrl+N / Ctrl+P | next / previous waiting session | Doesn't snooze. Same keys as list navigation. |
| PgUp / PgDn, Ctrl+K / Ctrl+J, wheel | scroll the transcript | As the main preview. |
| Ctrl+C | quit | As everywhere. |

Esc snoozes because closing the overlay without answering means you aren't answering now. Ctrl+Y closes without snoozing; the overlay then pops up again only for a new wait. Ctrl+N skips without snoozing, and ↑↓ do the same as Ctrl+P/N.

## Defer

A snooze holds a session out of the queue and stops it popping up until the time is up. Then, if it's still `waiting`, it can pop up again. The wait ending (answered in its own tab, busy again) or a new wait (the next prompt) ends the snooze. The length is `CCS_SNOOZE` (default `15m`); a value that isn't a positive duration gets the default and a warning at startup.

**The state lives in memory, not in a file.** Weighing the two:
- **A file** survives a restart and could be shared by two ccs windows. But it adds stale state to clean up, and it raises the question of whether one window's snooze should silence another.
- **Memory** is lost on restart, so a snoozed session pops up again when ccs starts. That's acceptable: restarting ccs means you're looking. Snoozes are carried across ccs's own post-update restart by adding them to `uiState`/`restore()`, which exist for exactly that.

## Answering menus: the main risk

Permission prompts and AskUserQuestion are menus inside the session's terminal, not text prompts. ccs sends through the session's message socket and only falls back to typing into the terminal when the socket can't be reached, never both (`deliverMessage`). For a session that's `waiting` it refuses the typing fallback, since typed keys could pick an option; it reads the session file again just before typing, because the socket attempt can take seconds. The refused text goes back to its box. *Hypothesis (not tested):* a socket message sent to a session sitting on a menu is queued as a message for later (as ccs already sees with busy sessions), so it doesn't pick an option. The overlay's copy says the message "may" wait for that reason.

So the first version answers only by message. For anything it can't answer, it points at Enter-on-empty to jump to the tab. Its title row says plainly that a menu may need answering in the session itself. Answering menu options from the overlay waits until tool-call parsing shows what the menu is.

## First version

- Read `status`, `waitingFor` and `statusUpdatedAt` for every live session in the live tick.
- The `?` list marker and the header count, for both signals.
- The overlay: open by hand, pop on entering `waiting` and at startup, oldest-first queue, the keys above, snooze in memory carried through `uiState`.
- The overlay's keys in its own hint row, Ctrl+Y in `shortcuts()`, and AGENTS.md updated.

Not in it: tool-call parsing, picking menu options, and sharing snoozes between ccs windows.

## Built differently

What the first version did differently from the plan above:

- **Its own message box** (`waitInput`), so a draft in the main box is never touched.
- **Its own transcript cache** (`waitPreview`), so the list's preview behind it isn't rebuilt every frame.
- **Ctrl+Y closes without snoozing**, and ↑↓ move between sessions like Ctrl+P/N.
- **The fast 250ms tick follows the overlay's session** while it's open (`chatTarget`), not the list's selection.
- **A pop-up doesn't take the keyboard** until Ctrl+Y. An earlier version took it and guarded typing with a 3s quiet check and a 300ms key drop; both leaked keys (a paused search ended up in the overlay), so they're gone.
- **Ctrl+G no longer lists the overlay's keys**, since nothing opens over the overlay; its hint row lists all of them instead.
- **Enter on an empty main message box only focuses the session's tab** (`focusCmd`), as in the overlay. Before, it could resume the session when its file couldn't be read, starting a second claude.

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

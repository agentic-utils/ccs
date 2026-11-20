# Input-prompt overlay (removed)

Status: removed. ccs no longer has the ^X W overlay. This page says what it was, why it went, and where its jobs live now.

## What it was

A popup over the list (Ctrl+X W) that showed the live sessions waiting on you, one at a time with its transcript and a message box, and let you answer, open or snooze each. A banner, later a header count, pointed at it.

## Why it was removed

Every answer moved into the main view, and Bharat asked for that to be the only place ("all answering happens in the main interface"):

- With `ccs hook install`, ccs holds a session's permission prompt or question menu and answers it by its id, with a menu like Claude Code's own, in the bottom pane of the list (held.go, menu.go; docs/remote.md). ccs selects a session whose prompt it holds once you've stopped typing, so the question is put in front of you.
- A prompt ccs doesn't hold can only be answered in its tab, and ccs never types into a terminal any more, so the overlay had nothing left to answer.
- Two places to answer the same prompt, with different keys, was confusing.

## Where its jobs went

| The overlay did | Now |
| --- | --- |
| Count the waits | The header's `N waiting` (no key) |
| Show the next wait | Tab goes to waiting sessions (the list stays in start order); held prompts are selected for you (`autoSelect`, oldest first) |
| Show the question | The bottom pane, in place of the selected session's message box (`chatAsk`) |
| Answer | Held prompts in the bottom pane; others in their tab (Enter opens it) |
| Snooze a wait (Esc, `CCS_SNOOZE`) | `Ctrl+X S` puts a session away for a while; the overlay's own snoozes and `CCS_SNOOZE` are gone |
| A draft per session | The message box's drafts (`chatDrafts`) |

## One view of a question

A question reaches ccs twice, separately: Claude Code's session file says the session is waiting (read every 250ms for the selected session), and the hook's held copy is in the holder's list (asked for every 2s). Drawing whichever arrived meant a held question first showed from the transcript (a magenta `? question`, `askLines`) until its held copy came, and again after it was answered, until the status stopped saying waiting.

So the bottom pane shows only a dim "waiting on you…" status line for a waiting session within `heldGrace` (1s) of its held prompt leaving the list (answered, sent back, a failed ask, the holder gone) or, while something holds prompts, of its wait beginning (`heldPending`). A prompt answered from ccs stays hidden longer: for as long as the session still reports the same wait (the same statusUpdatedAt) it answered, since the status and transcript can lag the answer by more than the grace (`heldAnswered`, capped at `heldAnsweredMax`, 30s, after which a session still on that wait is taken to be waiting on something else). A timer alone would flash the transcript view again whenever that lag is long. A wait beginning also asks for the held list at once, then every `heldQuick` (250ms) until its copy comes or the grace passes, whatever is selected (`heldRetry`), so the held menu is usually there within a frame or two. The grace is judged by the time Update last finished (`paneNow`), so the pane's measured and drawn heights never disagree within a frame. A question nothing holds (no hook, an older hook, or one sent back to its tab) still shows from the transcript once the grace has passed, and at once when nothing holds prompts at all.

The invariants that still hold (statuses only through `setStat`, a session whose file is briefly unreadable keeps its state while its process runs, nothing pops up) are in AGENTS.md beside `waitSignal`.

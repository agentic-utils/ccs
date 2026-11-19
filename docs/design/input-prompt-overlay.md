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
| Show the next wait | Waiting sessions sort first in the list; held prompts are selected for you (`autoSelect`, oldest first) |
| Show the question | The bottom pane, in place of the selected session's message box (`chatAsk`) |
| Answer | Held prompts in the bottom pane; others in their tab (Enter opens it) |
| Snooze a wait (Esc, `CCS_SNOOZE`) | `Ctrl+X S` puts a session away for a while; the overlay's own snoozes and `CCS_SNOOZE` are gone |
| A draft per session | The message box's drafts (`chatDrafts`) |

The invariants that still hold (statuses only through `setStat`, a session whose file is briefly unreadable keeps its state while its process runs, nothing pops up) are in AGENTS.md beside `waitSignal`.

# Relay and handoff between sessions

Status: proposal, not built. Revised after an adversarial review (codex), which found that Claude Code already covers most of the original gap. Decide before any code.

## Problem

Claude sessions on this machine message each other through Claude Code's own sockets (`messagingSocketPath` in `~/.claude/sessions/<pid>.json`, the `SendMessage` tool). ccs sends the user's messages over them (`sendViaSocket`) and, since #172, shows both ends of a message. The question was whether ccs should go further, as t3code's server does: delegate tasks, hand off context, queue messages for closed sessions, and act while the TUI is closed.

## What Claude Code already does (2.1.314)

- **Background sessions:** `claude --bg "<task>"` starts a session in the background and prints its id; `claude attach <id>`, `logs`, `stop` and `rm` act on it; `claude agents --json` lists active sessions, interactive and background (`--all` adds completed ones). `--bg --resume <id>` continues an existing session in the background.
- **Background subagents** (the Agent tool, async) report back to their parent with a completion notification.
- **Agent teams** (experimental) give coordinated sessions with messaging.

So "one session starts another with a task and finds out how it went" is native, and isn't ccs's job to reimplement. What ccs uniquely has is the view across every session. The gaps left:

1. **Seeing and opening background sessions.** They have no terminal, so ccs can't switch to them (like T3 Code's sessions, #171), but unlike those they can be opened: `claude attach <id>` in a new tab.
2. **Handoff of context** between independently started sessions, with clear provenance.
3. **Messages to closed sessions**, and work while ccs is closed (snooze wake-ups, auto-focus). Unsolved by anything native.

## Option 0: native features; ccs observes (recommended first)

- Read `claude agents --json` (each session's `pid`, `sessionId`, `kind`: `interactive` or `background`, `status`, `name`) every few ticks and mark background sessions in the list (`bg`), with their state.
- Enter on a background session opens `claude attach <id>` in a new tmux window or iTerm tab, instead of "can't switch to it". `^X K` (kill) maps to `claude stop`.
- Nest a background session under the session whose Bash ran `claude --bg` (extend `isSpawn`/`findSpawnParent`, which already match `claude -p`).
- Nothing new runs; no new authority is granted to any session.

**Cost:** small. Depends on the `claude agents --json` shape, which ccs must parse defensively and treat as optional (absent on older Claude Code).

## Option A: a `handoff` MCP tool, no daemon

Only if handoff between independent sessions proves wanted. Delegation is left to `claude --bg`.

- `handoff(to, summary, refs?)`: sends to a **live** session over its socket. Refused for a closed one.
- **A distinct envelope**, never ccs's user wrapper: `ccsNote` says "typed by the user", and model-written text must never claim that. The envelope names the source session, carries an id, and marks its content as untrusted context from another agent.
- **Content:** the caller's own summary plus explicit message references (`read_session` already lets the recipient fetch more). No automatic last-N history: recency isn't relevance, and it may disclose what wasn't meant to go.
- **Safety:** off by default (a setting); the recipient must be live and in the same project unless the user allows otherwise; size-bounded; logged to the activity log.
- **Delivery is reported honestly:** socket write accepted, then `✓ arrived` once the text is in the recipient's transcript (#172). Claude Code sends receipts only when a message doesn't go straight in (held, denied, expired, dropped), so a write with no receipt is "sent", not "delivered", until the transcript shows it.

**Cost:** medium. No process lifecycle.

## Option B: a `ccs daemon` relay

A launchd agent (`brew services start ccs`) owning `~/.claude/ccs/relay.sock` and a state store; the TUI and `ccs mcp` become clients. Adds queued delivery to closed sessions and background work (snoozes, auto-focus) while the TUI is closed.

What it would need to be safe, from the review:

- **Delivery state per message:** an id, durable outcome (sent, arrived, refused, expired, unknown), expiry and cancellation, reconciliation after a daemon restart, deduplicated callbacks. A status re-check before delivery narrows races but can't remove them. Never deliver by typing.
- **Authority:** `0600` and a same-user check keep other users out but don't tell an authorised request from a compromised agent. Any action that starts or messages sessions must be opt-in, project-scoped, bounded in size and rate, and visible to the user, with a stop.
- **Cost limits** shared across processes: concurrency, depth (a child can't keep delegating), total tasks and time; bounded retries.
- **Storage:** its own versioned store with locked read-modify-write and unique temp files. Not `sessions.json`, which today is a whole-file snapshot written through one `.tmp` name and would lose concurrent updates (and older binaries drop unknown fields).
- **Upgrades:** after `brew upgrade` the old daemon, open TUIs and each session's `ccs mcp` keep running old code; the protocol needs versions and capability negotiation.

**Cost:** large, ongoing.

## Option C: the TUI as the relay

The running TUI queues and delivers while it's open; `ccs mcp` uses it when it's there. Rejected: it stops exactly when queueing matters (ccs closed), needs leader election between two open TUIs, and carries B's version skew anyway (old TUIs and MCP processes outlive upgrades).

## Comparison

| | 0: native, ccs observes | A: handoff tool | B: daemon | C: TUI relay |
| --- | --- | --- | --- | --- |
| Start a session with a task | `claude --bg` (native) | native | native or daemon | native |
| Open a background session | yes, `claude attach` | yes | yes | yes |
| Hand off context, live recipient | no | yes | yes | yes |
| Message a closed session | no | refused | queued | queued while open |
| Snoozes, auto-focus with ccs closed | no | no | yes | no |
| New authority for sessions | none | handoff only, opt-in | start and message, opt-in | as B |
| New moving parts | none | one MCP tool | daemon, store, protocol | server inside the TUI |
| Effort | small | medium | large | medium-large |

## Recommendation

1. **Build option 0 now.** It makes the native delegation visible and openable, which covers the real day-to-day need (sessions in places ccs couldn't reach) at no new risk.
2. **Option A only on demand**, with the envelope and safety rules above, never reusing the user wrapper.
3. **Option B only with evidence** that messages to closed sessions, or background work with ccs closed, are needed often. If it comes, it gets its own design covering the requirements listed under B; A's tool would become a client of it, so A should be written as a small service (stable ids, a structured result) rather than logic tied to the stdio server.

## Decided from the open questions

- A session with no terminal asking for delegation uses `claude --bg` (native), not a headless `claude -p`; ccs doesn't start sessions itself.
- Handoff carries a caller-written summary and explicit references, not automatic history.
- Worktree isolation for delegated work is Claude Code's choice (`--worktree` exists natively), not ccs's.

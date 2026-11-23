---
title: Using ccs remotely (ccs serve)
read_before: changing ccs serve, its checks, or anything it can do to a session
---

# Using ccs remotely: `ccs serve`

`ccs serve` lets you use ccs from a browser, such as your phone's. It shows the sessions grouped by what they need from you, lets you read any session and lists your PRs. With `--allow-write` it can also send a message to a session, and launch, close or restart sessions. It shows a waiting session's question but doesn't answer it: see [Answering from afar](#answering-from-afar).

It controls a machine that runs agents with your credentials, so it never trusts the network in front of it:

- **Loopback only.** It listens on `127.0.0.1` (or `::1`) and refuses any other address. A Cloudflare Tunnel connects out from the Mac, so no inbound port is opened.
- **Cloudflare Access in front, checked again by ccs.** Access lets only your identity through. ccs then checks the signed `Cf-Access-Jwt-Assertion` token on every request: RS256, signed by your team's key (from `https://<team>/cdn-cgi/access/certs`), for this application's AUD tag, issued by your team, not expired, and for an email you allowed. A tunnel left without an Access policy, or a program or web page on the Mac reaching the port directly, gets a 403. ccs won't start without `--host`, `--team`, `--aud` and `--email`.
- **Host check.** Requests for any hostname but `--host` are refused (421), which stops DNS rebinding.
- **Read-only unless you say otherwise.** Without `--allow-write` there are no write routes and no forms.
- **Writes are deliberate.** Every write is a POST from the same origin (`Origin` and `Sec-Fetch-Site` checked), with a form token tied to your email that is valid until ccs serve restarts. Each one first shows a page saying exactly what it will do, and only acts once you confirm.
- **Limits.** Per email: reads in a burst of 60, then one a second; confirmed writes in a burst of 5, then one every 12 seconds. Bodies are capped at 64KB and messages at 8000 characters. At most 4 requests are served at once (others wait up to 15 seconds, then get a 503), and requests that arrive while the transcripts are being read share that one read. A request whose client goes away stops waiting and frees its place; the read itself finishes for the next request, since the parser can't be cancelled.
- **Audit log.** Every request, refused or not, goes to the activity log (`~/Library/Logs/ccs/activity.log`) with the email, path and outcome. Writes also record what was done and how it went.
- **No scripts.** The pages are plain HTML with a strict Content-Security-Policy (`default-src 'none'`), every value escaped, and `Cache-Control: no-store`.

What each write does:

| Write | How | Never |
| --- | --- | --- |
| Send a message | the session's own message socket, as the TUI's message box does (`the user, via ccs`), or Codex's queue | types into a terminal: from afar you can't see what's on the screen it would land in. Nor does it run commands: a line starting `!` or `/`, which the TUI's box runs here or types into the session, is sent from the web page as plain text, and a command's result is sent to the session only from the TUI (`Ctrl+S` on its output box) |
| Close, restart | with the session's id. Every stop (here, `ccs close`, `ccs restart`, ^X K and the tools) checks which session the pid runs immediately before the signal: its session file, or for Codex a fresh look at its live thread | stops a session that has taken the name since, a conversation that replaced it in the same process, or one whose session file can't be read at that moment |
| Launch, close, restart | the `launch_session`, `close_session` and `restart_session` MCP tools; tabs open on the Mac | anything those tools refuse |

The signing keys are fetched again every hour (and at once for a key id not seen yet, at most every 30 seconds), so a key Cloudflare retires stops working here too. If the fetch fails, keys up to 6 hours old are still used, then every request is refused. That's deliberate: a Cloudflare outage of a few hours shouldn't lock you out, and a retired key stops working at the next successful fetch, within the hour.

Every launch path (`ccs serve`, the `launch_session` tool, `ccs launch`) passes a brief that starts with `-` with a newline in front, so claude or codex reads it as the prompt, never as an option such as `--dangerously-skip-permissions`.

## The page

It's built for a phone and has no scripts at all.

- **Sessions page:** a bar shows how many sessions wait on you. Prompts held for you come first, then the sessions grouped by what they need, each as a card with its status, how long it's been, why, the last thing said, and its PRs.
- **Session page:** any held prompt sits on top, with its options as big rows (or Allow and Deny); the same menu is in its tab too, and the first answer wins. Below it the conversation shows as message bubbles, newest at the bottom: long ones folded, code in its own scrolling block. A message box is pinned to the bottom, and Restart and Close sit under "More actions".
- **Refreshing:** with no scripts, the sessions page and a waiting session's page reload every 10 seconds. A session that isn't waiting never reloads, so a message being typed isn't lost; while it waits, its message box is hidden.
- **Home screen:** it can be added to a phone's home screen, with its own name, icon and full-screen display.

## Answering from afar

`ccs serve` never types into a terminal to answer anything: whatever is on screen by the time the key lands (a permission prompt, say) would get it. It answers through Claude Code's own PermissionRequest hook instead, which Claude Code runs for each permission prompt and AskUserQuestion menu before showing it, and which can settle it with a decision.

Turn it on once with `ccs hook install`. It adds `ccs hook permission` under `hooks.PermissionRequest` in `~/.claude/settings.json`, keeping the old file beside it as `settings.json.before-ccs-hook`. A symlinked `settings.json` stays a link: the file it points to is edited. `ccs hook install --print` shows the entry without writing it, and `ccs hook uninstall` takes it out. Sessions started from then on use it; restart running ones to pick it up.

What happens to each prompt:

- **No `ccs serve --allow-write` running:** the hook finds nobody listening on ccs's socket (`serve-hook.sock` beside the settings, mode 0600) and prints nothing, so Claude Code shows the prompt in its tab as usual, at once.
- **`ccs serve --allow-write` running:** the prompt is held for the page as well. Claude Code still shows the same menu in the session's own tab while the hook holds it, so either can answer, and the first answer wins. The page lists it under "Waiting for you here", on the sessions page and the session's own page. A question offers its options (checkboxes for "pick any"). A permission prompt shows the command and its full input, with Allow and Deny. Each choice shows a confirm page first.
- **Answered in its tab:** Claude Code doesn't stop the hook then, so ccs watches the session's status (every 2 seconds) and drops its copy, with no decision (the hook prints nothing), once the session isn't waiting on that prompt any more.
  - A session asks one prompt at a time, even for tool calls made together. Each prompt gets its own wait: a `waiting` status with a newer `statusUpdatedAt`, written within about a second of its hook starting. This was measured on Claude Code 2.1.328.
  - So each copy is bound to its own wait: the `statusUpdatedAt` the hook read from its session file when it started (sent with the prompt), or, when the hook couldn't read it, the first wait stamped at or after the prompt arrived that no earlier copy of the same session was bound to. It's dropped when the status leaves `waiting` (busy or idle), or when a newer wait starts, meaning the next prompt.
  - An older wait still on show is the session file not having caught up yet, and drops nothing.
  - A session missing from a scan, because its file is unreadable or caught mid-rewrite, counts as unknown and keeps the copy. Only its claude process having exited ends it, or its pid now belonging to another process (a different start time).
  - An answer sent from ccs or the page is checked against the session's status just before it goes, and again right after it's taken to be sent. It's refused with "answered in its tab already" if the tab got there first.
  - **Known limit:** a tab answer landing in the few milliseconds between that last check and the reply can't be caught. Then both answers go out, but ccs's goes only to that prompt's own hook, which Claude Code has already settled, so it can't land on any other prompt.
- **Restarts keep held questions.** When ccs quits, restarts (an update), or hands the hold to ccs serve, it tells each held hook to retry rather than settling the prompt. The hook then dials again once a second, with every check below made afresh (the socket, the listener's user, a new nonce, the acknowledgement), and the next ccs to listen holds the prompt again. If ccs crashes, the hook's connection closes with no reply, and the hook dials again in the same way. The hook stops dialling, printing nothing, as soon as its session file shows its wait over: answered in the tab, another wait, or another session. It also stops 30 minutes after it started. A copy whose wait is over by the time it comes back is let go at once and never listed. The activity log says "handed back for a restart" for these, and "let go to its tab" for a prompt settled for good.
- **An automatic update doesn't wait for held prompts,** since they come back after the restart; waiting for none to be held could put an update off for hours. It waits only while a held question is half answered (an answer picked, a note or your own text typed, a box open, or past its first question), since that progress would be lost, or while a `!` command runs. The same is checked just before the restart of an install that finished meanwhile. The header says "update waiting: a question half answered" (or "a ! command running") while it waits, and the activity log notes each restart that hands questions back. Choosing "update and restart" yourself restarts at once. The offer, and a failed install's retry, says "N held question(s) come back once ccs restarts".
- **The answer goes back on that prompt's own hook connection,** so it settles that prompt and no other. A prompt answered, let go or ended meanwhile is gone, and a late answer sends nothing. Allow (with a question's answers in `updatedInput`) or Deny is only ever sent when you choose it on the page.
- **ccs stops offering a held prompt, with no decision, when:**
  - a key is pressed on the Mac after it arrived, which means you're back there (checked every 3 seconds; not in `--preview`, where you're at the Mac by design);
  - `--hook-wait` passes (30 minutes by default, and at most that);
  - the prompt's own wait ends while its holder is away (see "Restarts keep held questions").

  Nothing ever denies a prompt on its own.

**Who the hook trusts.**
- **Other macOS users are kept out.** The hook only talks to a socket that is a real socket (not a symlink), owned by you, usable by you alone (0600), in a folder that is yours and that no one else can write.
- **The kernel vouches for both ends.** After connecting, the hook asks the kernel which user is listening (`LOCAL_PEERCRED` on macOS, `SO_PEERCRED` on Linux) and refuses anyone but you, so a socket swapped between the check and the connect gets nothing. ccs serve checks the hook's user the same way.
- **The answer is tied to that prompt.** ccs serve must acknowledge the prompt within 2 seconds, echoing a random nonce the hook sent. Its answer must carry the same nonce, so a reply meant for another prompt, or replayed, settles nothing.
- **ccs serve can't change what runs.** It can say allow or deny, and nothing else. For a question it can also send the chosen labels, which the hook checks against the question's own options. The hook then builds what it gives Claude Code from its own copy of the tool input, so neither ccs serve nor anything posing as it can change a command. Any failed check lets the prompt show in its tab.
- **A stalled listener only delays the prompt.** One that acknowledges and then goes quiet holds it until `--hook-wait` passes (at most 30 minutes, then it shows in its tab). For the real ccs serve, a key press on the Mac hands it back sooner.
- **The TUI talks to ccs serve the same way.** Its control socket (`serve-control.sock`) gets the same checks on both ends, and it carries only a choice and labels. A sent answer whose reply never comes back is reported as "outcome unknown", never as failed, because ccs serve may already have settled it.
- **Programs running as you are out of scope.** Such a program could edit `~/.claude/settings.json`, remove the hook, or run claude itself, so a shared key file or any other check here would not stop it. That's why there's no signing key.

**Who holds a prompt.** Whoever owns the hold socket, as an flock on `serve-hook.lock` beside it:
- **`ccs serve --allow-write`, whenever it runs.** It takes the hold over from a ccs TUI that has it (the TUI's held prompts go to their tabs, undecided), and a second ccs serve is refused.
- **Otherwise, the first open ccs TUI.** Other TUIs answer through it.
  - It holds every prompt, whatever the focus, until it's answered. Questions arrive while you're in another window, and a prompt let go to its tab can't be answered from ccs any more (the hook has returned). Nothing in the header says so: a held question shows in its session's pane.
  - ccs selects the session holding it and focuses its answer pane, whether or not ^X A is on, so the question is in front of you when you come back: the oldest first, once no key has been pressed in ccs for 2 seconds, never over unsent text, a typed search or an open popup, and with the typing pause after the move. It stays on that prompt until it's answered or sent back, then moves to the next. It never switches terminal tabs for a held prompt (its tab shows no menu); ^X A's tab switching is for prompts it doesn't hold.
  - A held prompt goes to its tab only when you send it back (T), after 30 minutes, when ccs quits, or when ccs serve takes the hold over; none of those decides anything. An answer you've confirmed with y goes through even if one of those happens a moment later.
  - When ccs serve stops, a TUI takes the hold within a couple of seconds.
- **Nothing running:** with no ccs serve and no ccs TUI, the hook prints nothing and the prompt shows in its tab at once.

Sessions started before `ccs hook install` don't run the hook: restart them (^X R) to pick it up.

**Answering from the ccs TUI.** A held prompt can be answered in ccs on this Mac. Its question shows in the bottom pane of the main view, inside the message box's own frame (grown to fit, but never more than two thirds of the pane, so the transcript keeps a third; a taller menu scrolls inside it), laid out like Claude Code's own menu (one header line with the question's header, "1 of 3" and a dim "also in its tab", since Claude Code shows the menu in the session's tab too, and the first answer wins; the question; the numbered options with "❯" on the highlighted one and each description dim and indented under it, two lines at most; "Other…"; one hint row with only the keys that work at that step), and its menu takes keys as soon as the session is selected with the search empty, with no Enter first (the pane says "keys: menu"). It takes keys only while it's drawn: never behind the usage screen, help or a popup, and never while the session's status says it isn't waiting on it. Typing a search takes the keys from it for good: clearing the search doesn't give them back; selecting the session again or → does. Tab is never the menu's: it goes to the next session that needs you, by priority (a held question, or a menu or prompt in its tab; then a session that asked you something; then one done that you haven't read), wrapping round within each, and with none says "nothing needs you" (Tab never opens the usage screen; ^X Y does). ccs also selects a session holding a prompt once you've stopped typing, clicking or scrolling in ccs for 2 seconds (never two in a row any faster, and never away from a session that is itself waiting on you or holds a draft, so a question you're reading stays put), dropping keys for its first second or while keys still come under 600ms apart, so typing meant for the search can't answer it. Once you've answered it, that session's message box keeps the keys, unless it asks again straight away. There:

- a question works like Claude Code's own menu, one question at a time: ↑/↓ (j/k) move over its options, each with its description, and a last "Other…" row; a digit highlights an option, and Enter sends; a pick-any question ticks with Space (or a digit) and Enter confirms, refusing an empty set; "Other…" opens a box whose text is your answer; `n` opens a note on the question's answer (both wrap inside the frame, up to four lines that follow the cursor; Enter saves, and the text never gets a newline) (prefilled when you edit it; an empty note removes it), shown dimmed under the question; ← goes back a question, keeping your answers. One question shows what Enter will send, note included, and is sent then; several end on a review, "Review · N answers", that lists each question numbered and dim with its answer bold after → (your own text marked "(Other)", pick-any answers joined with ", ") and its note dim under it, all in full, where Enter submits and Esc goes back; a review taller than the pane scrolls (↑/↓, j/k, PgUp/PgDn, "↓ more" while there's more) and won't send until its end has been on screen;
- a question that has just appeared, or is shown again after being covered or switched away from, ignores Enter (an open box's included), digits and Space for half a second (the arrows work at once), so a key meant for something else can't answer it unread; after ccs brings a prompt up by itself, keys still being typed are dropped too;
- A allows and D denies a permission prompt, each asking y/n first;
- there's no "send it back to its tab" key any more (it was T): the menu is in its tab all along. For a permission prompt, Enter opens its tab. Esc goes back to the list from any step and keeps your answers (← goes back a question): ↑/↓ then move the list, and → (or selecting it again) returns to the menu. ^P/^N always move the list, and a letter the menu doesn't use types in the search.

ccs never types an answer into a terminal: a menu or prompt it doesn't hold is answered in its own tab. The TUI talks to ccs serve over a second socket, `serve-control.sock`, beside the hook's. It's guarded the same way on both ends: this user's private 0600 socket, and the kernel's word that the other end runs as this user. An answer carries only a choice and a question's labels, never tool input. The first answer wins, from the TUI, the page or the session's own tab, and the activity log names the TUI as `tui (this Mac)`. Outside `--preview`, a key press on the Mac sends held prompts back to their tabs within seconds, so this matters most in a preview and in those first seconds.

Checked on Claude Code 2.1.327 in a throwaway session: an allow approved a Bash prompt, and an allow with `updatedInput.answers` answered an AskUserQuestion menu without it being shown. The session got the chosen answer. Answers to a multi-select are the chosen labels joined with ", ". An "Other" answer, from the TUI's box or the page's Other field, is sent flagged as such; the hook takes free text for a question only when it's flagged, strips control characters, trims it, refuses it empty and caps it at 2000 characters, and still builds the input from its own copy, adding only the answers and annotations. A note, from the TUI's `n` or the page's Note field, goes in `updatedInput.annotations` as `{"<question text>": {"notes": "<text>"}}`, the shape Claude Code 2.1.328's AskUserQuestion schema gives it ("Free-text notes the user added to their selection"); the hook takes notes only for its own questions and cleans and caps them as it does Other text, dropping empty ones.

## Running it in the background

`ccs serve install <the same flags as ccs serve>` writes a per-user launchd agent (`~/Library/LaunchAgents/io.github.agentic-utils.ccs-serve.plist`, 0600 since it holds the Access values), checking the flags as `ccs serve` does. Its output goes to `serve.log` beside the update log, which ccs creates 0600 (and its folder 0700 if ccs makes it) at install and at start, before launchd first writes it, tightening an existing one. `install` refuses `--preview`: the preview is for a terminal on this Mac, and its secret link would only reach that log. Then:

- `ccs serve start` runs it and `ccs serve stop` stops it; `ccs serve status` says whether it runs, from `launchctl print`; `ccs serve uninstall` stops it and removes the plist.
- In the TUI, ^X V then w sets it up when there's no agent yet (below), starts it when it's installed, and while it runs shows its address and a QR code (S there asks y/n and stops it, E edits its setup and restarts it). Once installed, the header shows `^X V W web on` or `off`, read from launchd's state at most every 10 seconds, not just whether the plist exists.
- It never starts at login (`RunAtLoad` false). launchd restarts it only after a crash (`KeepAlive` `{SuccessfulExit: false}`), and stop unloads it (`launchctl bootout`), so a stopped agent stays stopped.
- It isn't hosted inside the TUI, which restarts on every auto-update and stops when it quits: remote access would drop with it.
- Changing its flags: `ccs serve install <new flags>`, then stop and start it.

## Opening it on your phone

`ccs serve` prints a QR code of `https://<--host>/` when it starts, `ccs serve qr` prints it again from the installed agent's plist, and ^X V then w shows it in the TUI while the agent runs. Point the phone's camera at it, and it opens the page through Cloudflare Access. The code is drawn in black on white whatever the terminal's colours, so it scans on a dark or a light theme; a window too small for it shows the address alone. A `--preview` never gets one: its link is `http://127.0.0.1:...`, which on a phone means the phone itself.

The tunnel is its own service: `brew services start cloudflared`.

## Previewing it on this Mac

`ccs serve --preview` serves the same pages on this Mac with no Cloudflare Access, to try them before setting the tunnel up.

It opens its link without putting the token on a command line (where `ps` would show it): it writes a page that redirects to the link into a private folder (0700, the page 0600), opens that page, and removes it 30 seconds later.

Loopback is shared by every user on a Mac, so the preview has a login of its own. Each start makes a random secret, and every page and form lives under `http://127.0.0.1:7467/p/<secret>/`. ccs prints that link and opens it in your browser. A request without the secret, or with a wrong one, gets a 404 and nothing else, and the secret is never written to the activity log. The secret is kept in the path, not a cookie, because a browser sends a 127.0.0.1 cookie to every port, so another user's service on another port would receive it. The pages' `Referrer-Policy: same-origin` keeps the path from being sent to other origins too.

The preview refuses the Access flags, answers only requests whose Host is `127.0.0.1`, `localhost` or `::1` (which stops DNS rebinding), and is read-only unless you add `--allow-write`. With it, writes work as they do remotely: same-origin only (`Origin` and `Sec-Fetch-Site`), with the form token, the confirm page, the limits and the activity log, so another website can't post to it. Programs running as you can read the printed link, as they can already run ccs, so stop the preview with ^C when you're done.

## Setting it up

The quickest way is ^X V then w in ccs. With no agent installed it opens a setup popup:

- **Try it on this Mac now** runs a read-only `ccs serve --preview` as a child of ccs, for as long as ccs runs (it stops when ccs quits, even during its startup; it's started with `--parent-pid`, so if ccs is killed outright it notices within 2 seconds and stops too; and it's never a launchd agent), and opens its secret link. It's for this Mac only: your phone can't reach 127.0.0.1. Pressing it again while it runs shows the link again.
- **Set up for my phone** is a form: the hostname, the Access team, the Access AUD tag and the email (prefilled from your Claude account), each with a hint on where to find it, and "Allow answering and sending from the phone" (`--allow-write`), unticked unless you tick it. Tab or ↑/↓ move, Space ticks, Enter on the last field submits. It's checked exactly as `ccs serve install` checks its flags, with errors shown in the form: the hostname is normalised to what the browser sends (lowercase, an internationalised name in its punycode form, a trailing dot dropped), and each email must be a plain address as Go's net/mail reads it. Pasting (Ctrl+V) goes into the field you're in, never a box hidden beneath the form. Before installing, it looks for `cloudflared` on PATH and an ingress rule for the hostname in `~/.cloudflared/config.yml` (or `config.yaml`) pointing at `http://127.0.0.1:7467`; if either is missing it shows the commands below, and Enter installs anyway or Esc goes back. Then it installs and starts the agent and shows its address, QR code and status.

By hand, the steps are:

1. Install cloudflared: `brew install cloudflared`, then `cloudflared tunnel login`.
2. Create a tunnel and a hostname for it:
   ```
   cloudflared tunnel create ccs
   cloudflared tunnel route dns ccs ccs.example.com
   ```
   with `~/.cloudflared/config.yml`:
   ```
   tunnel: ccs
   credentials-file: /Users/you/.cloudflared/<tunnel-id>.json
   ingress:
     - hostname: ccs.example.com
       service: http://127.0.0.1:7467
     - service: http_status:404
   ```
3. In Cloudflare Zero Trust, add a self-hosted Access application for `ccs.example.com`, with a policy that allows only your email (through Google or a one-time PIN). Set a short session duration. Copy the application's **AUD tag**, and note your team domain (`<team>.cloudflareaccess.com`).
4. Start ccs serve, read-only first:
   ```
   ccs serve --host ccs.example.com --team <team> --aud <AUD tag> --email you@example.com
   ```
   then `cloudflared tunnel run ccs`. Open `https://ccs.example.com` on your phone. Add `--allow-write` once you're happy with it.
5. To keep both running, run each as a launchd agent (`brew services start cloudflared` covers the tunnel) or in a tmux window.

## When the Mac sleeps

The tunnel goes down with the Mac, and Cloudflare shows its own error page until the Mac wakes. Nothing is queued. To stay reachable while away, keep the Mac awake on mains power, for example by running ccs serve under `caffeinate -s`.

## Not built, and why

- **A remote MCP server for the Claude apps.** claude.ai connectors call from Anthropic's cloud and need OAuth rather than an SSO login. A model reading untrusted transcript text would also hold the write tools.
- **Claude Code's Remote Control** as the way in. It drives one session at a time, and has to be turned on per session. ccs serve covers the view across every session and does its own writes. The two can be used side by side.

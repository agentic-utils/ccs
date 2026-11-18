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
| Send a message | the session's own message socket, as the TUI's message box does (`the user, via ccs`), or Codex's queue | types into a terminal: from afar you can't see what's on the screen it would land in |
| Close, restart | with the session's id. Every stop (here, `ccs close`, `ccs restart`, ^X K and the tools) checks which session the pid runs immediately before the signal: its session file, or for Codex a fresh look at its live thread | stops a session that has taken the name since, a conversation that replaced it in the same process, or one whose session file can't be read at that moment |
| Launch, close, restart | the `launch_session`, `close_session` and `restart_session` MCP tools; tabs open on the Mac | anything those tools refuse |

The signing keys are fetched again every hour (and at once for a key id not seen yet, at most every 30 seconds), so a key Cloudflare retires stops working here too. If the fetch fails, keys up to 6 hours old are still used, then every request is refused. That's deliberate: a Cloudflare outage of a few hours shouldn't lock you out, and a retired key stops working at the next successful fetch, within the hour.

Every launch path (`ccs serve`, the `launch_session` tool, `ccs launch`) passes a brief that starts with `-` with a newline in front, so claude or codex reads it as the prompt, never as an option such as `--dangerously-skip-permissions`.

## Answering from afar

ccs serve doesn't answer questions or permission prompts. The only way it could today is by typing the option's digit into the session's terminal, and whatever is on screen by the time the key lands (a permission prompt, say) would get it; no check can close that gap. Claude Code 2.1.327 has no way to answer a prompt by its id over the session socket (it takes only messages and renames), `--permission-prompt-tool` works only with `claude -p`, and `claude attach` has no answer command. The planned route is a PermissionRequest hook, which Claude Code itself names for answering prompts from a script: it would hand each prompt to ccs serve and answer it by its request id.

## Running it in the background

`ccs serve install <the same flags as ccs serve>` writes a per-user launchd agent (`~/Library/LaunchAgents/io.github.agentic-utils.ccs-serve.plist`, 0600 since it holds the Access values), checking the flags as `ccs serve` does. Its output goes to `serve.log` beside the update log. Then:

- `ccs serve start` runs it and `ccs serve stop` stops it; `ccs serve status` says whether it runs, from `launchctl print`; `ccs serve uninstall` stops it and removes the plist.
- In the TUI, ^X B starts it; while it runs, ^X B shows its address and a QR code (S there asks y/n and stops it). Once installed, the header shows `^X B browser on` or `off`, read from launchd's state at most every 10 seconds, not just whether the plist exists.
- It never starts at login (`RunAtLoad` false). launchd restarts it only after a crash (`KeepAlive` `{SuccessfulExit: false}`), and stop unloads it (`launchctl bootout`), so a stopped agent stays stopped.
- It isn't hosted inside the TUI, which restarts on every auto-update and stops when it quits: remote access would drop with it.
- Changing its flags: `ccs serve install <new flags>`, then stop and start it.

## Opening it on your phone

`ccs serve` prints a QR code of `https://<--host>/` when it starts, `ccs serve qr` prints it again from the installed agent's plist, and ^X B shows it in the TUI while the agent runs. Point the phone's camera at it, and it opens the page through Cloudflare Access. The code is drawn in black on white whatever the terminal's colours, so it scans on a dark or a light theme; a window too small for it shows the address alone. A `--preview` never gets one: its link is `http://127.0.0.1:...`, which on a phone means the phone itself.

The tunnel is its own service: `brew services start cloudflared`.

## Previewing it on this Mac

`ccs serve --preview` serves the same pages at `http://127.0.0.1:7467` with no Cloudflare Access, to try them before setting the tunnel up. It refuses the Access flags, answers only requests whose Host is `127.0.0.1`, `localhost` or `::1` (which stops DNS rebinding), and is read-only unless you add `--allow-write`. With it, writes work as they do remotely: same-origin only (`Origin` and `Sec-Fetch-Site`), with the form token, the confirm page, the limits and the activity log, so another website can't post to it. Anything on this Mac can use it while it runs, as it can already run ccs itself, so stop it with ^C when you're done.

## Setting it up

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

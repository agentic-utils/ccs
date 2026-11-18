---
title: Using ccs remotely (ccs serve)
read_before: changing ccs serve, its checks, or anything it can do to a session
---

# Using ccs remotely: `ccs serve`

`ccs serve` lets you use ccs from a browser, such as your phone's. It shows the sessions grouped by what they need from you, lets you read any session and lists your PRs. With `--allow-write` it can also send a message to a session, answer its question, and launch, close or restart sessions.

It controls a machine that runs agents with your credentials, so it never trusts the network in front of it:

- **Loopback only.** It listens on `127.0.0.1` (or `::1`) and refuses any other address. A Cloudflare Tunnel connects out from the Mac, so no inbound port is opened.
- **Cloudflare Access in front, checked again by ccs.** Access lets only your identity through. ccs then checks the signed `Cf-Access-Jwt-Assertion` token on every request: RS256, signed by your team's key (from `https://<team>/cdn-cgi/access/certs`), for this application's AUD tag, issued by your team, not expired, and for an email you allowed. A tunnel left without an Access policy, or a program or web page on the Mac reaching the port directly, gets a 403. ccs won't start without `--host`, `--team`, `--aud` and `--email`.
- **Host check.** Requests for any hostname but `--host` are refused (421), which stops DNS rebinding.
- **Read-only unless you say otherwise.** Without `--allow-write` there are no write routes and no forms.
- **Writes are deliberate.** Every write is a POST from the same origin (`Origin` and `Sec-Fetch-Site` checked), with a form token tied to your email that is valid until ccs serve restarts. Each one first shows a page saying exactly what it will do, and only acts once you confirm.
- **Limits.** Per email: reads in a burst of 60, then one a second; confirmed writes in a burst of 5, then one every 12 seconds. Bodies are capped at 64KB and messages at 8000 characters.
- **Audit log.** Every request, refused or not, goes to the activity log (`~/Library/Logs/ccs/activity.log`) with the email, path and outcome. Writes also record what was done and how it went.
- **No scripts.** The pages are plain HTML with a strict Content-Security-Policy (`default-src 'none'`), every value escaped, and `Cache-Control: no-store`.

What each write does:

| Write | How | Never |
| --- | --- | --- |
| Send a message | the session's own message socket, as the TUI's message box does (`the user, via ccs`), or Codex's queue | types into a terminal: from afar you can't see what's on the screen it would land in |
| Answer a question | presses the option's digit in its tmux pane (by its pane id) or iTerm tab, as the TUI's overlay does, one answer at a time and each question once, only while the transcript still has the same question pending and the session file still says waiting, both read just before the keypress | offers to answer a permission prompt, a multi-select or a multi-question menu |
| Close, restart | only while the name still belongs to exactly the session the page showed | |
| Launch, close, restart | the `launch_session`, `close_session` and `restart_session` MCP tools; tabs open on the Mac | anything those tools refuse |

Answering has one gap no check can close: the digit is pressed by typing into the terminal, so if the same question is answered in its tab in the few milliseconds between ccs's last check and the keypress, and a permission prompt comes up in that time, the digit lands on the prompt. Answering a question both remotely and at the Mac at the same moment is the only way to hit it.

The signing keys are fetched again every hour (and at once for a key id not seen yet, at most every 30 seconds), so a key Cloudflare retires stops working here too. If the fetch fails, keys up to 6 hours old are still used, then every request is refused.

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

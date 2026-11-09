#!/usr/bin/env python3
"""Record the README demo: fake conversations in a throwaway HOME, ccs driven
by scripted keys, written as an asciicast (demo/demo.cast). Live sessions are
idle `sleep` processes with session files pointing at them, and PRs come from
demo/fake_gh.py, so nothing real is read or shown. Render the GIF with
  agg --theme monokai --font-size 14 --idle-time-limit 3 demo/demo.cast demo.gif
Run from the repo root: python3 demo/record.py"""
import codecs, json, os, pty, select, shutil, stat, struct, subprocess, sys, tempfile, time, fcntl, termios, uuid
from datetime import datetime, timedelta, timezone

COLS, ROWS = 140, 40
NOW = datetime.now(timezone.utc)
iso = lambda d: d.strftime("%Y-%m-%dT%H:%M:%S.000Z")

def usage(ctx, out=400):
    return {"input_tokens": 6, "cache_read_input_tokens": int(ctx * 0.9), "cache_creation_input_tokens": int(ctx * 0.1),
            "output_tokens": out, "cache_creation": {"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": int(ctx * 0.1)}}

def write_conv(home, project, turns, title=None, custom=False, model="claude-opus-5-5", ctx=80_000, ago=timedelta(hours=1),
               prs=(), ask=None):
    """turns are (role, text); prs are PR addresses the session touched; ask is
    a pending AskUserQuestion's input, left unanswered."""
    sid = str(uuid.uuid4())
    d = os.path.join(home, ".claude", "projects", project.replace("/", "-").replace(" ", "-"))
    os.makedirs(d, exist_ok=True)
    t = NOW - ago - timedelta(minutes=3 * len(turns))
    lines = []
    for i, (role, text) in enumerate(turns):
        t += timedelta(minutes=3)
        if role == "user":
            lines.append({"type": "user", "cwd": project, "sessionId": sid, "entrypoint": "cli", "timestamp": iso(t),
                          "message": {"role": "user", "content": text}})
        else:
            c = int(ctx * (i + 1) / len(turns))
            content = [{"type": "text", "text": text}]
            if ask and i == len(turns) - 1:
                content.append({"type": "tool_use", "id": "toolu_" + uuid.uuid4().hex[:12], "name": "AskUserQuestion", "input": ask})
            lines.append({"type": "assistant", "sessionId": sid, "timestamp": iso(t),
                          "message": {"id": f"msg_{uuid.uuid4().hex[:12]}", "role": "assistant", "model": model,
                                      "content": content, "usage": usage(c)}})
    for u in prs:
        lines.append({"type": "pr-link", "prUrl": u, "sessionId": sid, "timestamp": iso(t)})
    if title:
        lines.append({"type": "custom-title" if custom else "ai-title", "customTitle" if custom else "aiTitle": title, "sessionId": sid})
    p = os.path.join(d, sid + ".jsonl")
    with open(p, "w") as f:
        f.write("\n".join(json.dumps(l) for l in lines) + "\n")
    mt = (NOW - ago).timestamp()
    os.utime(p, (mt, mt))
    return sid

def fixtures(home):
    """Writes the transcripts and returns the live ones as (session id, name, status)."""
    cfg = os.path.join(home, "Library", "Application Support", "ccs")
    os.makedirs(os.path.join(home, ".claude", "sessions"), exist_ok=True)
    os.makedirs(cfg, exist_ok=True)
    for p in (os.path.join(home, ".claude.json"), os.path.join(home, ".claude", ".claude.json")):
        with open(p, "w") as f:
            json.dump({"oauthAccount": {"emailAddress": "you@example.com"}}, f)
    with open(os.path.join(cfg, "settings.json"), "w") as f:
        json.dump({"auto_update": False}, f)
    gh = "https://github.com/example/"
    live = []
    coord = write_conv(home, os.path.join(cfg, "coordinator"), [
        ("user", "fix the flaky checkout test, sort out last night's staging deploy, and make settings work on mobile"),
        ("assistant", "Started a session for each:\n\n"
                      "| session | PR | state |\n|---|---|---|\n"
                      "| ccs://session/fix-flaky-checkout | payments-api#812 | ready to merge |\n"
                      "| ccs://session/staging-deploy-timeout | infra#97 | checks running |\n"
                      "| ccs://session/mobile-settings | web#41 | lint failing |\n\n"
                      "**Needs you:** mobile-settings is asking which breakpoint to use. Merge order: infra#97 before payments-api#812."),
    ], title="coordinator", custom=True, ctx=64_000, ago=timedelta(minutes=1))
    live.append((coord, "coordinator", "idle"))
    with open(os.path.join(cfg, "coordinator.json"), "w") as f:
        json.dump({"session_id": coord, "offered": True}, f)
    live.append((write_conv(home, "/Users/you/code/web", [
        ("user", "make the settings page work on mobile"),
        ("assistant", "The settings form uses a fixed 960px grid. I can switch it to one column on small screens, "
                      "but the save bar placement depends on where the break is."),
    ], title="mobile-settings", custom=True, model="claude-sonnet-5-5", ctx=41_000, ago=timedelta(minutes=2), prs=[gh + "web/pull/41"],
        ask={"questions": [{"question": "Where should the settings page switch to one column?", "header": "Breakpoint", "multiSelect": False,
                            "options": [{"label": "640px (Recommended)", "description": "Phones only; tablets keep two columns"},
                                        {"label": "768px", "description": "Phones and small tablets"},
                                        {"label": "1024px", "description": "Every tablet, landscape included"}]}]}),
        "mobile-settings", "waiting"))
    live.append((write_conv(home, "/Users/you/code/payments-api", [
        ("user", "the checkout test is flaky on CI, can you find out why?"),
        ("assistant", "Found it. `test_checkout_total` depends on the **order of a dict** built from a set, so it passes or fails at random.\n\n"
                      "| run | result | order |\n|---|---|---|\n| #4120 | pass | a, b |\n| #4121 | fail | b, a |\n| #4122 | pass | a, b |\n\n"
                      "Fix: sort the line items before summing. See [the failing run](https://github.com/example/payments-api/actions/runs/4121)."),
        ("user", "nice, fix it and open a PR"),
        ("assistant", "Done:\n\n- sorted line items in `checkout.total()`\n- added a regression test that shuffles the input 100 times\n\n"
                      "```python\ndef total(items):\n    return sum(i.price for i in sorted(items, key=lambda i: i.sku))\n```\n\n"
                      "PR: https://github.com/example/payments-api/pull/812"),
        ("user", "<task-notification>\n<summary>CI on #812 passed</summary>\n</task-notification>"),
        ("assistant", "CI is green on #812, so the flaky test is fixed and the PR is ready to merge."),
    ], title="fix-flaky-checkout", custom=True, ctx=212_000, ago=timedelta(minutes=4),
        prs=[gh + "payments-api/pull/808", gh + "payments-api/pull/812"]), "fix-flaky-checkout", "idle"))
    live.append((write_conv(home, "/Users/you/code/infra", [
        ("user", "why did the staging deploy fail last night?"),
        ("assistant", "The deploy timed out waiting for the database migration. The migration took a lock on `orders` while a long report query held it.\n\n"
                      "**Suggested fix:** run the migration with `lock_timeout = 5s` and retry, instead of waiting forever."),
        ("user", "ok add the lock timeout and open a PR"),
        ("assistant", "Opened https://github.com/example/infra/pull/97 with the lock timeout and a retry with backoff. Waiting for its checks."),
    ], title="staging-deploy-timeout", custom=True, ctx=95_000, ago=timedelta(minutes=6), prs=[gh + "infra/pull/97"]),
        "staging-deploy-timeout", "busy"))
    write_conv(home, "/Users/you/code/payments-api", [
        ("user", "add retries to the webhook sender"),
        ("assistant", "Webhooks now retry 5 times with exponential backoff (1s to 16s), and give up with an alert after that."),
    ], title="Webhook retries", ctx=58_000, ago=timedelta(days=1, hours=3), prs=[gh + "payments-api/pull/790"])
    write_conv(home, "/Users/you/code/data", [
        ("user", "the nightly export is 3x bigger than last week"),
        ("assistant", "A join on `events` started duplicating rows when a customer had two active plans. Deduplicating on `event_id` brings it back to normal size."),
    ], title="Nightly export size", model="claude-fable-5-1", ctx=730_000, ago=timedelta(days=3))
    write_conv(home, "/Users/you/code/infra", [
        ("user", "rotate the API keys for the staging environment"),
        ("assistant", "Rotated all four staging keys, updated the secrets, and restarted the two services that read them. Old keys are revoked."),
    ], title="Rotate staging keys", ctx=33_000, ago=timedelta(days=6))
    return live

def start_live(home, live):
    """One idle process per live session, on a terminal of its own (so ccs
    treats it as a tab it can switch to), and the session file Claude Code
    would write for it."""
    procs = []
    for sid, name, status in live:
        _, slave = pty.openpty()
        p = subprocess.Popen(["sleep", "600"], stdin=slave, stdout=slave, stderr=slave,
                             preexec_fn=lambda: (os.setsid(), fcntl.ioctl(0, termios.TIOCSCTTY, 0)))
        os.close(slave)
        procs.append(p)
        started = subprocess.check_output(["ps", "-o", "lstart=", "-p", str(p.pid)], text=True,
                                          env={"TZ": "UTC", "LC_ALL": "C", "PATH": "/bin:/usr/bin"}).strip()
        with open(os.path.join(home, ".claude", "sessions", f"{p.pid}.json"), "w") as f:
            json.dump({"pid": p.pid, "sessionId": sid, "name": name, "status": status, "procStart": started,
                       "statusUpdatedAt": int(time.time() * 1000) - 60_000}, f)
    return procs

ESC, DOWN, UP = "\x1b", "\x1b[B", "\x1b[A"
KEYS = [  # (delay before, keys)
    (3.0, ""),            # the coordinator: its sessions, PRs and ccs:// links
    (1.2, DOWN),          # waiting on you: its question, answerable with a digit
    (2.5, "\x18w"),       # ^X W the waiting overlay
    (3.0, ESC),
    (1.2, DOWN),          # ready to merge
    (2.0, "\x0f"),        # ^O its links and PRs
    (2.5, ESC),
    (1.0, DOWN),          # busy, checks running
    (1.8, DOWN),
    (1.0, DOWN),
    (1.2, ""),
    *[(0.12, c) for c in "flaky"],
    (2.0, "\x1d"),        # ^] next hit
    (1.8, ESC),           # clear the search
    (1.2, "?"),           # help
    (3.0, ESC),
    (1.2, "\t"),          # usage screen
    (3.0, "\t"),
    (1.5, "\x03"),        # quit
]

def main():
    root = os.getcwd()
    home = tempfile.mkdtemp(prefix="ccs-demo-")
    live = fixtures(home)
    bindir = os.path.join(home, "bin")
    os.makedirs(bindir)
    shutil.copy(os.path.join(root, "demo", "fake_gh.py"), os.path.join(bindir, "gh"))
    os.chmod(os.path.join(bindir, "gh"), stat.S_IRWXU)
    binp = os.path.join(bindir, "ccs")
    # the newest release, so ccs offers no update over the demo
    subprocess.check_call(["git", "fetch", "--quiet", "--tags", "origin"])
    tag = subprocess.check_output(["git", "describe", "--tags", "--abbrev=0", "origin/main"], text=True).strip().lstrip("v")
    subprocess.check_call(["go", "build", "-ldflags", f"-X main.version={tag}", "-o", binp, "."])
    procs = start_live(home, live)
    env = {"HOME": home, "CLAUDE_CONFIG_DIR": os.path.join(home, ".claude"), "TERM": "xterm-256color",
           "PATH": f"{bindir}:/usr/bin:/bin", "LANG": "en_GB.UTF-8", "COLORTERM": "truecolor"}
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(home)
        os.execve(binp, [binp], env)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    start = time.time()
    events = []
    dec = codecs.getincrementaldecoder("utf-8")("replace")  # a character can span two reads
    def pump(secs):
        end = time.time() + secs
        while True:
            left = end - time.time()
            if left <= 0:
                return True
            r, _, _ = select.select([fd], [], [], left)
            if r:
                try:
                    data = os.read(fd, 65536)
                except OSError:
                    return False
                if not data:
                    return False
                text = dec.decode(data)
                if text:
                    events.append([round(time.time() - start, 3), "o", text])
    # wait for the first full screen, and the PR lookups, before typing; the
    # clock starts there
    deadline = time.time() + 30
    while time.time() < deadline and not any("coordinator" in e[2] for e in events):
        pump(0.2)
    pump(4.0)
    events[:] = [[0.0, "o", "".join(e[2] for e in events)]]  # the start-up wait plays as an instant
    start = time.time()
    for delay, keys in KEYS:
        if not pump(delay):
            break
        if keys:
            os.write(fd, keys.encode())
    pump(1.0)
    for p in procs + [None]:
        try:
            os.kill(p.pid if p else pid, 9)
        except ProcessLookupError:
            pass
    out = os.path.join(root, "demo", "demo.cast")
    with open(out, "w") as f:
        f.write(json.dumps({"version": 2, "width": COLS, "height": ROWS, "env": {"TERM": "xterm-256color"}}) + "\n")
        for e in events:
            f.write(json.dumps(e) + "\n")
    shutil.rmtree(home, ignore_errors=True)
    print(f"wrote {out}: {len(events)} events, {events[-1][0] if events else 0:.1f}s")

main()

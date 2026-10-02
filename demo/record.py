#!/usr/bin/env python3
"""Record the README demo: fake conversations in a throwaway HOME, ccs driven
by scripted keys, written as an asciicast (demo/demo.cast). Render the GIF with
  agg --theme monokai --font-size 14 demo/demo.cast demo.gif
Run from the repo root: python3 demo/record.py"""
import codecs, json, os, pty, select, shutil, struct, subprocess, sys, tempfile, time, fcntl, termios, uuid
from datetime import datetime, timedelta, timezone

COLS, ROWS = 140, 40
NOW = datetime.now(timezone.utc)
iso = lambda d: d.strftime("%Y-%m-%dT%H:%M:%S.000Z")

def usage(ctx, out=400):
    return {"input_tokens": 6, "cache_read_input_tokens": int(ctx * 0.9), "cache_creation_input_tokens": int(ctx * 0.1),
            "output_tokens": out, "cache_creation": {"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": int(ctx * 0.1)}}

def write_conv(home, project, turns, title=None, custom=False, model="claude-opus-5-5", ctx=80_000, ago=timedelta(hours=1)):
    sid = str(uuid.uuid4())
    d = os.path.join(home, ".claude", "projects", project.replace("/", "-"))
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
            lines.append({"type": "assistant", "sessionId": sid, "timestamp": iso(t),
                          "message": {"id": f"msg_{uuid.uuid4().hex[:12]}", "role": "assistant", "model": model,
                                      "content": [{"type": "text", "text": text}], "usage": usage(c)}})
    if title:
        lines.append({"type": "custom-title" if custom else "ai-title", "customTitle" if custom else "aiTitle": title, "sessionId": sid})
    p = os.path.join(d, sid + ".jsonl")
    with open(p, "w") as f:
        f.write("\n".join(json.dumps(l) for l in lines) + "\n")
    mt = (NOW - ago).timestamp()
    os.utime(p, (mt, mt))

def fixtures(home):
    os.makedirs(os.path.join(home, ".claude", "sessions"), exist_ok=True)
    for p in (os.path.join(home, ".claude.json"), os.path.join(home, ".claude", ".claude.json")):
        with open(p, "w") as f:
            json.dump({"oauthAccount": {"emailAddress": "you@example.com"}}, f)
    write_conv(home, "/Users/you/code/payments-api", [
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
    ], title="Fix flaky checkout test", custom=True, ctx=212_000, ago=timedelta(minutes=4))
    write_conv(home, "/Users/you/code/infra", [
        ("user", "why did the staging deploy fail last night?"),
        ("assistant", "The deploy timed out waiting for the database migration. The migration took a lock on `orders` while a long report query held it.\n\n"
                      "**Suggested fix:** run the migration with `lock_timeout = 5s` and retry, instead of waiting forever."),
        ("user", "ok add the lock timeout"),
        ("assistant", "Added `SET lock_timeout = '5s'` to the migration runner and a retry with backoff. The flaky deploy test now passes 20 runs in a row."),
    ], title="Staging deploy timeout", ctx=95_000, ago=timedelta(hours=2))
    write_conv(home, "/Users/you/code/web", [
        ("user", "make the settings page work on mobile"),
        ("assistant", "The settings form used a fixed 960px grid. I switched it to a single column under 640px and made the save bar sticky.\n\n"
                      "- tested on iPhone SE and Pixel 7 sizes\n- no layout shift on the desktop page"),
    ], title="Mobile settings layout", model="claude-sonnet-5-5", ctx=41_000, ago=timedelta(hours=7))
    write_conv(home, "/Users/you/code/payments-api", [
        ("user", "add retries to the webhook sender"),
        ("assistant", "Webhooks now retry 5 times with exponential backoff (1s to 16s), and give up with an alert after that."),
    ], title="Webhook retries", ctx=58_000, ago=timedelta(days=1, hours=3))
    write_conv(home, "/Users/you/code/data", [
        ("user", "the nightly export is 3x bigger than last week"),
        ("assistant", "A join on `events` started duplicating rows when a customer had two active plans. Deduplicating on `event_id` brings it back to normal size."),
    ], title="Nightly export size", model="claude-fable-5-1", ctx=730_000, ago=timedelta(days=3))
    write_conv(home, "/Users/you/code/infra", [
        ("user", "rotate the API keys for the staging environment"),
        ("assistant", "Rotated all four staging keys, updated the secrets, and restarted the two services that read them. Old keys are revoked."),
    ], title="Rotate staging keys", ctx=33_000, ago=timedelta(days=6))

KEYS = [  # (delay before, keys)
    (2.5, ""),
    *[(0.12, c) for c in "flaky"],
    (2.0, "\x1d"),        # ^] next hit
    (1.8, "\x1d"),
    (1.8, "\x15"),        # ^U clears the search
    (1.2, "\x1b[B"), (1.2, "\x1b[B"), (1.2, "\x1b[A"), (1.0, "\x1b[A"),
    (1.5, "\x0b"), (0.8, "\x0b"), (1.2, "\x0a"),  # ^K/^J scroll the conversation
    (1.5, "\x07"),        # ^G help
    (3.0, "\x07"),
    (1.2, "\t"),          # usage screen
    (4.0, "\t"),
    (1.5, "\x0c"),        # ^L changelog
    (3.0, "\x1b[B" * 6),
    (2.0, "\x0c"),
    (1.5, "\x03"),        # quit
]

def main():
    root = os.getcwd()
    home = tempfile.mkdtemp(prefix="ccs-demo-")
    fixtures(home)
    binp = os.path.join(home, "ccs")
    tag = subprocess.check_output(["git", "describe", "--tags", "--abbrev=0"], text=True).strip().lstrip("v")
    subprocess.check_call(["go", "build", "-ldflags", f"-X main.version={tag}", "-o", binp, "."])
    env = {"HOME": home, "CLAUDE_CONFIG_DIR": os.path.join(home, ".claude"), "TERM": "xterm-256color",
           "PATH": "/usr/bin:/bin", "LANG": "en_GB.UTF-8", "COLORTERM": "truecolor"}
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
    # wait for the first full screen before typing, and start the clock there
    deadline = time.time() + 30
    while time.time() < deadline and not any("WHEN" in e[2] for e in events):
        pump(0.2)
    for e in events:  # the start-up wait plays as an instant
        e[0] = 0.0
    start = time.time()
    for delay, keys in KEYS:
        if not pump(delay):
            break
        if keys:
            os.write(fd, keys.encode())
    pump(1.0)
    try:
        os.kill(pid, 9)
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

#!/usr/bin/env python3
"""Record the README demo: fake conversations in a throwaway HOME, ccs driven
by scripted keys, written as an asciicast (demo/demo.cast). Live sessions are
idle processes with session files pointing at them, PRs come from
demo/fake_gh.py, and the waiting session's question reaches ccs through the
real `ccs hook permission`, so nothing real is read or shown. Render the GIF
with
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

def peer(name, text):
    """A message another session sent, as Claude Code logs it."""
    return ("user", f'Another Claude session sent a message:\n<cross-session-message from="uds:/tmp/demo/{name}.sock" '
                    f'from-name="{name}">\n{text}\n</cross-session-message>')

def conv_path(home, project, sid):
    return os.path.join(home, ".claude", "projects", project.replace("/", "-").replace(" ", "-").replace(".", "-"), sid + ".jsonl")

def write_conv(home, project, turns, title=None, custom=False, model="claude-opus-5-5", ctx=80_000, ago=timedelta(hours=1),
               prs=(), ask=None):
    """turns are (role, text); prs are PR addresses the session touched; ask is
    a pending AskUserQuestion's input, left unanswered. Returns the session id
    and the pending tool call's id."""
    sid = str(uuid.uuid4())
    p = conv_path(home, project, sid)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    t = NOW - ago - timedelta(minutes=3 * len(turns))
    lines, tool = [], None
    for i, (role, text) in enumerate(turns):
        t += timedelta(minutes=3)
        if role == "user":
            lines.append({"type": "user", "cwd": project, "sessionId": sid, "entrypoint": "cli", "timestamp": iso(t),
                          "message": {"role": "user", "content": text}})
        else:
            c = int(ctx * (i + 1) / len(turns))
            content = [{"type": "text", "text": text}]
            if ask and i == len(turns) - 1:
                tool = "toolu_" + uuid.uuid4().hex[:12]
                content.append({"type": "tool_use", "id": tool, "name": "AskUserQuestion", "input": ask})
            lines.append({"type": "assistant", "sessionId": sid, "timestamp": iso(t),
                          "message": {"id": f"msg_{uuid.uuid4().hex[:12]}", "role": "assistant", "model": model,
                                      "content": content, "usage": usage(c)}})
    for u in prs:
        lines.append({"type": "pr-link", "prUrl": u, "sessionId": sid, "timestamp": iso(t)})
    if title:
        lines.append({"type": "custom-title" if custom else "ai-title", "customTitle" if custom else "aiTitle": title, "sessionId": sid})
    with open(p, "w") as f:
        f.write("\n".join(json.dumps(l) for l in lines) + "\n")
    mt = (NOW - ago).timestamp()
    os.utime(p, (mt, mt))
    return sid, tool

ASK = {"questions": [{"question": "Where should the settings page switch to one column?", "header": "Breakpoint", "multiSelect": False,
                      "options": [{"label": "640px (Recommended)", "description": "Phones only; tablets keep two columns"},
                                  {"label": "768px", "description": "Phones and small tablets"},
                                  {"label": "1024px", "description": "Every tablet, landscape included"}]}]}

def git_repo(path, commits):
    """A small repo with made-up history, for the ! command to show."""
    os.makedirs(path, exist_ok=True)
    env = {"PATH": "/usr/bin:/bin", "HOME": path, "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
           "GIT_AUTHOR_NAME": "Demo Dev", "GIT_AUTHOR_EMAIL": "dev@example.com",
           "GIT_COMMITTER_NAME": "Demo Dev", "GIT_COMMITTER_EMAIL": "dev@example.com"}
    run = lambda *a: subprocess.check_call(["git", *a], cwd=path, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    run("init", "-q", "-b", "main")
    for msg in commits:
        with open(os.path.join(path, "CHANGES"), "a") as f:
            f.write(msg + "\n")
        run("add", "CHANGES")
        run("commit", "-q", "-m", msg)

def fixtures(home):
    """Writes the transcripts and returns the live ones as {name: (session id, status, cwd)}, plus
    what the waiting session's question needs."""
    cfg = os.path.join(home, "Library", "Application Support", "ccs")
    code = os.path.join(home, "code")
    os.makedirs(os.path.join(home, ".claude", "sessions"), exist_ok=True)
    os.makedirs(cfg, exist_ok=True)
    for p in (os.path.join(home, ".claude.json"), os.path.join(home, ".claude", ".claude.json")):
        with open(p, "w") as f:
            json.dump({"oauthAccount": {"emailAddress": "you@example.com", "organizationName": "Example Org"}}, f)
    with open(os.path.join(cfg, "settings.json"), "w") as f:
        json.dump({"auto_update": False}, f)
    gh = "https://github.com/example/"
    live = {}
    orch_dir, web_orch_dir = os.path.join(cfg, "orchestrator"), os.path.join(cfg, "web-orchestrator")
    for d in (orch_dir, web_orch_dir):
        os.makedirs(d, exist_ok=True)
    web, pay, infra = os.path.join(code, "web"), os.path.join(code, "payments-api"), os.path.join(code, "infra")
    git_repo(web, ["feat: settings form on a responsive grid", "feat: dark mode tokens for the settings page"])
    git_repo(pay, ["feat: retry webhooks with backoff", "test: shuffle line items in the checkout test",
                   "fix: sort line items before summing the checkout total"])
    git_repo(infra, ["fix: lock timeout for migrations"])

    orch, _ = write_conv(home, orch_dir, [
        ("user", "fix the flaky checkout test, sort out last night's staging deploy, and ship the settings page work"),
        ("assistant", "Started a session for each, and a sub-orchestrator for the web work:\n\n"
                      "| session | PR | state |\n|---|---|---|\n"
                      "| ccs://session/web-orchestrator | 2 workers | running |\n"
                      "| ccs://session/fix-flaky-checkout | payments-api#812 | ready to merge |\n"
                      "| ccs://session/staging-deploy-timeout | infra#97 | checks running |\n\n"
                      "Merge order: infra#97 before payments-api#812."),
        peer("web-orchestrator", "STATUS web#43 green and approved; mobile-settings is asking which breakpoint to use"),
        ("assistant", "**Needs you:** mobile-settings is asking which breakpoint to use. web#43 is ready to merge."),
    ], title="orchestrator", custom=True, ctx=64_000, ago=timedelta(minutes=1))
    live["orchestrator"] = (orch, "idle", orch_dir)
    with open(os.path.join(cfg, "orchestrator.json"), "w") as f:
        json.dump({"session_id": orch, "offered": True}, f)
    webo, _ = write_conv(home, web_orch_dir, [
        ("user", "You're the web sub-orchestrator: make the settings page work on mobile and add dark mode, one worker each."),
        ("assistant", "Launched ccs://session/mobile-settings and ccs://session/dark-mode."),
        peer("dark-mode", "HANDOVER https://github.com/example/web/pull/43 head 3f9c2ab checks passing"),
        ("assistant", "dark-mode handed over **web#43**: green and approved. mobile-settings is waiting on a breakpoint question."),
    ], title="web-orchestrator", custom=True, ctx=38_000, ago=timedelta(minutes=2))
    live["web-orchestrator"] = (webo, "idle", web_orch_dir)
    mob, tool = write_conv(home, web, [
        ("user", "make the settings page work on mobile"),
        ("assistant", "The settings form uses a fixed 960px grid. I can switch it to one column on small screens, "
                      "but the save bar placement depends on where the break is."),
    ], title="mobile-settings", custom=True, model="claude-sonnet-5-5", ctx=41_000, ago=timedelta(minutes=3),
        prs=[gh + "web/pull/41"], ask=ASK)
    live["mobile-settings"] = (mob, "waiting", web)
    dark, _ = write_conv(home, web, [
        ("user", "add dark mode to the settings page"),
        ("assistant", "Done: the settings page reads the theme tokens, and a toggle sits in the header.\n\n"
                      "PR: https://github.com/example/web/pull/43, checks passing. Sent the HANDOVER to web-orchestrator."),
    ], title="dark-mode", custom=True, model="claude-sonnet-5-5", ctx=52_000, ago=timedelta(minutes=5), prs=[gh + "web/pull/43"])
    live["dark-mode"] = (dark, "idle", web)
    flaky, _ = write_conv(home, pay, [
        ("user", "the checkout test is flaky on CI, can you find out why?"),
        ("assistant", "Found it. `test_checkout_total` depends on the **order of a dict** built from a set, so it passes or fails at random.\n\n"
                      "| run | result | order |\n|---|---|---|\n| #4120 | pass | a, b |\n| #4121 | fail | b, a |\n| #4122 | pass | a, b |\n\n"
                      "Fix: sort the line items before summing. See [the failing run](https://github.com/example/payments-api/actions/runs/4121)."),
        ("user", "nice, fix it and open a PR"),
        ("assistant", "Done:\n\n- sorted line items in `checkout.total()`\n- added a regression test that shuffles the input 100 times\n\n"
                      "```python\ndef total(items):\n    return sum(i.price for i in sorted(items, key=lambda i: i.sku))\n```\n\n"
                      "PR: https://github.com/example/payments-api/pull/812"),
        ("assistant", "CI is green on #812, so the flaky test is fixed and the PR is ready to merge."),
    ], title="fix-flaky-checkout", custom=True, ctx=212_000, ago=timedelta(minutes=4),
        prs=[gh + "payments-api/pull/808", gh + "payments-api/pull/812"])
    live["fix-flaky-checkout"] = (flaky, "idle", pay)
    stag, _ = write_conv(home, infra, [
        ("user", "why did the staging deploy fail last night?"),
        ("assistant", "The deploy timed out waiting for the database migration. The migration took a lock on `orders` while a long report query held it.\n\n"
                      "**Suggested fix:** run the migration with `lock_timeout = 5s` and retry, instead of waiting forever."),
        ("user", "ok add the lock timeout and open a PR"),
        ("assistant", "Opened https://github.com/example/infra/pull/97 with the lock timeout and a retry with backoff. Waiting for its checks."),
    ], title="staging-deploy-timeout", custom=True, ctx=95_000, ago=timedelta(minutes=6), prs=[gh + "infra/pull/97"])
    live["staging-deploy-timeout"] = (stag, "busy", infra)
    tree = {"web-orchestrator": "orchestrator", "fix-flaky-checkout": "orchestrator", "staging-deploy-timeout": "orchestrator",
            "mobile-settings": "web-orchestrator", "dark-mode": "web-orchestrator"}
    with open(os.path.join(cfg, "launch-parents.json"), "w") as f:  # who launched whom, as ccs launch records it
        json.dump({"parents": {child: {"child": child, "child_id": live[child][0], "parent": parent, "parent_id": live[parent][0],
                                       "at": iso(NOW - timedelta(hours=1))} for child, parent in tree.items()}}, f)
    write_conv(home, pay, [
        ("user", "add retries to the webhook sender"),
        ("assistant", "Webhooks now retry 5 times with exponential backoff (1s to 16s), and give up with an alert after that."),
    ], title="Webhook retries", ctx=58_000, ago=timedelta(days=1, hours=3), prs=[gh + "payments-api/pull/790"])
    write_conv(home, os.path.join(code, "data"), [
        ("user", "the nightly export is 3x bigger than last week"),
        ("assistant", "A join on `events` started duplicating rows when a customer had two active plans. Deduplicating on `event_id` brings it back to normal size."),
    ], title="Nightly export size", model="claude-fable-5-1", ctx=730_000, ago=timedelta(days=3))
    question = {"session_id": mob, "cwd": web, "hook_event_name": "PermissionRequest", "tool_name": "AskUserQuestion",
                "tool_input": ASK, "tool": tool, "path": conv_path(home, web, mob)}
    return live, question

def session_file(home, pid, sid, name, status, started, fresh=False):
    """fresh: the status changed just now, else a minute ago (a wait always just now)."""
    now = int(time.time() * 1000)
    with open(os.path.join(home, ".claude", "sessions", f"{pid}.json"), "w") as f:
        json.dump({"pid": pid, "sessionId": sid, "name": name, "status": status, "procStart": started,
                   "statusUpdatedAt": now if fresh or status == "waiting" else now - 60_000}, f)

def start_live(home, live, env, binp, question):
    """One idle process per live session, on a terminal of its own (so ccs
    treats it as a tab it can switch to), and the session file Claude Code
    would write for it. The waiting session's process runs `ccs hook
    permission` once told to (the go file), as Claude Code would for its
    AskUserQuestion, so ccs holds the question and draws its menu."""
    procs, files = [], {}
    req = os.path.join(home, "question.json")
    with open(req, "w") as f:
        json.dump({k: question[k] for k in ("session_id", "cwd", "hook_event_name", "tool_name", "tool_input")}, f)
    go = os.path.join(home, "go")
    for name, (sid, status, cwd) in live.items():
        cmd = ["sleep", "600"]
        if name == "mobile-settings":
            cmd = ["sh", "-c", f'while [ ! -e "{go}" ]; do sleep 0.1; done; "{binp}" hook permission < "{req}" > /dev/null 2>&1; exec sleep 600']
        _, slave = pty.openpty()
        p = subprocess.Popen(cmd, stdin=slave, stdout=slave, stderr=slave, env=env, cwd=cwd,
                             preexec_fn=lambda: (os.setsid(), fcntl.ioctl(0, termios.TIOCSCTTY, 0)))
        os.close(slave)
        procs.append(p)
        started = subprocess.check_output(["ps", "-o", "lstart=", "-p", str(p.pid)], text=True,
                                          env={"TZ": "UTC", "LC_ALL": "C", "PATH": "/bin:/usr/bin"}).strip()
        session_file(home, p.pid, sid, name, status, started)
        files[name] = (p.pid, sid, started)
    return procs, files, go

ESC, DOWN, UP, ENTER = "\x1b", "\x1b[B", "\x1b[A", "\r"

def main():
    root = os.getcwd()
    home = tempfile.mkdtemp(prefix="ccs-demo-", dir="/tmp")  # short: the hold socket's path has a length limit
    live, question = fixtures(home)
    bindir = os.path.join(home, "bin")
    os.makedirs(bindir)
    shutil.copy(os.path.join(root, "demo", "fake_gh.py"), os.path.join(bindir, "gh"))
    os.chmod(os.path.join(bindir, "gh"), stat.S_IRWXU)
    binp = os.path.join(bindir, "ccs")
    # the newest release, so ccs offers no update over the demo
    subprocess.check_call(["git", "fetch", "--quiet", "--tags", "origin"])
    tag = subprocess.check_output(["git", "describe", "--tags", "--abbrev=0", "origin/main"], text=True).strip().lstrip("v")
    subprocess.check_call(["go", "build", "-ldflags", f"-X main.version={tag}", "-o", binp, "."])
    env = {"HOME": home, "CLAUDE_CONFIG_DIR": os.path.join(home, ".claude"), "TERM": "xterm-256color", "SHELL": "/bin/sh",
           "PATH": f"{bindir}:/usr/bin:/bin", "LANG": "en_GB.UTF-8", "COLORTERM": "truecolor",
           "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1"}
    procs, files, go = start_live(home, live, env, binp, question)

    def ask():  # Claude Code would write the wait, then run the hook
        pid, sid, started = files["mobile-settings"]
        session_file(home, pid, sid, "mobile-settings", "waiting", started)
        open(go, "w").close()

    def answered():  # Claude Code takes the answer and carries on
        pid, sid, started = files["mobile-settings"]
        t = datetime.now(timezone.utc)
        with open(question["path"], "a") as f:
            f.write(json.dumps({"type": "user", "cwd": question["cwd"], "sessionId": sid, "timestamp": iso(t), "message": {
                "role": "user", "content": [{"type": "tool_result", "tool_use_id": question["tool"],
                                             "content": 'User has answered your questions: "Where should the settings page switch to one column?"="640px (Recommended)".'}]}}) + "\n")
            f.write(json.dumps({"type": "assistant", "sessionId": sid, "timestamp": iso(t), "message": {
                "id": f"msg_{uuid.uuid4().hex[:12]}", "role": "assistant", "model": "claude-sonnet-5-5",
                "content": [{"type": "text", "text": "640px it is: one column on phones, the save bar pinned to the bottom. Updating web#41."}],
                "usage": usage(44_000)}}) + "\n")
        session_file(home, pid, sid, "mobile-settings", "busy", started, fresh=True)

    KEYS = [  # (delay before, keys or a step to run)
        (3.0, ""),             # the orchestrator: its tree, a sub-orchestrator with its own workers
        (0.1, DOWN),           # the sub-orchestrator, with a worker's HANDOVER
        (2.8, ask),            # a question arrives through the hook
        (1.2, "\t"),           # Tab: the session that needs you, its menu in the box
        (2.5, "3"),            # a digit highlights an option
        (1.0, "1"),
        (1.2, ENTER),          # Enter sends it
        (0.6, answered),       # it moves to the end of its family as the newest
        (2.4, DOWN),           # a worker with a PR ready to merge
        (2.0, "\x0f"),         # ^O its links and PRs
        (2.2, ESC),
        (0.8, "\x13"),         # ^S the message box
        *[(0.07, c) for c in "!git log --oneline -3"],
        (1.5, ENTER),          # a ! command: runs in the session's folder
        (3.0, ESC),            # closes its output
        (0.6, ESC),            # back to the search
        (0.6, ""),
        *[(0.12, c) for c in "flaky"],
        (2.0, ESC),            # clear the search
        (1.0, "\x14y"),        # ^T Y: usage screen
        (3.0, ESC),            # back
        (1.5, "\x03"),         # quit
    ]

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
    while time.time() < deadline and not any("orchestrator" in e[2] for e in events):
        pump(0.2)
    pump(4.0)
    events[:] = [[0.0, "o", "".join(e[2] for e in events)]]  # the start-up wait plays as an instant
    start = time.time()
    for delay, keys in KEYS:
        if not pump(delay):
            break
        if callable(keys):
            keys()
        elif keys:
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
    if os.environ.get("KEEP_HOME"):
        print("kept", home)
    else:
        shutil.rmtree(home, ignore_errors=True)
    print(f"wrote {out}: {len(events)} events, {events[-1][0] if events else 0:.1f}s")

main()

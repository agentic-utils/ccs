package sessions

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// A restart stops a Claude Code session and resumes the same conversation,
// under the same name and in the same folder, in a new tab through the
// launcher, so every flag in CCS_CLAUDE_ARGS is passed again. It picks up a
// new ccs, Claude Code or plugin version, or flags the session lacked.
//
// An idle session is stopped at once: idle is already a safe point. One
// mid-turn is refused unless forced. A session restarting itself (the
// caller descends from its process) can't wait for its own stop, which would
// kill the waiting: a detached helper waits for its turn to end, then stops
// and resumes it.

// RestartNote heads the resumed session's first prompt.
const RestartNote = "[Automatic notice from ccs restart, run by the user or by the session that coordinates theirs. Not typed by the user.]\n\n"

// restartText is the resumed session's first prompt, after RestartNote.
const restartText = "This session was just restarted: stopped, then resumed in a new tab with the same conversation. " +
	"Background tasks, Monitors and subagents the old process ran are gone. Start again the ones you still need, then carry on with what you were doing. " +
	"If nothing was in progress, say so in one line and wait."

// RestartRetry is how long a restart waits before trying a failed resume
// once more.
var RestartRetry = 3 * time.Second

// RestartOpts says what to restart and how.
type RestartOpts struct {
	Name    string
	Force   bool             // restart it mid-turn, stopping it where it is
	Timeout time.Duration    // restarting itself: how long to wait for its turn to end; GracefulTimeout if zero
	OnTab   func(tty string) // called once the new tab is open
}

// restartTarget is the session a restart stops and resumes.
type restartTarget struct {
	pid      int
	id, cwd  string
	tty      string
	status   string
	statusAt int64
}

// findRestartTarget is the one running Claude Code session called name, with
// what resuming it needs.
func findRestartTarget(name string) (restartTarget, error) {
	if !state.SessionName.MatchString(name) {
		return restartTarget{}, fmt.Errorf("name %q: use letters, digits, '.', '_' and '-' (at most 64)", name)
	}
	pids, err := Named(name)
	if err != nil {
		return restartTarget{}, err
	}
	switch len(pids) {
	case 0:
		return restartTarget{}, fmt.Errorf("no running session named %s: ccs launch --name %s --resume <id> starts it again", name, name)
	case 1:
	default:
		return restartTarget{}, fmt.Errorf("%d running sessions are named %s (pids %v): restart one by hand", len(pids), name, pids)
	}
	pid := pids[0]
	f, err := ReadSessionFile(pid)
	if err != nil || f.SessionID == "" {
		return restartTarget{}, fmt.Errorf("%s (pid %d) isn't a Claude Code session (a Codex one, perhaps): ccs restart resumes Claude Code sessions only", name, pid)
	}
	if f.Name != name {
		return restartTarget{}, fmt.Errorf("pid %d is now another session (%s)", pid, f.Name)
	}
	tty := proc.TTYOf(pid)
	if tty == "" {
		return restartTarget{}, fmt.Errorf("%s (pid %d) has no terminal: an app or claude --bg started it, restart it there", name, pid)
	}
	cwd := f.Cwd
	if cwd == "" {
		if path := transcriptOf(f.SessionID); path != "" {
			if c, err := transcript.ParseConversationFile(path, time.Time{}, 0); err == nil && c != nil {
				cwd = c.Cwd
			}
		}
	}
	if cwd == "" || cwd == "unknown" {
		return restartTarget{}, fmt.Errorf("%s (pid %d): neither its session file nor its transcript says which folder it runs in", name, pid)
	}
	if !Resumable(f.SessionID, false) {
		return restartTarget{}, fmt.Errorf("%s (pid %d) has no transcript to resume yet, so a restart would lose it", name, pid)
	}
	return restartTarget{pid: pid, id: f.SessionID, cwd: cwd, tty: tty, status: f.Status, statusAt: f.StatusUpdatedAt}, nil
}

// midTurn: the session is working, or waiting on a menu or prompt mid-turn.
func midTurn(status string) bool { return status == "busy" || status == "waiting" }

// IsCaller: pid is this process or one of its ancestors, as when a session
// runs ccs restart (or calls restart_session) on itself. Stubbed in tests.
var IsCaller = func(pid int) bool {
	parent := map[int]int{}
	if out, err := proc.PsTree(); err == nil {
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			p, err1 := strconv.Atoi(f[0])
			pp, err2 := strconv.Atoi(f[1])
			if err1 == nil && err2 == nil {
				parent[p] = pp
			}
		}
	}
	p := os.Getpid()
	for range 40 {
		if p == pid {
			return true
		}
		if p <= 1 {
			return false
		}
		next, ok := parent[p]
		if !ok && p == os.Getpid() {
			next, ok = os.Getppid(), true
		}
		if !ok {
			return false
		}
		p = next
	}
	return false
}

// errRestarting: another restart of the name holds its lock.
var errRestarting = errors.New("a restart of it is already under way")

// lockRestart holds name's restart lock, so two restarts of one session
// never race to stop and resume it.
func lockRestart(name string) (func(), error) {
	dir := filepath.Join(state.LaunchDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "restart.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errRestarting
	}
	return func() { f.Close() }, nil
}

// RestartSession restarts the running session called name: at once when it
// is idle (or forced), refused mid-turn, and after its turn when it is the
// caller's own session.
func RestartSession(o RestartOpts) (string, error) {
	if o.Timeout <= 0 {
		o.Timeout = GracefulTimeout
	}
	t, err := findRestartTarget(o.Name)
	if err != nil {
		return "", err
	}
	if IsCaller(t.pid) {
		return restartSelf(o, t)
	}
	if midTurn(t.status) && !o.Force {
		since := ""
		if t.statusAt > 0 {
			since = " for " + time.Since(time.UnixMilli(t.statusAt)).Round(time.Second).String()
		}
		what := "working"
		if t.status == "waiting" {
			what = "waiting on a menu or prompt"
		}
		return "", fmt.Errorf("%s is mid-turn (%s%s), and a restart would stop it where it is: wait for it to go idle, or pass --force (force on the tool) to restart it anyway", o.Name, what, since)
	}
	unlock, err := lockRestart(o.Name)
	if err != nil {
		return "", fmt.Errorf("%s: %v", o.Name, err)
	}
	defer unlock()
	return stopAndResume(o, t, false)
}

// restartSelf hands the restart to a detached helper, which outlives this
// process, and returns at once: the session's turn has to end first, and
// stopping it would stop whatever waited for that.
func restartSelf(o RestartOpts, t restartTarget) (string, error) {
	unlock, err := lockRestart(o.Name)
	if err != nil {
		return "", fmt.Errorf("%s: %v", o.Name, err)
	}
	unlock() // the helper takes it
	if err := SpawnRestartHelper(o.Name, t.pid, t.id, o.Timeout); err != nil {
		return "", fmt.Errorf("couldn't start the restart of %s, so nothing was stopped: %v", o.Name, err)
	}
	state.LogActivity("restart %s: restarting itself (pid %d), once its turn ends", o.Name, t.pid)
	return fmt.Sprintf("%s restarts once this turn ends (in %s at the latest): ccs stops it, then resumes this conversation in a new tab, whose first message says it was restarted. End the turn now; anything still running then is cut off.", o.Name, o.Timeout), nil
}

// SpawnRestartHelper starts `ccs restart --after-turn` for name in its own
// session (setsid), detached from the terminal and the caller, so it lives
// on when the session it restarts, and the tab it ran in, go away. Stubbed
// in tests.
var SpawnRestartHelper = func(name string, pid int, id string, timeout time.Duration) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "restart", "--after-turn", fmt.Sprintf("%d:%s", pid, id), "--timeout", timeout.String(), name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = "/"
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// RestartAfterTurn is the helper's part of a session restarting itself:
// wait until the session pid, conversation id, has finished the turn that
// asked (it goes idle, or exits), or timeout passes, then stop and resume it.
func RestartAfterTurn(name string, pid int, id string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = GracefulTimeout
	}
	unlock, err := lockRestart(name)
	if err != nil {
		return "", fmt.Errorf("%s: %v", name, err)
	}
	defer unlock()
	t, err := findRestartTarget(name)
	if err != nil {
		return "", err
	}
	if t.pid != pid || t.id != id {
		return "", fmt.Errorf("%s is now pid %d, session %s, not the one that asked to restart: nothing was stopped", name, t.pid, t.id)
	}
	started := time.Now()
	deadline := started.Add(timeout)
	sawBusy, how := false, ""
	exited := false
	for how == "" {
		Sleep(GracefulPoll)
		cur, err := ReadSessionFile(pid)
		switch {
		case !proc.PIDAlive(pid):
			how, exited = "it exited before its turn ended", true
		case err == nil && cur.SessionID != id:
			return "", fmt.Errorf("%s's conversation changed (a /clear?) before its turn ended: nothing was stopped", name)
		case err == nil:
			sawBusy = sawBusy || midTurn(cur.Status)
			if cur.Status == "idle" && (sawBusy || cur.StatusUpdatedAt > started.UnixMilli()) {
				how = "its turn ended"
			}
		}
		if how == "" && time.Now().After(deadline) {
			how = fmt.Sprintf("its turn hadn't ended within %s, so it was stopped where it was", timeout)
		}
	}
	state.LogActivity("restart %s: %s", name, how)
	o := RestartOpts{Name: name}
	msg, err := stopAndResume(o, t, exited)
	if err != nil {
		state.LogActivity("restart %s: %v", name, err)
		return "", err
	}
	state.LogActivity("restart %s: %s", name, msg)
	return msg + " (" + how + ")", nil
}

// launch is LaunchSession, stubbed in tests.
var launch = LaunchSession

// stopAndResume stops t (unless it already exited, then only its tab is
// closed) and resumes its conversation in a new tab. A failed resume is
// tried once more; if that fails too, the error says how to bring it back.
func stopAndResume(o RestartOpts, t restartTarget, exited bool) (string, error) {
	if exited {
		CloseTab(t.tty)
	} else if _, err := CloseSession(o.Name, t.pid); err != nil && proc.PIDAlive(t.pid) {
		return "", fmt.Errorf("couldn't stop %s, so it wasn't restarted: %v", o.Name, err)
	} else if err != nil {
		state.LogActivity("restart %s: stopped, but %v", o.Name, err) // its tab, most likely: the resume goes on
	}
	lo := LaunchOpts{Name: o.Name, Resume: t.id, Cwd: t.cwd, Brief: RestartNote + restartText, NoPreamble: true, OnTab: o.OnTab}
	l, err := launch(lo)
	if err != nil && l.TTY == "" {
		// Nothing opened: a shell or iTerm hiccup. Once more.
		state.LogActivity("restart %s: resume failed (%v), trying again in %s", o.Name, err, RestartRetry)
		Sleep(RestartRetry)
		l, err = launch(lo)
	}
	if err != nil {
		return "", fmt.Errorf("stopped %s, but couldn't resume it: %v. Bring it back with: ccs launch --name %s --resume %s --cwd %s", o.Name, err, o.Name, t.id, proc.ShellQuote(t.cwd))
	}
	return fmt.Sprintf("restarted %s (was pid %d): %s", o.Name, t.pid, l.Text(o.Name)), nil
}

// RunRestart is `ccs restart`.
func RunRestart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "restart it even mid-turn, stopping it where it is")
	timeout := fs.Duration("timeout", GracefulTimeout, "a session restarting itself: how long to wait for its turn to end before stopping it anyway")
	afterTurn := fs.String("after-turn", "", "used by ccs itself: <pid>:<session id> of a session restarting itself")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs restart [--force] [--timeout 2m] <name>\n\nStop the named Claude Code session and resume the same conversation in a new tab, with the same name, folder and CCS_CLAUDE_ARGS. Refused while it is mid-turn, unless --force. Run by the session itself, it restarts once its turn ends.")
		fs.PrintDefaults()
	}
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		name = ""
	}
	if name == "" {
		fs.Usage()
		return 2
	}
	var msg string
	var err error
	if *afterTurn != "" {
		pidText, id, ok := strings.Cut(*afterTurn, ":")
		pid, perr := strconv.Atoi(pidText)
		if !ok || perr != nil || id == "" {
			fs.Usage()
			return 2
		}
		msg, err = RestartAfterTurn(name, pid, id, *timeout)
	} else {
		msg, err = RestartSession(RestartOpts{Name: name, Force: *force, Timeout: *timeout})
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccs restart:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

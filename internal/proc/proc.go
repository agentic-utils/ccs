// Package proc runs external commands with deadlines and asks about
// processes: whether one is alive, when it started, its terminal.
package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/agentic-utils/ccs/internal/state"
)

// RunBounded runs an external command with a deadline, so a hung ps, tmux
// server or unresponsive iTerm can't stall ccs indefinitely.
func RunBounded(timeout time.Duration, env []string, name string, args ...string) ([]byte, error) {
	return RunCommand(timeout, env, false, name, args...)
}

// ErrTimedOut marks a command that hit its deadline, as opposed to one that
// failed: a timed-out tab open may still have opened, a failed one didn't.
var ErrTimedOut = errors.New("timed out")

// RunCommand runs name with a deadline. detach puts it in its own session
// with no controlling terminal, so it can't prompt (git credentials, ssh
// passphrases) behind the TUI and swallow keystrokes. Output is stdout, or
// stdout+stderr when detached (for error messages).
func RunCommand(timeout time.Duration, env []string, detach bool, name string, args ...string) ([]byte, error) {
	return RunCommandIn("", timeout, env, detach, name, args...)
}

// RunCommandIn is runCommand in another folder.
func RunCommandIn(dir string, timeout time.Duration, env []string, detach bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Without this, a child that inherits the output pipe keeps Output()
	// waiting past the deadline.
	cmd.WaitDelay = time.Second
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out []byte
	var err error
	start := time.Now()
	if detach {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("%s: %w after %s", name, ErrTimedOut, timeout)
	}
	if detach { // git/brew during self-update: keep a record to diagnose slow or failed updates
		state.LogUpdate("$ %s %s\n  -> %v in %s\n%s", name, strings.Join(args, " "), state.ErrOrOK(err), time.Since(start).Round(time.Millisecond), state.Indent(out))
	}
	return out, err
}

// ShellResult is what RunShell saw: the output (stdout and stderr together,
// at most the limit, Cut when there was more), the exit code (-1 when it
// didn't exit by itself) and how long it ran.
type ShellResult struct {
	Out  []byte
	Cut  bool
	Code int
	Took time.Duration
}

// capped keeps the first limit bytes written to it and notes whether more came.
type capped struct {
	mu    sync.Mutex
	buf   []byte
	limit int
	cut   bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - len(c.buf); room < len(p) {
		c.buf = append(c.buf, p[:max(room, 0)]...)
		c.cut = true
	} else {
		c.buf = append(c.buf, p...)
	}
	return len(p), nil
}

// RunShell runs line with `shell -lc line` in dir: line is one argument,
// never split or interpolated. It runs in its own session (Setsid: no
// controlling terminal, so nothing can prompt behind the TUI) with no stdin,
// until it exits, timeout passes (ErrTimedOut) or ctx is cancelled
// (context.Canceled); either kills its whole process group.
var RunShell = func(ctx context.Context, dir, shell, line string, timeout time.Duration, limit int) (ShellResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-lc", line)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) } // the group: what it started too
	cmd.WaitDelay = time.Second
	out := &capped{limit: limit}
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	if cmd.Process != nil {
		// The shell is gone, but what it put in the background ("cmd &")
		// is still in its group, unowned: it goes with it.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	res := ShellResult{Took: time.Since(start), Code: -1}
	if cmd.ProcessState != nil {
		res.Code = cmd.ProcessState.ExitCode()
	}
	out.mu.Lock()
	res.Out, res.Cut = out.buf, out.cut
	out.mu.Unlock()
	if res.Cut { // the cap may have split a character: drop its first bytes
		i := len(res.Out) - 1
		for i > 0 && i > len(res.Out)-utf8.UTFMax && !utf8.RuneStart(res.Out[i]) {
			i--
		}
		if i >= 0 && !utf8.FullRune(res.Out[i:]) {
			res.Out = res.Out[:i]
		}
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return res, fmt.Errorf("%w after %s", ErrTimedOut, timeout)
	case ctx.Err() != nil:
		return res, context.Canceled
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) || errors.Is(err, exec.ErrWaitDelay) {
		err = nil // a non-zero exit is the command's answer, in Code; a child left holding the output was killed above
	}
	return res, err
}

// PIDAlive probes pid with signal 0; EPERM still means the process exists.
func PIDAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// ProcessStartTimes returns each pid's start time from one ps call. Pids ps
// can't report are left out.
var ProcessStartTimes = func(live map[string]int) map[int]time.Time {
	out := make(map[int]time.Time)
	if len(live) == 0 {
		return out
	}
	pids := make([]string, 0, len(live))
	for _, pid := range live {
		pids = append(pids, strconv.Itoa(pid))
	}
	// Fixed lstart layout, in UTC like procStart: local time is ambiguous in
	// the hour clocks go back.
	raw, _ := RunBounded(2*time.Second, []string{"LC_ALL=C", "TZ=UTC"}, "ps", "-o", "pid=,lstart=", "-p", strings.Join(pids, ","))
	for _, line := range strings.Split(string(raw), "\n") {
		pidStr, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(pidStr)
		if !ok || err != nil {
			continue
		}
		if t, err := time.Parse(time.ANSIC, strings.Join(strings.Fields(rest), " ")); err == nil {
			out[pid] = t
		}
	}
	return out
}

// ShellQuote wraps s for /bin/sh single-quoted use.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellSafe reports whether s can go through shellQuote into a shell safely
// whatever that shell is: fish treats backslash as an escape even inside
// single quotes, and control characters break the AppleScript string.
func ShellSafe(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f || r == utf8.RuneError })
}

// PsAll is every process's pid, tty and command line. Stubbed in tests.
var PsAll = func() (string, error) {
	out, err := RunBounded(3*time.Second, nil, "ps", "-axww", "-o", "pid=,tty=,command=")
	return string(out), err
}

// TTYOf is pid's terminal, "" when it has none. Stubbed in tests.
var TTYOf = func(pid int) string {
	out, err := RunBounded(2*time.Second, nil, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	tty := strings.TrimSpace(string(out))
	if err != nil || tty == "" || tty == "??" {
		return ""
	}
	return "/dev/" + tty
}

// SignalPID is syscall.Kill, stubbed in tests.
var SignalPID = syscall.Kill

// PsTree is every process's pid, parent pid and command line. Stubbed in tests.
var PsTree = func() (string, error) {
	out, err := RunBounded(3*time.Second, nil, "ps", "-axww", "-o", "pid=,ppid=,command=")
	return string(out), err
}

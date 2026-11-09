package app

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

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ccs launch and ccs close: start a named claude (or codex) session in a new
// iTerm tab, and close one by name. Also behind ^X N / ^X K in the list and
// the launch_session / close_session MCP tools.

// launchOpts says what to start: a brief (the first prompt), or a session to
// resume, under a name, in a folder.
type launchOpts struct {
	name, brief, resume, cwd string
	codex                    bool
	sessionID                string        // claude's id for a new session, known before it starts
	focus                    bool          // bring its tab to the front: it needs an answer there
	wait                     time.Duration // how long it may take to start; launchWait if zero
	inPlace                  bool          // no tab: replace this process with it
}

// launchWait is how long launch waits for the session to show up, and
// closeWait how long close waits for its process to exit.
var (
	launchWait = 20 * time.Second
	closeWait  = 10 * time.Second
)

// lookBin finds claude or codex (ccs isn't a shell, so no alias or function
// stands in the way). Stubbed in tests.
var lookBin = exec.LookPath

// launchScript is the launcher a new tab runs: a login zsh, so PATH and
// CCS_CLAUDE_ARGS are the user's; the secrets file the user's claude()
// wrapper sources; then claude with the brief first, since --channels (in
// CCS_CLAUDE_ARGS) takes every word after it as a channel.
func launchScript(o launchOpts, bin, dir string) string {
	var b strings.Builder
	b.WriteString("#!/bin/zsh -l\n")
	b.WriteString("cd " + proc.ShellQuote(o.cwd) + " || exit 1\n")
	brief := `"$(cat ` + proc.ShellQuote(filepath.Join(dir, "brief.md")) + `)"`
	if o.codex {
		args := []string{proc.ShellQuote(bin)}
		if o.resume != "" {
			args = append(args, "resume", proc.ShellQuote(o.resume))
		}
		// Its launch folder first among the writable ones: sessionsNamed finds
		// it by that.
		flags := transcript.CodexSandboxArgs(dir)
		for i, a := range flags {
			if i > 0 && flags[i-1] == "--add-dir" { // the folders; the flags themselves are plain words
				a = proc.ShellQuote(a)
			}
			args = append(args, a)
		}
		if o.brief != "" {
			args = append(args, brief)
		}
		b.WriteString("exec " + strings.Join(args, " ") + "\n")
		return b.String()
	}
	b.WriteString("[ -f ~/.claude.secret ] && source ~/.claude.secret\n")
	args := []string{proc.ShellQuote(bin)}
	if o.resume != "" {
		args = append(args, "--resume", proc.ShellQuote(o.resume))
	}
	if o.sessionID != "" {
		args = append(args, "--session-id", proc.ShellQuote(o.sessionID))
	}
	if o.brief != "" {
		args = append(args, brief)
	}
	args = append(args, "--name", proc.ShellQuote(o.name), "${=CCS_CLAUDE_ARGS}")
	b.WriteString("exec " + strings.Join(args, " ") + "\n")
	return b.String()
}

// launched is a session launch started: its process and terminal, and a
// Codex session's thread.
type launched struct {
	pid    int
	tty    string
	thread string
}

// resumable: id has a transcript to resume, a Claude Code session under
// the projects folder or, with codex, a Codex thread's rollout.
func resumable(id string, codex bool) bool {
	if codex {
		return transcript.CodexRollout(id) != ""
	}
	if strings.ContainsAny(id, `/\*?[`) {
		return false
	}
	paths, _ := filepath.Glob(filepath.Join(transcript.GetProjectsDir(), "*", id+".jsonl"))
	return len(paths) > 0
}

// launchSession starts o in a new iTerm tab and waits until it's running.
func launchSession(o launchOpts) (launched, error) {
	if !state.SessionName.MatchString(o.name) {
		return launched{}, fmt.Errorf("name %q: use letters, digits, '.', '_' and '-' (at most 64)", o.name)
	}
	if o.brief == "" && o.resume == "" {
		return launched{}, errors.New("give a brief or a session to resume")
	}
	if o.resume != "" && !proc.ShellSafe(o.resume) {
		return launched{}, fmt.Errorf("bad session id %q", o.resume)
	}
	if o.resume != "" && !resumable(o.resume, o.codex) {
		kind := "Claude Code session"
		if o.codex {
			kind = "Codex thread"
		}
		return launched{}, fmt.Errorf("no %s %q to resume", kind, o.resume)
	}
	if o.cwd == "" {
		o.cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(o.cwd); err == nil {
		o.cwd = abs
	}
	if info, err := os.Stat(o.cwd); err != nil || !info.IsDir() {
		return launched{}, fmt.Errorf("no folder %s", o.cwd)
	}
	if !proc.ShellSafe(o.cwd) {
		return launched{}, fmt.Errorf("folder %q can't go into a shell command", o.cwd)
	}
	if !canOpenTabs() && !o.inPlace {
		return launched{}, errors.New("not in iTerm or tmux: launch opens a tab there")
	}
	if err := checkLaunchShell(); err != nil {
		return launched{}, err
	}
	if pids, err := sessionsNamed(o.name); err != nil {
		return launched{}, err
	} else if len(pids) > 0 {
		return launched{}, fmt.Errorf("a session named %s is already running (pid %d)", o.name, pids[0])
	}
	binName := "claude"
	if o.codex {
		binName = "codex"
	}
	bin, err := lookBin(binName)
	if err != nil {
		return launched{}, fmt.Errorf("%s isn't installed: %v", binName, err)
	}
	dir := filepath.Join(state.LaunchDir(), o.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return launched{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte(o.brief), 0o600); err != nil {
		return launched{}, err
	}
	script := filepath.Join(dir, "launch.sh")
	if err := os.WriteFile(script, []byte(launchScript(o, bin, dir)), 0o700); err != nil {
		return launched{}, err
	}
	if o.inPlace {
		return launched{}, execScript(script)
	}
	tty, err := openLaunchTab(script)
	if err != nil {
		return launched{}, fmt.Errorf("couldn't open a tab: %v", err)
	}
	if o.focus {
		focusTTY(tty, true)
	}
	wait := launchWait
	if o.wait > 0 {
		wait = o.wait
	}
	deadline := time.Now().Add(wait)
	retypeAt := time.Now().Add(launchRetype)
	for {
		if !retypeAt.IsZero() && time.Now().After(retypeAt) {
			retypeAt = time.Time{}
			// The shell can draw (a "Last login" line) before it reads input,
			// and what was typed then is lost: type it again, once, if
			// nothing on the tab runs the launcher yet.
			if !launcherRunning(tty, script) {
				state.LogActivity("launch %s: nothing ran the launcher on %s, typing it again", o.name, tty)
				if err := retypeLauncher(tty, script); err != nil {
					state.LogActivity("launch %s: couldn't type it again: %v", o.name, err)
				}
			}
		}
		pids, _ := sessionsNamed(o.name)
		thread := ""
		if o.codex {
			thread = codexThreadFor(o.name) // queue and claim need it
		}
		if len(pids) > 0 && (!o.codex || thread != "" || o.resume != "") {
			return launched{pids[0], tty, thread}, nil
		}
		if time.Now().After(deadline) {
			if len(pids) > 0 {
				return launched{pids[0], tty, ""}, fmt.Errorf("%s started (pid %d) but Codex wrote no transcript for it within %s, so it can't be messaged: see the tab on %s", o.name, pids[0], wait, tty)
			}
			return launched{tty: tty}, fmt.Errorf("%s didn't start within %s: see the tab on %s, where a folder-trust question may be waiting (launcher %s)", o.name, wait, tty, script)
		}
		sleep(250 * time.Millisecond)
	}
}

// launchRetype is how long after typing the launcher ccs checks that
// something on the tab runs it.
var launchRetype = 4 * time.Second

// launcherRunning: a process on tty runs the launcher, or the claude or
// codex it execs.
var launcherRunning = func(tty, script string) bool {
	out, err := proc.PsAll()
	if err != nil {
		return true // can't tell: never type into what may be running
	}
	short := strings.TrimPrefix(tty, "/dev/")
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != short {
			continue
		}
		cmd := strings.Join(f[2:], " ")
		if strings.Contains(cmd, script) || strings.Contains(f[2], "claude") || strings.Contains(f[2], "codex") {
			return true
		}
	}
	return false
}

// retypeLauncher types ctrl-U and the launcher into the iTerm session on tty.
var retypeLauncher = func(tty, script string) error {
	as := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					tell s to write text ((character id 21) & %q)
					return "typed"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty, proc.ShellQuote(script))
	out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-e", as)
	if err == nil && strings.TrimSpace(string(out)) != "typed" {
		err = errors.New("no iTerm session on " + tty)
	}
	return err
}

// sleep is time.Sleep, stubbed in tests.
var sleep = time.Sleep

// launchShell runs every launcher: launchScript needs zsh's word splitting.
const launchShell = "/bin/zsh"

// checkLaunchShell says what's wrong before anything is changed or opened, since
// a launcher that can't run only shows up as a session that never starts.
// Stubbed in tests.
var checkLaunchShell = func() error {
	if _, err := os.Stat(launchShell); err != nil {
		return fmt.Errorf("sessions are started with zsh, and there's none at %s", launchShell)
	}
	return nil
}

// tmuxWindowArgs opens a background window on the launcher. Several words, so
// tmux runs them directly: one word would go through a shell, which splits a
// path with a space in it.
func tmuxWindowArgs(script string) []string {
	return []string{"new-window", "-d", "-P", "-F", "#{pane_tty}", launchShell, "-l", script}
}

// canOpenTabs: launch has somewhere to open a tab.
func canOpenTabs() bool { return os.Getenv("TMUX") != "" || inITerm() }

// execScript replaces this process with a launcher. Stubbed in tests.
var execScript = func(script string) error {
	return syscall.Exec(script, []string{script}, os.Environ())
}

// openLaunchTab opens a background tmux window running the launcher or, outside
// tmux (an iTerm tab there would land outside the session), an iTerm tab (at
// the end of the strip if iTerm's "new tabs at the end" is on), waits for its
// shell to draw something, then types ctrl-U (any stray input gone) and the
// launcher. The tab that had focus keeps it. Returns the new tab's tty.
var openLaunchTab = func(script string) (string, error) {
	if os.Getenv("TMUX") != "" {
		out, err := proc.RunBounded(5*time.Second, nil, "tmux", tmuxWindowArgs(script)...)
		tty := strings.TrimSpace(string(out))
		if err == nil && tty == "" {
			err = errors.New("tmux returned no tty")
		}
		return tty, err
	}
	as := fmt.Sprintf(`tell application "iTerm2"
	if (count of windows) = 0 then
		create window with default profile
		set s to current session of current window
	else
		tell current window
			set oldTab to current tab
			set newTab to (create tab with default profile)
			set s to current session of newTab
			select oldTab
		end tell
	end if
	repeat 50 times
		if length of (contents of s as text) > 0 then exit repeat
		delay 0.1
	end repeat
	delay 0.3
	tell s to write text ((character id 21) & %q)
	return tty of s
end tell`, proc.ShellQuote(script))
	out, err := proc.RunBounded(15*time.Second, nil, "osascript", "-e", as)
	tty := strings.TrimSpace(string(out))
	if err == nil && tty == "" {
		err = errors.New("iTerm returned no tty")
	}
	return tty, err
}

// sessionsNamed is the pids of running sessions called name: claude sessions
// whose session file says so (its pid checked against the file's start
// time), and codex sessions ccs launched (their --add-dir is the name's
// launch folder).
func sessionsNamed(name string) ([]int, error) {
	var pids []int
	for _, s := range scanSessions(nil) {
		if s.stat.name == name {
			pids = append(pids, s.pid)
		}
	}
	out, err := proc.PsAll()
	if err != nil {
		return pids, nil // claude's sessions are still known
	}
	marker := "--add-dir " + filepath.Join(state.LaunchDir(), name) + " " // ps shows arguments unquoted
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		cmd := strings.Join(f[2:], " ") + " "
		if !strings.Contains(cmd, "codex") || !strings.Contains(cmd, marker) {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil && !containsInt(pids, pid) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// closeByName closes the one running session called name.
func closeByName(name string) (string, error) {
	pids, err := sessionsNamed(name)
	if err != nil {
		return "", err
	}
	switch len(pids) {
	case 0:
		return "", fmt.Errorf("no running session named %s", name)
	case 1:
	default:
		return "", fmt.Errorf("%d running sessions are named %s (pids %v): close one by hand", len(pids), name, pids)
	}
	return closeSession(name, pids[0])
}

// closeSession stops pid (SIGTERM, as quitting does), confirms it exited,
// then closes its tab: the tmux pane, or the iTerm session on its tty, one
// osascript call per tab (closing several in one loop shifts the indexes).
// Stopping first means iTerm never asks whether to close a running job.
func closeSession(name string, pid int) (string, error) {
	tty := proc.TtyOf(pid)
	if tty == "" {
		return "", fmt.Errorf("%s (pid %d) has no terminal: an app or claude --bg started it, close it there", name, pid)
	}
	if err := proc.SignalPID(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return "", fmt.Errorf("couldn't stop %s (pid %d): %v", name, pid, err)
	}
	deadline := time.Now().Add(closeWait)
	for proc.PidAlive(pid) {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s (pid %d) still running %s after SIGTERM; its tab on %s is left open", name, pid, closeWait, tty)
		}
		sleep(100 * time.Millisecond)
	}
	if !closeTab(tty) {
		return "", fmt.Errorf("stopped %s (pid %d), but found no tab on %s to close", name, pid, tty)
	}
	return fmt.Sprintf("closed %s (pid %d, %s)", name, pid, tty), nil
}

// closeTab closes the tmux pane or iTerm session on tty.
var closeTab = realCloseTab

func realCloseTab(tty string) bool {
	if os.Getenv("TMUX") != "" {
		panes, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
		if target := tmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			_, err := proc.RunBounded(5*time.Second, nil, "tmux", "kill-pane", "-t", target)
			return err == nil
		}
		// A window launch opened runs the launcher itself, so it went with the session.
		if err == nil && !inITerm() {
			return true
		}
	}
	as := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					tell s to close
					return "closed"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty)
	out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-e", as)
	return err == nil && strings.TrimSpace(string(out)) == "closed"
}

// runLaunch is `ccs launch`.
func runLaunch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o launchOpts
	var briefFile string
	fs.StringVar(&o.name, "name", "", "the session's name (required)")
	fs.StringVar(&briefFile, "brief", "", "a file whose text is the session's first prompt")
	fs.StringVar(&o.resume, "resume", "", "a session id to resume instead (or as well)")
	fs.StringVar(&o.cwd, "cwd", "", "the folder to start in (default: this one)")
	fs.BoolVar(&o.codex, "codex", false, "start codex instead of claude")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs launch --name <name> (--brief <file> | --resume <id>) [--codex] [--cwd <dir>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.name == "" || (briefFile == "" && o.resume == "") || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	if briefFile != "" {
		data, err := os.ReadFile(briefFile)
		if err != nil {
			fmt.Fprintln(stderr, "ccs launch:", err)
			return 1
		}
		o.brief = strings.TrimRight(string(data), "\n")
	}
	l, err := launchSession(o)
	if err != nil {
		fmt.Fprintln(stderr, "ccs launch:", err)
		return 1
	}
	if l.thread != "" {
		fmt.Fprintf(stdout, "launched %s: pid %d, %s, codex thread %s\n", o.name, l.pid, l.tty, l.thread)
	} else {
		fmt.Fprintf(stdout, "launched %s: pid %d, %s\n", o.name, l.pid, l.tty)
	}
	return 0
}

// runClose is `ccs close`.
func runClose(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: ccs close <name>")
		return 2
	}
	msg, err := closeByName(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "ccs close:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// launchPrompt is ^X N's one-line box.
func launchPrompt(prompt, value string) textinput.Model {
	in := textinput.New()
	in.Prompt = prompt
	in.Width = 60
	in.SetValue(value)
	in.Focus()
	in.Cursor.SetMode(cursor.CursorStatic)
	return in
}

type launchDoneMsg struct {
	name string
	l    launched
	err  error
}

// launchCmd starts o off the UI goroutine: launch waits for the session.
func launchCmd(o launchOpts) tea.Cmd {
	return func() tea.Msg {
		l, err := launchSession(o)
		return launchDoneMsg{o.name, l, err}
	}
}

type closeDoneMsg struct {
	text string
	err  error
}

// closeCmd stops the session on pid and closes its tab, off the UI goroutine.
func closeCmd(name string, pid int) tea.Cmd {
	return func() tea.Msg {
		text, err := closeSession(name, pid)
		return closeDoneMsg{text, err}
	}
}

// tildePath is p with the home folder as ~.
func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

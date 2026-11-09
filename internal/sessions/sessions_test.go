package sessions

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
)

func TestCodexExecArgs(t *testing.T) {
	for _, c := range []struct {
		cmd  []string
		exec bool
		cd   string
	}{
		{[]string{"/usr/local/bin/codex", "exec", "do it"}, true, ""},
		{[]string{"codex", "-c", "model=x", "--cd", "/w", "exec", "-"}, true, "/w"},
		{[]string{"node", "/n/bin/codex.js", "e", "x"}, true, ""},
		{[]string{"/A/codex", "-c", "forced_login_method=chatgpt", "exec-server", "--remote", "x"}, false, ""},
		{[]string{"codex", "-c", "exec"}, false, ""}, // a flag's value, not the subcommand
		{[]string{"codex", "resume", "id"}, false, ""},
		{[]string{"zsh", "-c", "codex exec x"}, false, ""},
	} {
		if exec, cd := codexExecArgs(c.cmd); exec != c.exec || cd != c.cd {
			t.Errorf("%v: %v %q", c.cmd, exec, cd)
		}
	}
}

func TestLaunchOpensTabsInTmuxToo(t *testing.T) {
	t.Setenv("TMUX", "/tmp/s,1,0")
	if !CanOpenTabs() {
		t.Error("tmux can open a window")
	}
	t.Setenv("TMUX", "")
	if CanOpenTabs() {
		t.Error("nothing to open a tab in")
	}
}

func TestTmuxRunsTheLauncherDirectly(t *testing.T) {
	args := tmuxWindowArgs("/home/a b/launch.sh")
	// Everything after the options must be more than one word, or tmux hands it to a shell.
	i := slices.Index(args, "#{pane_tty}") + 1
	if got := args[i:]; len(got) < 2 || got[len(got)-1] != "/home/a b/launch.sh" {
		t.Errorf("%q", got)
	}
}

func TestCloseCountsAWindowGoneWithItsSessionAsClosed(t *testing.T) {
	t.Setenv("TMUX", "/tmp/s,1,0")
	for _, k := range []string{"TERM_PROGRAM", "LC_TERMINAL", "ITERM_SESSION_ID"} {
		t.Setenv(k, "")
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho '/dev/ttys1 other:0.0'\n"), 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	// Reads the real closeTab, which TestMain stubs.
	if !realCloseTab("/dev/ttys9") {
		t.Error("no pane on that tty in tmux: its window closed itself")
	}
}

func TestLaunchScript(t *testing.T) {
	dir := "/c/launch/n"
	got := launchScript(LaunchOpts{Name: "n", Brief: "do it", Cwd: "/w"}, "/bin/claude", dir)
	want := "#!/bin/zsh -l\ncd '/w' || exit 1\n[ -f ~/.claude.secret ] && source ~/.claude.secret\n" +
		`exec '/bin/claude' "$(cat '/c/launch/n/brief.md')" --name 'n' ${=CCS_CLAUDE_ARGS}` + "\n"
	if got != want {
		t.Errorf("the brief goes before CCS_CLAUDE_ARGS (its --channels takes every word after it):\n%s", got)
	}
	got = launchScript(LaunchOpts{Name: "n", Resume: "abc", Cwd: "/w"}, "/bin/claude", dir)
	if !strings.Contains(got, `exec '/bin/claude' --resume 'abc' --name 'n' ${=CCS_CLAUDE_ARGS}`) {
		t.Errorf("resume, no brief:\n%s", got)
	}
	got = launchScript(LaunchOpts{Name: "n", Brief: "x", Cwd: "/w", Codex: true}, "/bin/codex", dir)
	stateDir := proc.ShellQuote(filepath.Dir(state.SettingsPath))
	if !strings.Contains(got, `exec '/bin/codex' -s workspace-write -a on-request -c sandbox_workspace_write.network_access=true --add-dir '/c/launch/n' --add-dir `+stateDir+` "$(cat '/c/launch/n/brief.md')"`) || strings.Contains(got, "claude.secret") {
		t.Errorf("codex: the user's approval mode, network on, its launch folder (marking it) and ccs's state writable:\n%s", got)
	}
	got = launchScript(LaunchOpts{Name: "n", Resume: "r1", Cwd: "/w", Codex: true}, "/bin/codex", dir)
	if !strings.Contains(got, `exec '/bin/codex' resume 'r1' -s`) {
		t.Errorf("codex resume:\n%s", got)
	}
}

func TestParseSessionLink(t *testing.T) {
	for _, ok := range []string{"fix-login", "a", "A.b_c-1", strings.Repeat("a", 64)} {
		if name, err := parseSessionLink(linkPrefix + ok); err != nil || name != ok {
			t.Errorf("%q: got %q, %v", ok, name, err)
		}
	}
	for _, bad := range []string{
		"",
		"ccs://session/",
		"ccs://session",
		"ccs://other/name",
		"CCS://session/name",
		"http://session/name",
		" ccs://session/name",
		"ccs://user@session/name",
		"ccs://session:80/name",
		"ccs://session/a/b",
		"ccs://session/../a",
		"ccs://session/name/",
		"ccs://session/name?x=1",
		"ccs://session/name#x",
		"ccs://session/na%6De",
		"ccs://session/-rf",
		"ccs://session/.hidden",
		"ccs://session/na me",
		"ccs://session/name\n",
		"ccs://session/name\nx",
		"ccs://session/a'; touch /tmp/pwned; '",
		"ccs://session/a\"; do shell script \"x",
		"ccs://session/$(touch)",
		"ccs://session/`touch`",
		"ccs://session/é",
		"ccs://session/" + strings.Repeat("a", 65),
	} {
		if name, err := parseSessionLink(bad); err == nil {
			t.Errorf("%q: accepted as %q", bad, name)
		} else if strings.Contains(err.Error(), "pwned") {
			t.Errorf("%q: the error repeats the link: %v", bad, err)
		}
	}
}

// stubLink stubs what openLink reaches and records what it focuses.
func stubLink(t *testing.T, pids []int, tty string) (focused *[]string) {
	t.Helper()
	focused = &[]string{}
	oldNamed, oldTTY, oldFocus := namedSessions, proc.TtyOf, FocusLinkTTY
	t.Cleanup(func() { namedSessions, proc.TtyOf, FocusLinkTTY = oldNamed, oldTTY, oldFocus })
	namedSessions = func(string) ([]int, error) { return pids, nil }
	proc.TtyOf = func(int) string { return tty }
	FocusLinkTTY = func(tty string) error { *focused = append(*focused, tty); return nil }
	return focused
}

func TestOpenLink(t *testing.T) {
	focused := stubLink(t, []int{42}, "/dev/ttys042")
	msg, err := openLink(linkPrefix + "fix-login")
	if err != nil || !strings.Contains(msg, "fix-login") {
		t.Fatalf("got %q, %v", msg, err)
	}
	if len(*focused) != 1 || (*focused)[0] != "/dev/ttys042" {
		t.Fatalf("focused %v", *focused)
	}
}

// Every refusal says why and focuses nothing; none of them can start anything,
// since openLink's only action is focusLinkTTY.
func TestOpenLinkRefusals(t *testing.T) {
	for _, c := range []struct {
		name, link string
		pids       []int
		tty, want  string
	}{
		{"invalid", "ccs://session/a;b", []int{1}, "/dev/t", "isn't a valid ccs link"},
		{"not running", linkPrefix + "gone", nil, "/dev/t", "no session named gone is running"},
		{"ambiguous", linkPrefix + "twin", []int{1, 2}, "/dev/t", "2 running sessions are named twin"},
		{"no terminal", linkPrefix + "bg", []int{1}, "", "no terminal tab"},
	} {
		t.Run(c.name, func(t *testing.T) {
			focused := stubLink(t, c.pids, c.tty)
			_, err := openLink(c.link)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if len(*focused) != 0 {
				t.Fatalf("focused %v", *focused)
			}
		})
	}
}

// An invalid link must not even look at the sessions.
func TestOpenLinkInvalidLooksAtNothing(t *testing.T) {
	stubLink(t, nil, "")
	namedSessions = func(string) ([]int, error) { t.Fatal("looked up sessions"); return nil, nil }
	if _, err := openLink("ccs://session/$(x)"); err == nil {
		t.Fatal("accepted")
	}
}

func TestOpenLinkFocusFailure(t *testing.T) {
	stubLink(t, []int{1}, "/dev/t")
	FocusLinkTTY = func(string) error { return errors.New("it isn't in a tmux window") }
	if _, err := openLink(linkPrefix + "x"); err == nil || !strings.Contains(err.Error(), "couldn't switch to x: it isn't") {
		t.Fatalf("got %v", err)
	}
}

func TestRunLinkOpenExitCodes(t *testing.T) {
	stubLink(t, nil, "")
	t.Setenv("PATH", os.Getenv("PATH"))
	var out, errs bytes.Buffer
	if got := RunLink([]string{"open", linkPrefix + "gone"}, &out, &errs); got != 1 || !strings.Contains(errs.String(), "gone") {
		t.Fatalf("exit %d, stderr %q", got, errs.String())
	}
	if got := RunLink([]string{"open"}, &out, &errs); got != 2 {
		t.Fatalf("exit %d with no link", got)
	}
	if got := RunLink(nil, &out, &errs); got != 2 {
		t.Fatalf("exit %d with no arguments", got)
	}
}

func TestLatestTmuxClient(t *testing.T) {
	if got := latestTmuxClient("100 /dev/a\n300 /dev/b\n200 /dev/c\n"); got != "/dev/b" {
		t.Fatalf("got %q", got)
	}
	if got := latestTmuxClient(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestITermErrorPermission(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo 'execution error: Not authorized (-1743)' >&2; exit 1")
	_, err := cmd.Output()
	if got := iTermError(err); !strings.Contains(got.Error(), "Automation") {
		t.Fatalf("got %v", got)
	}
	cmd = exec.Command("sh", "-c", "exit 1")
	_, err = cmd.Output()
	if got := iTermError(err); !strings.Contains(got.Error(), "wouldn't switch") {
		t.Fatalf("got %v", got)
	}
}

func TestLinkTarget(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	os.WriteFile(filepath.Join(dir, "bin", "ccs"), nil, 0o755)
	if got := linkTarget(dir + "/Cellar/ccs/1.2.3/bin/ccs"); got != filepath.Join(dir, "bin", "ccs") {
		t.Fatalf("a keg should go through the stable symlink, got %q", got)
	}
	gone := t.TempDir() + "/Cellar/ccs/1.2.3/bin/ccs"
	if got := linkTarget(gone); got != gone {
		t.Fatalf("no symlink keeps the path, got %q", got)
	}
	if got := linkTarget("/usr/local/bin/ccs"); got != "/usr/local/bin/ccs" {
		t.Fatalf("got %q", got)
	}
}

func TestLinkAppletQuoting(t *testing.T) {
	src := linkApplet(`/odd "path"/ccs`)
	if !strings.Contains(src, `quoted form of theURL`) {
		t.Fatal("the URL isn't shell-quoted")
	}
	if strings.Count(src, "theURL") != 2 {
		t.Fatalf("the URL should only be received and quoted:\n%s", src)
	}
	if !strings.Contains(src, `(quoted form of "/odd \"path\"/ccs") & " link open "`) {
		t.Fatalf("the path isn't quoted:\n%s", src)
	}
}

// osacompile to a .scpt registers nothing, so this is safe on a real Mac.
func TestLinkAppletCompiles(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "a.applescript")
	os.WriteFile(src, []byte(linkApplet("/opt/homebrew/bin/ccs")), 0o600)
	if out, err := exec.Command("/usr/bin/osacompile", "-o", filepath.Join(dir, "a.scpt"), src).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func TestResumeInITermTabSkipsOutsideITerm(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	if handled, _ := ResumeInITermTab("/tmp", []string{"claude"}); handled {
		t.Error("should not open a tab outside iTerm")
	}
}

func TestResumeInTmuxWindowSkipsOutsideTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	if handled, _ := ResumeInTmuxWindow("/tmp", []string{"claude"}); handled {
		t.Error("should not open a window outside tmux")
	}
}

func TestReadLiveSessions(t *testing.T) {
	dir := t.TempDir()
	old := GetSessionsDir
	GetSessionsDir = func() string { return dir }
	defer func() { GetSessionsDir = old }()

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("self.json", fmt.Sprintf(`{"pid":%d,"sessionId":"alive"}`, os.Getpid()))
	write("dead.json", `{"pid":999999999,"sessionId":"dead"}`)
	write("junk.json", `not json`)

	live := ReadLiveSessions()
	if !live["alive"] || live["dead"] || len(live) != 1 {
		t.Errorf("want only 'alive', got %v", live)
	}
}

func TestTmuxPaneForTTY(t *testing.T) {
	panes := "/dev/ttys001 main:0.0\n/dev/ttys007 work:2.1\n"
	if got := TmuxPaneForTTY(panes, "/dev/ttys007"); got != "work:2.1" {
		t.Errorf("got %q", got)
	}
	if got := TmuxPaneForTTY(panes, "/dev/ttys999"); got != "" {
		t.Errorf("unknown tty should give no target, got %q", got)
	}
}

func TestSameStart(t *testing.T) {
	actual := time.Date(2026, 9, 23, 16, 52, 33, 0, time.UTC).In(time.FixedZone("NPT", 5*3600+45*60))
	if match, checked := sameStart("Wed Sep 23 16:52:33 2026", actual); !match || !checked {
		t.Error("same instant in different zones should match")
	}
	if match, _ := sameStart("Wed Sep 23 10:00:00 2026", actual); match {
		t.Error("a different start time means a recycled pid")
	}
	for _, c := range [][2]any{{"", actual}, {"Wed Sep 23 16:52:33 2026", time.Time{}}} {
		if match, checked := sameStart(c[0].(string), c[1].(time.Time)); !match || checked {
			t.Error("unknown start on either side must not hide a live session, and isn't a check")
		}
	}
}

func TestLiveSessionsDropsRecycledPid(t *testing.T) {
	dir := t.TempDir()
	old := GetSessionsDir
	GetSessionsDir = func() string { return dir }
	defer func() { GetSessionsDir = old }()
	oldPS := proc.ProcessStartTimes
	defer func() { proc.ProcessStartTimes = oldPS }()
	proc.ProcessStartTimes = func(map[string]int) map[int]time.Time {
		return map[int]time.Time{os.Getpid(): time.Date(2026, 9, 23, 16, 52, 33, 0, time.UTC)}
	}
	write := func(name, id, start string) {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"procStart":%q}`, os.Getpid(), id, start)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", "current", "Wed Sep 23 16:52:33 2026")
	write("b.json", "stale", "Mon Sep 21 09:00:00 2026") // same pid, earlier process
	live := ReadLiveSessions()
	if !live["current"] || live["stale"] {
		t.Errorf("recycled pid should not count as live: %v", live)
	}
}

func TestTmuxCwdHashIsEscaped(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "tmux")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TMUX", "fake")
	if handled, err := ResumeInTmuxWindow("/tmp/#(id)", []string{"claude"}); !handled || err != nil {
		t.Fatal(handled, err)
	}
	got, _ := os.ReadFile(log)
	if !strings.Contains(string(got), "/tmp/##(id)") {
		t.Errorf("tmux -c must get '#' escaped, got args:\n%s", got)
	}
}

func TestTabResultOnlyTimeoutMeansMaybeOpened(t *testing.T) {
	if h, err := tabResult(nil, nil); !h || err != nil {
		t.Error("success is handled")
	}
	if h, err := tabResult(nil, fmt.Errorf("osascript: %w", proc.ErrTimedOut)); !h || err == nil {
		t.Error("a timeout is handled with an error: it may have opened")
	}
	if h, _ := tabResult(nil, fmt.Errorf("exit status 1")); h {
		t.Error("a plain failure must fall back to resuming in place")
	}
	if _, err := proc.RunBounded(50*time.Millisecond, nil, "sleep", "5"); !errors.Is(err, proc.ErrTimedOut) {
		t.Errorf("runBounded should report its deadline, got %v", err)
	}
}

func TestStaleSessionFileWithOtherPidDoesNotHideLiveOne(t *testing.T) {
	dir := t.TempDir()
	old := GetSessionsDir
	GetSessionsDir = func() string { return dir }
	defer func() { GetSessionsDir = old }()
	oldPS := proc.ProcessStartTimes
	defer func() { proc.ProcessStartTimes = oldPS }()
	me := os.Getpid()
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	proc.ProcessStartTimes = func(map[string]int) map[int]time.Time {
		return map[int]time.Time{me: start, 1: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	write := func(name string, pid int, when string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"S","procStart":%q}`, pid, when)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("0-live.json", me, "Fri Sep 25 10:00:00 2026")
	write("9-stale.json", 1, "Mon Sep 21 09:00:00 2026") // crashed claude's leftover; pid 1 recycled
	if pid := LiveSessionPIDs()["S"]; pid != me {
		t.Errorf("live session hidden by a stale file: got pid %d", pid)
	}
}

func TestInITermThroughTmux(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "tmux") // tmux overrides it inside its panes
	if InITerm() {
		t.Error("tmux alone isn't iTerm")
	}
	t.Setenv("LC_TERMINAL", "iTerm2") // what iTerm sets, and tmux passes through
	if !InITerm() {
		t.Error("tmux inside iTerm should still find iTerm tabs")
	}
}

func TestFindHostOfThisProcess(t *testing.T) {
	// The test binary has a terminal locally and none in CI: either way an
	// answer, never a hang or a panic.
	_ = findHost(os.Getpid())
	if h := findHost(-1); h != "" {
		t.Errorf("no such process is treated as a terminal: %q", h)
	}
}

func TestListAgentsKeepsBackgroundOnes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "claude"), []byte(`#!/bin/sh
echo '[{"kind":"interactive","sessionId":"i1","pid":1},{"kind":"background","id":"k3x9","state":"blocked","sessionId":"b1"},{"kind":"background","id":"zz","state":"done"}]'
`), 0o755)
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	defer func(f func() (map[string]BgSession, error)) { ListAgents = f }(ListAgents)
	ListAgents = realListAgents
	got, err := ListAgents()
	if err != nil || len(got) != 1 || got["b1"] != (BgSession{"k3x9", "blocked"}) {
		t.Errorf("only background sessions with a session id: %v %v", got, err)
	}
}

// sessionsFixture points ccs at a temporary sessions folder; write adds a
// live session file for pid called name.
func sessionsFixture(t *testing.T) (write func(file string, pid int, name string)) {
	t.Helper()
	dir := t.TempDir()
	old, oldStarts := GetSessionsDir, proc.ProcessStartTimes
	GetSessionsDir = func() string { return dir }
	proc.ProcessStartTimes = func(map[string]int) map[int]time.Time { return nil } // unknown: not checked
	t.Cleanup(func() { GetSessionsDir, proc.ProcessStartTimes = old, oldStarts })
	return func(file string, pid int, name string) {
		os.WriteFile(filepath.Join(dir, file), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"id-%s-%d","name":%q}`, pid, name, pid, name)), 0o600)
	}
}

func stubLaunch(t *testing.T) {
	t.Helper()
	t.Setenv("ITERM_SESSION_ID", "w0t0p0")
	oldBin, oldWait := LookBin, LaunchWait
	LookBin = func(name string) (string, error) { return "/bin/" + name, nil }
	LaunchWait = 300 * time.Millisecond
	oldTab, oldRetype, oldRetypeAt, oldRunning := OpenLaunchTab, RetypeLauncher, LaunchRetype, LauncherRunning
	t.Cleanup(func() {
		LookBin, LaunchWait, OpenLaunchTab, RetypeLauncher, LaunchRetype, LauncherRunning = oldBin, oldWait, oldTab, oldRetype, oldRetypeAt, oldRunning
	})
}

func TestLaunchWaitsForTheSession(t *testing.T) {
	write := sessionsFixture(t)
	stubLaunch(t)
	var script string
	OpenLaunchTab = func(s string) (string, error) {
		script = s
		write("1.json", os.Getpid(), "worker") // claude starts and writes its session file
		return "/dev/ttys042", nil
	}
	cwd := t.TempDir()
	l, err := LaunchSession(LaunchOpts{Name: "worker", Brief: "fix the `thing` $NOW", Cwd: cwd})
	if err != nil || l.Pid != os.Getpid() || l.Tty != "/dev/ttys042" {
		t.Fatalf("launched: %+v, %v", l, err)
	}
	if script != filepath.Join(state.LaunchDir(), "worker", "launch.sh") {
		t.Errorf("the tab runs the launcher in its own folder: %s", script)
	}
	brief, _ := os.ReadFile(filepath.Join(state.LaunchDir(), "worker", "brief.md"))
	body, _ := os.ReadFile(script)
	if string(brief) != "fix the `thing` $NOW" || !strings.Contains(string(body), "cd '"+cwd+"'") {
		t.Errorf("brief kept as written, launcher starts in cwd: %q\n%s", brief, body)
	}

	if _, err := LaunchSession(LaunchOpts{Name: "worker", Brief: "again", Cwd: cwd}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a name already running is refused: %v", err)
	}
	for _, bad := range []string{"", "a b", "x;rm", "-n", strings.Repeat("a", 65)} {
		if _, err := LaunchSession(LaunchOpts{Name: bad, Brief: "x", Cwd: cwd}); err == nil {
			t.Errorf("name %q should be refused", bad)
		}
	}
	if _, err := LaunchSession(LaunchOpts{Name: "ok", Cwd: cwd}); err == nil {
		t.Error("nothing to start: no brief and nothing to resume")
	}
}

func TestLaunchTypesTheLauncherAgainWhenTheShellLostIt(t *testing.T) {
	write := sessionsFixture(t)
	stubLaunch(t)
	LaunchWait, LaunchRetype = time.Second, 50*time.Millisecond
	OpenLaunchTab = func(string) (string, error) { return "/dev/ttys044", nil } // typed before the shell read input: lost
	running := false
	LauncherRunning = func(tty, script string) bool { return running }
	typed := 0
	RetypeLauncher = func(tty, script string) error {
		typed++
		if tty != "/dev/ttys044" || !strings.HasSuffix(script, "launch.sh") {
			t.Errorf("typed into %s: %s", tty, script)
		}
		running = true
		write("1.json", os.Getpid(), "late") // this time it starts
		return nil
	}
	if l, err := LaunchSession(LaunchOpts{Name: "late", Brief: "x", Cwd: t.TempDir()}); err != nil || l.Pid != os.Getpid() || typed != 1 {
		t.Errorf("typed again once and started: %+v %v, typed %d", l, err, typed)
	}

	// Something runs the launcher already (claude at a trust prompt): never typed into.
	OpenLaunchTab = func(string) (string, error) { return "/dev/ttys045", nil }
	LauncherRunning = func(string, string) bool { return true }
	typed = 0
	LaunchWait = 200 * time.Millisecond
	LaunchSession(LaunchOpts{Name: "stuck", Brief: "x", Cwd: t.TempDir()})
	if typed != 0 {
		t.Errorf("typed into a tab that runs it: %d", typed)
	}
}

func TestLaunchFailsLoudlyWhenNothingStarts(t *testing.T) {
	sessionsFixture(t)
	stubLaunch(t)
	OpenLaunchTab = func(string) (string, error) { return "/dev/ttys043", nil }
	_, err := LaunchSession(LaunchOpts{Name: "ghost", Brief: "x", Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "didn't start") || !strings.Contains(err.Error(), "ttys043") {
		t.Errorf("says it didn't start and where to look: %v", err)
	}
	OpenLaunchTab = func(string) (string, error) { return "", errors.New("Automation denied") }
	if _, err := LaunchSession(LaunchOpts{Name: "ghost2", Brief: "x", Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "Automation denied") {
		t.Errorf("a tab that couldn't open is an error: %v", err)
	}
	t.Setenv("ITERM_SESSION_ID", "")
	if _, err := LaunchSession(LaunchOpts{Name: "ghost3", Brief: "x", Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "iTerm") {
		t.Errorf("outside iTerm: %v", err)
	}
}

func TestCodexSessionsFoundByTheirLaunchFolder(t *testing.T) {
	sessionsFixture(t)
	old := proc.PsAll
	t.Cleanup(func() { proc.PsAll = old })
	d := filepath.Join(state.LaunchDir(), "cx")
	proc.PsAll = func() (string, error) {
		return "  101 ttys001 /bin/codex -s workspace-write -a never --add-dir " + d + " do it\n" +
			"  102 ttys002 /bin/codex --add-dir " + d + "-other x\n" + // another name's folder
			"  103 ttys003 vim " + d + "/brief.md\n", nil
	}
	if pids, _ := SessionsNamed("cx"); len(pids) != 1 || pids[0] != 101 {
		t.Errorf("only codex with that launch folder: %v", pids)
	}
}

func TestCloseStopsThenClosesTheTab(t *testing.T) {
	write := sessionsFixture(t)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { child.Wait(); close(exited) }()
	pid := child.Process.Pid
	write("a.json", pid, "worker")
	oldTTY, oldSig, oldTab, oldWait := proc.TtyOf, proc.SignalPID, CloseTab, CloseWait
	t.Cleanup(func() {
		proc.TtyOf, proc.SignalPID, CloseTab, CloseWait = oldTTY, oldSig, oldTab, oldWait
		child.Process.Kill()
	})
	proc.TtyOf = func(int) string { return "/dev/ttys044" }
	CloseWait = 2 * time.Second
	var closed []string
	CloseTab = func(tty string) bool {
		if proc.PidAlive(pid) {
			select {
			case <-exited:
			default:
				t.Error("the tab is closed only after the process has exited")
			}
		}
		closed = append(closed, tty)
		return true
	}
	proc.SignalPID = func(p int, sig syscall.Signal) error {
		if p != pid {
			t.Fatalf("signalled %d, not the session's %d", p, pid)
		}
		err := syscall.Kill(p, sig)
		<-exited // reaped, so pidAlive sees it gone
		return err
	}
	msg, err := CloseByName("worker")
	if err != nil || !strings.Contains(msg, "closed worker") || len(closed) != 1 || closed[0] != "/dev/ttys044" {
		t.Fatalf("closed: %q, %v, tabs %v", msg, err, closed)
	}
}

func TestCloseRefusals(t *testing.T) {
	write := sessionsFixture(t)
	if _, err := CloseByName("nobody"); err == nil || !strings.Contains(err.Error(), "no running session") {
		t.Errorf("unknown name: %v", err)
	}
	write("a.json", os.Getpid(), "twin")
	write("b.json", os.Getppid(), "twin")
	if _, err := CloseByName("twin"); err == nil || !strings.Contains(err.Error(), "2 running sessions") {
		t.Errorf("two sessions with one name are left alone: %v", err)
	}
	write("c.json", os.Getpid(), "stuck")
	oldTTY, oldSig, oldTab, oldWait := proc.TtyOf, proc.SignalPID, CloseTab, CloseWait
	t.Cleanup(func() {
		proc.TtyOf, proc.SignalPID, CloseTab, CloseWait = oldTTY, oldSig, oldTab, oldWait
	})
	if _, err := CloseSession("stuck", os.Getpid()); err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Errorf("no tty (an app or claude --bg): %v", err)
	}
	proc.TtyOf = func(int) string { return "/dev/ttys045" }
	proc.SignalPID = func(int, syscall.Signal) error { return nil } // ignores it
	CloseWait = 200 * time.Millisecond
	tabClosed := false
	CloseTab = func(string) bool { tabClosed = true; return true }
	if _, err := CloseSession("stuck", os.Getpid()); err == nil || !strings.Contains(err.Error(), "still running") || tabClosed {
		t.Errorf("a process that won't exit keeps its tab: %v, closed %v", err, tabClosed)
	}
}

func readLog(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestFocusLinkTmuxWithoutEnv(t *testing.T) {
	t.Setenv("TMUX", "")
	log := fakeFocusTools(t, "/dev/ttys1 main:0.1\n", "100 /dev/old\n200 /dev/client\n", "found", true)
	if err := RealFocusLinkTTY("/dev/ttys1"); err != nil {
		t.Fatal(err)
	}
	got := readLog(log)
	if !strings.Contains(got, "tmux switch-client -c /dev/client -t main:0.1") {
		t.Fatalf("no switch: %s", got)
	}
	if !strings.Contains(got, `tty of s is "/dev/client"`) || !strings.Contains(got, "activate") {
		t.Fatalf("didn't raise the client's iTerm tab: %s", got)
	}
}

func TestFocusLinkTmuxDetached(t *testing.T) {
	log := fakeFocusTools(t, "/dev/ttys1 main:0.1\n", "", "", false)
	if err := RealFocusLinkTTY("/dev/ttys1"); err == nil || !strings.Contains(err.Error(), "isn't attached") {
		t.Fatalf("got %v", err)
	}
	if got := readLog(log); got != "" {
		t.Fatalf("ran %s", got)
	}
}

func TestFocusLinkITermWithoutEnv(t *testing.T) {
	log := fakeFocusTools(t, "/dev/other a:0.0\n", "", "found", true)
	if err := RealFocusLinkTTY("/dev/ttys1"); err != nil {
		t.Fatal(err)
	}
	if got := readLog(log); !strings.Contains(got, `tty of s is "/dev/ttys1"`) || !strings.Contains(got, "activate") {
		t.Fatalf("got %s", got)
	}
}

// Naming iTerm in AppleScript starts it, so with iTerm closed it's never named.
func TestFocusLinkNeverStartsITerm(t *testing.T) {
	log := fakeFocusTools(t, "", "", "found", false)
	if err := RealFocusLinkTTY("/dev/ttys1"); err == nil {
		t.Fatal("expected a refusal")
	}
	if got := readLog(log); got != "" {
		t.Fatalf("ran %s", got)
	}
}

// fakeAppTools fakes osacompile, plutil, codesign and lsregister, and points the
// app at a temp folder, so tests never register a real handler. plutil keeps
// each key in a text file beside the plist, and reads them back.
func fakeAppTools(t *testing.T) (log, app string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	app = filepath.Join(dir, "Applications", "CCS Links.app")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	write := func(name, body string) {
		os.WriteFile(filepath.Join(bin, name), []byte("#!"+sh+"\nPATH=/usr/bin:/bin\necho \""+name+" $*\" >> "+log+"\n"+body), 0o755)
	}
	write("osacompile", `mkdir -p "$2/Contents"; : > "$2/Contents/Info.plist"`+"\n")
	write("plutil", `if [ "$1" = -extract ]; then cat "$6.$2" 2>/dev/null || exit 1; else printf '%s' "$4" > "$5.$2"; fi`+"\n")
	write("codesign", "")
	write("lsregister", "")
	t.Setenv("PATH", bin)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	oldPath, oldLs := LinkAppPath, Lsregister
	LinkAppPath = func() string { return app }
	Lsregister = filepath.Join(bin, "lsregister")
	t.Cleanup(func() { LinkAppPath, Lsregister = oldPath, oldLs })
	return log, app
}

func TestInstallLinkApp(t *testing.T) {
	log, app := fakeAppTools(t)
	if err := InstallLinkApp("/opt/homebrew/bin/ccs"); err != nil {
		t.Fatal(err)
	}
	got := readLog(log)
	for _, want := range []string{"osacompile -o", "plutil -replace CFBundleURLTypes -json", `"CFBundleURLSchemes":["ccs"]`, "codesign --force --sign -", "lsregister -f " + app} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "codesign") < strings.LastIndex(got, "plutil -replace") {
		t.Error("signed before the plist was finished")
	}
	if v := LinkPlistValue(app, LinkTargetKey); v != "/opt/homebrew/bin/ccs" {
		t.Errorf("target %q", v)
	}
}

// An app already right stays untouched, or macOS would ask for permission again.
func TestInstallLinkAppKeepsCurrent(t *testing.T) {
	log, _ := fakeAppTools(t)
	if err := InstallLinkApp("/opt/homebrew/bin/ccs"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	if err := InstallLinkApp("/opt/homebrew/bin/ccs"); err != nil {
		t.Fatal(err)
	}
	got := readLog(log)
	if strings.Contains(got, "osacompile") || strings.Contains(got, "codesign") {
		t.Fatalf("rebuilt:\n%s", got)
	}
	if !strings.Contains(got, "lsregister -f") {
		t.Fatalf("not registered again:\n%s", got)
	}
}

func TestInstallLinkAppRebuildsForNewTarget(t *testing.T) {
	log, app := fakeAppTools(t)
	if err := InstallLinkApp("/old/ccs"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	if err := InstallLinkApp("/new/ccs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readLog(log), "osacompile") {
		t.Fatal("not rebuilt")
	}
	if v := LinkPlistValue(app, LinkTargetKey); v != "/new/ccs" {
		t.Fatalf("target %q", v)
	}
}

func TestInstallLinkAppRefusesOthersApp(t *testing.T) {
	log, app := fakeAppTools(t)
	os.MkdirAll(filepath.Join(app, "Contents"), 0o755)
	os.WriteFile(filepath.Join(app, "Contents", "Info.plist.CFBundleIdentifier"), []byte("com.someone.else"), 0o644)
	if err := InstallLinkApp("/x/ccs"); err == nil || !strings.Contains(err.Error(), "isn't ccs's") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(readLog(log), "osacompile") {
		t.Fatal("overwrote it")
	}
	var out, errs bytes.Buffer
	if got := LinkUninstall(&out, &errs); got != 1 {
		t.Fatalf("exit %d", got)
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatal("uninstall removed someone else's app")
	}
}

func TestInstallLinkAppRefusesUnquotablePath(t *testing.T) {
	log, _ := fakeAppTools(t)
	if err := InstallLinkApp("/bad\npath/ccs"); err == nil {
		t.Fatal("accepted")
	}
	if got := readLog(log); got != "" {
		t.Fatalf("ran %s", got)
	}
}

func TestLinkUninstall(t *testing.T) {
	log, app := fakeAppTools(t)
	if err := InstallLinkApp("/x/ccs"); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if got := LinkUninstall(&out, &errs); got != 0 {
		t.Fatalf("exit %d: %s", got, errs.String())
	}
	if _, err := os.Stat(app); err == nil {
		t.Fatal("still there")
	}
	if !strings.Contains(readLog(log), "lsregister -u "+app) {
		t.Fatal("not unregistered")
	}
	if got := LinkUninstall(&out, &errs); got != 0 {
		t.Fatalf("exit %d with nothing installed", got)
	}
}

func TestLinkInstallReportsAnotherHandler(t *testing.T) {
	_, app := fakeAppTools(t)
	oldHandler, oldSleep := UrlHandlerApp, Sleep
	t.Cleanup(func() { UrlHandlerApp, Sleep = oldHandler, oldSleep })
	Sleep = func(time.Duration) {}
	UrlHandlerApp = func() string { return "/Applications/Other.app" }
	var out, errs bytes.Buffer
	if got := LinkInstall(&out, &errs); got != 1 || !strings.Contains(errs.String(), "Other.app") {
		t.Fatalf("exit %d, %q", got, errs.String())
	}
	UrlHandlerApp = func() string { return app }
	if got := LinkInstall(&out, &errs); got != 0 || !strings.Contains(out.String(), "now open with") {
		t.Fatalf("exit %d, %q", got, out.String())
	}
	out.Reset()
	if got := LinkStatus(&out); got != 0 || !strings.Contains(out.String(), "which runs") {
		t.Fatalf("exit %d, %q", got, out.String())
	}
	UrlHandlerApp = func() string { return "" }
	if got := LinkStatus(&out); got != 1 {
		t.Fatalf("exit %d with no handler", got)
	}
}

func TestRefreshLinkApp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("refresh only runs on macOS")
	}
	oldVersion := state.Version
	state.Version = "1.0.0" // the running version, which refreshLinkApp checks
	t.Cleanup(func() { state.Version = oldVersion })
	log, app := fakeAppTools(t)

	RefreshLinkApp() // nothing installed: installs nothing
	if got := readLog(log); strings.Contains(got, "osacompile") {
		t.Fatalf("installed one unasked:\n%s", got)
	}

	target := filepath.Join(t.TempDir(), "ccs")
	os.WriteFile(target, nil, 0o755)
	if err := InstallLinkApp(target); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	RefreshLinkApp() // current and its ccs exists: left alone, as across a brew upgrade
	if got := readLog(log); strings.Contains(got, "osacompile") {
		t.Fatalf("rebuilt a current app:\n%s", got)
	}

	os.WriteFile(filepath.Join(app, "Contents", "Info.plist."+LinkVersionKey), []byte("0"), 0o644)
	RefreshLinkApp() // an older handler is rebuilt
	if got := readLog(log); !strings.Contains(got, "osacompile") {
		t.Fatalf("not rebuilt:\n%s", got)
	}
}

// fakeFocusTools puts a fake tmux and osascript on PATH, and nothing else.
func fakeFocusTools(t *testing.T, panes, clients, osascript string, iterm bool) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!"+sh+"\ncase \"$1\" in\nlist-panes) printf '"+panes+"';;\nlist-clients) printf '"+clients+"';;\n*) echo \"tmux $*\" >> "+log+";;\nesac\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "osascript"), []byte("#!"+sh+"\necho \"osascript $*\" >> "+log+"\nprintf '"+osascript+"'\n"), 0o755)
	t.Setenv("PATH", dir)
	old := ITermRunning
	ITermRunning = func() bool { return iterm }
	t.Cleanup(func() { ITermRunning = old })
	return log
}

func TestFocusLinkITermNoSuchTab(t *testing.T) {
	fakeFocusTools(t, "", "", "", true)
	if err := RealFocusLinkTTY("/dev/ttys1"); err == nil || !strings.Contains(err.Error(), "isn't in a tmux window or an iTerm tab") {
		t.Fatalf("got %v", err)
	}
}

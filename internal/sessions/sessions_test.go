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

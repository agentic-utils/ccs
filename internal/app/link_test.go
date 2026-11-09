package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

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
	oldNamed, oldTTY, oldFocus := namedSessions, ttyOf, focusLinkTTY
	t.Cleanup(func() { namedSessions, ttyOf, focusLinkTTY = oldNamed, oldTTY, oldFocus })
	namedSessions = func(string) ([]int, error) { return pids, nil }
	ttyOf = func(int) string { return tty }
	focusLinkTTY = func(tty string) error { *focused = append(*focused, tty); return nil }
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
	focusLinkTTY = func(string) error { return errors.New("it isn't in a tmux window") }
	if _, err := openLink(linkPrefix + "x"); err == nil || !strings.Contains(err.Error(), "couldn't switch to x: it isn't") {
		t.Fatalf("got %v", err)
	}
}

func TestRunLinkOpenExitCodes(t *testing.T) {
	stubLink(t, nil, "")
	t.Setenv("PATH", os.Getenv("PATH"))
	var out, errs bytes.Buffer
	if got := runLink([]string{"open", linkPrefix + "gone"}, &out, &errs); got != 1 || !strings.Contains(errs.String(), "gone") {
		t.Fatalf("exit %d, stderr %q", got, errs.String())
	}
	if got := runLink([]string{"open"}, &out, &errs); got != 2 {
		t.Fatalf("exit %d with no link", got)
	}
	if got := runLink(nil, &out, &errs); got != 2 {
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
	old := iTermRunning
	iTermRunning = func() bool { return iterm }
	t.Cleanup(func() { iTermRunning = old })
	return log
}

func readLog(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestFocusLinkTmuxWithoutEnv(t *testing.T) {
	t.Setenv("TMUX", "")
	log := fakeFocusTools(t, "/dev/ttys1 main:0.1\n", "100 /dev/old\n200 /dev/client\n", "found", true)
	if err := realFocusLinkTTY("/dev/ttys1"); err != nil {
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
	if err := realFocusLinkTTY("/dev/ttys1"); err == nil || !strings.Contains(err.Error(), "isn't attached") {
		t.Fatalf("got %v", err)
	}
	if got := readLog(log); got != "" {
		t.Fatalf("ran %s", got)
	}
}

func TestFocusLinkITermWithoutEnv(t *testing.T) {
	log := fakeFocusTools(t, "/dev/other a:0.0\n", "", "found", true)
	if err := realFocusLinkTTY("/dev/ttys1"); err != nil {
		t.Fatal(err)
	}
	if got := readLog(log); !strings.Contains(got, `tty of s is "/dev/ttys1"`) || !strings.Contains(got, "activate") {
		t.Fatalf("got %s", got)
	}
}

func TestFocusLinkITermNoSuchTab(t *testing.T) {
	fakeFocusTools(t, "", "", "", true)
	if err := realFocusLinkTTY("/dev/ttys1"); err == nil || !strings.Contains(err.Error(), "isn't in a tmux window or an iTerm tab") {
		t.Fatalf("got %v", err)
	}
}

// Naming iTerm in AppleScript starts it, so with iTerm closed it's never named.
func TestFocusLinkNeverStartsITerm(t *testing.T) {
	log := fakeFocusTools(t, "", "", "found", false)
	if err := realFocusLinkTTY("/dev/ttys1"); err == nil {
		t.Fatal("expected a refusal")
	}
	if got := readLog(log); got != "" {
		t.Fatalf("ran %s", got)
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
	oldPath, oldLs := linkAppPath, lsregister
	linkAppPath = func() string { return app }
	lsregister = filepath.Join(bin, "lsregister")
	t.Cleanup(func() { linkAppPath, lsregister = oldPath, oldLs })
	return log, app
}

func TestInstallLinkApp(t *testing.T) {
	log, app := fakeAppTools(t)
	if err := installLinkApp("/opt/homebrew/bin/ccs"); err != nil {
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
	if v := linkPlistValue(app, linkTargetKey); v != "/opt/homebrew/bin/ccs" {
		t.Errorf("target %q", v)
	}
}

// An app already right stays untouched, or macOS would ask for permission again.
func TestInstallLinkAppKeepsCurrent(t *testing.T) {
	log, _ := fakeAppTools(t)
	if err := installLinkApp("/opt/homebrew/bin/ccs"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	if err := installLinkApp("/opt/homebrew/bin/ccs"); err != nil {
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
	if err := installLinkApp("/old/ccs"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	if err := installLinkApp("/new/ccs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readLog(log), "osacompile") {
		t.Fatal("not rebuilt")
	}
	if v := linkPlistValue(app, linkTargetKey); v != "/new/ccs" {
		t.Fatalf("target %q", v)
	}
}

func TestInstallLinkAppRefusesOthersApp(t *testing.T) {
	log, app := fakeAppTools(t)
	os.MkdirAll(filepath.Join(app, "Contents"), 0o755)
	os.WriteFile(filepath.Join(app, "Contents", "Info.plist.CFBundleIdentifier"), []byte("com.someone.else"), 0o644)
	if err := installLinkApp("/x/ccs"); err == nil || !strings.Contains(err.Error(), "isn't ccs's") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(readLog(log), "osacompile") {
		t.Fatal("overwrote it")
	}
	var out, errs bytes.Buffer
	if got := linkUninstall(&out, &errs); got != 1 {
		t.Fatalf("exit %d", got)
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatal("uninstall removed someone else's app")
	}
}

func TestInstallLinkAppRefusesUnquotablePath(t *testing.T) {
	log, _ := fakeAppTools(t)
	if err := installLinkApp("/bad\npath/ccs"); err == nil {
		t.Fatal("accepted")
	}
	if got := readLog(log); got != "" {
		t.Fatalf("ran %s", got)
	}
}

func TestLinkUninstall(t *testing.T) {
	log, app := fakeAppTools(t)
	if err := installLinkApp("/x/ccs"); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if got := linkUninstall(&out, &errs); got != 0 {
		t.Fatalf("exit %d: %s", got, errs.String())
	}
	if _, err := os.Stat(app); err == nil {
		t.Fatal("still there")
	}
	if !strings.Contains(readLog(log), "lsregister -u "+app) {
		t.Fatal("not unregistered")
	}
	if got := linkUninstall(&out, &errs); got != 0 {
		t.Fatalf("exit %d with nothing installed", got)
	}
}

func TestLinkInstallReportsAnotherHandler(t *testing.T) {
	_, app := fakeAppTools(t)
	oldHandler, oldSleep := urlHandlerApp, sleep
	t.Cleanup(func() { urlHandlerApp, sleep = oldHandler, oldSleep })
	sleep = func(time.Duration) {}
	urlHandlerApp = func() string { return "/Applications/Other.app" }
	var out, errs bytes.Buffer
	if got := linkInstall(&out, &errs); got != 1 || !strings.Contains(errs.String(), "Other.app") {
		t.Fatalf("exit %d, %q", got, errs.String())
	}
	urlHandlerApp = func() string { return app }
	if got := linkInstall(&out, &errs); got != 0 || !strings.Contains(out.String(), "now open with") {
		t.Fatalf("exit %d, %q", got, out.String())
	}
	out.Reset()
	if got := linkStatus(&out); got != 0 || !strings.Contains(out.String(), "which runs") {
		t.Fatalf("exit %d, %q", got, out.String())
	}
	urlHandlerApp = func() string { return "" }
	if got := linkStatus(&out); got != 1 {
		t.Fatalf("exit %d with no handler", got)
	}
}

func TestRefreshLinkApp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("refresh only runs on macOS")
	}
	oldVersion := version
	version = "1.0.0"
	t.Cleanup(func() { version = oldVersion })
	log, app := fakeAppTools(t)

	refreshLinkApp() // nothing installed: installs nothing
	if got := readLog(log); strings.Contains(got, "osacompile") {
		t.Fatalf("installed one unasked:\n%s", got)
	}

	target := filepath.Join(t.TempDir(), "ccs")
	os.WriteFile(target, nil, 0o755)
	if err := installLinkApp(target); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	refreshLinkApp() // current and its ccs exists: left alone, as across a brew upgrade
	if got := readLog(log); strings.Contains(got, "osacompile") {
		t.Fatalf("rebuilt a current app:\n%s", got)
	}

	os.WriteFile(filepath.Join(app, "Contents", "Info.plist."+linkVersionKey), []byte("0"), 0o644)
	refreshLinkApp() // an older handler is rebuilt
	if got := readLog(log); !strings.Contains(got, "osacompile") {
		t.Fatalf("not rebuilt:\n%s", got)
	}
}

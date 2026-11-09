package tui

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
)

func readLog(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestFocusLinkTmuxWithoutEnv(t *testing.T) {
	t.Setenv("TMUX", "")
	log := fakeFocusTools(t, "/dev/ttys1 main:0.1\n", "100 /dev/old\n200 /dev/client\n", "found", true)
	if err := sessions.RealFocusLinkTTY("/dev/ttys1"); err != nil {
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
	if err := sessions.RealFocusLinkTTY("/dev/ttys1"); err == nil || !strings.Contains(err.Error(), "isn't attached") {
		t.Fatalf("got %v", err)
	}
	if got := readLog(log); got != "" {
		t.Fatalf("ran %s", got)
	}
}

func TestFocusLinkITermWithoutEnv(t *testing.T) {
	log := fakeFocusTools(t, "/dev/other a:0.0\n", "", "found", true)
	if err := sessions.RealFocusLinkTTY("/dev/ttys1"); err != nil {
		t.Fatal(err)
	}
	if got := readLog(log); !strings.Contains(got, `tty of s is "/dev/ttys1"`) || !strings.Contains(got, "activate") {
		t.Fatalf("got %s", got)
	}
}

// Naming iTerm in AppleScript starts it, so with iTerm closed it's never named.
func TestFocusLinkNeverStartsITerm(t *testing.T) {
	log := fakeFocusTools(t, "", "", "found", false)
	if err := sessions.RealFocusLinkTTY("/dev/ttys1"); err == nil {
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
	oldPath, oldLs := sessions.LinkAppPath, sessions.Lsregister
	sessions.LinkAppPath = func() string { return app }
	sessions.Lsregister = filepath.Join(bin, "lsregister")
	t.Cleanup(func() { sessions.LinkAppPath, sessions.Lsregister = oldPath, oldLs })
	return log, app
}

func TestInstallLinkApp(t *testing.T) {
	log, app := fakeAppTools(t)
	if err := sessions.InstallLinkApp("/opt/homebrew/bin/ccs"); err != nil {
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
	if v := sessions.LinkPlistValue(app, sessions.LinkTargetKey); v != "/opt/homebrew/bin/ccs" {
		t.Errorf("target %q", v)
	}
}

// An app already right stays untouched, or macOS would ask for permission again.
func TestInstallLinkAppKeepsCurrent(t *testing.T) {
	log, _ := fakeAppTools(t)
	if err := sessions.InstallLinkApp("/opt/homebrew/bin/ccs"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	if err := sessions.InstallLinkApp("/opt/homebrew/bin/ccs"); err != nil {
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
	if err := sessions.InstallLinkApp("/old/ccs"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	if err := sessions.InstallLinkApp("/new/ccs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readLog(log), "osacompile") {
		t.Fatal("not rebuilt")
	}
	if v := sessions.LinkPlistValue(app, sessions.LinkTargetKey); v != "/new/ccs" {
		t.Fatalf("target %q", v)
	}
}

func TestInstallLinkAppRefusesOthersApp(t *testing.T) {
	log, app := fakeAppTools(t)
	os.MkdirAll(filepath.Join(app, "Contents"), 0o755)
	os.WriteFile(filepath.Join(app, "Contents", "Info.plist.CFBundleIdentifier"), []byte("com.someone.else"), 0o644)
	if err := sessions.InstallLinkApp("/x/ccs"); err == nil || !strings.Contains(err.Error(), "isn't ccs's") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(readLog(log), "osacompile") {
		t.Fatal("overwrote it")
	}
	var out, errs bytes.Buffer
	if got := sessions.LinkUninstall(&out, &errs); got != 1 {
		t.Fatalf("exit %d", got)
	}
	if _, err := os.Stat(app); err != nil {
		t.Fatal("uninstall removed someone else's app")
	}
}

func TestInstallLinkAppRefusesUnquotablePath(t *testing.T) {
	log, _ := fakeAppTools(t)
	if err := sessions.InstallLinkApp("/bad\npath/ccs"); err == nil {
		t.Fatal("accepted")
	}
	if got := readLog(log); got != "" {
		t.Fatalf("ran %s", got)
	}
}

func TestLinkUninstall(t *testing.T) {
	log, app := fakeAppTools(t)
	if err := sessions.InstallLinkApp("/x/ccs"); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if got := sessions.LinkUninstall(&out, &errs); got != 0 {
		t.Fatalf("exit %d: %s", got, errs.String())
	}
	if _, err := os.Stat(app); err == nil {
		t.Fatal("still there")
	}
	if !strings.Contains(readLog(log), "lsregister -u "+app) {
		t.Fatal("not unregistered")
	}
	if got := sessions.LinkUninstall(&out, &errs); got != 0 {
		t.Fatalf("exit %d with nothing installed", got)
	}
}

func TestLinkInstallReportsAnotherHandler(t *testing.T) {
	_, app := fakeAppTools(t)
	oldHandler, oldSleep := sessions.UrlHandlerApp, sessions.Sleep
	t.Cleanup(func() { sessions.UrlHandlerApp, sessions.Sleep = oldHandler, oldSleep })
	sessions.Sleep = func(time.Duration) {}
	sessions.UrlHandlerApp = func() string { return "/Applications/Other.app" }
	var out, errs bytes.Buffer
	if got := sessions.LinkInstall(&out, &errs); got != 1 || !strings.Contains(errs.String(), "Other.app") {
		t.Fatalf("exit %d, %q", got, errs.String())
	}
	sessions.UrlHandlerApp = func() string { return app }
	if got := sessions.LinkInstall(&out, &errs); got != 0 || !strings.Contains(out.String(), "now open with") {
		t.Fatalf("exit %d, %q", got, out.String())
	}
	out.Reset()
	if got := sessions.LinkStatus(&out); got != 0 || !strings.Contains(out.String(), "which runs") {
		t.Fatalf("exit %d, %q", got, out.String())
	}
	sessions.UrlHandlerApp = func() string { return "" }
	if got := sessions.LinkStatus(&out); got != 1 {
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

	sessions.RefreshLinkApp() // nothing installed: installs nothing
	if got := readLog(log); strings.Contains(got, "osacompile") {
		t.Fatalf("installed one unasked:\n%s", got)
	}

	target := filepath.Join(t.TempDir(), "ccs")
	os.WriteFile(target, nil, 0o755)
	if err := sessions.InstallLinkApp(target); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(log, nil, 0o644)
	sessions.RefreshLinkApp() // current and its ccs exists: left alone, as across a brew upgrade
	if got := readLog(log); strings.Contains(got, "osacompile") {
		t.Fatalf("rebuilt a current app:\n%s", got)
	}

	os.WriteFile(filepath.Join(app, "Contents", "Info.plist."+sessions.LinkVersionKey), []byte("0"), 0o644)
	sessions.RefreshLinkApp() // an older handler is rebuilt
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
	old := sessions.ITermRunning
	sessions.ITermRunning = func() bool { return iterm }
	t.Cleanup(func() { sessions.ITermRunning = old })
	return log
}

func TestFocusLinkITermNoSuchTab(t *testing.T) {
	fakeFocusTools(t, "", "", "", true)
	if err := sessions.RealFocusLinkTTY("/dev/ttys1"); err == nil || !strings.Contains(err.Error(), "isn't in a tmux window or an iTerm tab") {
		t.Fatalf("got %v", err)
	}
}

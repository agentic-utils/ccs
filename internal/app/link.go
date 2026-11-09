package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
)

// ccs://session/<name> switches to that session's tab. macOS delivers a URL as
// an Apple event, not an argument, and a cgo-free Go binary can't receive one,
// so a small AppleScript app in ~/Applications takes it and runs `ccs link open`.
const (
	linkPrefix   = "ccs://session/"
	linkBundleID = "io.github.agentic-utils.ccs-links"
	// linkAppVersion changes when linkApplet or the app's plist does; a change
	// rebuilds the app, and macOS then asks again for permission to control iTerm.
	linkAppVersion = "1"
	linkVersionKey = "CCSLinkVersion"
	linkTargetKey  = "CCSLinkTarget"
)

var (
	linkAppPath = func() string {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Applications", "CCS Links.app")
	}
	lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

	// Stubbed in tests.
	namedSessions = sessionsNamed
	focusLinkTTY  = realFocusLinkTTY
	iTermRunning  = realITermRunning
	urlHandlerApp = realURLHandlerApp
)

// parseSessionLink is the session a link names. The link comes from anywhere,
// so it's matched whole against one exact shape, nothing decoded: no query,
// fragment, user, extra path or percent-escapes. Errors don't repeat it.
func parseSessionLink(raw string) (string, error) {
	name, ok := strings.CutPrefix(raw, linkPrefix)
	if !ok || !state.SessionName.MatchString(name) {
		return "", errors.New("that isn't a valid ccs link, so nothing was opened")
	}
	return name, nil
}

// openLink switches to the running session a link names. It only ever focuses:
// a session that isn't running is reported, never started or resumed.
func openLink(raw string) (string, error) {
	name, err := parseSessionLink(raw)
	if err != nil {
		return "", err
	}
	pids, err := namedSessions(name)
	if err != nil {
		return "", err
	}
	switch len(pids) {
	case 0:
		return "", fmt.Errorf("no session named %s is running, so nothing was opened", name)
	case 1:
	default:
		return "", fmt.Errorf("%d running sessions are named %s, so there's no telling which you meant", len(pids), name)
	}
	tty := proc.TtyOf(pids[0])
	if tty == "" {
		return "", fmt.Errorf("%s has no terminal tab to switch to: an app or claude --bg started it", name)
	}
	if err := focusLinkTTY(tty); err != nil {
		return "", fmt.Errorf("couldn't switch to %s: %v", name, err)
	}
	return "switched to " + name, nil
}

// realFocusLinkTTY selects the tmux window or iTerm tab on tty and brings it to
// the front. The handler app starts ccs with neither $TMUX nor $TERM_PROGRAM,
// so unlike focusTTY this asks tmux and iTerm directly. It never starts iTerm.
func realFocusLinkTTY(tty string) error {
	panes, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
	if target := tmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
		return focusTmuxTarget(target)
	}
	if !iTermRunning() {
		return errors.New("it isn't in a tmux window or an iTerm tab")
	}
	out, err := iTermSelect(tty, true)
	if err != nil {
		return iTermError(err)
	}
	if strings.TrimSpace(string(out)) != "found" {
		return errors.New("it isn't in a tmux window or an iTerm tab")
	}
	return nil
}

// focusTmuxTarget points tmux's most recently active client at target, then
// raises that client's iTerm tab if it has one. A client in another terminal
// switches but its window stays where it is.
func focusTmuxTarget(target string) error {
	out, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-clients", "-F", "#{client_activity} #{client_tty}")
	client := latestTmuxClient(string(out))
	if err != nil || client == "" {
		return errors.New("its tmux session isn't attached to a terminal")
	}
	if _, err := proc.RunBounded(5*time.Second, nil, "tmux", "switch-client", "-c", client, "-t", target, ";", "select-window", "-t", target, ";", "select-pane", "-t", target); err != nil {
		return fmt.Errorf("tmux wouldn't switch: %v", err)
	}
	if iTermRunning() {
		iTermSelect(client, true)
	}
	return nil
}

// latestTmuxClient picks the tty from `#{client_activity} #{client_tty}` lines.
func latestTmuxClient(lines string) string {
	best, bestAt := "", int64(-1)
	for _, line := range strings.Split(lines, "\n") {
		at, tty, ok := strings.Cut(strings.TrimSpace(line), " ")
		if n, err := strconv.ParseInt(at, 10, 64); ok && err == nil && n > bestAt {
			best, bestAt = tty, n
		}
	}
	return best
}

// iTermError says what to do about a failed osascript call. -1743 is macOS
// refusing the app permission to control iTerm.
func iTermError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "-1743") {
		return errors.New("macOS hasn't allowed it to control iTerm: turn on CCS Links under iTerm in System Settings, Privacy & Security, Automation")
	}
	return fmt.Errorf("iTerm wouldn't switch: %v", err)
}

// realITermRunning looks in the process list, since naming iTerm in AppleScript
// would start it, and opening a link must not start anything.
func realITermRunning() bool {
	out, err := proc.RunBounded(3*time.Second, nil, "ps", "-axo", "comm=")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "/MacOS/iTerm2") {
			return true
		}
	}
	return false
}

// realURLHandlerApp is the app macOS would open a ccs link with, "" for none.
func realURLHandlerApp() string {
	script := `ObjC.import("AppKit");
var app = $.NSWorkspace.sharedWorkspace.URLForApplicationToOpenURL($.NSURL.URLWithString("` + linkPrefix + `x"));
app.isNil() ? "" : app.path.js`
	out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-l", "JavaScript", "-e", script)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// linkTarget is the ccs the app runs. A Homebrew keg's folder changes with every
// upgrade but its bin symlink doesn't, so the app keeps working across upgrades.
func linkTarget(exe string) string {
	if prefix, _, ok := strings.Cut(exe, "/Cellar/"); ok {
		bin := filepath.Join(prefix, "bin", "ccs")
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}
	return exe
}

// appleScriptString is s as an AppleScript string literal.
func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// linkApplet is the handler app's source. The URL only ever reaches the shell
// through `quoted form of`, and ccs validates it before using it.
func linkApplet(target string) string {
	return fmt.Sprintf(`on open location theURL
	try
		do shell script (quoted form of %s) & " link open " & quoted form of theURL
	on error errorText
		activate
		display alert "ccs link" message errorText as warning giving up after 15
	end try
end open location

on run
	activate
	display alert "CCS Links" message "This app opens ccs://session/<name> links by switching to that session's tab. There's nothing to do here." giving up after 15
end run
`, appleScriptString(target))
}

// linkPlistValue reads one key of the app's Info.plist, "" when it can't.
func linkPlistValue(app, key string) string {
	out, err := proc.RunBounded(5*time.Second, nil, "plutil", "-extract", key, "raw", "-o", "-", filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// linkAppCurrent reports whether the app at path is this version's, running target.
func linkAppCurrent(app, target string) bool {
	return linkPlistValue(app, linkVersionKey) == linkAppVersion && linkPlistValue(app, linkTargetKey) == target
}

// installLinkApp builds the handler app and registers it. An app already right
// is only registered again: rebuilding would make macOS ask for permission again.
func installLinkApp(target string) error {
	if !proc.ShellSafe(target) {
		return fmt.Errorf("can't use ccs at %q: the path has characters the handler can't quote", target)
	}
	app := linkAppPath()
	if _, err := os.Stat(app); err == nil {
		if linkPlistValue(app, "CFBundleIdentifier") != linkBundleID {
			return fmt.Errorf("%s exists and isn't ccs's: move it first", app)
		}
		if linkAppCurrent(app, target) {
			return registerLinkApp(app)
		}
	}
	if err := buildLinkApp(app, target); err != nil {
		return err
	}
	return registerLinkApp(app)
}

// buildLinkApp compiles the app in a temp folder, then moves it into place.
func buildLinkApp(app, target string) error {
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(cache, "ccs-link-*") // beside home, so the renames stay on one volume
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	source := filepath.Join(tmp, "handler.applescript")
	if err := os.WriteFile(source, []byte(linkApplet(target)), 0o600); err != nil {
		return err
	}
	built := filepath.Join(tmp, filepath.Base(app))
	run := func(name string, args ...string) error {
		if _, err := proc.RunBounded(30*time.Second, nil, name, args...); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		return nil
	}
	if err := run("osacompile", "-o", built, source); err != nil {
		return err
	}
	plist := filepath.Join(built, "Contents", "Info.plist")
	for _, edit := range [][]string{
		{"CFBundleIdentifier", "-string", linkBundleID},
		{"CFBundleURLTypes", "-json", `[{"CFBundleURLName":"ccs session link","CFBundleURLSchemes":["ccs"]}]`},
		{"LSUIElement", "-bool", "true"}, // no Dock icon for a click
		{"NSAppleEventsUsageDescription", "-string", "CCS Links switches to a session's iTerm tab when you open a ccs:// link."},
		{linkVersionKey, "-string", linkAppVersion},
		{linkTargetKey, "-string", target},
	} {
		if err := run("plutil", append([]string{"-replace"}, append(edit, plist)...)...); err != nil {
			return err
		}
	}
	// The plist edits broke the signature osacompile gave it.
	if err := run("codesign", "--force", "--sign", "-", built); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(app), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(app); err == nil {
		if err := os.Rename(app, filepath.Join(tmp, "old.app")); err != nil {
			return err
		}
	}
	return os.Rename(built, app)
}

func registerLinkApp(app string) error {
	if _, err := proc.RunBounded(30*time.Second, nil, lsregister, "-f", app); err != nil {
		return fmt.Errorf("couldn't register %s with macOS: %v", app, err)
	}
	return nil
}

// refreshLinkApp rebuilds an installed handler after an update changed it, or
// when the ccs it points at has gone. It never installs one that isn't there.
func refreshLinkApp() {
	if runtime.GOOS != "darwin" || version == "dev" {
		return
	}
	app := linkAppPath()
	if _, err := os.Stat(app); err != nil || linkPlistValue(app, "CFBundleIdentifier") != linkBundleID {
		return
	}
	target := linkPlistValue(app, linkTargetKey)
	if _, err := os.Stat(target); err == nil && linkPlistValue(app, linkVersionKey) == linkAppVersion {
		return
	}
	exe, err := os.Executable()
	if err == nil {
		err = installLinkApp(linkTarget(exe))
	}
	state.LogActivity("link handler refreshed: %v", state.ErrOrOK(err))
}

// runLink is `ccs link`: open is what the handler app runs, the others manage it.
func runLink(args []string, stdout, stderr io.Writer) int {
	usage := "usage: ccs link install | uninstall | status | open ccs://session/<name>"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if args[0] == "open" {
		if len(args) != 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		// The handler app's PATH is only the system's, without Homebrew's tmux.
		os.Setenv("PATH", os.Getenv("PATH")+":/opt/homebrew/bin:/usr/local/bin")
		msg, err := openLink(args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(stderr, "ccs links are macOS only")
		return 1
	}
	switch args[0] {
	case "install":
		return linkInstall(stdout, stderr)
	case "uninstall":
		return linkUninstall(stdout, stderr)
	case "status":
		return linkStatus(stdout)
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

func linkInstall(stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err == nil {
		err = installLinkApp(linkTarget(exe))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	app := linkAppPath()
	handler := ""
	for range 10 { // macOS can take a moment to notice the new app
		if handler = urlHandlerApp(); handler == app {
			fmt.Fprintf(stdout, "ccs:// links now open with %s\n", app)
			return 0
		}
		sleep(300 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "installed %s, but macOS opens ccs:// links with %q\n", app, handler)
	return 1
}

func linkUninstall(stdout, stderr io.Writer) int {
	app := linkAppPath()
	if _, err := os.Stat(app); err != nil {
		fmt.Fprintln(stdout, "no handler installed")
		return 0
	}
	if linkPlistValue(app, "CFBundleIdentifier") != linkBundleID {
		fmt.Fprintf(stderr, "%s isn't ccs's, so it was left alone\n", app)
		return 1
	}
	if _, err := proc.RunBounded(30*time.Second, nil, lsregister, "-u", app); err != nil {
		fmt.Fprintf(stderr, "couldn't unregister %s: %v\n", app, err)
		return 1
	}
	if err := os.RemoveAll(app); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "removed %s\n", app)
	return 0
}

// linkStatus says whether a link would work; 0 only when ours handles them.
func linkStatus(stdout io.Writer) int {
	app := linkAppPath()
	handler := urlHandlerApp()
	if handler == "" {
		fmt.Fprintln(stdout, "nothing handles ccs:// links: run ccs link install")
		return 1
	}
	fmt.Fprintf(stdout, "ccs:// links open with %s\n", handler)
	if handler != app {
		fmt.Fprintln(stdout, "that isn't ccs's handler: run ccs link install")
		return 1
	}
	fmt.Fprintf(stdout, "which runs %s\n", linkPlistValue(app, linkTargetKey))
	return 0
}

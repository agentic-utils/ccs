package mcp

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// stubLaunchctl records launchctl calls and answers print with state.
func stubLaunchctl(t *testing.T, state *string) *[]string {
	t.Helper()
	var calls []string
	old := Launchctl
	Launchctl = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "print" {
			if *state == "" {
				return "Could not find service", errors.New("exit status 113")
			}
			return "gui/501/" + ServeLabel + " = {\n\tstate = " + *state + "\n\tpid = 4321\n}\n", nil
		}
		if args[0] == "bootstrap" {
			*state = "running"
		}
		if args[0] == "bootout" {
			*state = ""
		}
		return "", nil
	}
	t.Cleanup(func() { Launchctl = old })
	return &calls
}

func TestServeInstallWritesTheAgent(t *testing.T) {
	os.RemoveAll(LaunchAgentsDir())
	state := ""
	stubLaunchctl(t, &state)
	oldBin := serveBinary
	serveBinary = func() string { return "/opt/homebrew/bin/ccs" }
	t.Cleanup(func() { serveBinary = oldBin })

	var out, errb strings.Builder
	// Flags ccs serve would refuse are refused here too, and nothing is written.
	for _, args := range [][]string{
		{"install", "--host", "h"},
		{"install", "--preview", "--team", "t"},
		{"install", "--preview", "--addr", "0.0.0.0:1"},
	} {
		if code := RunServe(args, &out, &errb, nil); code != 2 {
			t.Errorf("%v: code %d", args, code)
		}
	}
	if _, err := os.Stat(ServePlistPath()); err == nil {
		t.Fatal("a refused install wrote the plist")
	}
	args := []string{"install", "--host", "ccs.example.com", "--team", "me", "--aud", "a&b<c>", "--email", "me@example.com", "--allow-write"}
	if code := RunServe(args, &out, &errb, nil); code != 0 {
		t.Fatalf("install: %d %s", code, errb.String())
	}
	info, err := os.Stat(ServePlistPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the plist holds the Access values, so it's 0600: %v %v", info.Mode(), err)
	}
	b, _ := os.ReadFile(ServePlistPath())
	plist := string(b)
	for _, want := range []string{
		"<string>" + ServeLabel + "</string>",
		"<string>/opt/homebrew/bin/ccs</string>\n\t\t<string>serve</string>\n\t\t<string>--host</string>\n\t\t<string>ccs.example.com</string>",
		"<string>a&amp;b&lt;c&gt;</string>",
		"<key>RunAtLoad</key>\n\t<false/>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>\n\t<integer>30</integer>",
		"<key>ProcessType</key>\n\t<string>Interactive</string>",
		"<string>" + ServeLogPath() + "</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("the plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "install") {
		t.Error("the agent runs `ccs serve <flags>`, not install")
	}
}

func TestServeServiceStartStopStatus(t *testing.T) {
	os.RemoveAll(LaunchAgentsDir())
	state := ""
	calls := stubLaunchctl(t, &state)
	if _, err := StartServeService(); err == nil || !strings.Contains(err.Error(), "ccs serve install") {
		t.Errorf("start with no plist: %v", err)
	}
	if st := ServeServiceStatus(); st.Installed || st.Loaded || st.Running {
		t.Errorf("nothing yet: %+v", st)
	}
	if _, err := InstallServeService(serveFlags{}, []string{"--preview"}); err != nil {
		t.Fatal(err)
	}
	if st := ServeServiceStatus(); !st.Installed || st.Running {
		t.Errorf("installed, not running (RunAtLoad false): %+v", st)
	}
	if _, err := StartServeService(); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*calls, func(c string) bool {
		return strings.HasPrefix(c, "bootstrap gui/") && strings.HasSuffix(c, ServePlistPath())
	}) {
		t.Errorf("start bootstraps the plist: %v", *calls)
	}
	st := ServeServiceStatus()
	if !st.Running || st.PID != 4321 {
		t.Errorf("running from launchctl print's state and pid: %+v", st)
	}
	n := len(*calls)
	if msg, _ := StartServeService(); !strings.Contains(msg, "already running") || slices.ContainsFunc((*calls)[n:], func(c string) bool { return strings.HasPrefix(c, "bootstrap") }) {
		t.Errorf("a second start does nothing: %q", msg)
	}
	// Loaded but not running (crashed, throttled): start loads it afresh.
	state = "not running"
	if _, err := StartServeService(); err != nil || !slices.Contains(*calls, "bootout gui/"+strconv.Itoa(os.Getuid())+"/"+ServeLabel) || state != "running" {
		t.Errorf("start after a crash: %v %v", err, *calls)
	}
	if _, err := StopServeService(); err != nil || ServeServiceStatus().Loaded {
		t.Errorf("stop unloads it, so it stays stopped: %v", err)
	}
	if msg, _ := StopServeService(); !strings.Contains(msg, "isn't running") {
		t.Errorf("stop when stopped: %q", msg)
	}
	if _, err := UninstallServeService(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ServePlistPath()); err == nil {
		t.Error("uninstall removes the plist")
	}
}

// A stop unloads the job (launchctl bootout of gui/<uid>/<label>), never
// only kills its process: with KeepAlive, launchd would start a killed one
// again. Afterwards launchd has nothing running.
func TestServeStopUnloadsTheJob(t *testing.T) {
	os.RemoveAll(LaunchAgentsDir())
	state := ""
	calls := stubLaunchctl(t, &state)
	if _, err := InstallServeService(serveFlags{}, []string{"--preview"}); err != nil {
		t.Fatal(err)
	}
	if _, err := StartServeService(); err != nil {
		t.Fatal(err)
	}
	n := len(*calls)
	if _, err := StopServeService(); err != nil {
		t.Fatal(err)
	}
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + ServeLabel
	after := (*calls)[n:]
	if !slices.Contains(after, "bootout "+target) {
		t.Errorf("stop must boot the job out: %v", after)
	}
	if slices.ContainsFunc(after, func(c string) bool { return strings.HasPrefix(c, "kill") || strings.HasPrefix(c, "stop") }) {
		t.Errorf("stop never only kills the process: %v", after)
	}
	if st := ServeServiceStatus(); st.Loaded || st.Running {
		t.Errorf("nothing runs after a stop: %+v", st)
	}
}

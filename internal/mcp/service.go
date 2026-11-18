package mcp

// ccs serve as a per-user launchd agent, which the user starts and stops
// (`ccs serve start|stop`, or ^X B in the TUI). It isn't hosted inside the
// TUI, which restarts on every auto-update and stops when it quits; see
// docs/design/relay.md. RunAtLoad is false, so it never starts at login, and
// KeepAlive restarts it only after a crash (SuccessfulExit false), at most
// every ThrottleInterval (30s), so a crash loop can't spin; stop unloads it
// (launchctl bootout, never only a kill), so a stopped agent stays stopped.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
)

// ServeLabel is the launchd label of ccs serve's agent (as the link
// handler's bundle id, io.github.agentic-utils.ccs-links).
const ServeLabel = "io.github.agentic-utils.ccs-serve"

// LaunchAgentsDir is where the agent's plist goes; a temp folder in tests.
var LaunchAgentsDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

// Launchctl runs launchctl with a deadline; stubbed in tests, which must
// never touch launchd.
var Launchctl = func(args ...string) (string, error) {
	out, err := proc.RunBounded(10*time.Second, nil, "launchctl", args...)
	return string(out), err
}

// serveBinary is the ccs the agent runs: by its full path, since launchd
// has no shell and no PATH of the user's.
var serveBinary = func() string {
	if bin, err := exec.LookPath("ccs"); err == nil {
		if abs, err := filepath.Abs(bin); err == nil {
			return abs
		}
	}
	if bin, err := os.Executable(); err == nil {
		return bin
	}
	return "ccs"
}

// ServePlistPath is the agent's plist.
func ServePlistPath() string { return filepath.Join(LaunchAgentsDir(), ServeLabel+".plist") }

// ServeLogPath is where the agent's output goes, beside the update log.
func ServeLogPath() string { return filepath.Join(filepath.Dir(state.UpdateLogPath), "serve.log") }

func serviceDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func serviceTarget() string { return serviceDomain() + "/" + ServeLabel }

func isServiceCommand(a string) bool {
	switch a {
	case "install", "uninstall", "start", "stop", "status", "qr":
		return true
	}
	return false
}

// RunService is `ccs serve install|uninstall|start|stop|status|qr`.
func RunService(args []string, stdout, stderr io.Writer) int {
	var msg string
	var err error
	switch args[0] {
	case "install":
		f, code := parseServeFlags(args[1:], stderr)
		if code != 0 {
			return code
		}
		msg, err = InstallServeService(f, args[1:])
	case "uninstall":
		msg, err = UninstallServeService()
	case "start":
		msg, err = StartServeService()
	case "stop":
		msg, err = StopServeService()
	case "qr":
		url, err := ServeURL()
		if err != nil {
			fmt.Fprintln(stderr, "ccs serve:", err)
			return 1
		}
		printQR(stdout, url)
		return 0
	case "status":
		st := ServeServiceStatus()
		switch {
		case !st.Installed:
			msg = "not installed: ccs serve install <flags> writes it (docs/remote.md)"
		case st.Running:
			msg = fmt.Sprintf("running (pid %d), %s", st.PID, ServePlistPath())
		default:
			msg = "installed, not running: ccs serve start runs it"
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccs serve:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// servePlist is the agent's plist: this ccs, `serve` and the flags given,
// checked as `ccs serve` checks them.
func servePlist(bin string, flags []string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + ServeLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range append([]string{bin, "serve"}, flags...) {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<false/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>` + esc(ServeLogPath()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(ServeLogPath()) + `</string>
</dict>
</plist>
`)
	return []byte(b.String())
}

// InstallServeService writes the agent's plist (0600: it holds the Access
// application's values). A running agent keeps its old flags until it's
// stopped and started again.
func InstallServeService(_ serveFlags, flags []string) (string, error) {
	if err := os.MkdirAll(LaunchAgentsDir(), 0o755); err != nil {
		return "", err
	}
	os.MkdirAll(filepath.Dir(ServeLogPath()), 0o755)
	path := ServePlistPath()
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, servePlist(serveBinary(), flags), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	msg := "wrote " + path + ": ccs serve start (or ^X B in ccs) runs it"
	if ServeServiceStatus().Running {
		msg = "wrote " + path + ": the running ccs serve keeps its old flags until ccs serve stop, then start"
	}
	state.LogActivity("serve: agent installed: %s", path)
	return msg, nil
}

// UninstallServeService stops the agent if it runs and removes its plist.
func UninstallServeService() (string, error) {
	path := ServePlistPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "not installed", nil
	}
	if ServeServiceStatus().Running {
		if _, err := StopServeService(); err != nil {
			return "", err
		}
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	state.LogActivity("serve: agent uninstalled")
	return "removed " + path, nil
}

// StartServeService loads and runs the agent.
func StartServeService() (string, error) {
	st := ServeServiceStatus()
	switch {
	case !st.Installed:
		return "", errors.New("ccs serve isn't installed: run ccs serve install <flags> first (docs/remote.md)")
	case st.Running:
		return fmt.Sprintf("ccs serve is already running (pid %d)", st.PID), nil
	}
	if st.Loaded { // loaded but not running (crashed out of throttling): start it afresh
		Launchctl("bootout", serviceTarget())
	}
	if out, err := Launchctl("bootstrap", serviceDomain(), ServePlistPath()); err != nil {
		return "", fmt.Errorf("launchctl bootstrap: %v %s", err, strings.TrimSpace(out))
	}
	state.LogActivity("serve: agent started")
	return "started ccs serve (its output: " + ServeLogPath() + ")", nil
}

// StopServeService unloads the agent, so it stays stopped.
func StopServeService() (string, error) {
	if !ServeServiceStatus().Loaded {
		return "ccs serve isn't running", nil
	}
	if out, err := Launchctl("bootout", serviceTarget()); err != nil {
		return "", fmt.Errorf("launchctl bootout: %v %s", err, strings.TrimSpace(out))
	}
	state.LogActivity("serve: agent stopped")
	return "stopped ccs serve", nil
}

// ServiceStatus is the agent as launchd has it.
type ServiceStatus struct {
	Installed bool // its plist exists
	Loaded    bool // launchd knows it (launchctl print succeeds)
	Running   bool // launchctl print says state = running
	PID       int
}

// ServeServiceStatus reads the plist and `launchctl print`'s state.
func ServeServiceStatus() ServiceStatus {
	var st ServiceStatus
	if _, err := os.Stat(ServePlistPath()); err == nil {
		st.Installed = true
	}
	out, err := Launchctl("print", serviceTarget())
	if err != nil {
		return st
	}
	st.Loaded = true
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			st.Running = v == "running"
		case "pid":
			st.PID, _ = strconv.Atoi(v)
		}
	}
	return st
}

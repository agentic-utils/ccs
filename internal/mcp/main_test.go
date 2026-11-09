package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// TestMain keeps tests off the machine, as in app's TestMain: launch_session
// and close_session reach sessions' tab and process hooks, so those are
// stubbed, and logs, settings and Codex's folder go to a temp folder.
func TestMain(m *testing.M) {
	for _, k := range []string{"TERM_PROGRAM", "LC_TERMINAL", "ITERM_SESSION_ID", "TMUX", "CODEX_THREAD_ID"} {
		os.Unsetenv(k)
	}
	dir, _ := os.MkdirTemp("", "ccs-test-mcp")
	state.UpdateLogPath = filepath.Join(dir, "update.log")
	state.ActivityLogPath = filepath.Join(dir, "activity.log")
	state.SettingsPath = filepath.Join(dir, "settings.json")
	state.MarksPath = filepath.Join(dir, "sessions.json")
	state.LaunchDir = func() string { return filepath.Join(dir, "launch") }
	sessions.OpenLaunchTab = func(string) (string, error) { return "", errors.New("tests open no tabs") }
	sessions.RetypeLauncher = func(string, string) error { return errors.New("tests type into no tabs") }
	sessions.CheckLaunchShell = func() error { return nil }
	sessions.CloseTab = func(string) bool { return false }
	sessions.FocusTTY = func(string, bool) bool { return false }
	sessions.CodexQueue = func(string, string) error { return errors.New("tests queue nothing") }
	sessions.LiveCodexSessions = func() map[string]sessions.LiveSession { return nil }
	transcript.CodexHome = func() string { return filepath.Join(dir, "codex") }
	proc.PsAll = func() (string, error) { return "", nil }
	proc.PsTree = func() (string, error) { return "", nil }
	proc.TtyOf = func(int) string { return "" }
	proc.SignalPID = func(int, syscall.Signal) error { return errors.New("tests signal nothing") }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

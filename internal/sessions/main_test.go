package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// TestMain keeps tests off the machine: no real logs, settings, tabs,
// processes, Codex or link handler, as in tui's TestMain. A test that needs
// one of these stubs it itself.
func TestMain(m *testing.M) {
	proc.MayRun = func(string) bool { return true } // never the machine's own processes
	for _, k := range []string{"TERM_PROGRAM", "LC_TERMINAL", "ITERM_SESSION_ID", "TMUX", "CODEX_THREAD_ID", "ANTIGRAVITY_CONVERSATION_ID", "ANTIGRAVITY_APP_DATA_DIR"} {
		os.Unsetenv(k)
	}
	dir, _ := os.MkdirTemp("", "ccs-test-sessions")
	state.UpdateLogPath = filepath.Join(dir, "update.log")
	state.ActivityLogPath = filepath.Join(dir, "activity.log")
	state.SettingsPath = filepath.Join(dir, "settings.json")
	state.MarksPath = filepath.Join(dir, "sessions.json")
	state.LaunchDir = func() string { return filepath.Join(dir, "launch") }
	OpenRecordPath = filepath.Join(dir, "open-sessions.json")
	HostOf = func(int, string) string { return "" }
	ListAgents = func() (map[string]BgSession, error) { return nil, nil }
	T3Link = func(int) (string, error) { return "", errors.New("no T3 Code in tests") }
	OpenApp = func(string) error { return errors.New("tests open nothing") }
	LinkAppPath = func() string { return filepath.Join(dir, "CCS Links.app") }
	Lsregister = "/bin/false"
	FocusLinkTTY = func(string) error { return errors.New("tests focus nothing") }
	ITermRunning = func() bool { return false }
	URLHandlerApp = func() string { return "" }
	OpenLaunchTab = func(string) (string, error) { return "", errors.New("tests open no tabs") }
	RetypeLauncher = func(string, string) error { return errors.New("tests type into no tabs") }
	CheckLaunchShell = func() error { return nil }
	CloseTab = func(string) bool { return false }
	FocusTTY = func(string, bool) bool { return false }
	TmuxRun = func(...string) (string, error) { return "", errors.New("tests run no tmux") }
	ITermAPI = func() (iTermAPI, error) { return nil, errors.New("tests talk to no iTerm") }
	ITermSessionTTYs = func() (map[string]string, error) { return nil, errors.New("tests ask no iTerm") }
	ITermCookie = func() (string, string, error) { return "", "", errors.New("tests ask no iTerm") }
	ExecScript = func(string) error { return errors.New("tests exec nothing") }
	SpawnRestartHelper = func(string, int, string, time.Duration) error { return errors.New("tests spawn no helper") }
	CodexQueue = func(string, string) error { return errors.New("tests queue nothing") }
	transcript.AgyHome = func() string { return filepath.Join(dir, "agy") }
	AgyDeliver = func(string, string) error { return errors.New("tests deliver nothing to agy") }
	LiveAgySessions = func() map[string]LiveSession { return nil }
	AgyPresenceLocks = func() map[int]string { return nil }
	LiveCodexSessions = func() map[string]LiveSession { return nil }
	transcript.CodexHome = func() string { return filepath.Join(dir, "codex") }
	transcript.CodexProcCwd = func(int) string { return "" }
	proc.PsAll = func() (string, error) { return "", nil }
	proc.PsTree = func() (string, error) { return "", nil }
	CallingSession = func() (Caller, bool) { return Caller{}, false } // never this machine's sessions
	proc.TTYOf = func(int) string { return "" }
	proc.SignalPID = func(int, syscall.Signal) error { return errors.New("tests signal nothing") }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

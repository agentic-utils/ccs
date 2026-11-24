package mcp

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Hook test state, read by the stubs TestMain installs: otherPeer makes the
// hook see another user listening; testClock, non-zero, is the hook's clock
// (unix ns), moved on by its sleeps; testOwnFile is its own session file.
var (
	otherPeer   atomic.Bool
	testClock   atomic.Int64
	testOwnFile atomic.Pointer[ownFile]
)

// TestMain keeps tests off the machine, as in tui's TestMain: launch_session
// and close_session reach sessions' tab and process hooks, so those are
// stubbed, and logs, settings and Codex's folder go to a temp folder.
func TestMain(m *testing.M) {
	proc.MayRun = func(string) bool { return true } // never the machine's own processes
	os.Setenv("CCS_NO_STATS", "1")                  // feature stats are tested in internal/stats; nothing else writes them
	// ccs prs checks only the claims' pids (github.LiveAmong): through each test's LiveByPID stub.
	github.LiveAmong = func([]int) map[int]github.PRClaim { return github.LiveByPID() }
	statusDir, _ := os.MkdirTemp("", "ccs-test-statusline")
	github.StatusLineDir = func() string { return statusDir } // never the real TMPDIR the status line reads
	for _, k := range []string{"TERM_PROGRAM", "LC_TERMINAL", "ITERM_SESSION_ID", "TMUX", "CODEX_THREAD_ID", "ANTIGRAVITY_CONVERSATION_ID", "ANTIGRAVITY_APP_DATA_DIR"} {
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
	sessions.SpawnRestartHelper = func(string, int, string, time.Duration) error { return errors.New("tests spawn no helper") }
	sessions.FocusTTY = func(string, bool) bool { return false }
	sessions.FindTerminal = func(int) sessions.Terminal { return sessions.Terminal{} }
	Launchctl = func(...string) (string, error) { return "", errors.New("tests run no launchctl") }
	LaunchAgentsDir = func() string { return filepath.Join(dir, "LaunchAgents") }
	sessions.SendKeys = func(sessions.Terminal, string, bool) error { return errors.New("tests type into no terminals") }
	sessions.CodexQueue = func(string, string) error { return errors.New("tests queue nothing") }
	sessions.LiveCodexSessions = func() map[string]sessions.LiveSession { return nil }
	transcript.AgyHome = func() string { return filepath.Join(dir, "agy") }
	sessions.AgyDeliver = func(string, string) error { return errors.New("tests deliver nothing to agy") }
	sessions.LiveAgySessions = func() map[string]sessions.LiveSession { return nil }
	sessions.AgyPresenceLocks = func() map[int]string { return nil }
	liveSessions = func() map[string]sessions.LiveSession { return nil }
	procStart = func(int) (time.Time, bool) { return time.Time{}, false } // never a real ps
	// A hook's own wait reads as over (another session), so a hook its test
	// left behind stops at its first redial; tests of redialling set
	// testOwnFile. The hook's stubs are set once here and read test state
	// through atomics, so a hook still running from a test never races the
	// next one's set-up.
	hookSessionFile = func() (sessions.SessionFile, error) {
		if f := testOwnFile.Load(); f != nil {
			return f.file(), nil
		}
		return sessions.SessionFile{}, nil
	}
	hookNow = func() time.Time {
		if v := testClock.Load(); v != 0 {
			return time.Unix(0, v)
		}
		return time.Now()
	}
	hookSleep = func(d time.Duration) {
		if testClock.Load() != 0 {
			testClock.Add(int64(d))
			d = time.Millisecond
		}
		time.Sleep(d)
	}
	hookRedial = 20 * time.Millisecond
	hookPeerUID = func(c net.Conn) (uint32, error) { // otherPeer makes the listener another user
		if otherPeer.Load() {
			return uint32(os.Getuid()) + 1, nil
		}
		return peerUID(c)
	}
	openPreview = func(string) {}           // never a real browser
	sock, _ := os.MkdirTemp("/tmp", "ccsh") // a socket path must be short
	hookSockPath = func() string { return filepath.Join(sock, "h.sock") }
	claudeSettingsPath = func() string { return filepath.Join(dir, "claude-settings.json") }
	keyIdle = func() (time.Duration, bool) { return 0, false } // never the Mac's keyboard
	transcript.CodexHome = func() string { return filepath.Join(dir, "codex") }
	proc.PsAll = func() (string, error) { return "", nil }
	proc.PsTree = func() (string, error) { return "", nil }
	sessions.CallingSession = func() (sessions.Caller, bool) { return sessions.Caller{}, false } // never this machine's sessions
	proc.TTYOf = func(int) string { return "" }
	proc.SignalPID = func(int, syscall.Signal) error { return errors.New("tests signal nothing") }
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(sock)
	os.Exit(code)
}

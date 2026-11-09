package transcript

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-utils/ccs/internal/state"
)

// TestMain keeps tests off the machine's own files: logs, settings and
// Codex's data folder go to a temp folder, as in tui's TestMain.
func TestMain(m *testing.M) {
	for _, k := range []string{"TERM_PROGRAM", "LC_TERMINAL", "ITERM_SESSION_ID", "TMUX", "CODEX_THREAD_ID"} {
		os.Unsetenv(k)
	}
	dir, _ := os.MkdirTemp("", "ccs-test-transcript")
	state.UpdateLogPath = filepath.Join(dir, "update.log")
	state.ActivityLogPath = filepath.Join(dir, "activity.log")
	state.SettingsPath = filepath.Join(dir, "settings.json")
	state.MarksPath = filepath.Join(dir, "sessions.json")
	state.LaunchDir = func() string { return filepath.Join(dir, "launch") }
	CodexHome = func() string { return filepath.Join(dir, "codex") }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

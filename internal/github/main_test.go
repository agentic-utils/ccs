package github

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// TestMain keeps tests off the machine's own files and gh, as in tui's
// TestMain: logs, settings and the watcher's state go to a temp folder.
func TestMain(m *testing.M) {
	os.Setenv("CCS_NO_STATS", "1") // feature stats are tested in internal/stats; nothing else writes them
	dir, _ := os.MkdirTemp("", "ccs-test-github")
	state.UpdateLogPath = filepath.Join(dir, "update.log")
	state.ActivityLogPath = filepath.Join(dir, "activity.log")
	state.SettingsPath = filepath.Join(dir, "settings.json")
	state.MarksPath = filepath.Join(dir, "sessions.json")
	// ccs prs checks only the claims' pids (github.LiveAmong): through each test's LiveByPID stub.
	LiveAmong = func([]int) map[int]PRClaim { return LiveByPID() }
	Login = func() (string, error) { return "", errors.New("tests ask gh nothing") }
	// No test reads the machine's own transcripts for who opened or merged a PR.
	os.Unsetenv("ANTIGRAVITY_CONVERSATION_ID")
	os.Unsetenv("ANTIGRAVITY_APP_DATA_DIR")
	transcript.GetProjectsDir = func() string { return filepath.Join(dir, "projects") }
	transcript.AgyHome = func() string { return filepath.Join(dir, "agy") }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

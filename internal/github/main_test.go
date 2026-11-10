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
	dir, _ := os.MkdirTemp("", "ccs-test-github")
	state.UpdateLogPath = filepath.Join(dir, "update.log")
	state.ActivityLogPath = filepath.Join(dir, "activity.log")
	state.SettingsPath = filepath.Join(dir, "settings.json")
	state.MarksPath = filepath.Join(dir, "sessions.json")
	Login = func() (string, error) { return "", errors.New("tests ask gh nothing") }
	// No test reads the machine's own transcripts for who opened or merged a PR.
	transcript.GetProjectsDir = func() string { return filepath.Join(dir, "projects") }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

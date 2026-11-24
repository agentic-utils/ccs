package profiling

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-utils/ccs/internal/state"
)

var sockDir string

// TestMain keeps tests out of the real settings folder and logs. The
// sockets get a short folder of their own: a temp folder is too long for a
// socket path on macOS.
func TestMain(m *testing.M) {
	os.Setenv("CCS_NO_STATS", "1")
	dir, _ := os.MkdirTemp("", "ccs-test-profiling")
	state.SettingsPath = filepath.Join(dir, "settings.json")
	state.UpdateLogPath = filepath.Join(dir, "logs", "update.log")
	state.ActivityLogPath = filepath.Join(dir, "logs", "activity.log")
	sockDir, _ = os.MkdirTemp("/tmp", "ccsp")
	SockDir = func() string { return filepath.Join(sockDir, "p") }
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(sockDir)
	os.Exit(code)
}

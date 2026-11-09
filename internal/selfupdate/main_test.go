package selfupdate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-utils/ccs/internal/state"
)

// TestMain keeps tests out of the real update and activity logs, as in tui's
// TestMain.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "ccs-test-selfupdate")
	state.UpdateLogPath = filepath.Join(dir, "update.log")
	state.ActivityLogPath = filepath.Join(dir, "activity.log")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

package selfupdate

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-utils/ccs/internal/state"
)

// TestMain keeps tests out of the real update and activity logs and trusts
// testReleaseKey instead of the release key, as in tui's TestMain.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "ccs-test-selfupdate")
	state.UpdateLogPath = filepath.Join(dir, "update.log")
	state.ActivityLogPath = filepath.Join(dir, "activity.log")
	ReleaseKey = testReleaseKey.Public().(ed25519.PublicKey)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

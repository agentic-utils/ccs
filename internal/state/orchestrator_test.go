package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOrchestratorFallsBackToCoordinator(t *testing.T) {
	old := SettingsPath
	SettingsPath = filepath.Join(t.TempDir(), "settings.json")
	t.Cleanup(func() { SettingsPath = old })

	if _, _, err := ReadOrchestrator(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("none: %v", err)
	}
	os.WriteFile(LegacyOrchestratorPath(), []byte("old"), 0o600)
	if b, legacy, err := ReadOrchestrator(); err != nil || !legacy || string(b) != "old" {
		t.Fatalf("coordinator.json only: %q %v %v", b, legacy, err)
	}
	os.WriteFile(OrchestratorPath(), []byte("new"), 0o600)
	if b, legacy, err := ReadOrchestrator(); err != nil || legacy || string(b) != "new" {
		t.Errorf("both: %q %v %v", b, legacy, err)
	}
}

package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// LegacyOrchestratorName is what the orchestrator was called before it was
// renamed: ccs coordinator, coordinator.json, the coordinator folder and the
// session name ccs gave it. Kept so that setups from then go on working.
const LegacyOrchestratorName = "coordinator"

// OrchestratorPath is the orchestrator's record, beside the settings.
func OrchestratorPath() string {
	return filepath.Join(filepath.Dir(SettingsPath), "orchestrator.json")
}

// LegacyOrchestratorPath is the record a ccs from before the rename wrote.
func LegacyOrchestratorPath() string {
	return filepath.Join(filepath.Dir(SettingsPath), LegacyOrchestratorName+".json")
}

// ReadOrchestrator reads the record: orchestrator.json, else coordinator.json
// (legacy true) when there's no orchestrator.json yet. A missing record is an
// fs.ErrNotExist error.
func ReadOrchestrator() (data []byte, legacy bool, err error) {
	data, err = os.ReadFile(OrchestratorPath())
	if !errors.Is(err, fs.ErrNotExist) {
		return data, false, err
	}
	data, err = os.ReadFile(LegacyOrchestratorPath())
	return data, err == nil, err
}

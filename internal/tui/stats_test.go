package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
)

func TestCLICommandsAreRegisteredStats(t *testing.T) {
	for cmd, f := range cliCommands {
		if !stats.Known(f) {
			t.Errorf("ccs %s counts as %q, which isn't registered", cmd, f)
		}
	}
	for _, f := range snoozeFeatures {
		if !stats.Known(f) {
			t.Errorf("snooze choice %q isn't registered", f)
		}
	}
	if len(snoozeFeatures) != len(snoozeChoices(time.Now())) {
		t.Errorf("%d snooze features for %d choices", len(snoozeFeatures), len(snoozeChoices(time.Now())))
	}
}

// The list rewrites settings.json on every toggle; ccs stats --off's key
// must survive that.
func TestSaveSettingsKeepsFeatureStats(t *testing.T) {
	old := state.SettingsPath
	state.SettingsPath = t.TempDir() + "/settings.json"
	t.Cleanup(func() { state.SettingsPath = old })
	os.WriteFile(state.SettingsPath, []byte(`{"feature_stats":false,"agy_trusted_roots":["~/Code"],"table":true,"grouped":true}`), 0o600)
	saveSettings(settings{Table: false, Grouped: true})
	b, _ := os.ReadFile(state.SettingsPath)
	s := string(b)
	if !strings.Contains(s, `"feature_stats":false`) {
		t.Errorf("feature_stats lost: %s", s)
	}
	if !strings.Contains(s, `"agy_trusted_roots":["~/Code"]`) {
		t.Errorf("agy_trusted_roots lost: %s", s)
	}
	if strings.Contains(s, `"table"`) || !strings.Contains(s, `"grouped":true`) {
		t.Errorf("the list's own keys not replaced: %s", s)
	}
}

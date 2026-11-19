package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/selfupdate"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func stubHeldCount(t *testing.T, n *int) {
	t.Helper()
	old := heldCount
	heldCount = func() int { return *n }
	t.Cleanup(func() { heldCount = old })
}

// An automatic update never restarts ccs while it holds a prompt: the
// restart would send it back to its tab. It goes ahead once none is held.
func TestAutoUpdateWaitsWhileHolding(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	held := 1
	stubHeldCount(t, &held)
	installs := 0
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.autoUpdate = true
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { installs++; return "/bin/ccs", nil }}
	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	m.blurred = true // away: only the held prompt stops it

	if m.awayForUpdate() {
		t.Error("awayForUpdate must be false while a prompt is held")
	}
	res, cmd := m.Update(autoUpdateTickMsg{})
	if m = res.(model); m.updating || cmd == nil {
		t.Fatal("holding a prompt: no install, check again later")
	}
	res, _ = m.Update(autoUpdateTickMsg{})
	m = res.(model)
	if log, _ := os.ReadFile(state.ActivityLogPath); strings.Count(string(log), "update waits: holding 1 prompt(s)") != 1 {
		t.Errorf("the wait is logged once:\n%s", log)
	}

	held = 0
	res, cmd = m.Update(autoUpdateTickMsg{})
	if m = res.(model); !m.updating || !m.autoInstalling {
		t.Fatal("nothing held and away: install")
	}
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); installs != 1 || m.restart != "/bin/ccs" {
		t.Error("installed and restarting")
	}
}

// Enter on the offer with prompts held warns first; a second Enter soon
// after restarts.
func TestManualUpdateWarnsWhileHolding(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	held := 2
	stubHeldCount(t, &held)
	installs := 0
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { installs++; return "/bin/ccs", nil }}
	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	if !m.updateOpen {
		t.Fatal("the offer is open")
	}
	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)

	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); m.updating || !m.updateOpen || !strings.Contains(m.updatePopup(), "sends 2 held question(s) back to their tabs") {
		t.Fatalf("the first enter warns and installs nothing:\n%s", m.updatePopup())
	}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); !m.updating || cmd == nil {
		t.Fatal("a second enter within the window restarts")
	}
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); installs != 1 {
		t.Error("installed")
	}

	// A second enter long after the warning warns again.
	m2 := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m2.width, m2.height = 100, 30
	m2.upgrade = m.upgrade
	res, _ = m2.Update(latestMsg{tag: "v0.25.0"})
	m2 = res.(model)
	m2.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 = res.(model)
	m2.updateConfirmAt = time.Now().Add(-2 * heldRestartConfirm)
	res, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m2 = res.(model); m2.updating {
		t.Error("an enter after the window warns again rather than restarting")
	}
}

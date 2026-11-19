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

// Enter on the offer with prompts held says they come back after the
// restart, and installs at once: no second Enter.
func TestManualUpdateWhileHolding(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	held, installs := 2, 0
	stubHeldCount(t, &held)
	m := updateModel(t, &installs)
	if !strings.Contains(m.updatePopup(), "2 held question(s) come back once ccs restarts") {
		t.Errorf("the offer says held questions come back:\n%s", m.updatePopup())
	}
	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); !m.updating || cmd == nil {
		t.Fatal("one Enter installs")
	}
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); installs != 1 || !m.quitting || m.restart != "/bin/ccs" {
		t.Error("installed and restarting")
	}
}

// updateModel is a ccs offered v0.25.0, installing to /bin/ccs.
func updateModel(t *testing.T, installs *int) model {
	t.Helper()
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { *installs++; return "/bin/ccs", nil }}
	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	return res.(model)
}

// A prompt held while the automatic install ran: the restart waits for it,
// the header says so, and it restarts once none is held.
func TestAutoUpdateArrivalDuringInstall(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	held, installs := 0, 0
	stubHeldCount(t, &held)
	m := updateModel(t, &installs)
	m.autoUpdate, m.blurred, m.updateOpen = true, true, false
	res, cmd := m.Update(autoUpdateTickMsg{})
	if m = res.(model); !m.updating {
		t.Fatal("away and nothing held: installing")
	}
	held = 1 // a question arrives while it installs
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); m.quitting || m.restart != "" || m.restartReady != "/bin/ccs" {
		t.Fatalf("installed but not restarted while a question is held: quitting %v restart %q ready %q", m.quitting, m.restart, m.restartReady)
	}
	if h := strip2(m.View()); !strings.Contains(h, "update waiting: 1 held") {
		t.Errorf("the header says the update waits:\n%s", h)
	}
	res, _ = m.Update(autoUpdateTickMsg{})
	if m = res.(model); m.quitting {
		t.Fatal("still held: no restart")
	}
	held = 0
	res, _ = m.Update(autoUpdateTickMsg{})
	if m = res.(model); !m.quitting || m.restart != "/bin/ccs" || installs != 1 {
		t.Errorf("nothing held: restart into the ready install, without installing again (installs %d)", installs)
	}
}

// By hand: a question arriving during the install doesn't stop the
// restart, since it comes back afterwards.
func TestManualUpdateArrivalDuringInstall(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	held, installs := 0, 0
	stubHeldCount(t, &held)
	m := updateModel(t, &installs)
	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); !m.updating {
		t.Fatal("Enter installs")
	}
	held = 1
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); !m.quitting || m.restart != "/bin/ccs" || installs != 1 {
		t.Errorf("restarts into the install (installs %d)", installs)
	}
}

// A failed install's retry says the same about held questions.
func TestRetryAfterFailureShowsTheHeldNote(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	held, installs := 1, 0
	stubHeldCount(t, &held)
	m := updateModel(t, &installs)
	m.updateErr = "network down"
	if p := m.updatePopup(); !strings.Contains(p, "1 held question(s) come back once ccs restarts") || !strings.Contains(p, "retry") {
		t.Errorf("the retry offer has the note:\n%s", p)
	}
}

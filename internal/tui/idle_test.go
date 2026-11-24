package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
)

func TestIdleWhenUnfocusedOrUnused(t *testing.T) {
	m := initialModel(nil, "", nil)
	if m.idle() {
		t.Fatal("just opened: in use")
	}
	m.usedAt = time.Now().Add(-idleAfter)
	if !m.idle() {
		t.Fatal("no key for a minute: idle")
	}
	m.usedAt = time.Now()
	m.blurred = true
	if !m.idle() {
		t.Fatal("not focused: idle")
	}
}

func TestWakeRestartsTheTicksAtFullSpeed(t *testing.T) {
	t.Cleanup(func() { sessions.Frugal.Store(false) })
	m := initialModel(nil, "", nil)
	m.reload = func() ([]listItem, error) { return nil, nil }
	m.usedAt = time.Now().Add(-2 * idleAfter)
	res, _ := m.Update(previewSettledMsg{})
	m = res.(model)
	if !sessions.Frugal.Load() {
		t.Fatal("idle: the live tick's lookups go frugal")
	}
	m.chatTickOn = true // a slow fast tick pending
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = res.(model)
	if sessions.Frugal.Load() || m.idle() {
		t.Fatal("a key: in use again")
	}
	if m.liveGen != 1 || m.chatGen != 1 || cmd == nil {
		t.Fatalf("a wake starts new chains: live %d chat %d", m.liveGen, m.chatGen)
	}
	if _, c := m.Update(liveTickMsg{gen: 0}); c != nil {
		t.Error("the old live chain's tick is dropped")
	}
	if _, c := m.Update(chatTickMsg{gen: 0}); c != nil {
		t.Error("the old fast chain's tick is dropped")
	}
	m.chatTickOn = true
	res, _ = m.Update(chatTickResult{tickGen: 0})
	if !res.(model).chatTickOn {
		t.Error("the old fast chain's late read doesn't start a second chain")
	}
	res, _ = m.Update(liveMsg{tickGen: 0})
	if res.(model).liveGen != 1 {
		t.Error("the old live chain's late read is applied, nothing more")
	}
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if res.(model).liveGen != 1 {
		t.Error("only a wake from idle starts new chains")
	}
	_ = cmd
}

package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// Idle back-off: while nobody is using ccs, its background work slows, since
// every program it starts and every process it lists costs battery. The
// first key, click or focus brings it back to full speed at once.
const (
	idleAfter        = time.Minute      // no key, mouse report or focus for this long
	liveIdleInterval = 10 * time.Second // the live tick, LiveInterval in use
	chatIdleRefresh  = 2 * time.Second  // the fast tick, chatFastRefresh in use
)

// idle: ccs isn't focused, or hasn't been used for idleAfter.
func (m model) idle() bool {
	return m.blurred || time.Since(m.usedAt) >= idleAfter
}

// wakeIfUsed notes a key, mouse report or focus as use and, when ccs was
// idle before it, starts both tick chains afresh at full speed (the old ones
// stop at their next step). It also keeps sessions.Frugal in step.
func (m *model) wakeIfUsed(msg tea.Msg, wasIdle bool) tea.Cmd {
	switch msg.(type) {
	case tea.KeyMsg, tea.MouseMsg, tea.FocusMsg:
		m.usedAt = time.Now()
	}
	idle := m.idle()
	sessions.Frugal.Store(idle)
	if !wasIdle || idle || m.quitting {
		return nil
	}
	m.chatGen++
	m.chatTickOn = false // Update's ensureChatTick starts the new chain
	if m.reload == nil {
		return nil // no live tick runs (Init starts it only with a reload: tests)
	}
	m.liveGen++
	gen := m.liveGen
	return func() tea.Msg { return liveTickMsg{gen} }
}

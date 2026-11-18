package tui

// ^X B: start or stop ccs serve's launchd agent (`ccs serve install` writes
// it). The TUI never hosts ccs serve itself: it restarts on every
// auto-update and stops when it quits, and remote access mustn't. The
// header shows `^X B browser on` while launchd says the agent runs.

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// serveCheckEvery is the least time between reads of the agent's state.
const serveCheckEvery = 10 * time.Second

// serveStatus reads the agent's state; stubbed in tests.
var serveStatus = mcp.ServeServiceStatus

// startServe and stopServe run `ccs serve start|stop`; stubbed in tests.
var (
	startServe = mcp.StartServeService
	stopServe  = mcp.StopServeService
)

type serveStatusMsg struct{ st mcp.ServiceStatus }

type serveDoneMsg struct {
	text string
	err  error
}

// serveCheckCmd reads the agent's state off the UI goroutine, at most every
// serveCheckEvery; nil when it's not due.
func (m *model) serveCheckCmd() tea.Cmd {
	if m.serveChecking || time.Since(m.serveCheckedAt) < serveCheckEvery {
		return nil
	}
	m.serveChecking = true
	return func() tea.Msg { return serveStatusMsg{serveStatus()} }
}

// serveKey is ^X B: a note when there's no agent, a y/n before stopping it,
// else it starts it.
func (m *model) serveKey() tea.Cmd {
	switch {
	case !m.serve.Installed:
		m.note("^X B runs ccs serve in the background: set it up first with ccs serve install <flags> (docs/remote.md)")
		return nil
	case m.serve.Running:
		m.confirmServeStop = true
		return nil
	}
	return func() tea.Msg {
		text, err := startServe()
		return serveDoneMsg{text, err}
	}
}

func stopServeCmd() tea.Cmd {
	return func() tea.Msg {
		text, err := stopServe()
		return serveDoneMsg{text, err}
	}
}

// applyServe handles the agent's messages.
func (m *model) applyServe(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case serveStatusMsg:
		m.serve, m.serveChecking, m.serveCheckedAt = msg.st, false, time.Now()
	case serveDoneMsg:
		if msg.err != nil {
			m.errorMsg = msg.err.Error()
		} else {
			m.note(msg.text)
		}
		m.serveCheckedAt = time.Time{} // read it again at once
		return m.serveCheckCmd()
	}
	return nil
}

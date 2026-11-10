package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
)

// ccs launch and ccs close: start a named claude (or codex) session in a new
// iTerm tab, and close one by name. Also behind ^X N / ^X K in the list and
// the launch_session / close_session MCP tools.

type launchDoneMsg struct {
	name string
	l    sessions.Launched
	err  error
}

// launchCmd starts o off the UI goroutine: launch waits for the session.
func launchCmd(o sessions.LaunchOpts) tea.Cmd {
	return func() tea.Msg {
		l, err := sessions.LaunchSession(o)
		return launchDoneMsg{o.Name, l, err}
	}
}

type closeDoneMsg struct {
	text string
	err  error
}

// closeCmd stops the session on pid and closes its tab, off the UI goroutine.
// restartCmd restarts the session called name (^X R, after y), force when
// it was mid-turn when asked.
func restartCmd(name string, force bool) tea.Cmd {
	return func() tea.Msg {
		text, err := restartSession(sessions.RestartOpts{Name: name, Force: force})
		return closeDoneMsg{text, err}
	}
}

// restartSession is sessions.RestartSession, stubbed in tests.
var restartSession = sessions.RestartSession

func closeCmd(name string, pid int) tea.Cmd {
	return func() tea.Msg {
		text, err := sessions.CloseSession(name, pid)
		return closeDoneMsg{text, err}
	}
}

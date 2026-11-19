package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// Out-of-date hooks: Claude Code's settings run some of ccs's hooks but not
// all, because they were installed by a ccs that had fewer. The header says
// so, dim and static, until `ccs hook install` adds the rest. ccs only ever
// reads the settings here: installing is the user's to do.

// hooksOutOfDate is mcp.HooksOutOfDate, stubbed in tests.
var hooksOutOfDate = mcp.HooksOutOfDate

// hooksMsg is the hooks missing from the settings, nil when none are (or
// none of ccs's are there at all).
type hooksMsg struct{ missing []string }

// checkHooksCmd reads the settings off the UI goroutine: from each refresh
// (once a minute) and when ccs gets the terminal's focus.
func checkHooksCmd() tea.Msg { return hooksMsg{missing: hooksOutOfDate()} }

// hooksNote is the header's note while hooks are missing.
func (m model) hooksNote() string {
	if len(m.hooksMissing) == 0 {
		return ""
	}
	return "\033[90mhooks out of date: run ccs hook install\033[0m"
}

package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ccs launch and ccs close: start a named claude (or codex or agy) session in a new
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

// handoverCmd asks the session called name (still id) to hand its
// leftovers to its parent, then closes it (^X W, after y), off the UI
// goroutine: it waits until the session goes idle after the notice.
func handoverCmd(name, id string) tea.Cmd {
	return func() tea.Msg {
		text, err := closeHandover(name, id, 0, nil)
		return closeDoneMsg{text, err}
	}
}

// handoverParent and closeHandover are sessions.HandoverParentName and
// sessions.CloseHandover, stubbed in tests.
var (
	handoverParent = sessions.HandoverParentName
	closeHandover  = sessions.CloseHandover
)

func closeCmd(name string, pid int, id string) tea.Cmd {
	return func() tea.Msg {
		text, err := sessions.CloseSession(name, pid, id)
		return closeDoneMsg{text, err}
	}
}

// launchAgent is one agent ^X N can start: its id (saved as launch_agent),
// the name its row shows and the CLI it runs.
type launchAgent struct{ id, label, bin string }

// launchAgents are the agents ^X N offers, in the order its row shows them.
// Gemini is the Antigravity CLI, agy.
var launchAgents = []launchAgent{
	{"claude", "Claude", "claude"},
	{transcript.KindCodex, "Codex", "codex"},
	{transcript.KindAgy, "Gemini", "agy"},
}

// availableAgents are the launchAgents whose CLI is on PATH.
func availableAgents() []launchAgent {
	var out []launchAgent
	for _, a := range launchAgents {
		if _, err := sessions.LookBin(a.bin); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// agentIndex is id's place in agents, else 0: the first offered, Claude
// when it's installed.
func agentIndex(agents []launchAgent, id string) int {
	for i, a := range agents {
		if a.id == id {
			return i
		}
	}
	return 0
}

// launchAgentNow is the agent ^X N has chosen.
func (m model) launchAgentNow() launchAgent {
	if m.launchAgentIdx < 0 || m.launchAgentIdx >= len(m.launchAgents) {
		return launchAgents[0]
	}
	return m.launchAgents[m.launchAgentIdx]
}

// cycleAgent moves ^X N's choice by d, wrapping.
func (m *model) cycleAgent(d int) {
	if n := len(m.launchAgents); n > 1 {
		m.launchAgentIdx = ((m.launchAgentIdx+d)%n + n) % n
	}
}

// agentGlyph is the agent's list glyph in its colour.
func agentGlyph(id string) string {
	g, c := transcript.AgentGlyph(transcript.Conversation{Kind: id})
	return "\033[" + c + "m" + g + "\033[0m"
}

// agentRow is ^X N's agent choice as one row: every installed agent, the
// chosen one bold and underlined.
func (m model) agentRow() string {
	parts := make([]string, len(m.launchAgents))
	for i, a := range m.launchAgents {
		label := a.label
		if i == m.launchAgentIdx {
			label = "\033[1;4m" + label + "\033[0m"
		} else {
			label = "\033[90m" + label + "\033[0m"
		}
		parts[i] = agentGlyph(a.id) + " " + label
	}
	return "Agent: " + strings.Join(parts, "   ")
}

// launchOptsFor is ^X N's launch with the chosen agent.
func launchOptsFor(a launchAgent, name, brief, cwd string) sessions.LaunchOpts {
	return sessions.LaunchOpts{Name: name, Brief: brief, Cwd: cwd, Codex: a.id == transcript.KindCodex, Agy: a.id == transcript.KindAgy}
}

// launchLine is ^X N's row under the header: the agent choice at step 1,
// then the name or brief box with the agent it will start.
func (m model) launchLine() string {
	where := "\033[90min " + sessions.TildePath(m.launchCwd) + "  "
	if m.launchStep == 1 {
		return "  " + m.agentRow() + "  " + where + hints("←/→", "agent", keyEnter, "next", keyEsc, "cancel") + "\033[0m"
	}
	a := m.launchAgentNow()
	next := map[bool]string{true: "next", false: "start it"}[m.launchStep == 2]
	keys := []string{keyEnter, next, keyEsc, "cancel"}
	if len(m.launchAgents) > 1 {
		keys = append([]string{keyTab, "agent"}, keys...)
	}
	return "  " + m.launchInput.View() + "  " + agentGlyph(a.id) + " \033[90m" + a.label + "  " + where + hints(keys...) + "\033[0m"
}

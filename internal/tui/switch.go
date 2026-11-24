package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Switching a session to another agent (sessions.SwitchSession): the key,
// the y/n prompt with the agent choice, and the command.

// switchLetter is the ^X chord that switches the selected session to another
// agent (A for agent), and the one place that names it.
const switchLetter = "A"

// switchKey is switchLetter as Update sees the chord, and switchHint as the
// help and hints show it.
var (
	switchKey  = "^x " + strings.ToLower(switchLetter)
	switchHint = "^X " + switchLetter
)

// switchSession is sessions.SwitchSession, stubbed in tests.
var switchSession = sessions.SwitchSession

// switchPrompt is ^X A's question: which session, the agents it can go to
// and the one chosen.
type switchPrompt struct {
	id, name string
	agents   []sessions.Agent
	idx      int
	force    bool // a Codex or agy session mid-turn: y stops it where it is
}

// switchName is the name a switch of conv runs under: its running name, its
// own title, else one made from its topic.
func (m model) switchName(conv transcript.Conversation) string {
	if st, ok := m.chatStatus[conv.SessionID]; ok && st.Name != "" {
		return st.Name
	}
	if conv.Title != "" {
		return sessions.SwitchName(conv.Title)
	}
	return sessions.SwitchName(transcript.GetTopic(conv))
}

// switchTargets are the installed agents conv can switch to: every one but
// its own.
func switchTargets(conv transcript.Conversation) []sessions.Agent {
	from := sessions.AgentOf(conv.Kind)
	var out []sessions.Agent
	for _, a := range sessions.Agents {
		if a.ID == from.ID {
			continue
		}
		if _, err := sessions.LookBin(a.Bin); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// switchRefusal is why ^X A can't switch conv, "" when it can.
func (m model) switchRefusal(conv transcript.Conversation) string {
	switch {
	case conv.Kind == transcript.KindSubagent:
		return switchHint + " switches a session, not a subagent"
	case !sessions.CanOpenTabs():
		return switchHint + " opens a tab in iTerm or tmux, and this is neither"
	case len(switchTargets(conv)) == 0:
		return "No other agent is installed to switch " + m.sessionName(conv) + " to"
	}
	id := conv.SessionID
	if m.live[id] {
		if _, bg := m.bg[id]; bg {
			return switchHint + " can't switch a background session: stop it first"
		}
		if _, ok := m.livePIDs[id]; !ok || m.hostedApp(id) != "" {
			return switchHint + " switches a session in a terminal tab, or one that isn't running; this one runs in an app"
		}
		if name := m.switchName(conv); sessions.SwitchName(name) != name {
			return switchHint + " needs a session name of letters, digits, '.', '_' and '-'; " + name + " isn't one"
		}
	}
	return ""
}

// switchKeyPressed is ^X A: ask which agent, and whether to.
func (m model) switchKeyPressed() (tea.Model, tea.Cmd) {
	stats.Record("tui.switch", stats.Key)
	m.helpOpen = false
	conv := m.filtered[m.cursor].conv
	if why := m.switchRefusal(conv); why != "" {
		m.errorMsg = why
		return m, nil
	}
	p := &switchPrompt{id: conv.SessionID, name: m.switchName(conv), agents: switchTargets(conv)}
	if conv.Kind != "" && m.live[conv.SessionID] {
		st := m.chatStatus[conv.SessionID].Status
		p.force = st == "busy" || st == "waiting"
	}
	m.confirmSwitch = p
	return m, nil
}

// switchConfirmKey handles a key while ^X A asks.
func (m model) switchConfirmKey(k string) (tea.Model, tea.Cmd) {
	p := m.confirmSwitch
	switch k {
	case "left", "shift+tab":
		p.idx = (p.idx - 1 + len(p.agents)) % len(p.agents)
	case "right", "tab":
		p.idx = (p.idx + 1) % len(p.agents)
	case "y", "Y":
		m.confirmSwitch = nil
		if !slices.ContainsFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == p.id }) {
			m.errorMsg = "It isn't listed any more"
			return m, nil
		}
		to := p.agents[p.idx]
		m.note(fmt.Sprintf("Switching %s to %s; a running Claude Code session writes its handover note first", p.name, to.Label))
		return m, switchCmd(sessions.SwitchOpts{Name: p.name, ID: p.id, To: to.ID, Force: p.force})
	case "n", "N", "esc":
		m.confirmSwitch = nil
	}
	return m, nil
}

// switchCmd switches off the UI goroutine: a running Claude Code session's
// handover note takes up to two minutes.
func switchCmd(o sessions.SwitchOpts) tea.Cmd {
	return func() tea.Msg {
		text, err := switchSession(o)
		return closeDoneMsg{text, err}
	}
}

// switchLine is ^X A's question under the header.
func (m model) switchLine() string {
	p := m.confirmSwitch
	parts := make([]string, len(p.agents))
	for i, a := range p.agents {
		label := "\033[90m" + a.Label + "\033[0m"
		if i == p.idx {
			label = "\033[1;4m" + a.Label + "\033[0m"
		}
		parts[i] = agentGlyph(a.ID) + " " + label
	}
	what, colour := fmt.Sprintf("Switch %q to another agent? It closes this session; the new one gets a brief, not its memory.", transcript.Truncate(p.name, 40)), "214" // Amber
	if p.force {
		what, colour = fmt.Sprintf("%q is mid-turn: switch it anyway, stopping it where it is? The new one gets a brief, not its memory.", transcript.Truncate(p.name, 40)), "196" // Red
	}
	keys := []string{"y", "switch", "n/" + keyEsc, "cancel"}
	if len(p.agents) > 1 {
		keys = append([]string{"←/→", "agent"}, keys...)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colour)).Render(what) + "  " + strings.Join(parts, "   ") + "  \033[90m" + hints(keys...) + "\033[0m"
}

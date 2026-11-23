package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
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

// agentGlyph is the agent's list glyph in its colour.
func agentGlyph(id string) string {
	g, c := transcript.AgentGlyph(transcript.Conversation{Kind: id})
	return "\033[" + c + "m" + g + "\033[0m"
}

// launchOptsFor is ^X N's launch with the chosen agent.
func launchOptsFor(a launchAgent, name, brief, cwd string) sessions.LaunchOpts {
	return sessions.LaunchOpts{Name: name, Brief: brief, Cwd: cwd, Codex: a.id == transcript.KindCodex, Agy: a.id == transcript.KindAgy}
}

// The new-session popup's fields, top to bottom.
const (
	launchFieldAgent = iota
	launchFieldName
	launchFieldBrief
	launchFields
)

// launchForm is ^X N's popup: every field on screen at once. It owns the
// keyboard while open, like ^X B's setup form.
type launchForm struct {
	agents      []launchAgent // those on PATH when it opened
	agent       int           // the one chosen, in agents
	focus       int           // the field with the keyboard
	name, brief textinput.Model
	cwd         string // where it starts: the selected session's folder
	err         string // why Enter didn't start it, shown in the popup
}

func newLaunchForm(agents []launchAgent, last, cwd string) *launchForm {
	f := &launchForm{agents: agents, agent: agentIndex(agents, last), cwd: cwd}
	f.name = sessions.LaunchPrompt("", "")
	f.brief = sessions.LaunchPrompt("", "")
	f.focusField(launchFieldAgent)
	if len(agents) == 1 { // nothing to choose
		f.focusField(launchFieldName)
	}
	return f
}

// focusField gives field i the keyboard, wrapping; the agent row is passed
// over when there's only one agent.
func (f *launchForm) focusField(i int) {
	f.focus = (i + launchFields) % launchFields
	f.name.Blur()
	f.brief.Blur()
	switch f.focus {
	case launchFieldName:
		f.name.Focus()
	case launchFieldBrief:
		f.brief.Focus()
	}
}

// move goes d fields along, skipping the agent row with one agent.
func (f *launchForm) move(d int) {
	f.focusField(f.focus + d)
	if f.focus == launchFieldAgent && len(f.agents) < 2 {
		f.focusField(f.focus + d)
	}
}

// chosen is the agent the form will start.
func (f *launchForm) chosen() launchAgent {
	if f.agent < 0 || f.agent >= len(f.agents) {
		return launchAgents[0]
	}
	return f.agents[f.agent]
}

// cycleAgent moves the choice by d, wrapping.
func (f *launchForm) cycleAgent(d int) {
	if n := len(f.agents); n > 1 {
		f.agent = ((f.agent+d)%n + n) % n
	}
}

// box is the focused text field, nil on the agent row.
func (f *launchForm) box() *textinput.Model {
	switch f.focus {
	case launchFieldName:
		return &f.name
	case launchFieldBrief:
		return &f.brief
	}
	return nil
}

// openLaunch is ^X N: the popup, starting in the selected session's folder
// on the agent chosen last.
func (m *model) openLaunch() {
	if !sessions.CanOpenTabs() {
		m.errorMsg = "^X N opens the new session in an iTerm tab or tmux window, and this is neither"
		return
	}
	agents := availableAgents()
	if len(agents) == 0 {
		m.errorMsg = "^X N starts claude, codex or agy, and none is on PATH"
		return
	}
	cwd, _ := os.Getwd()
	if len(m.filtered) > 0 {
		if c := m.filtered[m.cursor].conv.Cwd; c != "" && c != "unknown" {
			cwd = c
		}
	}
	m.launch, m.errorMsg = newLaunchForm(agents, m.launchAgent, cwd), ""
}

// launchKey handles a key while the popup is open. Tab and ↑/↓ move between
// fields, ←/→ change the agent on its row (and the cursor in a text box),
// Enter goes to the next field and starts it from the brief, Esc cancels.
func (m model) launchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.launch
	switch k := msg.String(); k {
	case "esc", "ctrl+c":
		m.launch = nil
		return m, nil
	case "tab", "down":
		f.move(1)
		return m, nil
	case "shift+tab", "up":
		f.move(-1)
		return m, nil
	case "left", "right":
		if f.focus == launchFieldAgent {
			f.cycleAgent(map[string]int{"right": 1, "left": -1}[k])
			return m, nil
		}
	case "enter":
		if f.focus != launchFieldBrief {
			f.move(1)
			return m, nil
		}
		return m.startLaunch()
	}
	if b := f.box(); b != nil {
		var cmd tea.Cmd
		*b, cmd = editKey(*b, msg)
		f.err = ""
		return m, cmd
	}
	return m, nil // the agent row takes no text
}

// startLaunch checks the form and starts the session; a bad field gets the
// keyboard with the reason in the popup.
func (m model) startLaunch() (tea.Model, tea.Cmd) {
	f := m.launch
	name, brief := strings.TrimSpace(f.name.Value()), strings.TrimSpace(f.brief.Value())
	switch {
	case !state.SessionName.MatchString(name):
		f.err = "A session name is letters, digits, '.', '_' and '-'"
		f.focusField(launchFieldName)
		return m, nil
	case brief == "":
		f.err = "Type its first prompt"
		return m, nil
	}
	a := f.chosen()
	m.launch = nil
	if a.id != m.launchAgent {
		m.launchAgent = a.id
		saveSettings(m.settings())
	}
	m.note("Starting " + name + " (" + a.label + ")…")
	return m, launchCmd(launchOptsFor(a, name, brief, f.cwd))
}

// launchPopup draws the new-session form.
func (m model) launchPopup() string {
	f := m.launch
	dim := func(s string) string { return "\033[90m" + s + "\033[0m" }
	width := max(min(m.width-14, 80), 40)
	name, brief := f.name, f.brief // drawn at this width; the form keeps its own
	name.Width, brief.Width = width-10, width-10
	var b strings.Builder
	b.WriteString("\033[1mNew session\033[0m\n\n")
	mark := func(i int) string {
		if i == f.focus {
			return "\033[1;36m❯\033[0m "
		}
		return "  "
	}
	parts := make([]string, len(f.agents))
	for i, a := range f.agents {
		label := dim(a.label)
		if i == f.agent {
			label = "\033[1;4m" + a.label + "\033[0m"
		}
		parts[i] = agentGlyph(a.id) + " " + label
	}
	b.WriteString(mark(launchFieldAgent) + "Agent  " + strings.Join(parts, "   ") + "\n")
	b.WriteString(mark(launchFieldName) + "Name   " + name.View() + "\n")
	b.WriteString(mark(launchFieldBrief) + "Brief  " + brief.View() + "\n")
	b.WriteString("\n" + dim("in "+sessions.TildePath(f.cwd)) + "\n")
	if f.err != "" {
		b.WriteString("\n" + lipgloss.NewStyle().Width(width).Render("\033[31m"+f.err+"\033[0m") + "\n")
	}
	keys := []string{keyTab + " ↑↓", "field"}
	if len(f.agents) > 1 {
		keys = append(keys, "←/→", "agent")
	}
	enter := "next"
	if f.focus == launchFieldBrief {
		enter = "start it"
	}
	keys = append(keys, keyEnter, enter, keyEsc, "cancel")
	b.WriteString("\n" + dim(hints(keys...)))
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 2).
		Render(b.String())
}

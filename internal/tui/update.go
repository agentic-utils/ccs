package tui

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/selfupdate"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// How often ccs asks GitHub for a newer release (one ~5KB HEAD request to the
// releases/latest redirect), and how long it waits after a failed check, so
// a throttled or offline check doesn't retry every interval from a shared IP.
const (
	updateCheckInterval = 2 * time.Minute
	updateCheckBackoff  = 10 * time.Minute
)

func updatingTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return updatingTickMsg{} })
}

// updateKeyGrace is how long a freshly opened update popup ignores keys.
const updateKeyGrace = time.Second

type updateCheckTickMsg struct{}

type updatingTickMsg struct{}

// updateProgress is what the header shows while an upgrade runs. The upgrade
// runs off the UI goroutine, hence the lock.
type updateProgress struct {
	mu      sync.Mutex
	step    string
	started time.Time
}

func (p *updateProgress) set(step string) {
	p.mu.Lock()
	p.step = step
	p.mu.Unlock()
}

func (p *updateProgress) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("%s (%ds)", p.step, int(time.Since(p.started).Seconds()))
}

type latestMsg struct {
	tag string
	err error
}

type notesMsg struct {
	lines []string
	err   error
}

type changelogMsg struct {
	tag   string
	lines []string
}

type upgradeDoneMsg struct {
	path string
	err  error
}

func (m model) checkUpdateCmd() tea.Cmd {
	check := m.checkLatest
	return func() tea.Msg {
		tag, err := check()
		return latestMsg{tag, err}
	}
}

// restoreEnv carries uiState across the restart after a self-update.
const restoreEnv = "CCS_RESTORE_STATE"

// uiState is where the user was: which screen, the search text and cursor
// position within it, the selected conversation and the preview scroll.
// Screen is a name, so a screen added later only needs a case in
// screenName and restore.
type uiState struct {
	Screen        string               `json:"screen"`
	Query         string               `json:"query"`
	QueryCursor   int                  `json:"query_cursor"`
	Selected      string               `json:"selected"` // SessionID, so it survives a reordered list
	PreviewScroll int                  `json:"preview_scroll"`
	Snoozes       map[string]snoozeRec `json:"snoozes,omitempty"`  // input-prompt overlay snoozes, by SessionID
	Table         bool                 `json:"table,omitempty"`    // ^X T: the full table, not the sidebar
	Grouped       bool                 `json:"grouped,omitempty"`  // ^X G: grouped by folder
	Headless      *bool                `json:"headless,omitempty"` // ^X H: headless runs shown; nil (an older ccs) keeps --all's
}

func (m model) screenName() string {
	if m.showUsage {
		return "usage"
	}
	return "list"
}

func (m model) uiState() uiState {
	s := uiState{
		Screen:        m.screenName(),
		Query:         m.textInput.Value(),
		QueryCursor:   m.textInput.Position(),
		PreviewScroll: m.previewScroll,
		Snoozes:       m.snoozed,
		Table:         m.table,
		Grouped:       m.grouped,
		Headless:      new(bool),
	}
	*s.Headless = showHeadless
	if len(m.filtered) > 0 {
		s.Selected = m.filtered[m.cursor].conv.SessionID
	}
	return s
}

// restore puts the user back where uiState says; anything that no longer
// applies (a conversation that's gone) is skipped.
func (m *model) restore(s uiState) {
	m.textInput.SetValue(s.Query)
	m.textInput.SetCursor(s.QueryCursor)
	m.grouped = s.Grouped // before updateFilter, which sorts
	if s.Headless != nil {
		showHeadless = *s.Headless
	}
	m.updateFilter()
	for i, item := range m.filtered {
		if item.conv.SessionID == s.Selected {
			m.cursor = i
			break
		}
	}
	// Not clamped here: the window size isn't known yet at startup; the
	// preview clamps it when drawing.
	m.previewScroll = max(0, s.PreviewScroll)
	m.showUsage = s.Screen == "usage"
	m.snoozed = s.Snoozes
	m.table = s.Table
}

// notesCmd fetches the changelog off the UI goroutine.
func (m *model) notesCmd() tea.Cmd {
	m.notesFetching = true
	f := m.fetchNotes
	return func() tea.Msg { lines, err := f(); return notesMsg{lines, err} }
}

// notesRows is how many changelog lines the popup shows at once.
func (m model) notesRows() int { return max(m.height-10, 3) }

func (m *model) scrollNotes(by int) {
	m.notesScroll = min(max(m.notesScroll+by, 0), max(len(m.notes)-m.notesRows(), 0))
}

// notesPopup is the Ctrl+L changelog: every release in the feed, newest first,
// the installed one and any newer marked, scrolled by notesScroll.
func (m model) notesPopup() string {
	width := min(max(m.width-12, 30), 90)
	var body []string
	switch {
	case m.notesErr != "":
		body = []string{"couldn't fetch the changelog: " + transcript.Truncate(m.notesErr, width-30), "", "\033[90m" + hints(keyEnter, "retry") + "\033[0m"}
	case m.notes == nil:
		body = []string{"fetching…"}
	default:
		end := min(m.notesScroll+m.notesRows(), len(m.notes))
		for _, l := range m.notes[m.notesScroll:end] {
			if strings.HasPrefix(l, " ") {
				body = append(body, ansi.Truncate(l, width, "…"))
				continue
			}
			mark := ""
			if selfupdate.NewerVersion(l, state.Version) {
				mark = " \033[32m(new)\033[0m"
			} else if strings.TrimPrefix(l, "v") == state.Version {
				mark = " \033[90m(installed)\033[0m"
			}
			body = append(body, ansi.Truncate("\033[1m"+l+"\033[22m"+mark, width, "…"))
		}
	}
	for len(body) < min(m.notesRows(), max(len(m.notes), 1)) {
		body = append(body, "") // a steady height while scrolling
	}
	pos, keys := "", hints("^L/"+keyEsc, "close")
	if len(m.notes) > m.notesRows() {
		keys = hints("↑↓", "scroll", "^L/"+keyEsc, "close")
		pos = fmt.Sprintf("  %d–%d of %d", m.notesScroll+1, min(m.notesScroll+m.notesRows(), len(m.notes)), len(m.notes))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Width(width + 6). // fixed, so scrolling past longer or shorter lines doesn't resize it
		Render("\033[1mchangelog\033[0m\n\n" + strings.Join(body, "\n") +
			"\n\n\033[90m" + keys + pos + "\033[0m")
}

// updatePopup renders the update offer; View overlays it on any screen.
func (m model) updatePopup() string {
	body := fmt.Sprintf("ccs %s is available (you have v%s).\n\n", m.updateTo, state.Version)
	if len(m.changelog) > 0 {
		body += m.changelogBlock() + "\n\n"
	}
	if m.updateErr != "" {
		body = fmt.Sprintf("Updating to %s failed:\n%s\nDetails: %s\n\n", m.updateTo, transcript.Truncate(m.updateErr, 60), state.UpdateLogPath)
	}
	if m.upgrade != nil && m.updateErr != "" {
		body += hints(keyEnter, "retry", keyEsc, "later")
	} else if m.upgrade != nil {
		body += hints(keyEnter, "update and restart", keyEsc, "later", "^L", "changelog")
	} else {
		body += "Update with your package manager.    " + hints(keyEsc, "close")
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(1, 3).
		Render(body)
	return box
}

// changelogBlock fits the changelog into the popup: lines cut to the popup's
// width, and at most maxChangelogLines of them, then how many more.
func (m model) changelogBlock() string {
	width := min(max(m.width-12, 30), 80)
	lines := m.changelog
	var more string
	if len(lines) > selfupdate.MaxChangelogLines {
		more = fmt.Sprintf("\n  … and %d more", len(lines)-selfupdate.MaxChangelogLines+1)
		lines = lines[:selfupdate.MaxChangelogLines-1]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Truncate(l, width, "…")
		if !strings.HasPrefix(l, " ") {
			out[i] = "\033[1m" + out[i] + "\033[22m"
		}
	}
	return strings.Join(out, "\n") + more
}

// autoUpdateTickMsg checks whether you're away, so a prepared update can
// install without interrupting you.
type autoUpdateTickMsg struct{}

type updatedChangelogMsg struct{ lines []string }

func autoUpdateTick() tea.Cmd {
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg { return autoUpdateTickMsg{} })
}

// autoUpdateIdle is how long without a key press counts as away, when the
// terminal doesn't say ccs lost focus.
const autoUpdateIdle = 3 * time.Minute

// updatedEnv tells the ccs started after an automatic update which version
// it updated from, so it can say so.
const updatedEnv = "CCS_UPDATED_FROM"

// awayForUpdate: an automatic update may restart ccs now. You're away (ccs
// isn't focused, or no key for autoUpdateIdle) and nothing would be lost or
// cut short: no prompt or popup open, no unsent text in any message box, no
// session or coordinator opening. The restart keeps the screen, search and selection.
func (m model) awayForUpdate() bool {
	if !m.blurred && time.Since(m.lastKeyAt) < autoUpdateIdle {
		return false
	}
	if m.prompting() || m.resuming || m.coordBusy || m.waitOpen || m.linksOpen || m.snoozeOpen || len(m.reopen) > 0 ||
		slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open }) {
		return false
	}
	if strings.TrimSpace(m.chatInput.Value()) != "" || strings.TrimSpace(m.waitInput.Value()) != "" {
		return false
	}
	for _, d := range m.chatDrafts {
		if strings.TrimSpace(d) != "" {
			return false
		}
	}
	for _, d := range m.waitDrafts {
		if strings.TrimSpace(d) != "" {
			return false
		}
	}
	return true
}

// startInstall installs m.updateTo (Enter on the offer, or automatically),
// showing its progress in the header.
func (m *model) startInstall() tea.Cmd {
	m.updating = true
	m.updateErr = ""
	m.progress = &updateProgress{step: "starting", started: time.Now()}
	upgrade, tag, progress := m.upgrade, m.updateTo, m.progress
	return tea.Batch(func() tea.Msg {
		path, err := upgrade.Install(tag, progress.set)
		return upgradeDoneMsg{path, err}
	}, updatingTick())
}

// updatedPopup says ccs updated itself while you were away, and what changed.
func (m model) updatedPopup() string {
	body := fmt.Sprintf("\033[1mccs updated itself\033[0m from v%s to v%s while you were away.\n\n", strings.TrimPrefix(m.updatedFrom, "v"), state.Version)
	if len(m.changelog) > 0 {
		body += m.changelogBlock() + "\n\n"
	}
	body += "\033[90m" + hints(keyEnter+"/"+keyEsc, "close", "^X I", "stop updating on its own") + "\033[0m"
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(body)
}

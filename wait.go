package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ============================================================================
// Input-prompt overlay: live sessions waiting on the user
// ============================================================================

// waitQuiet is how long after the last key a pop-up waits, so it never takes
// keys meant for the search or the message box.
const waitQuiet = 3 * time.Second

// defaultSnooze is how long Esc puts a waiting session aside (CCS_SNOOZE).
const defaultSnooze = 15 * time.Minute

func snoozeLength(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return defaultSnooze
}

// shortDuration drops zero tails: 15m0s reads 15m, 1h0m0s reads 1h.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// How a live session waits on the user.
const (
	waitNone   = iota
	waitPrompt // its session file says "waiting": a permission prompt or a question menu
	waitAsked  // idle, and its last message ends in "?": a guess, so it never pops up
)

func (m model) waitSignal(conv Conversation) int {
	if !m.live[conv.SessionID] {
		return waitNone
	}
	switch m.chatStatus[conv.SessionID].status {
	case "waiting":
		return waitPrompt
	case "idle":
		if n := len(conv.Messages); n > 0 && conv.Messages[n-1].Role == "assistant" &&
			strings.HasSuffix(strings.TrimSpace(conv.Messages[n-1].Text), "?") {
			return waitAsked
		}
	}
	return waitNone
}

// waitQueue is the overlay's sessions, oldest wait first so a snoozed one
// coming back can't be starved by newer ones; all adds the waitAsked ones
// after the prompts. Snoozed sessions are left out.
func (m model) waitQueue(all bool) []Conversation {
	var prompts, asked []Conversation
	now := time.Now()
	for _, item := range m.items {
		if now.Before(m.snoozed[item.conv.SessionID]) {
			continue
		}
		switch m.waitSignal(item.conv) {
		case waitPrompt:
			prompts = append(prompts, item.conv)
		case waitAsked:
			if all {
				asked = append(asked, item.conv)
			}
		}
	}
	for _, q := range [][]Conversation{prompts, asked} {
		sort.SliceStable(q, func(i, j int) bool {
			return m.chatStatus[q[i].SessionID].since.Before(m.chatStatus[q[j].SessionID].since)
		})
	}
	return append(prompts, asked...)
}

// setStat records a live session's status. Entering "waiting" marks a pop-up
// due; any change ends its snooze, which only ever covers one wait.
func (m *model) setStat(id string, st sessionStat) {
	if m.chatStatus == nil {
		m.chatStatus = make(map[string]sessionStat)
	}
	old := m.chatStatus[id]
	if st.status != old.status {
		delete(m.snoozed, id)
		if st.status == "waiting" {
			m.waitDue = true
		}
	}
	m.chatStatus[id] = st
}

// applyStats takes the live tick's statuses and pops the overlay when one is
// due and nothing else is on screen. At startup every waiting session counts
// as new, so ccs opens on the oldest.
func (m *model) applyStats(stats map[string]sessionStat) {
	if stats == nil {
		return
	}
	for id, st := range stats {
		m.setStat(id, st)
	}
	for id, until := range m.snoozed {
		if !time.Now().Before(until) {
			delete(m.snoozed, id)
			if m.chatStatus[id].status == "waiting" {
				m.waitDue = true
			}
		}
	}
	if m.waitDue && !m.waitOpen && m.canPop() && len(m.waitQueue(false)) > 0 {
		m.waitDue = false
		m.openWait(false)
	}
	if m.waitOpen {
		m.checkDelivered(m.waitID)
	}
}

// canPop: no other popup or prompt is open and the user isn't typing.
func (m model) canPop() bool {
	return !m.prompting() && !m.updateOpen && !m.acctOpen && !m.helpOpen && !m.notesOpen && !m.linksOpen &&
		time.Since(m.lastKey) >= waitQuiet
}

// openWait opens the overlay on the first session in its queue.
func (m *model) openWait(all bool) {
	q := m.waitQueue(all)
	if len(q) == 0 {
		m.errorMsg = "No session is waiting on you"
		return
	}
	m.waitOpen, m.waitAll, m.waitID, m.waitScroll = true, all, q[0].SessionID, 0
	m.chatFocus = false
}

// waitConv is the session the overlay shows, as last read.
func (m model) waitConv() (Conversation, bool) {
	for _, item := range m.items {
		if item.conv.SessionID == m.waitID {
			return item.conv, true
		}
	}
	return Conversation{}, false
}

// waitStep shows the session by places away in the queue (clamped); a
// session no longer in it starts from the top.
func (m *model) waitStep(by int) {
	q := m.waitQueue(m.waitAll)
	if len(q) == 0 {
		return
	}
	i := -1
	for j, c := range q {
		if c.SessionID == m.waitID {
			i = j
		}
	}
	if i < 0 {
		i = 0
	} else {
		i = max(0, min(i+by, len(q)-1))
	}
	if q[i].SessionID != m.waitID {
		m.waitID, m.waitScroll = q[i].SessionID, 0
	}
}

func (m model) waitKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	conv, ok := m.waitConv()
	switch msg.String() {
	case "ctrl+y":
		m.waitOpen = false
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc": // defer: snooze this one, show the one that was after it
		q := m.waitQueue(m.waitAll)
		i := 0
		for j, c := range q {
			if c.SessionID == m.waitID {
				i = j
			}
		}
		if m.snoozed == nil {
			m.snoozed = make(map[string]time.Time)
		}
		m.snoozed[m.waitID] = time.Now().Add(m.snooze)
		if q = m.waitQueue(m.waitAll); len(q) == 0 {
			m.waitOpen = false
		} else {
			m.waitID, m.waitScroll = q[min(i, len(q)-1)].SessionID, 0
		}
	case "ctrl+n", "down":
		m.waitStep(1)
	case "ctrl+p", "up":
		m.waitStep(-1)
	case "pgup", "ctrl+k":
		m.waitScroll = min(m.waitScroll+10, m.maxWaitScroll())
	case "pgdown", "ctrl+j":
		m.waitScroll = max(0, m.waitScroll-10)
	case "enter":
		if !ok || !m.live[conv.SessionID] {
			return m, nil
		}
		if strings.TrimSpace(m.waitInput.Value()) == "" { // go to its tab, e.g. to answer a menu
			m.waitOpen = false
			return m, m.resumeCmd(conv, false)
		}
		return m, m.sendTo(conv, &m.waitInput)
	default:
		var cmd tea.Cmd
		m.waitInput, cmd = m.waitInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// waitLayout: the overlay's outer width, its text width, and how many
// transcript rows fit (the rest: borders, title, menu note, message box, hints).
func (m model) waitLayout() (width, inner, rows int) {
	width = max(m.width*9/10, 30)
	return width, width - 4, max(m.height*9/10-2-1-1-4-1, 3)
}

func (m model) waitLines() []string {
	conv, ok := m.waitConv()
	if !ok {
		return nil
	}
	_, inner, _ := m.waitLayout()
	return cachedPreview(m.waitPreview, conv, "", inner)
}

func (m model) maxWaitScroll() int {
	_, _, rows := m.waitLayout()
	return max(0, len(m.waitLines())-rows)
}

// waitPopup draws the overlay: which session and why, its transcript (newest
// at the bottom), a message box and the keys.
func (m model) waitPopup() string {
	_, inner, rows := m.waitLayout()
	conv, ok := m.waitConv()
	q := m.waitQueue(m.waitAll)
	pos := -1
	for i, c := range q {
		if c.SessionID == m.waitID {
			pos = i
		}
	}
	title := "\033[90mno longer waiting\033[0m"
	if pos >= 0 {
		title = fmt.Sprintf("\033[1;33mwaiting %d of %d\033[0m", pos+1, len(q))
	}
	st := m.chatStatus[m.waitID]
	note := ""
	switch {
	case !ok:
		title += " · \033[90msession gone\033[0m"
	case m.waitSignal(conv) == waitPrompt:
		why := st.waitingFor
		if why == "" {
			why = "waiting"
		}
		title += fmt.Sprintf(" · %s · %s · %dm", m.sessionName(conv), why, int(time.Since(st.since).Minutes()))
		note = "\033[90m  it's showing a menu: answer it in its tab (enter on an empty box goes there); a message here waits behind it\033[0m"
	case m.waitSignal(conv) == waitAsked:
		title += " · " + m.sessionName(conv) + " · \033[90mits last message asks you something\033[0m"
	default:
		title += " · " + m.sessionName(conv)
	}

	lines := m.waitLines()
	back := min(m.waitScroll, max(0, len(lines)-rows))
	start := max(0, len(lines)-rows-back)
	visible := append([]string(nil), lines[start:min(start+rows, len(lines))]...)
	for len(visible) < rows {
		visible = append(visible, "")
	}

	var b strings.Builder
	b.WriteString(ansi.Truncate(title, inner, "…") + "\033[0m\n")
	b.WriteString(strings.Join(visible, "\n") + "\n")
	b.WriteString(ansi.Truncate(note, inner, "…") + "\n")
	if ok {
		b.WriteString(m.chatBox(conv, m.waitInput, true, inner) + "\n")
	} else {
		b.WriteString("\n\n\n\n")
	}
	b.WriteString("\033[90m" + ansi.Truncate(hints(keyEnter, "send", keyEsc, "snooze "+shortDuration(m.snooze), "^N/P", "next/prev", "^J/K", "scroll", "^Y", "close"), inner, "…") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(0, 1).
		Width(inner + 2).
		Render(b.String())
}

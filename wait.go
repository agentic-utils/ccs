package main

import (
	"fmt"
	"regexp"
	"slices"
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

// defaultSnooze is how long Esc puts a waiting session aside (CCS_SNOOZE).
const defaultSnooze = 15 * time.Minute

// snoozeLength reads CCS_SNOOZE; ok is false for a set value that isn't a
// positive duration, which gets the default.
func snoozeLength(s string) (d time.Duration, ok bool) {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, true
	}
	return defaultSnooze, s == ""
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

// snoozeRec puts one wait aside. Since is the session's statusUpdatedAt when
// it was snoozed, so after a restart the snooze is matched to the same wait.
type snoozeRec struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// How a live session waits on the user.
const (
	waitNone   = iota
	waitPrompt // its session file says "waiting": a permission prompt or a question menu
	waitAsked  // idle, and its last message ends in "?": a guess, so it never pops up
)

var inlineCode = regexp.MustCompile("`[^`]*`")

func (m model) waitSignal(conv Conversation) int {
	if !m.live[conv.SessionID] {
		return waitNone
	}
	switch m.chatStatus[conv.SessionID].status {
	case "waiting":
		return waitPrompt
	case "idle":
		if n := len(conv.Messages); n > 0 && conv.Messages[n-1].Role == "assistant" && asks(conv.Messages[n-1].Text) {
			return waitAsked
		}
	}
	return waitNone
}

// asks: the text ends in a question. Markdown closing it ("(yes?)", "push?**")
// still asks; a bare "?", one in inline code or ending a URL doesn't.
func asks(text string) bool {
	t := strings.TrimRight(inlineCode.ReplaceAllString(strings.TrimSpace(text), ""), ")*_ ")
	words := strings.Fields(t)
	return strings.HasSuffix(t, "?") && strings.TrimRight(t, "?") != "" && !strings.Contains(words[len(words)-1], "://")
}

// snoozedNow reports whether id's wait is snoozed.
func (m model) snoozedNow(id string) bool {
	s, ok := m.snoozed[id]
	return ok && time.Now().Before(s.Until)
}

// waitQueue is the overlay's sessions, oldest wait first so a snoozed one
// coming back can't be starved by newer ones (an unknown wait time goes
// last). all (opened by hand) adds the snoozed ones and, after the prompts,
// the waitAsked ones.
func (m model) waitQueue(all bool) []Conversation {
	var prompts, asked []Conversation
	for _, item := range m.items {
		if !all && m.snoozedNow(item.conv.SessionID) {
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
			a, b := m.chatStatus[q[i].SessionID].since, m.chatStatus[q[j].SessionID].since
			if a.IsZero() != b.IsZero() {
				return b.IsZero()
			}
			return a.Before(b)
		})
	}
	return append(prompts, asked...)
}

// waitPos is where the overlay's session is in q, or -1.
func (m model) waitPos(q []Conversation) int {
	return slices.IndexFunc(q, func(c Conversation) bool { return c.SessionID == m.waitID })
}

// setStat records a live session's status; it's the only place statuses are
// set. A read without a status says nothing and is skipped. A wait starts
// when a session enters "waiting", or gets a new statusUpdatedAt while
// waiting; it ends when the session leaves "waiting". A snooze (and whether
// it was seen) belongs to one wait, so it ends with it. The first sight of a
// session after ccs starts counts as a new wait, except that a restored
// snooze for the same statusUpdatedAt still holds.
func (m *model) setStat(id string, st sessionStat) {
	if st.status == "" {
		return
	}
	if m.chatStatus == nil {
		m.chatStatus = make(map[string]sessionStat)
	}
	old, had := m.chatStatus[id]
	m.chatStatus[id] = st
	switch {
	case st.status != "waiting":
		delete(m.snoozed, id)
		delete(m.waitDue, id)
	case !had:
		if s, ok := m.snoozed[id]; ok && !s.Since.Equal(st.since) {
			delete(m.snoozed, id)
		}
		m.markDue(id)
	case old.status != "waiting" || !st.since.IsZero() && !st.since.Equal(old.since):
		delete(m.snoozed, id)
		m.markDue(id)
	}
}

// applyStats takes the live tick's statuses, forgets sessions that are gone
// (the live tick keeps one whose file was unreadable for a tick while its
// process runs), ends snoozes whose time is up, and pops the overlay when a
// due wait can show. At startup every waiting session counts as new, so ccs
// opens on the oldest.
func (m *model) applyStats(stats map[string]sessionStat) {
	if stats == nil {
		return
	}
	for id, st := range stats {
		m.setStat(id, st)
	}
	for id := range m.chatStatus {
		if !m.live[id] {
			delete(m.chatStatus, id)
			delete(m.waitDrafts, id)
		}
	}
	for id, s := range m.snoozed {
		switch {
		case !m.live[id]:
			delete(m.snoozed, id)
		case !time.Now().Before(s.Until):
			delete(m.snoozed, id)
			m.markDue(id)
		}
	}
	for id := range m.waitDue {
		if m.chatStatus[id].status != "waiting" {
			delete(m.waitDue, id)
		}
	}
	if !m.waitOpen && m.canPop() && m.popDue() {
		m.openWait(false)
	}
	if m.waitOpen {
		m.checkDelivered(m.waitID)
	}
}

func (m *model) markDue(id string) {
	if m.waitDue == nil {
		m.waitDue = make(map[string]bool)
	}
	m.waitDue[id] = true
}

// popDue: a due wait is in the queue.
func (m model) popDue() bool {
	return slices.ContainsFunc(m.waitQueue(false), func(c Conversation) bool { return m.waitDue[c.SessionID] })
}

// popupAboveWait: a popup drawn over the overlay is open (see popups). It
// takes the keys and the wheel first, and holds off a pop-up. Nothing opens
// over the overlay from the keyboard, so in practice it's the update offer.
func (m model) popupAboveWait() bool {
	return slices.ContainsFunc(m.popups()[1:], func(p popup) bool { return p.open })
}

// canPop: no popup or prompt is open. A pop-up takes no keys until engaged,
// so typing never has to hold it off.
func (m model) canPop() bool {
	return !m.prompting() && !m.popupAboveWait()
}

// openWait opens the overlay on the first session in its queue. By hand it
// has the keyboard at once and also shows snoozed sessions (keeping their
// snoozes) and the ones whose last message asks something. On its own it
// waits for Ctrl+Y.
func (m *model) openWait(byHand bool) {
	q := m.waitQueue(byHand)
	if len(q) == 0 {
		m.errorMsg = "No session is waiting on you"
		return
	}
	m.waitOpen, m.waitAll, m.waitEngaged, m.waitDue, m.waitErr = true, byHand, byHand, nil, ""
	m.showWait(q[0].SessionID)
	m.waitScroll = 0
}

// closeWait closes the overlay. What was due has been seen, so only a new
// wait pops it again.
func (m *model) closeWait() {
	m.waitOpen, m.waitEngaged, m.waitDue = false, false, nil
}

// showWait shows session id, keeping each session's draft with it.
func (m *model) showWait(id string) {
	if id == m.waitID {
		return
	}
	if v := m.waitInput.Value(); v != "" {
		if m.waitDrafts == nil {
			m.waitDrafts = make(map[string]string)
		}
		m.waitDrafts[m.waitID] = v
	}
	m.waitInput.SetValue(m.waitDrafts[id])
	delete(m.waitDrafts, id)
	m.waitID, m.waitScroll, m.waitErr = id, 0, ""
}

// giveBack puts text that never reached session id back in the box it came
// from, unless that box has moved on (another session, or a new draft).
func (m *model) giveBack(id, text string, overlay bool) {
	switch {
	case overlay && m.waitID == id:
		if m.waitInput.Value() == "" {
			m.waitInput.SetValue(text)
		}
	case overlay:
		if m.waitDrafts[id] == "" {
			if m.waitDrafts == nil {
				m.waitDrafts = make(map[string]string)
			}
			m.waitDrafts[id] = text
		}
	case m.selectedLive() && m.filtered[m.cursor].conv.SessionID == id && m.chatInput.Value() == "":
		m.chatInput.SetValue(text)
	}
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
	i := m.waitPos(q)
	if i < 0 {
		i = 0
	} else {
		i = max(0, min(i+by, len(q)-1))
	}
	m.showWait(q[i].SessionID)
}

// waitKey handles a key while the overlay has the keyboard (only Esc gets
// here before it's engaged).
func (m model) waitKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.waitErr = ""
	conv, ok := m.waitConv()
	switch msg.String() {
	case "ctrl+y":
		m.closeWait()
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc": // defer: snooze this one, show the one after it
		before := m.waitQueue(m.waitAll)
		i := max(m.waitPos(before), 0)
		if m.snoozed == nil {
			m.snoozed = make(map[string]snoozeRec)
		}
		m.snoozed[m.waitID] = snoozeRec{Since: m.chatStatus[m.waitID].since, Until: time.Now().Add(m.snooze)}
		q := m.waitQueue(m.waitAll)
		if len(q) == len(before) { // by hand, snoozed sessions stay listed: move past it
			i++
		}
		if i >= len(q) {
			m.closeWait()
		} else {
			m.showWait(q[i].SessionID)
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
		switch {
		case !ok:
			m.waitErr = "That session is gone"
		case !m.live[conv.SessionID]:
			m.waitErr = m.sessionName(conv) + " has exited"
		case strings.TrimSpace(m.waitInput.Value()) == "": // go to its tab, e.g. to answer a menu
			m.closeWait()
			return m, m.focusCmd(conv)
		default:
			return m, m.sendTo(conv, &m.waitInput)
		}
	default:
		var cmd tea.Cmd
		m.waitInput, cmd = m.waitInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// waitChrome is the overlay's rows besides the transcript: borders, title,
// menu note, message box (status line and bordered input) and hints.
const waitChrome = 2 + 1 + 1 + 4 + 1

// waitLayout: the overlay's text width, how many transcript rows fit, and
// whether the menu note and the hints get a row. It fits the terminal: a
// short screen uses all of it, shrinks the transcript first, then drops the
// note, then the hints.
func (m model) waitLayout() (inner, rows int, note, hints bool) {
	width := min(max(m.width*9/10, 30), m.width)
	avail := m.height * 9 / 10
	if avail < waitChrome+3 {
		avail = m.height
	}
	rows, note, hints = avail-waitChrome, true, true
	if rows < 1 {
		rows, note = rows+1, false
	}
	if rows < 0 {
		rows, hints = rows+1, false
	}
	return max(width-4, 1), max(rows, 0), note, hints
}

func (m model) waitLines() []string {
	conv, ok := m.waitConv()
	if !ok {
		return nil
	}
	inner, _, _, _ := m.waitLayout()
	return cachedPreview(m.waitPreview, conv, "", inner)
}

func (m model) maxWaitScroll() int {
	_, rows, _, _ := m.waitLayout()
	return max(0, len(m.waitLines())-rows)
}

// waitPopup draws the overlay: which session and why, its transcript (newest
// at the bottom), a message box and the keys.
func (m model) waitPopup() string {
	inner, rows, showNote, showHints := m.waitLayout()
	conv, ok := m.waitConv()
	q := m.waitQueue(m.waitAll)
	title := "\033[90mno longer waiting\033[0m"
	if pos := m.waitPos(q); pos >= 0 {
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
		title += " · " + m.sessionName(conv) + " · " + why
		if !st.since.IsZero() {
			title += fmt.Sprintf(" · %dm", int(time.Since(st.since).Minutes()))
		}
		note = "\033[90m  it's showing a menu: answer it in its tab (enter on an empty box goes there); a message sent here may wait until it's answered\033[0m"
	case m.waitSignal(conv) == waitAsked:
		title += " · " + m.sessionName(conv) + " · \033[90mits last message asks you something\033[0m"
	default:
		title += " · " + m.sessionName(conv)
	}
	if ok && m.snoozedNow(m.waitID) {
		title += " · \033[90msnoozed\033[0m"
	}
	if ok && m.unverified[m.waitID] {
		title += " · \033[90munverified: its process start couldn't be checked\033[0m"
	}
	if m.waitErr != "" {
		note = "\033[31m  " + m.waitErr + "\033[0m"
	}

	lines := m.waitLines()
	start, end := scrollWindow(len(lines), rows, m.waitScroll)
	visible := append([]string(nil), lines[start:end]...)
	for len(visible) < rows {
		visible = append(visible, "")
	}

	var b strings.Builder
	b.WriteString(ansi.Truncate(title, inner, "…") + "\033[0m\n")
	if rows > 0 {
		b.WriteString(strings.Join(visible, "\n") + "\n")
	}
	if showNote {
		b.WriteString(ansi.Truncate(note, inner, "…") + "\033[0m\n")
	}
	if ok {
		b.WriteString(m.chatBox(conv, m.waitInput, m.waitEngaged, inner))
	} else {
		b.WriteString("\n\n\n")
	}
	if showHints {
		keys := hints("^Y", "answer here", keyEsc, "snooze "+shortDuration(m.snooze)) + "  other keys work as usual"
		if m.waitEngaged {
			keys = hints(keyEnter, "send", keyEsc, "snooze "+shortDuration(m.snooze), "↑↓ ^N/P", "next/prev", "pgup/dn ^K/J", "scroll", "^Y", "close")
		}
		b.WriteString("\n\033[90m" + ansi.Truncate(keys, inner, "…") + "\033[0m")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(0, 1).
		Width(inner + 2).
		Render(b.String())
}

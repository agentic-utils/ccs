package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
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

// amber marks a live session that's busy, or (as a ?) waiting on you.
const amber = "38;5;214"

// How a live session waits on the user.
const (
	waitNone   = iota
	waitPrompt // its session file says "waiting": a permission prompt or a question menu
	waitAsked  // idle, and its last message ends in "?": a guess, so it never gets the banner
)

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
// still asks; a bare "?" doesn't.
func asks(text string) bool {
	t := strings.TrimRight(strings.TrimSpace(text), ")*_` ")
	return strings.HasSuffix(t, "?") && strings.TrimRight(t, "?") != ""
}

// snoozedNow reports whether id's wait is snoozed.
func (m model) snoozedNow(id string) bool {
	s, ok := m.snoozed[id]
	return ok && time.Now().Before(s.Until)
}

// waitQueue is the waiting sessions, oldest wait first so a snoozed one
// coming back can't be starved by newer ones (an unknown wait time goes
// last). all (the overlay's list) adds the snoozed ones and, after the
// prompts, the waitAsked ones.
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
// snooze for the same statusUpdatedAt still holds (without one, it can't be
// matched, so it goes).
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
		if s, ok := m.snoozed[id]; ok && (st.since.IsZero() || !s.Since.Equal(st.since)) {
			delete(m.snoozed, id)
		}
		m.markDue(id)
	case old.status != "waiting" || !st.since.IsZero() && !st.since.Equal(old.since):
		delete(m.snoozed, id)
		m.markDue(id)
	}
}

// applyStats takes the live tick's statuses, forgets sessions that are gone
// (the live tick keeps one whose file is briefly unreadable while its
// process runs), and ends snoozes whose time is up, so their wait gets the
// banner again.
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
		}
	}
	m.dropDrafts()
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

// banner is the wait the one-row banner shows: the oldest not yet seen in the
// overlay (or whose snooze ran out), and how many more there are. None while
// the overlay is open: it lists them.
func (m model) banner() (conv Conversation, more int, ok bool) {
	if m.waitOpen {
		return
	}
	for _, c := range m.waitQueue(false) {
		switch {
		case !m.waitDue[c.SessionID]:
		case ok:
			more++
		default:
			conv, ok = c, true
		}
	}
	return
}

// waitingCount is how many live sessions wait on a menu or prompt, for the
// header (a dim ? guess isn't counted).
func (m model) waitingCount() int {
	n := 0
	for _, it := range m.items {
		if m.waitSignal(it.conv) == waitPrompt {
			n++
		}
	}
	return n
}

// popupAboveWait: a popup drawn over the overlay is open (see overPopups). It
// takes the keys and the wheel first. Nothing opens over the overlay from the
// keyboard, so in practice it's the update offer.
func (m model) popupAboveWait() bool {
	return slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open })
}

// waitMinHeight is the shortest terminal the overlay fits: its frame and a
// transcript row. Shorter, there's only the banner.
const waitMinHeight = waitChrome + 1

const waitTooShort = "The window is too short for the waiting sessions: make it taller, or answer in the session's tab"

// openWait opens the overlay, which owns the keyboard until closed. It shows
// the banner's session first (else the oldest), and lists the snoozed
// sessions too (keeping their snoozes) and the ones whose last message asks
// something.
func (m *model) openWait() {
	if m.height < waitMinHeight {
		m.errorMsg = waitTooShort
		return
	}
	q := m.waitQueue(true)
	if len(q) == 0 {
		m.errorMsg = "No session is waiting on you"
		return
	}
	first := q[0]
	if c, _, ok := m.banner(); ok {
		first = c
	}
	m.waitOpen, m.waitErr = true, ""
	m.showWait(first.SessionID)
	m.waitScroll = 0
}

func (m *model) closeWait() {
	m.waitOpen = false
}

// showWait shows session id, which marks its wait seen, keeping each
// session's draft with it.
func (m *model) showWait(id string) {
	delete(m.waitDue, id)
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
// from (the overlay's or the main one), as that session's draft: in the box
// if it still shows the session, else kept until it does. A draft written
// since keeps the text after it, so it's never dropped. It returns what the
// send note should say.
func (m *model) giveBack(id, text string, overlay bool) string {
	m.syncChatDraft()
	in, shown, drafts, where := &m.chatInput, m.chatFor == id, &m.chatDrafts, "its message box: select it to see it"
	if overlay {
		in, shown, drafts, where = &m.waitInput, m.waitID == id, &m.waitDrafts, "its draft in the waiting list (^Y)"
	}
	if shown {
		in.SetValue(joinDraft(in.Value(), text))
		in.CursorEnd()
		return "unsent text put back in the box"
	}
	if *drafts == nil {
		*drafts = make(map[string]string)
	}
	(*drafts)[id] = joinDraft((*drafts)[id], text)
	return "unsent text kept in " + where
}

// dropDrafts throws away the kept drafts of sessions that have exited, and
// says so: they can't be sent anywhere now.
func (m *model) dropDrafts() {
	for _, drafts := range []map[string]string{m.waitDrafts, m.chatDrafts} {
		for id, d := range drafts {
			if m.live[id] {
				continue
			}
			delete(drafts, id)
			if strings.TrimSpace(d) != "" {
				name := id
				for _, item := range m.items {
					if item.conv.SessionID == id {
						name = m.sessionName(item.conv)
					}
				}
				m.errorMsg = "Unsent text to " + name + " discarded: it has exited"
			}
		}
	}
}

func joinDraft(draft, text string) string {
	if draft == "" {
		return text
	}
	return draft + " " + text
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
	q := m.waitQueue(true)
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

// waitKey handles a key while the overlay is open.
func (m model) waitKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.waitErr = ""
	conv, ok := m.waitConv()
	switch msg.String() {
	case "ctrl+y":
		m.closeWait()
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc": // defer: snooze this wait, show the oldest one not snoozed
		if ok && m.waitSignal(conv) == waitPrompt {
			if m.snoozed == nil {
				m.snoozed = make(map[string]snoozeRec)
			}
			m.snoozed[m.waitID] = snoozeRec{Since: m.chatStatus[m.waitID].since, Until: time.Now().Add(m.snooze)}
		}
		q := m.waitQueue(true)
		if i := slices.IndexFunc(q, func(c Conversation) bool {
			return c.SessionID != m.waitID && m.waitSignal(c) == waitPrompt && !m.snoozedNow(c.SessionID)
		}); i >= 0 {
			m.showWait(q[i].SessionID)
		} else {
			m.closeWait()
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
			return m, m.sendTo(conv, true)
		}
	default:
		if k := msg.String(); ok && len(k) == 1 && k >= "1" && k <= "9" && m.waitInput.Value() == "" && m.waitSignal(conv) == waitPrompt {
			if n := int(k[0] - '0'); n <= answerable(conv) {
				return m, m.answerCmd(conv, n)
			}
		}
		var cmd tea.Cmd
		m.waitInput, cmd = m.waitInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// waitChrome is the overlay's rows besides the transcript: borders, title,
// menu note, message box (status line and bordered input) and hints.
const waitChrome = 2 + 1 + 1 + 4 + 1

// waitLayout: the overlay's text width and how many transcript rows fit. A
// short screen (down to waitMinHeight) gets all of it.
func (m model) waitLayout() (inner, rows int) {
	avail := m.height * 9 / 10
	if avail < waitChrome+3 {
		avail = m.height
	}
	return max(min(max(m.width*9/10, 30), m.width)-4, 1), max(avail-waitChrome, 0)
}

func (m model) waitLines() []string {
	conv, ok := m.waitConv()
	if !ok {
		return nil
	}
	inner, _ := m.waitLayout()
	return cachedPreview(m.waitPreview, conv, "", inner)
}

func (m model) maxWaitScroll() int {
	_, rows := m.waitLayout()
	return max(0, len(m.waitLines())-max(rows-len(m.waitAsk()), 0))
}

// waitAsk is what the overlay's session is asking, drawn under its transcript.
func (m model) waitAsk() []string {
	conv, ok := m.waitConv()
	if !ok || m.waitSignal(conv) != waitPrompt {
		return nil
	}
	inner, _ := m.waitLayout()
	return askLines(conv, inner)
}

// askLines shows a waiting session's question menu, numbered as claude shows
// it, or the tool call its permission prompt is about. Neither is in the
// transcript as text until answered.
func askLines(conv Conversation, width int) []string {
	cut := func(s string) string { return ansi.Truncate(s, width, "…") + "\033[0m" }
	// wrapped is s wrapped to width after prefix, its other lines indented
	// to match and each line in style, so a long question reads in full.
	wrapped := func(prefix, style, s string) []string {
		indent := strings.Repeat(" ", ansi.StringWidth(prefix))
		var out []string
		for i, l := range strings.Split(ansi.Wrap(s, max(width-len(indent), 10), " "), "\n") {
			if i == 0 {
				out = append(out, cut(prefix+style+l))
			} else {
				out = append(out, cut(indent+style+l))
			}
		}
		return out
	}
	if qs := conv.questions(); len(qs) > 0 {
		var out []string
		for _, q := range qs {
			question := cleanText(q.Question)
			if q.MultiSelect {
				question += "  (pick any)"
			}
			out = append(append(out, ""), wrapped("\033[1;"+amber+"m? ", "\033[1;"+amber+"m", question)...)
			for i, o := range q.Options {
				out = append(out, wrapped(fmt.Sprintf("  \033[1m%d\033[0m  ", i+1), "", cleanText(o.Label))...)
				if o.Description != "" {
					out = append(out, wrapped("     ", "\033[90m", cleanText(o.Description))...)
				}
			}
		}
		return out
	}
	if conv.pending == nil {
		return nil
	}
	return []string{"", cut("\033[1;" + amber + "m? wants to use " + conv.pending.Name + "\033[0m " + toolSummary(conv.pending.Input))}
}

// answerable is how many options a digit can pick: only a single
// single-choice question, where claude takes the digit as the answer.
func answerable(conv Conversation) int {
	qs := conv.questions()
	if len(qs) != 1 || qs[0].MultiSelect {
		return 0
	}
	return min(len(qs[0].Options), 9)
}

// toolSummary is the part of a tool call's input a person would check first.
func toolSummary(input json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(input, &in)
	for _, k := range []string{"command", "file_path", "url", "pattern", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			return cleanText(strings.Join(strings.Fields(v), " "))
		}
	}
	return ""
}

type answerDoneMsg struct {
	name, where string
	err         error
}

// answerCmd picks option n of a waiting session's question by pressing the
// digit in its terminal, as typing it there would.
func (m model) answerCmd(conv Conversation, n int) tea.Cmd {
	pid, name := m.livePIDs[conv.SessionID], m.sessionName(conv)
	return func() tea.Msg {
		where, err := typeKeys(pid, strconv.Itoa(n), false)
		return answerDoneMsg{name: name, where: where, err: err}
	}
}

// waitPopup draws the overlay: which session and why, its transcript (newest
// at the bottom), a message box and the keys.
func (m model) waitPopup() string {
	inner, rows := m.waitLayout()
	conv, ok := m.waitConv()
	q := m.waitQueue(true)
	title := "\033[90mno longer waiting\033[0m"
	if pos := m.waitPos(q); pos >= 0 {
		title = fmt.Sprintf("\033[1;"+amber+"mwaiting %d of %d\033[0m", pos+1, len(q))
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

	ask := m.waitAsk()
	if len(ask) > rows {
		ask = ask[len(ask)-rows:]
	}
	lines := m.waitLines()
	start, end := scrollWindow(len(lines), rows-len(ask), m.waitScroll)
	visible := append([]string(nil), lines[start:end]...)
	for len(visible) < rows-len(ask) {
		visible = append(visible, "")
	}
	visible = append(visible, ask...)

	var b strings.Builder
	b.WriteString(ansi.Truncate(title, inner, "…") + "\033[0m\n")
	if rows > 0 {
		b.WriteString(strings.Join(visible, "\n") + "\n")
	}
	b.WriteString(ansi.Truncate(note, inner, "…") + "\033[0m\n")
	if ok {
		b.WriteString(m.chatBox(conv, m.waitInput, true, inner))
	} else {
		b.WriteString("\n\n\n")
	}
	enter := "send"
	if strings.TrimSpace(m.waitInput.Value()) == "" {
		enter = "open its tab"
	}
	var keys string
	if n := answerable(conv); ok && n > 0 && m.waitSignal(conv) == waitPrompt && m.waitInput.Value() == "" {
		keys = hints(fmt.Sprintf("1-%d", n), "answer") + "  "
	}
	keys += hints(keyEnter, enter, keyEsc, "snooze "+shortDuration(m.snooze), "↑↓ ^N/P", "next/prev", "pgup/dn ^K/J", "scroll", "^Y", "close", "^C", "quit")
	b.WriteString("\n\033[90m" + ansi.Truncate(keys, inner, "…") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(0, 1).
		Width(inner + 2).
		Render(b.String())
}

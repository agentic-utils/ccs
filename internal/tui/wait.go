package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ============================================================================
// Live sessions waiting on the user: their statuses, the header's waiting
// count, the question shown in the bottom pane, and ^X A auto-focus. All
// answering happens in the main view (held prompts in its bottom pane,
// held.go and menu.go; the rest in their own tab). The ^X W overlay that
// used to list waits is gone: see docs/design/input-prompt-overlay.md.
// ============================================================================

// amber marks a live session that's busy, or (as a ?) waiting on you.
const amber = "38;5;214"

// How a live session waits on the user.
const (
	waitNone   = iota
	waitPrompt // its session file says "waiting": a permission prompt or a question menu
	waitAsked  // idle, and its last message ends in "?": a guess
)

func (m model) waitSignal(conv transcript.Conversation) int {
	if !m.live[conv.SessionID] {
		return waitNone
	}
	switch m.chatStatus[conv.SessionID].Status {
	case "waiting":
		return waitPrompt
	case "idle":
		if n := lastSaid(conv.Messages); n >= 0 && conv.Messages[n].Role == "assistant" && asks(conv.Messages[n].Text) {
			return waitAsked
		}
	}
	return waitNone
}

// asks: the text ends in a question. Markdown closing it ("(yes?)", "push?**")
// still asks; a bare "?" doesn't.
func asks(text string) bool { return transcript.Asks(text) }

// setStat records a live session's status; it's the only place statuses are
// set. A read without a status says nothing and is skipped.
func (m *model) setStat(id string, st sessions.SessionStat) {
	if st.Status == "" {
		return
	}
	if m.chatStatus == nil {
		m.chatStatus = make(map[string]sessions.SessionStat)
	}
	m.chatStatus[id] = st
}

// applyStats takes the live tick's statuses and forgets sessions that are
// gone (the live tick keeps one whose file is briefly unreadable while its
// process runs).
func (m *model) applyStats(stats map[string]sessions.SessionStat) {
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

// giveBack puts text that wasn't sent back in the message box, if it still
// shows the session, else kept as that session's draft until it does. A
// draft written since keeps the text after it, so it's never dropped. It
// returns what the send note should say.
func (m *model) giveBack(id, text string) string {
	m.syncChatDraft()
	if m.chatFor == id {
		m.chatInput.SetValue(joinDraft(m.chatInput.Value(), text))
		m.chatInput.CursorEnd()
		return "unsent text put back in the box"
	}
	if m.chatDrafts == nil {
		m.chatDrafts = make(map[string]string)
	}
	m.chatDrafts[id] = joinDraft(m.chatDrafts[id], text)
	return "unsent text kept in its message box: select it to see it"
}

// dropDrafts throws away the kept drafts of sessions that have exited, and
// says so: they can't be sent anywhere now.
func (m *model) dropDrafts() {
	for id, d := range m.chatDrafts {
		if m.live[id] {
			continue
		}
		delete(m.chatDrafts, id)
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

func joinDraft(draft, text string) string {
	if draft == "" {
		return text
	}
	return draft + " " + text
}

// askLines shows a waiting session's question menu, numbered as claude shows
// it, or the tool call its permission prompt is about. Neither is in the
// transcript as text until answered.
func askLines(conv transcript.Conversation, width int) []string {
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
	if qs := conv.Questions(); len(qs) > 0 {
		var out []string
		for _, q := range qs {
			question := transcript.CleanText(q.Question)
			if q.MultiSelect {
				question += "  (pick any)"
			}
			out = append(append(out, ""), wrapped("\033[1;"+waitColour+"m? ", "\033[1;"+waitColour+"m", question)...)
			for i, o := range q.Options {
				out = append(out, wrapped(fmt.Sprintf("  \033[1m%d\033[0m  ", i+1), "", transcript.CleanText(o.Label))...)
				if o.Description != "" {
					out = append(out, wrapped("     ", "\033[90m", transcript.CleanText(o.Description))...)
				}
			}
		}
		return out
	}
	if conv.Pending == nil {
		return nil
	}
	return []string{"", cut("\033[1;" + amber + "m? wants to use " + conv.Pending.Name + "\033[0m " + toolSummary(conv.Pending.Input))}
}

// toolSummary is the part of a tool call's input a person would check first.
func toolSummary(input json.RawMessage) string { return transcript.ToolSummary(input) }

// autoFocusIdle is how long since the last key press before auto-focus
// switches tabs, so keystrokes meant for another window never land in a
// waiting session's menu, where a digit or Enter would answer it. The mouse
// doesn't count: moving it can't answer anything, and it's rarely still.
const autoFocusIdle = 2 * time.Second

// userIdle is how long since the last key press (macOS, through CoreGraphics:
// kCGEventSourceStateHIDSystemState, kCGEventKeyDown); not ok when unknown,
// and then auto-focus never switches.
var userIdle = sessions.KeyIdle

// autoFocusTab selects a session's tab or pane without bringing iTerm in front.
var autoFocusTab = func(pid int) bool { return sessions.FocusSession(pid, false) }

type autoFocusMsg struct {
	id      string
	since   time.Time
	idle    time.Duration
	idleOK  bool // the idle time could be read
	tried   bool // false: the user was busy, so try again on a later tick
	focused bool
}

// autoFocusCmd (^X A) switches to a session that starts waiting on a menu or
// prompt: once per wait, after autoFocusIdle without input, and not again
// until the question it switched to is answered (focusHeld). That includes
// waits already there when it starts; the waits it has dealt with are kept
// across restarts (focusRecordPath), so a restart doesn't switch to them
// again. Each step goes to the activity log once per wait (focusNoted).
func (m *model) autoFocusCmd() tea.Cmd {
	if !m.autoFocus || m.focusBusy {
		return nil
	}
	if m.focusHandled == nil {
		m.focusHandled, m.focusNoted = map[string]time.Time{}, map[string]bool{}
	}
	if held := m.focusHeld; held != "" {
		if m.chatStatus[held].Status == "waiting" {
			for sid, st := range m.chatStatus {
				if h, ok := m.focusHandled[sid]; sid != held && st.Status == "waiting" && !(ok && h.Equal(st.Since)) {
					m.noteFocus(sid, "held", "auto-focus: %s is waiting too; holding on %s until it's answered", m.focusName(sid), m.focusName(held))
				}
			}
			return nil
		}
		state.LogActivity("auto-focus: %s answered, free to switch again", m.focusName(held))
		m.focusHeld = ""
	}
	id := ""
	for sid, st := range m.chatStatus {
		if h, ok := m.focusHandled[sid]; st.Status != "waiting" || ok && h.Equal(st.Since) {
			continue
		}
		if _, held := m.heldPrompt[sid]; held { // its tab shows no menu: ccs selects it instead (autoSelect)
			continue
		}
		id = sid // ponytail: any one of several waits; the rest follow once it's answered
	}
	pid, ok := m.livePIDs[id]
	if id == "" || !ok {
		return nil
	}
	if h := m.hosts[id]; h != "" { // no terminal to switch to (a background one too: attach is your call)
		m.focusHandled[id] = m.chatStatus[id].Since
		m.noteFocus(id, "hosted", "auto-focus: %s is waiting in %s, which ccs can't switch to", m.focusName(id), h)
		return nil
	}
	why := ""
	if w := m.chatStatus[id].WaitingFor; w != "" {
		why = " (" + w + ")"
	}
	m.noteFocus(id, "waiting", "auto-focus: %s is waiting%s", m.focusName(id), why)
	m.focusBusy = true
	since := m.chatStatus[id].Since
	return func() tea.Msg {
		idle, ok := userIdle()
		if !ok || idle < autoFocusIdle {
			return autoFocusMsg{id: id, since: since, idle: idle, idleOK: ok}
		}
		return autoFocusMsg{id: id, since: since, idle: idle, idleOK: true, tried: true, focused: autoFocusTab(pid)}
	}
}

// applyAutoFocus records an attempt: a tried wait isn't tried again, and one
// it switched to holds auto-focus until it's answered.
func (m *model) applyAutoFocus(msg autoFocusMsg) {
	m.focusBusy = false
	name := m.focusName(msg.id)
	switch {
	case !msg.idleOK:
		m.noteFocus(msg.id, "noidle", "auto-focus: can't read when a key was last pressed, so not switching to %s", name)
	case !msg.tried:
		m.noteFocus(msg.id, "busy", "auto-focus: you're typing (last key %.1fs ago); switching to %s once you've stopped for %v", msg.idle.Seconds(), name, autoFocusIdle)
	case msg.focused:
		stats.Record("tui.auto_focus_switch", stats.Auto)
		state.LogActivity("auto-focus: switched to %s", name)
	default:
		state.LogActivity("auto-focus: couldn't find %s's tab or pane", name)
	}
	if msg.tried {
		m.focusHandled[msg.id] = msg.since
		m.saveFocusRecord()
	}
	if msg.focused {
		m.focusHeld = msg.id
	}
}

// noteFocus logs a step of a wait once (by kind), not on every tick it lasts.
func (m *model) noteFocus(id, kind, format string, args ...any) {
	key := id + " " + kind + " " + m.chatStatus[id].Since.String()
	if m.focusNoted[key] {
		return
	}
	m.focusNoted[key] = true
	state.LogActivity(format, args...)
}

// focusName names a session for the log.
func (m model) focusName(id string) string {
	if n := m.chatStatus[id].Name; n != "" {
		return n
	}
	return id[:min(8, len(id))]
}

// focusRecordPath keeps the waits auto-focus has dealt with, so after a
// restart (an update, a crash) a question that came up meanwhile is still
// news and one already dealt with isn't.
var focusRecordPath = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccs", "auto-focus.json")
}()

// loadFocusRecord reads it; ok is false when there's none yet.
func loadFocusRecord() (handled map[string]time.Time, ok bool) {
	data, err := os.ReadFile(focusRecordPath)
	if err != nil || json.Unmarshal(data, &handled) != nil {
		return nil, false
	}
	return handled, true
}

// saveFocusRecord writes the waits still open that auto-focus has dealt
// with (the rest can't come back: a new wait has a new statusUpdatedAt).
func (m *model) saveFocusRecord() {
	open := map[string]time.Time{}
	for id, since := range m.focusHandled {
		if m.chatStatus[id].Status == "waiting" {
			open[id] = since
		}
	}
	data, _ := json.Marshal(open)
	if os.MkdirAll(filepath.Dir(focusRecordPath), 0o700) != nil {
		return
	}
	tmp := focusRecordPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, focusRecordPath)
	}
}

// lastSaid is the index of the last message said in the session itself, past
// any it sent to other sessions and any channel events; -1 if none.
func lastSaid(msgs []transcript.Message) int { return transcript.LastSaid(msgs) }

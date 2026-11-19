package tui

// Prompts held by `ccs hook permission` (docs/remote.md): by ccs serve for
// its page, or, when no ccs serve --allow-write runs, by this ccs itself
// (tuiHold) for this list. Answered from here by their id (the owner's
// holder directly, or the control socket: mcp.HeldPrompts, mcp.AnswerHeld),
// never by typing into a terminal: the prompt isn't on screen in its tab
// while it's held. The selected session's
// held prompt is answered in the bottom pane (Enter or a click focuses it;
// with ^X A on, ccs selects a session that starts waiting once you've
// stopped typing, and focuses it there): a digit
// picks a single question's option, A allows and D denies a permission
// prompt (each asks y/n), and T sends it back to its tab. Nothing is ever
// typed into a terminal to answer. Outside --preview a key press on the Mac
// sends held prompts back to their tabs within seconds, so this matters most
// in a preview and in those first seconds.

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// heldEvery is the least time between asks for the held prompts.
const heldEvery = 2 * time.Second

// heldGuard and heldGuardGap pause the answer pane when ccs focused it by
// itself (auto-select): keys in its first heldGuard, or while keys still
// come less than heldGuardGap apart, were meant for the search or a message
// box, so they're dropped with a note rather than answering the prompt.
const (
	heldGuard    = time.Second
	heldGuardGap = 600 * time.Millisecond
)

// heldNow is the clock the pane's guard and auto-select read; stubbed in tests.
var heldNow = time.Now

// The held prompts and their answers: this ccs's own holder when it owns
// the hold, else the owner's control socket; stubbed in tests.
var (
	heldPrompts = func() ([]mcp.HeldPrompt, error) {
		if h := ownHold(); h != nil {
			return h.List(), nil
		}
		return mcp.HeldPrompts()
	}
	answerHeld = func(id, choice string, labels map[int][]string, other map[int]bool) (string, error) {
		if h := ownHold(); h != nil {
			return h.Answer(id, choice, labels, other)
		}
		return mcp.AnswerHeld(id, choice, labels, other)
	}
	controlSock = func() bool {
		if ownHold() != nil {
			return true
		}
		_, err := os.Lstat(mcp.ControlSockPath())
		return err == nil
	}
)

// tuiHold is this ccs's holder while it owns the hold: when no ccs serve
// --allow-write (or another ccs) does, every prompt from the hook is held
// for this list, whatever the focus, since questions arrive while you're
// in another window, until it's answered (here or on the page), sent
// back with T, or after tuiHoldWait, or ccs quits or yields to ccs serve;
// none of those but an answer decides anything. ccs selects the session
// holding it (autoSelect), so the question is in front of you when you
// come back.
var tuiHold struct {
	sync.Mutex
	h *mcp.Holder
}

// tuiHoldWait is how long this ccs holds a prompt: the hook's longest.
const tuiHoldWait = mcp.MaxHookWait

// tuiHoldConfig is how this ccs holds: every prompt (no MayHold: focus
// doesn't matter), no key-press release, tuiHoldWait, and it yields to ccs
// serve.
func tuiHoldConfig() mcp.HoldConfig {
	return mcp.HoldConfig{Who: "tui", Wait: func() time.Duration { return tuiHoldWait }, Yields: true}
}

// newTUIHolder makes this ccs's holder; stubbed in tests.
var newTUIHolder = func() *mcp.Holder { return mcp.NewHolder(tuiHoldConfig()) }

// ownHold is this ccs's holder while it owns the hold, else nil (never
// owned, or yielded to ccs serve).
func ownHold() *mcp.Holder {
	tuiHold.Lock()
	defer tuiHold.Unlock()
	if tuiHold.h != nil && !tuiHold.h.Owns() {
		tuiHold.h = nil
	}
	return tuiHold.h
}

// claimHold makes this ccs the owner when nothing else holds (the lock is
// free): at startup and from the live tick, at heldCmd's pace. A var:
// tests must never take the real lock.
var claimHold = claimHoldReal

func claimHoldReal() {
	tuiHold.Lock()
	defer tuiHold.Unlock()
	if tuiHold.h != nil && tuiHold.h.Owns() {
		return
	}
	tuiHold.h = nil
	if h := newTUIHolder(); h.Listen(false) == nil {
		tuiHold.h = h
	}
}

// releaseHold lets every held prompt go to its tab and gives the hold up.
func releaseHold(why string) {
	tuiHold.Lock()
	h := tuiHold.h
	tuiHold.h = nil
	tuiHold.Unlock()
	if h != nil {
		h.Close(why)
	}
}

type heldMsg struct {
	prompts []mcp.HeldPrompt
	err     error
}

type heldDoneMsg struct {
	name, text string
	err        error
}

// heldConfirm is a digit, A or D waiting for its y/n.
type heldConfirm struct {
	id, choice, what string
	labels           map[int][]string // an answer's chosen option, for a digit
}

// heldCmd asks ccs serve for its held prompts off the UI goroutine, at most
// every heldEvery and only while its control socket exists.
func (m *model) heldCmd() tea.Cmd {
	if m.heldBusy || time.Since(m.heldAt) < heldEvery {
		return nil
	}
	m.heldAt = time.Now()
	claimHold()
	if !controlSock() {
		m.heldPrompt = nil
		return nil
	}
	m.heldBusy = true
	return func() tea.Msg {
		p, err := heldPrompts()
		return heldMsg{p, err}
	}
}

func (m *model) applyHeld(msg heldMsg) {
	m.heldBusy = false
	if msg.err != nil {
		m.heldPrompt = nil
		return
	}
	held := map[string]mcp.HeldPrompt{}
	for _, p := range msg.prompts {
		if _, ok := held[p.Session]; !ok { // the oldest per session first
			held[p.Session] = p
		}
	}
	m.heldPrompt = held
	if m.heldConfirmed != nil {
		if _, ok := m.heldByID(m.heldConfirmed.id); !ok {
			m.heldConfirmed = nil // answered elsewhere meanwhile
		}
	}
}

func (m model) heldByID(id string) (mcp.HeldPrompt, bool) {
	for _, p := range m.heldPrompt {
		if p.ID == id {
			return p, true
		}
	}
	return mcp.HeldPrompt{}, false
}

// heldLines shows a held prompt with its keys: a question's menu
// (menu.go), or a permission prompt's tool with A, D and T.
func (m model) heldLines(h mcp.HeldPrompt, width int) []string {
	cut := func(s string) string { return transcript.Truncate(s, max(width, 10)) }
	lines := []string{"", "\033[1;" + amber + "m⏸ held by ccs\033[0m \033[90m(not on screen in its tab)\033[0m"}
	if len(h.Questions) > 0 {
		return append(lines, m.menuLines(h, width)...)
	}
	lines = append(lines, cut("\033[1;"+amber+"m? wants to use "+h.Tool+"\033[0m "+h.Summary))
	return append(lines, "\033[90m"+hints("A", "allow", "D", "deny", "T", "back to its tab")+"\033[0m")
}

// heldKey handles a key for its session's held prompt, in the bottom
// pane: a question's menu (menuKey) or a permission prompt's A and D (each
// asking y/n) and T. done is false when the key isn't the prompt's.
func (m *model) heldKey(k string, msg tea.KeyMsg, conv transcript.Conversation) (cmd tea.Cmd, done bool) {
	h, ok := m.heldPrompt[conv.SessionID]
	if !ok {
		return nil, false
	}
	name := m.sessionName(conv)
	if len(h.Questions) > 0 {
		return m.menuKey(h, name, k, msg)
	}
	switch {
	case len(k) == 1 && k >= "0" && k <= "9":
		// While a prompt is held every digit is the held prompt's: none may
		// reach anything below it.
		m.setHeldNote("a permission prompt: A allows, D denies")
		return nil, true
	case k == "A" || k == "a" || k == "D" || k == "d":
		choice, verb := "allow", "Allow"
		if k == "D" || k == "d" {
			choice, verb = "deny", "Deny"
		}
		m.heldConfirmed = &heldConfirm{id: h.ID, choice: choice, what: fmt.Sprintf("%s %s for %s? y/n", verb, h.Tool, name)}
		return nil, true
	case k == "T" || k == "t":
		return m.answerHeldCmd(h, name, "mac", nil, nil), true
	}
	return nil, false
}

// heldConfirmKey: y sends the confirmed digit, A or D; anything else drops it.
func (m *model) heldConfirmKey(k string) tea.Cmd {
	c := m.heldConfirmed
	m.heldConfirmed = nil
	h, ok := m.heldByID(c.id)
	if k != "y" || !ok {
		m.setHeldNote("not sent")
		return nil
	}
	return m.answerHeldCmd(h, m.sessionNameByID(h.Session), c.choice, nil, nil)
}

func (m model) sessionNameByID(id string) string {
	for _, it := range m.items {
		if it.conv.SessionID == id {
			return m.sessionName(it.conv)
		}
	}
	return id
}

func (m *model) answerHeldCmd(h mcp.HeldPrompt, name, choice string, labels map[int][]string, other map[int]bool) tea.Cmd {
	m.heldAt = time.Time{} // ask again straight after
	return func() tea.Msg {
		text, err := answerHeld(h.ID, choice, labels, other)
		return heldDoneMsg{name: name, text: text, err: err}
	}
}

func (m *model) applyHeldDone(msg heldDoneMsg) {
	m.heldErr = ""
	switch {
	case errors.Is(msg.err, mcp.ErrNoServe):
		m.heldErr = "Nothing holds prompts now: the prompt is in " + msg.name + "'s tab"
	case errors.Is(msg.err, mcp.ErrOutcomeUnknown):
		m.heldErr = "Sent to " + msg.name + ", but the reply never came: check its tab"
	case msg.err != nil:
		m.heldErr = "Couldn't answer " + msg.name + ": " + msg.err.Error()
	default:
		m.setHeldNote("✓ " + msg.name + ": " + strings.ReplaceAll(msg.text, "\n", " · "))
		return
	}
	m.heldNote = ""
}

// setHeldNote says how a held-prompt key went, in the pane.
func (m *model) setHeldNote(s string) { m.heldNote, m.heldErr = s, "" }

// selectedHeld is the selected session's held prompt, if ccs serve holds one.
func (m model) selectedHeld() (mcp.HeldPrompt, transcript.Conversation, bool) {
	if len(m.filtered) == 0 || m.cursor >= len(m.filtered) {
		return mcp.HeldPrompt{}, transcript.Conversation{}, false
	}
	conv := m.filtered[m.cursor].conv
	h, ok := m.heldPrompt[conv.SessionID]
	return h, conv, ok && m.live[conv.SessionID]
}

// heldPaneLines is the bottom pane's held prompt: the prompt and its keys,
// then the y/n, the last outcome, and how to focus or leave it.
func (m model) heldPaneLines(h mcp.HeldPrompt) []string {
	lines := m.heldLines(h, m.paneWidth()-4)
	switch {
	case m.heldConfirmed != nil && m.heldConfirmed.id == h.ID:
		lines = append(lines, "\033[1;"+amber+"m"+m.heldConfirmed.what+"\033[0m")
	case m.heldErr != "":
		lines = append(lines, "\033[31m"+m.heldErr+"\033[0m")
	case m.heldNote != "":
		lines = append(lines, "\033[90m"+m.heldNote+"\033[0m")
	}
	switch {
	case m.heldFocus && len(h.Questions) > 0:
		lines = append(lines, "\033[90m"+hints(keyEsc, "search (keeps your answers)")+"\033[0m")
	case m.heldFocus:
		lines = append(lines, "\033[90m"+hints(keyEnter, "back to its tab and open it", keyEsc, "search")+"\033[0m")
	default:
		lines = append(lines, "\033[90m"+hints(keyEnter, "answer it here")+"\033[0m")
	}
	return lines
}

// heldPaneKey handles a key while the bottom pane's held prompt has focus:
// a question's menu (menu.go), or a permission prompt's A or D (each with y/n) and T; letters
// in either case, Enter sends it back to its tab and opens it, Esc returns
// to the search. Keys for moving through the list fall through (done is
// false); every other key is the pane's, so none reaches the search. Right
// after an auto-select the guard drops keys still being typed.
func (m *model) heldPaneKey(msg tea.KeyMsg, prevKey time.Time) (tea.Cmd, bool) {
	h, conv, ok := m.selectedHeld()
	if !ok {
		m.heldFocus = false
		return nil, false
	}
	k := msg.String()
	menu := len(h.Questions) > 0
	switch k {
	case "ctrl+p", "ctrl+n", "pgup", "pgdown", "ctrl+c", "tab":
		return nil, false
	case "up", "down":
		if !menu {
			return nil, false
		}
	case "esc":
		if !menu || m.heldConfirmed != nil {
			m.heldFocus, m.heldConfirmed = false, nil
			return nil, true
		}
	}
	if !m.heldAutoAt.IsZero() {
		now := heldNow()
		if now.Sub(m.heldAutoAt) < heldGuard || now.Sub(prevKey) < heldGuardGap {
			m.setHeldNote("paused: ccs just brought this up, so that key was dropped; press it again")
			return nil, true
		}
		m.heldAutoAt = time.Time{}
	}
	if m.heldConfirmed != nil {
		return m.heldConfirmKey(k), true
	}
	if menu {
		cmd, done := m.heldKey(k, msg, conv)
		if !done && k == "esc" { // Esc at the first question: back to the search, progress kept
			m.heldFocus = false
			return nil, true
		}
		return cmd, true
	}
	if k == "enter" {
		m.heldFocus = false
		return tea.Sequence(m.answerHeldCmd(h, m.sessionName(conv), "mac", nil, nil), m.focusCmd(conv)), true
	}
	if cmd, done := m.heldKey(k, msg, conv); done {
		return cmd, true
	}
	m.setHeldNote("keys here: " + strings.TrimSpace(heldKeysFor(h)))
	return nil, true
}

func heldKeysFor(h mcp.HeldPrompt) string {
	switch {
	case len(h.Questions) == 0:
		return "A allow, D deny, T back to its tab"
	}
	return "↑↓ move, enter pick (space ticks pick-any), ← back, T back to its tab"
}

// autoSelect: once you've stopped typing in ccs for autoFocusIdle, select
// a session whose prompt ccs holds, the oldest first, and focus its answer
// pane behind the guard, whether or not ^X A is on: the hold is how ccs
// answers, so its question is put in front of you. With ^X A on it also
// selects a session waiting in its tab. One at a time: never off a held
// prompt that's still waiting (the next comes once it's answered or sent
// back). Never while a popup or prompt is open, the search holds text or
// any box holds unsent text. Once per wait. A deliberate exception to "the
// selection never changes without the user".
func (m *model) autoSelect() {
	if m.showUsage || m.prompting() || m.heldFocus || m.textInput.Value() != "" || m.hasDraft() ||
		heldNow().Sub(m.lastKeyAt) < autoFocusIdle ||
		slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open }) {
		return
	}
	if _, _, ok := m.selectedHeld(); ok {
		return // on a held prompt that's still waiting: stay until it's answered or sent back
	}
	if m.selectHandled == nil {
		m.selectHandled = map[string]string{}
	}
	pick, mark, held := "", "", false
	ids := make([]string, 0, len(m.heldPrompt))
	for id := range m.heldPrompt {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { // the oldest held first
		a, b := m.heldPrompt[ids[i]], m.heldPrompt[ids[j]]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		if m.live[id] && m.selectHandled[id] != "held "+m.heldPrompt[id].ID {
			pick, mark, held = id, "held "+m.heldPrompt[id].ID, true
			break
		}
	}
	if pick == "" && m.autoFocus {
		ids = ids[:0]
		for id, st := range m.chatStatus {
			if _, held := m.heldPrompt[id]; st.Status == "waiting" && m.live[id] && !held { // a held one is the branch above's
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			if mk := "wait " + m.chatStatus[id].Since.String(); m.selectHandled[id] != mk {
				pick, mark = id, mk
				break
			}
		}
	}
	if pick == "" {
		return
	}
	m.selectHandled[pick] = mark
	if len(m.filtered) > 0 && m.filtered[m.cursor].conv.SessionID == pick && (!held || m.heldFocus) {
		return
	}
	if slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == pick }) < 0 {
		return // not in the list (filtered out): nothing to select
	}
	m.selectSession(pick)
	if held {
		m.heldFocus, m.heldAutoAt = true, heldNow()
	}
	state.LogActivity("auto-select: selected %s in ccs (%s)", m.focusName(pick), map[bool]string{true: "held by ccs", false: "waiting in its tab"}[held])
}

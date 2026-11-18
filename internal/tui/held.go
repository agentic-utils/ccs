package tui

// Prompts held by `ccs hook permission` (docs/remote.md): by ccs serve for
// its page, or, when no ccs serve --allow-write runs, by this ccs itself
// (tuiHold) for this list. Answered from here by their id (the owner's
// holder directly, or the control socket: mcp.HeldPrompts, mcp.AnswerHeld),
// never by typing into a terminal: the prompt isn't on screen in its tab
// while it's held. The selected session's
// held prompt is answered in the bottom pane (Enter or a click focuses it;
// with ^X A on, ccs selects a session that starts waiting once you've
// stopped typing, and focuses it there), and in the ^X W overlay: a digit
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	answerHeld = func(id, choice string, labels map[int][]string) (string, error) {
		if h := ownHold(); h != nil {
			return h.Answer(id, choice, labels)
		}
		return mcp.AnswerHeld(id, choice, labels)
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
// --allow-write (or another ccs) does, prompts from the hook are held for
// this list, but only while ccs has the terminal's focus (you're looking at
// it): unfocused, a new prompt goes straight to its tab, and losing focus,
// tuiHoldWait or quitting lets held ones go there, with no decision. ccs
// serve takes the hold over (yield) whenever it starts with --allow-write.
var tuiHold struct {
	sync.Mutex
	h *mcp.Holder
}

// tuiFocused mirrors the terminal's focus for the holder, which asks off the
// UI goroutine; tuiFocusKnown says a focus report has come at all. Unlike
// the rest of ccs (where a terminal that doesn't report focus counts as
// focused), holding starts unfocused: until the terminal says ccs has
// focus, this ccs never claims the hold, so prompts go to their tabs.
var tuiFocused, tuiFocusKnown atomic.Bool

// tuiHoldWait is how long this ccs holds a prompt: you're at the Mac, so short.
const tuiHoldWait = 120 * time.Second

// newTUIHolder makes this ccs's holder; stubbed in tests.
var newTUIHolder = func() *mcp.Holder {
	return mcp.NewHolder(mcp.HoldConfig{
		Who:     "tui",
		Wait:    func() time.Duration { return tuiHoldWait },
		MayHold: tuiFocused.Load,
		Yields:  true,
	})
}

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
	if !tuiFocusKnown.Load() {
		return // focus unknown: never hold
	}
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

// holdFocus is the terminal's focus as the holder sees it; losing it lets
// held prompts go to their tabs.
func holdFocus(focused bool) {
	tuiFocusKnown.Store(true)
	tuiFocused.Store(focused)
	if !focused {
		if h := ownHold(); h != nil {
			h.ReleaseAll("ccs lost focus")
		}
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

// heldTyped: a held question menu with several questions or a pick-any one,
// answered by typing one answer per question.
func heldTyped(h mcp.HeldPrompt) bool { return len(h.Questions) > 0 && heldPick(h) == 0 }

// heldTypeHint says how to type a menu's answers.
const heldTypeHint = "type 1,2,… one per question (1+3 for pick-any), enter"

// parseHeldAnswers reads typed answers: one per question, in order,
// comma-separated; a pick-any question takes several numbers joined by
// "+"; spaces are ignored. It returns the chosen labels by question and a
// summary, or what's wrong.
func parseHeldAnswers(h mcp.HeldPrompt, typed string) (map[int][]string, string, error) {
	parts := strings.Split(strings.ReplaceAll(typed, " ", ""), ",")
	if len(parts) != len(h.Questions) {
		return nil, "", fmt.Errorf("%d answers for %d questions: one per question, comma-separated", len(parts), len(h.Questions))
	}
	labels := map[int][]string{}
	var said []string
	for i, part := range parts {
		q := h.Questions[i]
		nums := strings.Split(part, "+")
		if len(nums) > 1 && !q.Multi {
			return nil, "", fmt.Errorf("question %d takes one answer, not %s", i+1, part)
		}
		for _, n := range nums {
			k, err := strconv.Atoi(n)
			if err != nil || k < 1 || k > len(q.Options) {
				return nil, "", fmt.Errorf("question %d has options 1-%d, not %q", i+1, len(q.Options), n)
			}
			if !slices.Contains(labels[i], q.Options[k-1]) {
				labels[i] = append(labels[i], q.Options[k-1])
			}
		}
		said = append(said, q.Question+" → "+strings.Join(labels[i], ", "))
	}
	return labels, strings.Join(said, " · "), nil
}

// heldPick: the single single-choice question a digit can answer, its option count.
func heldPick(h mcp.HeldPrompt) int {
	if len(h.Questions) != 1 || h.Questions[0].Multi {
		return 0
	}
	return min(len(h.Questions[0].Options), 9)
}

// heldLines shows a held prompt, marked as ccs serve's, with its keys.
func heldLines(h mcp.HeldPrompt, width int) []string {
	cut := func(s string) string { return transcript.Truncate(s, max(width, 10)) }
	lines := []string{"", "\033[1;" + amber + "m⏸ held by ccs\033[0m \033[90m(not on screen in its tab)\033[0m"}
	switch {
	case len(h.Questions) > 0:
		for _, q := range h.Questions {
			title := q.Question
			if q.Multi {
				title += " (pick any)"
			}
			lines = append(lines, cut("\033[1m"+title+"\033[0m"))
			for i, o := range q.Options {
				lines = append(lines, cut(fmt.Sprintf("  %d. %s", i+1, o)))
			}
		}
		if n := heldPick(h); n > 0 {
			lines = append(lines, "\033[90m"+hints(fmt.Sprintf("1-%d", n), "answer", "T", "back to its tab")+"\033[0m")
		} else {
			lines = append(lines, "\033[90m"+heldTypeHint+", or "+hints("T", "back to its tab")+"\033[0m")
		}
	default:
		lines = append(lines, cut("\033[1;"+amber+"m? wants to use "+h.Tool+"\033[0m "+h.Summary))
		lines = append(lines, "\033[90m"+hints("A", "allow", "D", "deny", "T", "back to its tab")+"\033[0m")
	}
	return lines
}

// heldKey handles a key in the overlay for its session's held prompt; done
// is false when the key isn't one of the held prompt's.
func (m *model) heldKey(k string, conv transcript.Conversation) (cmd tea.Cmd, done bool) {
	h, ok := m.heldPrompt[conv.SessionID]
	if !ok {
		return nil, false
	}
	name := m.sessionName(conv)
	switch {
	case heldTyped(h) && len(k) == 1 && strings.ContainsAny(k, "0123456789,+ "):
		return nil, false // typed into the answer: the pane's buffer, or ^X W's box
	case len(k) == 1 && k >= "1" && k <= "9":
		// While a prompt is held every digit is the held prompt's: none may
		// reach the terminal-typing answer below it, whatever the transcript
		// last showed.
		n := int(k[0] - '0')
		if n > heldPick(h) {
			m.setHeldNote("no option " + k + " here")
			return nil, true
		}
		label := h.Questions[0].Options[n-1]
		m.heldConfirmed = &heldConfirm{id: h.ID, choice: "answer", labels: map[int][]string{0: {label}},
			what: fmt.Sprintf("Answer %s with %d. %s? y/n", name, n, label)}
		return nil, true
	case (k == "A" || k == "D") && len(h.Questions) == 0:
		choice, verb := "allow", "Allow"
		if k == "D" {
			choice, verb = "deny", "Deny"
		}
		m.heldConfirmed = &heldConfirm{id: h.ID, choice: choice, what: fmt.Sprintf("%s %s for %s? y/n", verb, h.Tool, name)}
		return nil, true
	case k == "T":
		return m.answerHeldCmd(h, name, "mac", nil), true
	}
	return nil, false
}

// heldTypedEnter parses typed answers for a held menu and, when they're
// right, asks y/n with a summary of each question's answer.
func (m *model) heldTypedEnter(h mcp.HeldPrompt, name, typed string) {
	labels, summary, err := parseHeldAnswers(h, typed)
	if err != nil {
		m.heldErr, m.waitErr, m.heldNote, m.waitNote = err.Error()+": nothing sent", err.Error()+": nothing sent", "", ""
		return
	}
	m.heldConfirmed = &heldConfirm{id: h.ID, choice: "answer", labels: labels, what: fmt.Sprintf("Answer %s: %s? y/n", name, summary)}
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
	return m.answerHeldCmd(h, m.sessionNameByID(h.Session), c.choice, c.labels)
}

func (m model) sessionNameByID(id string) string {
	for _, it := range m.items {
		if it.conv.SessionID == id {
			return m.sessionName(it.conv)
		}
	}
	return id
}

func (m *model) answerHeldCmd(h mcp.HeldPrompt, name, choice string, labels map[int][]string) tea.Cmd {
	m.heldAt = time.Time{} // ask again straight after
	return func() tea.Msg {
		text, err := answerHeld(h.ID, choice, labels)
		return heldDoneMsg{name: name, text: text, err: err}
	}
}

func (m *model) applyHeldDone(msg heldDoneMsg) {
	m.heldErr = ""
	switch {
	case errors.Is(msg.err, mcp.ErrNoServe):
		m.waitErr = "Nothing holds prompts now: the prompt is in " + msg.name + "'s tab"
	case errors.Is(msg.err, mcp.ErrOutcomeUnknown):
		m.waitErr = "Sent to " + msg.name + ", but the reply never came: check its tab"
	case msg.err != nil:
		m.waitErr = "Couldn't answer " + msg.name + ": " + msg.err.Error()
	default:
		m.setHeldNote("✓ " + msg.name + ": " + strings.ReplaceAll(msg.text, "\n", " · "))
		return
	}
	m.heldErr, m.heldNote = m.waitErr, ""
}

// setHeldNote says how a held-prompt key went, in the overlay and the pane.
func (m *model) setHeldNote(s string) { m.waitNote, m.heldNote, m.heldErr = s, s, "" }

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
	lines := heldLines(h, m.paneWidth()-4)
	switch {
	case m.heldConfirmed != nil && m.heldConfirmed.id == h.ID:
		lines = append(lines, "\033[1;"+amber+"m"+m.heldConfirmed.what+"\033[0m")
	case m.heldErr != "":
		lines = append(lines, "\033[31m"+m.heldErr+"\033[0m")
	case m.heldNote != "":
		lines = append(lines, "\033[90m"+m.heldNote+"\033[0m")
	}
	if m.heldFocus && heldTyped(h) {
		lines = append(lines, "answer: "+m.heldInput+"█")
	}
	if m.heldFocus {
		lines = append(lines, "\033[90m"+hints(keyEnter, "back to its tab and open it", keyEsc, "search")+"\033[0m")
	} else {
		lines = append(lines, "\033[90m"+hints(keyEnter, "answer it here")+"\033[0m")
	}
	return lines
}

// heldPaneKey handles a key while the bottom pane's held prompt has focus:
// the same keys as the overlay (a digit, A or D, each with y/n; T), letters
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
	switch k {
	case "up", "ctrl+p", "down", "ctrl+n", "pgup", "pgdown", "ctrl+c", "tab":
		return nil, false
	case "esc":
		m.heldFocus, m.heldConfirmed = false, nil
		return nil, true
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
	if heldTyped(h) {
		switch {
		case k == "enter" && strings.TrimSpace(m.heldInput) != "":
			m.heldTypedEnter(h, m.sessionName(conv), m.heldInput)
			if m.heldConfirmed != nil {
				m.heldInput = ""
			}
			return nil, true
		case k == "backspace":
			if m.heldInput != "" {
				m.heldInput = m.heldInput[:len(m.heldInput)-1]
			}
			return nil, true
		case len(k) == 1 && strings.ContainsAny(k, "0123456789,+ "):
			m.heldInput += k
			return nil, true
		}
	}
	if k == "enter" {
		m.heldFocus = false
		return tea.Sequence(m.answerHeldCmd(h, m.sessionName(conv), "mac", nil), m.focusCmd(conv)), true
	}
	if len(k) == 1 {
		k = strings.ToUpper(k)
	}
	if cmd, done := m.heldKey(k, conv); done {
		return cmd, true
	}
	m.setHeldNote("keys here: " + strings.TrimSpace(heldKeysFor(h)))
	return nil, true
}

func heldKeysFor(h mcp.HeldPrompt) string {
	switch {
	case len(h.Questions) == 0:
		return "A allow, D deny, T back to its tab"
	case heldPick(h) > 0:
		return fmt.Sprintf("1-%d answer, T back to its tab", heldPick(h))
	}
	return heldTypeHint + ", T back to its tab"
}

// autoSelect (part of ^X A): once you've stopped typing in ccs for
// autoFocusIdle, select a session that started waiting on you (a prompt ccs
// serve holds first, then a menu or prompt in its tab) and, when it's held,
// focus its answer pane, behind the guard. Never while a popup or prompt is
// open, the search holds text or any box holds unsent text. Once per wait.
// A deliberate exception to "the selection never changes without the
// user", asked for by turning ^X A on.
func (m *model) autoSelect() {
	if !m.autoFocus || m.showUsage || m.prompting() || m.waitOpen || m.heldFocus || m.textInput.Value() != "" || m.hasDraft() ||
		heldNow().Sub(m.lastKeyAt) < autoFocusIdle ||
		slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open }) {
		return
	}
	if m.selectHandled == nil {
		m.selectHandled = map[string]string{}
	}
	pick, mark, held := "", "", false
	ids := make([]string, 0, len(m.heldPrompt))
	for id := range m.heldPrompt {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if m.live[id] && m.selectHandled[id] != "held "+m.heldPrompt[id].ID {
			pick, mark, held = id, "held "+m.heldPrompt[id].ID, true
			break
		}
	}
	if pick == "" {
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

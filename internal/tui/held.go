package tui

// Prompts held by `ccs hook permission` (docs/remote.md): by ccs serve for
// its page, or, when no ccs serve --allow-write runs, by this ccs itself
// (tuiHold) for this list. Answered from here by their id (the owner's
// holder directly, or the control socket: mcp.HeldPrompts, mcp.AnswerHeld),
// never by typing into a terminal. Claude Code shows the same menu in the
// session's own tab meanwhile, and the first answer wins: one given in the
// tab drops ccs's copy on the next live tick (mcp.Holder.Reconcile). The
// selected session's held prompt is answered in the bottom pane: a
// question's menu (menu.go), or A allows and D denies a permission prompt
// (each asks y/n).

import (
	"errors"
	"fmt"
	"github.com/charmbracelet/x/ansi"
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

// heldEvery is the least time between asks for the held prompts;
// heldQuick while a wait's held copy is due (heldPending).
const (
	heldEvery = 2 * time.Second
	heldQuick = 250 * time.Millisecond
)

// heldGrace is how long the bottom pane holds back the transcript's view of
// a question (askLines) while something holds prompts: after a wait begins,
// since its held copy usually lands a moment after the status says waiting,
// and after a held prompt leaves the list (answered, sent back), since the
// status still says waiting until Claude Code moves on. Within it the pane
// shows only a dim "waiting on you…" status line (heldPendingLine), so the
// question only ever appears in the held menu; a question nothing holds
// (no hook, an older one, sent back to its tab) shows its transcript view
// once it has passed.
const heldGrace = time.Second

// answeredWait is the wait a held prompt ccs answered belonged to: its
// statusUpdatedAt (since) and when the answer was sent (at). While the
// session still reports that same wait, the answer just hasn't reached
// its status yet, so the transcript's view stays hidden however long that
// takes, up to heldAnsweredMax. setStat drops it when the wait ends or
// changes.
type answeredWait struct{ since, at time.Time }

// heldAnsweredMax bounds that: a session still on the same wait this long
// after its answer is waiting on something else, so its view shows.
const heldAnsweredMax = 30 * time.Second

// heldPendingLine is the status line while heldPending holds the
// transcript's view back: neutral, never the question itself.
const heldPendingLine = "\033[90m  waiting on you…\033[0m"

// paneNow is the bottom pane's clock: the time Update last finished (paneAt),
// so chatRows and chatView agree on heldPending within one frame; heldNow
// when nothing has set it (a test drawing the model directly).
func (m model) paneNow() time.Time {
	if !m.paneAt.IsZero() {
		return m.paneAt
	}
	return heldNow()
}

// heldPending: the selected pane should wait for session id's held copy
// rather than draw its question from the transcript: within heldGrace of
// its held prompt leaving the list (answered, sent back, or the holder
// gone), or, while something holds prompts (the last ask answered, or one
// is on its way), of its wait beginning. Without a holder only its tab can
// answer a new question, so its view shows at once.
func (m model) heldPending(id string) bool {
	if _, held := m.heldPrompt[id]; held {
		return false
	}
	now := m.paneNow()
	if a, ok := m.heldAnswered[id]; ok && m.chatStatus[id].Status == "waiting" && m.chatStatus[id].Since.Equal(a.since) && now.Sub(a.at) < heldAnsweredMax {
		return true // answered from ccs: its status and transcript lag behind, however long
	}
	if now.Sub(m.heldGone[id]) < heldGrace {
		return true
	}
	if m.heldPrompt == nil && !m.heldBusy {
		return false
	}
	return now.Sub(m.waitSeen[id]) < heldGrace
}

// heldDue: some wait began within heldGrace and its held copy hasn't come.
func (m model) heldDue() bool {
	for id, t := range m.waitSeen {
		if _, held := m.heldPrompt[id]; !held && heldNow().Sub(t) < heldGrace && m.chatStatus[id].Status == "waiting" {
			return true
		}
	}
	return false
}

// heldRetryMsg asks for the held prompts again while one is due.
type heldRetryMsg struct{}

// heldRetry keeps asking every heldQuick while a wait's held copy is due,
// whatever is selected (the chat tick runs only for a live selection).
func (m model) heldRetry() tea.Cmd {
	if !m.heldDue() {
		return nil
	}
	return tea.Tick(heldQuick, func(time.Time) tea.Msg { return heldRetryMsg{} })
}

// dropHeld forgets every held prompt (an error, or nothing holds any more),
// marking each gone so its question isn't drawn from the transcript at once.
func (m *model) dropHeld() {
	m.noteGone(nil)
	m.heldPrompt = nil
}

// noteGone marks gone each held prompt that held no longer has.
func (m *model) noteGone(held map[string]mcp.HeldPrompt) {
	for id, p := range m.heldPrompt {
		if q, ok := held[id]; !ok || q.ID != p.ID { // answered or sent back: its status may still say waiting
			if m.heldGone == nil {
				m.heldGone = make(map[string]time.Time)
			}
			m.heldGone[id] = heldNow()
		}
	}
}

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
	answerHeld = func(id, choice string, labels map[int][]string, other map[int]bool, notes map[int]string) (string, error) {
		if h := ownHold(); h != nil {
			return h.Answer(id, choice, labels, other, notes)
		}
		return mcp.AnswerHeld(id, choice, labels, other, notes)
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

// releaseHold gives the hold up on the way out (quit, an error, or the
// restart into an update), handing every held prompt back with retry:
// each hook dials the next holder, so its question isn't lost, dropped or
// answered. Never a final release (Close), which would send them to their
// tabs for good.
func releaseHold(why string) {
	tuiHold.Lock()
	h := tuiHold.h
	tuiHold.h = nil
	tuiHold.Unlock()
	if h != nil {
		handBack(h, why)
	}
}

// handBack hands h's prompts back with retry; a var so tests can see it.
var handBack = func(h *mcp.Holder, why string) { h.Handoff(why) }

type heldMsg struct {
	prompts []mcp.HeldPrompt
	err     error
}

type heldDoneMsg struct {
	name, text string
	session    string // the session the answer was for: its outcome shows only there
	prompt     string // the held prompt answered
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
	every := heldEvery
	if m.heldDue() {
		every = heldQuick
	}
	if m.heldBusy || time.Since(m.heldAt) < every {
		return nil
	}
	m.heldAt = time.Now()
	claimHold()
	if !controlSock() {
		m.dropHeld()
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
		m.dropHeld()
		return
	}
	held := map[string]mcp.HeldPrompt{}
	for _, p := range msg.prompts {
		if _, ok := held[p.Session]; !ok { // the oldest per session first
			held[p.Session] = p
		}
	}
	m.noteGone(held)
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
func (m model) heldLines(h mcp.HeldPrompt, width int, active bool) []string {
	if len(h.Questions) > 0 {
		return m.menuLines(h, width, active)
	}
	// A permission prompt, in the menu's style: one header line, what it
	// wants to run wrapped, one hint row.
	lines := []string{paneHeader("\033[1;"+amber+"m"+h.Tool+"\033[0m \033[90m· permission\033[0m", width)}
	lines = append(lines, wrapIndent("", "\033[1m", h.Summary, width)...)
	if !active {
		return lines
	}
	return append(lines, "", paneHintsFit(width, "a", "allow", "d", "deny", keyEnter, "open its tab", keyEsc, "list"))
}

// paneHeader is a held prompt's one header line: left on the left, a dim
// "also in its tab" on the right, padded to width.
func paneHeader(left string, width int) string {
	right := "\033[90malso in its tab\033[0m" // Claude Code shows it there too
	gap := width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 2 {
		return ansi.Truncate(left, max(width, 10), "…")
	}
	return left + strings.Repeat(" ", gap) + right
}

// paneHints is the pane's one dim hint row: keys bold, pairs joined by " · ".
func paneHints(pairs ...string) string { return paneHintsFit(0, pairs...) }

// paneHintsFit is paneHints within width (0: any). When the row is too
// wide, the obvious ↑↓ pair goes first, then pairs from the end, so it's
// never cut mid-word and Esc's way out stays as long as it can.
func paneHintsFit(width int, pairs ...string) string {
	row := func(ps []string) string {
		parts := make([]string, 0, len(ps)/2)
		for i := 0; i+1 < len(ps); i += 2 {
			parts = append(parts, "\033[1m"+ps[i]+"\033[22m "+ps[i+1])
		}
		return "\033[90m" + strings.Join(parts, " · ") + "\033[0m"
	}
	if width <= 0 || ansi.StringWidth(row(pairs)) <= width {
		return row(pairs)
	}
	if len(pairs) >= 4 && pairs[0] == "↑↓" {
		pairs = pairs[2:]
	}
	for len(pairs) > 2 && ansi.StringWidth(row(pairs)) > width {
		pairs = pairs[:len(pairs)-2]
	}
	return row(pairs)
}

// heldKey handles a key for its session's held prompt, in the bottom
// pane: a question's menu (menuKey) or a permission prompt's A and D (each
// asking y/n). done is false when the key isn't the prompt's.
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
	return m.answerHeldCmd(h, m.sessionNameByID(h.Session), c.choice, nil, nil, nil)
}

func (m model) sessionNameByID(id string) string {
	for _, it := range m.items {
		if it.conv.SessionID == id {
			return m.sessionName(it.conv)
		}
	}
	return id
}

func (m *model) answerHeldCmd(h mcp.HeldPrompt, name, choice string, labels map[int][]string, other map[int]bool, notes map[int]string) tea.Cmd {
	m.heldAt = time.Time{} // ask again straight after
	return func() tea.Msg {
		text, err := answerHeld(h.ID, choice, labels, other, notes)
		return heldDoneMsg{name: name, session: h.Session, prompt: h.ID, text: text, err: err}
	}
}

func (m *model) applyHeldDone(msg heldDoneMsg) {
	m.heldErr = ""
	m.heldNoteFor = msg.session
	switch {
	case errors.Is(msg.err, mcp.ErrNoServe):
		m.heldErr = "Nothing holds prompts now: the prompt is in " + msg.name + "'s tab"
	case errors.Is(msg.err, mcp.ErrOutcomeUnknown):
		m.heldErr = "Sent to " + msg.name + ", but the reply never came: check its tab"
	case msg.err != nil:
		m.heldErr = "Couldn't answer " + msg.name + ": " + msg.err.Error()
	default:
		m.setHeldNote("✓ " + msg.name + ": " + strings.ReplaceAll(msg.text, "\n", " · "))
		m.chatAfter, m.chatAfterID = msg.session, msg.prompt // keepChatAfterAnswer gives it the keys
		if m.heldAnswered == nil {
			m.heldAnswered = make(map[string]answeredWait)
		}
		m.heldAnswered[msg.session] = answeredWait{since: m.chatStatus[msg.session].Since, at: heldNow()}
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
// then the y/n and the last outcome; while the menu doesn't have the keys,
// how to give them to it.
func (m model) heldPaneLines(h mcp.HeldPrompt) []string {
	active := m.heldKeysActive()
	lines := m.heldLines(h, m.heldInner(), active)
	switch {
	case m.heldConfirmed != nil && m.heldConfirmed.id == h.ID:
		lines = append(lines, "\033[1;"+amber+"m"+m.heldConfirmed.what+"\033[0m")
	case m.heldErr != "":
		lines = append(lines, "\033[31m"+m.heldErr+"\033[0m")
	case m.heldNote != "" && (m.heldNoteFor == "" || m.heldNoteFor == h.Session):
		lines = append(lines, "\033[90m"+m.heldNote+"\033[0m")
	}
	if !active {
		lines = append(lines, paneHints("→", "answer it here", keyEnter, "the same"))
	}
	return lines
}

// heldKeysActive: the menu (or a permission prompt's keys) takes keys now,
// with no Enter first: the selected session's prompt is held, the search is
// empty, nothing else has the keyboard (a box, a popup, a prompt, a chord,
// the usage screen), and it hasn't been left with Esc (menuLeft) since it
// was selected. heldFocus (Enter, a click, an auto-select) also gives it
// the keys.
func (m model) heldKeysActive() bool {
	// Only while it's drawn (heldDrawn: the same rule chatAsk draws by), and
	// never with a pending chord or a prompt open, focused or not.
	if m.chordX || m.prompting() {
		return false
	}
	h, _, ok := m.heldDrawn()
	if !ok {
		return false
	}
	// A search being typed has the keys, whatever focused the menu before:
	// Enter or a digit there must never answer the prompt.
	if m.textInput.Value() != "" {
		return false
	}
	if m.heldFocus {
		return true
	}
	return m.menuLeft != h.ID && !m.chatFocus
}

// heldDrawn is the held prompt the bottom pane draws right now: the
// selected session's, held and live, while it waits on its menu or prompt
// (waitSignal, chatAsk's own rule) and nothing covers the pane (the usage
// screen, help, a popup). Drawing, key routing and the menu's appearance
// clock all go by it, so the menu never takes a key it isn't showing.
func (m model) heldDrawn() (mcp.HeldPrompt, transcript.Conversation, bool) {
	if m.showUsage || m.helpOpen || m.popupOpen() {
		return mcp.HeldPrompt{}, transcript.Conversation{}, false
	}
	h, conv, ok := m.selectedHeld()
	if !ok || m.waitSignal(conv) != waitPrompt {
		return mcp.HeldPrompt{}, transcript.Conversation{}, false
	}
	return h, conv, true
}

// heldPaneKey handles a key while the held prompt has the keys
// (heldKeysActive): a question's menu (menu.go), or a permission prompt's A
// or D (each with y/n), T, and Enter (sends it back and opens its tab).
// Esc leaves it for the list (menuLeft): then ↑/↓ move the list, and →
// or Enter give it the keys again (Tab is never the menu's: it goes to the next session that needs you). ^P/^N, PgUp/PgDn and ^X chords
// always fall through; so does any key the prompt doesn't use, which goes
// to the search. Right after an auto-select the guard drops keys still
// being typed.
func (m *model) heldPaneKey(msg tea.KeyMsg, prevKey time.Time) (tea.Cmd, bool) {
	h, conv, ok := m.selectedHeld()
	if !ok {
		m.heldFocus = false
		return nil, false
	}
	k := msg.String()
	menu := len(h.Questions) > 0
	boxOpen := menu && m.menus[h.ID] != nil && m.menus[h.ID].editing != boxNone
	review := menu && m.menus[h.ID] != nil && m.menus[h.ID].step >= len(h.Questions)
	switch {
	case (k == "pgup" || k == "pgdown") && review && !boxOpen:
		// the review scrolls with them (menuKey)
	case k == "ctrl+p", k == "ctrl+n", k == "pgup", k == "pgdown", k == "ctrl+c", k == "tab", k == "ctrl+x":
		return nil, false
	case (strings.HasPrefix(k, "ctrl+") || strings.HasPrefix(k, "alt+")) && !boxOpen:
		return nil, false // the search's editing keys; an open box edits with them
	case (k == "up" || k == "down") && !menu:
		return nil, false
	}
	leave := func() (tea.Cmd, bool) {
		m.heldFocus, m.heldConfirmed, m.menuLeft = false, nil, h.ID
		return nil, true
	}
	if k == "esc" && (!menu || m.heldConfirmed != nil) {
		return leave()
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
		if !done && k == "esc" { // Esc at the first question: back to the list, progress kept
			return leave()
		}
		return cmd, done
	}
	if k == "enter" {
		m.heldFocus = false
		return m.focusCmd(conv), true // the menu is in its tab too: answer it there
	}
	return m.heldKey(k, msg, conv)
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
		heldNow().Sub(m.lastActive()) < autoFocusIdle ||
		slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open }) {
		return
	}
	if m.selectionNeedsYou() {
		return // you may be reading its question: stay until it's answered (Tab moves on)
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
	m.autoSelectAt = heldNow()
	if held {
		m.heldFocus, m.heldAutoAt = true, heldNow()
	}
	state.LogActivity("auto-select: selected %s in ccs (%s)", m.focusName(pick), map[bool]string{true: "held in ccs too", false: "waiting in its tab"}[held])
}

// lastActive is when you last did anything in ccs that auto-select must
// wait out: a key, the mouse (a click or the wheel moves the selection
// too), or ccs's own last auto-select, so two never land back to back.
func (m model) lastActive() time.Time {
	t := m.lastKeyAt
	for _, u := range []time.Time{m.lastMouse, m.autoSelectAt} {
		if u.After(t) {
			t = u
		}
	}
	return t
}

// selectionNeedsYou: the selected session itself waits on you (a question
// ccs holds for it, or a menu or prompt in its tab) or its message box
// holds unsent text. Auto-select and auto-focus never take you away from
// it, since you may be reading it; Tab moves on by hand.
func (m model) selectionNeedsYou() bool {
	if len(m.filtered) == 0 || m.cursor >= len(m.filtered) {
		return false
	}
	c := m.filtered[m.cursor].conv
	if _, held := m.heldPrompt[c.SessionID]; held {
		return true
	}
	return m.waitSignal(c) == waitPrompt || strings.TrimSpace(m.chatInput.Value()) != ""
}

// keepChatAfterAnswer: once a question answered from ccs has gone from the
// pane, the same session's message box takes the keys, as ^S would give
// them. Another question of that session's drawn first keeps them for its
// menu; moving the selection drops it.
func (m *model) keepChatAfterAnswer() {
	id := m.chatAfter
	if id == "" {
		return
	}
	if len(m.filtered) == 0 || m.cursor >= len(m.filtered) || m.filtered[m.cursor].conv.SessionID != id {
		m.chatAfter, m.chatAfterID = "", ""
		return
	}
	if h, _, drawn := m.heldDrawn(); drawn && h.ID != m.chatAfterID {
		m.chatAfter, m.chatAfterID = "", "" // the next question: its menu has the keys
		return
	}
	if h, held := m.heldPrompt[id]; held && h.ID == m.chatAfterID || m.boxHidden() {
		return // the answered one still showing: wait for it to go
	}
	if !m.selectedLive() || m.showUsage || m.popupOpen() || m.prompting() || m.textInput.Value() != "" {
		return
	}
	m.chatFocus, m.heldFocus, m.heldAutoAt = true, false, time.Time{}
	m.chatAfter, m.chatAfterID = "", ""
}

// heldCount is how many prompts this ccs holds now, an answer on its way
// included; stubbed in tests.
var heldCount = func() int {
	if h := ownHold(); h != nil {
		return h.Active()
	}
	return 0
}

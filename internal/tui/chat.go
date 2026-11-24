package tui

import (
	"errors"
	"fmt"
	"hash/maphash"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ============================================================================
// transcript.Message box: talk to the selected live session from ccs
// ============================================================================

// chatFastRefresh is how often the selected live session's transcript and
// busy/idle status are checked (one stat and one small file read).
const chatFastRefresh = 250 * time.Millisecond

// chatTickMsg starts the fast read; gen is its chain, as liveTickMsg's.
type chatTickMsg struct{ gen int }

type chatTickResult struct {
	id      string
	conv    *transcript.Conversation // set when the transcript grew
	stat    sessions.SessionStat
	gen     int
	tickGen int // the fast tick's chain (chatTickMsg.gen)
}

type sendDoneMsg struct {
	id   string
	note string
	err  error
	text string // what was sent, given back to its box when nothing was (errUnsent)
	// keepDraft: the text wasn't typed in the box (a command's result, ^S), so
	// a failure puts nothing back there.
	keepDraft bool
}

func (m model) selectedLive() bool {
	return len(m.filtered) > 0 && m.live[m.filtered[m.cursor].conv.SessionID]
}

// selectionMoved runs when the user moves the selection (keys, wheel,
// click): landing on a live session puts typing in its message box, and
// anywhere else (no box, or one hidden behind its question) in the search.
// Never called for list changes caused by typing a search, so a search can't
// end up in the message box; the search gets typing back only when asked
// (Esc in the box, or a click on the search row).
func (m *model) selectionMoved() {
	m.chatFocus = m.selectedLive() && !m.boxHidden() && !m.showUsage
	m.heldFocus, m.heldAutoAt, m.menuLeft = false, time.Time{}, ""
	// A held prompt's outcome belongs to its session: it goes with the selection.
	if m.heldNoteFor != "" && (len(m.filtered) == 0 || m.filtered[m.cursor].conv.SessionID != m.heldNoteFor) {
		if m.heldNoteShown && m.errorMsg == m.heldNote {
			m.errorMsg, m.noteMsg = "", ""
		}
		m.heldNote, m.heldErr, m.heldNoteFor, m.heldNoteShown = "", "", "", false
	}
}

// syncChatDraft gives the message box the selected session's draft, keeping
// the one it held with its own session.
func (m *model) syncChatDraft() {
	id := ""
	if len(m.filtered) > 0 {
		id = m.filtered[m.cursor].conv.SessionID
	}
	if id == m.chatFor {
		return
	}
	if v := m.chatInput.Value(); v != "" && m.chatFor != "" {
		if m.chatDrafts == nil {
			m.chatDrafts = make(map[string]string)
		}
		m.chatDrafts[m.chatFor] = joinDraft(m.chatDrafts[m.chatFor], v)
		m.chatInput.SetValue("")
	}
	if d, ok := m.chatDrafts[id]; ok {
		m.chatInput.SetValue(joinDraft(m.chatInput.Value(), d))
		m.chatInput.CursorEnd()
		delete(m.chatDrafts, id)
	}
	m.chatFor = id
}

// chatRows is the height the message area takes under the preview: what a
// waiting session asks, a status line and the bordered box (hidden while it
// waits). Zero unless a live session is selected.
func (m model) chatRows() int {
	if m.showUsage || !m.selectedLive() {
		return 0
	}
	if m.boxHidden() {
		return 1 + len(m.chatAsk())
	}
	return 4 + len(m.runLines())
}

// boxHidden: the selected session waits on a menu or prompt, which holds any
// message until it's answered, so the bottom pane shows the question instead.
func (m model) boxHidden() bool {
	return m.selectedLive() && m.waitSignal(m.filtered[m.cursor].conv) == waitPrompt
}

// chatAsk is the selected session's question menu (or permission request)
// while it waits, at most a third of the screen.
func (m model) chatAsk() []string {
	conv := m.filtered[m.cursor].conv
	if m.waitSignal(conv) != waitPrompt {
		return nil
	}
	if h, _, held := m.heldDrawn(); held {
		body, tail, room, review := m.heldParts(h)
		var inner []string
		if review {
			out, _ := reviewWindow(body, mn(m, h).revTop, room)
			inner = append(out, tail...)
		} else {
			inner = append(cursorWindow(body, room), tail...)
		}
		return heldBox(inner, m.paneWidth(), m.heldKeysActive())
	}
	if m.heldPending(conv.SessionID) { // its held copy is due, or was just answered: never the transcript's view meanwhile
		return nil
	}
	lines := askLines(conv, m.paneWidth()-2)
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return cutAsk(lines, m.askLimit())
}

// askLimit is how many rows the bottom pane gives a question ccs doesn't
// hold (only its tab can answer it): half the screen.
func (m model) askLimit() int { return max(m.height/2, 3) }

// heldLimit is how many rows a held prompt's box takes at most, borders
// included: it grows to the menu, but the transcript above keeps at least
// a third of the pane. heldRoom is what's inside the borders.
func (m model) heldLimit() int {
	_, _, top := m.listLayout()
	return max((m.height-top)*2/3-1, 7)
}

func (m model) heldRoom() int { return m.heldLimit() - 2 }

// heldInner is the text width inside a held prompt's box: the pane less the
// border and one column of padding each side.
func (m model) heldInner() int { return m.paneWidth() - 4 }

// heldBox draws a held prompt inside the message box's own frame: the same
// rounded border, purple while it has the keys (as the message box is when
// focused), grey otherwise, the full pane wide.
func heldBox(inner []string, width int, active bool) []string {
	border := lipgloss.Color("240")
	if active {
		border = lipgloss.Color("62")
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(max(width-2, 1)).Render(strings.Join(inner, "\n"))
	return strings.Split(box, "\n")
}

// cutAsk cuts a question taller than the pane; its tab shows the rest.
func cutAsk(lines []string, limit int) []string {
	if len(lines) > limit {
		return append(lines[:limit-1], "  \033[90m… its tab shows the rest\033[0m")
	}
	return lines
}

// reviewWindow is the part of a held menu's review on screen from line top:
// all of it when it fits, else limit-1 lines and a "↓ more" while the end
// isn't shown. atEnd says the last line is on screen, which is what lets
// Enter send it (noteReview records it).
func reviewWindow(lines []string, top, limit int) (out []string, atEnd bool) {
	if len(lines) <= limit {
		return lines, true
	}
	room := limit - 1
	top = min(max(top, 0), len(lines)-room)
	if top+room >= len(lines) {
		return lines[top:], true
	}
	return append(append([]string{}, lines[top:top+room]...), "\033[90m↓ more · enter sends once you've seen the end\033[0m"), false
}

// noteReview clamps a held menu's review scroll to what the pane shows and
// marks it seen once its last line has been drawn. From Update, after
// every message, so it reflects what is on screen when the next key comes.
func (m *model) noteReview() {
	h, _, ok := m.heldDrawn()
	if !ok || len(h.Questions) == 0 {
		return
	}
	mn := m.menus[h.ID]
	if mn == nil || mn.step < len(h.Questions) {
		return
	}
	body, _, limit, _ := m.heldParts(h)
	if len(body) > limit {
		mn.revTop = min(max(mn.revTop, 0), len(body)-(limit-1))
	} else {
		mn.revTop = 0
	}
	if _, atEnd := reviewWindow(body, mn.revTop, limit); atEnd {
		mn.revSeen = true
		if strings.HasPrefix(m.heldNote, "scroll to the end first") {
			m.heldNote = "" // seen now: the refusal no longer applies
		}
	}
}

// heldParts is a held prompt's box contents split as heldSplit does, with
// room, the rows its body gets. A short screen gives the body the room
// first: the footer's rule and "enter sends" go before the hint row does,
// and an open box only gives up the blank line above it. chatAsk draws by
// it and noteReview scrolls by it, so both agree on what's on screen.
func (m model) heldParts(h mcp.HeldPrompt) (body, tail []string, room int, review bool) {
	body, tail, review = m.heldSplit(h, m.heldPaneLines(h))
	room = m.heldRoom() - len(tail)
	for room < 5 && len(tail) > 1 && (mn(m, h).editing == boxNone || ansi.Strip(tail[0]) == "") {
		tail, room = tail[1:], room+1
	}
	return body, tail, room, review
}

// heldSplit splits a held prompt's pane into its body (header, question,
// options; or the review), which scrolls when the pane is short, and its
// footer (what Enter sends, an open box, the hint row, the outcome note),
// which always stays on screen. review says the menu is at its review.
func (m model) heldSplit(h mcp.HeldPrompt, pane []string) (body, tail []string, review bool) {
	mnu := mn(m, h)
	if len(h.Questions) == 0 { // a permission prompt: its rows scroll, No's box and the hints stay
		n := len(m.permLines(h, m.heldInner(), false))
		if mnu.editing != boxNone {
			n -= 2 + len(menuBoxLines(mnu, m.heldInner()))
		}
		n = min(max(n, 0), len(pane))
		return pane[:n], pane[n:], false
	}
	n := len(m.menuLines(h, m.heldInner(), false)) // the menu without its footer
	if mnu.editing != boxNone && mnu.step < len(h.Questions) {
		n -= 2 + len(menuBoxLines(mnu, m.heldInner())) // the open box (a blank, its lines, its hints) belongs to the footer
	}
	n = min(max(n, 0), len(pane))
	return pane[:n], pane[n:], mnu.step >= len(h.Questions)
}

// cursorWindow fits a question's body into room lines, keeping its header
// line and the highlighted "❯" row on screen,
// with "↑ more" or "↓ more" where options are out of view.
func cursorWindow(body []string, room int) []string {
	if len(body) <= room || room < 4 {
		if len(body) > room && room > 0 {
			return body[:room]
		}
		return body
	}
	head, rest := body[:1], body[1:]
	at := 0
	for i, l := range rest {
		if strings.Contains(l, "❯") {
			at = i
		}
	}
	space := room - len(head)
	top := 0
	if at >= space-1 {
		top = at - (space - 2)
	}
	top = min(top, max(len(rest)-space, 0))
	out := append([]string{}, head...)
	view := rest[top:]
	if top > 0 {
		out = append(out, "  \033[90m↑ more\033[0m")
		space--
		view = view[1:]
	}
	if len(view) > space {
		view = append(append([]string{}, view[:space-1]...), "  \033[90m↓ more\033[0m")
	}
	return append(out, view...)
}

// mn is a held prompt's menu, or a fresh one (for drawing).
func mn(m model, h mcp.HeldPrompt) *heldMenu {
	if x := m.menus[h.ID]; x != nil {
		return x
	}
	return newHeldMenu(h.ID)
}

// chatTarget is the live session the fast refresh follows: the selected one.
func (m model) chatTarget() (transcript.Conversation, bool) {
	if m.showUsage || !m.selectedLive() {
		return transcript.Conversation{}, false
	}
	return m.filtered[m.cursor].conv, true
}

// ensureChatTick starts the fast refresh while there's a chatTarget. Each
// tick reschedules through Update, so it stops by itself otherwise.
func (m *model) ensureChatTick() tea.Cmd {
	if _, ok := m.chatTarget(); m.chatTickOn || m.quitting || !ok {
		return nil
	}
	m.chatTickOn = true
	every, gen := chatFastRefresh, m.chatGen
	if m.idle() {
		every = chatIdleRefresh
	}
	return tea.Tick(every, func(time.Time) tea.Msg { return chatTickMsg{gen} })
}

// chatTickCmd reads the chatTarget's status and any new transcript lines
// off the UI goroutine.
func (m model) chatTickCmd() tea.Cmd {
	conv, ok := m.chatTarget()
	gen, tickGen := m.gen, m.chatGen
	if !ok {
		// Still this chain's result: without its tickGen, Update would never
		// clear chatTickOn once a wake had moved chatGen past 0, and the fast
		// tick would stay stopped for good.
		return func() tea.Msg { return chatTickResult{tickGen: tickGen} }
	}
	pid := m.livePIDs[conv.SessionID]
	return func() (msg tea.Msg) {
		res := chatTickResult{id: conv.SessionID, gen: gen, tickGen: tickGen}
		defer func() {
			if r := recover(); r != nil {
				transcript.RecoverWorkerValue(r)
				msg = res
			}
		}()
		switch conv.Kind {
		case transcript.KindCodex:
			res.stat = sessions.CodexStat(conv.FilePath)
		case transcript.KindAgy:
			res.stat = sessions.AgyStat(conv.SessionID, conv.FilePath)
		default:
			if f, err := sessions.ReadSessionFile(pid); err == nil && f.SessionID == conv.SessionID {
				res.stat = f.Stat()
			}
		}
		if _, busy := sessions.LiveReads.LoadOrStore(conv.FilePath, true); !busy {
			defer sessions.LiveReads.Delete(conv.FilePath)
			if c, err := transcript.ParseAppended(&conv); err == nil && c != nil && c.Size != conv.Size {
				res.conv = c
			}
		}
		return res
	}
}

// sessionName is how the status line names a session: its Claude Code name,
// else its title, else its topic.
func (m model) sessionName(conv transcript.Conversation) string {
	if st, ok := m.chatStatus[conv.SessionID]; ok && st.Name != "" {
		return st.Name
	}
	if conv.Title != "" {
		return transcript.Truncate(conv.Title, 30)
	}
	return transcript.Truncate(transcript.GetTopic(conv), 30)
}

// waitingLine says what a waiting session asks and where to answer it.
func waitingLine(conv transcript.Conversation) string {
	// The question itself is drawn just above (askLines).
	switch {
	case len(conv.Questions()) > 0:
		return "waiting on you · answer it in its tab: " + keyEnter + " opens it"
	case conv.Pending != nil:
		return "waiting on you · approve it in its tab: " + keyEnter + " opens it"
	}
	return "its question is only shown in its own tab: " + keyEnter + " opens it"
}

// chatView draws the status line and message box for the selected session.
func (m model) chatView() string {
	box := m.chatBox(m.filtered[m.cursor].conv, m.chatInput, m.chatFocus, m.paneWidth())
	if m.boxHidden() {
		status, _, _ := strings.Cut(box, "\n")
		if _, _, held := m.heldDrawn(); held { // the question in the message box's place, under its status line
			return strings.Join(append([]string{status}, m.chatAsk()...), "\n")
		}
		return strings.Join(append(m.chatAsk(), status), "\n")
	}
	if run := m.runLines(); len(run) > 0 { // a ! command's output, above the box
		return strings.Join(run, "\n") + "\n" + box
	}
	return box
}

// spinnerFrames are the working line's glyph, out and back as Claude Code's is.
var spinnerFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}

// workingLine is what a busy session is doing, drawn as Claude Code's terminal
// does: its glyph and word in the agent's colour, then how long this turn has
// run and its output tokens so far. Claude Code keeps its verb and tip to
// itself, so the word is always the same and there's no tip.
func workingLine(conv transcript.Conversation, st sessions.SessionStat, now time.Time) string {
	_, colour := transcript.AgentGlyph(conv)
	frame := spinnerFrames[int(now.UnixMilli()/int64(chatFastRefresh/time.Millisecond))%len(spinnerFrames)]
	var facts []string
	// The status clock starts with the turn and steps don't reset it, but
	// Claude Code doesn't always reset it between turns either.
	if start := conv.TurnStart(st.Since); !start.IsZero() {
		facts = append(facts, formatTook(now.Sub(start)))
		if n := conv.OutputSince(start); n > 0 {
			count := transcript.FormatTokens(int(n))
			if n >= 1000 && n < 10000 { // the 2.5k Claude Code would say, not 2k
				count = fmt.Sprintf("%.1fk", float64(n)/1000)
			}
			facts = append(facts, "↓ "+count+" tokens")
		}
	}
	line := "\033[" + colour + "m" + frame + " Working…\033[0m"
	if len(facts) > 0 {
		line += " \033[90m(" + strings.Join(facts, " · ") + ")\033[0m"
	}
	return line
}

// chatBox draws conv's status line and the message box in, width wide.
func (m model) chatBox(conv transcript.Conversation, in textinput.Model, focus bool, width int) string {
	id, name := conv.SessionID, m.sessionName(conv)
	working := ""
	if st := m.chatStatus[id]; st.Status == "busy" {
		working = workingLine(conv, st, frameNow()) // the clock frames are keyed by (frame.go)
	}
	full := width
	width = max(width-lipgloss.Width(working), 0) // what's left for a note beside it
	left := ""
	switch {
	case m.pending[id] != "" && m.sending:
		left = "\033[90m  sending: " + chatSnippet(m.pending[id], width-40) + "\033[0m"
	case m.pending[id] != "" && m.held[id]:
		left = "\033[33m  ⏸ held: approve it in " + name + " · " + chatSnippet(m.pending[id], width-50-len(name)) + "\033[0m"
	case m.pending[id] != "" && m.typed[id] && !m.sentAt[id].IsZero() && time.Since(m.sentAt[id]) > heldTimeout:
		why := " · typed, but not in its conversation: it may be sitting unsent in its prompt, press Enter in its tab"
		left = "\033[31m  ✗ not arrived: " + chatSnippet(m.pending[id], width-20-len(why)) + why + "\033[0m"
	case m.pending[id] != "" && !m.sentAt[id].IsZero() && time.Since(m.sentAt[id]) > heldTimeout:
		why := " · if it runs with bypass permissions, approve it in that session"
		if m.chatStatus[id].Status == "waiting" {
			why = " · it may wait until its menu is answered"
		}
		left = "\033[33m  ⚠ not picked up yet: " + chatSnippet(m.pending[id], width-25-len(why)) + why + "\033[0m"
	case m.pending[id] != "" && m.chatStatus[id].Status == "busy":
		left = "\033[90m  sent: " + chatSnippet(m.pending[id], width-48) + " · Claude sees it after its current step\033[0m"
	case m.pending[id] != "":
		left = "\033[90m  sent: " + chatSnippet(m.pending[id], width-40) + "\033[0m"
	case m.sendNote[id] != "":
		left = "  " + m.sendNote[id]
	case m.chatStatus[id].Status == "waiting" && m.heldPrompt[id].ID == "" && m.heldPending(id): // its held copy is due, or was just answered (heldGrace)
		left = heldPendingLine
	case m.chatStatus[id].Status == "waiting" && m.heldPrompt[id].ID == "": // menus and prompts never reach the transcript; a held one is drawn above
		left = "\033[1;" + amber + "m  ? " + waitingLine(conv) + "\033[0m"
	}
	width = full
	left = ansi.Truncate(working+left, max(width, 1), "…") + "\033[0m"
	status := left + strings.Repeat(" ", max(width-lipgloss.Width(left), 0))

	in.Placeholder = "message " + name + "…  · !cmd runs here · /cmd in it"
	in.Width = chatInputWidth(width)
	if !focus {
		in.Blur() // no cursor while typing goes to the search
	}
	var border lipgloss.TerminalColor = lipgloss.Color("240")
	if focus {
		border = lipgloss.Color("62")
	}
	if bashMode(in.Value()) {
		in = asBashMode(in)
		border = bashColour
		if strings.TrimSpace(ansi.Strip(left)) == "" { // a note, a warning or the working line keeps its place
			hint := "  ! runs a command here · in " + tildePath(conv.Cwd)
			status = lipgloss.NewStyle().Foreground(bashColour).Render(ansi.Truncate(hint, max(full, 1), "…"))
			status += strings.Repeat(" ", max(full-lipgloss.Width(status), 0))
		}
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Width(max(width-2, 1)).Render(in.View())
	return status + "\n" + box
}

// bashColour is Claude Code's bashBorder, the pink its prompt turns in bash
// mode: rgb(255,0,135) in its light theme, rgb(253,93,177) in its dark one.
var bashColour = lipgloss.AdaptiveColor{Light: "#ff0087", Dark: "#fd5db1"}

// bashMode says whether the message box's text would run as a command in
// ccs: a single leading "!" ("!!" sends a literal "!" message), with
// commandLine's own trimming.
func bashMode(text string) bool {
	t := strings.TrimLeft(text, " \t")
	return strings.HasPrefix(t, "!") && !strings.HasPrefix(t, "!!")
}

// asBashMode is the box drawn as Claude Code draws bash mode: its "›" becomes
// a pink "!" and the text's own leading "!" is left out, so it shows once.
// in is a copy, so the draft itself is untouched.
func asBashMode(in textinput.Model) textinput.Model {
	v := in.Value()
	cut := strings.Index(v, "!") + 1
	pos := in.Position()
	in.Prompt = "! "
	in.PromptStyle = lipgloss.NewStyle().Foreground(bashColour).Bold(true)
	in.SetValue(v[cut:])
	in.SetCursor(max(pos-cut, 0))
	return in
}

// tildePath is path with the home folder shown as ~.
func tildePath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if path == home {
			return "~"
		}
		if strings.HasPrefix(path, home+"/") {
			return "~" + path[len(home):]
		}
	}
	return path
}

// chatInputWidth is the text width of a message box width wide.
func chatInputWidth(width int) int { return max(width-8, 1) }

// chatSnippet is a pending message on one line, cut to width with a single "…".
func chatSnippet(s string, width int) string {
	return ansi.Truncate(strings.Join(strings.Fields(s), " "), max(width, 10), "…")
}

// sendCmd sends the message box's text to the selected live session.
func (m *model) sendCmd() tea.Cmd {
	if !m.selectedLive() {
		return nil
	}
	return m.sendTo(m.filtered[m.cursor].conv)
}

// sendTo sends the text in the message box to conv, a live session, and
// empties the box.
func (m *model) sendTo(conv transcript.Conversation) tea.Cmd {
	text := strings.TrimSpace(m.chatInput.Value())
	return m.sendLine(conv, text, text)
}

// sendLine sends payload to conv and empties the box; draft is the box's
// text as typed, which a failure puts back (an escaped "!!x" stays "!!x").
func (m *model) sendLine(conv transcript.Conversation, draft, payload string) tea.Cmd {
	in := &m.chatInput
	text := payload
	if text == "" {
		return nil
	}
	if m.sending {
		m.errorMsg = "Still sending the last message…"
		return nil
	}
	id, name, pid, verified, kind := conv.SessionID, m.sessionName(conv), m.livePIDs[conv.SessionID], !m.unverified[conv.SessionID], conv.Kind
	wire := text
	if box := m.pasted[id]; box != nil {
		wire = withImages(text, box.images)
	}
	if m.pending == nil {
		m.pending = make(map[string]string)
	}
	m.pending[id] = text
	delete(m.sendNote, id)
	m.sending = true
	in.SetValue("")
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				transcript.RecoverWorkerValue(r)
				msg = sendDoneMsg{id: id, note: "✗ Internal error, logged to " + transcript.WorkerPanicLog, err: errors.New("panic"), text: draft}
			}
		}()
		deliver := deliverMessage
		switch kind {
		case transcript.KindCodex:
			deliver = sessions.DeliverCodexMessage
		case transcript.KindAgy:
			deliver = sessions.DeliverAgy
		}
		note, err := deliver(pid, id, name, wire, verified)
		return sendDoneMsg{id: id, note: note, err: err, text: draft}
	}
}

// deliverMessage sends text to a live Claude Code session: typed into its
// terminal (iTerm tab / tmux pane) first when safe, else through its message
// socket as a fallback (for apps without a terminal, menus, slash/shell
// commands, or multi-line/control text). Never both: once typing or the socket
// attempted delivery, a later error isn't retried the other way.
// Typing needs a verified pid: keys typed into a recycled one reach a stranger.
func deliverMessage(pid int, id, name, text string, verified bool) (string, error) {
	changed := func(err error) (string, error) {
		return "✗ Couldn't reach " + name + ": its session has changed", fmt.Errorf("%w: session file: %v", sessions.ErrUnsent, err)
	}
	f, err := sessions.ReadSessionFile(pid)
	if err != nil || f.SessionID != id {
		return changed(err)
	}
	t := strings.TrimSpace(text)
	isCmd := strings.HasPrefix(t, "/") || strings.HasPrefix(t, "!")
	if verified && f.Status != "waiting" && !isCmd && !hasControl(text) {
		where, err := typeIntoSession(pid, text)
		switch {
		case where != "" && err == nil:
			return "✓ Typed into " + name + "'s " + where + " (as you)", nil
		case where != "" && errors.Is(err, proc.ErrTimedOut):
			return "⚠ Typing into " + name + "'s " + where + " timed out; it may have been typed", err
		case where != "":
			return "✗ Couldn't type into " + name + "'s " + where + ": " + err.Error(), err
		}
	}
	if f.Socket != "" {
		msgID, err := sendViaSocket(f.Socket, sessions.PeerToken(pid, f.Socket), text)
		if msgID != "" {
			sessions.SentMsgs.Store(msgID, id) // so a receipt can find the session
		}
		if err == nil {
			return "✓ Sent to " + name + " via its message socket (as a message from ccs)", nil
		}
		if !errors.Is(err, sessions.ErrNotConnected) {
			return "✗ Sending to " + name + " may have failed: " + err.Error(), err
		}
	}
	switch {
	case f.Status == "waiting":
		return "✗ Not typed into " + name + ": it's showing a menu, answer it in its tab", fmt.Errorf("%w: on a menu", sessions.ErrUnsent)
	case !verified:
		return "✗ Not typed into " + name + ": its process couldn't be checked against its session file", fmt.Errorf("%w: unverified pid", sessions.ErrUnsent)
	case isCmd:
		return "✗ Not typed into " + name + ": its message socket isn't reachable, and typed, a line starting " + t[:1] + " would run as a command", fmt.Errorf("%w: a command if typed", sessions.ErrUnsent)
	case hasControl(text):
		return "✗ Couldn't type into " + name + "'s terminal: the message contains control characters", fmt.Errorf("%w: control characters", sessions.ErrUnsent)
	}
	return "✗ Couldn't reach " + name + ": it isn't in iTerm or tmux and its message socket isn't reachable", fmt.Errorf("%w: unreachable", sessions.ErrUnsent)
}

// sendViaSocket writes one message to a Claude Code session's message
// socket: newline-delimited JSON, an auth line with the session's peer token
// first, then the message wrapped so the session sees it came from ccs.
func sendViaSocket(path, token, text string) (string, error) {
	return sessions.SendEnvelope(path, token, ccsSender, transcript.CcsNote, text, true)
}

// waitReceipt delivers the next receipt to Update; it blocks, so an idle
// screen stays still.
func waitReceipt() tea.Msg { return <-sessions.ReceiptsCh }

// typeIntoSession types text into the terminal running pid and presses
// Enter on its own a moment later (sessions.SubmitDelay): its tmux pane, else
// its iTerm tab. where names what it used ("" if neither found it). A timeout
// may still have typed it, so it's not retried. Typing gives no receipt, so
// checkDelivered watches the transcript and the status line says when a typed
// message hasn't arrived.
func typeIntoSession(pid int, text string) (where string, err error) {
	return typeKeys(pid, text, true)
}

// typedNote: a send's note says the message was typed into its terminal.
func typedNote(note string) bool { return strings.HasPrefix(note, "✓ Typed into ") }

// typeKeys is typeIntoSession with Enter optional: a digit alone picks a
// question menu's option.
func typeKeys(pid int, text string, enter bool) (where string, err error) {
	return sessions.TypeKeys(pid, text, enter)
}

// checkDelivered clears a pending message once it shows up in the session's
// transcript, sent through the socket (wrapped) or typed.
func (m *model) checkDelivered(id string) {
	text := m.pending[id]
	if text == "" || m.sending {
		return
	}
	for _, item := range m.items {
		if item.conv.SessionID != id {
			continue
		}
		msgs := item.conv.Messages
		for i := len(msgs) - 1; i >= 0 && i >= len(msgs)-20; i-- {
			body := msgs[i].Text
			if _, b, ok := transcript.PeerParts(body); ok {
				body = b
			}
			if msgs[i].Role == "user" && strings.TrimSpace(transcript.StripImageNote(body)) == text {
				delete(m.pending, id)
				delete(m.sentAt, id)
				delete(m.held, id)
				switch {
				case m.typed[id]:
					m.sendNote[id] = "✓ Delivered to " + m.sessionName(item.conv) + " (typed, as you)"
				case strings.Contains(m.sendNote[id], "socket"):
					m.sendNote[id] = "✓ Delivered to " + m.sessionName(item.conv) + " (as a message from ccs)"
				}
				delete(m.typed, id)
				return
			}
		}
		// Claude Code logs a message as queued the moment it arrives while
		// it's busy (over the socket, or typed and submitted), then hands it
		// to Claude at the next step.
		for _, q := range item.conv.Queued {
			body := q
			if _, b, ok := transcript.PeerParts(q); ok {
				body = b
			}
			if strings.TrimSpace(transcript.StripImageNote(body)) == text {
				delete(m.sentAt, id)
			}
		}
	}
}

// applyReceipt updates the status line from a session's delivery receipt.
func (m *model) applyReceipt(r sessions.ReceiptMsg) {
	name := r.ID
	for _, item := range m.items {
		if item.conv.SessionID == r.ID {
			name = m.sessionName(item.conv)
		}
	}
	if m.sendNote == nil {
		m.sendNote = make(map[string]string)
	}
	if m.held == nil {
		m.held = make(map[string]bool)
	}
	delete(m.sentAt, r.ID) // a receipt beats the guess
	switch r.Status {
	case "held":
		m.held[r.ID] = true
	case "delivered":
		delete(m.held, r.ID) // approved; the transcript shows it next
	default:
		why := map[string]string{
			"denied":  "declined in that session",
			"expired": "it was held and expired without approval",
			"refused": "that session isn't accepting messages",
			"dropped": "dropped at its inbox",
		}[r.Status]
		if why == "" {
			why = r.Status
		}
		if r.Status == "dropped" && r.Reason != "" {
			why += " (" + r.Reason + ")"
		}
		delete(m.held, r.ID)
		delete(m.pending, r.ID)
		m.sendNote[r.ID] = "✗ " + name + " didn't take it: " + why
	}
}

// heldTimeout is how long a socket message may go unqueued before ccs says
// the session may be holding it.
const heldTimeout = 5 * time.Second

// Claude Code frames socket messages as coming from a peer session, so a
// recipient can take one from ccs for another Claude and try to answer it
// there. The sender name and a leading note say it's the user; peerParts
// drops the note again for the preview.
const ccsSender = "the user, via ccs"

// peerEnds maps message keys to the sessions that sent and received them,
// across every session ccs has.
type peerEnds struct{ senders, recipients map[string]string }

func (m model) peerEnds() peerEnds {
	e := peerEnds{map[string]string{}, map[string]string{}}
	for _, it := range m.items {
		for _, p := range it.conv.Peers {
			if p.Sent {
				e.senders[p.Key] = it.conv.SessionID
			} else {
				e.recipients[p.Key] = it.conv.SessionID
			}
		}
	}
	return e
}

// sessionAt is the live session listening on a uds:/…/<pid>.sock address.
func (m model) sessionAt(addr string) string {
	base := filepath.Base(strings.TrimPrefix(addr, "uds:"))
	pid, err := strconv.Atoi(strings.TrimSuffix(base, ".sock"))
	if err != nil || !strings.HasPrefix(addr, "uds:") {
		return ""
	}
	for id, p := range m.livePIDs {
		if p == pid {
			return id
		}
	}
	return ""
}

// peerOther is the session at the other end of conv's peer message p: the
// one whose transcript holds the same message, else (live) the one on its
// socket, else for a received one the session named as its sender. arrived:
// a sent message found in the recipient's transcript.
func (m model) peerOther(e peerEnds, conv transcript.Conversation, p transcript.PeerRef) (id string, arrived bool) {
	if p.Sent {
		if id := e.recipients[p.Key]; id != "" && id != conv.SessionID {
			return id, true
		}
		return m.sessionAt(p.Addr), false
	}
	if id := e.senders[p.Key]; id != "" && id != conv.SessionID {
		return id, true
	}
	if id := m.sessionAt(p.Addr); id != "" {
		return id, true
	}
	for _, it := range m.items {
		if p.Name != "" && it.conv.Title == p.Name && it.conv.SessionID != conv.SessionID {
			return it.conv.SessionID, true
		}
	}
	return "", false
}

// peerLabels is what the preview says about each of conv's peer messages'
// other ends, by message index: "To name ✓ arrived" or "From name".
func (m model) peerLabels(conv transcript.Conversation) map[int]string {
	if len(conv.Peers) == 0 {
		return nil
	}
	e := m.peerEnds()
	byID := m.convByID()
	out := make(map[int]string, len(conv.Peers))
	for _, p := range conv.Peers {
		id, arrived := m.peerOther(e, conv, p)
		name := ""
		if c, ok := byID[id]; ok {
			name = m.sessionName(c)
		}
		switch {
		case p.Sent && name != "" && arrived:
			out[p.Idx] = name + " \033[32m✓ arrived\033[0m"
		case p.Sent && name != "":
			out[p.Idx] = name
		case p.Sent:
			out[p.Idx] = transcript.ShortAddr(p.Addr)
		case name != "":
			out[p.Idx] = name
		}
	}
	return out
}

// peerLabelCache memoises peerLabels for the selected conversation. Working
// them out walks every session's peer messages and copies every listed
// conversation, and the preview asks on every frame and every scroll step, so
// it's done again only when something it reads has changed (peerLabelKey).
type peerLabelCache struct {
	key    uint64
	valid  bool
	notes  map[int]string
	builds int // how often peerLabels ran, for tests
	// topics is a hash of each untitled session's topic once a message has
	// given it (transcript.MessageTopic), which no later message changes:
	// finding it can walk every message of a long session.
	topics map[string]uint64
}

var topicSeed = maphash.MakeSeed()

// topicKey stands for what an untitled session's name falls back to (its
// topic), for peerLabelKey: the topic's hash once a message has given it,
// kept by session; until then its message count, since any message may.
func (c *peerLabelCache) topicKey(conv *transcript.Conversation) uint64 {
	if c == nil {
		return uint64(len(conv.Messages))
	}
	if k, ok := c.topics[conv.SessionID]; ok {
		return k
	}
	t, ok := transcript.MessageTopic(conv)
	if !ok {
		return uint64(len(conv.Messages))
	}
	if c.topics == nil {
		c.topics = map[string]uint64{}
	}
	k := maphash.String(topicSeed, t)
	c.topics[conv.SessionID] = k
	return k
}

// cachedPeerLabels is peerLabels through m.peerNotes.
func (m model) cachedPeerLabels(conv transcript.Conversation) map[int]string {
	c := m.peerNotes
	if c == nil || len(conv.Peers) == 0 {
		return m.peerLabels(conv)
	}
	k := m.peerLabelKey(conv)
	if !c.valid || c.key != k {
		c.key, c.valid, c.notes = k, true, m.peerLabels(conv)
		c.builds++
	}
	return c.notes
}

// peerLabelKey hashes what peerLabels reads: conv's peers, and for every
// listed session (in order, since a name match takes the first) its id, its
// peers, title, the name its status gives it and, with neither, its topic
// (what sessionName falls back to), plus the live sessions' pids. Not their
// messages: a message anywhere that isn't between sessions leaves the labels
// as they were. It reads the items in place, never copying a conversation.
func (m model) peerLabelKey(conv transcript.Conversation) uint64 {
	h := uint64(14695981039346656037)
	str := func(s string) {
		for i := 0; i < len(s); i++ {
			h = (h ^ uint64(s[i])) * 1099511628211
		}
		h = (h ^ 0xff) * 1099511628211 // separator
	}
	num := func(n int) { h = (h ^ uint64(n)) * 1099511628211 }
	str(conv.SessionID)
	num(len(conv.Peers))
	for i := range m.items {
		c := &m.items[i].conv
		str(c.SessionID)
		str(c.Title)
		num(len(c.Peers))
		name := m.chatStatus[c.SessionID].Name
		str(name)
		if name == "" && c.Title == "" {
			num(int(m.peerNotes.topicKey(c)))
		}
	}
	var live uint64 // the map's order is random: sum each entry's own hash
	for id, pid := range m.livePIDs {
		e := uint64(14695981039346656037)
		for i := 0; i < len(id); i++ {
			e = (e ^ uint64(id[i])) * 1099511628211
		}
		live += (e ^ uint64(pid)) * 1099511628211
	}
	num(int(live))
	return h
}

// convByID indexes the listed sessions.
func (m model) convByID() map[string]transcript.Conversation {
	out := make(map[string]transcript.Conversation, len(m.items))
	for _, it := range m.items {
		out[it.conv.SessionID] = it.conv
	}
	return out
}

// sessionLinks are the ^O popup's jumps to the sessions conv messaged or
// heard from, each once, newest first.
func (m model) sessionLinks(conv transcript.Conversation) []linkItem {
	e := m.peerEnds()
	byID := m.convByID()
	seen := map[string]bool{}
	var out []linkItem
	for i := len(conv.Peers) - 1; i >= 0; i-- {
		p := conv.Peers[i]
		id, _ := m.peerOther(e, conv, p)
		c, ok := byID[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		dir := "← from "
		if p.Sent {
			dir = "→ to "
		}
		out = append(out, linkItem{sessionLink + id, "\033[36m" + dir + m.sessionName(c) + "\033[39m"})
	}
	return out
}

// sessionLink starts a ^O link that selects a session in the list.
const sessionLink = "ccs:session:"

// selectSession puts the cursor on session id, clearing the search if it
// hides it.
func (m *model) selectSession(id string) {
	find := func() int {
		return slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id })
	}
	if find() < 0 {
		if m.revealed == nil {
			m.revealed = map[string]bool{}
		}
		m.revealed[id] = true // a headless run is shown once asked for
		m.textInput.SetValue("")
		m.updateFilter()
	}
	if i := find(); i >= 0 {
		m.cursor = i
		m.previewScroll = 0
		m.selectionMoved()
	} else {
		m.errorMsg = "That session isn't in the list (older than --max-age, or filtered out)"
	}
}

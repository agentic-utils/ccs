package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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

type chatTickMsg struct{}

type chatTickResult struct {
	id   string
	conv *transcript.Conversation // set when the transcript grew
	stat sessions.SessionStat
	gen  int
}

type sendDoneMsg struct {
	id      string
	note    string
	err     error
	text    string // what was sent, given back to its box when nothing was (errUnsent)
	overlay bool   // sent from the overlay's box, not the main one
}

func (m model) selectedLive() bool {
	return len(m.filtered) > 0 && m.live[m.filtered[m.cursor].conv.SessionID]
}

// selectionMoved runs when the user moves the selection (keys, wheel,
// click): landing on a live session puts typing in its message box. Never
// called for list changes caused by typing a search, so a search can't end
// up in the message box.
// The message box takes focus only on Enter, never by moving the selection.
func (m *model) selectionMoved() {
	m.chatFocus = false
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
	return 4
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
	lines := askLines(conv, m.paneWidth()-2)
	for i, l := range lines {
		lines[i] = "  " + l
	}
	if limit := max(m.height/2, 3); len(lines) > limit { // the overlay shows the rest
		lines = append(lines[:limit-1], "  \033[90m… ^X W shows the rest\033[0m")
	}
	return lines
}

// chatTarget is the live session the fast refresh follows: the overlay's
// while it's open, else the selected one.
func (m model) chatTarget() (transcript.Conversation, bool) {
	if m.waitOpen {
		c, ok := m.waitConv()
		return c, ok && m.live[c.SessionID]
	}
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
	return tea.Tick(chatFastRefresh, func(time.Time) tea.Msg { return chatTickMsg{} })
}

// chatTickCmd reads the chatTarget's status and any new transcript lines
// off the UI goroutine.
func (m model) chatTickCmd() tea.Cmd {
	conv, ok := m.chatTarget()
	if !ok {
		return func() tea.Msg { return chatTickResult{} }
	}
	gen := m.gen
	pid := m.livePIDs[conv.SessionID]
	return func() (msg tea.Msg) {
		res := chatTickResult{id: conv.SessionID, gen: gen}
		defer func() {
			if r := recover(); r != nil {
				transcript.RecoverWorkerValue(r)
				msg = res
			}
		}()
		if conv.Kind == transcript.KindCodex {
			res.stat = sessions.CodexStat(conv.FilePath)
		} else if f, err := sessions.ReadSessionFile(pid); err == nil && f.SessionID == conv.SessionID {
			res.stat = f.Stat()
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
	case answerable(conv) > 0:
		return "waiting on you · ^X W answers it by number, " + keyEnter + " opens its tab"
	case len(conv.Questions()) > 0:
		return "waiting on you · answer it in its tab: " + keyEnter + " opens it"
	case conv.Pending != nil:
		return "waiting on you · approve it in its tab: " + keyEnter + " opens it"
	}
	return "its question is only shown in its own tab: " + keyEnter + " opens it"
}

// chatView draws the status line and message box for the selected session.
func (m model) chatView() string {
	box := m.chatBox(m.filtered[m.cursor].conv, m.chatInput, m.chatFocus && !m.waitOpen, m.paneWidth())
	if m.boxHidden() {
		status, _, _ := strings.Cut(box, "\n")
		return strings.Join(append(m.chatAsk(), status), "\n")
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
	if !st.Since.IsZero() { // the status clock starts with the turn; steps don't reset it
		facts = append(facts, formatTook(now.Sub(st.Since)))
		if n := conv.OutputSince(st.Since); n > 0 {
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
		working = workingLine(conv, st, time.Now())
	}
	full := width
	width = max(width-lipgloss.Width(working), 0) // what's left for a note beside it
	left := ""
	switch {
	case m.pending[id] != "" && m.sending:
		left = "\033[90m  sending: " + chatSnippet(m.pending[id], width-40) + "\033[0m"
	case m.pending[id] != "" && m.held[id]:
		left = "\033[33m  ⏸ held: approve it in " + name + " · " + chatSnippet(m.pending[id], width-50-len(name)) + "\033[0m"
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
	case m.chatStatus[id].Status == "waiting": // menus and prompts never reach the transcript
		left = "\033[1;" + amber + "m  ? " + waitingLine(conv) + "\033[0m"
	}
	width = full
	left = ansi.Truncate(working+left, max(width, 1), "…") + "\033[0m"
	status := left + strings.Repeat(" ", max(width-lipgloss.Width(left), 0))

	in.Placeholder = "message " + name + "…"
	in.Width = chatInputWidth(width)
	if !focus {
		in.Blur() // no cursor while typing goes to the search
	}
	border := lipgloss.Color("240")
	if focus {
		border = lipgloss.Color("62")
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Width(max(width-2, 1)).Render(in.View())
	return status + "\n" + box
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
	return m.sendTo(m.filtered[m.cursor].conv, false)
}

// sendTo sends the text in the overlay's box (or the main one) to conv, a
// live session, and empties the box.
func (m *model) sendTo(conv transcript.Conversation, overlay bool) tea.Cmd {
	in := &m.chatInput
	if overlay {
		in = &m.waitInput
	}
	text := strings.TrimSpace(in.Value())
	if text == "" {
		return nil
	}
	if m.sending {
		if overlay {
			m.waitErr = "Still sending the last message…"
		} else {
			m.errorMsg = "Still sending the last message…"
		}
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
				msg = sendDoneMsg{id: id, note: "✗ Internal error, logged to " + transcript.WorkerPanicLog, err: errors.New("panic")}
			}
		}()
		deliver := deliverMessage
		if kind == transcript.KindCodex {
			deliver = func(_ int, id, name, text string, _ bool) (string, error) {
				return sessions.DeliverCodex(id, name, text)
			}
		}
		note, err := deliver(pid, id, name, wire, verified)
		return sendDoneMsg{id: id, note: note, err: err, text: text, overlay: overlay}
	}
}

// deliverMessage sends text to a live session: through its message socket
// if it can be reached, else typed into its terminal. Never both: once the
// socket accepted a connection, a later error isn't retried another way.
// Typing needs a verified pid: keys typed into a recycled one reach a stranger.
func deliverMessage(pid int, id, name, text string, verified bool) (string, error) {
	changed := func(err error) (string, error) {
		return "✗ Couldn't reach " + name + ": its session has changed", fmt.Errorf("%w: session file: %v", sessions.ErrUnsent, err)
	}
	f, err := sessions.ReadSessionFile(pid)
	if err != nil || f.SessionID != id {
		return changed(err)
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
	// Read again: the socket attempt can take seconds and a menu may have come
	// up since. Typed keys would land in it and could pick an option.
	if f, err := sessions.ReadSessionFile(pid); err != nil || f.SessionID != id {
		return changed(err)
	} else if f.Status == "waiting" {
		return "✗ Not typed into " + name + ": it's showing a menu, answer it in its tab", fmt.Errorf("%w: on a menu", sessions.ErrUnsent)
	}
	if !verified {
		return "✗ Not typed into " + name + ": its process couldn't be checked against its session file", fmt.Errorf("%w: unverified pid", sessions.ErrUnsent)
	}
	where, err := typeIntoSession(pid, text)
	switch {
	case where == "":
		return "✗ Couldn't reach " + name + ": its message socket isn't reachable and it isn't in iTerm or tmux", fmt.Errorf("%w: unreachable", sessions.ErrUnsent)
	case errors.Is(err, proc.ErrTimedOut):
		return "⚠ Typing into " + name + "'s " + where + " timed out; it may have been typed", err
	case err != nil:
		return "✗ Couldn't type into " + name + "'s " + where + ": " + err.Error(), err
	}
	return "✓ Typed into " + name + "'s " + where + " (as you)", nil
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
// Enter: its tmux pane, else its iTerm tab. where names what it used ("" if
// neither found it). A timeout may still have typed it, so it's not retried.
func typeIntoSession(pid int, text string) (where string, err error) {
	return typeKeys(pid, text, true)
}

// typeKeys is typeIntoSession with Enter optional: a digit alone picks a
// question menu's option.
func typeKeys(pid int, text string, enter bool) (where string, err error) {
	if strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f || r == utf8.RuneError }) {
		return "terminal", errors.New("the message contains control characters")
	}
	out, err := proc.RunBounded(2*time.Second, nil, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	tty := strings.TrimSpace(string(out))
	if err != nil || tty == "" || tty == "??" {
		return "", nil
	}
	tty = "/dev/" + tty
	if os.Getenv("TMUX") != "" {
		panes, err := proc.RunBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
		if target := sessions.TmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			if _, err := proc.RunBounded(5*time.Second, nil, "tmux", "send-keys", "-t", target, "-l", "--", text); err != nil || !enter {
				return "tmux pane", err
			}
			_, err := proc.RunBounded(5*time.Second, nil, "tmux", "send-keys", "-t", target, "Enter")
			return "tmux pane", err
		}
	}
	if !sessions.InITerm() {
		return "", nil
	}
	script := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					tell s to write text %q newline %t
					return "typed"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty, text, enter)
	out, err = proc.RunBounded(5*time.Second, nil, "osascript", "-e", script)
	if err != nil {
		return "iTerm tab", err
	}
	if strings.TrimSpace(string(out)) != "typed" {
		return "", nil
	}
	return "iTerm tab", nil
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
			if _, b, ok := peerParts(body); ok {
				body = b
			}
			if msgs[i].Role == "user" && strings.TrimSpace(stripImageNote(body)) == text {
				delete(m.pending, id)
				delete(m.sentAt, id)
				delete(m.held, id)
				if strings.Contains(m.sendNote[id], "socket") {
					m.sendNote[id] = "✓ Delivered to " + m.sessionName(item.conv) + " (as a message from ccs)"
				}
				return
			}
		}
		// Claude Code logs a socket message as queued the moment it arrives,
		// then hands it to Claude at the next step (or at once when idle).
		for _, q := range item.conv.PeerQueued {
			if _, b, ok := peerParts(q); ok && strings.TrimSpace(b) == text {
				delete(m.sentAt, id)
			}
		}
	}
}

// applyReceipt updates the status line from a session's delivery receipt.
func (m *model) applyReceipt(r sessions.ReceiptMsg) {
	name := r.Id
	for _, item := range m.items {
		if item.conv.SessionID == r.Id {
			name = m.sessionName(item.conv)
		}
	}
	if m.sendNote == nil {
		m.sendNote = make(map[string]string)
	}
	if m.held == nil {
		m.held = make(map[string]bool)
	}
	delete(m.sentAt, r.Id) // a receipt beats the guess
	switch r.Status {
	case "held":
		m.held[r.Id] = true
	case "delivered":
		delete(m.held, r.Id) // approved; the transcript shows it next
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
		delete(m.held, r.Id)
		delete(m.pending, r.Id)
		m.sendNote[r.Id] = "✗ " + name + " didn't take it: " + why
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

func peerParts(text string) (from, body string, ok bool) {
	m := transcript.PeerMessage.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	from = "another session"
	if f := transcript.PeerFrom.FindStringSubmatch(m[1]); f != nil {
		from = f[1]
	}
	return from, stripImageNote(strings.TrimPrefix(m[2], transcript.CcsNote)), true
}

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
			out[p.Idx] = shortAddr(p.Addr)
		case name != "":
			out[p.Idx] = name
		}
	}
	return out
}

// convByID indexes the listed sessions.
func (m model) convByID() map[string]transcript.Conversation {
	out := make(map[string]transcript.Conversation, len(m.items))
	for _, it := range m.items {
		out[it.conv.SessionID] = it.conv
	}
	return out
}

// shortAddr is a SendMessage address for show: "session 123" for a socket,
// an agent's id as it is.
func shortAddr(a string) string {
	if strings.HasPrefix(a, "uds:") {
		return "session " + strings.TrimSuffix(filepath.Base(a), ".sock")
	}
	return a
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
		for m.cursor > 0 && m.filtered[m.cursor].hidden { // its folder is collapsed
			m.toggleCollapse()
			m.cursor = find()
		}
		m.previewScroll = 0
		m.selectionMoved()
	} else {
		m.errorMsg = "That session isn't in the list (older than --max-age, or filtered out)"
	}
}

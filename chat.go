package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ============================================================================
// Message box: talk to the selected live session from ccs
// ============================================================================

// chatFastRefresh is how often the selected live session's transcript and
// busy/idle status are checked (one stat and one small file read).
const chatFastRefresh = 250 * time.Millisecond

type chatTickMsg struct{}

type chatTickResult struct {
	id   string
	conv *Conversation // set when the transcript grew
	stat sessionStat
	gen  int
}

type sendDoneMsg struct {
	id   string
	note string
	err  error
}

// sessionStat is what a live session's file says about it right now.
type sessionStat struct {
	status string // "busy" or "idle"
	since  time.Time
	name   string
}

// sessionFile is the part of ~/.claude/sessions/<pid>.json ccs uses.
type sessionFile struct {
	SessionID       string `json:"sessionId"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"` // epoch ms
	Socket          string `json:"messagingSocketPath"`
}

func readSessionFile(pid int) (sessionFile, error) {
	var f sessionFile
	data, err := os.ReadFile(filepath.Join(getSessionsDir(), strconv.Itoa(pid)+".json"))
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(data, &f)
}

func (m model) selectedLive() bool {
	return len(m.filtered) > 0 && m.live[m.filtered[m.cursor].conv.SessionID]
}

// selectionMoved runs when the user moves the selection (keys, wheel,
// click): landing on a live session puts typing in its message box. Never
// called for list changes caused by typing a search, so a search can't end
// up in the message box.
func (m *model) selectionMoved() {
	m.chatFocus = m.selectedLive() && !m.showUsage
}

// chatRows is the height the message area takes under the preview: a status
// line and the bordered box. Zero unless a live session is selected.
func (m model) chatRows() int {
	if m.showUsage || !m.selectedLive() {
		return 0
	}
	return 4
}

// ensureChatTick starts the fast refresh while a live session is selected.
// Each tick reschedules through Update, so it stops by itself otherwise.
func (m *model) ensureChatTick() tea.Cmd {
	if m.chatTickOn || m.quitting || m.showUsage || !m.selectedLive() {
		return nil
	}
	m.chatTickOn = true
	return tea.Tick(chatFastRefresh, func(time.Time) tea.Msg { return chatTickMsg{} })
}

// chatTickCmd reads the selected live session's status and any new
// transcript lines off the UI goroutine.
func (m model) chatTickCmd() tea.Cmd {
	if !m.selectedLive() {
		return func() tea.Msg { return chatTickResult{} }
	}
	conv, gen := m.filtered[m.cursor].conv, m.gen
	pid := m.livePIDs[conv.SessionID]
	return func() (msg tea.Msg) {
		res := chatTickResult{id: conv.SessionID, gen: gen}
		defer func() {
			if r := recover(); r != nil {
				recoverWorkerValue(r)
				msg = res
			}
		}()
		if f, err := readSessionFile(pid); err == nil && f.SessionID == conv.SessionID {
			res.stat = sessionStat{status: f.Status, since: time.UnixMilli(f.StatusUpdatedAt), name: f.Name}
		}
		if _, busy := liveReads.LoadOrStore(conv.FilePath, true); !busy {
			defer liveReads.Delete(conv.FilePath)
			if c, err := parseAppended(&conv); err == nil && c != nil && c.Size != conv.Size {
				res.conv = c
			}
		}
		return res
	}
}

// sessionName is how the status line names a session: its Claude Code name,
// else its title, else its topic.
func (m model) sessionName(conv Conversation) string {
	if st, ok := m.chatStatus[conv.SessionID]; ok && st.name != "" {
		return st.name
	}
	if conv.Title != "" {
		return truncate(conv.Title, 30)
	}
	return truncate(getTopic(conv), 30)
}

// chatView draws the status line and message box for the selected session.
func (m model) chatView() string {
	conv := m.filtered[m.cursor].conv
	id, name := conv.SessionID, m.sessionName(conv)
	left := ""
	switch {
	case m.pending[id] != "" && m.sending:
		left = "\033[90m  sending: " + chatSnippet(m.pending[id], m.width-40) + "\033[0m"
	case m.pending[id] != "" && m.held[id]:
		left = "\033[33m  ⏸ held: approve it in " + name + " · " + chatSnippet(m.pending[id], m.width-50-len(name)) + "\033[0m"
	case m.pending[id] != "" && !m.sentAt[id].IsZero() && time.Since(m.sentAt[id]) > heldTimeout:
		left = "\033[33m  ⚠ not picked up yet: " + chatSnippet(m.pending[id], m.width-90) +
			" · if it runs with bypass permissions, approve it in that session\033[0m"
	case m.pending[id] != "" && m.chatStatus[id].status == "busy":
		left = "\033[90m  sent: " + chatSnippet(m.pending[id], m.width-60) + " · Claude sees it after its current step\033[0m"
	case m.pending[id] != "":
		left = "\033[90m  sent: " + chatSnippet(m.pending[id], m.width-40) + "\033[0m"
	case m.sendNote[id] != "":
		left = "  " + m.sendNote[id]
	}
	right := ""
	if st := m.chatStatus[id]; st.status == "busy" {
		right = fmt.Sprintf("\033[33m✻ working %ds\033[0m ", int(time.Since(st.since).Seconds()))
	}
	status := left + strings.Repeat(" ", max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)) + right

	in := m.chatInput
	in.Placeholder = "message " + name + "…"
	in.Width = max(m.width-8, 10)
	if !m.chatFocus {
		in.Blur() // no cursor while typing goes to the search
	}
	border := lipgloss.Color("240")
	if m.chatFocus {
		border = lipgloss.Color("62")
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Width(max(m.width-2, 10)).Render(in.View())
	return status + "\n" + box
}

// chatSnippet is a pending message on one line, cut to width with a single "…".
func chatSnippet(s string, width int) string {
	return ansi.Truncate(strings.Join(strings.Fields(s), " "), max(width, 10), "…")
}

// sendCmd sends the message box's text to the selected live session.
func (m *model) sendCmd() tea.Cmd {
	text := strings.TrimSpace(m.chatInput.Value())
	if text == "" || m.sending || !m.selectedLive() {
		return nil
	}
	conv := m.filtered[m.cursor].conv
	id, name, pid := conv.SessionID, m.sessionName(conv), m.livePIDs[conv.SessionID]
	if m.pending == nil {
		m.pending = make(map[string]string)
	}
	m.pending[id] = text
	delete(m.sendNote, id)
	m.sending = true
	m.chatInput.SetValue("")
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				recoverWorkerValue(r)
				msg = sendDoneMsg{id: id, note: "✗ Internal error, logged to " + workerPanicLog, err: errors.New("panic")}
			}
		}()
		note, err := deliverMessage(pid, id, name, text)
		return sendDoneMsg{id: id, note: note, err: err}
	}
}

// errNotConnected means the socket couldn't be reached at all, so nothing
// was sent and the terminal fallback is safe.
var errNotConnected = errors.New("not connected")

// deliverMessage sends text to a live session: through its message socket
// if it can be reached, else typed into its terminal. Never both: once the
// socket accepted a connection, a later error isn't retried another way.
func deliverMessage(pid int, id, name, text string) (string, error) {
	f, err := readSessionFile(pid)
	if err != nil || f.SessionID != id {
		return "✗ Couldn't reach " + name + ": its session has changed", fmt.Errorf("session file: %v", err)
	}
	if f.Socket != "" {
		msgID, err := sendViaSocket(f.Socket, peerToken(pid, f.Socket), text)
		if msgID != "" {
			sentMsgs.Store(msgID, id) // so a receipt can find the session
		}
		if err == nil {
			return "✓ Sent to " + name + " via its message socket (as a message from ccs)", nil
		}
		if !errors.Is(err, errNotConnected) {
			return "✗ Sending to " + name + " may have failed: " + err.Error(), err
		}
	}
	where, err := typeIntoSession(pid, text)
	switch {
	case where == "":
		return "✗ Couldn't reach " + name + ": its message socket isn't reachable and it isn't in iTerm or tmux", errors.New("unreachable")
	case errors.Is(err, errTimedOut):
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
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errNotConnected, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	var out bytes.Buffer
	if token != "" {
		line, _ := json.Marshal(map[string]string{"type": "auth", "token": token})
		out.Write(append(line, '\n'))
	}
	body := strings.ReplaceAll(text, "</cross-session-message>", "</ cross-session-message>")
	msgID := newUUID()
	msg := map[string]any{
		"type":    "user",
		"message": map[string]string{"role": "user", "content": "<cross-session-message from-name=\"" + ccsSender + "\">\n" + ccsNote + body + "\n</cross-session-message>"},
		"msg_id":  msgID,
		"msgV":    1,
	}
	// A return address lets the session send receipts (held, refused…); it
	// only replies to one in its own socket folder.
	if inbox := receiptInbox(filepath.Dir(path)); inbox != "" {
		msg["from"] = "uds:" + inbox
	}
	line, _ := json.Marshal(msg)
	out.Write(append(line, '\n'))
	if _, err := conn.Write(out.Bytes()); err != nil {
		return msgID, err
	}
	time.Sleep(150 * time.Millisecond) // as Claude Code's own sender does before ending
	if uc, ok := conn.(*net.UnixConn); ok {
		return msgID, uc.CloseWrite()
	}
	return msgID, nil
}

// Delivery receipts. A Claude Code session sends one back, as a
// {"type":"control","action":"peer_message_status"} line, only when a
// message doesn't go straight in: held for approval (bypass-permissions
// sessions), then delivered, denied or expired; refused; or dropped. It
// connects to the "from" address, which must be a .sock in the recipient's
// own socket folder, and checks the listener is the process that sent.
var (
	inboxMu    sync.Mutex
	inboxes    = map[string]net.Listener{} // socket folder -> ccs's listener there
	sentMsgs   sync.Map                    // msg_id -> SessionID
	receiptsCh = make(chan receiptMsg, 16)
)

type receiptMsg struct {
	id, status, reason string // id is the SessionID the message went to
}

// receiptInbox returns ccs's listening socket in dir, starting it on first
// use; "" if it can't (the send then just goes without a return address).
func receiptInbox(dir string) string {
	inboxMu.Lock()
	defer inboxMu.Unlock()
	if l, ok := inboxes[dir]; ok {
		return l.Addr().String()
	}
	path := filepath.Join(dir, fmt.Sprintf("ccs-%d-inbox.sock", os.Getpid()))
	if len(path) > 100 { // unix socket paths are limited to ~104 bytes
		return ""
	}
	reapInboxes(dir)
	os.Remove(path) // our own pid's leftover, if any
	l, err := net.Listen("unix", path)
	if err != nil {
		return ""
	}
	os.Chmod(path, 0o600)
	inboxes[dir] = l
	go serveReceipts(l)
	return path
}

// reapInboxes removes inbox sockets left by ccs processes that have exited.
func reapInboxes(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "ccs-*-inbox.sock"))
	for _, m := range matches {
		var pid int
		if _, err := fmt.Sscanf(filepath.Base(m), "ccs-%d-inbox.sock", &pid); err != nil || pid == os.Getpid() {
			continue
		}
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			os.Remove(m)
		}
	}
}

func serveReceipts(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return // closed
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			sc := bufio.NewScanner(conn)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				if r, ok := parseReceipt(sc.Bytes()); ok {
					select {
					case receiptsCh <- r:
					default: // nobody's reading fast enough; the heuristic still covers it
					}
				}
			}
		}()
	}
}

// parseReceipt reads one line sent to ccs's inbox; anything but a receipt
// for a message ccs sent (an auth line, a reply, an idle notice) is ignored.
func parseReceipt(line []byte) (receiptMsg, bool) {
	var r struct {
		Type, Action, Status, Reason string
		StatusDetail                 string   `json:"status_detail"`
		OrigMsgID                    string   `json:"orig_msg_id"`
		DropReason                   string   `json:"drop_reason"`
		DroppedMsgIDs                []string `json:"dropped_msg_ids"`
	}
	if json.Unmarshal(line, &r) != nil || r.Type != "control" || r.Action != "peer_message_status" {
		return receiptMsg{}, false
	}
	if r.Status == "expired" && r.StatusDetail == "refused" {
		r.Status = "refused"
	}
	if r.Status == "dropped" && r.DropReason != "" {
		r.Reason = r.DropReason
	}
	for _, id := range append([]string{r.OrigMsgID}, r.DroppedMsgIDs...) {
		if sid, ok := sentMsgs.Load(id); ok && id != "" {
			return receiptMsg{id: sid.(string), status: r.Status, reason: r.Reason}, true
		}
	}
	return receiptMsg{}, false
}

// waitReceipt delivers the next receipt to Update; it blocks, so an idle
// screen stays still.
func waitReceipt() tea.Msg { return <-receiptsCh }

// closeInboxes stops ccs's receipt sockets and removes their files.
func closeInboxes() {
	inboxMu.Lock()
	defer inboxMu.Unlock()
	for dir, l := range inboxes {
		l.Close() // Go removes the socket file on Close
		delete(inboxes, dir)
	}
}

// peerToken reads the recipient's peer token from
// ~/.claude/sessions/<pid>.<sha256(socket path)>.key, "" if there isn't one
// (macOS sessions don't require it by default).
func peerToken(pid int, socket string) string {
	paths := []string{socket}
	if real, err := filepath.EvalSymlinks(socket); err == nil && real != socket {
		paths = append(paths, real)
	}
	for _, p := range paths {
		sum := sha256.Sum256([]byte(p))
		data, err := os.ReadFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:]))))
		if err != nil {
			continue
		}
		var k struct {
			PeerToken string `json:"peerToken"`
		}
		if json.Unmarshal(data, &k) == nil && k.PeerToken != "" {
			return k.PeerToken
		}
	}
	return ""
}

// typeIntoSession types text into the terminal running pid and presses
// Enter: its tmux pane, else its iTerm tab. where names what it used ("" if
// neither found it). A timeout may still have typed it, so it's not retried.
func typeIntoSession(pid int, text string) (where string, err error) {
	if strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f || r == utf8.RuneError }) {
		return "terminal", errors.New("the message contains control characters")
	}
	out, err := runBounded(2*time.Second, nil, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	tty := strings.TrimSpace(string(out))
	if err != nil || tty == "" || tty == "??" {
		return "", nil
	}
	tty = "/dev/" + tty
	if os.Getenv("TMUX") != "" {
		panes, err := runBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
		if target := tmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			if _, err := runBounded(5*time.Second, nil, "tmux", "send-keys", "-t", target, "-l", "--", text); err != nil {
				return "tmux pane", err
			}
			_, err := runBounded(5*time.Second, nil, "tmux", "send-keys", "-t", target, "Enter")
			return "tmux pane", err
		}
	}
	if os.Getenv("TERM_PROGRAM") != "iTerm.app" {
		return "", nil
	}
	script := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					tell s to write text %q
					return "typed"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty, text)
	out, err = runBounded(5*time.Second, nil, "osascript", "-e", script)
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
			if msgs[i].Role == "user" && strings.TrimSpace(body) == text {
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
		for _, q := range item.conv.peerQueued {
			if _, b, ok := peerParts(q); ok && strings.TrimSpace(b) == text {
				delete(m.sentAt, id)
			}
		}
	}
}

// applyReceipt updates the status line from a session's delivery receipt.
func (m *model) applyReceipt(r receiptMsg) {
	name := r.id
	for _, item := range m.items {
		if item.conv.SessionID == r.id {
			name = m.sessionName(item.conv)
		}
	}
	if m.sendNote == nil {
		m.sendNote = make(map[string]string)
	}
	if m.held == nil {
		m.held = make(map[string]bool)
	}
	delete(m.sentAt, r.id) // a receipt beats the guess
	switch r.status {
	case "held":
		m.held[r.id] = true
	case "delivered":
		delete(m.held, r.id) // approved; the transcript shows it next
	default:
		why := map[string]string{
			"denied":  "declined in that session",
			"expired": "it was held and expired without approval",
			"refused": "that session isn't accepting messages",
			"dropped": "dropped at its inbox",
		}[r.status]
		if why == "" {
			why = r.status
		}
		if r.status == "dropped" && r.reason != "" {
			why += " (" + r.reason + ")"
		}
		delete(m.held, r.id)
		delete(m.pending, r.id)
		m.sendNote[r.id] = "✗ " + name + " didn't take it: " + why
	}
}

// heldTimeout is how long a socket message may go unqueued before ccs says
// the session may be holding it.
const heldTimeout = 5 * time.Second

// Claude Code frames socket messages as coming from a peer session, so a
// recipient can take one from ccs for another Claude and try to answer it
// there. The sender name and a leading note say it's the user; peerParts
// drops the note again for the preview.
const (
	ccsSender = "the user, via ccs"
	ccsNote   = "[Typed by the user in ccs, their session browser. Not from another Claude session: reply here as you would to any message from the user; there is no session to send a reply to.]\n\n"
)

// peerMessage matches a message another session (or ccs) sent through the
// message socket; peerParts pulls out who it's from and the text.
// Claude Code may add its own paragraph after the closing tag (a note that
// peers can't grant permissions), so anything after it is allowed.
var peerMessage = regexp.MustCompile(`(?s)^(?:Another Claude session sent a message:\n)?<cross-session-message([^>]*)>\n(.*?)\n</cross-session-message>(?:\s.*)?$`)

var peerFrom = regexp.MustCompile(`from-name="([^"]*)"`)

func peerParts(text string) (from, body string, ok bool) {
	m := peerMessage.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	from = "another session"
	if f := peerFrom.FindStringSubmatch(m[1]); f != nil {
		from = f[1]
	}
	return from, strings.TrimPrefix(m[2], ccsNote), true
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

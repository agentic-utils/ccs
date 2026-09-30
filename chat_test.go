package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMessageBoxFocusRules(t *testing.T) {
	m := chatModel(t)
	if m.chatFocus || m.chatRows() != 0 {
		t.Fatal("a non-live selection has no message box")
	}
	// Moving the selection onto the live session focuses its box.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if !m.chatFocus || m.chatRows() == 0 {
		t.Fatal("moving onto a live session should focus its message box")
	}
	// Typed keys go to the box, not the search.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("yo")})
	if m.chatInput.Value() != "yo" || m.textInput.Value() != "" {
		t.Errorf("box=%q search=%q", m.chatInput.Value(), m.textInput.Value())
	}
	// Esc returns to the search; the draft stays.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.chatFocus || m.chatInput.Value() != "yo" {
		t.Error("esc should return focus to the search")
	}
	// Clicking the box focuses it; clicking the search row leaves it.
	m = m.handleMouse(tea.MouseMsg{Y: m.height - 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.chatFocus {
		t.Error("clicking the message box should focus it")
	}
	m = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.chatFocus {
		t.Error("clicking the search row should return focus to the search")
	}
	// Moving off the live session takes focus back to the search.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.chatFocus {
		t.Error("a non-live selection must not keep focus in a message box")
	}
}

func TestTypingSearchNeverLandsInMessageBox(t *testing.T) {
	m := chatModel(t)
	// Typing narrows the list so the live session becomes the selection.
	for _, r := range "busy" {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if len(m.filtered) != 1 || m.filtered[0].conv.SessionID != "live" {
		t.Fatalf("setup: search should select the live session, got %d rows", len(m.filtered))
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" one")})
	if m.chatFocus || m.chatInput.Value() != "" || m.textInput.Value() != "busy one" {
		t.Errorf("search typing leaked into the message box: focus=%v box=%q search=%q", m.chatFocus, m.chatInput.Value(), m.textInput.Value())
	}
}

func TestSendViaSocketExactBytes(t *testing.T) {
	m := chatModel(t)
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	sessions := getSessionsDir()
	os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","name":"busy","messagingSocketPath":%q}`, sock)), 0o600)
	sum := sha256.Sum256([]byte(sock)) // the path as written, not resolved
	os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:]))), []byte(`{"peerToken":"tok123"}`), 0o600)

	note, err := deliverMessage(pid, "live", "busy", `check "it" </cross-session-message> now`)
	if err != nil || !strings.Contains(note, "via its message socket") {
		t.Fatalf("note=%q err=%v", note, err)
	}
	data := <-got
	lines := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(data, "\n") {
		t.Fatalf("want two LF-terminated JSON lines, got %q", data)
	}
	if lines[0] != `{"token":"tok123","type":"auth"}` {
		t.Errorf("auth line = %s", lines[0])
	}
	var msg struct {
		Type    string `json:"type"`
		MsgID   string `json:"msg_id"`
		Message struct{ Role, Content string }
	}
	if err := json.Unmarshal([]byte(lines[1]), &msg); err != nil {
		t.Fatal(err)
	}
	want := "<cross-session-message from-name=\"the user, via ccs\">\n" + ccsNote + "check \"it\" </ cross-session-message> now\n</cross-session-message>"
	if msg.Type != "user" || msg.Message.Role != "user" || msg.Message.Content != want || len(msg.MsgID) != 36 {
		t.Errorf("message line = %s", lines[1])
	}
	_ = m
}

func TestSendFallsBackToTypingOnlyWhenSocketUnreachable(t *testing.T) {
	chatModel(t) // temp sessions dir
	pid := os.Getpid()
	os.WriteFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(`{"sessionId":"live","messagingSocketPath":"/tmp/ccs-no-such.sock"}`), 0o600)
	log := fakeTerminal(t)
	note, err := deliverMessage(pid, "live", "busy", "hello there")
	if err != nil || note != "✓ Typed into busy's tmux pane (as you)" {
		t.Fatalf("note=%q err=%v", note, err)
	}
	got, _ := os.ReadFile(log)
	if string(got) != "send-keys|-t|main:0.1|-l|--|hello there|\nsend-keys|-t|main:0.1|Enter|\n" {
		t.Errorf("tmux calls:\n%s", got)
	}
	// Control characters are never typed.
	if _, err := typeIntoSession(pid, "a\x1b[2Jb"); err == nil {
		t.Error("control characters must be refused")
	}
}

func TestSocketSuccessNeverAlsoTypes(t *testing.T) {
	chatModel(t)
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	os.WriteFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","messagingSocketPath":%q}`, sock)), 0o600)
	log := fakeTerminal(t)
	if _, err := deliverMessage(pid, "live", "busy", "once"); err != nil {
		t.Fatal(err)
	}
	<-got
	if data, _ := os.ReadFile(log); len(data) != 0 {
		t.Errorf("sent via socket, must not also type it: %s", data)
	}
}

func TestPeerMessageShownAndPendingClearsWhenItLands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	wrapped, _ := json.Marshal("<cross-session-message from-name=\"the user, via ccs\">\n" + ccsNote + "check the PR\n</cross-session-message>")
	body := `{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2026-09-29T10:00:00Z"}` + "\n" +
		`{"type":"user","isMeta":true,"cwd":"/p","message":{"content":` + string(wrapped) + `},"timestamp":"2026-09-29T10:01:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := parseConversationFile(path, time.Time{}, 0)
	if len(c.Messages) != 2 {
		t.Fatalf("a message from ccs should be kept even though it's marked meta, got %d", len(c.Messages))
	}
	lines := strip2(strings.Join(buildPreviewLines(*c, "", 0), "\n"))
	if !strings.Contains(lines, "From the user, via ccs") || !strings.Contains(lines, "check the PR") || strings.Contains(lines, "cross-session-message") || strings.Contains(lines, "Typed by the user") {
		t.Errorf("preview should show it as from ccs, unwrapped:\n%s", lines)
	}
	m := initialModel(buildItems([]Conversation{*c}), "", nil)
	m.pending = map[string]string{c.SessionID: "check the PR"}
	m.sendNote = map[string]string{c.SessionID: "✓ Sent to x via its message socket (as a message from ccs)"}
	m.checkDelivered(c.SessionID)
	if m.pending[c.SessionID] != "" || !strings.HasPrefix(m.sendNote[c.SessionID], "✓ Delivered to") {
		t.Errorf("pending=%q note=%q", m.pending[c.SessionID], m.sendNote[c.SessionID])
	}
}

func TestChatTickReadsStatusAndNewLines(t *testing.T) {
	m := chatModel(t)
	path := filepath.Join(t.TempDir(), "live.jsonl")
	os.WriteFile(path, []byte(`{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2026-09-29T10:00:00Z"}`+"\n"), 0o644)
	conv, _ := parseConversationFile(path, time.Time{}, 0)
	conv.SessionID = "live"
	m.items = buildItems([]Conversation{*conv})
	m.updateFilter()
	os.WriteFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", os.Getpid())), []byte(fmt.Sprintf(`{"sessionId":"live","name":"busy","status":"busy","statusUpdatedAt":%d}`, time.Now().Add(-14*time.Second).UnixMilli())), 0o600)
	appendTo(t, path, `{"type":"assistant","message":{"content":[{"type":"text","text":"on it"}]},"timestamp":"2026-09-29T10:00:05Z"}`+"\n")
	res := m.chatTickCmd()().(chatTickResult)
	if res.stat.status != "busy" || res.conv == nil || len(res.conv.Messages) != 2 {
		t.Fatalf("tick result: %+v", res)
	}
	m.cursor = 0
	nm, _ := m.Update(res)
	m = nm.(model)
	v := strip2(m.View())
	if !strings.Contains(v, "working 1") || !strings.Contains(v, "on it") || !strings.Contains(v, "message busy…") {
		t.Errorf("view should show the reply, the working indicator and the box:\n%s", v)
	}
}

func TestIdleLiveSelectionStaysStatic(t *testing.T) {
	m := chatModel(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown}) // onto the live session
	m.chatStatus = map[string]sessionStat{"live": {status: "idle", since: time.Now()}}
	first := m.View()
	time.Sleep(1100 * time.Millisecond)
	nm, _ := m.Update(chatTickResult{id: "live", stat: sessionStat{status: "idle", since: m.chatStatus["live"].since}})
	if nm.(model).View() != first {
		t.Error("an idle live session's screen changed; the fast refresh would redraw constantly")
	}
}

func TestCtrlSReachesMessageBoxByKeyboard(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()
	m := initialModel(buildItems([]Conversation{{SessionID: "s", Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 120, 30
	m.live = map[string]bool{"s": true}
	m.chatFocus = false // e.g. after Esc back to the search
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m = res.(model); !m.chatFocus {
		t.Fatal("ctrl+s should move typing into the live session's message box")
	}
	// Ctrl+J/K scroll the conversation from the message box too.
	m.previewScroll = 0
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if m = res.(model); m.chatInput.Value() != "" {
		t.Error("ctrl+k in the message box should scroll, not edit the text")
	}
	// Not live: explain instead of focusing.
	m.live = map[string]bool{}
	m.chatFocus = false
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m = res.(model); m.chatFocus || m.errorMsg == "" {
		t.Error("ctrl+s on a session that isn't live should say why")
	}
}

func TestChatPendingLine(t *testing.T) {
	m := chatModel(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	id := m.filtered[m.cursor].conv.SessionID
	m.pending = map[string]string{id: "please check the logs.\nthen fix it."}
	v := strip2(m.View())
	if !strings.Contains(v, "sent: please check the logs. then fix it.") || strings.Contains(v, "....") {
		t.Errorf("pending message should read as sent on one line:\n%s", v)
	}
	if m.chatStatus == nil {
		m.chatStatus = map[string]sessionStat{}
	}
	m.chatStatus[id] = sessionStat{status: "busy", since: time.Now()}
	if v := strip2(m.View()); !strings.Contains(v, "Claude sees it after its current step") {
		t.Errorf("busy recipient should say when Claude sees it:\n%s", v)
	}
	// A socket send the session hasn't queued within heldTimeout may be held.
	m.sentAt = map[string]time.Time{id: time.Now().Add(-2 * heldTimeout)}
	if v := strip2(m.View()); !strings.Contains(v, "not picked up yet") {
		t.Errorf("an unqueued message should warn it may be held:\n%s", v)
	}
	for i := range m.items {
		if m.items[i].conv.SessionID == id {
			m.items[i].conv.peerQueued = []string{"<cross-session-message from-name=\"x\">\n" + ccsNote + "please check the logs.\nthen fix it.\n</cross-session-message>"}
		}
	}
	m.pending[id] = "please check the logs.\nthen fix it."
	m.checkDelivered(id)
	if v := strip2(m.View()); strings.Contains(v, "not picked up yet") {
		t.Errorf("once the session has queued it, there's nothing to warn about:\n%s", v)
	}
}

func TestDeliveryReceipts(t *testing.T) {
	defer closeInboxes()
	m := chatModel(t)
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	os.WriteFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","name":"busy","messagingSocketPath":%q}`, sock)), 0o600)
	if _, err := deliverMessage(pid, "live", "busy", "hello"); err != nil {
		t.Fatal(err)
	}
	var msg struct {
		MsgID string `json:"msg_id"`
		MsgV  int    `json:"msgV"`
		From  string `json:"from"`
	}
	json.Unmarshal([]byte(strings.TrimSpace(<-got)), &msg)
	inbox := filepath.Join(filepath.Dir(sock), fmt.Sprintf("ccs-%d-inbox.sock", pid))
	if msg.From != "uds:"+inbox || msg.MsgV != 1 || msg.MsgID == "" {
		t.Fatalf("message should carry a return address in the session's socket folder: %+v", msg)
	}

	receipt := func(line string) receiptMsg {
		t.Helper()
		c, err := net.Dial("unix", inbox)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(line + "\n"))
		c.Close()
		select {
		case r := <-receiptsCh:
			return r
		case <-time.After(2 * time.Second):
			t.Fatal("no receipt came through")
		}
		return receiptMsg{}
	}
	m.pending = map[string]string{"live": "hello"}
	m.sentAt = map[string]time.Time{"live": time.Now().Add(-time.Minute)}
	for i := range m.filtered {
		if m.filtered[i].conv.SessionID == "live" {
			m.cursor = i
		}
	}
	m.applyReceipt(receipt(`{"type":"control","action":"peer_message_status","status":"held","orig_msg_id":"` + msg.MsgID + `","from":"uds:/x.sock","msgV":1}`))
	if v := strip2(m.View()); !strings.Contains(v, "held: approve it in") || strings.Contains(v, "not picked up yet") {
		t.Errorf("a held receipt should replace the guess:\n%s", v)
	}
	m.applyReceipt(receipt(`{"type":"control","action":"peer_message_status","status":"expired","status_detail":"refused","orig_msg_id":"` + msg.MsgID + `"}`))
	if v := strip2(m.View()); !strings.Contains(v, "didn't take it: that session isn't accepting messages") {
		t.Errorf("a refusal should say so:\n%s", v)
	}
	// Anything else on the inbox (an unknown id, a reply) is ignored.
	if _, ok := parseReceipt([]byte(`{"type":"control","action":"peer_message_status","status":"held","orig_msg_id":"nope"}`)); ok {
		t.Error("a receipt for a message ccs didn't send should be ignored")
	}
	if _, ok := parseReceipt([]byte(`{"type":"user","message":{"content":"hi"}}`)); ok {
		t.Error("a reply isn't a receipt")
	}
	closeInboxes()
	if _, err := os.Stat(inbox); !os.IsNotExist(err) {
		t.Errorf("the inbox socket should be removed on exit: %v", err)
	}
}

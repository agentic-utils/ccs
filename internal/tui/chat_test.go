package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestMessageBoxFocusRules(t *testing.T) {
	m := chatModel(t)
	if m.chatFocus || m.chatRows() != 0 {
		t.Fatal("a non-live selection has no message box")
	}
	// Moving onto the live session shows its box and puts typing in it.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	if !m.chatFocus || m.chatRows() == 0 {
		t.Fatal("moving onto a live session should show its box and focus it")
	}
	// After Esc back to the search, Enter focuses it again.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.chatFocus {
		t.Fatal("esc should leave the message box for the search")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.chatFocus {
		t.Fatal("enter on a live session should focus its message box")
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
	m = m.handleMouse(tea.MouseMsg{X: m.width - 5, Y: m.height - 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.chatFocus {
		t.Error("clicking the message box should focus it")
	}
	m = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.chatFocus {
		t.Error("clicking the search row should return focus to the search")
	}
	// Moving off the live session takes focus back to the search.
	m = moveTo(t, m, "live")
	m = moveTo(t, m, "idle")
	if m.chatFocus {
		t.Error("a non-live selection must not keep focus in a message box")
	}
}

func TestQuestionMarkHelpOnlyOnAnEmptyMessageBox(t *testing.T) {
	m := chatModel(t)
	q := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown}) // onto the live session: its box has focus
	if !m.chatFocus {
		t.Fatal("setup: the live session's message box should have focus")
	}
	if head := strip2(strings.Split(m.View(), "\n")[0]); !strings.HasSuffix(head, "? help") {
		t.Errorf("the message box's header keys should end with ? help: %q", head)
	}
	if m, _ = key(m, q); !m.helpOpen || m.chatInput.Value() != "" {
		t.Fatalf("? on an empty message box opens help: help=%v box=%q", m.helpOpen, m.chatInput.Value())
	}
	if m, _ = key(m, q); m.helpOpen || !m.chatFocus {
		t.Fatalf("? again closes it, leaving the box focused: help=%v focus=%v", m.helpOpen, m.chatFocus)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("why")})
	if m, _ = key(m, q); m.helpOpen || m.chatInput.Value() != "why?" || m.textInput.Value() != "" {
		t.Errorf("after text, ? types into the box: help=%v box=%q search=%q", m.helpOpen, m.chatInput.Value(), m.textInput.Value())
	}
	// A search typed doesn't matter while the box has focus: its emptiness decides.
	m.chatInput.SetValue("")
	m.textInput.SetValue("busy")
	if m, _ = key(m, q); !m.helpOpen || m.textInput.Value() != "busy" {
		t.Errorf("? on an empty box opens help whatever the search holds: help=%v search=%q", m.helpOpen, m.textInput.Value())
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

// twoLive is chatModel with every session live, so the selection can move
// from one message box to another.
func twoLive(t *testing.T) model {
	m := chatModel(t)
	m.live["idle"] = true
	m.livePIDs["idle"] = os.Getpid()
	return m
}

// moveTo moves the selection onto session id with the arrow keys, as the user
// would, stepping off it first if it's already selected.
func moveTo(t *testing.T, m model, id string) model {
	t.Helper()
	want := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id })
	if want < 0 || len(m.filtered) < 2 {
		t.Fatalf("%q isn't listed with another session to move from", id)
	}
	if m.cursor == want {
		dir := tea.KeyDown
		if want == len(m.filtered)-1 {
			dir = tea.KeyUp
		}
		m, _ = key(m, tea.KeyMsg{Type: dir})
	}
	for range len(m.filtered) {
		if m.selectedSession() == id {
			return m
		}
		dir := tea.KeyDown
		if m.cursor > want {
			dir = tea.KeyUp
		}
		m, _ = key(m, tea.KeyMsg{Type: dir})
	}
	if m.selectedSession() != id {
		t.Fatalf("couldn't reach %q, at %q", id, m.selectedSession())
	}
	return m
}

func TestScrollingKeepsTypingInTheMessageBox(t *testing.T) {
	m := twoLive(t)
	listTop, _, _ := m.listLayout()
	moves := []struct {
		name string
		move func(model) model
	}{
		{"down", func(m model) model { m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown}); return m }},
		{"up", func(m model) model { m, _ = key(m, tea.KeyMsg{Type: tea.KeyUp}); return m }},
		{"ctrl+n", func(m model) model { m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN}); return m }},
		{"ctrl+p", func(m model) model { m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlP}); return m }},
		{"wheel down", func(m model) model {
			res, _ := m.Update(tea.MouseMsg{X: 2, Y: listTop + 1, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
			return res.(model)
		}},
		{"wheel up", func(m model) model {
			res, _ := m.Update(tea.MouseMsg{X: 2, Y: listTop + 1, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
			return res.(model)
		}},
	}
	for _, mv := range moves {
		before := m.selectedSession()
		m = mv.move(m)
		if m.selectedSession() == before {
			t.Fatalf("%s: setup, the selection didn't move from %q", mv.name, before)
		}
		if !m.chatFocus {
			t.Fatalf("%s onto live %q left typing in the search", mv.name, m.selectedSession())
		}
		// Page keys scroll the conversation and keep the box focused.
		for _, k := range []tea.KeyType{tea.KeyPgUp, tea.KeyPgDown} {
			if m, _ = key(m, tea.KeyMsg{Type: k}); !m.chatFocus {
				t.Fatalf("%s then %v: the message box lost focus", mv.name, k)
			}
		}
	}
}

func TestTypingAfterScrollingGoesToTheSelectedSession(t *testing.T) {
	m := twoLive(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello there")})
	if m.chatInput.Value() != "hello there" || m.textInput.Value() != "" {
		t.Fatalf("box=%q search=%q", m.chatInput.Value(), m.textInput.Value())
	}
	// Moving on keeps that draft with its session and types into the next one.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyUp})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.selectedSession() != "idle" || m.chatInput.Value() != "x" || m.chatDrafts["live"] != "hello there" || m.textInput.Value() != "" {
		t.Errorf("sel=%q box=%q drafts=%v search=%q", m.selectedSession(), m.chatInput.Value(), m.chatDrafts, m.textInput.Value())
	}
}

func TestSearchTakesTypingOnlyWhenAsked(t *testing.T) {
	m := twoLive(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	// Esc in the box asks for the search: typing filters again.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("busy")})
	if m.chatFocus || m.textInput.Value() != "busy" || len(m.filtered) != 1 {
		t.Fatalf("esc then typing should search: focus=%v search=%q rows=%d", m.chatFocus, m.textInput.Value(), len(m.filtered))
	}
	// So does a click on the search row.
	m.textInput.SetValue("")
	m.updateFilter()
	m = moveTo(t, m, "live")
	if !m.chatFocus {
		t.Fatal("setup: moving onto a live session focuses its box")
	}
	m = m.handleMouse(tea.MouseMsg{Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.chatFocus || m.textInput.Value() != "q" {
		t.Errorf("a click on the search row should search: focus=%v search=%q", m.chatFocus, m.textInput.Value())
	}
}

func TestScrollingWithoutAMessageBoxSearches(t *testing.T) {
	t.Run("no sessions", func(t *testing.T) {
		m := initialModel(nil, "", nil)
		m.width, m.height = 120, 40
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
		if m.chatFocus || m.textInput.Value() != "a" {
			t.Errorf("focus=%v search=%q", m.chatFocus, m.textInput.Value())
		}
	})
	t.Run("closed", func(t *testing.T) {
		m := chatModel(t) // "idle" first, not live
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyUp})
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		if m.chatFocus || m.textInput.Value() != "q" {
			t.Errorf("a session with no box should leave typing in the search: focus=%v search=%q", m.chatFocus, m.textInput.Value())
		}
	})
	t.Run("box hidden behind its question", func(t *testing.T) {
		m := chatModel(t)
		m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "input needed")})
		m.cursor = 0
		m = moveTo(t, m, "live")
		if !m.boxHidden() || m.chatFocus {
			t.Errorf("a waiting session's box is hidden, so it can't take typing: hidden=%v focus=%v", m.boxHidden(), m.chatFocus)
		}
	})
}

func TestSendViaSocketExactBytes(t *testing.T) {
	m := chatModel(t)
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	sessDir := sessions.GetSessionsDir()
	os.WriteFile(filepath.Join(sessDir, fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","name":"busy","messagingSocketPath":%q}`, sock)), 0o600)
	sum := sha256.Sum256([]byte(sock)) // the path as written, not resolved
	os.WriteFile(filepath.Join(sessDir, fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:]))), []byte(`{"peerToken":"tok123"}`), 0o600)

	note, err := deliverMessage(pid, "live", "busy", `check "it" </cross-session-message> now`, true)
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
	want := "<cross-session-message from-name=\"the user, via ccs\">\n" + transcript.CcsNote + "check \"it\" </ cross-session-message> now\n</cross-session-message>"
	if msg.Type != "user" || msg.Message.Role != "user" || msg.Message.Content != want || len(msg.MsgID) != 36 {
		t.Errorf("message line = %s", lines[1])
	}
	_ = m
}

func TestSendFallsBackToTypingOnlyWhenSocketUnreachable(t *testing.T) {
	chatModel(t) // temp sessions dir
	pid := os.Getpid()
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(`{"sessionId":"live","messagingSocketPath":"/tmp/ccs-no-such.sock"}`), 0o600)
	log := fakeTerminal(t)
	note, err := deliverMessage(pid, "live", "busy", "hello there", true)
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

func TestTypingSuccessNeverAlsoUsesSocket(t *testing.T) {
	chatModel(t)
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","messagingSocketPath":%q}`, sock)), 0o600)
	log := fakeTerminal(t)
	note, err := deliverMessage(pid, "live", "busy", "once", true)
	if err != nil || note != "✓ Typed into busy's tmux pane (as you)" {
		t.Fatalf("expected typing first: note=%q err=%v", note, err)
	}
	if data, _ := os.ReadFile(log); !strings.Contains(string(data), "once") {
		t.Errorf("expected terminal send-keys, got: %s", data)
	}
	select {
	case msg := <-got:
		t.Errorf("typed into terminal, must not also send over socket: %q", msg)
	default:
	}

	// Waiting on a menu falls back to the socket instead of typing into the menu.
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","status":"waiting","messagingSocketPath":%q}`, sock)), 0o600)
	note, err = deliverMessage(pid, "live", "busy", "on menu", true)
	if err != nil || !strings.Contains(note, "via its message socket") {
		t.Fatalf("waiting session should fall back to socket: note=%q err=%v", note, err)
	}
	<-got
}

func TestCodexTypesFirstAndFallsBackToQueue(t *testing.T) {
	pid := os.Getpid()
	queued := ""
	old := sessions.CodexQueue
	sessions.CodexQueue = func(_, text string) error { queued = text; return nil }
	t.Cleanup(func() { sessions.CodexQueue = old })

	// With a terminal: types directly, never queues.
	log := fakeTerminal(t)
	note, err := sessions.DeliverCodexMessage(pid, "cx-1", "flux", "hello codex", true)
	if err != nil || note != "✓ Typed into flux's tmux pane (as you)" {
		t.Fatalf("expected typing first for codex: note=%q err=%v", note, err)
	}
	if queued != "" {
		t.Errorf("typed into terminal, must not also queue: %q", queued)
	}
	if data, _ := os.ReadFile(log); !strings.Contains(string(data), "hello codex") {
		t.Errorf("expected tmux send-keys, got: %s", data)
	}

	// Without a terminal (unverified or no tty): falls back to codex queue.
	note, err = sessions.DeliverCodexMessage(pid, "cx-1", "flux", "via queue", false)
	if err != nil || !strings.Contains(note, "codex queue") || queued != "via queue" {
		t.Fatalf("fallback to codex queue: note=%q err=%v queued=%q", note, err, queued)
	}
}

func TestPeerMessageShownAndPendingClearsWhenItLands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	wrapped, _ := json.Marshal("<cross-session-message from-name=\"the user, via ccs\">\n" + transcript.CcsNote + "check the PR\n</cross-session-message>")
	body := `{"type":"user","cwd":"/p","message":{"content":"hi"},"timestamp":"2026-09-29T10:00:00Z"}` + "\n" +
		`{"type":"user","isMeta":true,"cwd":"/p","message":{"content":` + string(wrapped) + `},"timestamp":"2026-09-29T10:01:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := transcript.ParseConversationFile(path, time.Time{}, 0)
	if len(c.Messages) != 2 {
		t.Fatalf("a message from ccs should be kept even though it's marked meta, got %d", len(c.Messages))
	}
	lines := strip2(strings.Join(buildPreviewLines(*c, "", 0), "\n"))
	if !strings.Contains(lines, "From the user, via ccs") || !strings.Contains(lines, "check the PR") || strings.Contains(lines, "cross-session-message") || strings.Contains(lines, "Typed by the user") {
		t.Errorf("preview should show it as from ccs, unwrapped:\n%s", lines)
	}
	m := initialModel(buildItems([]transcript.Conversation{*c}), "", nil)
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
	conv, _ := transcript.ParseConversationFile(path, time.Time{}, 0)
	conv.SessionID = "live"
	m.items = buildItems([]transcript.Conversation{*conv})
	m.updateFilter()
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", os.Getpid())), []byte(fmt.Sprintf(`{"sessionId":"live","name":"busy","status":"busy","statusUpdatedAt":%d}`, time.Now().Add(-14*time.Second).UnixMilli())), 0o600)
	appendTo(t, path, `{"type":"assistant","message":{"content":[{"type":"text","text":"on it"}]},"timestamp":"2026-09-29T10:00:05Z"}`+"\n")
	res := m.chatTickCmd()().(chatTickResult)
	if res.stat.Status != "busy" || res.conv == nil || len(res.conv.Messages) != 2 {
		t.Fatalf("tick result: %+v", res)
	}
	m.cursor = 0
	nm, _ := m.Update(res)
	m = nm.(model)
	v := strip2(m.View())
	if !strings.Contains(v, " Working… (1") || !strings.Contains(v, "on it") || !strings.Contains(v, "message busy…") {
		t.Errorf("view should show the reply, the working indicator and the box:\n%s", v)
	}
}

func TestIdleLiveSelectionStaysStatic(t *testing.T) {
	m := chatModel(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown}) // onto the live session
	m.chatStatus = map[string]sessions.SessionStat{"live": {Status: "idle", Since: time.Now()}}
	m.sortList() // as the tick that set the status would
	first := m.View()
	time.Sleep(1100 * time.Millisecond)
	nm, _ := m.Update(chatTickResult{id: "live", stat: sessions.SessionStat{Status: "idle", Since: m.chatStatus["live"].Since}})
	if nm.(model).View() != first {
		t.Error("an idle live session's screen changed; the fast refresh would redraw constantly")
	}
}

// enterReady is chatModel on its live session with typing back in the search,
// as after Esc or at startup: where Enter used to go straight to its tab.
func enterReady(t *testing.T) model {
	t.Helper()
	m := moveTo(t, chatModel(t), "live")
	m.chatFocus = false
	return m
}

func TestEnterThenEnterOnAnEmptyBoxGoesToItsTab(t *testing.T) {
	m := enterReady(t)
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.chatFocus || m.resuming {
		t.Fatalf("the first enter should only focus the message box (focus %v)", m.chatFocus)
	}
	if !strings.Contains(strip2(m.View()), "enter its tab") {
		t.Error("the header should say enter on the empty box goes to its tab")
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.chatFocus || cmd == nil || !m.resuming {
		t.Error("enter on the empty box should go to the session's tab")
	}
}

func TestEnterWithTextSendsAndStaysInTheBox(t *testing.T) {
	m := enterReady(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi there")})
	if !strings.Contains(strip2(m.View()), "enter send") {
		t.Error("with text in the box the header should say enter sends")
	}
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !m.sending || m.resuming || !m.chatFocus {
		t.Errorf("enter with text should send and stay in the box (sending %v, resuming %v, focus %v)", m.sending, m.resuming, m.chatFocus)
	}
	if m.chatInput.Value() != "" {
		t.Errorf("the box should empty once sent, holds %q", m.chatInput.Value())
	}
}

func TestEscLeavesTheBoxThatEnterFocused(t *testing.T) {
	m := enterReady(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.chatFocus || cmd != nil {
		t.Error("esc should leave the message box for the search")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.textInput.Value() != "x" || m.chatInput.Value() != "" {
		t.Errorf("typing after esc goes to the search: search %q box %q", m.textInput.Value(), m.chatInput.Value())
	}
}

func TestEnterWithoutAMessageBoxGoesStraightOn(t *testing.T) {
	// Waiting on a menu: the box is hidden behind its question, so Enter opens the tab.
	m := enterReady(t)
	m.setStat("live", sessions.SessionStat{Status: "waiting", Since: time.Now()})
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.chatFocus || cmd == nil || !m.resuming {
		t.Error("enter on a session waiting on a menu should go to its tab")
	}
	// Not live: Enter resumes it, as before.
	m = moveTo(t, chatModel(t), "idle")
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.chatFocus || cmd == nil || !m.resuming {
		t.Error("enter on a session that isn't live should resume it")
	}
}

func TestEnterReachesMessageBoxByKeyboard(t *testing.T) {
	dir := t.TempDir()
	old := sessions.GetSessionsDir
	sessions.GetSessionsDir = func() string { return dir }
	defer func() { sessions.GetSessionsDir = old }()
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "s", Messages: []transcript.Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 120, 30
	m.live = map[string]bool{"s": true}
	m.chatFocus = false // e.g. after Esc back to the search
	// ^S was the old way in; Enter is the only one now, so ^S does nothing.
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m = res.(model); m.chatFocus || m.textInput.Value() != "" {
		t.Fatal("ctrl+s should do nothing")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); !m.chatFocus {
		t.Fatal("enter should move typing into the live session's message box")
	}
	// Ctrl+J/K scroll the conversation from the message box too.
	m.previewScroll = 0
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if m = res.(model); m.chatInput.Value() != "" {
		t.Error("ctrl+k in the message box should scroll, not edit the text")
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
		m.chatStatus = map[string]sessions.SessionStat{}
	}
	m.chatStatus[id] = sessions.SessionStat{Status: "busy", Since: time.Now()}
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
			m.items[i].conv.Queued = []string{"<cross-session-message from-name=\"x\">\n" + transcript.CcsNote + "please check the logs.\nthen fix it.\n</cross-session-message>"}
		}
	}
	m.pending[id] = "please check the logs.\nthen fix it."
	m.checkDelivered(id)
	if v := strip2(m.View()); strings.Contains(v, "not picked up yet") {
		t.Errorf("once the session has queued it, there's nothing to warn about:\n%s", v)
	}
}

// A typed message gives no receipt: ccs watches the transcript, says plainly
// when it hasn't arrived (sitting unsent in the prompt), and counts a plain
// enqueue (typed while busy) or a user line as arrival.
func TestTypedMessageDelivery(t *testing.T) {
	m := chatModel(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	id := m.filtered[m.cursor].conv.SessionID
	text := "a message long enough that one burst with its Enter reads as a paste"
	m.pending = map[string]string{id: text}
	mm, _ := m.Update(sendDoneMsg{id: id, note: "✓ Typed into x's iTerm tab (as you)"})
	m = mm.(model)
	if !m.typed[id] || m.sentAt[id].IsZero() {
		t.Fatalf("a typed send should be tracked until it shows up: typed=%v sentAt=%v", m.typed[id], m.sentAt[id])
	}
	m.sentAt[id] = time.Now().Add(-2 * heldTimeout)
	if v := strip2(m.View()); !strings.Contains(v, "not arrived") || strings.Contains(v, "not picked up yet") {
		t.Errorf("a typed message missing from the transcript should say it may sit in the prompt:\n%s", v)
	}
	idx := -1
	for i := range m.items {
		if m.items[i].conv.SessionID == id {
			idx = i
		}
	}
	m.items[idx].conv.Queued = []string{text}
	m.checkDelivered(id)
	if v := strip2(m.View()); strings.Contains(v, "not arrived") {
		t.Errorf("queued while busy is arrived:\n%s", v)
	}
	m.items[idx].conv.Messages = append(m.items[idx].conv.Messages, transcript.Message{Role: "user", Text: text})
	m.checkDelivered(id)
	if m.pending[id] != "" || m.typed[id] || !strings.Contains(m.sendNote[id], "Delivered") {
		t.Errorf("a user line should settle it as delivered: pending=%q note=%q", m.pending[id], m.sendNote[id])
	}
	// A socket send keeps its own wording.
	m.pending[id] = "other"
	mm, _ = m.Update(sendDoneMsg{id: id, note: "✓ Sent to x via its message socket (as a message from ccs)"})
	m = mm.(model)
	if m.typed[id] {
		t.Error("a socket send isn't typed")
	}
}

// A typed message that reaches the transcript between fast ticks, or while
// the fast tick isn't following its session, still counts as delivered once
// the live tick reads it: no "not arrived" for a message in the conversation.
func TestTypedMessageDeliveredByLiveTick(t *testing.T) {
	m := chatModel(t)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	id := m.filtered[m.cursor].conv.SessionID
	text := "is the made-up widget report finished yet?"
	m.pending = map[string]string{id: text}
	mm, _ := m.Update(sendDoneMsg{id: id, note: "✓ Typed into x's iTerm tab (as you)"})
	m = mm.(model)
	m.sentAt[id] = time.Now().Add(-2 * heldTimeout)
	var conv transcript.Conversation
	for _, item := range m.items {
		if item.conv.SessionID == id {
			conv = item.conv
		}
	}
	conv.Messages = append(append([]transcript.Message(nil), conv.Messages...), transcript.Message{Role: "user", Text: text})
	conv.ReadAt = time.Now()
	mm, _ = m.Update(liveMsg{live: map[string]bool{id: true}, pids: map[string]int{id: os.Getpid()}, updated: []transcript.Conversation{conv}, gen: m.gen, tickGen: m.liveGen + 1})
	m = mm.(model)
	if m.pending[id] != "" || !strings.Contains(m.sendNote[id], "Delivered") {
		t.Errorf("a typed message the live tick read as a user line is delivered: pending=%q note=%q", m.pending[id], m.sendNote[id])
	}
	if v := strip2(m.View()); strings.Contains(v, "not arrived") {
		t.Errorf("a message in the conversation must not say it hasn't arrived:\n%s", v)
	}
	// A truly unsent one still warns.
	m.pending[id], m.typed[id], m.sentAt[id] = "a made-up line nobody submitted", true, time.Now().Add(-2*heldTimeout)
	mm, _ = m.Update(liveMsg{live: map[string]bool{id: true}, pids: map[string]int{id: os.Getpid()}, gen: m.gen, tickGen: m.liveGen + 1})
	m = mm.(model)
	if v := strip2(m.View()); !strings.Contains(v, "not arrived") {
		t.Errorf("a typed message missing from the transcript should still warn:\n%s", v)
	}
}

// The fast tick's chain survives a tick that fires with no live session
// selected: its empty result still carries the chain's generation, so
// Update clears chatTickOn and starts the chain again on a live session.
func TestChatTickSurvivesNoTarget(t *testing.T) {
	m := chatModel(t)
	m.chatGen = 3 // a wake has moved it on
	m.chatTickOn = true
	res, ok := m.chatTickCmd()().(chatTickResult)
	if !ok || res.tickGen != 3 {
		t.Fatalf("an empty fast-tick result must carry its chain's generation: %+v", res)
	}
	mm, _ := m.Update(res)
	m = mm.(model)
	if m.chatTickOn {
		t.Fatal("the chain stopped for good: chatTickOn stayed set with no tick scheduled")
	}
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyDown}) // onto the live session
	if !m.chatTickOn || cmd == nil {
		t.Error("selecting a live session should start the fast tick again")
	}
}

func TestDeliveryReceipts(t *testing.T) {
	defer sessions.CloseInboxes()
	m := chatModel(t)
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"live","name":"busy","messagingSocketPath":%q}`, sock)), 0o600)
	if _, err := deliverMessage(pid, "live", "busy", "hello", true); err != nil {
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

	receipt := func(line string) sessions.ReceiptMsg {
		t.Helper()
		c, err := net.Dial("unix", inbox)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(line + "\n"))
		c.Close()
		select {
		case r := <-sessions.ReceiptsCh:
			return r
		case <-time.After(2 * time.Second):
			t.Fatal("no receipt came through")
		}
		return sessions.ReceiptMsg{}
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
	if _, ok := sessions.ParseReceipt([]byte(`{"type":"control","action":"peer_message_status","status":"held","orig_msg_id":"nope"}`)); ok {
		t.Error("a receipt for a message ccs didn't send should be ignored")
	}
	if _, ok := sessions.ParseReceipt([]byte(`{"type":"user","message":{"content":"hi"}}`)); ok {
		t.Error("a reply isn't a receipt")
	}
	sessions.CloseInboxes()
	if _, err := os.Stat(inbox); !os.IsNotExist(err) {
		t.Errorf("the inbox socket should be removed on exit: %v", err)
	}
}

func TestPeerMessagesLinkBothEnds(t *testing.T) {
	a := transcript.Conversation{SessionID: "a", Title: "sender", Cwd: "/p"}
	for _, l := range []string{
		`{"type":"user","timestamp":"2026-10-30T10:00:00Z","message":{"role":"user","content":"start"}}`,
		`{"type":"assistant","timestamp":"2026-10-30T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Telling the other one."},{"type":"tool_use","id":"t1","name":"SendMessage","input":{"to":"uds:/tmp/cc-socks/4242.sock","summary":"rebase first","message":"PR 99 is changing; rebase before touching icons."}}]}}`,
	} {
		transcript.ParseLine(&a, []byte(l))
	}
	transcript.FinishParse(&a)
	b := transcript.Conversation{SessionID: "b", Title: "recipient", Cwd: "/p"}
	for _, l := range []string{
		`{"type":"user","timestamp":"2026-10-30T09:00:00Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-10-30T10:01:05Z","message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message from=\"uds:/tmp/cc-socks/1111.sock\" from-name=\"sender\">\nPR 99 is changing; rebase before touching icons.\n</cross-session-message>"}}`,
	} {
		transcript.ParseLine(&b, []byte(l))
	}
	transcript.FinishParse(&b)
	if n := len(a.Messages); n != 3 || a.Messages[2].Role != "sent" || a.Messages[2].To != "uds:/tmp/cc-socks/4242.sock" {
		t.Fatalf("SendMessage is a sent message after the text: %+v", a.Messages)
	}
	m := initialModel(buildItems([]transcript.Conversation{a, b}), "", nil)
	m.width, m.height = 160, 40
	m.selectSession("a")
	v := strip2(m.View())
	if !strings.Contains(v, "To recipient ✓ arrived · rebase first") || !strings.Contains(v, "PR 99 is changing") {
		t.Errorf("the sender sees who it went to and that it arrived:\n%s", v)
	}
	m.selectSession("b")
	if v := strip2(m.View()); !strings.Contains(v, "From sender") {
		t.Errorf("the recipient sees the sending session:\n%s", v)
	}
	m.selectSession("a")
	if links := m.linksWithPRs(); len(links) != 0 {
		t.Errorf("^O lists no session it messaged or heard from: %+v", links)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if v := strip2(m.View()); strings.Contains(v, "→ to") || strings.Contains(v, "← from") {
		t.Fatalf("^O lists no session rows:\n%s", v)
	}
	// Not arrived yet: named by its socket while live, no tick.
	b2 := b
	b2.Messages, b2.Peers = b.Messages[:1], nil
	m = initialModel(buildItems([]transcript.Conversation{a, b2}), "", nil)
	m.width, m.height = 160, 40
	m.livePIDs = map[string]int{"b": 4242}
	m.selectSession("a")
	if v := strip2(m.View()); !strings.Contains(v, "To recipient · rebase first") || strings.Contains(v, "arrived") {
		t.Errorf("not in its transcript yet: named by its socket, not arrived:\n%s", v)
	}
}

// The preview's peer labels walk every session, so scrolling and redrawing
// must reuse them; they're worked out again only when a session changes.
func TestPeerLabelsNotRebuiltPerFrameOrScroll(t *testing.T) {
	a := transcript.Conversation{SessionID: "a", Title: "sender", Cwd: "/p"}
	lines := []string{`{"type":"user","timestamp":"2026-10-30T10:00:00Z","message":{"role":"user","content":"start"}}`}
	for i := 0; i < 60; i++ { // enough to scroll
		lines = append(lines, fmt.Sprintf(`{"type":"assistant","timestamp":"2026-10-30T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"step %d"},{"type":"tool_use","id":"t%d","name":"SendMessage","input":{"to":"uds:/tmp/cc-socks/4242.sock","summary":"note %d","message":"message number %d"}}]}}`, i, i, i, i))
	}
	for _, l := range lines {
		transcript.ParseLine(&a, []byte(l))
	}
	transcript.FinishParse(&a)
	b := transcript.Conversation{SessionID: "b", Title: "recipient", Cwd: "/p", Messages: []transcript.Message{{Role: "user", Text: "hi"}}}
	m := initialModel(buildItems([]transcript.Conversation{a, b}), "", nil)
	m.width, m.height = 160, 30
	m.livePIDs = map[string]int{"b": 4242}
	m.selectSession("a")
	if v := strip2(m.View()); !strings.Contains(v, "To recipient · note 59") {
		t.Fatalf("the label is drawn:\n%s", v)
	}
	for i := 0; i < 20; i++ {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyPgUp})
		m.View()
		res, _ := m.Update(tea.MouseMsg{X: 100, Y: 20, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
		m = res.(model)
		m.View()
	}
	if m.previewScroll == 0 {
		t.Fatal("PgUp should have scrolled the preview")
	}
	if n := m.peerNotes.builds; n != 1 {
		t.Errorf("peer labels built %d times over 40 scroll steps and frames, want 1", n)
	}
	// A change in another session is seen: renamed, the label follows.
	for i := range m.items {
		if m.items[i].conv.SessionID == "b" {
			m.items[i].conv.Title = "renamed"
		}
	}
	m.previewScroll = 0
	if v := strip2(m.View()); !strings.Contains(v, "To renamed · note 59") {
		t.Errorf("a renamed recipient is relabelled:\n%s", v)
	}
	if n := m.peerNotes.builds; n != 2 {
		t.Errorf("built %d times after the rename, want 2", n)
	}
	// A message that isn't between sessions changes no label: not worked out again.
	for i := range m.items {
		if m.items[i].conv.SessionID == "b" {
			m.items[i].conv.Messages = append(m.items[i].conv.Messages, transcript.Message{Role: "assistant", Text: "busy"})
		}
	}
	m.View()
	if n := m.peerNotes.builds; n != 2 {
		t.Errorf("built %d times after a plain message elsewhere, want 2", n)
	}
	// An untitled recipient is named by its first prompt, which the key follows.
	for i := range m.items {
		if m.items[i].conv.SessionID == "b" {
			m.items[i].conv.Title = ""
			m.items[i].conv.Messages[0].Text = "fix the flaky test"
		}
	}
	if v := strip2(m.View()); !strings.Contains(v, "To fix the flaky test · note 59") {
		t.Errorf("an untitled recipient is named by its topic:\n%s", v)
	}
	m.livePIDs = nil
	if v := strip2(m.View()); strings.Contains(v, "To renamed") {
		t.Errorf("a recipient no longer live isn't named by its socket:\n%s", v)
	}
}

func TestSentMessageDoesNotHideAQuestion(t *testing.T) {
	c := transcript.Conversation{SessionID: "s", Messages: []transcript.Message{{Role: "assistant", Text: "Which one?"}, {Role: "sent", Text: "fyi", To: "x"}}}
	m := initialModel(buildItems([]transcript.Conversation{c}), "", nil)
	m.live = map[string]bool{"s": true}
	m.chatStatus = map[string]sessions.SessionStat{"s": {Status: "idle"}}
	if m.waitSignal(c) != waitAsked {
		t.Error("a message it sent after asking doesn't hide the question")
	}
}

func TestWorkingLine(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	conv := &transcript.Conversation{}
	transcript.NoteOutput(conv, now.Add(-time.Hour).Format(time.RFC3339), 9000) // the turn before
	transcript.NoteOutput(conv, now.Add(-40*time.Second).Format(time.RFC3339), 2000)
	transcript.NoteOutput(conv, now.Add(-10*time.Second).Format(time.RFC3339Nano), 500)
	transcript.NoteOutput(conv, "not a time", 7)
	line := workingLine(*conv, sessions.SessionStat{Status: "busy", Since: now.Add(-64 * time.Second)}, now)
	if got := strip2(line); !strings.HasSuffix(got, " Working… (1m 4s · ↓ 2.5k tokens)") {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(line, "\033[38;5;173m") || !strings.Contains(line, "\033[90m(") {
		t.Errorf("the word in Claude's colour, the facts dim: %q", line)
	}
	// No tokens yet: only the time. No clock: only the word, never a made-up time.
	if got := strip2(workingLine(*conv, sessions.SessionStat{Since: now}, now.Add(time.Second))); !strings.HasSuffix(got, " Working… (1s)") {
		t.Errorf("got %q", got)
	}
	if got := strip2(workingLine(*conv, sessions.SessionStat{}, now)); !strings.HasSuffix(got, " Working…") {
		t.Errorf("got %q", got)
	}
	// The glyph moves with each tick and comes back round.
	seen := map[string]bool{}
	for i := range spinnerFrames {
		seen[strings.Fields(strip2(workingLine(*conv, sessions.SessionStat{}, now.Add(time.Duration(i)*chatFastRefresh))))[0]] = true
	}
	if len(seen) != 6 {
		t.Errorf("frames seen: %v", seen)
	}
}

func TestWorkingLineIsOnTheLeftBesideNotes(t *testing.T) {
	m := chatModel(t)
	conv := m.items[0].conv
	m.chatStatus = map[string]sessions.SessionStat{conv.SessionID: {Status: "busy", Since: time.Now().Add(-5 * time.Second)}}
	first := func() string {
		status, _, _ := strings.Cut(m.chatBox(conv, m.chatInput, false, 80), "\n")
		return status
	}
	got := strip2(first())
	if !strings.Contains(got, "Working… (5s)") || strings.Fields(got)[1] != "Working…" || got[0] == ' ' {
		t.Errorf("should start at the left edge, its glyph in the first column: %q", got)
	}
	if w := ansi.StringWidth(first()); w != 80 {
		t.Errorf("the row should fill the width, got %d", w)
	}
	m.pending = map[string]string{conv.SessionID: "check docs"}
	got = strip2(first())
	if !strings.Contains(got, "Working… (5s)") || !strings.Contains(got, "sent: check docs") || strings.Index(got, "Working") > strings.Index(got, "sent:") {
		t.Errorf("a note follows it: %q", got)
	}
	m.chatStatus = map[string]sessions.SessionStat{conv.SessionID: {Status: "idle"}}
	m.pending = nil
	if got := strings.TrimSpace(strip2(first())); got != "" {
		t.Errorf("idle says nothing: %q", got)
	}
}

// The working line times the turn in progress, not a status time Claude Code
// left from a turn the day before.
func TestWorkingLineIgnoresAStaleStatusTime(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 40, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	conv := &transcript.Conversation{}
	for _, l := range []string{
		`{"type":"user","timestamp":"` + ts(20*time.Hour) + `","message":{"role":"user","content":"yesterday"}}`,
		`{"type":"assistant","timestamp":"` + ts(20*time.Hour-time.Second) + `","message":{"id":"a","usage":{"input_tokens":5,"output_tokens":9000},"content":[]}}`,
		`{"type":"system","subtype":"stop_hook_summary","timestamp":"` + ts(20*time.Hour-2*time.Second) + `"}`,
		`{"type":"user","timestamp":"` + ts(90*time.Second) + `","message":{"role":"user","content":"now"}}`,
		`{"type":"assistant","timestamp":"` + ts(30*time.Second) + `","message":{"id":"b","usage":{"input_tokens":5,"output_tokens":1200},"content":[]}}`,
	} {
		transcript.ParseLine(conv, []byte(l))
	}
	st := sessions.SessionStat{Status: "busy", Since: now.Add(-20 * time.Hour)}
	if got := strip2(workingLine(*conv, st, now)); !strings.HasSuffix(got, " Working… (1m 30s · ↓ 1.2k tokens)") {
		t.Errorf("got %q, want this turn's 1m 30s and its tokens only", got)
	}
}

// A fast tick that found nothing new is quiet: Update skips the sort and
// the header's counts for it. One with new lines or a changed status isn't.
func TestQuietTick(t *testing.T) {
	since := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	st := sessions.SessionStat{Status: "idle", Since: since}
	m := model{chatStatus: map[string]sessions.SessionStat{"a": st}}
	conv := transcript.Conversation{SessionID: "a"}
	for name, c := range map[string]struct {
		msg  tea.Msg
		want bool
	}{
		"the tick":        {chatTickMsg{}, true},
		"same status":     {chatTickResult{id: "a", stat: st}, true},
		"no status":       {chatTickResult{id: "a"}, true},
		"new lines":       {chatTickResult{id: "a", stat: st, conv: &conv}, false},
		"status changed":  {chatTickResult{id: "a", stat: sessions.SessionStat{Status: "busy", Since: since}}, false},
		"first status":    {chatTickResult{id: "b", stat: st}, false},
		"another message": {liveTickMsg{}, false},
	} {
		if got := m.quietTick(c.msg); got != c.want {
			t.Errorf("%s: quietTick = %v, want %v", name, got, c.want)
		}
	}
}

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// turnConv parses transcript lines into the live session's conversation.
func turnConv(t *testing.T, lines ...string) transcript.Conversation {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := transcript.ParseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil {
		t.Fatalf("parse: %v", err)
	}
	c.SessionID, c.Title = "live", "Busy one"
	return *c
}

const (
	turnUser     = `{"type":"user","timestamp":"2026-11-23T08:00:00Z","message":{"role":"user","content":"go"}}`
	turnBash     = `{"type":"assistant","timestamp":"2026-11-23T08:00:05Z","message":{"role":"assistant","id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"perl -e 1"}}]}}`
	turnQueued   = `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-23T08:00:06Z","content":"and this"}`
	turnWriting  = `{"type":"assistant","timestamp":"2026-11-23T08:00:07Z","message":{"role":"assistant","id":"m2","content":[{"type":"text","text":"Once upon a time"}]}}`
	turnLiveName = "Busy one"
)

// turnModel is chatModel with the live session set to conv, status st and
// a session file saying the same, selected; keys pressed are recorded.
func turnModel(t *testing.T, conv transcript.Conversation, st string) (model, *[]string) {
	t.Helper()
	m := chatModel(t)
	i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m.filtered[i].conv = conv
	m.cursor, m.chatFocus = i, true
	m.chatStatus = map[string]sessions.SessionStat{"live": {Status: st}}
	writeTurnFile(t, st)
	var pressed []string
	oldFind, oldSend, oldEffect := sessions.FindTerminal, sessions.SendKey, turnEffect
	sessions.FindTerminal = func(int) sessions.Terminal { return sessions.Terminal{Kind: "tmux pane", Target: "%1"} }
	sessions.SendKey = func(_ sessions.Terminal, k sessions.Key) error { pressed = append(pressed, k.Name); return nil }
	turnEffect = func(int, string, int64, int64, turnAct) bool { return true }
	t.Cleanup(func() { sessions.FindTerminal, sessions.SendKey, turnEffect = oldFind, oldSend, oldEffect })
	return m, &pressed
}

func writeTurnFile(t *testing.T, st string) {
	t.Helper()
	f := filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", os.Getpid()))
	if err := os.WriteFile(f, []byte(fmt.Sprintf(`{"sessionId":"live","status":%q,"statusUpdatedAt":1}`, st)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func offered(m model, k string) bool {
	return slices.ContainsFunc(m.shortcuts(), func(s [2]string) bool { return s[0] == k })
}

// Ctrl+B in the message box backgrounds a running Bash call, as Claude
// Code's own does, and only then; otherwise it moves back a character. ^X S
// needs a reply being written, so it's refused during the call with a note.
func TestTurnBackgroundWhileBashRuns(t *testing.T) {
	m, pressed := turnModel(t, turnConv(t, turnUser, turnBash, turnQueued), "busy")
	if !offered(m, "ctrl+b") || offered(m, "^X S") || !strings.Contains(helpFor(m, keyEsc), "interrupt") {
		t.Errorf("a Bash call running: ctrl+b and esc to interrupt offered, not ^X S: %v", m.shortcuts())
	}
	m, cmd := chord(m, "s")
	if cmd != nil || !strings.Contains(m.noteMsg, "works while "+turnLiveName+" writes a reply; it's running Bash") {
		t.Errorf("^X S during a tool call is a note: %q", m.noteMsg)
	}
	m.chatInput.SetValue("draft")
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyCtrlB})
	if cmd == nil {
		t.Fatalf("ctrl+b presses Ctrl+B: %q", m.noteMsg)
	}
	m = runTurn(m, cmd)
	if len(*pressed) != 1 || (*pressed)[0] != "Ctrl+B" || !strings.Contains(m.noteMsg, "✓ Move the running command to the background") {
		t.Errorf("pressed %v, note %q", *pressed, m.noteMsg)
	}
	if m.chatInput.Value() != "draft" || m.chatInput.Position() != 5 {
		t.Errorf("the draft and cursor stay: %q at %d", m.chatInput.Value(), m.chatInput.Position())
	}
	// No Bash running: ctrl+b is back a character, nothing pressed.
	m, pressed = turnModel(t, turnConv(t, turnUser, turnWriting), "busy")
	m.chatInput.SetValue("ab")
	m.chatInput.CursorEnd()
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlB})
	if len(*pressed) != 0 || m.chatInput.Position() != 1 || offered(m, "ctrl+b") {
		t.Errorf("no command running: ctrl+b edits: %v at %d", *pressed, m.chatInput.Position())
	}
}

// helpFor is what ? help says for key, "" when it isn't offered.
func helpFor(m model, k string) string {
	for _, s := range m.shortcuts() {
		if s[0] == k {
			return s[1]
		}
	}
	return ""
}

// ^X S sends the queued message while a reply is being written.
func TestTurnSendNowWhileReplying(t *testing.T) {
	m, pressed := turnModel(t, turnConv(t, turnUser, turnQueued, turnWriting), "busy")
	if !offered(m, "^X S") || offered(m, "ctrl+b") {
		t.Errorf("writing a reply with one queued: ^X S, not ctrl+b: %v", m.shortcuts())
	}
	m, cmd := chord(m, "s")
	m = run(m, cmd)
	if len(*pressed) != 1 || (*pressed)[0] != "Ctrl+X Ctrl+S" {
		t.Errorf("pressed %v, note %q", *pressed, m.noteMsg)
	}
	// Nothing queued: a note, nothing pressed.
	m, pressed = turnModel(t, turnConv(t, turnUser, turnWriting), "busy")
	m, cmd = chord(m, "s")
	if cmd != nil || len(*pressed) != 0 || !strings.Contains(m.noteMsg, "needs a queued message") {
		t.Errorf("nothing queued: %v %q", *pressed, m.noteMsg)
	}
}

// Esc in a busy session's message box presses Esc at once, as Claude Code's
// own does: no y/n, the draft kept and the box keeping the keys, but
// everything checked again just before the key.
func TestTurnInterruptPressesAtOnce(t *testing.T) {
	m, pressed := turnModel(t, turnConv(t, turnUser, turnWriting), "busy")
	for _, s := range m.shortcuts() {
		if slices.Contains([]string{keyEsc, "^X S", "ctrl+b"}, s[0]) && strings.Contains(s[1], "asks first") {
			t.Errorf("%s mirrors Claude Code's key, which doesn't ask: %q", s[0], s[1])
		}
	}
	m.chatInput.SetValue("half a thought")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil || m.prompting() || !m.chatFocus {
		t.Fatalf("Esc doesn't ask and keeps the box: prompting %v, cmd %v, focus %v", m.prompting(), cmd, m.chatFocus)
	}
	m = runTurn(m, cmd)
	if len(*pressed) != 1 || (*pressed)[0] != "Esc" || !strings.Contains(m.noteMsg, "✓ Interrupt") {
		t.Errorf("Esc presses Esc: %v %q", *pressed, m.noteMsg)
	}
	if m.chatInput.Value() != "half a thought" {
		t.Errorf("the draft stays: %q", m.chatInput.Value())
	}
	// Idle by the time the key would go (the session file read just
	// before it): nothing pressed.
	*pressed = nil
	writeTurnFile(t, "idle")
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m = runTurn(m, cmd)
	if len(*pressed) != 0 || !strings.Contains(m.noteMsg, "Esc not pressed") {
		t.Errorf("rechecked before the key: %v %q", *pressed, m.noteMsg)
	}
	// Idle: Esc leaves the box, as ever.
	m, pressed = turnModel(t, turnConv(t, turnUser, turnWriting), "idle")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if len(*pressed) != 0 || m.chatFocus || !strings.Contains(helpFor(m, keyEsc), "") {
		t.Errorf("idle: Esc leaves the box: %v %v", *pressed, m.chatFocus)
	}
}

// Never while it waits (Esc would answer no), never into a Codex, agy or
// idle session, never while ccs holds its question. A busy session ccs
// can't reach says why in a grey note, and Esc still leaves the box.
func TestTurnKeysRefused(t *testing.T) {
	for _, c := range []struct {
		name, st, kind, want string
	}{
		{"waiting", "waiting", "", ""},
		{"idle", "idle", "", ""},
		{"codex", "busy", transcript.KindCodex, "Codex session"},
		{"agy", "busy", transcript.KindAgy, "agy session"},
	} {
		t.Run(c.name, func(t *testing.T) {
			conv := turnConv(t, turnUser, turnBash)
			conv.Kind = c.kind
			m, pressed := turnModel(t, conv, c.st)
			if offered(m, "^X S") || offered(m, "ctrl+b") || strings.Contains(helpFor(m, keyEsc), "interrupt") {
				t.Errorf("%s: no turn key offered", c.name)
			}
			m, cmd := key(m, tea.KeyMsg{Type: tea.KeyCtrlB})
			if m = runTurn(m, cmd); len(*pressed) != 0 {
				t.Errorf("%s: ctrl+b pressed %v", c.name, *pressed)
			}
			m.chatFocus = true
			m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEsc})
			if cmd != nil || len(*pressed) != 0 || !strings.Contains(m.noteMsg, c.want) || m.chatFocus {
				t.Errorf("%s: %v %q focus %v", c.name, *pressed, m.noteMsg, m.chatFocus)
			}
		})
	}
}

// The Working… line ends with the hints for the keys that work now, as
// Claude Code's own says "esc to interrupt", each key bold as ? help names
// it; never one that would be refused, and never at the cost of a note.
func TestWorkingLineHintsTheTurnKeys(t *testing.T) {
	status := func(m model, width int) string {
		s, _, _ := strings.Cut(m.chatBox(m.filtered[m.cursor].conv, m.chatInput, m.chatFocus, width), "\n")
		return s
	}
	m, _ := turnModel(t, turnConv(t, turnUser, turnBash, turnQueued), "busy")
	raw := status(m, 140)
	got := strip2(raw)
	if !strings.Contains(got, "Working… (") || !strings.Contains(got, "esc to interrupt · ctrl+b to run in background)") {
		t.Errorf("a Bash call running: both hints, inside the facts: %q", got)
	}
	if !strings.Contains(raw, "\033[1mesc\033[22m to interrupt") || !strings.Contains(raw, "\033[1mctrl+b\033[22m to run in background") || !offered(m, keyEsc) || !offered(m, "ctrl+b") {
		t.Errorf("the keys bold, as ? help names them: %q", raw)
	}
	m, _ = turnModel(t, turnConv(t, turnUser, turnQueued, turnWriting), "busy")
	if got := strip2(status(m, 140)); !strings.Contains(got, "esc to interrupt") || strings.Contains(got, "ctrl+b") {
		t.Errorf("writing a reply: esc only: %q", got)
	}
	// A note beside it keeps its room; the hints go first.
	m.sendNote = map[string]string{"live": "a note that must be read in full"}
	if got := strip2(status(m, 60)); !strings.Contains(got, "a note that must be read in full") || strings.Contains(got, "interrupt") {
		t.Errorf("narrow, with a note: %q", got)
	}
	// The search has the keys: Esc is the search's, so no hints.
	m, _ = turnModel(t, turnConv(t, turnUser, turnBash), "busy")
	m.chatFocus = false
	if got := strip2(status(m, 140)); strings.Contains(got, "interrupt") || strings.Contains(got, "ctrl+b") {
		t.Errorf("box not focused: no hints: %q", got)
	}
	// Held for a question: no hints.
	m, _ = turnModel(t, turnConv(t, turnUser, turnBash), "busy")
	m.heldPrompt = map[string]mcp.HeldPrompt{"live": {ID: "p"}}
	if got := strip2(status(m, 140)); strings.Contains(got, "interrupt") {
		t.Errorf("a held question: no turn keys: %q", got)
	}
}

// runTurn runs cmd, a turn key's press, and applies its turnDoneMsg; the
// other commands Update batches with it (ticks, a blink) are dropped.
func runTurn(m model, cmd tea.Cmd) model {
	if cmd == nil {
		return m
	}
	msgs := []tea.Msg{cmd()}
	if b, ok := msgs[0].(tea.BatchMsg); ok {
		msgs = nil
		for _, c := range b {
			if c != nil {
				msgs = append(msgs, c())
			}
		}
	}
	for _, msg := range msgs {
		if d, ok := msg.(turnDoneMsg); ok {
			res, _ := m.Update(d)
			m = res.(model)
		}
	}
	return m
}

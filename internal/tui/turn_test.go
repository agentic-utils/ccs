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
	m.cursor = i
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

// chordKey presses ^X then k.
func chordKey(m model, k tea.KeyType) (model, tea.Cmd) {
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	return key(m, tea.KeyMsg{Type: k})
}

func offered(m model, k string) bool {
	return slices.ContainsFunc(m.shortcuts(), func(s [2]string) bool { return s[0] == k })
}

// ^X B backgrounds a running Bash call, and only then; ^X S needs a
// reply being written, so it's refused during the call with a grey note.
func TestTurnBackgroundWhileBashRuns(t *testing.T) {
	m, pressed := turnModel(t, turnConv(t, turnUser, turnBash, turnQueued), "busy")
	if !offered(m, "^X B") || offered(m, "^X S") || !offered(m, "^X Esc") {
		t.Errorf("a Bash call running: ^X B and ^X Esc offered, not ^X S: %v", m.shortcuts())
	}
	m, cmd := chord(m, "s")
	if cmd != nil || !strings.Contains(m.noteMsg, "works while "+turnLiveName+" writes a reply; it's running Bash") {
		t.Errorf("^X S during a tool call is a note: %q", m.noteMsg)
	}
	m, cmd = chord(m, "b")
	if cmd == nil {
		t.Fatalf("^X B presses Ctrl+B: %q", m.noteMsg)
	}
	m = run(m, cmd)
	if len(*pressed) != 1 || (*pressed)[0] != "Ctrl+B" || !strings.Contains(m.noteMsg, "✓ Move the running command to the background") {
		t.Errorf("pressed %v, note %q", *pressed, m.noteMsg)
	}
}

// ^X S sends the queued message while a reply is being written.
func TestTurnSendNowWhileReplying(t *testing.T) {
	m, pressed := turnModel(t, turnConv(t, turnUser, turnQueued, turnWriting), "busy")
	if !offered(m, "^X S") || offered(m, "^X B") {
		t.Errorf("writing a reply with one queued: ^X S, not ^X B: %v", m.shortcuts())
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

// ^X Esc presses Esc at once, as Claude Code's own Esc does: no y/n, but
// everything is checked again just before the key.
func TestTurnInterruptPressesAtOnce(t *testing.T) {
	m, pressed := turnModel(t, turnConv(t, turnUser, turnWriting), "busy")
	for _, s := range m.shortcuts() {
		if slices.Contains([]string{"^X Esc", "^X S", "^X B"}, s[0]) && strings.Contains(s[1], "asks first") {
			t.Errorf("%s mirrors Claude Code's key, which doesn't ask: %q", s[0], s[1])
		}
	}
	m, cmd := chordKey(m, tea.KeyEsc)
	if cmd == nil || m.prompting() || strings.Contains(strip2(m.View()), "Interrupt \"Busy one\"'s turn?") {
		t.Fatalf("^X Esc doesn't ask: prompting %v, cmd %v", m.prompting(), cmd)
	}
	m = run(m, cmd)
	if len(*pressed) != 1 || (*pressed)[0] != "Esc" || !strings.Contains(m.noteMsg, "✓ Interrupt") {
		t.Errorf("^X Esc presses Esc: %v %q", *pressed, m.noteMsg)
	}
	// Idle by the time the key would go (the session file read just
	// before it): nothing pressed.
	*pressed = nil
	writeTurnFile(t, "idle")
	m, cmd = chordKey(m, tea.KeyEsc)
	m = run(m, cmd)
	if len(*pressed) != 0 || !strings.Contains(m.noteMsg, "Esc not pressed") {
		t.Errorf("rechecked before the key: %v %q", *pressed, m.noteMsg)
	}
}

// Never while it waits (Esc would answer no), never into a Codex, agy or
// idle session, never while ccs holds its question.
func TestTurnKeysRefused(t *testing.T) {
	for _, c := range []struct {
		name, st, kind, want string
	}{
		{"waiting", "waiting", "", "waiting on a menu"},
		{"idle", "idle", "", "needs a turn running"},
		{"codex", "busy", transcript.KindCodex, "Codex session"},
		{"agy", "busy", transcript.KindAgy, "agy session"},
	} {
		t.Run(c.name, func(t *testing.T) {
			conv := turnConv(t, turnUser, turnBash)
			conv.Kind = c.kind
			m, pressed := turnModel(t, conv, c.st)
			for _, k := range []tea.KeyType{tea.KeyEsc, tea.KeyEnter} {
				m, _ = chordKey(m, k)
			}
			m, cmd := chord(m, "b")
			if cmd != nil || len(*pressed) != 0 || !strings.Contains(m.noteMsg, c.want) {
				t.Errorf("%s: %v %q", c.name, *pressed, m.noteMsg)
			}
			if offered(m, "^X Esc") || offered(m, "^X S") || offered(m, "^X B") {
				t.Errorf("%s: no turn key offered", c.name)
			}
		})
	}
}

// ^X Esc on a session that isn't running is a cancelled chord, as before.
func TestChordEscOnNothingLiveCancels(t *testing.T) {
	m := chatModel(t)
	i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "idle" })
	m.cursor = i
	m, cmd := chordKey(m, tea.KeyEsc)
	if cmd != nil || m.noteMsg != "" || m.chordX {
		t.Errorf("a cancelled chord: %q %v", m.noteMsg, m.chordX)
	}
}

package tui

import (
	"errors"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// The turn keys: Claude Code's own keys for a running turn, pressed in the
// selected session's terminal, since nothing else reaches them (its socket
// has no control action that interrupts, a socket message with priority
// "now" waits for the running tool call, and SIGINT quits the session).
//
//	esc       interrupt the turn (Esc, chat:cancel), from the message box
//	^X S      send the queued message now (Ctrl+X Ctrl+S, chat:sendNow)
//	ctrl+b    move the running command to the background (Ctrl+B, task:background), from the message box
//
// Each is offered only when it works now: the session busy, and for
// send-now a reply being written with a message queued (Claude Code ignores
// it during a tool call), for background a Bash call running. Esc is also
// "no" on a permission prompt and "cancel" on a menu, so nothing is pressed
// while the session waits: the same checks as a slash command's on the key
// (turnRefusal), and again immediately before the keys (turnKeyNow), which
// then confirms the effect from the transcript or session file.

// turnAct is one of the turn keys.
type turnAct struct {
	stat  stats.Feature // stats registry id
	what  string        // what it does, in notes and the log
	key   sessions.Key
	chord string // ccs's key for it
	mark  string // what the transcript gains when it worked; "" for interrupt, judged by status too
}

var (
	turnInterrupt  = turnAct{"tui.turn_interrupt", "interrupt", sessions.KeyInterrupt, "esc", "[Request interrupted by user"}
	turnSendNow    = turnAct{"tui.turn_send_now", "send the queued message now", sessions.KeySendNow, "^X S", `"operation":"dequeue"`}
	turnBackground = turnAct{"tui.turn_background", "move the running command to the background", sessions.KeyBackground, "ctrl+b", "manually backgrounded by user"}
)

// turnConfirmWait is how long ccs looks for a turn key's effect after
// pressing it; turnConfirmEvery how often.
var (
	turnConfirmWait  = 5 * time.Second
	turnConfirmEvery = 250 * time.Millisecond
)

// turnDoneMsg ends a turn key's press.
type turnDoneMsg struct {
	note string
	err  error
}

// turnRefusal says why act can't be pressed in conv's terminal now, or ""
// when it can: slashRefusal's checks, but for a busy session, plus what act
// itself needs.
func (m model) turnRefusal(conv transcript.Conversation, act turnAct) string {
	id, name, pid := conv.SessionID, m.sessionName(conv), m.livePIDs[conv.SessionID]
	_, bg := m.bg[id]
	not := act.chord + " (" + act.what + ") "
	switch st := m.chatStatus[id].Status; {
	case !m.live[id]:
		return not + "needs a running session; " + name + " isn't running"
	case conv.Kind == transcript.KindCodex:
		return not + "is for Claude Code sessions; " + name + " is a Codex session"
	case conv.Kind == transcript.KindAgy:
		return not + "is for Claude Code sessions; " + name + " is an agy session"
	case conv.Kind == transcript.KindSubagent:
		return not + "goes to the subagent's session, not the subagent"
	case bg:
		return not + "can't reach " + name + ": it runs in the background, with no terminal ccs can type into"
	case m.hostedApp(id) != "":
		return not + "can't reach " + name + ": it runs in " + m.hostedApp(id) + ", which ccs can't type into"
	case m.heldPrompt[id].ID != "":
		return not + "isn't pressed: ccs holds a question for " + name + ", answer that first"
	case st == "waiting":
		return not + "isn't pressed: " + name + " is waiting on a menu, which would take the key"
	case st != "busy":
		return not + "needs a turn running; " + name + " is " + statusWords(st)
	case m.unverified[id] || pid == 0:
		return not + "isn't pressed: " + name + "'s process couldn't be checked against its session file"
	case m.sending:
		return not + "isn't pressed: still sending the last message"
	}
	switch tool := conv.RunningTool(); act.stat {
	case turnSendNow.stat:
		switch {
		case tool != "":
			return not + "works while " + name + " writes a reply; it's running " + tool + " now"
		case len(conv.QueuedNow()) == 0:
			return not + "needs a queued message; " + name + " has none"
		}
	case turnBackground.stat:
		if tool != "Bash" {
			return not + "needs a command running in the foreground; " + name + " isn't running one"
		}
	}
	return ""
}

// turnKey answers a turn chord: a grey note when it doesn't apply, else the
// key pressed now. None asks first, interrupt included: Claude Code's own
// Esc doesn't, and turnKeyNow checks everything again before the key.
func (m *model) turnKey(act turnAct) tea.Cmd {
	m.helpOpen = false
	if len(m.filtered) == 0 {
		return nil
	}
	conv := m.filtered[m.cursor].conv
	if why := m.turnRefusal(conv, act); why != "" {
		m.note(why)
		return nil
	}
	return m.turnCmd(conv, act)
}

// turnCmd presses act's key for conv off the UI goroutine.
func (m *model) turnCmd(conv transcript.Conversation, act turnAct) tea.Cmd {
	id, name, pid, unverified := conv.SessionID, m.sessionName(conv), m.livePIDs[conv.SessionID], m.unverified[conv.SessionID]
	return func() tea.Msg {
		note, err := turnKeyNow(pid, id, name, act, unverified)
		state.LogActivity("turn key: %s in %s (%s): %s", act.key.Name, name, act.what, transcript.CleanText(note))
		return turnDoneMsg{note, err}
	}
}

// turnKeyNow finds pid's terminal, checks everything again immediately
// before the key (the same session in the same process, busy, no prompt ccs
// holds), presses it, then watches for its effect. An error wraps
// sessions.ErrUnsent when nothing was pressed.
func turnKeyNow(pid int, id, name string, act turnAct, unverified bool) (string, error) {
	unsent := func(why string) (string, error) {
		return act.key.Name + " not pressed: " + why, errors.Join(sessions.ErrUnsent, errors.New(why))
	}
	t := sessions.FindTerminal(pid)
	if t.Kind == "" {
		return unsent(name + " isn't in iTerm or tmux")
	}
	path := transcript.TranscriptPath(id)
	var off int64
	if info, err := os.Stat(path); err == nil {
		off = info.Size()
	}
	f, same, checked := sessions.StillTheSession(pid, id)
	switch {
	case !same || (!checked && unverified):
		return unsent(name + "'s session has changed")
	case f.Status != "busy":
		return unsent(name + " is " + statusWords(f.Status) + " now, not mid-turn")
	case heldFor(id):
		return unsent("ccs holds a question for " + name)
	}
	where := t.Kind
	switch err := sessions.SendKey(t, act.key); {
	case errors.Is(err, sessions.ErrNoTerminal):
		return unsent(name + " isn't in iTerm or tmux")
	case errors.Is(err, proc.ErrTimedOut):
		return "⚠ Pressing " + act.key.Name + " in " + name + "'s " + where + " timed out; it may have been pressed", err
	case err != nil:
		return "✗ Couldn't press " + act.key.Name + " in " + name + "'s " + where + ": " + err.Error(), err
	}
	if turnEffect(pid, path, off, f.StatusUpdatedAt, act) {
		return "✓ " + upperFirst(act.what) + ": pressed " + act.key.Name + " in " + name + "'s " + where, nil
	}
	return "⚠ Pressed " + act.key.Name + " in " + name + "'s " + where + ", but its transcript doesn't show it took effect yet", nil
}

// turnEffect watches, up to turnConfirmWait, for act's effect: its mark in
// what the transcript gained past off, or for interrupt the session file
// leaving busy after statusAt.
var turnEffect = func(pid int, path string, off, statusAt int64, act turnAct) bool {
	deadline := time.Now().Add(turnConfirmWait)
	for {
		if path != "" && sessions.AppendedHas(path, off, act.mark) {
			return true
		}
		if act.stat == turnInterrupt.stat {
			if f, err := sessions.ReadSessionFile(pid); err == nil && f.Status != "busy" && f.StatusUpdatedAt > statusAt {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(turnConfirmEvery)
	}
}

// statusWords is a session file's status as a note says it: "shell" is
// Claude Code's idle with a shell still running in the background.
func statusWords(st string) string {
	switch st {
	case "shell":
		return "idle, with a shell in the background"
	case "":
		return "not busy"
	}
	return st
}

// upperFirst is s with its first letter capitalised.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// applyTurnDone shows a turn key's outcome.
func (m *model) applyTurnDone(msg turnDoneMsg) {
	switch {
	case errors.Is(msg.err, sessions.ErrUnsent):
		m.note(msg.note)
	case msg.err != nil:
		m.errorMsg = msg.note
	default:
		m.note(msg.note)
	}
}

// busySelected is the selected session when its status is busy.
func (m model) busySelected() (transcript.Conversation, bool) {
	if len(m.filtered) == 0 {
		return transcript.Conversation{}, false
	}
	conv := m.filtered[m.cursor].conv
	return conv, m.chatStatus[conv.SessionID].Status == "busy"
}

// turnHintWords are the turn keys' words on the Working… line, as Claude
// Code's own say them.
var turnHintWords = map[stats.Feature]string{
	turnInterrupt.stat:  "to interrupt",
	turnBackground.stat: "to run in background",
}

// turnHints are the Working… line's hints, as Claude Code's own "esc to
// interrupt": Esc and Ctrl+B, each only while the message box has the keys
// and it works now (turnRefusal), the key in bold as ? help names it.
func (m model) turnHints(conv transcript.Conversation) []string {
	if !m.chatFocus {
		return nil
	}
	var out []string
	for _, act := range []turnAct{turnInterrupt, turnBackground} {
		if act.stat == turnInterrupt.stat && m.runShown() {
			continue // Esc stops or closes the command's box first
		}
		if m.turnRefusal(conv, act) == "" {
			out = append(out, hints(act.chord, turnHintWords[act.stat]))
		}
	}
	return out
}

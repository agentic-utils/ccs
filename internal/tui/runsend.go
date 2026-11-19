package tui

// Sending a finished ! command's result to the session it ran for (^S on
// its output box): "o" with the output, "e" the exit code only. Only ever to
// the same process the command ran for (its pid and start time, recorded at
// the start), through its message socket, as a message the user sent via
// ccs; never typed, and never to whatever is selected now.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// runTargetName names the session the shown command ran for.
func (m model) runTargetName() string {
	if m.run == nil {
		return ""
	}
	return m.sessionNameByID(m.run.id)
}

// runSendable: ^S applies now, to the selected session's finished command
// box, with nothing typed in the message box (text there is a message being
// written, and ^S leaves it alone).
func (m model) runSendable() bool {
	return m.runShown() && !m.run.running && strings.TrimSpace(m.chatInput.Value()) == ""
}

// runOutcome is how the command ended, for the message: "exit 0", "timed
// out after 2m", "stopped" or the error.
func (r *shellRun) runOutcome() string {
	switch {
	case errors.Is(r.err, proc.ErrTimedOut):
		return "timed out after " + formatTook(runTimeout)
	case errors.Is(r.err, context.Canceled):
		return "stopped"
	case r.err != nil:
		return r.err.Error()
	}
	return fmt.Sprintf("exit %d", r.res.Code)
}

// runMessage is what's sent: the first line always, the output after it
// when withOutput, in a fence longer than any run of backticks in it, cut
// at a line boundary to runSendCap.
func (r *shellRun) runMessage(withOutput bool) string {
	head := "I ran `" + r.line + "` in " + tildePath(r.dir) + ": " + r.runOutcome()
	if !withOutput {
		return head
	}
	out := strings.TrimRight(cleanOutput(r.res.Out), "\n")
	if out == "" {
		return head + " (no output)"
	}
	full := len(out)
	cutLines := 0
	if len(out) > runSendCap {
		keep := out[:runSendCap]
		if i := strings.LastIndexByte(keep, '\n'); i > 0 {
			keep = keep[:i]
		}
		cutLines = strings.Count(out[len(keep):], "\n")
		if !strings.HasPrefix(out[len(keep):], "\n") {
			cutLines++ // the partial line that was dropped
		}
		out = keep
	}
	switch {
	case cutLines > 0:
		head += fmt.Sprintf(" (output cut to %s of %s)", kb(len(out)), kb(full))
	case r.res.Cut:
		head += fmt.Sprintf(" (output cut at %s when it ran)", kb(runOutputCap))
	}
	fence := strings.Repeat("`", max(3, longestRun(out, '`')+1))
	msg := head + ":\n" + fence + "\n" + out + "\n" + fence
	if cutLines > 0 {
		msg += fmt.Sprintf("\n… %d more lines cut", cutLines)
	}
	return msg
}

func kb(n int) string { return fmt.Sprintf("%dKB", (n+1023)>>10) }

// longestRun is the longest run of c in s.
func longestRun(s string, c byte) int {
	best, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}

// runKey handles a key while ^S's choice is open: o, e or esc. Everything
// else is ignored, so a stray key can't send.
func (m *model) runKey(k string) tea.Cmd {
	switch k {
	case "o", "e":
		m.run.asking = false
		return m.sendRunResult(k == "o")
	case "esc", "ctrl+c":
		m.run.asking = false
	}
	return nil
}

// sendRunResult sends the finished command's result to the process it ran
// for, through its message socket only.
func (m *model) sendRunResult(withOutput bool) tea.Cmd {
	r := m.run
	id, name := r.id, m.runTargetName()
	if start, ok := liveStart(r.pid, id); r.pid <= 0 || !ok || start != r.procFrom || r.procFrom == "" {
		m.boxNote(id, grey("not sent: "+name+" isn't the same running session the command ran for (it has exited or restarted)"))
		return nil
	}
	if m.sending {
		m.errorMsg = "Still sending the last message…"
		return nil
	}
	conv := m.convByID()[id]
	text := r.runMessage(withOutput)
	stats.Record("tui.run_send", stats.Key)
	state.LogActivity("run: sent `%s` (%s) to %s %s", r.line, r.runOutcome(), name, map[bool]string{true: "with its output", false: "without its output"}[withOutput])
	if m.pending == nil {
		m.pending = make(map[string]string)
	}
	m.pending[id] = text
	delete(m.sendNote, id)
	m.sending = true
	pid, kind := r.pid, conv.Kind
	return func() tea.Msg {
		note, err := deliverRunResult(pid, id, name, kind, text)
		return sendDoneMsg{id: id, note: note, err: err, keepDraft: true}
	}
}

// deliverRunResult sends text over the session's message socket, never
// typed (a Codex session through codex queue, which types nothing either).
var deliverRunResult = func(pid int, id, name, kind, text string) (string, error) {
	if kind == transcript.KindCodex {
		return sessions.DeliverCodex(id, name, text)
	}
	f, err := sessions.ReadSessionFile(pid)
	if err != nil || f.SessionID != id {
		return "✗ Couldn't reach " + name + ": its session has changed", fmt.Errorf("%w: session file", sessions.ErrUnsent)
	}
	if f.Socket == "" {
		return "✗ Not sent to " + name + ": it has no message socket, and a command's result is never typed", fmt.Errorf("%w: no socket", sessions.ErrUnsent)
	}
	msgID, err := sendViaSocket(f.Socket, sessions.PeerToken(pid, f.Socket), text)
	if msgID != "" {
		sessions.SentMsgs.Store(msgID, id)
	}
	switch {
	case err == nil:
		return "✓ Sent the result to " + name + " via its message socket (as a message from ccs)", nil
	case errors.Is(err, sessions.ErrNotConnected):
		return "✗ Not sent to " + name + ": its message socket isn't reachable, and a command's result is never typed", fmt.Errorf("%w: %v", sessions.ErrUnsent, err)
	}
	return "✗ Sending to " + name + " may have failed: " + err.Error(), err
}

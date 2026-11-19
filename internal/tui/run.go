package tui

// Commands from the message box. Only a key pressed in this TUI starts one:
// Enter in the box goes through commandLine, and nothing else calls it. ccs
// serve, the web page, MCP and messages from other sessions never reach it,
// so text that merely starts with ! or / from anywhere else stays text.
//
//   - "!cmd" runs cmd here, in ccs, in the selected session's folder: the
//     user's shell with -lc and cmd as one argument, detached, no stdin, a
//     deadline (runTimeout), its output in a box above the message box.
//   - "/cmd" is a slash command, typed into the session's own terminal
//     (tmux or iTerm) so Claude Code runs it, only while the session is idle.
//   - "!!x" and "//x" send "!x" and "/x" as an ordinary message.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// runTimeout is how long a ! command may run; runOutputCap how much of its
// output is kept.
var runTimeout = 2 * time.Minute

const runOutputCap = 64 << 10

// runShell runs a ! command; stubbed in tests.
var runShell = proc.RunShell

// activeRun is the ! command running now, if any, outside the model so
// ccs's exit (quit, or the restart into an update) can stop it: a command
// must never outlive the ccs that ran it, or its deadline.
var activeRun struct {
	sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// runActive: a ! command is running. Automatic updates wait for it.
func runActive() bool {
	activeRun.Lock()
	defer activeRun.Unlock()
	return activeRun.done != nil
}

// stopActiveRun kills the running ! command's process group, if any, and
// waits for it to end, at most wait.
func stopActiveRun(wait time.Duration) {
	activeRun.Lock()
	cancel, done := activeRun.cancel, activeRun.done
	activeRun.Unlock()
	if done == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

// shellRun is a ! command: running, or finished and still shown.
type shellRun struct {
	id, dir, line string
	gen           int
	start         time.Time
	running       bool
	cancel        context.CancelFunc
	res           proc.ShellResult
	err           error
	top           int // the first output line in view
}

type runDoneMsg struct {
	gen int
	res proc.ShellResult
	err error
}

type runTickMsg struct{ gen int }

// commandLine handles a box line that is a command (see the top of this
// file) and says whether it did. A "!!" or "//" line is sent as a message
// less its first character; the draft itself is never changed, so a refused
// or failed send gives back the escaped line, never one that would run.
func (m *model) commandLine() (tea.Cmd, bool) {
	text := strings.TrimSpace(m.chatInput.Value())
	conv := m.filtered[m.cursor].conv
	switch {
	case strings.HasPrefix(text, "!!"), strings.HasPrefix(text, "//"):
		stats.Record("tui.send", stats.Key)
		return m.sendLine(conv, text, text[1:]), true
	case strings.HasPrefix(text, "!"):
		stats.Record("tui.run_command", stats.Key)
		return m.startRun(conv, strings.TrimSpace(text[1:])), true
	case strings.HasPrefix(text, "/"):
		stats.Record("tui.slash_command", stats.Key)
		return m.typeSlash(conv, text), true
	}
	return nil, false
}

// hasControl: a character a terminal would act on rather than show.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError })
}

func (m *model) boxNote(id, note string) {
	if m.sendNote == nil {
		m.sendNote = make(map[string]string)
	}
	m.sendNote[id] = note
}

// grey is a note: information, not an error.
func grey(s string) string { return "\033[90m" + s + "\033[0m" }

// startRun runs line in conv's folder.
func (m *model) startRun(conv transcript.Conversation, line string) tea.Cmd {
	id := conv.SessionID
	switch {
	case line == "":
		m.boxNote(id, grey("! needs a command after it, as in !git status"))
		return nil
	case m.run != nil && m.run.running:
		m.boxNote(id, grey("a command is still running: "+keyEsc+" stops it"))
		return nil
	}
	dir := conv.Cwd
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		m.boxNote(id, grey("! needs the session's folder, and "+cmpOr(dir, "it has none")+" isn't there"))
		return nil
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.runGen++
	gen := m.runGen
	m.run = &shellRun{id: id, dir: dir, line: line, gen: gen, start: time.Now(), running: true, cancel: cancel}
	m.chatInput.SetValue("")
	delete(m.sendNote, id)
	timeout := runTimeout
	done := make(chan struct{})
	activeRun.Lock()
	activeRun.cancel, activeRun.done = cancel, done
	activeRun.Unlock()
	return tea.Batch(func() tea.Msg {
		defer func() {
			activeRun.Lock()
			if activeRun.done == done {
				activeRun.cancel, activeRun.done = nil, nil
			}
			activeRun.Unlock()
			close(done)
		}()
		res, err := runShell(ctx, dir, shell, line, timeout, runOutputCap)
		return runDoneMsg{gen: gen, res: res, err: err}
	}, runTick(gen))
}

// runTook is how long a command ran: tenths of a second under ten seconds.
func runTook(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return formatTook(d)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// runTick redraws "running… Ns" once a second, only while it runs; stubbed
// in tests, so no timer outlives one.
var runTick = func(gen int) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return runTickMsg{gen} })
}

// applyRun takes a run's result or tick.
func (m *model) applyRun(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case runTickMsg:
		if m.run != nil && m.run.gen == msg.gen && m.run.running {
			return runTick(msg.gen)
		}
	case runDoneMsg:
		r := m.run
		if r == nil || r.gen != msg.gen {
			return nil
		}
		r.running, r.res, r.err = false, msg.res, msg.err
		r.cancel()
		outcome := fmt.Sprintf("exit %d", msg.res.Code)
		if msg.err != nil {
			outcome = msg.err.Error()
		}
		state.LogActivity("run: $ %s in %s: %s after %s", r.line, r.dir, outcome, msg.res.Took.Round(time.Millisecond))
	}
	return nil
}

// stopRun cancels a running command, or closes a finished one's box. It
// says whether there was one for the selected session.
func (m *model) stopRun() bool {
	r := m.run
	if r == nil || len(m.filtered) == 0 || r.id != m.filtered[m.cursor].conv.SessionID {
		return false
	}
	if r.running {
		r.cancel()
		return true
	}
	m.run = nil
	return true
}

// runShown: the selected session's command box is drawn.
func (m model) runShown() bool {
	return m.run != nil && len(m.filtered) > 0 && m.run.id == m.filtered[m.cursor].conv.SessionID && !m.boxHidden()
}

// runLimit is the most rows the command box takes, borders included: with
// the status line and message box under it, no more than two thirds of the
// pane, as a held question's box.
func (m model) runLimit() int { return max(m.heldLimit()-4, 5) }

// runOutput is the command's output as lines wrapped to width.
func (r *shellRun) runOutput(width int) []string {
	text := strings.TrimRight(cleanOutput(r.res.Out), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(ansi.Wrap(text, max(width, 10), ""), "\n")
}

// cleanOutput makes a command's output safe to draw: invalid UTF-8 becomes
// U+FFFD, escape sequences go whatever else is there (CleanText skips that
// unless it sees a C0 control), and then every C0 control but newline and
// tab, DEL and every C1 control (U+0080-U+009F: a lone CSI or OSC some
// terminals act on) are dropped. Carriage returns go too, so progress bars
// don't redraw over the box.
func cleanOutput(b []byte) string {
	s := ansi.Strip(strings.ToValidUTF8(string(b), "\uFFFD"))
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// runLines is the command box for the selected session: the command, the
// output in view and how it ended.
func (m model) runLines() []string {
	if !m.runShown() {
		return nil
	}
	r, inner := m.run, m.heldInner()
	head := "\033[1m$ " + ansi.Truncate(r.line, max(inner-2, 1), "…") + "\033[0m"
	var foot string
	switch {
	case r.running:
		foot = grey(fmt.Sprintf("running… %ds · %s stops it", int(time.Since(r.start).Seconds()), keyEsc))
	case errors.Is(r.err, proc.ErrTimedOut):
		foot = "\033[31mtimed out after " + formatTook(runTimeout) + "\033[0m" + grey(" · "+keyEsc+" closes")
	case errors.Is(r.err, context.Canceled):
		foot = "\033[31mstopped\033[0m" + grey(" · "+keyEsc+" closes")
	case r.err != nil:
		foot = "\033[31m" + ansi.Truncate(r.err.Error(), max(inner-14, 10), "…") + "\033[0m" + grey(" · "+keyEsc+" closes")
	case r.res.Code != 0:
		foot = fmt.Sprintf("\033[31mexit %d\033[0m", r.res.Code) + grey(" · "+runTook(r.res.Took)+" · "+keyEsc+" closes")
	default:
		foot = grey("exit 0 · " + runTook(r.res.Took) + " · " + keyEsc + " closes")
	}
	out := r.runOutput(inner)
	if r.res.Cut {
		out = append(out, grey(fmt.Sprintf("… cut at %dKB", runOutputCap>>10)))
	}
	room := max(m.runLimit()-4, 1) // borders, the command and its ending
	top := min(r.top, max(len(out)-room, 0))
	view := out[top:min(top+room, len(out))]
	if len(out) > room {
		foot += grey(fmt.Sprintf(" · %d-%d of %d · pgup/pgdn", top+1, top+len(view), len(out)))
	}
	inner2 := append([]string{head}, view...)
	return heldBox(append(inner2, foot), m.paneWidth(), false)
}

// scrollRun moves the command box's output by delta lines.
func (m *model) scrollRun(delta int) {
	if !m.runShown() {
		return
	}
	n := len(m.run.runOutput(m.heldInner()))
	room := max(m.runLimit()-4, 1)
	m.run.top = max(min(m.run.top+delta, n-room), 0)
}

// heldFor: this ccs holds a prompt for session id now; stubbed in tests.
var heldFor = func(id string) bool {
	if h := ownHold(); h != nil {
		for _, p := range h.List() {
			if p.Session == id {
				return true
			}
		}
	}
	return false
}

// typeSlash types a slash command into conv's terminal and presses Enter, so
// Claude Code runs it. Only into an idle session ccs can type into: a busy
// one, or one on a menu (its own or one ccs holds), would take the keys
// as an answer or a message.
func (m *model) typeSlash(conv transcript.Conversation, text string) tea.Cmd {
	id, name, pid := conv.SessionID, m.sessionName(conv), m.livePIDs[conv.SessionID]
	word, _, _ := strings.Cut(text, " ")
	refuse := func(why string) tea.Cmd {
		m.boxNote(id, grey(why))
		return nil
	}
	switch st := m.chatStatus[id].Status; {
	case hasControl(text):
		return refuse(word + " not typed: it has control characters")
	case conv.Kind == transcript.KindCodex:
		return refuse(word + " not typed: " + name + " is a Codex session; slash commands go to Claude Code sessions only")
	case m.hostedApp(id) != "":
		return refuse(word + " not typed: " + name + " runs in " + m.hostedApp(id) + ", which ccs can't type into")
	case m.heldPrompt[id].ID != "":
		return refuse(word + " needs " + name + " idle; ccs holds a question for it, answer that first")
	case st == "busy":
		return refuse(word + " needs " + name + " idle; it's busy")
	case st == "waiting":
		return refuse(word + " needs " + name + " idle; it's waiting on a menu")
	case m.unverified[id] || pid == 0:
		return refuse(word + " not typed: " + name + "'s process couldn't be checked against its session file")
	case m.sending:
		return refuse(word + " not typed: still sending the last message")
	}
	m.sending = true
	m.chatInput.SetValue("")
	delete(m.sendNote, id)
	unverified := m.unverified[id]
	return func() tea.Msg {
		unsent := func(note string) tea.Msg {
			return sendDoneMsg{id: id, note: grey(word + " not typed: " + note), err: fmt.Errorf("%w: %s", sessions.ErrUnsent, note), text: text}
		}
		// Find the terminal first (ps, tmux list-panes: it can take seconds),
		// then check everything again immediately before the keys: the same
		// session in the same process, idle, with no prompt ccs holds. That
		// narrows the race to the moment between this check and the keys
		// landing; only an atomic operation on the session's side could
		// rule it out, and Claude Code has none.
		t := sessions.FindTerminal(pid)
		if t.Kind == "" {
			return unsent(name + " isn't in iTerm or tmux")
		}
		f, same, checked := sessions.StillTheSession(pid, id)
		switch {
		case !same || (!checked && unverified):
			return unsent(name + "'s session has changed")
		case f.Status != "idle":
			return unsent(word + " needs " + name + " idle; it's " + cmpOr(f.Status, "not idle"))
		case heldFor(id):
			return unsent(word + " needs " + name + " idle; ccs holds a question for it")
		}
		where := t.Kind
		err := sessions.SendKeys(t, text, true)
		switch {
		case errors.Is(err, sessions.ErrNoTerminal):
			return unsent(name + " isn't in iTerm or tmux")
		case errors.Is(err, proc.ErrTimedOut):
			return sendDoneMsg{id: id, note: "⚠ Typing " + word + " into " + name + "'s " + where + " timed out; it may have been typed", err: err}
		case err != nil:
			return sendDoneMsg{id: id, note: "✗ Couldn't type " + word + " into " + name + "'s " + where + ": " + err.Error(), err: err}
		}
		return sendDoneMsg{id: id, note: "✓ Typed " + word + " into " + name + "'s " + where + " (as you)"}
	}
}

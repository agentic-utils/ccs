package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ^X V's popup: Remote Control for running Claude Code sessions, typed as
// `/remote-control <name>` into each one's terminal (Claude Code has no
// other way in for a terminal session), and the launch setting
// (state.RemoteControl) that used to be ^X V by itself.
//
// Typed only into an idle session with Remote Control off: with it on, the
// same command opens a menu that waits for a key, and any other state would
// take the keys as an answer or a message. Everything typeSlash checks is
// checked here too, through the same code (slashRefusal on the key,
// typeSlashNow immediately before the keys, with the session file's
// bridgeSessionId read then).

// rcConfirmWait is how long ccs waits for bridgeSessionId to appear in a
// session's file after typing; rcConfirmEvery how often it looks.
var (
	rcConfirmWait  = 15 * time.Second
	rcConfirmEvery = 250 * time.Millisecond
)

// rcTarget is a session to type /remote-control into.
type rcTarget struct {
	id, name, label string // label is how ccs shows it; name the Remote Control name
	pid             int
	unverified      bool
}

// rcResult is what happened to one session.
type rcResult struct {
	id, label, why string // why: the grey reason it was skipped, or the outcome
	on, skipped    bool
}

// rcDoneMsg ends a ^X V run.
type rcDoneMsg struct{ results []rcResult }

// rcCommand is what ccs types for name.
func rcCommand(name string) string { return "/remote-control " + name }

// rcName is the ccs name Remote Control gets: the session file's name, else
// a user-set title, when it is a session name (state.SessionName).
func (m model) rcName(conv transcript.Conversation) string {
	for _, n := range []string{m.chatStatus[conv.SessionID].Name, conv.Title} {
		if state.SessionName.MatchString(n) {
			return n
		}
	}
	return ""
}

// rcCheck says why ccs won't type /remote-control into conv now ("" when
// it will) and the name it would use.
func (m model) rcCheck(conv transcript.Conversation) (name, why string) {
	id, label := conv.SessionID, m.sessionName(conv)
	if !m.live[id] {
		return "", label + " isn't running"
	}
	name = m.rcName(conv)
	st := m.chatStatus[id]
	if st.Remote {
		return name, "Remote Control is already on in " + label
	}
	if name == "" {
		return "", label + " has no ccs name to show it under (a name is letters, digits, . _ -)"
	}
	if why := m.slashRefusal(conv, rcCommand(name)); why != "" {
		return name, why
	}
	if st.Status != "idle" {
		return name, "/remote-control needs " + label + " idle; it's " + cmpOr(st.Status, "not reporting a status")
	}
	return name, ""
}

// rcCandidates are the running sessions ^X V's "all" considers: Claude
// Code ones (Codex, agy and subagent rows never have Remote Control).
func (m model) rcCandidates() []transcript.Conversation {
	var out []transcript.Conversation
	for _, it := range m.items {
		c := it.conv
		if !m.live[c.SessionID] || c.Kind == transcript.KindCodex || c.Kind == transcript.KindAgy || c.Kind == transcript.KindSubagent {
			continue
		}
		out = append(out, c)
	}
	return out
}

// rcSelected is the selected session, if it's a candidate.
func (m model) rcSelected() (transcript.Conversation, bool) {
	if len(m.filtered) == 0 || m.cursor >= len(m.filtered) {
		return transcript.Conversation{}, false
	}
	return m.filtered[m.cursor].conv, true
}

// rcPopup is ^X V's popup.
func (m model) rcPopup() string {
	var b strings.Builder
	b.WriteString("\033[1mRemote Control\033[0m\n\n")
	if c, ok := m.rcSelected(); ok {
		if name, why := m.rcCheck(c); why == "" {
			b.WriteString(fmt.Sprintf("\033[1mt\033[0m  turn it on in %s now, as %s\n", m.sessionName(c), name))
		} else {
			b.WriteString("\033[90mt  " + why + "\033[0m\n")
		}
	} else {
		b.WriteString("\033[90mt  no session selected\033[0m\n")
	}
	ready := 0
	for _, c := range m.rcCandidates() {
		if _, why := m.rcCheck(c); why == "" {
			ready++
		}
	}
	switch ready {
	case 0:
		b.WriteString("\033[90ma  no other running session can take it now\033[0m\n")
	case 1:
		b.WriteString("\033[1ma\033[0m  turn it on in the 1 running session that can take it\n")
	default:
		b.WriteString(fmt.Sprintf("\033[1ma\033[0m  turn it on in all %d running sessions that can take it\n", ready))
	}
	launch := "off → on"
	if m.remoteControl {
		launch = "on → off"
	}
	b.WriteString("\033[1mv\033[0m  for sessions ccs starts or resumes: " + launch + "\n")
	b.WriteString("\n\033[90monly idle Claude Code sessions in a terminal, with it off\033[0m\n")
	b.WriteString("\n\033[1mccs serve\033[0m, the sessions as a web page for your phone\n")
	switch {
	case !m.serve.Installed:
		b.WriteString("\033[1mw\033[0m  set it up: try it on this Mac now, or set it up for your phone\n")
	case m.serve.Running:
		b.WriteString("\033[1mw\033[0m  its address and QR code; s there stops it, e edits its setup\n")
	default:
		b.WriteString("\033[1mw\033[0m  start it (its launchd agent)\n")
	}
	b.WriteString("\n\033[90m" + hints("t", "this one", "a", "all", "v", "setting", "w", "web page", "^X V/"+keyEsc, "close") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(b.String())
}

// rcKey handles a key while ^X V's popup is open.
func (m model) rcKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "esc", "^x v", "q":
		m.rcOpen = false
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "v":
		m.rcOpen = false
		m.toggleRemoteControl()
	case "w":
		m.rcOpen = false
		stats.Record("tui.serve", stats.Key)
		return m, m.serveKey()
	case "t":
		m.rcOpen = false
		stats.Record("tui.remote_control_now", stats.Key)
		c, ok := m.rcSelected()
		if !ok {
			return m, nil
		}
		return m, m.rcStart([]transcript.Conversation{c})
	case "a":
		m.rcOpen = false
		stats.Record("tui.remote_control_all", stats.Key)
		return m, m.rcStart(m.rcCandidates())
	}
	return m, nil
}

// toggleRemoteControl flips the launch setting (state.RemoteControl).
func (m *model) toggleRemoteControl() {
	stats.Record("tui.remote_control", stats.Key)
	if err := state.SetRemoteControl(!m.remoteControl); err != nil {
		m.errorMsg = "Couldn't save the Remote Control setting: " + err.Error()
		return
	}
	m.remoteControl = !m.remoteControl
	if m.remoteControl {
		m.note("Remote Control on: Claude sessions ccs launches, resumes or restarts show on your phone under their ccs name")
	} else {
		m.note("Remote Control off for new launches and resumes; a restart keeps it on for a session that had it")
	}
}

// rcStart checks each of convs on the key and starts typing into those that
// pass; the rest get their grey note and a log line straight away.
func (m *model) rcStart(convs []transcript.Conversation) tea.Cmd {
	if m.rcRunning {
		m.note("Remote Control: still turning it on in the last ones")
		return nil
	}
	var targets []rcTarget
	var skipped []rcResult
	for _, c := range convs {
		name, why := m.rcCheck(c)
		label := m.sessionName(c)
		if why != "" {
			skipped = append(skipped, rcResult{id: c.SessionID, label: label, why: why, skipped: true})
			continue
		}
		targets = append(targets, rcTarget{id: c.SessionID, name: name, label: label, pid: m.livePIDs[c.SessionID], unverified: m.unverified[c.SessionID]})
	}
	if len(targets) == 0 {
		m.applyRC(rcDoneMsg{results: skipped})
		return nil
	}
	m.rcRunning = true
	m.note(fmt.Sprintf("Remote Control: typing /remote-control into %s…", plural(len(targets), "session")))
	return func() tea.Msg {
		return rcDoneMsg{results: append(skipped, rcType(targets)...)}
	}
}

// rcType types into each target in turn, then waits for each typed one's
// session file to gain a bridgeSessionId.
func rcType(targets []rcTarget) []rcResult {
	out := make([]rcResult, len(targets))
	var typed []int
	for i, t := range targets {
		out[i] = rcResult{id: t.id, label: t.label}
		note, err := typeSlashNow(t.pid, t.id, t.label, rcCommand(t.name), t.unverified, func(f sessions.SessionFile) string {
			if f.BridgeSessionID != "" {
				return "Remote Control is already on in " + t.label
			}
			return ""
		})
		if err != nil {
			out[i].why, out[i].skipped = note, true
			continue
		}
		typed = append(typed, i)
	}
	deadline := time.Now().Add(rcConfirmWait)
	for len(typed) > 0 {
		var left []int
		for _, i := range typed {
			f, err := sessions.ReadSessionFile(targets[i].pid)
			switch {
			case err == nil && f.SessionID == targets[i].id && f.BridgeSessionID != "":
				out[i].on = true
				out[i].why = "✓ Remote Control on in " + targets[i].label + " as " + targets[i].name
			case err != nil || f.SessionID != targets[i].id:
				out[i].why = "⚠ Typed /remote-control into " + targets[i].label + ", but its session file is gone or changed"
			default:
				left = append(left, i)
			}
		}
		typed = left
		if len(typed) == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			for _, i := range typed {
				out[i].why = fmt.Sprintf("⚠ Typed /remote-control into %s, but no bridgeSessionId showed in its session file within %s: check its tab", targets[i].label, rcConfirmWait)
			}
			break
		}
		time.Sleep(rcConfirmEvery)
	}
	return out
}

// applyRC shows each session's outcome in its box, logs it, and sums it up.
func (m *model) applyRC(msg rcDoneMsg) {
	m.rcRunning = false
	on, skipped, unsure := 0, 0, 0
	var why []string
	for _, r := range msg.results {
		state.LogActivity("remote control: %s (%s): %s", r.label, r.id, transcript.CleanText(ansi.Strip(r.why)))
		note := r.why
		switch {
		case r.on:
			on++
		case r.skipped:
			skipped++
			note = grey(ansi.Strip(r.why))
			why = append(why, ansi.Strip(r.why))
		default:
			unsure++
		}
		m.boxNote(r.id, note)
	}
	if len(msg.results) == 1 {
		r := msg.results[0]
		if r.skipped {
			m.note("Remote Control not turned on: " + ansi.Strip(r.why))
		} else {
			m.note(ansi.Strip(r.why))
		}
		return
	}
	parts := []string{fmt.Sprintf("on in %d", on)}
	if unsure > 0 {
		parts = append(parts, fmt.Sprintf("%d typed but not confirmed", unsure))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (%s)", skipped, strings.Join(why, "; ")))
	}
	m.note("Remote Control: " + strings.Join(parts, ", "))
}

// plural is "1 session" or "3 sessions".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

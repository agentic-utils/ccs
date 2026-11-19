package tui

// ^X B with no agent installed: a setup popup instead of a note. It offers
// a read-only preview on this Mac now (`ccs serve --preview` as a child of
// this ccs, stopped when ccs quits, never a launchd agent), or a form for
// the phone that writes and starts the agent with the same checks as
// `ccs serve install` (mcp.ServeSetup). The form also edits an installed
// agent's setup (e in the ^X B popup). Write access is off unless ticked.

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// The setup popup's steps.
const (
	setupMenu = iota // try it here, or set it up for the phone
	setupForm        // the phone's form
	setupWarn        // the tunnel check found something missing: install anyway?
	setupBusy        // installing and starting
)

// The form's text fields, then the write box.
const (
	fieldHost = iota
	fieldTeam
	fieldAUD
	fieldEmail
	fieldWrite
	fieldCount
)

var setupLabels = [fieldCount]string{"Hostname", "Access team", "Access AUD tag", "Email", "Allow answering and sending from the phone"}

var setupHints = [fieldCount]string{
	"the hostname your tunnel serves, e.g. ccs.example.com (cloudflared tunnel route dns ccs <hostname>)",
	"your Zero Trust team, e.g. me or me.cloudflareaccess.com",
	"Zero Trust → Access → Applications → your ccs app → its AUD tag",
	"who may sign in, as your Access policy allows (commas between several)",
	"lets the phone send messages, answer questions and permission prompts, and launch, close and restart sessions",
}

// serveSetup is the open setup popup.
type serveSetup struct {
	step       int
	choice     int // on the menu: 0 try it here, 1 set up for the phone
	focus      int // the form's field
	fields     [fieldEmail + 1]textinput.Model
	write      bool
	extra      []string // flags the form doesn't show, kept from the plist
	editing    bool     // editing an installed agent's setup
	wasRunning bool     // it ran when editing began: restart it after
	err        string   // why the form was refused, inline
	warn       []string // what the tunnel check found missing
}

func newServeSetup(s mcp.ServeSetup, editing, wasRunning bool) *serveSetup {
	st := &serveSetup{editing: editing, wasRunning: wasRunning, write: s.Write, extra: s.Extra}
	for i, v := range []string{s.Host, s.Team, s.AUD, s.Email} {
		b := textinput.New()
		b.Prompt = ""
		b.CharLimit = 500
		b.SetValue(v)
		st.fields[i] = b
	}
	st.fields[0].Placeholder, st.fields[1].Placeholder = "ccs.example.com", "me"
	if editing {
		st.step = setupForm
	}
	st.focusField(0)
	return st
}

func (st *serveSetup) focusField(i int) {
	st.focus = (i + fieldCount) % fieldCount
	for j := range st.fields {
		if j == st.focus {
			st.fields[j].Focus()
			st.fields[j].CursorEnd()
		} else {
			st.fields[j].Blur()
		}
	}
}

func (st *serveSetup) setup() mcp.ServeSetup {
	v := func(i int) string { return strings.TrimSpace(st.fields[i].Value()) }
	return mcp.ServeSetup{Host: v(fieldHost), Team: v(fieldTeam), AUD: v(fieldAUD), Email: v(fieldEmail), Write: st.write, Extra: st.extra}
}

// Stubbed in tests.
var (
	installServeSetup = mcp.InstallServeSetup
	serveTunnelCheck  = mcp.ServeTunnelCheck
	readServeSetup    = mcp.ReadServeSetup
)

// openServeSetup opens the setup popup for a new agent: the menu, with the
// form's email prefilled from the Claude account.
func (m *model) openServeSetup() {
	m.setup = newServeSetup(mcp.ServeSetup{Email: accountEmail()}, false, false)
}

// editServeSetup opens the form prefilled from the installed agent's plist.
func (m *model) editServeSetup() {
	s, err := readServeSetup()
	m.setup = newServeSetup(s, true, m.serve.Running)
	if err != nil {
		m.setup.err = err.Error()
	}
}

type setupDoneMsg struct {
	text string
	err  error
	st   mcp.ServiceStatus
}

// setupInstallCmd writes the agent and starts it; editing, it restarts it
// only if it was running.
func setupInstallCmd(s mcp.ServeSetup, editing, wasRunning bool) tea.Cmd {
	return func() tea.Msg {
		text, err := installServeSetup(s)
		if err != nil {
			return setupDoneMsg{err: err, st: serveStatus()}
		}
		if editing && !wasRunning {
			return setupDoneMsg{text: text, st: serveStatus()}
		}
		if editing {
			if _, err := stopServe(); err != nil {
				return setupDoneMsg{err: err, st: serveStatus()}
			}
		}
		started, err := startServe()
		if err != nil {
			return setupDoneMsg{err: err, st: serveStatus()}
		}
		return setupDoneMsg{text: started, st: serveStatus()}
	}
}

// applySetupDone: the agent is written (and started): the ^X B popup shows
// its address, QR code and status. A failure goes back to the form.
func (m *model) applySetupDone(msg setupDoneMsg) tea.Cmd {
	m.serve, m.serveCheckedAt = msg.st, time.Now()
	if msg.err != nil {
		if m.setup != nil {
			m.setup.step, m.setup.err = setupForm, msg.err.Error()
		} else {
			m.errorMsg = msg.err.Error()
		}
		return nil
	}
	m.setup = nil
	m.note(msg.text)
	m.openServeQR()
	return nil
}

// serveSetupKey handles a key while the setup popup is open; it owns the
// keyboard.
func (m model) serveSetupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	st, k := m.setup, msg.String()
	if k == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}
	switch st.step {
	case setupBusy:
		return m, nil
	case setupMenu:
		switch k {
		case "esc", "q":
			m.setup = nil
		case "up", "k", "down", "j", "tab":
			st.choice = 1 - st.choice
		case "1":
			st.choice = 0
			return m, m.previewCmd()
		case "2":
			st.choice, st.step = 1, setupForm
		case "enter":
			if st.choice == 0 {
				return m, m.previewCmd()
			}
			st.step = setupForm
		}
		return m, nil
	case setupWarn:
		switch k {
		case "enter":
			st.step, st.warn = setupBusy, nil
			return m, setupInstallCmd(st.setup(), st.editing, st.wasRunning)
		case "esc":
			st.step, st.warn = setupForm, nil
		}
		return m, nil
	}
	// the form
	switch k {
	case "esc":
		if st.editing {
			m.setup = nil
		} else {
			st.step, st.err = setupMenu, ""
		}
		return m, nil
	case "tab", "down":
		st.focusField(st.focus + 1)
		return m, nil
	case "shift+tab", "up":
		st.focusField(st.focus - 1)
		return m, nil
	case "enter":
		if st.focus < fieldWrite {
			st.focusField(st.focus + 1)
			return m, nil
		}
		return m, m.submitSetup()
	case " ", "space":
		if st.focus == fieldWrite {
			st.write = !st.write
			return m, nil
		}
	}
	if st.focus < fieldWrite {
		var cmd tea.Cmd
		st.fields[st.focus], cmd = editKey(st.fields[st.focus], msg)
		st.err = ""
		return m, cmd
	}
	return m, nil
}

// submitSetup checks the form as `ccs serve install` does (errors inline),
// then the tunnel; anything missing there asks before installing.
func (m *model) submitSetup() tea.Cmd {
	st := m.setup
	s := st.setup()
	if err := mcp.CheckServeSetup(s); err != nil {
		st.err = err.Error()
		return nil
	}
	st.err = ""
	if warn := serveTunnelCheck(s); len(warn) > 0 {
		st.step, st.warn = setupWarn, warn
		return nil
	}
	st.step = setupBusy
	return setupInstallCmd(s, st.editing, st.wasRunning)
}

// serveSetupPopup draws the setup popup's current step.
func (m model) serveSetupPopup() string {
	st := m.setup
	dim := func(s string) string { return "\033[90m" + s + "\033[0m" }
	width := max(min(m.width-10, 90), 40)
	wrap := func(s string) string { return lipgloss.NewStyle().Width(width).Render(s) }
	var b strings.Builder
	switch st.step {
	case setupMenu:
		b.WriteString("\033[1mccs serve isn't set up yet\033[0m\n\n")
		for i, opt := range []struct{ title, desc string }{
			{"Try it on this Mac now", "a read-only preview in your browser, for as long as ccs runs. This Mac only: your phone can't reach 127.0.0.1."},
			{"Set up for my phone", "through a Cloudflare Tunnel with Cloudflare Access in front (docs/remote.md), run in the background by launchd"},
		} {
			mark := "  "
			if i == st.choice {
				mark = "\033[1;36m❯\033[0m "
			}
			b.WriteString(mark + "\033[1m" + string(rune('1'+i)) + ". " + opt.title + "\033[0m\n")
			b.WriteString(wrap("     "+dim(opt.desc)) + "\n")
		}
		if p := m.servePreview; p.link != "" || p.err != "" || p.starting {
			b.WriteString("\n")
			switch {
			case p.starting:
				b.WriteString(dim("starting the preview…") + "\n")
			case p.err != "":
				b.WriteString("\033[31m" + p.err + "\033[0m\n")
			default:
				b.WriteString("Preview running, opened in your browser (this Mac only):\n" + p.link + "\n")
			}
		}
		b.WriteString("\n" + dim(hints("1 2", "choose", "↑↓ "+keyEnter, "the same", keyEsc, "close")))
	case setupForm, setupBusy:
		title := "Set up ccs serve for your phone"
		if st.editing {
			title = "Edit ccs serve's setup"
		}
		b.WriteString("\033[1m" + title + "\033[0m\n\n")
		for i := 0; i < fieldCount; i++ {
			mark := "  "
			if i == st.focus {
				mark = "\033[1;36m❯\033[0m "
			}
			if i == fieldWrite {
				box := "[ ]"
				if st.write {
					box = "[x]"
				}
				b.WriteString(mark + box + " " + setupLabels[i] + "\n")
			} else {
				b.WriteString(mark + setupLabels[i] + ": " + st.fields[i].View() + "\n")
			}
			b.WriteString(wrap("    "+dim(setupHints[i])) + "\n")
		}
		if st.err != "" {
			b.WriteString("\n" + wrap("\033[31m"+st.err+"\033[0m") + "\n")
		}
		if st.step == setupBusy {
			b.WriteString("\n" + dim("installing and starting ccs serve…"))
		} else {
			submit := "submit"
			if st.focus < fieldWrite {
				submit = "next"
			}
			back := "back"
			if st.editing {
				back = "cancel"
			}
			b.WriteString("\n" + dim(hints(keyTab+" ↑↓", "move", "space", "tick", keyEnter, submit, keyEsc, back)))
		}
	case setupWarn:
		b.WriteString("\033[1mThe tunnel isn't ready for " + st.setup().Host + "\033[0m\n\n")
		for _, l := range st.warn {
			b.WriteString(wrap(l) + "\n")
		}
		b.WriteString("\n" + wrap(dim("ccs serve can be installed now and reached once the tunnel is set up (docs/remote.md).")) + "\n")
		b.WriteString("\n" + dim(hints(keyEnter, "install anyway", keyEsc, "back to the form")))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 2).
		Render(b.String())
}

// previewState is the preview child as the popup shows it.
type previewState struct {
	link, err string
	starting  bool
}

// previewArgs are the preview child's flags: read-only, never --allow-write.
var previewArgs = []string{"serve", "--preview"}

// runPreview starts `ccs <args>` as a child of this ccs and waits for its
// secret link; stop kills it, done closes when it exits. Stubbed in tests.
var runPreview = startPreviewChild

// The preview child, for as long as this ccs runs: stopPreview (at quit)
// kills it.
var (
	previewMu   sync.Mutex
	previewStop func()
	previewDone <-chan struct{}
	previewLink string
)

type previewMsg struct {
	link string
	err  error
}

// previewCmd starts the preview, or shows its link again while it runs.
func (m *model) previewCmd() tea.Cmd {
	previewMu.Lock()
	running := previewStop != nil && !closed(previewDone)
	link := previewLink
	previewMu.Unlock()
	if running {
		m.servePreview = previewState{link: link}
		return nil
	}
	if m.servePreview.starting {
		return nil
	}
	m.servePreview = previewState{starting: true}
	return func() tea.Msg {
		link, stop, done, err := runPreview(previewArgs)
		if err != nil {
			return previewMsg{err: err}
		}
		previewMu.Lock()
		previewStop, previewDone, previewLink = stop, done, link
		previewMu.Unlock()
		return previewMsg{link: link}
	}
}

func (m *model) applyPreview(msg previewMsg) {
	m.servePreview = previewState{link: msg.link}
	if msg.err != nil {
		m.servePreview = previewState{err: "the preview didn't start: " + msg.err.Error()}
	}
}

func closed(ch <-chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// stopPreview kills the preview child, if one runs; called when ccs quits.
func stopPreview() {
	previewMu.Lock()
	stop := previewStop
	previewStop, previewDone, previewLink = nil, nil, ""
	previewMu.Unlock()
	if stop != nil {
		stop()
	}
}

// startPreviewChild runs this ccs binary with args, reads its output for
// the preview's link (ccs serve opens it in the browser itself), and keeps
// draining it. It gives up after 20 seconds.
func startPreviewChild(args []string) (string, func(), <-chan struct{}, error) {
	self, err := os.Executable()
	if err != nil {
		return "", nil, nil, err
	}
	cmd := exec.Command(self, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, nil, err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return "", nil, nil, err
	}
	links := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); strings.HasPrefix(l, "http://") {
				select {
				case links <- l:
				default:
				}
			}
		}
	}()
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	stop := func() {
		cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	select {
	case link := <-links:
		return link, stop, done, nil
	case <-done:
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = "it exited"
		}
		return "", nil, nil, errors.New(msg)
	case <-time.After(20 * time.Second):
		stop()
		return "", nil, nil, errors.New("no link after 20 seconds")
	}
}

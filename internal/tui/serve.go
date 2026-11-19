package tui

// ^X B: start or stop ccs serve's launchd agent (`ccs serve install` writes
// it). The TUI never hosts ccs serve itself: it restarts on every
// auto-update and stops when it quits, and remote access mustn't. The
// header shows `^X B browser on` while launchd says the agent runs.

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// serveCheckEvery is the least time between reads of the agent's state.
const serveCheckEvery = 10 * time.Second

// serveStatus reads the agent's state; stubbed in tests.
var serveStatus = mcp.ServeServiceStatus

// startServe and stopServe run `ccs serve start|stop`; stubbed in tests.
var (
	startServe = mcp.StartServeService
	stopServe  = mcp.StopServeService
)

type serveStatusMsg struct{ st mcp.ServiceStatus }

type serveDoneMsg struct {
	text string
	err  error
}

// serveCheckCmd reads the agent's state off the UI goroutine, at most every
// serveCheckEvery; nil when it's not due.
func (m *model) serveCheckCmd() tea.Cmd {
	if m.serveChecking || time.Since(m.serveCheckedAt) < serveCheckEvery {
		return nil
	}
	m.serveChecking = true
	return func() tea.Msg { return serveStatusMsg{serveStatus()} }
}

// serveURL is the agent's address (its --host); stubbed in tests.
var serveURL = mcp.ServeURL

// serveKey is ^X B: the setup popup when there's no agent (serve_setup.go),
// the QR popup while it runs (S there asks y/n and stops it, E edits its
// setup), else it starts it.
func (m *model) serveKey() tea.Cmd {
	if !m.serve.Installed { // read it now: the last check may be old
		m.serve, m.serveCheckedAt = serveStatus(), time.Now()
	}
	switch {
	case !m.serve.Installed:
		m.openServeSetup()
		return nil
	case m.serve.Running:
		m.openServeQR()
		return nil
	}
	return func() tea.Msg {
		text, err := startServe()
		return serveDoneMsg{text, err}
	}
}

func stopServeCmd() tea.Cmd {
	return func() tea.Msg {
		text, err := stopServe()
		return serveDoneMsg{text, err}
	}
}

// applyServe handles the agent's messages.
func (m *model) applyServe(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case serveStatusMsg:
		m.serve, m.serveChecking, m.serveCheckedAt = msg.st, false, time.Now()
	case serveDoneMsg:
		if msg.err != nil {
			m.errorMsg = msg.err.Error()
		} else {
			m.note(msg.text)
		}
		m.serveCheckedAt = time.Time{} // read it again at once
		return m.serveCheckCmd()
	}
	return nil
}

// openServeQR opens the ^X B popup: the agent's address and its QR code.
func (m *model) openServeQR() {
	m.serveQROpen, m.serveQR, m.serveQRWidth, m.serveQRURL, m.serveQRErr = true, "", 0, "", ""
	url, err := serveURL()
	if err != nil {
		m.serveQRErr = err.Error()
		return
	}
	m.serveQRURL = url
	if text, w, err := mcp.QRCode(url); err == nil {
		m.serveQR, m.serveQRWidth = strings.TrimRight(text, "\n"), w
	}
}

// serveQRKey handles a key while the ^X B popup is open: S stops ccs serve
// (after the usual y/n), esc or ^X B closes it.
func (m model) serveQRKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "s", "S":
		m.serveQROpen, m.confirmServeStop = false, true
	case "e", "E":
		m.serveQROpen = false
		m.editServeSetup()
	case "esc", "^x b", "q":
		m.serveQROpen = false
	case "ctrl+x": // a chord can't start while a popup is open, so this one starts its own: ^X B closes it
		m.chordX = true
	}
	return m, nil
}

// serveQRPopup is the ^X B popup. The QR code is drawn only when it fits
// the window; otherwise the address alone, and a note saying so.
func (m model) serveQRPopup() string {
	var b strings.Builder
	status := "installed, not running"
	if m.serve.Running {
		status = "running"
		if m.serve.PID > 0 {
			status += " (pid " + strconv.Itoa(m.serve.PID) + ")"
		}
	}
	b.WriteString("\033[1mccs serve\033[0m \033[90m· " + status + "\033[0m\n\n")
	switch {
	case m.serveQRErr != "":
		b.WriteString(m.serveQRErr + "\n")
	case m.serveQR != "" && m.serveQRWidth+8 <= m.width && (m.serveQRWidth+1)/2+12 <= m.height:
		b.WriteString(m.serveQR + "\n\nScan it with your phone's camera, or open\n" + m.serveQRURL + "\n")
	default:
		b.WriteString("Open " + m.serveQRURL + "\n\n\033[90mthe window is too small for its QR code: make it bigger, or run ccs serve qr\033[0m\n")
	}
	b.WriteString("\n\033[90m" + hints("s", "stop it", "e", "edit setup", "^X B/"+keyEsc, "close") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 2).
		Render(b.String())
}

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
)

func chordB(m model) (model, tea.Cmd) {
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	return key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
}

// run applies cmd's message to m, as the program would.
func run(m model, cmd tea.Cmd) model {
	if cmd == nil {
		return m
	}
	res, _ := m.Update(cmd())
	return res.(model)
}

func TestServeChord(t *testing.T) {
	var started, stopped int
	oldStatus, oldStart, oldStop, oldURL := serveStatus, startServe, stopServe, serveURL
	t.Cleanup(func() { serveStatus, startServe, stopServe, serveURL = oldStatus, oldStart, oldStop, oldURL })
	serveURL = func() (string, error) { return "https://ccs.example.com/", nil }
	st := mcp.ServiceStatus{}
	serveStatus = func() mcp.ServiceStatus { return st }
	startServe = func() (string, error) { started++; st.Loaded, st.Running = true, true; return "started ccs serve", nil }
	stopServe = func() (string, error) {
		stopped++
		st.Loaded, st.Running = false, false
		return "stopped ccs serve", nil
	}

	m := initialModel(buildItems(nil), "", nil)
	m.width, m.height = 160, 30
	m = run(m, m.serveCheckCmd())

	// Not installed: a note on how to set it up, nothing started, no header toggle.
	m, cmd := chordB(m)
	if cmd != nil || started != 0 || !strings.Contains(m.errorMsg, "ccs serve install") || m.errorMsg != m.noteMsg {
		t.Errorf("no agent: a grey note and nothing run: %q %v", m.errorMsg, cmd)
	}
	if strings.Contains(strip2(m.View()), "browser") {
		t.Error("the header shows ^X B only once it's set up")
	}

	// Installed, not running: ^X B starts it, and the header says it's on.
	st.Installed = true
	m.serveCheckedAt = m.serveCheckedAt.AddDate(0, 0, -1)
	m = run(m, m.serveCheckCmd())
	if head := strip2(strings.Split(m.View(), "\n")[0]); !strings.Contains(head, "^X B browser off") {
		t.Errorf("installed but stopped: %q", head)
	}
	m, cmd = chordB(m)
	m = run(m, cmd) // serveDoneMsg, which reads the state again at once
	m = run(m, func() tea.Msg { return serveStatusMsg{serveStatus()} })
	if started != 1 || !m.serve.Running {
		t.Fatalf("started %d, running %v", started, m.serve.Running)
	}
	if head := strip2(strings.Split(m.View(), "\n")[0]); !strings.Contains(head, "^X B browser on") {
		t.Errorf("running: %q", head)
	}

	// Running: ^X B shows its address and QR code; esc closes it.
	m, cmd = chordB(m)
	view := strip2(m.View())
	if cmd != nil || !m.serveQROpen || !strings.Contains(view, "https://ccs.example.com/") || !strings.Contains(view, "▀") {
		t.Fatalf("running: the QR popup: open %v\n%s", m.serveQROpen, view)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.serveQROpen || stopped != 0 {
		t.Error("esc closes the popup and stops nothing")
	}
	// A window too small for the code shows the address alone.
	m.width, m.height = 60, 20
	m, _ = chordB(m)
	if v := strip2(m.View()); strings.Contains(v, "▀") || !strings.Contains(v, "too small for its QR code") || !strings.Contains(v, "https://ccs.example.com/") {
		t.Errorf("a small window:\n%s", v)
	}
	m, _ = chordB(m) // ^X B closes it too
	if m.serveQROpen {
		t.Error("^X B closes the popup")
	}
	m.width, m.height = 160, 30
	// s in the popup asks first; n keeps it, y stops it.
	m, _ = chordB(m)
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if cmd != nil || m.serveQROpen || !m.confirmServeStop || !strings.Contains(strip2(m.View()), "Stop ccs serve?") {
		t.Fatalf("stopping asks first")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.confirmServeStop || stopped != 0 {
		t.Errorf("n keeps it running")
	}
	m, _ = chordB(m)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = run(m, cmd)
	if stopped != 1 || m.confirmServeStop || !strings.Contains(m.errorMsg, "stopped ccs serve") {
		t.Errorf("y stops it: stopped %d, %q", stopped, m.errorMsg)
	}

	// A failure is shown as an error, not a note.
	startServe = func() (string, error) { return "", errors.New("launchctl bootstrap: boom") }
	st.Running = false
	m.serve.Running = false
	m, cmd = chordB(m)
	m = run(m, cmd)
	if !strings.Contains(m.errorMsg, "boom") || m.errorMsg == m.noteMsg {
		t.Errorf("a failed start is an error: %q", m.errorMsg)
	}
}

// The agent's state is read at most every serveCheckEvery, off the UI goroutine.
func TestServeCheckIsThrottled(t *testing.T) {
	m := initialModel(buildItems(nil), "", nil)
	if m.serveCheckCmd() == nil {
		t.Fatal("the first check runs")
	}
	if m.serveCheckCmd() != nil {
		t.Error("not a second one while the first is out")
	}
	m = run(m, func() tea.Msg { return serveStatusMsg{} })
	if m.serveCheckCmd() != nil {
		t.Error("not again within serveCheckEvery")
	}
}

package tui

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// setupStubs stubs the agent and the tunnel check: status st, install and
// start recorded, the tunnel all set unless warn says otherwise.
type setupStubs struct {
	st        mcp.ServiceStatus
	installed []mcp.ServeSetup
	calls     []string // start, stop
	warn      []string
}

func stubSetup(t *testing.T) *setupStubs {
	t.Helper()
	s := &setupStubs{}
	oldStatus, oldStart, oldStop, oldURL, oldInstall, oldCheck, oldEmail := serveStatus, startServe, stopServe, serveURL, installServeSetup, serveTunnelCheck, accountEmail
	t.Cleanup(func() {
		serveStatus, startServe, stopServe, serveURL, installServeSetup, serveTunnelCheck, accountEmail = oldStatus, oldStart, oldStop, oldURL, oldInstall, oldCheck, oldEmail
	})
	serveStatus = func() mcp.ServiceStatus { return s.st }
	serveURL = func() (string, error) { return "https://ccs.example.com/", nil }
	installServeSetup = func(ss mcp.ServeSetup) (string, error) {
		if err := mcp.CheckServeSetup(ss); err != nil {
			return "", err
		}
		s.installed = append(s.installed, ss)
		s.st.Installed = true
		return "wrote it", nil
	}
	startServe = func() (string, error) {
		s.calls = append(s.calls, "start")
		s.st.Loaded, s.st.Running, s.st.PID = true, true, 4321
		return "started ccs serve", nil
	}
	stopServe = func() (string, error) {
		s.calls = append(s.calls, "stop")
		s.st.Loaded, s.st.Running = false, false
		return "stopped", nil
	}
	serveTunnelCheck = func(mcp.ServeSetup) []string { return s.warn }
	accountEmail = func() string { return "me@example.com" }
	return s
}

func setupModel(t *testing.T) (model, *setupStubs) {
	t.Helper()
	s := stubSetup(t)
	m := initialModel(buildItems(nil), "", nil)
	m.width, m.height = 160, 50
	m, _ = chordB(m)
	if m.setup == nil {
		t.Fatal("^X B with no agent opens the setup popup")
	}
	return m, s
}

// flat is the screen as one line, the popup's border and wrapping gone.
func flat(m model) string {
	return strings.Join(strings.Fields(strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(strip2(m.View()))), " ")
}

func pressKey(m model, k tea.KeyType) model { m, _ = key(m, tea.KeyMsg{Type: k}); return m }

func typeText(m model, s string) model {
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return m
}

// fillForm opens the phone's form and fills its text fields, leaving the
// email as prefilled, on the write box at the end.
func fillForm(m model, host, team, aud string) model {
	m = typeText(m, "2")
	for _, v := range []string{host, team, aud} {
		m = typeText(m, v)
		m = pressKey(m, tea.KeyTab)
	}
	return pressKey(m, tea.KeyTab) // past the email
}

// No agent: ^X B opens the setup popup, a popup that owns the keyboard, with
// its two choices, the phone caveat, and the email prefilled.
func TestSetupPopupOpens(t *testing.T) {
	m, _ := setupModel(t)
	v := flat(m)
	for _, want := range []string{"ccs serve isn't set up yet", "Try it on this Mac now", "Set up for my phone", "your phone can't reach 127.0.0.1"} {
		if !strings.Contains(v, want) {
			t.Errorf("lacks %q:\n%s", want, v)
		}
	}
	if !slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open }) {
		t.Error("listed in overPopups")
	}
	m = typeText(m, "x")
	if m.textInput.Value() != "" {
		t.Error("the popup owns the keyboard")
	}
	m = typeText(m, "2")
	if got := m.setup.fields[fieldEmail].Value(); got != "me@example.com" {
		t.Errorf("email prefilled from the account: %q", got)
	}
	if v := flat(m); !strings.Contains(v, "[ ] Allow answering and sending from the phone") || !strings.Contains(v, "launch, close and restart sessions") || !strings.Contains(v, "AUD tag") {
		t.Errorf("the form:\n%s", v)
	}
}

// "Try it on this Mac now" runs ccs serve --preview (never --allow-write)
// as a child, shows its link, shows it again on a second press without a
// second child, and stops it when ccs quits.
func TestSetupPreviewChild(t *testing.T) {
	m, _ := setupModel(t)
	var runs [][]string
	stops := 0
	done := make(chan struct{})
	old := runPreview
	runPreview = func(args []string, started func(func(), <-chan struct{})) (string, error) {
		runs = append(runs, args)
		started(func() { stops++ }, done)
		return "http://127.0.0.1:7467/p/secret/", nil
	}
	resetPreview(t)
	t.Cleanup(func() { runPreview = old })
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	m = run(m, cmd)
	if len(runs) != 1 || !slices.Equal(runs[0], []string{"serve", "--preview", "--parent-pid", strconv.Itoa(os.Getpid())}) || slices.Contains(runs[0], "--allow-write") {
		t.Fatalf("one read-only preview child: %q", runs)
	}
	if v := strip2(m.View()); !strings.Contains(v, "http://127.0.0.1:7467/p/secret/") || !strings.Contains(v, "this Mac only") {
		t.Errorf("its link shows:\n%s", v)
	}
	m = pressKey(m, tea.KeyEsc)
	m, _ = chordB(m)
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter}) // the first choice: shows the link again
	m = run(m, cmd)
	if len(runs) != 1 || !strings.Contains(strip2(m.View()), "/p/secret/") {
		t.Errorf("a second press shows the link again, no second child: %d", len(runs))
	}
	stopPreview()
	if stops != 1 {
		t.Errorf("quitting stops it: %d", stops)
	}
}

// A preview that fails says why in the popup.
func TestSetupPreviewFails(t *testing.T) {
	m, _ := setupModel(t)
	old := runPreview
	runPreview = func([]string, func(func(), <-chan struct{})) (string, error) {
		return "", errors.New("listen tcp 127.0.0.1:7467: address already in use")
	}
	resetPreview(t)
	t.Cleanup(func() { runPreview = old })
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if m = run(m, cmd); !strings.Contains(strip2(m.View()), "address already in use") {
		t.Errorf("the error shows:\n%s", strip2(m.View()))
	}
}

// The form is checked by ccs serve install's own checks, errors inline,
// nothing installed.
func TestSetupFormValidation(t *testing.T) {
	m, s := setupModel(t)
	m = fillForm(m, "https://ccs.example.com/", "me", "")
	m = pressKey(m, tea.KeyEnter)
	if v := strip2(m.View()); !strings.Contains(v, "all required") || len(s.installed) != 0 {
		t.Fatalf("a missing AUD tag:\n%s", v)
	}
	m = pressKey(m, tea.KeyUp) // the email
	m = pressKey(m, tea.KeyUp) // the AUD tag
	m = typeText(m, "abc")
	m = pressKey(m, tea.KeyTab)
	m = pressKey(m, tea.KeyTab)
	m = pressKey(m, tea.KeyEnter)
	if v := strip2(m.View()); !strings.Contains(v, "bare hostname") || len(s.installed) != 0 {
		t.Errorf("a hostname with a scheme:\n%s", v)
	}
}

// The default form installs read-only and starts it, then shows the ^X B
// popup with its address, QR code and status; ticking the box adds
// --allow-write.
func TestSetupInstallsReadOnlyByDefault(t *testing.T) {
	m, s := setupModel(t)
	m = fillForm(m, "ccs.example.com", "me", "abc123")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = run(m, cmd)
	if len(s.installed) != 1 || slices.Contains(s.installed[0].Args(), "--allow-write") || !slices.Equal(s.calls, []string{"start"}) {
		t.Fatalf("installed read-only and started: %+v %v", s.installed, s.calls)
	}
	if want := []string{"--host=ccs.example.com", "--team=me", "--aud=abc123", "--email=me@example.com"}; !slices.Equal(s.installed[0].Args(), want) {
		t.Errorf("flags %q", s.installed[0].Args())
	}
	v := strip2(m.View())
	if m.setup != nil || !m.serveQROpen || !strings.Contains(v, "https://ccs.example.com/") || !strings.Contains(v, "running (pid 4321)") {
		t.Errorf("the ^X B popup with its status:\n%s", v)
	}

	m, s = setupModel(t)
	m = fillForm(m, "ccs.example.com", "me", "abc123")
	m = typeText(m, " ")
	if !strings.Contains(strip2(m.View()), "[x] Allow answering") {
		t.Error("space ticks it")
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = run(m, cmd)
	if len(s.installed) != 1 || !slices.Contains(s.installed[0].Args(), "--allow-write") {
		t.Errorf("ticked: %+v", s.installed)
	}
}

// A tunnel that isn't set up warns with the commands, before installing;
// Esc goes back to the form, Enter installs anyway.
func TestSetupTunnelWarning(t *testing.T) {
	m, s := setupModel(t)
	s.warn = []string{"cloudflared isn't on PATH. Install it and log in:", "  brew install cloudflared"}
	m = fillForm(m, "ccs.example.com", "me", "abc123")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || len(s.installed) != 0 || !strings.Contains(strip2(m.View()), "brew install cloudflared") || !strings.Contains(strip2(m.View()), "install anyway") {
		t.Fatalf("warned first:\n%s", strip2(m.View()))
	}
	m = pressKey(m, tea.KeyEsc)
	if m.setup == nil || m.setup.step != setupForm {
		t.Fatal("esc goes back to the form")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = run(m, cmd)
	if len(s.installed) != 1 || !slices.Equal(s.calls, []string{"start"}) {
		t.Errorf("enter installs anyway: %+v %v", s.installed, s.calls)
	}
}

// e in the ^X B popup opens the form prefilled from the plist; saving
// reinstalls and restarts the running agent.
func TestSetupEditFromThePlist(t *testing.T) {
	s := stubSetup(t)
	dir := t.TempDir()
	oldDir := mcp.LaunchAgentsDir
	mcp.LaunchAgentsDir = func() string { return dir }
	t.Cleanup(func() { mcp.LaunchAgentsDir = oldDir })
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>` + mcp.ServeLabel + `</string><key>ProgramArguments</key><array>
<string>/bin/ccs</string><string>serve</string><string>--host=ccs.example.com</string><string>--team=me</string><string>--aud=a1</string><string>--email=me@example.com</string><string>--allow-write</string><string>--addr=127.0.0.1:9000</string>
</array></dict></plist>`
	if err := os.WriteFile(mcp.ServePlistPath(), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	s.st = mcp.ServiceStatus{Installed: true, Loaded: true, Running: true, PID: 7}
	m := initialModel(buildItems(nil), "", nil)
	m.width, m.height = 160, 50
	m = run(m, m.serveCheckCmd())
	m, _ = chordB(m)
	if !m.serveQROpen || !strings.Contains(strip2(m.View()), "e edit setup") {
		t.Fatalf("the ^X B popup offers e:\n%s", strip2(m.View()))
	}
	m = typeText(m, "e")
	if m.serveQROpen || m.setup == nil || !m.setup.editing {
		t.Fatal("e opens the form")
	}
	st := m.setup
	if st.fields[fieldHost].Value() != "ccs.example.com" || st.fields[fieldTeam].Value() != "me" || st.fields[fieldAUD].Value() != "a1" || st.fields[fieldEmail].Value() != "me@example.com" || !st.write {
		t.Fatalf("prefilled from the plist: %+v", st.setup())
	}
	if v := strip2(m.View()); !strings.Contains(v, "Edit ccs serve's setup") || !strings.Contains(v, "[x] Allow") {
		t.Errorf("the edit form:\n%s", v)
	}
	for range fieldWrite {
		m = pressKey(m, tea.KeyTab)
	}
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = run(m, cmd)
	if len(s.installed) != 1 || !slices.Contains(s.installed[0].Args(), "--addr=127.0.0.1:9000") || !slices.Contains(s.installed[0].Args(), "--allow-write") {
		t.Errorf("reinstalled with its flags kept: %+v", s.installed)
	}
	if !slices.Equal(s.calls, []string{"stop", "start"}) {
		t.Errorf("the running agent restarted: %v", s.calls)
	}
}

// resetPreview starts a test with no preview child and ccs not quitting.
func resetPreview(t *testing.T) {
	t.Helper()
	previewMu.Lock()
	previewStop, previewDone, previewLink, previewQuit = nil, nil, "", false
	previewMu.Unlock()
	t.Cleanup(func() {
		stopPreview()
		previewMu.Lock()
		previewQuit = false
		previewMu.Unlock()
	})
}

// Quitting while the preview child is still starting stops it: it's
// recorded before the wait for its link.
func TestSetupPreviewQuitDuringStartup(t *testing.T) {
	m, _ := setupModel(t)
	stopped := make(chan struct{})
	release := make(chan struct{})
	old := runPreview
	runPreview = func(args []string, started func(func(), <-chan struct{})) (string, error) {
		started(func() { close(stopped) }, make(chan struct{}))
		<-release // still waiting for its link
		return "", errors.New("stopped")
	}
	resetPreview(t)
	t.Cleanup(func() { runPreview = old })
	_, cmd := key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	go cmd()
	time.Sleep(20 * time.Millisecond)
	stopPreview() // ccs quits
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("a quit during startup left the child running")
	}
	close(release)

	// A child that only starts after the quit is stopped at once.
	late := false
	previewStarted(func() { late = true }, nil)
	if !late {
		t.Error("a child starting after the quit must be stopped")
	}
}

// What isn't a key (a Ctrl+V paste's result) goes to the box that has the
// keyboard, never one hidden under a popup or form.
func TestPasteGoesToTheBoxWithTheKeyboard(t *testing.T) {
	m, _ := setupModel(t)
	m = pressKey(m, tea.KeyDown)
	m = pressKey(m, tea.KeyEnter) // the phone's form
	if m.setup == nil || m.setup.step != setupForm {
		t.Fatalf("the form: %+v", m.setup)
	}
	m.setup.focus = fieldAUD
	if in := m.keyboardBox(); in != &m.setup.fields[fieldAUD] {
		t.Error("the setup form's focused field has the paste")
	}
	m.setup.focus = fieldWrite
	if in := m.keyboardBox(); in != nil || !m.keyboardOwned() {
		t.Error("on the tick box nothing takes a paste, and nothing beneath does either")
	}
	before := m.textInput.Value()
	res, _ := m.Update(struct{ fake string }{"pasted"}) // a message that isn't a key
	if res.(model).textInput.Value() != before {
		t.Error("the search box under the form changed")
	}
	m.setup = nil
	m.linksOpen = true
	if in := m.keyboardBox(); in != &m.linksInput {
		t.Error("the links filter has the paste while its popup is open")
	}
	m.linksOpen, m.serveQROpen = false, true
	if in := m.keyboardBox(); in != nil || !m.keyboardOwned() {
		t.Error("the QR popup has no box, but owns the keyboard")
	}
}

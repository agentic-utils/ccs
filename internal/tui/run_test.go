package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// boxModel is chatModel with the live session selected, its message box
// focused and its folder dir; its session file says status.
func boxModel(t *testing.T, status, dir string) model {
	t.Helper()
	oldTick := runTick
	runTick = func(int) tea.Cmd { return nil }
	t.Cleanup(func() { runTick = oldTick })
	m := chatModel(t)
	m.selectSession("live")
	m.chatFocus = true
	m.chatTickOn = true // no fast refresh timer either
	for i := range m.filtered {
		if m.filtered[i].conv.SessionID == "live" {
			m.filtered[i].conv.Cwd = dir
		}
	}
	m.chatStatus = map[string]sessions.SessionStat{"live": {Status: status}}
	pid := os.Getpid()
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", pid)),
		[]byte(fmt.Sprintf(`{"sessionId":"live","status":%q,"messagingSocketPath":"/tmp/ccs-no-such.sock"}`, status)), 0o600)
	return m
}

// enter types text into the box and presses Enter.
func enter(m model, text string) (model, tea.Cmd) {
	m.chatInput.SetValue(text)
	return key(m, tea.KeyMsg{Type: tea.KeyEnter})
}

// work runs every command cmd holds at once and returns the run's or the
// typing's result. boxModel stubs out the timers, so nothing else waits.
func work(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	got := make(chan tea.Msg, 16)
	var start func(tea.Cmd)
	start = func(c tea.Cmd) {
		go func() {
			msg := c()
			if b, ok := msg.(tea.BatchMsg); ok {
				for _, c := range b {
					if c != nil {
						start(c)
					}
				}
				return
			}
			switch msg.(type) {
			case runDoneMsg, sendDoneMsg:
				got <- msg
			}
		}()
	}
	start(cmd)
	select {
	case msg := <-got:
		return msg
	case <-time.After(20 * time.Second):
		t.Fatal("no result")
		return nil
	}
}

func update(m model, msg tea.Msg) model {
	res, _ := m.Update(msg)
	return res.(model)
}

func TestSlashIsTypedNotSent(t *testing.T) {
	m := boxModel(t, "idle", t.TempDir())
	log := fakeTerminal(t)
	m, cmd := enter(m, "/compact")
	if m.chatInput.Value() != "" || m.pending["live"] != "" {
		t.Fatalf("a slash command is typed, not sent as a message: box %q pending %q", m.chatInput.Value(), m.pending["live"])
	}
	m = update(m, work(t, cmd))
	got, _ := os.ReadFile(log)
	if string(got) != "send-keys|-t|main:0.1|-l|--|/compact|\nsend-keys|-t|main:0.1|Enter|\n" {
		t.Errorf("tmux calls:\n%s", got)
	}
	if !strings.Contains(m.sendNote["live"], "Typed /compact into") {
		t.Errorf("note %q", m.sendNote["live"])
	}
}

func TestSlashRefusedUnlessIdleAndReachable(t *testing.T) {
	log := fakeTerminal(t)
	for _, c := range []struct {
		name, status, want string
		prep               func(*model)
	}{
		{"busy", "busy", "/compact needs Busy one idle; it's busy", nil},
		{"on a menu", "waiting", "it's waiting on a menu", nil},
		{"held by ccs", "idle", "ccs holds a question", func(m *model) { m.heldPrompt = map[string]mcp.HeldPrompt{"live": {ID: "p1"}} }},
		{"codex", "idle", "Codex session", func(m *model) {
			for i := range m.filtered {
				m.filtered[i].conv.Kind = transcript.KindCodex
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := boxModel(t, c.status, t.TempDir())
			if c.prep != nil {
				c.prep(&m)
			}
			text := "/compact"
			m, _ = enter(m, text)
			if m.sending {
				t.Fatal("nothing is typed")
			}
			if m.chatInput.Value() != text || !strings.Contains(m.sendNote["live"], c.want) {
				t.Errorf("kept %q, note %q", m.chatInput.Value(), m.sendNote["live"])
			}
		})
	}
	if got, _ := os.ReadFile(log); len(got) != 0 {
		t.Errorf("typed: %s", got)
	}
}

// The box never holds a control character, but typeSlash refuses one anyway.
func TestSlashRefusesControlCharacters(t *testing.T) {
	m := boxModel(t, "idle", t.TempDir())
	if cmd := m.typeSlash(m.filtered[m.cursor].conv, "/compact\x1b[2J"); cmd != nil || m.sending || !strings.Contains(m.sendNote["live"], "control characters") {
		t.Errorf("note %q", m.sendNote["live"])
	}
}

// No terminal ccs can reach: nothing is typed and the text goes back to the box.
func TestSlashWithNoTerminalGivesTheTextBack(t *testing.T) {
	m := boxModel(t, "idle", t.TempDir())
	old := sessions.FindTerminal
	sessions.FindTerminal = func(int) sessions.Terminal { return sessions.Terminal{} }
	t.Cleanup(func() { sessions.FindTerminal = old })
	m, cmd := enter(m, "/model sonnet")
	m = update(m, work(t, cmd))
	if m.chatInput.Value() != "/model sonnet" || !strings.Contains(m.sendNote["live"], "isn't in iTerm or tmux") {
		t.Errorf("box %q note %q", m.chatInput.Value(), m.sendNote["live"])
	}
}

func TestDoubledPrefixSendsTheLine(t *testing.T) {
	for in, want := range map[string]string{"//x": "/x", "!!x": "!x"} {
		m := boxModel(t, "idle", t.TempDir())
		m, _ = enter(m, in)
		if m.pending["live"] != want || m.run != nil {
			t.Errorf("%s: sent %q, run %v", in, m.pending["live"], m.run)
		}
	}
}

func TestBangRunsInTheSessionFolder(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	m := boxModel(t, "busy", dir) // a busy session doesn't matter: it never sees it
	m, cmd := enter(m, "!echo hi; pwd")
	if m.run == nil || !m.run.running || m.chatInput.Value() != "" || m.pending["live"] != "" {
		t.Fatalf("run %+v box %q pending %q", m.run, m.chatInput.Value(), m.pending["live"])
	}
	if v := strip2(m.chatView()); !strings.Contains(v, "$ echo hi; pwd") || !strings.Contains(v, "running… 0s") {
		t.Errorf("while running:\n%s", v)
	}
	m = update(m, work(t, cmd))
	v := strip2(m.chatView())
	if out := string(m.run.res.Out); out != "hi\n"+dir+"\n" {
		t.Errorf("output %q, want it run in %s", out, dir)
	}
	for _, want := range []string{"$ echo hi; pwd", "hi", "exit 0 · 0.", "s · esc closes"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in:\n%s", want, v)
		}
	}
	if m.chatRows() <= 4 || !m.chatFocus {
		t.Errorf("the box sits above the message box, which keeps the keys: rows %d focus %v", m.chatRows(), m.chatFocus)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.run != nil || !m.chatFocus {
		t.Error("esc closes the box, keeping the message box")
	}
}

func TestBangNonZeroExitIsRed(t *testing.T) {
	m := boxModel(t, "idle", t.TempDir())
	m, cmd := enter(m, "!echo oops >&2; exit 3")
	m = update(m, work(t, cmd))
	v := m.chatView()
	if !strings.Contains(v, "\033[31mexit 3\033[0m") || !strings.Contains(strip2(v), "oops") {
		t.Errorf("non-zero exit:\n%q", v)
	}
}

// pidIn reads the pid a command wrote, and whether it still runs.
func pidAlive(t *testing.T, file string) bool {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func TestBangTimeoutKillsIt(t *testing.T) {
	old := runTimeout
	runTimeout = 300 * time.Millisecond
	t.Cleanup(func() { runTimeout = old })
	dir := t.TempDir()
	m := boxModel(t, "idle", dir)
	m, cmd := enter(m, "!sleep 30 & echo $! > child; wait")
	start := time.Now()
	m = update(m, work(t, cmd))
	if time.Since(start) > 5*time.Second || !errors.Is(m.run.err, proc.ErrTimedOut) {
		t.Fatalf("took %s err %v", time.Since(start), m.run.err)
	}
	if !strings.Contains(strip2(m.chatView()), "timed out after") {
		t.Errorf("view:\n%s", strip2(m.chatView()))
	}
	if pidAlive(t, filepath.Join(dir, "child")) {
		t.Error("what it started is killed with it")
	}
}

func TestBangEscStopsIt(t *testing.T) {
	dir := t.TempDir()
	m := boxModel(t, "idle", dir)
	m, cmd := enter(m, "!sleep 30 & echo $! > child; wait")
	done := make(chan tea.Msg, 1)
	go func() { done <- work(t, cmd) }()
	for range 100 { // until it has started its child
		if _, err := os.Stat(filepath.Join(dir, "child")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if m.quitting {
		t.Fatal("ctrl+c stops the command, not ccs")
	}
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still running after ctrl+c")
	}
	m = update(m, msg)
	if !errors.Is(m.run.err, context.Canceled) || !strings.Contains(strip2(m.chatView()), "stopped") {
		t.Errorf("err %v view:\n%s", m.run.err, strip2(m.chatView()))
	}
	if pidAlive(t, filepath.Join(dir, "child")) {
		t.Error("what it started is killed with it")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.run != nil {
		t.Error("esc then closes it")
	}
}

func TestBangOneAtATime(t *testing.T) {
	m := boxModel(t, "idle", t.TempDir())
	m.run = &shellRun{id: "live", running: true, cancel: func() {}}
	m, _ = enter(m, "!ls")
	if !m.run.running || m.run.line != "" || m.chatInput.Value() != "!ls" || !strings.Contains(m.sendNote["live"], "still running") {
		t.Errorf("second run: box %q note %q", m.chatInput.Value(), m.sendNote["live"])
	}
}

func TestRunOutputCappedAndCleaned(t *testing.T) {
	res, err := proc.RunShell(context.Background(), t.TempDir(), "/bin/sh", "head -c 5000 /dev/zero | tr '\\0' x", time.Minute, 1000)
	if err != nil || len(res.Out) != 1000 || !res.Cut || res.Code != 0 {
		t.Fatalf("len %d cut %v code %d err %v", len(res.Out), res.Cut, res.Code, err)
	}
	m := boxModel(t, "idle", t.TempDir())
	m.run = &shellRun{id: "live", line: "x", res: proc.ShellResult{Out: []byte("a\x1b[2Jb\x1b]8;;http://x\x07c\n"), Cut: true}}
	v := m.chatView()
	if strings.Contains(v, "\x1b[2J") || strings.Contains(v, "\x1b]8") || !strings.Contains(strip2(v), "… cut at 64KB") {
		t.Errorf("view:\n%q", v)
	}
}

func TestRunScrollsWithPageKeys(t *testing.T) {
	m := boxModel(t, "idle", t.TempDir())
	var lines []string
	for i := range 100 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	m.run = &shellRun{id: "live", line: "seq", res: proc.ShellResult{Out: []byte(strings.Join(lines, "\n"))}}
	if rows := len(m.runLines()); rows > m.runLimit() {
		t.Errorf("capped at %d rows: %d", m.runLimit(), rows)
	}
	if v := strip2(m.chatView()); !strings.Contains(v, "line 0") || strings.Contains(v, "line 99") || !strings.Contains(v, "of 100") {
		t.Errorf("first page:\n%s", v)
	}
	scroll := m.previewScroll
	for range 20 {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyPgDown})
	}
	if v := strip2(m.chatView()); !strings.Contains(v, "line 99") || m.previewScroll != scroll {
		t.Errorf("pgdown scrolls the output, not the preview:\n%s", v)
	}
}

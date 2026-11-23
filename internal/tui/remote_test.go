package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// rcModel is a model with "live" selected, idle and named rc-test, its
// session file in place; typed records what the stubbed terminal got, and
// onType runs as the keys land (a session turning Remote Control on).
func rcModel(t *testing.T, file string) (m model, typed *[]string, onType *func()) {
	t.Helper()
	m = chatModel(t)
	m.selectSession("live")
	m.chatStatus = map[string]sessions.SessionStat{"live": {Status: "idle", Name: "rc-test"}}
	path := filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", os.Getpid()))
	if file == "" {
		file = `{"sessionId":"live","status":"idle","name":"rc-test"}`
	}
	os.WriteFile(path, []byte(file), 0o600)
	var got []string
	hook := func() {
		os.WriteFile(path, []byte(`{"sessionId":"live","status":"idle","name":"rc-test","bridgeSessionId":"b1"}`), 0o600)
	}
	oldFind, oldSend, oldHeld, oldWait, oldEvery := sessions.FindTerminal, sessions.SendKeys, heldFor, rcConfirmWait, rcConfirmEvery
	sessions.FindTerminal = func(int) sessions.Terminal { return sessions.Terminal{Kind: "tmux pane", Target: "%1"} }
	sessions.SendKeys = func(_ sessions.Terminal, text string, enter bool) error {
		if !enter {
			t.Error("typed without Enter")
		}
		got = append(got, text)
		if hook != nil {
			hook()
		}
		return nil
	}
	heldFor = func(string) bool { return false }
	rcConfirmWait, rcConfirmEvery = 200*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		sessions.FindTerminal, sessions.SendKeys, heldFor, rcConfirmWait, rcConfirmEvery = oldFind, oldSend, oldHeld, oldWait, oldEvery
	})
	return m, &got, &hook
}

// runRC presses ^X V then k and applies what the worker returns.
func runRC(t *testing.T, m model, k string) model {
	t.Helper()
	m, _ = chord(m, "v")
	if !m.rcOpen {
		t.Fatal("^X V opens the popup")
	}
	if v := strip2(m.View()); !strings.Contains(v, "Remote Control") {
		t.Fatalf("popup not drawn:\n%s", v)
	}
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	if m.rcOpen {
		t.Fatal("the popup closes")
	}
	if cmd != nil {
		m = update(m, cmd())
	}
	return m
}

func TestRemoteControlTypesAndConfirms(t *testing.T) {
	m, typed, _ := rcModel(t, "")
	m = runRC(t, m, "t")
	if len(*typed) != 1 || (*typed)[0] != "/remote-control rc-test" {
		t.Fatalf("typed %q", *typed)
	}
	if !strings.Contains(m.sendNote["live"], "Remote Control on") || !strings.Contains(m.noteMsg, "Remote Control on") {
		t.Errorf("box %q note %q", m.sendNote["live"], m.noteMsg)
	}
	if m.rcRunning {
		t.Error("still marked running")
	}
	log, _ := os.ReadFile(state.ActivityLogPath)
	if !strings.Contains(string(log), "remote control: ") {
		t.Errorf("not logged: %s", log)
	}
}

// Typed, but no bridgeSessionId appears: ccs says so.
func TestRemoteControlUnconfirmed(t *testing.T) {
	m, typed, hook := rcModel(t, "")
	*hook = nil
	m = runRC(t, m, "t")
	if len(*typed) != 1 || !strings.Contains(m.sendNote["live"], "no bridgeSessionId") {
		t.Errorf("typed %q box %q", *typed, m.sendNote["live"])
	}
}

// Never typed when anything says no, on the key or just before the keys.
func TestRemoteControlRefusals(t *testing.T) {
	for _, c := range []struct {
		name, want string
		file       string
		set        func(m *model)
	}{
		{"already on", "already on", "", func(m *model) {
			m.chatStatus["live"] = sessions.SessionStat{Status: "idle", Name: "rc-test", Remote: true}
		}},
		{"turned on before the keys", "already on", `{"sessionId":"live","status":"idle","bridgeSessionId":"b0"}`, nil},
		{"busy", "it's busy", "", func(m *model) {
			m.chatStatus["live"] = sessions.SessionStat{Status: "busy", Name: "rc-test"}
		}},
		{"waiting", "waiting on a menu", "", func(m *model) {
			m.chatStatus["live"] = sessions.SessionStat{Status: "waiting", Name: "rc-test"}
		}},
		{"turned busy before the keys", "it's busy", `{"sessionId":"live","status":"busy"}`, nil},
		{"another session", "session has changed", `{"sessionId":"other","status":"idle"}`, nil},
		{"held", "holds a question", "", func(m *model) {
			heldFor = func(string) bool { return true }
		}},
		{"background", "background", "", func(m *model) { m.bg = map[string]sessions.BgSession{"live": {}} }},
		{"app", "which ccs can't type into", "", func(m *model) {
			m.hosts = map[string]string{"live": "Some App"}
		}},
		{"no name", "no ccs name", "", func(m *model) {
			m.chatStatus["live"] = sessions.SessionStat{Status: "idle", Name: "has space"}
			for i := range m.filtered {
				m.filtered[i].conv.Title = "Busy one"
			}
		}},
		{"codex", "Codex", "", func(m *model) {
			for i := range m.filtered {
				m.filtered[i].conv.Kind = transcript.KindCodex
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, typed, _ := rcModel(t, c.file)
			if c.set != nil {
				c.set(&m)
			}
			m = runRC(t, m, "t")
			if len(*typed) != 0 || !strings.Contains(m.sendNote["live"], c.want) {
				t.Errorf("typed %q box %q", *typed, m.sendNote["live"])
			}
		})
	}
}

// a does every running Claude Code session that can take it, and says what
// it skipped.
func TestRemoteControlAll(t *testing.T) {
	m, typed, _ := rcModel(t, "")
	m.live["idle"] = true // running, but no process ccs can check
	m = runRC(t, m, "a")
	if len(*typed) != 1 || !strings.Contains(m.noteMsg, "on in 1") || !strings.Contains(m.noteMsg, "1 skipped") {
		t.Errorf("typed %q note %q", *typed, m.noteMsg)
	}
}

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ^X A offers the installed agents other than the session's own, asks
// first, and y switches it by name and id to the one chosen.
func TestSwitchFromTheList(t *testing.T) {
	oldSwitch, oldLook := switchSession, sessions.LookBin
	t.Cleanup(func() { switchSession, sessions.LookBin = oldSwitch, oldLook })
	t.Setenv("TMUX", "/tmp/tmux-test,1,0")
	sessions.LookBin = func(bin string) (string, error) {
		if bin == "agy" {
			return "", errors.New("not installed")
		}
		return bin, nil
	}
	var got []sessions.SwitchOpts
	switchSession = func(o sessions.SwitchOpts) (string, error) {
		got = append(got, o)
		return "switched " + o.Name, nil
	}
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "s1", Title: "one", LastTimestamp: "2026-11-03T10:00:00Z"}}), "", nil)
	m.width, m.height = 160, 30
	m, cmd := chord(m, strings.ToLower(switchLetter))
	if cmd != nil || m.confirmSwitch == nil {
		t.Fatalf("^X A asks first: %q", m.errorMsg)
	}
	if p := m.confirmSwitch; len(p.agents) != 1 || p.agents[0].ID != transcript.KindCodex || p.name != "one" {
		t.Fatalf("only Codex: not its own agent, nor agy, which isn't installed: %+v", p)
	}
	if v := m.View(); !strings.Contains(v, `Switch "one" to another agent?`) || !strings.Contains(v, "not its memory") {
		t.Fatal("the question says the new one gets a brief, not its memory")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.confirmSwitch != nil || len(got) != 0 {
		t.Fatal("esc cancels it")
	}
	m, _ = chord(m, strings.ToLower(switchLetter))
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil || m.confirmSwitch != nil {
		t.Fatal("y switches it")
	}
	res, _ := m.Update(cmd())
	m = res.(model)
	if len(got) != 1 || got[0].Name != "one" || got[0].ID != "s1" || got[0].To != transcript.KindCodex || !strings.Contains(m.errorMsg, "switched one") {
		t.Errorf("switched by name and id: %+v, %q", got, m.errorMsg)
	}

	// A Codex session mid-turn asks in red, and y then forces it.
	m = initialModel(buildItems([]transcript.Conversation{{SessionID: "c1", Title: "cx", Kind: transcript.KindCodex, LastTimestamp: "2026-11-03T10:00:00Z"}}), "", nil)
	m.width, m.height = 160, 30
	m.live, m.livePIDs = map[string]bool{"c1": true}, map[string]int{"c1": 999999}
	m.chatStatus = map[string]sessions.SessionStat{"c1": {Status: "busy", Name: "cx"}}
	m, _ = chord(m, strings.ToLower(switchLetter))
	if p := m.confirmSwitch; p == nil || !p.force || p.agents[0].ID != "claude" || !strings.Contains(m.View(), "mid-turn") {
		t.Fatalf("mid-turn, it says so: %+v", p)
	}
}

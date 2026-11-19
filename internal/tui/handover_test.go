package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// ^X W refuses a session with no running parent at once; otherwise it asks
// first, naming the parent, and only y hands it over (by name and session
// id). Esc cancels.
func TestHandoverFromTheList(t *testing.T) {
	oldP, oldH := handoverParent, closeHandover
	t.Cleanup(func() { handoverParent, closeHandover = oldP, oldH })
	parent := ""
	handoverParent = func(name string) (string, error) {
		if parent == "" {
			return "", errors.New(name + " has no running parent: use ^X K to close it, or adopt it first")
		}
		return parent, nil
	}
	type call struct{ name, id string }
	var got []call
	closeHandover = func(name, id string, _ time.Duration, _ func(int)) (string, error) {
		got = append(got, call{name, id})
		return "closed " + name + "\nhanded over to lead", nil
	}
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "s1", Title: "one", LastTimestamp: "2026-11-03T10:00:00Z"}}), "", nil)
	m.width, m.height = 120, 30
	m, cmd := chord(m, "w")
	if cmd != nil || m.confirmHandover != "" || !strings.Contains(m.errorMsg, "^X W hands") {
		t.Fatalf("a session that isn't running can't be handed over: %q", m.errorMsg)
	}
	m.errorMsg = ""
	m.live = map[string]bool{"s1": true}
	m.livePIDs = map[string]int{"s1": 999999}
	m, cmd = chord(m, "w")
	if cmd != nil || m.confirmHandover != "" || m.errorMsg != "one has no running parent: use ^X K to close it, or adopt it first" {
		t.Fatalf("no running parent: refused before asking: %q", m.errorMsg)
	}
	m.errorMsg, parent = "", "lead"
	m, cmd = chord(m, "w")
	if cmd != nil || m.confirmHandover != "s1" || !strings.Contains(m.View(), `Hand "one" over to lead and close it?`) {
		t.Fatalf("^X W asks first, naming the parent: %q", m.confirmHandover)
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || m.confirmHandover != "" || len(got) != 0 {
		t.Fatal("esc cancels it")
	}
	m, _ = chord(m, "w")
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd != nil || m.confirmHandover != "s1" || len(got) != 0 {
		t.Fatal("other keys do nothing while it asks")
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil || m.confirmHandover != "" || !strings.Contains(m.errorMsg, "Asking one to hand over to lead") {
		t.Fatalf("y hands it over: %q", m.errorMsg)
	}
	res, _ := m.Update(cmd())
	m = res.(model)
	if len(got) != 1 || got[0] != (call{"one", "s1"}) || !strings.Contains(m.errorMsg, "handed over to lead") {
		t.Errorf("handed over by name and id: %+v, %q", got, m.errorMsg)
	}
}

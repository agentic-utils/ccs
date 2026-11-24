package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// selModel is a model showing conv in a 100x40 sidebar layout.
func selModel(conv transcript.Conversation) model {
	m := initialModel([]listItem{{conv: conv}}, "", nil)
	m.width, m.height = 100, 40
	m.headerCollapsed = true
	return m
}

// screenOf finds want in the drawn preview: its screen cell.
func screenOf(t *testing.T, m model, want string) (x, y int) {
	t.Helper()
	_, _, previewTop := m.listLayout()
	_, _, paneX := m.msgArea()
	for row, l := range strings.Split(m.renderPreview(m.filtered[0], m.previewRenderHeight()), "\n") {
		plain := strip2(l)
		if i := strings.Index(plain, want); i >= 0 {
			return paneX + len([]rune(plain[:i])), previewTop + row
		}
	}
	t.Fatalf("%q isn't drawn", want)
	return 0, 0
}

func mouse(m model, x, y int, action tea.MouseAction) (model, tea.Cmd) {
	nm, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: action})
	return nm.(model), cmd
}

func TestSelectionJoinsWrapsAndDropsMarkers(t *testing.T) {
	long := strings.Repeat("alpha beta gamma ", 12) + "end"
	conv := transcript.Conversation{SessionID: "s", Cwd: "/p", Messages: []transcript.Message{
		{Role: "user", Text: "first question", Ts: "2026-09-30T10:00:00Z"},
		{Role: "assistant", Text: long + "\nsecond line\n```\nfunc f() {\n    return 1\n}\n```", Ts: "2026-09-30T10:01:00Z"},
	}}
	m := selModel(conv)
	lines := m.previewLines()
	got := selectionText(lines, selPoint{0, 0}, selPoint{len(lines) - 1, 1 << 20})
	want := "first question\n\n" + long + "\nsecond line\nfunc f() {\n    return 1\n}"
	if got != want {
		t.Errorf("selection text:\n%q\nwant\n%q", got, want)
	}
	if strings.ContainsAny(got, "●❯\x1e\x1f") {
		t.Errorf("markers or soft-wrap marks leaked: %q", got)
	}
}

func TestSelectionJoinsAWordCutInTwo(t *testing.T) {
	word := strings.Repeat("x", 150) // longer than a line: cut, not wrapped at a space
	conv := transcript.Conversation{SessionID: "s", Cwd: "/p", Messages: []transcript.Message{{Role: "assistant", Text: word, Ts: "2026-09-30T10:01:00Z"}}}
	m := selModel(conv)
	lines := m.previewLines()
	if got := selectionText(lines, selPoint{0, 0}, selPoint{len(lines) - 1, 1 << 20}); got != word {
		t.Errorf("a word cut in two should join with no space: %q", got)
	}
}

func TestDragSelectsAndCopies(t *testing.T) {
	var copied []string
	defer func(f func(string) error) { pbcopy = f }(pbcopy)
	pbcopy = func(s string) error { copied = append(copied, s); return nil }
	var opened string
	defer func(f func(string) error) { openURL = f }(openURL)
	openURL = func(u string) error { opened = u; return nil }
	t.Setenv("SSH_TTY", "")

	conv := transcript.Conversation{SessionID: "s", Cwd: "/p", Messages: []transcript.Message{
		{Role: "assistant", Text: "see https://example.com/a for the rest", Ts: "2026-09-30T10:00:00Z"},
	}}
	m := selModel(conv)
	x, y := screenOf(t, m, "example.com")
	m, cmd := mouse(m, x, y, tea.MouseActionPress)
	if cmd != nil {
		t.Fatal("nothing happens on the press")
	}
	m, _ = mouse(m, x+6, y, tea.MouseActionMotion)
	if !m.sel.shown {
		t.Fatal("a drag of a cell or more should select")
	}
	if drawn := m.renderPreview(m.filtered[0], m.previewRenderHeight()); !strings.Contains(drawn, "\033[7mexample") {
		t.Errorf("the selection should be drawn in reverse video:\n%q", drawn)
	}
	m, cmd = mouse(m, x+6, y, tea.MouseActionRelease)
	if cmd == nil {
		t.Fatal("letting go should copy")
	}
	nm, _ := m.Update(cmd())
	m = nm.(model)
	if opened != "" {
		t.Errorf("a drag over a link mustn't open it: %s", opened)
	}
	if len(copied) != 1 || copied[0] != "example" {
		t.Errorf("copied %q, want example", copied)
	}
	if m.errorMsg != "copied 7 chars" || m.noteMsg != m.errorMsg {
		t.Errorf("note: %q", m.errorMsg)
	}
	if !m.sel.shown {
		t.Error("the selection stays drawn after letting go")
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if nm.(model).sel.shown {
		t.Error("a key should clear the selection")
	}
	m, _ = mouse(m, x, y+1, tea.MouseActionPress)
	if m.sel.shown {
		t.Error("a click should clear the selection")
	}
}

func TestClickWithoutDragStillOpensLink(t *testing.T) {
	var opened string
	defer func(f func(string) error) { openURL = f }(openURL)
	openURL = func(u string) error { opened = u; return nil }
	conv := transcript.Conversation{SessionID: "s", Cwd: "/p", Messages: []transcript.Message{
		{Role: "assistant", Text: "see https://example.com/a", Ts: "2026-09-30T10:00:00Z"},
	}}
	m := selModel(conv)
	x, y := screenOf(t, m, "example.com")
	m, _ = mouse(m, x+2, y, tea.MouseActionPress)
	m, _ = mouse(m, x+2, y, tea.MouseActionMotion) // a twitch within the cell
	m, cmd := mouse(m, x+2, y, tea.MouseActionRelease)
	if cmd == nil {
		t.Fatal("a click should open the link")
	}
	cmd()
	if opened != "https://example.com/a" || m.sel.shown {
		t.Errorf("opened %q, selection %v", opened, m.sel.shown)
	}
}

func TestDragInTheListDoesNotSelect(t *testing.T) {
	conv := transcript.Conversation{SessionID: "s", Cwd: "/p", Messages: []transcript.Message{{Role: "assistant", Text: "hello there", Ts: "2026-09-30T10:00:00Z"}}}
	m := selModel(conv)
	listTop, _, _ := m.listLayout()
	m, _ = mouse(m, 2, listTop, tea.MouseActionPress)
	m, _ = mouse(m, 10, listTop+1, tea.MouseActionMotion)
	m, cmd := mouse(m, 10, listTop+1, tea.MouseActionRelease)
	if m.sel.shown || cmd != nil {
		t.Error("a drag in the list shouldn't select or copy")
	}
}

func TestDragPastTheTopScrolls(t *testing.T) {
	var msgs []transcript.Message
	for range 40 {
		msgs = append(msgs, transcript.Message{Role: "assistant", Text: "line", Ts: "2026-09-30T10:00:00Z"})
	}
	m := selModel(transcript.Conversation{SessionID: "s", Cwd: "/p", Messages: msgs})
	top, rows, x := m.msgArea()
	m, _ = mouse(m, x+3, top+rows-1, tea.MouseActionPress)
	m, _ = mouse(m, x+3, top-1, tea.MouseActionMotion)
	m, _ = mouse(m, x+3, top-1, tea.MouseActionMotion)
	if m.previewScroll != 2 {
		t.Errorf("each move past the top should scroll a line: %d", m.previewScroll)
	}
	from, to := m.sel.selSpan()
	if to.line-from.line != rows-1+2 {
		t.Errorf("the selection should grow as it scrolls: %v to %v", from, to)
	}
}

func TestWriteClipboard(t *testing.T) {
	defer func(f func(string) error, g func(string)) { pbcopy, osc52 = f, g }(pbcopy, osc52)
	var pb, osc []string
	pbcopy = func(s string) error { pb = append(pb, s); return nil }
	osc52 = func(s string) { osc = append(osc, s) }
	t.Setenv("SSH_TTY", "")
	if err := writeClipboard("a"); err != nil || len(pb) != 1 || len(osc) != 0 {
		t.Errorf("locally pbcopy alone: %v %q %q", err, pb, osc)
	}
	t.Setenv("SSH_TTY", "/dev/ttys001")
	if err := writeClipboard("b"); err != nil || len(pb) != 2 || len(osc) != 1 {
		t.Errorf("over ssh OSC 52 too: %v %q %q", err, pb, osc)
	}
	t.Setenv("SSH_TTY", "")
	pbcopy = func(string) error { return errNoPbcopy }
	if err := writeClipboard("c"); err != nil || len(osc) != 2 {
		t.Errorf("no pbcopy: OSC 52 instead: %v %q", err, osc)
	}
	pbcopy = func(string) error { return errors.New("boom") }
	if err := writeClipboard("d"); err == nil {
		t.Error("a failing pbcopy is an error")
	}
}

func TestCopyReplyKey(t *testing.T) {
	var copied string
	defer func(f func(string) error) { pbcopy = f }(pbcopy)
	pbcopy = func(s string) error { copied = s; return nil }
	t.Setenv("SSH_TTY", "")
	conv := transcript.Conversation{SessionID: "s", Cwd: "/p", Messages: []transcript.Message{
		{Role: "assistant", Text: "the **reply**", Ts: "2026-09-30T10:00:00Z"},
		{Role: "user", Text: "thanks", Ts: "2026-09-30T10:01:00Z"},
	}}
	m := selModel(conv)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	nm, cmd := nm.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	if cmd == nil {
		t.Fatal("^T C should copy")
	}
	nm, _ = nm.(model).Update(cmd())
	if copied != "the **reply**" || nm.(model).errorMsg != "copied 13 chars" {
		t.Errorf("copied %q, note %q", copied, nm.(model).errorMsg)
	}
}

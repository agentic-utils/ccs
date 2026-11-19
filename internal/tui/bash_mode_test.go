package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// pinkBox draws the message box with text in it, with colours on and a dark
// background, so Claude Code's dark bashBorder (253,93,177) is what's used.
func pinkBox(t *testing.T, text string, focus bool) (status, box string) {
	t.Helper()
	old, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() { lipgloss.SetColorProfile(old); lipgloss.SetHasDarkBackground(dark) })
	m := chatModel(t)
	conv := m.items[0].conv
	conv.Cwd = "/tmp/proj"
	in := m.chatInput
	in.SetValue(text)
	status, box, _ = strings.Cut(m.chatBox(conv, in, focus, 80), "\n")
	return status, box
}

const pinkSGR = "38;2;253;93;177"

func TestBangTurnsTheBoxPink(t *testing.T) {
	status, box := pinkBox(t, "!git status", true)
	if !strings.Contains(box, pinkSGR) {
		t.Errorf("border isn't pink: %q", box)
	}
	plain := strip2(box)
	if !strings.Contains(plain, "! git status") || strings.Contains(plain, "!git") || strings.Contains(plain, "›") {
		t.Errorf("the › should become a pink ! and the text's own ! shown once: %q", plain)
	}
	if !strings.Contains(strip2(status), "! runs a command here · in /tmp/proj") || !strings.Contains(status, pinkSGR) {
		t.Errorf("hint: %q", status)
	}
}

func TestBangIsPinkUnfocusedToo(t *testing.T) {
	_, box := pinkBox(t, "!ls", false)
	if !strings.Contains(box, pinkSGR) {
		t.Errorf("the mode follows the text, focused or not: %q", box)
	}
}

func TestOnlyASingleBangIsBashMode(t *testing.T) {
	for _, text := range []string{"!!git", "git status", "", "/compact", "hello !there"} {
		status, box := pinkBox(t, text, true)
		if strings.Contains(box+status, pinkSGR) {
			t.Errorf("%q: pink when it shouldn't be: %q", text, box)
		}
		if strings.Contains(strip2(status), "runs a command here") {
			t.Errorf("%q: bash hint shown: %q", text, status)
		}
	}
}

func TestRemovingTheBangGoesBack(t *testing.T) {
	_, pink := pinkBox(t, "!git", true)
	_, plain := pinkBox(t, "git", true)
	if !strings.Contains(pink, pinkSGR) || strings.Contains(plain, pinkSGR) {
		t.Fatalf("pink %q, plain %q", pink, plain)
	}
	if !strings.Contains(strip2(plain), "›") {
		t.Errorf("the › is back: %q", strip2(plain))
	}
}

func TestBashModeLeavesTheDraftAlone(t *testing.T) {
	m := chatModel(t)
	conv := m.items[0].conv
	m.chatInput.SetValue("!echo hi")
	m.chatInput.SetCursor(3)
	_ = m.chatBox(conv, m.chatInput, true, 80)
	if m.chatInput.Value() != "!echo hi" || m.chatInput.Position() != 3 {
		t.Errorf("draft changed: %q at %d", m.chatInput.Value(), m.chatInput.Position())
	}
}

func TestHeldBoxStaysPurple(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	box := strings.Join(heldBox([]string{"!git question"}, 60, true), "\n")
	if strings.Contains(box, pinkSGR) {
		t.Errorf("a held menu's box isn't bash mode: %q", box)
	}
}

func TestTildePath(t *testing.T) {
	t.Setenv("HOME", "/Users/x")
	for in, want := range map[string]string{"/Users/x": "~", "/Users/x/Code/ccs": "~/Code/ccs", "/Users/xy": "/Users/xy", "/tmp": "/tmp"} {
		if got := tildePath(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

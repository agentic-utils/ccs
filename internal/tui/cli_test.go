package tui

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestPrintHelp(t *testing.T) {
	// Just call it to ensure no panics - we can't easily test stdout
	// but this at least ensures the function doesn't crash
	printHelp()
}

func TestHelpTextListsEveryKey(t *testing.T) {
	var buf bytes.Buffer
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	printHelp()
	w.Close()
	os.Stdout = old
	buf.ReadFrom(r)
	for _, k := range []string{"Ctrl+X U", "Ctrl+L", "?", "Tab", "Ctrl+X", "Ctrl+R", "Ctrl+X E", "Ctrl+F", "Ctrl+X D", "Ctrl+X F", "Single keys act now and leave nothing behind"} {
		if !strings.Contains(buf.String(), k) {
			t.Errorf("--help should list %s", k)
		}
	}
}

func TestWithEnvClaudeArgs(t *testing.T) {
	got := withEnvClaudeArgs("  --mcp-config /a.json\t--x ", []string{"--plan"})
	want := []string{"--mcp-config", "/a.json", "--x", "--plan"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := withEnvClaudeArgs("", nil); len(got) != 0 {
		t.Errorf("empty env: got %q", got)
	}
}

func TestEraseSavedLinesAfterClear(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	e := eraseSavedLines{w}
	if n, err := io.WriteString(e, "a\x1b[2Jb"); n != 6 || err != nil { // how bubbletea sends its clears
		t.Fatalf("WriteString = %d, %v; want 6, nil", n, err)
	}
	w.Close()
	got, _ := io.ReadAll(r)
	if string(got) != "a\x1b[2J\x1b[3Jb" {
		t.Errorf("a clear should be followed by an erase of the saved lines, got %q", got)
	}
}

func TestProfileChord(t *testing.T) {
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 200, 30
	m, _ = chord(m, "q")
	if m.profileStop == nil || !strings.Contains(m.View(), "recording profile") {
		t.Fatal("^X Q starts recording and says so in the header")
	}
	path := m.profilePath
	if filepath.Dir(path) != filepath.Dir(state.UpdateLogPath) {
		t.Errorf("the profile goes beside the logs: %s", path)
	}
	m, _ = chord(m, "q")
	if m.profileStop != nil || strings.Contains(m.View(), "recording profile") || !strings.Contains(m.errorMsg, path) {
		t.Errorf("^X Q again stops it and says where it is: %q", m.errorMsg)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Errorf("profile not written: %v", err)
	}
	if _, err := startProfile(filepath.Join(t.TempDir(), "missing", "cpu.out")); err == nil {
		t.Error("an unwritable path is an error")
	}
}

package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

func TestShellQuote(t *testing.T) {
	if got := ShellQuote(`a b`); got != `'a b'` {
		t.Errorf("got %s", got)
	}
	if got := ShellQuote(`it's`); got != `'it'\''s'` {
		t.Errorf("got %s", got)
	}
}

func TestShellSafe(t *testing.T) {
	for s, want := range map[string]bool{"/Users/me/proj": true, "/tmp/it's": true, `/r/a\`: false, "/tmp/a\x01b": false, "/tmp/\xff": false} {
		if ShellSafe(s) != want {
			t.Errorf("shellSafe(%q) != %v", s, want)
		}
	}
}

func TestRunBoundedTimesOut(t *testing.T) {
	if _, err := RunBounded(50*time.Millisecond, nil, "sleep", "5"); !errors.Is(err, ErrTimedOut) {
		t.Errorf("want ErrTimedOut, got %v", err)
	}
}

func TestDetachedCommandIsLogged(t *testing.T) {
	old := state.UpdateLogPath
	state.UpdateLogPath = filepath.Join(t.TempDir(), "logs", "update.log")
	defer func() { state.UpdateLogPath = old }()

	if _, err := RunCommand(5*time.Second, nil, true, "sh", "-c", "echo downloading; echo boom >&2; exit 3"); err == nil {
		t.Fatal("want failure")
	}
	got, err := os.ReadFile(state.UpdateLogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"$ sh -c", "exit status 3", "  | downloading", "  | boom"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

package sessions

import (
	"testing"

	"github.com/agentic-utils/ccs/internal/proc"
)

// With no codex process in the process table, the Codex lookup never reads
// every process's command line, whether or not the TUI is in use.
func TestCodexLookupSkipsTheListingWithNoCodexRunning(t *testing.T) {
	oldMay, oldPs := proc.MayRun, proc.PsAll
	t.Cleanup(func() {
		proc.MayRun, proc.PsAll = oldMay, oldPs
		CodexLiveCache.Lock()
		CodexLiveCache.Out = nil
		CodexLiveCache.Unlock()
	})
	proc.MayRun = func(name string) bool { return name != "codex" }
	listed := false
	proc.PsAll = func() (string, error) { listed = true; return "", nil }
	Frugal.Store(false)
	CodexLiveCache.Lock()
	CodexLiveCache.Out = nil
	CodexLiveCache.Unlock()
	if got := CodexLiveSessions(); len(got) != 0 || listed {
		t.Fatalf("listed every process (%v) or found %v with no codex running", listed, got)
	}
}

// The boot is read once a process: RecordOpen asks on every live tick.
func TestBootIDIsKeptForTheProcess(t *testing.T) {
	bootID.Lock()
	old := bootID.id
	bootID.id = "kept"
	bootID.Unlock()
	t.Cleanup(func() { bootID.Lock(); bootID.id = old; bootID.Unlock() })
	if got := BootID(); got != "kept" {
		t.Fatalf("BootID = %q, want the kept answer", got)
	}
}

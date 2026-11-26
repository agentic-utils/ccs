package sessions

import (
	"fmt"
	"os"
	"testing"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// realSenderSession is SenderSession before TestMain stubs it (package
// variables are set before TestMain runs).
var realSenderSession = SenderSession

func senderTest(t *testing.T, tree string, codex map[string]LiveSession) {
	t.Helper()
	oldDir, oldTree, oldCodex := GetSessionsDir, proc.PsTree, LiveCodexSessions
	t.Cleanup(func() { GetSessionsDir, proc.PsTree, LiveCodexSessions = oldDir, oldTree, oldCodex })
	dir := t.TempDir()
	GetSessionsDir = func() string { return dir }
	proc.PsTree = func() (string, error) { return tree, nil }
	LiveCodexSessions = func() map[string]LiveSession { return codex }
}

func TestSenderSessionFindsACodexAncestor(t *testing.T) {
	// this process -> its parent -> 700, the codex TUI
	senderTest(t, fmt.Sprintf("%d %d x\n%d 700 x\n700 1 codex", os.Getpid(), os.Getppid(), os.Getppid()),
		map[string]LiveSession{"019a0000-0000-7000-8000-000000000001": {PID: 700, Codex: true}})
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "") // the tree wins over the environment
	c, ok := realSenderSession()
	if !ok || c.ID != "019a0000-0000-7000-8000-000000000001" || c.Kind != transcript.KindCodex {
		t.Fatalf("want the codex ancestor, got %+v %v", c, ok)
	}
}

func TestSenderSessionSkipsAnUnverifiedPID(t *testing.T) {
	senderTest(t, fmt.Sprintf("%d 700 x\n700 1 codex", os.Getppid()),
		map[string]LiveSession{"019a0000-0000-7000-8000-000000000001": {PID: 700, Codex: true, Unverified: true}})
	if c, ok := realSenderSession(); ok {
		t.Fatalf("a pid whose start time couldn't be checked may be recycled: got %+v", c)
	}
}

func TestSenderSessionFallsBackToTheEnvironment(t *testing.T) {
	senderTest(t, "", nil)
	t.Setenv("CODEX_THREAD_ID", "019a0000-0000-7000-8000-000000000002")
	c, ok := realSenderSession()
	if !ok || c.ID != "019a0000-0000-7000-8000-000000000002" || c.Kind != transcript.KindCodex {
		t.Fatalf("with no session among its ancestors, the environment's thread: got %+v %v", c, ok)
	}
}

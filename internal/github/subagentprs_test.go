package github

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// A session whose subagents are all quiet isn't globbed or read again every
// TTL, however often the list asks: only when its own transcript grows or a
// subagent starts. A session with none is never globbed at all.
func TestSubagentPRsQuietSessionsCostOnlyStats(t *testing.T) {
	globs := 0
	subagentGlob = func(p string) ([]string, error) { globs++; return filepath.Glob(p) }
	t.Cleanup(func() { subagentGlob = filepath.Glob })
	expire := func(conv transcript.Conversation) { // past subagentPRsTTL
		SubPRCache.Lock()
		r := SubPRCache.Dirs[conv.FilePath]
		r.at = time.Now().Add(-2 * subagentPRsTTL)
		SubPRCache.Dirs[conv.FilePath] = r
		SubPRCache.Unlock()
	}

	dir := t.TempDir()
	none := transcript.Conversation{FilePath: filepath.Join(dir, "none.jsonl")}
	for range 3 {
		subagentPRs(none)
		expire(none)
	}
	if globs != 0 {
		t.Errorf("no subagents folder: nothing to glob, got %d globs", globs)
	}

	conv := transcript.Conversation{FilePath: filepath.Join(dir, "s.jsonl"), Size: 100}
	sub := filepath.Join(dir, "s", "subagents")
	os.MkdirAll(sub, 0o755)
	a := filepath.Join(sub, "agent-a.jsonl")
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr create --fill"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"https://github.com/o/r/pull/1"}]}}` + "\n"
	os.WriteFile(a, []byte(line), 0o644)
	old := time.Now().Add(-2 * transcript.SubagentIdle)
	os.Chtimes(a, old, old)
	// The folders' times stay as the writes left them; a folder whose time
	// moved since the last read is a new subagent.
	if got := subagentPRs(conv); !slices.Equal(got, []string{"https://github.com/o/r/pull/1"}) {
		t.Fatalf("first read finds the subagent's PR: %v", got)
	}
	globs = 0
	for range 50 { // 50 TTLs of frames
		expire(conv)
		subagentPRs(conv)
	}
	if globs != 0 {
		t.Errorf("quiet subagents, nothing changed: re-globbed %d times over 50 TTLs", globs)
	}

	conv.Size = 200 // the session wrote: a quiet subagent may have been resumed
	expire(conv)
	subagentPRs(conv)
	if globs == 0 {
		t.Error("the session's transcript grew: its subagents are read again")
	}

	os.WriteFile(a, []byte(line+strings.Replace(line, "pull/1", "pull/2", 1)), 0o644) // written just now
	conv.Size = 300
	expire(conv)
	subagentPRs(conv)
	globs = 0
	expire(conv)
	if got := subagentPRs(conv); globs == 0 || len(got) != 2 {
		t.Errorf("a subagent written within SubagentIdle is read every TTL: %d globs, %v", globs, got)
	}
}

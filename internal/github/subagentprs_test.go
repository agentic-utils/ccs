package github

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// WarmPRs reads many sessions' subagents at once and leaves the same answers
// a read one by one would, so the list's first frame finds them cached.
func TestWarmPRsFillsTheCacheConcurrently(t *testing.T) {
	globs := 0
	var mu sync.Mutex
	subagentGlob = func(p string) ([]string, error) { mu.Lock(); globs++; mu.Unlock(); return filepath.Glob(p) }
	t.Cleanup(func() { subagentGlob = filepath.Glob })

	dir := t.TempDir()
	var convs []transcript.Conversation
	for i := range 20 {
		name := fmt.Sprintf("s%d", i)
		conv := transcript.Conversation{FilePath: filepath.Join(dir, name+".jsonl"), Size: 1}
		sub := filepath.Join(dir, name, "subagents")
		os.MkdirAll(sub, 0o755)
		pad := ""
		if i%2 == 0 { // a line past the reader's 64KB buffer before the gh call
			pad = `{"type":"user","message":{"content":"` + strings.Repeat("z", 200<<10) + `"}}` + "\n"
		}
		line := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr create --fill"}}]}}`+"\n"+
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"https://github.com/o/r/pull/%d"}]}}`+"\n", i)
		os.WriteFile(filepath.Join(sub, "agent-a.jsonl"), []byte(pad+line), 0o644)
		convs = append(convs, conv)
	}
	convs = append(convs, convs[3]) // the same session twice reads safely
	WarmPRs(convs)
	if globs == 0 {
		t.Fatal("WarmPRs read nothing")
	}
	globs = 0
	for i, conv := range convs[:20] {
		want := fmt.Sprintf("https://github.com/o/r/pull/%d", i)
		if got := PRsOf(conv); !slices.Equal(got, []string{want}) {
			t.Errorf("session %d: PRsOf = %v, want [%s]", i, got, want)
		}
	}
	if globs != 0 {
		t.Errorf("after WarmPRs, PRsOf within the TTL must come from the cache: %d globs", globs)
	}
	WarmPRs(nil) // nothing to read: returns at once
}

// A session whose subagents printed a PR it doesn't list gets the merged list
// built once: asking again allocates nothing until either list changes.
func TestPRsOfKeepsTheMergedList(t *testing.T) {
	dir := t.TempDir()
	conv := transcript.Conversation{FilePath: filepath.Join(dir, "m.jsonl"), Size: 1, PRs: []string{"https://github.com/o/r/pull/9"}}
	sub := filepath.Join(dir, "m", "subagents")
	os.MkdirAll(sub, 0o755)
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr create --fill"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"https://github.com/o/r/pull/1"}]}}` + "\n"
	os.WriteFile(filepath.Join(sub, "agent-a.jsonl"), []byte(line), 0o644)

	want := []string{"https://github.com/o/r/pull/9", "https://github.com/o/r/pull/1"}
	if got := PRsOf(conv); !slices.Equal(got, want) {
		t.Fatalf("PRsOf = %v, want %v", got, want)
	}
	if n := testing.AllocsPerRun(50, func() { PRsOf(conv) }); n != 0 {
		t.Errorf("asking again allocated %v times", n)
	}
	conv.PRs = append(slices.Clone(conv.PRs), "https://github.com/o/r/pull/1")
	if got := PRsOf(conv); !slices.Equal(got, []string{"https://github.com/o/r/pull/9", "https://github.com/o/r/pull/1"}) {
		t.Fatalf("after its own list changed: %v", got)
	}
	conv.PRs = []string{"https://github.com/o/r/pull/7"}
	if got := PRsOf(conv); !slices.Equal(got, []string{"https://github.com/o/r/pull/7", "https://github.com/o/r/pull/1"}) {
		t.Fatalf("after its own list changed again: %v", got)
	}
}

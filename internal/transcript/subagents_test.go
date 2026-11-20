package transcript

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A live session's subagents are looked at every subagentsTTL, but a
// meta.json is read and parsed again only when it changes, and a
// transcript's first line once: a session with hundreds of subagents
// otherwise rereads them all every two seconds.
func TestSubagentsOfRereadsOnlyChangedMeta(t *testing.T) {
	reads := 0
	readSubagentMeta = func(p string) ([]byte, error) { reads++; return os.ReadFile(p) }
	t.Cleanup(func() { readSubagentMeta = os.ReadFile })

	dir := t.TempDir()
	conv := Conversation{FilePath: filepath.Join(dir, "s.jsonl")}
	sub := filepath.Join(dir, "s", "subagents")
	os.MkdirAll(sub, 0o755)
	meta := filepath.Join(sub, "agent-a.meta.json")
	os.WriteFile(meta, []byte(`{"name":"worker","finished":false}`), 0o644)
	os.WriteFile(filepath.Join(sub, "agent-a.jsonl"), []byte(`{"timestamp":"2026-11-20T10:00:00Z"}`+"\n"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(meta, old, old)

	expire := func() {
		subagentCache.Lock()
		delete(subagentCache.m, filepath.Join(sub)) // past subagentsTTL
		subagentCache.Unlock()
	}
	var subs []Subagent
	for range 10 {
		expire()
		subs = SubagentsOf(conv)
	}
	if reads != 1 {
		t.Errorf("an unchanged meta.json is read once, got %d reads over 10 TTLs", reads)
	}
	if len(subs) != 1 || subs[0].Name != "worker" || subs[0].Finished || !subs[0].Started.Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("the subagent as its meta.json and first line say: %+v", subs)
	}

	os.WriteFile(meta, []byte(`{"name":"worker","finished":true}`), 0o644) // it finished: meta.json written again
	expire()
	if subs = SubagentsOf(conv); reads != 2 || !subs[0].Finished {
		t.Errorf("a changed meta.json is read again: %d reads, %+v", reads, subs)
	}
}

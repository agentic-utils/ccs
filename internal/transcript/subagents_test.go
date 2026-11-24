package transcript

import (
	"os"
	"path/filepath"
	"strings"
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

// Once none of a session's subagents has written for SubagentIdle, each TTL
// costs one stat of their folder: they're listed again only when the
// session's own transcript grows (a SendMessage or Agent call wakes one) or
// the folder changes (a subagent added).
func TestQuietSubagentsCostOneStat(t *testing.T) {
	globs := 0
	subagentMetaGlob = func(p string) ([]string, error) { globs++; return filepath.Glob(p) }
	t.Cleanup(func() { subagentMetaGlob = filepath.Glob })
	expire := func(conv Conversation) {
		dir := filepath.Join(strings.TrimSuffix(conv.FilePath, ".jsonl"), "subagents")
		subagentCache.Lock()
		r := subagentCache.m[dir]
		r.at = time.Now().Add(-2 * subagentsTTL)
		subagentCache.m[dir] = r
		subagentCache.Unlock()
	}

	dir := t.TempDir()
	none := Conversation{FilePath: filepath.Join(dir, "none.jsonl")}
	for range 3 {
		SubagentsOf(none)
		expire(none)
	}
	if globs != 0 {
		t.Errorf("no subagents folder: nothing to glob, got %d globs", globs)
	}

	conv := Conversation{FilePath: filepath.Join(dir, "s.jsonl"), Size: 100}
	sub := filepath.Join(dir, "s", "subagents")
	os.MkdirAll(sub, 0o755)
	long := time.Now().Add(-2 * SubagentIdle)
	for _, f := range []string{"agent-a.meta.json", "agent-a.jsonl"} {
		os.WriteFile(filepath.Join(sub, f), []byte(`{"name":"worker","finished":true}`+"\n"), 0o644)
		os.Chtimes(filepath.Join(sub, f), long, long)
	}
	os.Chtimes(sub, long, long)
	globs = 0
	for range 5 {
		if subs := SubagentsOf(conv); len(subs) != 1 {
			t.Fatalf("subagents %+v", subs)
		}
		expire(conv)
	}
	if globs != 1 {
		t.Errorf("quiet subagents listed %d times over 5 TTLs, want 1", globs)
	}

	conv.Size = 200 // the session wrote: perhaps it woke one
	os.WriteFile(filepath.Join(sub, "agent-a.jsonl"), []byte("more\n"), 0o644)
	os.Chtimes(sub, long, long)
	if subs := SubagentsOf(conv); globs != 2 || time.Since(subs[0].LastWrite) > time.Minute {
		t.Errorf("a grown session lists them again: %d globs, %+v", globs, subs)
	}
	expire(conv)
	SubagentsOf(conv)
	if globs != 3 {
		t.Errorf("a subagent writing isn't quiet: listed %d times, want 3", globs)
	}

	os.WriteFile(filepath.Join(sub, "agent-a.jsonl"), []byte("more\n"), 0o644)
	os.Chtimes(filepath.Join(sub, "agent-a.jsonl"), long, long)
	expire(conv)
	SubagentsOf(conv) // quiet again
	expire(conv)
	SubagentsOf(conv)
	n := globs
	os.WriteFile(filepath.Join(sub, "agent-b.meta.json"), []byte(`{"name":"new"}`), 0o644) // the folder changes
	expire(conv)
	if subs := SubagentsOf(conv); globs != n+1 || len(subs) != 2 {
		t.Errorf("a new subagent is listed: %d globs, %+v", globs-n, subs)
	}
}

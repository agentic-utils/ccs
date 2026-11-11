package transcript

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func useProjectsDir(t *testing.T, dir string) {
	t.Helper()
	old := GetProjectsDir
	GetProjectsDir = func() string { return dir }
	t.Cleanup(func() { GetProjectsDir = old })
}

// A session whose folder moved has its transcript in two project folders:
// the list shows it once, from the transcript written last, and a copy put
// aside in a dot folder is never read.
func TestGetConversationsOneRowPerSession(t *testing.T) {
	root := t.TempDir()
	useProjectsDir(t, root)
	id := "56e4cb9a-321b-41a8-ac16-2c7b1bd19bec"
	old := `{"type":"user","cwd":"/old","message":{"content":"old copy"},"timestamp":"2026-10-25T10:00:00Z"}` + "\n"
	cur := old + `{"type":"user","cwd":"/new","message":{"content":"live copy"},"timestamp":"2026-11-11T09:00:00Z"}` + "\n"
	now := time.Now()
	writeTranscript(t, filepath.Join(root, "-old", id+".jsonl"), old, now.Add(-time.Hour))
	writeTranscript(t, filepath.Join(root, "-new", id+".jsonl"), cur, now)
	writeTranscript(t, filepath.Join(root, ".moved", id+".jsonl"), cur, now.Add(time.Hour)) // newest, but hidden
	writeTranscript(t, filepath.Join(root, ".moved", "only-hidden.jsonl"), old, now)
	// The session's own folder beside its transcript (subagents and so on) is no session.
	writeTranscript(t, filepath.Join(root, "-new", id, "subagents", "agent-x.jsonl"), old, now)

	convs, err := GetConversations(time.Time{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		var paths []string
		for _, c := range convs {
			paths = append(paths, c.FilePath)
		}
		t.Fatalf("want one row, got %d: %v", len(convs), paths)
	}
	if got, want := convs[0].FilePath, filepath.Join(root, "-new", id+".jsonl"); got != want {
		t.Errorf("row from %s, want the newest visible transcript %s", got, want)
	}
	if got := TranscriptPath(id); got != filepath.Join(root, "-new", id+".jsonl") {
		t.Errorf("TranscriptPath = %s, want the newest visible transcript", got)
	}
	if got := TranscriptPath("only-hidden"); got != "" {
		t.Errorf("TranscriptPath read a dot folder: %s", got)
	}
	if got := TranscriptPath("../x"); got != "" {
		t.Errorf("TranscriptPath accepted a path: %s", got)
	}
}

func TestNewestPerSessionKeepsKindsApart(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
	now := time.Now()
	writeTranscript(t, a, "x", now)
	writeTranscript(t, b, "x", now.Add(-time.Minute))
	got := newestPerSession([]Conversation{
		{SessionID: "s", FilePath: b},
		{SessionID: "s", FilePath: a},
		{SessionID: "s", FilePath: b, Kind: KindCodex},
		{SessionID: "t", FilePath: b},
	})
	if len(got) != 3 || got[0].FilePath != a || got[1].Kind != KindCodex || got[2].SessionID != "t" {
		t.Errorf("newestPerSession = %+v", got)
	}
}

// A session started in a folder since moved shows the folder its latest line
// was in, while a session whose first folder still exists keeps it.
func TestResolveCwd(t *testing.T) {
	exists := map[string]bool{"/new": true, "/start": true}
	old := DirExists
	DirExists = func(p string) bool { return exists[p] }
	t.Cleanup(func() { DirExists = old })
	line := func(cwd, ts string) string {
		return `{"type":"user","cwd":"` + cwd + `","message":{"content":"hi"},"timestamp":"` + ts + `"}` + "\n"
	}
	for _, tc := range []struct {
		name, content, want string
	}{
		{"moved", line("/gone", "2026-11-01T00:00:00Z") + line("/new", "2026-11-11T00:00:00Z"), "/new"},
		{"still there", line("/start", "2026-11-01T00:00:00Z") + line("/new", "2026-11-11T00:00:00Z"), "/start"},
		{"both gone", line("/gone", "2026-11-01T00:00:00Z") + line("/gone2", "2026-11-11T00:00:00Z"), "/gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			writeTranscript(t, path, tc.content, time.Now())
			c, err := ParseConversationFile(path, time.Time{}, 0)
			if err != nil || c == nil {
				t.Fatalf("parse: %v", err)
			}
			if c.Cwd != tc.want {
				t.Errorf("Cwd = %s, want %s", c.Cwd, tc.want)
			}
		})
	}
}

// Lines appended later can move a session to its new folder.
func TestResolveCwdOnAppend(t *testing.T) {
	exists := map[string]bool{"/new": true}
	old := DirExists
	DirExists = func(p string) bool { return exists[p] }
	t.Cleanup(func() { DirExists = old })
	path := filepath.Join(t.TempDir(), "s.jsonl")
	first := `{"type":"user","cwd":"/gone","message":{"content":"hi"},"timestamp":"2026-11-01T00:00:00Z"}` + "\n"
	writeTranscript(t, path, first, time.Now().Add(-time.Minute))
	c, err := ParseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil || c.Cwd != "/gone" {
		t.Fatalf("parse: %v %+v", err, c)
	}
	writeTranscript(t, path, first+`{"type":"user","cwd":"/new","message":{"content":"again"},"timestamp":"2026-11-11T00:00:00Z"}`+"\n", time.Now())
	n, err := ParseAppended(c)
	if err != nil {
		t.Fatal(err)
	}
	if n.Cwd != "/new" {
		t.Errorf("Cwd after append = %s, want /new", n.Cwd)
	}
}

// An incremental parse gives the folder a full parse of the same file does,
// even when the session moved twice.
func TestResolveCwdAppendMatchesFullParse(t *testing.T) {
	exists := map[string]bool{"/a": true, "/b": true}
	old := DirExists
	DirExists = func(p string) bool { return exists[p] }
	t.Cleanup(func() { DirExists = old })
	line := func(cwd, ts string) string {
		return `{"type":"user","cwd":"` + cwd + `","message":{"content":"hi"},"timestamp":"` + ts + `"}` + "\n"
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	first := line("/gone", "2026-11-01T00:00:00Z") + line("/a", "2026-11-02T00:00:00Z")
	writeTranscript(t, path, first, time.Now().Add(-time.Minute))
	c, err := ParseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil || c.Cwd != "/a" {
		t.Fatalf("parse: %v %+v", err, c)
	}
	writeTranscript(t, path, first+line("/b", "2026-11-03T00:00:00Z"), time.Now())
	n, err := ParseAppended(c)
	if err != nil {
		t.Fatal(err)
	}
	full, err := ParseConversationUncached(path, mustStat(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if n.Cwd != full.Cwd || n.Cwd != "/b" {
		t.Errorf("append Cwd = %s, full parse Cwd = %s, want both /b", n.Cwd, full.Cwd)
	}
}

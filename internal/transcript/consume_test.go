package transcript

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// consumeLinesReadBytes is consumeLines as it was before it reused one line
// buffer: a fresh slice for every line. consumeLines must parse exactly as it
// does.
func consumeLinesReadBytes(conv *Conversation, r io.Reader) (complete, total int64, tail bool, err error) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			total += int64(len(line))
			applied := ParseLine(conv, line)
			if line[len(line)-1] == '\n' {
				complete = total
			} else {
				tail = applied
			}
		}
		if err == io.EOF {
			return complete, total, tail, nil
		}
		if err != nil {
			return complete, total, tail, err
		}
	}
}

// equivTranscript holds every kind of line parseLine reads, lines longer
// than any reader's buffer, a line that isn't JSON and Unicode, so a
// reused buffer that something kept a slice of would show.
func equivTranscript() string {
	big := strings.Repeat("x", 3<<20) // past the 1MB pooled reader and a small file's 4KB one
	lines := []string{
		`{"type":"custom-title","customTitle":"equiv ✻ test"}`,
		`{"type":"ai-title","aiTitle":"ignored, a custom title wins"}`,
		`{"type":"user","entrypoint":"cli","promptId":"p1","cwd":"/tmp/equiv","timestamp":"2026-01-02T03:04:05Z","message":{"content":"first ask: open a PR ünïcödé"}}`,
		`{"type":"assistant","timestamp":"2026-01-02T03:04:06Z","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_read_input_tokens":200,"output_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":5}},"content":[{"type":"text","text":"Opening it."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr create --fill"}}]}}`,
		`{"type":"user","timestamp":"2026-01-02T03:04:07Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"https://github.com/o/r/pull/7\n` + big + `"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-02T03:04:08Z","message":{"id":"m2","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"sleep 100","run_in_background":true}}]}}`,
		`{"type":"user","timestamp":"2026-01-02T03:04:09Z","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"Command running in background with ID: b1"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-02T03:04:10Z","message":{"id":"m3","content":[{"type":"tool_use","id":"t3","name":"AskUserQuestion","input":{"questions":[{"question":"Which?","multiSelect":false,"options":[{"label":"A","description":"first"},{"label":"B","description":"second"}]}]}}]}}`,
		`{"type":"user","timestamp":"2026-01-02T03:04:11Z","toolUseResult":{"answers":{"Which?":"A"}},"message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":"User has answered your questions: \"Which?\"=\"A\""}]}}`,
		`{"type":"pr-link","prUrl":"https://github.com/o/r/pull/8"}`,
		`{"type":"system","subtype":"turn_duration","durationMs":4000,"timestamp":"2026-01-02T03:04:12Z"}`,
		`not json at all ` + big[:5000],
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-01-02T03:04:13Z","content":"queued while busy"}`,
		`{"type":"attachment","timestamp":"2026-01-02T03:04:14Z","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"queued while busy"}}`,
		`{"type":"queue-operation","operation":"remove","reason":"absorbed_mid_turn","content":"queued while busy"}`,
		`{"type":"user","timestamp":"2026-01-02T03:04:15Z","message":{"content":"<task-notification><task-id>b1</task-id><status>completed</status></task-notification>"}}`,
		`{"type":"assistant","timestamp":"2026-01-02T03:04:16Z","message":{"id":"m4","usage":{"input_tokens":1,"output_tokens":2},"content":[{"type":"text","text":"` + big[:70000] + ` done?"}]}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestConsumeLinesMatchesReadBytes(t *testing.T) {
	body := equivTranscript()
	cases := map[string]string{
		"whole":              body,
		"no final newline":   strings.TrimSuffix(body, "\n"),
		"partial last write": body + `{"type":"user","timestamp":"2026-01-02T03:05:00Z","message":{"content":"half writt`,
		"one long line only": strings.Repeat("y", 2<<20),
		"empty":              "",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			// A whole file uses the pooled 1MB reader; an append (a small read
			// from an offset) a reader of its own size: try both.
			path := filepath.Join(t.TempDir(), "s.jsonl")
			if err := os.WriteFile(path, []byte("{}\n"+in), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, from := range []int64{0, 3} {
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				f.Seek(from, io.SeekStart)
				var got Conversation
				gc, gt, gtail, gerr := consumeLines(&got, f)
				f.Close()
				var want Conversation
				wc, wt, wtail, werr := consumeLinesReadBytes(&want, bytes.NewReader([]byte("{}\n" + in)[from:]))
				if gc != wc || gt != wt || gtail != wtail || (gerr == nil) != (werr == nil) {
					t.Errorf("from %d: counts %d %d %v %v, want %d %d %v %v", from, gc, gt, gtail, gerr, wc, wt, wtail, werr)
				}
				assertSameParse(t, fmt.Sprintf("from %d", from), &got, &want)
			}
		})
	}
	// Concurrent scans share the pooled readers: each must still parse its own.
	t.Run("pooled readers in parallel", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 8 {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i)), []byte(strings.ReplaceAll(body, "equiv ✻ test", fmt.Sprintf("session %d", i))), 0o600)
		}
		done := make(chan string, 8)
		for i := range 8 {
			go func() {
				f, _ := os.Open(filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i)))
				defer f.Close()
				var c Conversation
				consumeLines(&c, f)
				done <- c.Title
			}()
		}
		seen := map[string]bool{}
		for range 8 {
			seen[<-done] = true
		}
		if len(seen) != 8 {
			t.Errorf("each parse keeps its own title: %v", seen)
		}
	})
}

// assertSameParse: the two parses agree in every field, and in the search
// text and messages the preview draws, once finished as a scan finishes them.
func assertSameParse(t *testing.T, label string, got, want *Conversation) {
	t.Helper()
	FinishParse(got)
	FinishParse(want)
	if g, w := SearchTextOf(*got), SearchTextOf(*want); g != w {
		t.Errorf("%s: search text differs (%d vs %d bytes)", label, len(g), len(w))
	}
	if !reflect.DeepEqual(got.Messages, want.Messages) {
		t.Errorf("%s: messages differ", label)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: conversations differ:\n got %+v\nwant %+v", label, *got, *want)
	}
}

// TestConsumeLinesMatchesReadBytesOnRealTranscripts runs the same comparison
// over every transcript in CCS_EQUIV_DIR (a Claude projects folder, read
// only); skipped otherwise, since real transcripts aren't in the repo.
func TestConsumeLinesMatchesReadBytesOnRealTranscripts(t *testing.T) {
	dir := os.Getenv("CCS_EQUIV_DIR")
	if dir == "" {
		t.Skip("set CCS_EQUIV_DIR to a projects folder")
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	var compared int
	var bytesRead int64
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		var got Conversation
		gc, gt, gtail, _ := consumeLines(&got, f)
		f.Close()
		f, err = os.Open(path)
		if err != nil {
			continue
		}
		var want Conversation
		wc, wt, wtail, _ := consumeLinesReadBytes(&want, f)
		f.Close()
		if wt != gt { // written to meanwhile (a live session): not comparable
			continue
		}
		if gc != wc || gtail != wtail {
			t.Errorf("%s: counts %d %v, want %d %v", path, gc, gtail, wc, wtail)
		}
		assertSameParse(t, filepath.Base(path), &got, &want)
		compared++
		bytesRead += gt
	}
	t.Logf("compared %d transcripts, %d MB", compared, bytesRead>>20)
}

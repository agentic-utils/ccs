package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPruneStreamRemovesDuplicatesKeepsDialogue(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","message":{"content":"hello"},"uuid":"u1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]},"uuid":"a1"}`,
		`{"type":"file-history-snapshot","snapshot":{"trackedFileBackups":{"big":"xxxxxxxxxx"}}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"the real output"}]},"toolUseResult":{"type":"text","text":"the real output dup"},"uuid":"u2"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	st, err := pruneStream(strings.NewReader(input), &out, pruneOpts{dropSnapshots: true, stripToolResults: true})
	if err != nil {
		t.Fatalf("pruneStream: %v", err)
	}
	if st.droppedSnapshots != 1 {
		t.Errorf("droppedSnapshots = %d, want 1", st.droppedSnapshots)
	}
	if st.strippedResults != 1 {
		t.Errorf("strippedResults = %d, want 1", st.strippedResults)
	}
	if st.convLinesIn != 3 || st.convLinesOut != 3 {
		t.Errorf("conv lines in/out = %d/%d, want 3/3", st.convLinesIn, st.convLinesOut)
	}
	o := out.String()
	if strings.Contains(o, "trackedFileBackups") {
		t.Error("file-history-snapshot should be dropped")
	}
	if strings.Contains(o, "toolUseResult") {
		t.Error("toolUseResult field should be stripped")
	}
	for _, want := range []string{"hello", "the real output", "u2", "a1"} {
		if !strings.Contains(o, want) {
			t.Errorf("output should preserve %q", want)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(o), "\n") {
		var v map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &v) != nil {
			t.Errorf("output line is not valid JSON: %s", line)
		}
	}
}

func TestPruneOptsRespected(t *testing.T) {
	input := `{"type":"file-history-snapshot","snapshot":{}}` + "\n" +
		`{"type":"user","toolUseResult":{"x":1},"message":{"content":"hi"}}` + "\n"
	var a bytes.Buffer
	pruneStream(strings.NewReader(input), &a, pruneOpts{dropSnapshots: false, stripToolResults: true})
	if !strings.Contains(a.String(), "file-history-snapshot") {
		t.Error("--no-snapshots should keep snapshot lines")
	}
	var b bytes.Buffer
	pruneStream(strings.NewReader(input), &b, pruneOpts{dropSnapshots: true, stripToolResults: false})
	if !strings.Contains(b.String(), "toolUseResult") {
		t.Error("--no-tool-results should keep toolUseResult")
	}
}

func TestPruneFileReplacesAndShrinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	content := `{"type":"user","message":{"content":"keep me"},"uuid":"u1"}` + "\n" +
		`{"type":"file-history-snapshot","snapshot":{"data":"` + strings.Repeat("x", 5000) + `"}}` + "\n" +
		`{"type":"assistant","message":{"content":"keep me too"},"uuid":"a1"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	st, err := pruneFile(path, true, pruneOpts{dropSnapshots: true, stripToolResults: true})
	if err != nil {
		t.Fatalf("pruneFile: %v", err)
	}
	if st.convLinesIn != st.convLinesOut {
		t.Fatalf("integrity broken: %d != %d", st.convLinesIn, st.convLinesOut)
	}
	after, _ := os.Stat(path)
	if after.Size() >= before.Size() {
		t.Errorf("file should shrink: before %d, after %d", before.Size(), after.Size())
	}
	if _, err := os.Stat(path + ".pruned"); !os.IsNotExist(err) {
		t.Error(".pruned temp should be gone after atomic replace")
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, "file-history-snapshot") {
		t.Error("snapshot not removed from file")
	}
	if !strings.Contains(s, "keep me") || !strings.Contains(s, "keep me too") {
		t.Error("dialogue not preserved in file")
	}
}

func TestPruneFileDryRunLeavesFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jsonl")
	content := `{"type":"file-history-snapshot","snapshot":{}}` + "\n" + `{"type":"user","message":{"content":"hi"}}` + "\n"
	os.WriteFile(path, []byte(content), 0644)
	orig, _ := os.ReadFile(path)
	st, err := pruneFile(path, false, pruneOpts{dropSnapshots: true, stripToolResults: true})
	if err != nil {
		t.Fatalf("pruneFile dry: %v", err)
	}
	if st.bytesOut >= st.bytesIn {
		t.Error("dry run should still report savings")
	}
	now, _ := os.ReadFile(path)
	if string(now) != string(orig) {
		t.Error("dry run must not modify the file")
	}
}

package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	artA = "https://claude.ai/artifact/AbCdEf123"
	artB = "https://claude.ai/code/artifact/0b5e6c1a-1111-4222-8333-944455556666"
	artC = "https://claude.ai/artifact/XyZ789"
)

// artifactLines is an Artifact call with input and its result: text, and
// toolUseResult unless it is "".
func artifactLines(id, input, text, result string, isError bool) []string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	res := `{"type":"user","timestamp":"2026-10-01T10:00:0` + id[len(id)-1:] + `Z","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + q(text)
	if isError {
		res += `,"is_error":true`
	}
	res += `}]}`
	if result != "" {
		res += `,"toolUseResult":` + result
	}
	return []string{
		`{"type":"assistant","timestamp":"2026-10-01T10:00:00Z","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Artifact","input":` + input + `}]}}`,
		res + `}`,
	}
}

func parseAll(c *Conversation, groups ...[]string) {
	for _, g := range groups {
		for _, l := range g {
			ParseLine(c, []byte(l))
		}
	}
}

func TestArtifactsFromToolResults(t *testing.T) {
	var c Conversation
	parseAll(&c,
		// a publish: url and title from toolUseResult
		artifactLines("t1", `{"file_path":"/tmp/x/report.html","icon":"chart"}`,
			"Published /tmp/x/report.html at "+artA+" (Version 1)", `{"url":"`+artA+`","title":"Made-up Report"}`, false),
		// ccs prune stripped toolUseResult: the text gives the url, the file its name
		artifactLines("t2", `{"file_path":"/tmp/x/plan.html"}`,
			"Published /tmp/x/plan.html at "+artB+" (Version 1)", "", false),
		// opened, not published
		artifactLines("t3", `{"action":"open","url":"`+artC+`"}`,
			"Opened the Artifact at "+artC+" for the user.", `{"opened":true,"url":"`+artC+`","title":"Someone's Page"}`, false),
		// an error counts for nothing
		artifactLines("t4", `{"file_path":"/tmp/x/bad.html"}`,
			"Publish failed; see https://claude.ai/artifact/Nope1", "", true),
		// an asset upload is not a page
		artifactLines("t5", `{"url":"`+artA+`","asset":true,"file_path":"/tmp/x/logo.png"}`,
			"Uploaded at https://claude.ai/artifact/AssetOnly", `{"url":"https://claude.ai/artifact/AssetOnly"}`, false),
		// a list names many artifacts: none of them is this session's
		artifactLines("t6", `{"action":"list"}`,
			"- Other - https://claude.ai/artifact/Listed1", `{"artifacts":[{"url":"https://claude.ai/artifact/Listed1"}]}`, false),
	)
	// an address merely mentioned in a message never counts
	ParseLine(&c, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"see https://claude.ai/artifact/Mentioned"}]}}`))
	if len(c.Artifacts) != 3 {
		t.Fatalf("publish, pruned publish and open, nothing else: %+v", c.Artifacts)
	}
	if a := c.Artifacts[0]; a.URL != artA || a.Title != "Made-up Report" || !a.Published {
		t.Errorf("publish: %+v", a)
	}
	if a := c.Artifacts[1]; a.URL != artB || a.Title != "plan.html" || !a.Published {
		t.Errorf("pruned publish: %+v", a)
	}
	if a := c.Artifacts[2]; a.URL != artC || a.Title != "Someone's Page" || a.Published {
		t.Errorf("open: %+v", a)
	}

	// a republish updates the one entry; a later read keeps it published
	parseAll(&c,
		artifactLines("t7", `{"file_path":"/tmp/x/report.html"}`,
			"Published /tmp/x/report.html at "+artA+" (Version 2)", `{"url":"`+artA+`","title":"Made-up Report v2"}`, false),
		artifactLines("t8", `{"action":"read","url":"`+artA+`"}`, "[Artifact …]", `{"read":{"code":200,"url":"`+artA+`"}}`, false),
	)
	if len(c.Artifacts) != 3 || c.Artifacts[0].Title != "Made-up Report v2" || !c.Artifacts[0].Published || c.Artifacts[0].Ts != "2026-10-01T10:00:08Z" {
		t.Errorf("republish and read update the entry: %+v", c.Artifacts)
	}
	if a, _ := LatestArtifact(c.Artifacts); a.URL != artA {
		t.Errorf("latest is the one touched last: %+v", a)
	}

	// a delete drops it
	parseAll(&c, artifactLines("t9", `{"action":"delete","url":"`+artC+`"}`, "Deleted.", "", false))
	if len(c.Artifacts) != 2 {
		t.Errorf("a deleted artifact is gone: %+v", c.Artifacts)
	}
}

func TestArtifactsIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := artifactLines("t1", `{"file_path":"/tmp/x/a.html"}`, "Published /tmp/x/a.html at "+artA+" (Version 1)", `{"url":"`+artA+`","title":"A"}`, false)
	user := `{"type":"user","timestamp":"2026-10-01T09:59:00Z","message":{"role":"user","content":"make a page"}}`
	os.WriteFile(path, []byte(user+"\n"+lines[0]+"\n"), 0o644)
	c, err := ParseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Artifacts) != 0 {
		t.Fatalf("no result yet: %+v", c.Artifacts)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(lines[1] + "\n")
	f.Close()
	c2, err := ParseAppended(c)
	if err != nil || len(c2.Artifacts) != 1 || c2.Artifacts[0].Title != "A" {
		t.Errorf("the call kept across the incremental parse meets its result: %+v %v", c2, err)
	}
	if len(c.Artifacts) != 0 {
		t.Errorf("the earlier copy is untouched: %+v", c.Artifacts)
	}
}

func TestSubagentArtifacts(t *testing.T) {
	dir := t.TempDir()
	conv := Conversation{FilePath: filepath.Join(dir, "s.jsonl"), Artifacts: []Artifact{{URL: artA, Title: "mine", Published: true}}}
	sub := filepath.Join(dir, "s", "subagents")
	os.MkdirAll(filepath.Join(sub, "workflows", "wf_1"), 0o755)
	write := func(path string, lines []string) {
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(strings.Join(lines, "\n") + "\n")
		f.Close()
	}
	a := filepath.Join(sub, "agent-a.jsonl")
	write(a, artifactLines("t1", `{"file_path":"/tmp/x/a.html"}`, "Published at "+artA, `{"url":"`+artA+`","title":"dup"}`, false))
	write(filepath.Join(sub, "workflows", "wf_1", "agent-w.jsonl"),
		artifactLines("t2", `{"file_path":"/tmp/x/b.html"}`, "Published at "+artB, `{"url":"`+artB+`","title":"B"}`, false))
	got := ArtifactsOf(conv)
	if len(got) != 2 || got[0].Title != "mine" || got[1].URL != artB {
		t.Errorf("the session's own, then its subagents' not among them: %+v", got)
	}
	write(a, artifactLines("t3", `{"action":"open","url":"`+artC+`"}`, "Opened the Artifact at "+artC, "", false))
	SubArtifactCache.Lock()
	delete(SubArtifactCache.Dirs, sub) // past subagentArtifactsTTL
	SubArtifactCache.Unlock()
	if got := ArtifactsOf(conv); len(got) != 3 || !slices.ContainsFunc(got, func(a Artifact) bool { return a.URL == artC }) {
		t.Errorf("a subagent's later calls are read too: %+v", got)
	}
}

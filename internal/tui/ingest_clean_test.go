package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// poison is what an outsider can get into a transcript, a session file or a
// GitHub answer: a 7-bit escape (clear screen), a C1 CSI, and an OSC that
// would set the window title. In JSON they arrive escaped (\u001b).
const poison = "\x1b[2J\u009b2J\x1b]0;PWNED\x07"

// entityPoison and jsonPoison are the same escape written so that it only
// becomes one when ccs decodes it: an HTML entity in a teammate tag's
// attribute, and JSON escaped inside a teammate's JSON report.
const (
	entityPoison = "&#27;[2J&#27;]0;PWNED&#7;"
	jsonPoison   = `\u001b[2J\u001b]0;PWNED\u0007`
)

// anyControl is any C0 control other than newline and tab, DEL, or C1.
func anyControl(s string) bool {
	for _, r := range s {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0) {
			return true
		}
	}
	return false
}

// poisoned is whether text ccs drew still carries a live escape from the
// poison. Drawn text has ccs's own colours and OSC 8 links, so it can't be
// checked for escapes as such: the poison's own sequences (a clear screen, a
// C1 byte, an OSC 0 title) are what must be gone. Its escaped forms (the
// characters `\u001b` or `&#27;` kept as text) are harmless and may stay.
func poisoned(s string) bool {
	return strings.Contains(s, "\x1b[2J") || strings.Contains(s, "\u009b") || strings.Contains(s, "\x9b") || strings.Contains(s, "\x1b]0;")
}

func jsonLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// walkStrings calls f with every string reachable from v (fields, slices,
// maps, pointers), named by its path, so a new field is covered unasked.
func walkStrings(v reflect.Value, path string, f func(path, s string)) {
	switch v.Kind() {
	case reflect.String:
		f(path, v.String())
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkStrings(v.Elem(), path, f)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			walkStrings(v.Field(i), path+"."+v.Type().Field(i).Name, f)
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 { // raw bytes (a tool's JSON input): decoded where drawn
			return
		}
		for i := range v.Len() {
			walkStrings(v.Index(i), path+"["+strconv.Itoa(i)+"]", f)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			walkStrings(v.MapIndex(k), path+"["+k.String()+"]", f)
		}
	}
}

// rawOnPurpose are matched byte for byte rather than drawn (ingest.go).
var rawOnPurpose = map[string]bool{".SessionID": true, ".FilePath": true}

func assertCleanConv(t *testing.T, conv transcript.Conversation) {
	t.Helper()
	walkStrings(reflect.ValueOf(conv), "", func(path, s string) {
		if rawOnPurpose[path] || strings.HasPrefix(path, ".Peers[") && (strings.HasSuffix(path, ".Key") || strings.HasSuffix(path, ".Addr")) {
			return
		}
		if path == ".SearchText" || path == ".SearchLower" {
			s = strings.ReplaceAll(s, transcript.SearchSep, "") // its own part separator, never drawn
		}
		if anyControl(s) {
			t.Errorf("%s keeps the poison: %q", path, s)
		}
	})
}

// claudeTranscript writes a transcript whose every field ccs keeps or draws
// carries the poison, and parses it as a scan does.
func claudeTranscript(t *testing.T) transcript.Conversation {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "s-poison.jsonl")
	ts := "2026-09-29T10:00:00Z"
	teammate := `<teammate-message teammate_id="` + entityPoison + `lead" summary="s` + entityPoison + `">` + "\n" + `{"type":"report","result":"` + jsonPoison + `done"}` + "\n" + `</teammate-message>`
	lines := jsonLine(t, map[string]any{"type": "custom-title", "customTitle": "title" + poison}) +
		jsonLine(t, map[string]any{"type": "user", "cwd": "/tmp/x" + poison, "timestamp": ts, "entrypoint": "cli", "sessionId": "s-poison",
			"message": map[string]any{"role": "user", "content": "hello" + poison}}) +
		jsonLine(t, map[string]any{"type": "assistant", "timestamp": ts, "message": map[string]any{"id": "m1", "role": "assistant", "model": "custom" + poison,
			"content": []any{
				map[string]any{"type": "text", "text": "reply" + poison},
				map[string]any{"type": "tool_use", "id": "t1", "name": "Tool" + poison, "input": map[string]any{"x": "arg" + poison}},
				map[string]any{"type": "tool_use", "id": "b1", "name": "Bash", "input": map[string]any{"command": "sleep 1", "description": "build" + poison, "run_in_background": true}},
				map[string]any{"type": "tool_use", "id": "q1", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{
					map[string]any{"question": "pick" + poison, "options": []any{map[string]any{"label": "a" + poison, "description": "d" + poison}}}}}},
				map[string]any{"type": "tool_use", "id": "m2", "name": "SendMessage", "input": map[string]any{"to": "worker" + poison, "message": "hi there" + poison, "summary": "sum" + poison}},
			}}}) +
		jsonLine(t, map[string]any{"type": "user", "timestamp": ts, "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "b1", "content": "Command running in background with ID: x"},
			map[string]any{"type": "tool_result", "tool_use_id": "q1", "content": "answered" + poison}}},
			"toolUseResult": map[string]any{"answers": map[string]any{"pick" + poison: "a" + poison}, "annotations": map[string]any{"pick" + poison: map[string]any{"notes": "n" + poison}}}}) +
		jsonLine(t, map[string]any{"type": "user", "timestamp": ts, "message": map[string]any{"role": "user", "content": teammate}}) +
		jsonLine(t, map[string]any{"type": "user", "timestamp": "bad" + poison, "message": map[string]any{"role": "user", "content": "late"}})
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	// A subagent beside it, named in its meta.json.
	sub := filepath.Join(dir, "s-poison", "subagents")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(sub, "agent-1.meta.json"), []byte(jsonLine(t, map[string]any{"name": "worker" + poison})), 0o600)
	os.WriteFile(filepath.Join(sub, "agent-1.jsonl"), []byte(jsonLine(t, map[string]any{"type": "user", "timestamp": ts})), 0o600)
	info, _ := os.Stat(path)
	conv, err := transcript.ParseConversationUncached(path, info)
	if err != nil || conv == nil {
		t.Fatalf("parse: %v %v", conv, err)
	}
	return *conv
}

// Everything that reaches ccs from outside is cleaned where it enters, so
// nothing drawn from it carries a control: transcripts (Claude and Codex),
// subagents' meta.json, session files, and GitHub's answers, including text
// decoded later (a teammate tag's entities, a teammate's JSON report).
func TestUntrustedTextIsCleanedWhereItEnters(t *testing.T) {
	conv := claudeTranscript(t)

	t.Run("transcript fields", func(t *testing.T) {
		assertCleanConv(t, conv)
		if len(conv.Tasks) == 0 || len(conv.Asks) == 0 || len(conv.Asks[0].Answers) == 0 || len(conv.Tools) == 0 {
			t.Fatalf("the fixture didn't reach every record: tasks %d asks %+v tools %d", len(conv.Tasks), conv.Asks, len(conv.Tools))
		}
		if !strings.HasPrefix(conv.Messages[0].Text, "hello") || conv.Asks[0].Answers[0].Answer != "a" || conv.Asks[0].Answers[0].Notes != "n" {
			t.Errorf("cleaning lost the text or the answer: %q %+v", conv.Messages[0].Text, conv.Asks[0].Answers)
		}
	})

	t.Run("subagent meta.json", func(t *testing.T) {
		subs := transcript.SubagentsOf(conv)
		if len(subs) != 1 || subs[0].Name != "worker" {
			t.Errorf("subagent name: %+v", subs)
		}
	})

	t.Run("session file", func(t *testing.T) {
		dir := t.TempDir()
		old := sessions.GetSessionsDir
		sessions.GetSessionsDir = func() string { return dir }
		t.Cleanup(func() { sessions.GetSessionsDir = old })
		os.WriteFile(filepath.Join(dir, "4242.json"), []byte(jsonLine(t, map[string]any{"sessionId": "s-poison", "name": "build" + poison, "status": "idle", "cwd": "/tmp" + poison})), 0o600)
		f, err := sessions.ReadSessionFile(4242)
		if err != nil {
			t.Fatal(err)
		}
		if st := f.Stat(); poisoned(f.Name) || poisoned(st.Name) || poisoned(st.Cwd) || st.Name != "build" {
			t.Errorf("session file: %+v %+v", f, st)
		}
	})

	t.Run("GitHub", func(t *testing.T) {
		gh := filepath.Join(t.TempDir(), "gh")
		answer := `{"data":{"p0":{"pullRequest":{"number":1,"title":"fix` + jsonPoison + `","state":"OPEN","isDraft":false,"mergeStateStatus":"BLOCKED","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE","contexts":{"nodes":[{"name":"ci` + jsonPoison + `","conclusion":"FAILURE"},{"context":"deploy` + jsonPoison + `","state":"ERROR"}]}}}}]}}}}}`
		os.WriteFile(gh, []byte("#!/bin/sh\ncat <<'EOF'\n"+answer+"\nEOF\n"), 0o755)
		got, err := github.QueryPRs(gh, []string{prA})
		if err != nil {
			t.Fatal(err)
		}
		m := model{prs: got, width: 100}
		_, why := got[prA].Status()
		for name, s := range map[string]string{"status": why, "PR line": m.prLine(prA), "^O row": m.prLinkText(prA)} {
			if poisoned(s) {
				t.Errorf("%s: %q", name, s)
			}
		}
		if !strings.Contains(why, "ci") || !strings.Contains(why, "deploy") {
			t.Errorf("the check names went: %q", why)
		}
	})

	t.Run("drawn", func(t *testing.T) {
		m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
		m.width, m.height = 140, 60
		m.chatStatus = map[string]sessions.SessionStat{conv.SessionID: (sessions.SessionFile{Name: "build" + poison}).Stat()}
		drawn := map[string]string{"screen": m.View(), "session name": m.sessionName(conv)}
		for i, l := range m.peerLabels(conv) {
			drawn["peer label "+strconv.Itoa(i)] = l
		}
		for _, l := range m.visibleLinks() {
			drawn["link "+l.url] = l.text
		}
		for _, l := range m.taskLines(conv, 100) {
			drawn["task"] = l
		}
		for _, l := range m.subagentLines(conv, 100) {
			drawn["subagent"] = l
		}
		blocks := teammateBlocks(conv.Messages[len(conv.Messages)-2].Text)
		if len(blocks) != 1 || !strings.Contains(blocks[0].from, "lead") || !strings.Contains(blocks[0].body, "done") {
			t.Fatalf("the teammate fixture wasn't decoded: %+v", blocks)
		}
		for _, b := range blocks {
			drawn["teammate "+b.from] = b.from + " " + b.note + " " + b.body
		}
		for name, s := range drawn {
			if poisoned(s) {
				t.Errorf("%s draws the poison: %q", name, s)
			}
		}
		if !strings.Contains(drawn["screen"], "title") {
			t.Errorf("the title went missing from the screen")
		}
	})

	t.Run("Codex rollout", func(t *testing.T) {
		home := t.TempDir()
		old := transcript.CodexHome
		transcript.CodexHome = func() string { return home }
		t.Cleanup(func() { transcript.CodexHome = old })
		day := filepath.Join(home, "sessions", "2026", "09", "29")
		os.MkdirAll(day, 0o700)
		path := filepath.Join(day, "rollout-2026-09-29T10-00-00-c1.jsonl")
		ts := "2026-09-29T10:00:00Z"
		lines := jsonLine(t, map[string]any{"timestamp": ts, "type": "session_meta", "payload": map[string]any{"id": "c1", "cwd": "/tmp/c" + poison, "timestamp": ts}}) +
			jsonLine(t, map[string]any{"timestamp": ts, "type": "turn_context", "payload": map[string]any{"model": "gpt" + poison}}) +
			jsonLine(t, map[string]any{"timestamp": ts, "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi" + poison}}}}) +
			jsonLine(t, map[string]any{"timestamp": ts, "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok" + poison}}}})
		os.WriteFile(path, []byte(lines), 0o600)
		c, err := transcript.ParseCodexFile(path, time.Time{}, 0)
		if err != nil || c == nil {
			t.Fatalf("codex parse: %v %v", c, err)
		}
		assertCleanConv(t, *c)
		if c.ActiveModel != "gpt" || c.Messages[0].Text != "hi" {
			t.Errorf("codex text lost: %q %q", c.ActiveModel, c.Messages[0].Text)
		}
	})
}

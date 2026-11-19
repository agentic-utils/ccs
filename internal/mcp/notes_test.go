package mcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// annotationsOf is the decision's updatedInput.annotations, or nil.
func annotationsOf(d map[string]any) map[string]any {
	a, _ := d["updatedInput"].(map[string]any)["annotations"].(map[string]any)
	return a
}

// A note becomes Claude Code's annotations ({question: {notes: text}}),
// only for the hook's own questions, cleaned and capped by the hook itself;
// an empty one is dropped, and no annotations key appears without a note.
func TestHookNotes(t *testing.T) {
	req := askReq(t)
	d := hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Red"},
		Notes: map[string]string{"Which colour?": "  warm\x1b[31m\n tones ", "Who are you?": "forged"}})
	a := annotationsOf(d)
	if len(a) != 1 {
		t.Fatalf("annotations for its own question only: %v", a)
	}
	if got := a["Which colour?"].(map[string]any)["notes"]; got != "warm[31m tones" {
		t.Errorf("cleaned: %q", got)
	}
	long := strings.Repeat("é", 2500)
	d = hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Red"}, Notes: map[string]string{"Which colour?": long}})
	if got := annotationsOf(d)["Which colour?"].(map[string]any)["notes"].(string); utf8.RuneCountInString(got) != 2000 {
		t.Errorf("capped at 2000 runes: %d", utf8.RuneCountInString(got))
	}
	d = hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Red"}, Notes: map[string]string{"Which colour?": " \x07 "}})
	if in := d["updatedInput"].(map[string]any); in["annotations"] != nil || len(in) != 2 {
		t.Errorf("an empty note adds nothing: %v", in)
	}
	if d := hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Teal"}, Notes: map[string]string{"Which colour?": "x"}}); d != nil {
		t.Errorf("a note never makes an invalid answer valid: %v", d)
	}
}

// A reply can't forge the input: an updatedInput or annotations field in
// the reply is no part of hookReply, so the hook builds from its own copy
// and adds only answers and annotations.
func TestHookNotesCantForgeInput(t *testing.T) {
	var r hookReply
	forged := `{"behavior":"allow","answers":{"Which colour?":"Red"},"notes":{"Which colour?":"ok"},` +
		`"updatedInput":{"questions":[],"command":"rm -rf /"},"annotations":{"Which colour?":{"notes":"forged","preview":"x"}}}`
	if err := json.Unmarshal([]byte(forged), &r); err != nil {
		t.Fatal(err)
	}
	in := hookDecision(askReq(t), r)["updatedInput"].(map[string]any)
	if len(in) != 3 || in["command"] != nil {
		t.Fatalf("only questions, answers and annotations: %v", in)
	}
	if qs, _ := in["questions"].([]any); len(qs) != 1 {
		t.Errorf("its own questions: %v", in["questions"])
	}
	note := in["annotations"].(map[string]any)["Which colour?"].(map[string]any)
	if len(note) != 1 || note["notes"] != "ok" {
		t.Errorf("the note, nothing else: %v", note)
	}
}

// The TUI's holder sends a note through promptDecision and the control
// socket; the hook turns it into annotations.
func TestAnswerNotesThroughTheHolder(t *testing.T) {
	h := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, askHook)
	p := waitList(t, h)[0]
	text, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Blue"}}, nil, map[int]string{0: "cooler", 3: "no such question"})
	if err != nil || !strings.Contains(text, "note: cooler") {
		t.Fatalf("%q %v", text, err)
	}
	d := decisionOf(t, hookOutput(t, out))
	a := annotationsOf(d)
	if len(a) != 1 || a["Which colour?"].(map[string]any)["notes"] != "cooler" {
		t.Errorf("decision %v", d)
	}
}

// An answer for a prompt id no longer held (replaced by a newer prompt in
// the same session) fails and sends nothing.
func TestAnswerStaleIDSendsNothing(t *testing.T) {
	h := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, askHook)
	p := waitList(t, h)[0]
	if _, err := h.Answer(p.ID+"-old", "answer", map[int][]string{0: {"Red"}}, nil, nil); err == nil || !strings.Contains(err.Error(), "isn't waiting any more") {
		t.Fatalf("a stale id: %v", err)
	}
	if got := waitList(t, h); len(got) != 1 || got[0].ID != p.ID {
		t.Fatalf("the held prompt is untouched: %+v", got)
	}
	if _, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Red"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if d := decisionOf(t, hookOutput(t, out)); d["updatedInput"].(map[string]any)["answers"].(map[string]any)["Which colour?"] != "Red" {
		t.Errorf("only the current id's answer reached the hook: %v", d)
	}
}

// The page has a note field per question; its text goes in annotations.
func TestPageNoteField(t *testing.T) {
	s := hookServer(t, time.Minute)
	out := runHook(t, askHook)
	p := waitHeld(t, s)
	page := request(s, "GET", "/session?id=s-1", signJWT(t, testKey, "RS256", goodClaims()), nil, nil).Body.String()
	if !strings.Contains(page, `name="q0_note"`) || strings.Contains(page, "<script") {
		t.Errorf("the page offers a note field, with no script:\n%s", page)
	}
	w := postAct(t, s, url.Values{"action": {"prompt"}, "choice": {"answer"}, "prompt": {p.id}, "id": {"s-1"}, "name": {"worker"},
		"q0": {"Red"}, "q0_note": {"  warmer please "}, "q9_note": {"stray"}, "confirm": {"yes"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "note: warmer please") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	d := decisionOf(t, hookOutput(t, out))
	if a := annotationsOf(d); len(a) != 1 || a["Which colour?"].(map[string]any)["notes"] != "warmer please" {
		t.Errorf("decision %v", d)
	}
}

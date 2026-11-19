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

func askReq(t *testing.T) hookRequest {
	t.Helper()
	var req hookRequest
	if err := json.Unmarshal([]byte(askHook), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

// The hook takes free text for a question only when it's flagged as Other,
// and cleans and caps it itself whatever it was sent.
func TestHookOtherText(t *testing.T) {
	req := askReq(t)
	if d := hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Teal"}}); d != nil {
		t.Errorf("free text not flagged as Other is refused: %v", d)
	}
	d := hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "  Teal\x1b[31m\n "}, Other: []string{"Which colour?"}})
	if d == nil {
		t.Fatal("flagged free text is taken")
	}
	if got := d["updatedInput"].(map[string]any)["answers"].(map[string]any)["Which colour?"]; got != "Teal[31m" {
		t.Errorf("cleaned: %q", got)
	}
	long := strings.Repeat("é", 2500)
	d = hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": long}, Other: []string{"Which colour?"}})
	if got := d["updatedInput"].(map[string]any)["answers"].(map[string]any)["Which colour?"].(string); utf8.RuneCountInString(got) != 2000 {
		t.Errorf("capped at 2000 runes: %d", utf8.RuneCountInString(got))
	}
	if d := hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": " \x07 "}, Other: []string{"Which colour?"}}); d != nil {
		t.Errorf("empty once cleaned is refused: %v", d)
	}
	// The input it allows is still its own, with only answers added.
	if in := hookDecision(req, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Teal"}, Other: []string{"Which colour?"}})["updatedInput"].(map[string]any); in["questions"] == nil || len(in) != 2 {
		t.Errorf("input %v", in)
	}
}

// promptDecision and the control socket carry the Other flag; the TUI's
// holder answers with free text.
func TestAnswerOtherThroughTheHolder(t *testing.T) {
	h := tuiHolder(t, focusedNow(), time.Minute)
	out := runHook(t, askHook)
	p := waitList(t, h)[0]
	if len(p.Questions) != 1 || p.Questions[0].Header != "Colour" || p.Questions[0].Descriptions[0] != "warm" {
		t.Errorf("held question: %+v", p.Questions)
	}
	if _, err := h.Answer(p.ID, "answer", map[int][]string{0: {"   "}}, map[int]bool{0: true}, nil); err == nil {
		t.Error("empty Other text is refused")
	}
	if text, err := h.Answer(p.ID, "answer", map[int][]string{0: {"Teal"}}, map[int]bool{0: true}, nil); err != nil || !strings.Contains(text, "Teal") {
		t.Fatalf("%q %v", text, err)
	}
	d := decisionOf(t, hookOutput(t, out))
	if d["updatedInput"].(map[string]any)["answers"].(map[string]any)["Which colour?"] != "Teal" {
		t.Errorf("decision %v", d)
	}
}

// The page's "Other" choice sends its text as the answer.
func TestPageOtherAnswer(t *testing.T) {
	s := hookServer(t, time.Minute)
	out := runHook(t, askHook)
	p := waitHeld(t, s)
	page := request(s, "GET", "/session?id=s-1", signJWT(t, testKey, "RS256", goodClaims()), nil, nil).Body.String()
	if !strings.Contains(page, `value="__other__"`) || !strings.Contains(page, `name="q0_other"`) {
		t.Errorf("the page offers Other:\n%s", page)
	}
	w := postAct(t, s, url.Values{"action": {"prompt"}, "choice": {"answer"}, "prompt": {p.id}, "id": {"s-1"}, "name": {"worker"},
		"q0": {"__other__"}, "q0_other": {"Teal"}, "confirm": {"yes"}})
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if d := decisionOf(t, hookOutput(t, out)); d["updatedInput"].(map[string]any)["answers"].(map[string]any)["Which colour?"] != "Teal" {
		t.Errorf("decision %v", d)
	}
}

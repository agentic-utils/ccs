package mcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// suggestHook is a Bash prompt with Claude Code's suggestions: a rule ccs
// offers, a switch to bypassPermissions it never offers, and a folder.
const suggestHook = `{"session_id":"s-1","cwd":"/r","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"npm test"},` +
	`"permission_suggestions":[` +
	`{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test:*"}],"behavior":"allow","destination":"localSettings"},` +
	`{"type":"setMode","mode":"bypassPermissions","destination":"session"},` +
	`{"type":"addDirectories","directories":["/r/lib"],"destination":"session"}]}`

func suggestReq(t *testing.T) hookRequest {
	t.Helper()
	var req hookRequest
	if err := json.Unmarshal([]byte(suggestHook), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

// The menu is Claude Code's: Yes, a "don't ask again" per suggestion ccs
// offers (by its index in the input), No; never a switch that turns
// prompts off, and nothing ccs can't word.
func TestPermOptions(t *testing.T) {
	got := permOptions(suggestReq(t))
	want := []HeldOption{
		{"allow", "Yes"},
		{"always:1", "Yes, and don't ask again for Bash(npm test:*) in this project"},
		{"always:3", "Yes, and allow access to /r/lib in this session"},
		{"deny", denyLabel},
	}
	if len(got) != len(want) {
		t.Fatalf("options %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("option %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{
		`{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"deny","destination":"session"}`,
		`{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"elsewhere"}`,
		`{"type":"setMode","mode":"dontAsk","destination":"session"}`,
		`{"type":"replaceRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}`,
		`not json`,
	} {
		if l := suggestionLabel(json.RawMessage(bad)); l != "" {
			t.Errorf("%s is offered as %q", bad, l)
		}
	}
	if l := suggestionLabel(json.RawMessage(`{"type":"setMode","mode":"acceptEdits","destination":"session"}`)); l != "Yes, and allow all edits in this session" {
		t.Errorf("acceptEdits: %q", l)
	}
	if l := suggestionLabel(json.RawMessage(`{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"x\u001b[31m"}],"behavior":"allow","destination":"session"}`)); strings.ContainsRune(l, 0x1b) {
		t.Errorf("a label is cleaned: %q", l)
	}
}

// "Don't ask again" hands Claude Code the suggestion from the hook's own
// input, unchanged, as updatedPermissions; an index it doesn't offer, or
// one on a question, decides nothing, so the tab asks.
func TestHookDecisionSavesASuggestion(t *testing.T) {
	req := suggestReq(t)
	d := hookDecision(req, hookReply{Behavior: "allow", Permission: 1})
	ups, _ := d["updatedPermissions"].([]json.RawMessage)
	if d["behavior"] != "allow" || len(ups) != 1 || string(ups[0]) != string(req.PermissionSuggestions[0]) || d["updatedInput"] != nil {
		t.Fatalf("decision %v", d)
	}
	for _, n := range []int{2, 4, -1} { // bypassPermissions, out of range, nonsense
		if d := hookDecision(req, hookReply{Behavior: "allow", Permission: n}); d != nil {
			t.Errorf("permission %d: %v", n, d)
		}
	}
	if d := hookDecision(askReq(t), hookReply{Behavior: "allow", Permission: 1, Answers: map[string]string{"Which colour?": "Red"}}); d != nil {
		t.Errorf("a question saves no rule: %v", d)
	}
	if d := hookDecision(req, hookReply{Behavior: "allow"}); d["updatedPermissions"] != nil {
		t.Errorf("plain yes saves nothing: %v", d)
	}
}

// promptDecision takes the same choices as the menu: always:<n> only for
// one it offers, and deny with q0's note as what Claude should do instead.
func TestPromptDecisionPermissionChoices(t *testing.T) {
	p := &heldPrompt{req: suggestReq(t)}
	r, what, err := promptDecision(p, "always:3", nil, nil, nil, "me")
	if err != nil || r.Behavior != "allow" || r.Permission != 3 || !strings.Contains(what, "/r/lib") {
		t.Fatalf("always:3: %+v %q %v", r, what, err)
	}
	for _, c := range []string{"always:2", "always:9", "always:x"} {
		if _, _, err := promptDecision(p, c, nil, nil, nil, "me"); err == nil {
			t.Errorf("%s is refused", c)
		}
	}
	r, what, err = promptDecision(p, "deny", nil, nil, map[string]string{"q0": " run make test\x07 instead "}, "me")
	if err != nil || r.Behavior != "deny" || !strings.Contains(r.Message, "Denied from ccs by me") || !strings.Contains(r.Message, "run make test instead") || !strings.Contains(what, "note: run make test instead") {
		t.Fatalf("deny with a note: %+v %q %v", r, what, err)
	}
	if r, _, _ := promptDecision(p, "deny", nil, nil, nil, "me"); r.Message != "Denied from ccs by me" {
		t.Errorf("plain deny: %q", r.Message)
	}
	d := hookDecision(p.req, hookReply{Behavior: "deny", Message: strings.Repeat("é", 3000)})
	if m := d["message"].(string); !strings.HasPrefix(m, "éé") || len([]rune(m)) != maxDenyRunes || !json.Valid([]byte(`"`+m+`"`)) {
		t.Errorf("a long message is cut on a character: %d runes", len([]rune(m)))
	}
}

// The control socket lists a permission prompt's options and its answer op
// takes always:<n> and a deny note, end to end through the hook.
func TestControlAnswersAPermissionMenu(t *testing.T) {
	s := controlServer(t)
	out := runHook(t, suggestHook)
	p := waitHeld(t, s)
	held, err := HeldPrompts()
	if err != nil || len(held) != 1 || len(held[0].Options) != 4 || held[0].Options[1].Choice != "always:1" {
		t.Fatalf("list: %+v %v", held, err)
	}
	if _, err := AnswerHeld(p.id, "always:1", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	d := decisionOf(t, hookOutput(t, out))
	ups, _ := d["updatedPermissions"].([]any)
	if d["behavior"] != "allow" || len(ups) != 1 || ups[0].(map[string]any)["destination"] != "localSettings" {
		t.Errorf("decision %v", d)
	}

	out = runHook(t, suggestHook)
	p = waitHeld(t, s)
	if _, err := AnswerHeld(p.id, "deny", nil, nil, map[int]string{0: "use the make target"}); err != nil {
		t.Fatal(err)
	}
	d = decisionOf(t, hookOutput(t, out))
	if d["behavior"] != "deny" || !strings.Contains(d["message"].(string), "use the make target") {
		t.Errorf("deny %v", d)
	}
}

// The phone page offers the same rows: a "don't ask again" button that
// saves the rule, and No with a box for what Claude should do instead,
// which the confirm page carries through.
func TestPagePermissionMenu(t *testing.T) {
	s := hookServer(t, time.Minute)
	out := runHook(t, suggestHook)
	p := waitHeld(t, s)
	page := request(s, "GET", "/session?id=s-1", signJWT(t, testKey, "RS256", goodClaims()), nil, nil).Body.String()
	for _, want := range []string{">Yes<", `value="always:1"`, "don&#39;t ask again for Bash(npm test:*)", `name="q0_note"`, ">No, and tell Claude what to do differently<"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, "always:2") {
		t.Error("bypassPermissions is never offered")
	}
	w := postAct(t, s, url.Values{"action": {"prompt"}, "choice": {"always:1"}, "prompt": {p.id}, "id": {"s-1"}, "name": {"worker"}, "confirm": {"yes"}})
	if w.Code != http.StatusOK {
		t.Fatalf("always:1: %d %s", w.Code, w.Body)
	}
	if d := decisionOf(t, hookOutput(t, out)); d["updatedPermissions"] == nil {
		t.Errorf("decision %v", d)
	}

	out = runHook(t, suggestHook)
	p = waitHeld(t, s)
	form := url.Values{"action": {"prompt"}, "choice": {"deny"}, "prompt": {p.id}, "id": {"s-1"}, "name": {"worker"}, "q0_note": {"try make test"}}
	if w := postAct(t, s, form); !strings.Contains(w.Body.String(), "note: try make test") || !strings.Contains(w.Body.String(), `name="q0_note"`) {
		t.Errorf("the confirm page shows and carries the note:\n%s", w.Body)
	}
	form.Set("confirm", "yes")
	if w := postAct(t, s, form); w.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	if d := decisionOf(t, hookOutput(t, out)); d["behavior"] != "deny" || !strings.Contains(d["message"].(string), "try make test") {
		t.Errorf("decision %v", d)
	}
}

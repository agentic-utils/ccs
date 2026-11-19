package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
)

type sentNotice struct {
	pid      int
	parentID string
	text     string
}

// childTurnFixture: "kid" (k1, pid 11) launched by "boss" (b1, pid 10),
// both running; kid's transcript is written by the test. Returns the
// transcript path and the notices sent.
func childTurnFixture(t *testing.T) (string, *[]sentNotice) {
	t.Helper()
	dir := t.TempDir()
	oldParents, oldNotices, oldSend, oldSock, oldLive, oldCaller := sessions.ParentsPath, childNoticesPath, sendChildTurn, parentSocket, hookSessions, hookCallerName
	t.Cleanup(func() {
		sessions.ParentsPath, childNoticesPath, sendChildTurn, parentSocket, hookSessions, hookCallerName = oldParents, oldNotices, oldSend, oldSock, oldLive, oldCaller
		os.Remove(github.PREventsPath())
	})
	t.Cleanup(stubOwned(nil))
	sessions.ParentsPath = func() string { return filepath.Join(dir, "launch-parents.json") }
	childNoticesPath = func() string { return filepath.Join(dir, "child-notices.json") }
	hookCallerName = func(string) string { return "" }
	parentSocket = func(int) string { return "/tmp/cc-socks/10.sock" }
	var sent []sentNotice
	sendChildTurn = func(pid int, parentID, text string) error {
		sent = append(sent, sentNotice{pid, parentID, text})
		return nil
	}
	hookSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{
			"k1": {PID: 11, Stat: sessions.SessionStat{Status: "busy", Name: "kid"}},
			"b1": {PID: 10, Stat: sessions.SessionStat{Status: "idle", Name: "boss"}},
		}
	}
	data, _ := json.Marshal(sessions.ParentsFile{Parents: map[string]sessions.ParentRec{"kid": {Child: "kid", ChildID: "k1", Parent: "boss", ParentID: "b1"}}})
	os.WriteFile(sessions.ParentsPath(), data, 0o600)
	os.Remove(github.PREventsPath())
	return filepath.Join(dir, "k1.jsonl"), &sent
}

func turn(t *testing.T, path string, at time.Time, prompt string, said map[string]any, last ...map[string]any) {
	t.Helper()
	lines := []any{userLine(at, "earlier ask", false), assistantLine(at.Add(time.Second), say("earlier answer")), userLine(at.Add(time.Minute), prompt, false)}
	for _, b := range last {
		lines = append(lines, assistantLine(at.Add(70*time.Second), b))
	}
	lines = append(lines, assistantLine(at.Add(80*time.Second), said))
	writeLines(t, path, lines...)
}

func stop(path string) stopRequest {
	return stopRequest{SessionID: "k1", TranscriptPath: path, Event: "Stop"}
}

func childEvents(t *testing.T) []ChildTurnEvent {
	t.Helper()
	f, err := os.Open(github.PREventsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ChildTurnEvent
	for s := bufio.NewScanner(f); s.Scan(); {
		var e ChildTurnEvent
		if json.Unmarshal(s.Bytes(), &e) == nil && e.Event == "child_turn" {
			out = append(out, e)
		}
	}
	return out
}

func TestChildTurnNotice(t *testing.T) {
	path, sent := childTurnFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	turn(t, path, now.Add(-2*time.Minute), "fix the login test", say("Fixed the flake. Should I also bump the timeout?"))

	stopHook(strings.NewReader(`{"session_id":"k1","transcript_path":"`+path+`","hook_event_name":"Stop"}`), now)
	if len(*sent) != 1 {
		t.Fatalf("one notice: %+v", *sent)
	}
	n := (*sent)[0]
	if n.pid != 10 || n.parentID != "b1" {
		t.Errorf("to boss's pid and id: %+v", n)
	}
	for _, want := range []string{"kid finished a turn · now waiting_on_user", `last: "Fixed the flake. Should I also bump the timeout?"`} {
		if !strings.Contains(n.text, want) {
			t.Errorf("notice lacks %q:\n%s", want, n.text)
		}
	}
	evs := childEvents(t)
	if len(evs) != 1 || !evs[0].Delivered || evs[0].Parent != "boss" || evs[0].Session != "kid" || evs[0].Bucket != BucketWaitingOnUser {
		t.Errorf("one delivered child_turn event: %+v", evs)
	}

	// The same bucket again within the debounce: nothing. A new bucket: told.
	if _, ok := childTurn(stop(path), now.Add(10*time.Second)); ok {
		t.Error("same bucket within 30s is skipped")
	}
	turn(t, path, now, "carry on", say("All done."))
	e, ok := childTurn(stop(path), now.Add(20*time.Second))
	if !ok || e.Bucket != BucketDone {
		t.Errorf("a new bucket inside the window is told: %v %+v", ok, e)
	}
	turn(t, path, now, "and again", say("All done."))
	if _, ok := childTurn(stop(path), now.Add(80*time.Second)); !ok {
		t.Error("past the window the same bucket is told again")
	}

	// stop_hook_active, or another event: nothing at all.
	*sent = nil
	stopHook(strings.NewReader(`{"session_id":"k1","transcript_path":"`+path+`","hook_event_name":"Stop","stop_hook_active":true}`), now.Add(5*time.Minute))
	stopHook(strings.NewReader(`{"session_id":"k1","hook_event_name":"SubagentStop"}`), now.Add(5*time.Minute))
	if len(*sent) != 0 {
		t.Errorf("stop_hook_active and other events send nothing: %+v", *sent)
	}
}

func TestChildTurnSkips(t *testing.T) {
	path, sent := childTurnFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	at := now.Add(-2 * time.Minute)

	// Started only by an automatic ccs notice: not told, so turns don't cascade up the tree.
	turn(t, path, at, strings.Replace(viaCCSMsg, "the user, via ccs", "ccs watch", 1), say("Fixed the failing check."))
	if _, ok := childTurn(stop(path), now); ok {
		t.Error("a turn a ccs watch nudge started isn't told")
	}
	turn(t, path, at, "Another Claude session sent a message:\n<cross-session-message from-name=\"ccs hook\">\n[x]\n\nkid2 finished a turn\n</cross-session-message>", say("ok"))
	if _, ok := childTurn(stop(path), now); ok {
		t.Error("a turn a child-turn notice started isn't told")
	}

	// Its last action told the parent itself: not told again.
	sendTo := func(to string) map[string]any {
		return map[string]any{"type": "tool_use", "name": "SendMessage", "input": map[string]any{"to": to, "message": "HANDOVER x"}}
	}
	for _, to := range []string{"boss", "boss [be0c4d]", "uds:/tmp/cc-socks/10.sock"} {
		turn(t, path, at, "open the PR", say("Sent."), sendTo(to))
		if _, ok := childTurn(stop(path), now); ok {
			t.Errorf("SendMessage to the parent as %q isn't told", to)
		}
	}
	turn(t, path, at, "open the PR", say("Sent."), sendTo("someone-else"))
	if _, ok := childTurn(stop(path), now); !ok {
		t.Error("SendMessage to someone else is told")
	}

	// Turned off for boss: nothing; on again: told.
	if err := SetChildNotices("boss", false, now); err != nil {
		t.Fatal(err)
	}
	turn(t, path, at, "next", say("Should I go on?"))
	if _, ok := childTurn(stop(path), now.Add(time.Hour)); ok {
		t.Error("off for boss: not told")
	}
	SetChildNotices("boss", true, now)
	if _, ok := childTurn(stop(path), now.Add(2*time.Hour)); !ok {
		t.Error("on again: told")
	}
	if len(*sent) != 2 {
		t.Errorf("two notices sent in all: %d", len(*sent))
	}
}

func TestChildTurnNoParent(t *testing.T) {
	path, sent := childTurnFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	turn(t, path, now.Add(-2*time.Minute), "go", say("Should I go on?"))

	// The parent isn't running: an event saying so, nothing sent.
	hookSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{"k1": {PID: 11, Stat: sessions.SessionStat{Name: "kid"}}}
	}
	e, ok := childTurn(stop(path), now)
	if !ok || e.Delivered || e.Error != "parent not running" || len(*sent) != 0 {
		t.Errorf("parent closed: logged, not sent: %v %+v %+v", ok, e, *sent)
	}
	// Two running sessions named like the parent, neither its id: never guess.
	hookSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{
			"k1": {PID: 11, Stat: sessions.SessionStat{Name: "kid"}},
			"x1": {PID: 20, Stat: sessions.SessionStat{Name: "boss"}},
			"x2": {PID: 21, Stat: sessions.SessionStat{Name: "boss"}},
		}
	}
	if e, _ := childTurn(stop(path), now.Add(time.Hour)); e.Delivered || len(*sent) != 0 {
		t.Errorf("an ambiguous parent name gets nothing: %+v", e)
	}
	// Under a parent that /clear'ed: the one running by its name.
	hookSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{
			"k1": {PID: 11, Stat: sessions.SessionStat{Name: "kid"}},
			"b2": {PID: 30, Stat: sessions.SessionStat{Name: "boss"}},
		}
	}
	if e, _ := childTurn(stop(path), now.Add(2*time.Hour)); !e.Delivered || len(*sent) != 1 || (*sent)[0].pid != 30 || e.ParentID != "b2" {
		t.Errorf("a parent with a new id is found by name: %+v %+v", e, *sent)
	}
	// A session with no parent record: nothing.
	os.WriteFile(sessions.ParentsPath(), []byte(`{"parents":{}}`), 0o600)
	if _, ok := childTurn(stop(path), now.Add(3*time.Hour)); ok {
		t.Error("no parent: nothing")
	}
}

func TestChildTurnFastPath(t *testing.T) {
	path, sent := childTurnFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	turn(t, path, now.Add(-2*time.Minute), "go", say("Should I go on?"))
	scans := 0
	inner := hookSessions
	hookSessions = func() map[string]sessions.LiveSession { scans++; return inner() }

	// Records all carry child ids and none is this session's: no scan at all.
	os.WriteFile(sessions.ParentsPath(), []byte(`{"parents":{"other":{"child":"other","child_id":"o1","parent":"boss","parent_id":"b1"}}}`), 0o600)
	if _, ok := childTurn(stop(path), now); ok || scans != 0 {
		t.Errorf("not a child: no notice and no scan: %v %d", ok, scans)
	}
	// A record by name only (no child id yet): the scan finds it by name.
	os.WriteFile(sessions.ParentsPath(), []byte(`{"parents":{"kid":{"child":"kid","parent":"boss","parent_id":"b1"}}}`), 0o600)
	if e, ok := childTurn(stop(path), now); !ok || !e.Delivered || scans != 1 || len(*sent) != 1 {
		t.Errorf("matched by name after one scan: %v %+v %d", ok, e, scans)
	}
	// The hook's claude process names it: found without the id.
	hookCallerName = func(string) string { return "kid" }
	if e, ok := childTurn(stop(path), now.Add(time.Hour)); !ok || !e.Delivered {
		t.Errorf("named by its own process: %v %+v", ok, e)
	}
}

func TestChildNoticesCLIAndTool(t *testing.T) {
	childTurnFixture(t)
	var out, errb bytes.Buffer
	if code := RunHook([]string{"child-notices", "off", "--orchestrator", "boss"}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "off for boss") {
		t.Fatalf("CLI off: %d %q %q", code, out.String(), errb.String())
	}
	data, _ := os.ReadFile(childNoticesPath())
	if !strings.Contains(string(data), `"boss"`) {
		t.Errorf("recorded off: %s", data)
	}
	if text, err := Call("child_notices", json.RawMessage(`{"enabled":true,"orchestrator":"boss"}`), nil); err != nil || text != "child-turn notices on for boss" {
		t.Errorf("tool on: %v %q", err, text)
	}
	if _, err := Call("child_notices", json.RawMessage(`{"orchestrator":"boss"}`), nil); err == nil {
		t.Error("enabled is required")
	}
	if code := RunHook([]string{"child-notices", "maybe"}, nil, &out, &errb); code != 2 {
		t.Error("bad argument: usage")
	}
}

func TestHookInstallBoth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify-me"}]}]}}`), 0o600)
	msg, err := editHookSettings(path, true)
	if err != nil || !strings.Contains(msg, "PermissionRequest, Stop and SessionStart hooks") {
		t.Fatalf("installs both: %v %q", err, msg)
	}
	var s struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &s)
	stops := s.Hooks["Stop"]
	if len(stops) != 2 || !isOurHookFor(stops[1], "stop") || stops[1]["matcher"] != nil || len(s.Hooks["PermissionRequest"]) != 1 {
		t.Errorf("Stop keeps the user's own hook and gains ccs's, with no matcher: %s", b)
	}
	// Only the Stop hook missing (installed before it existed): just that is added.
	editHookSettings(path, false)
	editHookSettings(path, true)
	raw, _ := os.ReadFile(path)
	var m map[string]any
	json.Unmarshal(raw, &m)
	hooks := m["hooks"].(map[string]any)
	hooks["Stop"] = withoutOursFor(hooks["Stop"].([]any), "stop")
	out, _ := json.Marshal(m)
	os.WriteFile(path, out, 0o600)
	if msg, _ := editHookSettings(path, true); !strings.Contains(msg, "added ccs's Stop hook") {
		t.Errorf("adds only the missing one: %q", msg)
	}
	if msg, _ := editHookSettings(path, true); !strings.Contains(msg, "already") {
		t.Errorf("both there: already: %q", msg)
	}
	if msg, _ := editHookSettings(path, false); !strings.Contains(msg, "removed ccs's PermissionRequest, Stop and SessionStart hooks") {
		t.Errorf("removes both: %q", msg)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "hook stop") || !strings.Contains(string(b), "notify-me") {
		t.Errorf("the user's Stop hook stays: %s", b)
	}
	var printed bytes.Buffer
	RunHook([]string{"install", "--print"}, nil, &printed, &printed)
	if !strings.Contains(printed.String(), "hooks.PermissionRequest") || !strings.Contains(printed.String(), "hooks.Stop") {
		t.Errorf("--print shows both: %s", printed.String())
	}
}

func TestStopHookDeadline(t *testing.T) {
	path, _ := childTurnFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	turn(t, path, now.Add(-2*time.Minute), "go", say("Should I go on?"))
	oldDeadline := stopHookDeadline
	defer func() { stopHookDeadline = oldDeadline }()
	stopHookDeadline = 50 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	sendChildTurn = func(int, string, string) error { <-release; return nil } // a socket that never answers
	start := time.Now()
	stopHook(strings.NewReader(`{"session_id":"k1","transcript_path":"`+path+`","hook_event_name":"Stop"}`), now)
	if d := time.Since(start); d > time.Second {
		t.Errorf("a stuck send holds the turn's end for %s", d)
	}
}

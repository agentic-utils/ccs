package sessions

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// handoverFixture is newGracefulFixture's worker with a running parent
// called lead (its own process and session file with a socket), recorded
// as worker's parent, and a session helper recorded under worker. Claims
// moves are recorded in moves.
type handoverFixture struct {
	*gracefulFixture
	leadPID int
	moves   [][2]string
	notices map[int][]string
}

const leadSock = "/tmp/ccs-test-lead.sock"

func newHandoverFixture(t *testing.T) *handoverFixture {
	t.Helper()
	g := newGracefulFixture(t)
	lead := exec.Command("sleep", "30")
	if err := lead.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lead.Process.Kill(); lead.Wait() })
	h := &handoverFixture{gracefulFixture: g, leadPID: lead.Process.Pid, notices: map[int][]string{}}
	body := fmt.Sprintf(`{"pid":%d,"sessionId":"id-lead","name":"lead","status":"idle","messagingSocketPath":%q}`, h.leadPID, leadSock)
	os.WriteFile(filepath.Join(GetSessionsDir(), fmt.Sprintf("%d.json", h.leadPID)), []byte(body), 0o600)
	os.Remove(ParentsPath())
	t.Cleanup(func() { os.Remove(ParentsPath()) })
	if err := SetParents([]ParentRec{
		{Child: "worker", ChildID: g.id, Parent: "lead", ParentID: "id-lead"},
		{Child: "helper", Parent: "worker", ParentID: g.id},
		{Child: "bystander", Parent: "someone-else"},
	}); err != nil {
		t.Fatal(err)
	}
	old := MoveClaims
	t.Cleanup(func() { MoveClaims = old })
	MoveClaims = func(from, to string) ([]string, error) {
		h.moves = append(h.moves, [2]string{from, to})
		return []string{"https://github.com/o/r/pull/1"}, nil
	}
	return h
}

// sendMessage appends worker's SendMessage call to to, as Claude Code logs it.
func (h *handoverFixture) sendMessage(t *testing.T, to string) {
	f, err := os.OpenFile(h.transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintf(f, `{"type":"assistant","sessionId":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"SendMessage","input":{"to":%q,"message":"Finished: the fix. Left: PR 1 awaits review."}}]}}`+"\n", h.id, to)
}

// stubNotices records each notice by pid; the worker reads its notice, runs
// handOver (when not nil) and goes idle.
func (h *handoverFixture) stubNotices(t *testing.T, handOver func()) {
	SendCloseNotice = func(pid int, f SessionFile, text string) error {
		h.notices[pid] = append(h.notices[pid], text)
		if pid == h.pid {
			h.say("user", "<cross-session-message from-name=\"ccs close\">\n"+CloseNote+text+"\n</cross-session-message>")
			if handOver != nil {
				handOver()
			}
			h.say("assistant", "Handed over to lead.")
			h.setStatus("idle", time.Now().Add(time.Second))
		}
		return nil
	}
}

// The notice names the parent and its address; the close waits for idle and
// stops it; claims and children move to the parent, which is told the
// handover arrived.
func TestHandoverCloseHandsToTheParent(t *testing.T) {
	h := newHandoverFixture(t)
	h.stubNotices(t, func() { h.sendMessage(t, "lead") })
	msg, err := CloseHandover("worker", "", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.signalled != 1 || len(h.tabs) != 1 {
		t.Errorf("stopped once and its tab closed: %d, %v", h.signalled, h.tabs)
	}
	notice := strings.Join(h.notices[h.pid], "")
	for _, want := range []string{"handed to its parent, lead", `to: "lead"`, "uds:" + leadSock, "SendMessage", "leftover work", "worktrees and their branches", "pid and command", "handed over to lead", "1m0s"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice lacks %q: %s", want, notice)
		}
	}
	if len(h.moves) != 1 || h.moves[0] != [2]string{h.id, "lead"} {
		t.Errorf("its claims move to lead: %v", h.moves)
	}
	f := LoadParents()
	if r := f.Parents["helper"]; r.Parent != "lead" || r.ParentID != "id-lead" || r.Source != "handover" {
		t.Errorf("its child moves to lead: %+v", r)
	}
	if r := f.Parents["bystander"]; r.Parent != "someone-else" {
		t.Errorf("others' children stay: %+v", r)
	}
	told := strings.Join(h.notices[h.leadPID], "")
	for _, want := range []string{"worker was closed", "seen in its transcript", "https://github.com/o/r/pull/1", "child sessions: helper"} {
		if !strings.Contains(told, want) {
			t.Errorf("the parent's notice lacks %q: %s", want, told)
		}
	}
	if strings.Contains(told, "not seen") {
		t.Errorf("the handover was seen: %s", told)
	}
	for _, want := range []string{"closed worker", "handed over to lead (its handover message: seen)", "1 PR claim(s), 1 child session(s)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("result lacks %q: %s", want, msg)
		}
	}
}

// A session that never messages its parent is still closed, and the parent
// is told it wasn't seen, with the session's last message.
func TestHandoverCloseTellsTheParentWhenNoHandoverCame(t *testing.T) {
	h := newHandoverFixture(t)
	h.stubNotices(t, func() { h.sendMessage(t, "someone-else") })
	msg, err := CloseHandover("worker", "", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	told := strings.Join(h.notices[h.leadPID], "")
	if !strings.Contains(told, "not seen in its transcript") || !strings.Contains(told, "Its last message:\nHanded over to lead.") {
		t.Errorf("the parent hears it wasn't seen, with its last message: %s", told)
	}
	if !strings.Contains(msg, "its handover message: not seen") || h.signalled != 1 {
		t.Errorf("closed anyway: %s, %d", msg, h.signalled)
	}
}

// No running parent: nothing is sent and nothing stopped.
func TestHandoverCloseRefusesWithoutARunningParent(t *testing.T) {
	for _, c := range []struct {
		name string
		rec  *ParentRec
		want string
	}{
		{"no record", nil, "worker has no running parent: use ^X K to close it, or adopt it first"},
		{"a parent that isn't running", &ParentRec{Child: "worker", Parent: "ghost", ParentID: "id-ghost"}, "worker's parent ghost isn't running"},
		{"a Codex parent", &ParentRec{Child: "worker", Parent: "cx", ParentKind: "codex"}, "is a Codex session"},
		{"an agy parent", &ParentRec{Child: "worker", Parent: "gem", ParentKind: "agy"}, "is an agy session"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHandoverFixture(t)
			WithParents(func(f *ParentsFile) bool {
				delete(f.Parents, "worker")
				if c.rec != nil {
					f.Parents["worker"] = *c.rec
				}
				return true
			})
			h.stubNotices(t, nil)
			_, err := CloseHandover("worker", "", time.Minute, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refused with %q: %v", c.want, err)
			}
			if _, perr := HandoverParentName("worker"); perr == nil || perr.Error() != err.Error() {
				t.Errorf("HandoverParentName says the same: %v", perr)
			}
			if len(h.notices) != 0 || h.signalled != 0 || len(h.moves) != 0 {
				t.Errorf("nothing sent, stopped or moved: %v, %d, %v", h.notices, h.signalled, h.moves)
			}
		})
	}
}

// ccs close --handover goes through CloseHandover; a refusal exits 1.
func TestRunCloseHandover(t *testing.T) {
	h := newHandoverFixture(t)
	h.stubNotices(t, func() { h.sendMessage(t, "uds:"+leadSock) })
	var out, errOut strings.Builder
	if code := RunClose([]string{"worker", "--handover", "--timeout", "1m"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "handed over to lead (its handover message: seen)") {
		t.Errorf("a handover by address counts as seen: %s", out.String())
	}
	errOut.Reset()
	if code := RunClose([]string{"--handover", "nobody"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "nobody has no running parent") {
		t.Errorf("no parent: exit %d, %s", code, errOut.String())
	}
}

// The notice can't be sent: nothing is stopped or moved.
func TestHandoverCloseStopsNothingWhenTheNoticeCantBeSent(t *testing.T) {
	h := newHandoverFixture(t)
	SendCloseNotice = func(int, SessionFile, string) error { return errors.New("socket gone") }
	if _, err := CloseHandover("worker", "", time.Minute, nil); err == nil || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Fatalf("refused: %v", err)
	}
	if h.signalled != 0 || len(h.moves) != 0 || slices.ContainsFunc([]string{"helper"}, func(c string) bool { return LoadParents().Parents[c].Parent != "worker" }) {
		t.Error("nothing stopped or moved")
	}
}

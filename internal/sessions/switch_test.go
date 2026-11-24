package sessions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// switchFixture is gracefulFixture's worker, in a folder that exists, in
// tmux, with every agent installed and the launch, git and hooks stubbed.
type switchFixture struct {
	*gracefulFixture
	cwd       string
	launches  []LaunchOpts
	stopsSeen []int
	launchErr error
	moved     [][2]string
	claimed   []string
}

func newSwitchFixture(t *testing.T) *switchFixture {
	t.Helper()
	s := &switchFixture{gracefulFixture: newGracefulFixture(t), cwd: t.TempDir()}
	// A later user line in a folder that exists: /code/app doesn't, so it wins.
	f, _ := os.OpenFile(s.transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintf(f, `{"type":"user","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"and add a test"}}`+"\n", s.id, s.cwd, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(f, `{"type":"assistant","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"Test added, PR next."}]}}`+"\n", s.id, s.cwd, time.Now().UTC().Format(time.RFC3339))
	f.Close()
	t.Setenv("TMUX", "/tmp/tmux-test,1,0")
	oldLaunch, oldLook, oldGit, oldCaller, oldMove, oldClaim, oldPRs, oldRetry := switchLaunch, LookBin, gitLines, IsCaller, MoveClaims, ClaimUnowned, SwitchPRs, RestartRetry
	t.Cleanup(func() {
		switchLaunch, LookBin, gitLines, IsCaller, MoveClaims, ClaimUnowned, SwitchPRs, RestartRetry = oldLaunch, oldLook, oldGit, oldCaller, oldMove, oldClaim, oldPRs, oldRetry
	})
	RestartRetry = 0
	parents := filepath.Join(t.TempDir(), "launch-parents.json")
	oldParents := ParentsPath
	t.Cleanup(func() { ParentsPath = oldParents })
	ParentsPath = func() string { return parents }
	IsCaller = func(int) bool { return false }
	LookBin = func(bin string) (string, error) { return "/usr/local/bin/" + bin, nil }
	gitLines = func(cwd string) []string { return []string{"Git: branch feat/x in " + cwd} }
	SwitchPRs = func(transcript.Conversation) ([]string, []string) {
		return []string{"https://github.com/o/r/pull/7 Fix it · checks running"}, []string{"https://github.com/o/r/pull/7"}
	}
	MoveClaims = func(from, to string) ([]string, error) {
		s.moved = append(s.moved, [2]string{from, to})
		return []string{"https://github.com/o/r/pull/5"}, nil
	}
	ClaimUnowned = func(prs []string, to string) []string { s.claimed = append(s.claimed, prs...); return prs }
	switchLaunch = func(o LaunchOpts) (Launched, error) {
		s.launches = append(s.launches, o)
		s.stopsSeen = append(s.stopsSeen, s.signalled)
		if s.launchErr != nil {
			return Launched{}, s.launchErr
		}
		return Launched{PID: 4243, TTY: "/dev/ttys052", thread: "thread-new"}, nil
	}
	return s
}

func TestSwitchAsksAClaudeSessionForANoteThenLaunchesTheOtherAgent(t *testing.T) {
	s := newSwitchFixture(t)
	SetParents([]ParentRec{
		{Child: "worker", ChildID: s.id, Parent: "lead", ParentID: "id-lead"},
		{Child: "kid", ChildID: "id-kid", Parent: "worker", ParentID: s.id},
	})
	var notice string
	SendCloseNotice = func(pid int, f SessionFile, text string) error {
		notice = text
		s.say("user", "<cross-session-message from-name=\"ccs close\">\n"+CloseNote+text+"\n</cross-session-message>")
		s.say("assistant", "Handover: PR 7 is open, the docs are left.")
		s.setStatus("idle", time.Now().Add(time.Second))
		return nil
	}
	msg, err := SwitchSession(SwitchOpts{Name: "worker", To: "codex", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Codex", "only a brief", "keep your worktrees"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice lacks %q: %s", want, notice)
		}
	}
	if s.signalled != 1 || len(s.launches) != 1 || s.stopsSeen[0] != 1 {
		t.Fatalf("closed first, then launched once: %d stops, %d launches, stops seen %v", s.signalled, len(s.launches), s.stopsSeen)
	}
	o := s.launches[0]
	if o.Name != "worker" || !o.Codex || o.Agy || o.Cwd != s.cwd || o.Resume != "" || o.RecordParent {
		t.Errorf("a new codex session with the same name and folder: %+v", o)
	}
	for _, want := range []string{SwitchNote, "Claude session (id " + s.id, "Codex", "do the thing", "Handover: PR 7 is open", "pull/7 Fix it", "branch feat/x", "Test added, PR next.", "not your memory"} {
		if want == "not your memory" {
			want = "don't have its memory"
		}
		if !strings.Contains(o.Brief, want) {
			t.Errorf("the brief lacks %q:\n%s", want, o.Brief)
		}
	}
	if strings.Contains(o.Brief, "about to be closed") || strings.Contains(o.Brief, "moving to a new") {
		t.Errorf("the notice itself stays out of the brief:\n%s", o.Brief)
	}
	p := LoadParents().Parents
	if p["worker"].Parent != "lead" || p["worker"].ChildID != "thread-new" || p["worker"].Source != "switch" {
		t.Errorf("it keeps its parent, under its new id: %+v", p["worker"])
	}
	if p["kid"].Parent != "worker" || p["kid"].ParentID != "thread-new" || p["kid"].ParentKind != transcript.KindCodex {
		t.Errorf("its child moves to the new session: %+v", p["kid"])
	}
	if len(s.moved) != 1 || s.moved[0] != [2]string{s.id, "worker"} || len(s.claimed) != 1 {
		t.Errorf("its claims move, and its open PRs are claimed: %v %v", s.moved, s.claimed)
	}
	if !strings.Contains(msg, "switched worker from Claude to Codex") || !strings.Contains(msg, "2 PR claim(s), 1 child session(s)") {
		t.Errorf("the result: %s", msg)
	}
}

func TestSwitchRefusesBeforeClosingAnything(t *testing.T) {
	s := newSwitchFixture(t)
	for _, c := range []struct {
		to, want string
		look     func(string) (string, error)
	}{
		{"claude", "already a Claude session", nil},
		{"cobol", "no agent", nil},
		{"agy", "Gemini isn't installed", func(string) (string, error) { return "", errors.New("not found") }},
	} {
		if c.look != nil {
			LookBin = c.look
		}
		_, err := SwitchSession(SwitchOpts{Name: "worker", To: c.to})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("to %s: %v", c.to, err)
		}
	}
	IsCaller = func(int) bool { return true }
	LookBin = func(bin string) (string, error) { return bin, nil }
	if _, err := SwitchSession(SwitchOpts{Name: "worker", To: "codex"}); err == nil || !strings.Contains(err.Error(), "can't switch itself") {
		t.Errorf("a session switching itself: %v", err)
	}
	if s.signalled != 0 || len(s.launches) != 0 {
		t.Errorf("nothing closed or launched: %d, %d", s.signalled, len(s.launches))
	}
}

func TestSwitchLeavesASessionThatIsntRunningAlone(t *testing.T) {
	s := newSwitchFixture(t)
	os.Remove(filepath.Join(GetSessionsDir(), fmt.Sprintf("%d.json", s.pid)))
	if _, err := SwitchSession(SwitchOpts{Name: "worker", To: "agy"}); err == nil || !strings.Contains(err.Error(), "give its session id") {
		t.Errorf("not running and no record: %v", err)
	}
	SetParents([]ParentRec{{Child: "worker", ChildID: s.id, Parent: "lead"}})
	msg, err := SwitchSession(SwitchOpts{Name: "worker", To: "agy"})
	if err != nil {
		t.Fatal(err)
	}
	if s.signalled != 0 || len(s.launches) != 1 || !s.launches[0].Agy {
		t.Fatalf("nothing stopped, agy launched: %d, %+v", s.signalled, s.launches)
	}
	if b := s.launches[0].Brief; strings.Contains(b, "handover note") || !strings.Contains(b, "do the thing") {
		t.Errorf("no note asked for, the conversation carried:\n%s", b)
	}
	if !strings.Contains(msg, "wasn't running, left as it was") {
		t.Errorf("the result: %s", msg)
	}
}

func TestSwitchSaysHowToBringTheOldOneBackWhenTheLaunchFails(t *testing.T) {
	s := newSwitchFixture(t)
	os.Remove(filepath.Join(GetSessionsDir(), fmt.Sprintf("%d.json", s.pid)))
	s.launchErr = errors.New("iTerm said no")
	_, err := SwitchSession(SwitchOpts{Name: "worker", ID: s.id, To: "codex"})
	if err == nil || !strings.Contains(err.Error(), "ccs launch --name worker --resume "+s.id) || len(s.launches) != 2 {
		t.Errorf("tried twice, then says how to bring it back: %d launches, %v", len(s.launches), err)
	}
}

func TestSwitchBriefStaysWithinItsCaps(t *testing.T) {
	conv := &transcript.Conversation{}
	conv.Messages = append(conv.Messages, transcript.Message{Role: "user", Text: "first " + strings.Repeat("a", 9000)})
	for i := range 200 {
		role := []string{"user", "assistant"}[i%2]
		conv.Messages = append(conv.Messages, transcript.Message{Role: role, Text: fmt.Sprintf("m%d %s", i, strings.Repeat("b", 3000))})
	}
	conv.Messages = append(conv.Messages, transcript.Message{Role: "user", Text: "<system-reminder>noise</system-reminder>"})
	first, note, recent := briefMessages(conv, "")
	if !strings.HasPrefix(first, "first ") || note != "" {
		t.Errorf("first %q note %q", first[:10], note)
	}
	if len(recent) == 0 || len(recent) > briefRecentMax || recent[len(recent)-1].Text[:4] != "m199" || strings.HasPrefix(recent[0].Text, "first ") {
		t.Errorf("the newest messages, within the cap: %d", len(recent))
	}
	_, _, short := briefMessages(&transcript.Conversation{Messages: conv.Messages[:3]}, "")
	if len(short) != 2 {
		t.Errorf("the first prompt isn't repeated among the recent messages: %d", len(short))
	}
	b := switchBrief(briefIn{name: "w", from: Agents[0], to: Agents[1], first: "## a heading\nline", recent: recent})
	if !strings.Contains(b, "> ## a heading\n> line") {
		t.Error("the first prompt is quoted")
	}
	if n := len([]rune(b)); n > 30000 {
		t.Errorf("brief is %d characters", n)
	}
	if strings.Contains(b, "noise") {
		t.Error("harness notes stay out")
	}
}

func TestSwitchNameAndAgents(t *testing.T) {
	for in, want := range map[string]string{"ccs-worker": "ccs-worker", "Fix the Login flow!": "fix-the-login-flow", "": "switched"} {
		if got := SwitchName(in); got != want {
			t.Errorf("SwitchName(%q) = %q", in, got)
		}
	}
	if a, ok := AgentByID("Gemini"); !ok || a.Bin != "agy" {
		t.Errorf("gemini is agy: %+v", a)
	}
	if AgentOf("").ID != "claude" || AgentOf(transcript.KindCodex).Label != "Codex" {
		t.Error("AgentOf")
	}
}

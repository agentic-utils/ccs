package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestEnterOnLiveSessionOpensItsTab(t *testing.T) {
	dir := t.TempDir()
	old := sessions.GetSessionsDir
	sessions.GetSessionsDir = func() string { return dir }
	defer func() { sessions.GetSessionsDir = old }()
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"s"}`, os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	t.Setenv("TERM_PROGRAM", "") // no terminal to focus in the test

	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.live = map[string]bool{"s": true}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // the first puts typing in its message box
	if m = res.(model); !m.chatFocus || m.resuming {
		t.Fatal("the first enter on a live session should focus its message box, nothing else")
	}
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	if m.selected != nil || m.quitting || cmd == nil {
		t.Fatal("enter on a live session must focus it (in the background), not resume a second copy")
	}
	res, _ = m.Update(firstOfBatch(cmd)) // the fast refresh for the live selection rides along
	if m = res.(model); !strings.Contains(m.errorMsg, "^F") {
		t.Errorf("unfocusable live session should point at fork, got %q", m.errorMsg)
	}
}

func TestCtrlFForksInPlaceOutsideTabbedTerminals(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TERM_PROGRAM", "")
	flags := make([]string, 1, 4) // spare capacity: fork must not write into it
	flags[0] = "--plan"
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", flags)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	m = res.(model)
	if cmd == nil {
		t.Fatal("ctrl+f should start opening the fork")
	}
	res, _ = m.Update(cmd())
	m = res.(model)
	if m.selected == nil || !m.fork || !m.quitting {
		t.Error("ctrl+f should select the conversation for a forked resume")
	}
	if len(m.claudeFlags) != 1 || m.claudeFlags[:2][1] == "--fork-session" {
		t.Error("fork flag must not leak into the shared claude flags")
	}
}

func TestLiveMsgUpdatesAndResortsKeepingCursor(t *testing.T) {
	mk := func(id, ts string) transcript.Conversation {
		return transcript.Conversation{SessionID: id, LastTimestamp: ts, Messages: []transcript.Message{{Role: "user", Text: id}}}
	}
	m := initialModel(buildItems([]transcript.Conversation{mk("a", "2026-09-25T10:00:00Z"), mk("b", "2026-09-25T09:00:00Z")}), "", nil)
	m.cursor = 1 // on b
	grown := mk("b", "2026-09-25T11:00:00Z")
	grown.Messages = append(grown.Messages, transcript.Message{Role: "assistant", Text: "new"})
	res, cmd := m.Update(liveMsg{live: map[string]bool{"b": true}, updated: []transcript.Conversation{grown}})
	m = res.(model)
	if cmd == nil || !m.live["b"] {
		t.Fatal("live tick should apply liveness and reschedule")
	}
	if m.items[0].conv.SessionID != "b" || len(m.items[0].conv.Messages) != 2 {
		t.Errorf("updated session should move to the top with its new message: %+v", m.items[0].conv)
	}
	if m.filtered[m.cursor].conv.SessionID != "b" {
		t.Error("cursor should follow the same conversation")
	}

	// Content updates wait while a prompt is open; liveness still applies.
	m.confirmDelete = true
	res, _ = m.Update(liveMsg{live: map[string]bool{}, updated: []transcript.Conversation{mk("a", "2026-09-25T12:00:00Z")}})
	if m = res.(model); m.items[0].conv.SessionID != "b" || m.live["b"] {
		t.Error("prompt open: no reshuffle, but liveness should update")
	}
}

func TestUnknownLiveSessionTriggersEarlyScanOnce(t *testing.T) {
	m := initialModel(nil, "", nil)
	scans := 0
	m.reload = func() ([]listItem, error) { scans++; return []listItem{}, nil }
	res, cmd := m.Update(liveMsg{live: map[string]bool{"new": true}, unknown: []string{"new"}})
	m = res.(model)
	if m.refreshStarted.IsZero() || cmd == nil {
		t.Fatal("an unlisted live session should start a scan now")
	}
	// The scheduled tick arriving mid-scan doesn't start a second scan.
	res, _ = m.Update(transcript.RefreshTickMsg{})
	if m = res.(model); scans != 0 {
		t.Error("tick during an early scan should only reschedule")
	}
	// The early scan's result doesn't start its own one-minute chain.
	res, cmd = m.Update(refreshMsg{items: []listItem{}, early: true})
	if m = res.(model); cmd != nil {
		t.Error("early scan must not add a second refresh schedule")
	}
	// And kicks are rate-limited.
	res, _ = m.Update(liveMsg{live: map[string]bool{"new": true}, unknown: []string{"new"}})
	if m = res.(model); !m.refreshStarted.IsZero() {
		t.Error("a second kick within 15s should wait")
	}
}

func TestStaleLiveResultDoesNotRollBackScan(t *testing.T) {
	now := time.Now()
	scanned := transcript.Conversation{SessionID: "s", Size: 300, LastTimestamp: "2026-09-25T11:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "new"}}, ReadAt: now}
	liveOld := scanned
	liveOld.Size, liveOld.ReadAt = 200, now.Add(-time.Second)
	m := initialModel(buildItems([]transcript.Conversation{scanned}), "", nil)
	m.applyLive([]transcript.Conversation{liveOld})
	if m.items[0].conv.Size != 300 {
		t.Error("a live read older than the list's copy must be ignored")
	}
}

func TestDoubleEnterOpensOnce(t *testing.T) {
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	res, first := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	_, second := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if first == nil || second != nil {
		t.Error("a second Enter while the first is opening must be ignored")
	}
}

func TestSecondEnterAfterTabOpenDoesNotLaunchAgain(t *testing.T) {
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	res, _ := m.Update(resumeDoneMsg{conv: transcript.Conversation{SessionID: "s"}, opened: true})
	m = res.(model)
	// A live tick before claude writes its session file must keep it live.
	res, _ = m.Update(liveMsg{live: map[string]bool{}})
	if m = res.(model); !m.live["s"] {
		t.Error("a session ccs just opened should stay live until its file appears")
	}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); cmd != nil {
		t.Error("enter again must not start another resume")
	}
}

func TestUnlistableLiveSessionScansOnce(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.reload = func() ([]listItem, error) { return []listItem{}, nil }
	res, _ := m.Update(liveMsg{live: map[string]bool{"x": true}, unknown: []string{"x"}})
	m = res.(model)
	res, _ = m.Update(refreshMsg{items: []listItem{}, early: true})
	m = res.(model)
	m.lastKick = time.Time{} // even past the 15s limit
	res, _ = m.Update(liveMsg{live: map[string]bool{"x": true}, unknown: []string{"x"}})
	if m = res.(model); !m.refreshStarted.IsZero() {
		t.Error("a session an early scan already looked for must not trigger scans forever")
	}
}

func TestReopenAfterRestart(t *testing.T) {
	sessDir := t.TempDir()
	old, oldBoot, oldPath := sessions.GetSessionsDir, sessions.BootID, sessions.OpenRecordPath
	sessions.GetSessionsDir = func() string { return sessDir }
	sessions.OpenRecordPath = filepath.Join(t.TempDir(), "open-sessions.json")
	boot := "boot-1"
	sessions.BootID = func() string { return boot }
	t.Cleanup(func() {
		sessions.GetSessionsDir, sessions.BootID, sessions.OpenRecordPath, sessions.LastOpenRecord = old, oldBoot, oldPath, ""
	})

	sessions.RecordOpen(map[string]bool{"a": true, "b": true})
	if msg := reopenCheckCmd().(reopenMsg); len(msg.ids) != 0 {
		t.Fatalf("same boot offered %v", msg.ids)
	}
	boot = "boot-2"
	msg := reopenCheckCmd().(reopenMsg)
	if !slices.Equal(msg.ids, []string{"a", "b"}) {
		t.Fatalf("after a restart offered %v, want [a b]", msg.ids)
	}

	// Only sessions still in the list are offered; esc dismisses.
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "a", Cwd: "/x/proj"}}), "", nil)
	res, _ := m.Update(msg)
	m = res.(model)
	if len(m.reopen) != 1 || !m.reopenChecked || !strings.Contains(strip2(m.reopenPopup()), "Reopen the session open before the restart?") {
		t.Fatalf("reopen = %v, popup %q", m.reopen, strip2(m.reopenPopup()))
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m = res.(model); len(m.reopen) != 0 {
		t.Fatal("esc didn't dismiss")
	}
	// Enter starts reopening (and closes the prompt).
	res, _ = m.Update(msg)
	res, cmd := res.(model).Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); cmd == nil || len(m.reopen) != 0 || !m.resuming {
		t.Fatal("enter didn't start reopening")
	}
}

func TestHostedSessionIsNotSwitchable(t *testing.T) {
	conv := transcript.Conversation{SessionID: "t3", Title: "from t3", Cwd: "/p", LastTimestamp: "2026-09-29T11:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "hi"}}}
	m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
	m.width, m.height = 140, 30
	m.live = map[string]bool{"t3": true}
	m.livePIDs = map[string]int{"t3": 99999}
	m.hosts = map[string]string{"t3": "Zed"} // an app ccs has no link for
	m.chatStatus = map[string]sessions.SessionStat{"t3": {Status: "idle"}}
	v := strip2(m.View())
	if !strings.Contains(v, "○✻ from t3") || !strings.Contains(v, "in Zed ·") {
		t.Errorf("a session in an app gets a hollow mark and says where it runs:\n%s", v)
	}
	if cmd := m.resumeCmd(conv, false); cmd != nil || !strings.Contains(m.errorMsg, "In Zed: open it there") {
		t.Errorf("enter doesn't try to switch to or resume it: %q", m.errorMsg)
	}
	if m.noteMsg != m.errorMsg {
		t.Error("saying where it runs is a note, not a red error")
	}
	if got := sessions.AppName("T3 Code (Beta)"); got != "T3 Code" {
		t.Errorf("the edition is dropped: %q", got)
	}
	if m.resuming {
		t.Error("nothing is opening")
	}
	if cmd := m.focusCmd(conv); cmd != nil {
		t.Error("focus doesn't try either")
	}
}

func TestT3SessionOpensInT3(t *testing.T) {
	defer func(l func(int) (string, error), o func(string) error) { sessions.T3Link, sessions.OpenApp = l, o }(sessions.T3Link, sessions.OpenApp)
	var asked int
	var opened string
	sessions.T3Link = func(pid int) (string, error) { asked = pid; return "t3code://threads/env/thread", nil }
	sessions.OpenApp = func(link string) error { opened = link; return nil }
	conv := transcript.Conversation{SessionID: "t3", Title: "from t3", Cwd: "/p", LastTimestamp: "2026-09-29T11:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "hi"}}}
	m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
	m.width, m.height = 140, 30
	m.live = map[string]bool{"t3": true}
	m.hosts = map[string]string{"t3": "T3 Code"}
	m.chatStatus = map[string]sessions.SessionStat{"t3": {Status: "idle"}}
	if v := strip2(m.View()); !strings.Contains(v, "  ✻ from t3") || strings.Contains(v, "○") || !strings.Contains(v, "in T3 Code ·") {
		t.Errorf("one ccs can open gets no hollow mark (its section says idle) and says where it runs:\n%s", v)
	}
	dir := t.TempDir()
	old := sessions.GetSessionsDir
	sessions.GetSessionsDir = func() string { return dir }
	defer func() { sessions.GetSessionsDir = old }()
	oldPS := proc.ProcessStartTimes
	defer func() { proc.ProcessStartTimes = oldPS }()
	proc.ProcessStartTimes = func(map[string]int) map[int]time.Time {
		return map[int]time.Time{os.Getpid(): time.Date(2026, 9, 23, 16, 52, 33, 0, time.UTC)}
	}
	body := fmt.Sprintf(`{"pid":%d,"sessionId":"t3","procStart":"Wed Sep 23 16:52:33 2026"}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "t3.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, open := range []func() tea.Cmd{func() tea.Cmd { return m.resumeCmd(conv, false) }, func() tea.Cmd { return m.focusCmd(conv) }} {
		asked, opened = 0, ""
		cmd := open()
		if cmd == nil {
			t.Fatal("enter opens it in T3 Code")
		}
		res, _ := m.Update(cmd())
		if asked != os.Getpid() || opened != "t3code://threads/env/thread" || res.(model).errorMsg != "" {
			t.Errorf("its thread's link, for its claude: pid %d, %q, %q", asked, opened, res.(model).errorMsg)
		}
	}
	sessions.T3Link = func(int) (string, error) { return "", errors.New("no thread") }
	res, _ := m.Update(m.resumeCmd(conv, false)())
	if m = res.(model); !strings.Contains(m.errorMsg, "In T3 Code: open it there") || m.noteMsg != m.errorMsg {
		t.Errorf("without a link, says to open it there: %q", m.errorMsg)
	}
	if !sessions.T3ID("mcp:launch:7ea8-1f") || sessions.T3ID("a/b") || sessions.T3ID("a b") || sessions.T3ID("") {
		t.Error("thread ids are letters, digits, : and - only")
	}
}

func TestBackgroundSessionsOpenWithAttach(t *testing.T) {
	log := fakeTerminal(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "claude.log")
	os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	conv := transcript.Conversation{SessionID: "bg1", Title: "night job", Cwd: "/p", LastTimestamp: "2026-09-29T11:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "go"}}}
	m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
	m.width, m.height = 140, 30
	m.live = map[string]bool{"bg1": true}
	m.livePIDs = map[string]int{"bg1": 99999}
	m.hosts = map[string]string{"bg1": "claude"} // no terminal, like any background session
	m.bg = map[string]sessions.BgSession{"bg1": {ID: "k3x9", State: "working"}}
	m.chatStatus = map[string]sessions.SessionStat{"bg1": {Status: "busy"}}
	v := strip2(m.View())
	if !strings.Contains(v, "  ✻ night job") || strings.Contains(v, "○") || !strings.Contains(v, "bg working") {
		t.Errorf("a background session can be opened, so it's never hollow, and says it's in the background:\n%s", v)
	}
	cmd := m.resumeCmd(conv, false)
	if cmd == nil {
		t.Fatalf("enter opens it: %q", m.errorMsg)
	}
	if msg := cmd().(attachDoneMsg); !msg.opened || msg.err != nil {
		t.Errorf("opened in a new window: %+v", msg)
	}
	got, _ := os.ReadFile(log)
	if !strings.Contains(string(got), "new-window|-d|-c|/p|"+filepath.Join(dir, "claude")+"|attach|k3x9|") {
		t.Errorf("claude attach <short id> in a new tmux window, got %s", got)
	}

	m.resuming = false
	m, _ = chord(m, "k")
	if !strings.Contains(m.View(), "Stop background session \"night job\"?") {
		t.Fatalf("^X K asks first:\n%s", m.View())
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("y stops it")
	}
	res, _ := m.Update(cmd())
	m = res.(model)
	if got, _ := os.ReadFile(calls); strings.TrimSpace(string(got)) != "stop k3x9" || !strings.Contains(m.errorMsg, "Stopped night job") {
		t.Errorf("claude stop <id>: %q, %q", got, m.errorMsg)
	}
	m.bg = nil
	m, cmd = chord(m, "k")
	if cmd != nil || !strings.Contains(m.errorMsg, "isn't") {
		t.Error("^X K does nothing to a session that isn't in the background")
	}
}

// A live session listed from a transcript it no longer writes is noticed,
// at most once per movedCheckEvery.
func TestMovedTranscript(t *testing.T) {
	root := t.TempDir()
	old := transcript.GetProjectsDir
	transcript.GetProjectsDir = func() string { return root }
	t.Cleanup(func() { transcript.GetProjectsDir = old })
	id := "moved-session"
	oldPath, newPath := filepath.Join(root, "-old", id+".jsonl"), filepath.Join(root, "-new", id+".jsonl")
	for i, p := range []string{oldPath, newPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mod := time.Now().Add(time.Duration(i-1) * time.Hour)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if !movedTranscript(id, oldPath, now) {
		t.Error("listed from the old copy: want moved")
	}
	if movedTranscript(id, oldPath, now.Add(time.Second)) {
		t.Error("checked again within movedCheckEvery")
	}
	if movedTranscript(id, newPath, now.Add(movedCheckEvery)) {
		t.Error("listed from the newest copy: want not moved")
	}
}

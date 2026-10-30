package main

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
)

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`a b`); got != `'a b'` {
		t.Errorf("got %s", got)
	}
	if got := shellQuote(`it's`); got != `'it'\''s'` {
		t.Errorf("got %s", got)
	}
}

func TestResumeInITermTabSkipsOutsideITerm(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	if handled, _ := resumeInITermTab("/tmp", []string{"claude"}); handled {
		t.Error("should not open a tab outside iTerm")
	}
}

func TestResumeInTmuxWindowSkipsOutsideTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	if handled, _ := resumeInTmuxWindow("/tmp", []string{"claude"}); handled {
		t.Error("should not open a window outside tmux")
	}
}

func TestReadLiveSessions(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("self.json", fmt.Sprintf(`{"pid":%d,"sessionId":"alive"}`, os.Getpid()))
	write("dead.json", `{"pid":999999999,"sessionId":"dead"}`)
	write("junk.json", `not json`)

	live := readLiveSessions()
	if !live["alive"] || live["dead"] || len(live) != 1 {
		t.Errorf("want only 'alive', got %v", live)
	}
}

func TestTmuxPaneForTTY(t *testing.T) {
	panes := "/dev/ttys001 main:0.0\n/dev/ttys007 work:2.1\n"
	if got := tmuxPaneForTTY(panes, "/dev/ttys007"); got != "work:2.1" {
		t.Errorf("got %q", got)
	}
	if got := tmuxPaneForTTY(panes, "/dev/ttys999"); got != "" {
		t.Errorf("unknown tty should give no target, got %q", got)
	}
}

func TestEnterOnLiveSessionOpensItsTab(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"s"}`, os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	t.Setenv("TERM_PROGRAM", "") // no terminal to focus in the test

	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.live = map[string]bool{"s": true}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
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
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", flags)
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

func TestSameStart(t *testing.T) {
	actual := time.Date(2026, 9, 23, 16, 52, 33, 0, time.UTC).In(time.FixedZone("NPT", 5*3600+45*60))
	if match, checked := sameStart("Wed Sep 23 16:52:33 2026", actual); !match || !checked {
		t.Error("same instant in different zones should match")
	}
	if match, _ := sameStart("Wed Sep 23 10:00:00 2026", actual); match {
		t.Error("a different start time means a recycled pid")
	}
	for _, c := range [][2]any{{"", actual}, {"Wed Sep 23 16:52:33 2026", time.Time{}}} {
		if match, checked := sameStart(c[0].(string), c[1].(time.Time)); !match || checked {
			t.Error("unknown start on either side must not hide a live session, and isn't a check")
		}
	}
}

func TestLiveSessionsDropsRecycledPid(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()
	oldPS := processStartTimes
	defer func() { processStartTimes = oldPS }()
	processStartTimes = func(map[string]int) map[int]time.Time {
		return map[int]time.Time{os.Getpid(): time.Date(2026, 9, 23, 16, 52, 33, 0, time.UTC)}
	}
	write := func(name, id, start string) {
		body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"procStart":%q}`, os.Getpid(), id, start)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", "current", "Wed Sep 23 16:52:33 2026")
	write("b.json", "stale", "Mon Sep 21 09:00:00 2026") // same pid, earlier process
	live := readLiveSessions()
	if !live["current"] || live["stale"] {
		t.Errorf("recycled pid should not count as live: %v", live)
	}
}

func TestLiveMsgUpdatesAndResortsKeepingCursor(t *testing.T) {
	mk := func(id, ts string) Conversation {
		return Conversation{SessionID: id, LastTimestamp: ts, Messages: []Message{{Role: "user", Text: id}}}
	}
	m := initialModel(buildItems([]Conversation{mk("a", "2026-09-25T10:00:00Z"), mk("b", "2026-09-25T09:00:00Z")}), "", nil)
	m.cursor = 1 // on b
	grown := mk("b", "2026-09-25T11:00:00Z")
	grown.Messages = append(grown.Messages, Message{Role: "assistant", Text: "new"})
	res, cmd := m.Update(liveMsg{live: map[string]bool{"b": true}, updated: []Conversation{grown}})
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
	res, _ = m.Update(liveMsg{live: map[string]bool{}, updated: []Conversation{mk("a", "2026-09-25T12:00:00Z")}})
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
	res, _ = m.Update(refreshTickMsg{})
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
	scanned := Conversation{SessionID: "s", Size: 300, LastTimestamp: "2026-09-25T11:00:00Z", Messages: []Message{{Role: "user", Text: "new"}}, readAt: now}
	liveOld := scanned
	liveOld.Size, liveOld.readAt = 200, now.Add(-time.Second)
	m := initialModel(buildItems([]Conversation{scanned}), "", nil)
	m.applyLive([]Conversation{liveOld})
	if m.items[0].conv.Size != 300 {
		t.Error("a live read older than the list's copy must be ignored")
	}
}

func TestTmuxCwdHashIsEscaped(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "tmux")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TMUX", "fake")
	if handled, err := resumeInTmuxWindow("/tmp/#(id)", []string{"claude"}); !handled || err != nil {
		t.Fatal(handled, err)
	}
	got, _ := os.ReadFile(log)
	if !strings.Contains(string(got), "/tmp/##(id)") {
		t.Errorf("tmux -c must get '#' escaped, got args:\n%s", got)
	}
}

func TestDoubleEnterOpensOnce(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	res, first := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	_, second := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if first == nil || second != nil {
		t.Error("a second Enter while the first is opening must be ignored")
	}
}

func TestTabResultOnlyTimeoutMeansMaybeOpened(t *testing.T) {
	if h, err := tabResult(nil, nil); !h || err != nil {
		t.Error("success is handled")
	}
	if h, err := tabResult(nil, fmt.Errorf("osascript: %w", errTimedOut)); !h || err == nil {
		t.Error("a timeout is handled with an error: it may have opened")
	}
	if h, _ := tabResult(nil, fmt.Errorf("exit status 1")); h {
		t.Error("a plain failure must fall back to resuming in place")
	}
	if _, err := runBounded(50*time.Millisecond, nil, "sleep", "5"); !errors.Is(err, errTimedOut) {
		t.Errorf("runBounded should report its deadline, got %v", err)
	}
}

func TestShellSafe(t *testing.T) {
	for s, want := range map[string]bool{"/Users/me/proj": true, "/tmp/it's": true, `/r/a\`: false, "/tmp/a\x01b": false, "/tmp/\xff": false} {
		if shellSafe(s) != want {
			t.Errorf("shellSafe(%q) != %v", s, want)
		}
	}
}

func TestSecondEnterAfterTabOpenDoesNotLaunchAgain(t *testing.T) {
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	res, _ := m.Update(resumeDoneMsg{conv: Conversation{SessionID: "s"}, opened: true})
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

func TestStaleSessionFileWithOtherPidDoesNotHideLiveOne(t *testing.T) {
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	defer func() { getSessionsDir = old }()
	oldPS := processStartTimes
	defer func() { processStartTimes = oldPS }()
	me := os.Getpid()
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	processStartTimes = func(map[string]int) map[int]time.Time {
		return map[int]time.Time{me: start, 1: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	write := func(name string, pid int, when string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf(`{"pid":%d,"sessionId":"S","procStart":%q}`, pid, when)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("0-live.json", me, "Fri Sep 25 10:00:00 2026")
	write("9-stale.json", 1, "Mon Sep 21 09:00:00 2026") // crashed claude's leftover; pid 1 recycled
	if pid := liveSessionPIDs()["S"]; pid != me {
		t.Errorf("live session hidden by a stale file: got pid %d", pid)
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
	sessions := t.TempDir()
	old, oldBoot, oldPath := getSessionsDir, bootID, openRecordPath
	getSessionsDir = func() string { return sessions }
	openRecordPath = filepath.Join(t.TempDir(), "open-sessions.json")
	boot := "boot-1"
	bootID = func() string { return boot }
	t.Cleanup(func() { getSessionsDir, bootID, openRecordPath, lastOpenRecord = old, oldBoot, oldPath, "" })

	recordOpen(map[string]bool{"a": true, "b": true})
	if msg := reopenCheckCmd().(reopenMsg); len(msg.ids) != 0 {
		t.Fatalf("same boot offered %v", msg.ids)
	}
	boot = "boot-2"
	msg := reopenCheckCmd().(reopenMsg)
	if !slices.Equal(msg.ids, []string{"a", "b"}) {
		t.Fatalf("after a restart offered %v, want [a b]", msg.ids)
	}

	// Only sessions still in the list are offered; esc dismisses.
	m := initialModel(buildItems([]Conversation{{SessionID: "a", Cwd: "/x/proj"}}), "", nil)
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

func TestInITermThroughTmux(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "tmux") // tmux overrides it inside its panes
	if inITerm() {
		t.Error("tmux alone isn't iTerm")
	}
	t.Setenv("LC_TERMINAL", "iTerm2") // what iTerm sets, and tmux passes through
	if !inITerm() {
		t.Error("tmux inside iTerm should still find iTerm tabs")
	}
}

func TestHostedSessionIsNotSwitchable(t *testing.T) {
	conv := Conversation{SessionID: "t3", Title: "from t3", Cwd: "/p", LastTimestamp: "2026-09-29T11:00:00Z", Messages: []Message{{Role: "user", Text: "hi"}}}
	m := initialModel(buildItems([]Conversation{conv}), "", nil)
	m.width, m.height = 140, 30
	m.live = map[string]bool{"t3": true}
	m.livePIDs = map[string]int{"t3": 99999}
	m.hosts = map[string]string{"t3": "T3 Code (Beta)"}
	m.chatStatus = map[string]sessionStat{"t3": {status: "idle"}}
	v := strip2(m.View())
	if !strings.Contains(v, "○ from t3") || !strings.Contains(v, "in T3 Code (Beta)") {
		t.Errorf("a session in an app gets a hollow mark and says where it runs:\n%s", v)
	}
	if cmd := m.resumeCmd(conv, false); cmd != nil || !strings.Contains(m.errorMsg, "Runs in T3 Code (Beta)") {
		t.Errorf("enter doesn't try to switch to or resume it: %q", m.errorMsg)
	}
	if m.resuming {
		t.Error("nothing is opening")
	}
	if cmd := m.focusCmd(conv); cmd != nil {
		t.Error("focus doesn't try either")
	}
}

func TestFindHostOfThisProcess(t *testing.T) {
	// The test binary has a terminal locally and none in CI: either way an
	// answer, never a hang or a panic.
	_ = findHost(os.Getpid())
	if h := findHost(-1); h != "" {
		t.Errorf("no such process is treated as a terminal: %q", h)
	}
}

func TestBackgroundSessionsOpenWithAttach(t *testing.T) {
	log := fakeTerminal(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "claude.log")
	os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	conv := Conversation{SessionID: "bg1", Title: "night job", Cwd: "/p", LastTimestamp: "2026-09-29T11:00:00Z", Messages: []Message{{Role: "user", Text: "go"}}}
	m := initialModel(buildItems([]Conversation{conv}), "", nil)
	m.width, m.height = 140, 30
	m.live = map[string]bool{"bg1": true}
	m.livePIDs = map[string]int{"bg1": 99999}
	m.hosts = map[string]string{"bg1": "claude"} // no terminal, like any background session
	m.bg = map[string]bgSession{"bg1": {ID: "k3x9", State: "working"}}
	m.chatStatus = map[string]sessionStat{"bg1": {status: "busy"}}
	v := strip2(m.View())
	if !strings.Contains(v, "● night job") || !strings.Contains(v, "bg working") {
		t.Errorf("a background session can be opened, so it keeps ●, and says it's in the background:\n%s", v)
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
	if !strings.Contains(m.errorMsg, "again to stop night job") {
		t.Fatalf("^X K asks for a second press: %q", m.errorMsg)
	}
	m, cmd = chord(m, "k")
	if cmd == nil {
		t.Fatal("the second ^X K stops it")
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

func TestListAgentsKeepsBackgroundOnes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "claude"), []byte(`#!/bin/sh
echo '[{"kind":"interactive","sessionId":"i1","pid":1},{"kind":"background","id":"k3x9","state":"blocked","sessionId":"b1"},{"kind":"background","id":"zz","state":"done"}]'
`), 0o755)
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	defer func(f func() (map[string]bgSession, error)) { listAgents = f }(listAgents)
	listAgents = realListAgents
	got, err := listAgents()
	if err != nil || len(got) != 1 || got["b1"] != (bgSession{"k3x9", "blocked"}) {
		t.Errorf("only background sessions with a session id: %v %v", got, err)
	}
}

func TestBgCommandIsASpawn(t *testing.T) {
	if !isSpawn(`claude --bg "fix the flaky test"`) || !isSpawn("claude --background 'x'") {
		t.Error("claude --bg starts a session, so it nests under its parent")
	}
}

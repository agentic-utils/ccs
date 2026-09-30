package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestEnterOnLiveSessionFocusesInsteadOfResuming(t *testing.T) {
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
	if !sameStart("Wed Sep 23 16:52:33 2026", actual) {
		t.Error("same instant in different zones should match")
	}
	if sameStart("Wed Sep 23 10:00:00 2026", actual) {
		t.Error("a different start time means a recycled pid")
	}
	if !sameStart("", actual) || !sameStart("Wed Sep 23 16:52:33 2026", time.Time{}) {
		t.Error("unknown start on either side must not hide a live session")
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

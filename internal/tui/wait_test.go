package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// waitModel adds two live sessions to chatModel's (idle, and live, which
// waits in the tests that need it): menu, which may wait too, and asks, whose
// last message asks something.
func waitModel(t *testing.T) model {
	t.Helper()
	m := chatModel(t)
	var convs []transcript.Conversation
	for _, item := range m.items {
		convs = append(convs, item.conv)
	}
	m.items = buildItems(append(convs,
		transcript.Conversation{SessionID: "menu", Title: "Menu one", LastTimestamp: "2026-09-29T09:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "go"}}},
		transcript.Conversation{SessionID: "asks", Title: "Asking one", LastTimestamp: "2026-09-29T08:00:00Z", Messages: []transcript.Message{{Role: "assistant", Text: "Which fruit do you prefer? "}}},
	))
	m.updateFilter()
	m.live = map[string]bool{"live": true, "menu": true, "asks": true}
	return m
}

func waiting(since time.Time, why string) sessions.SessionStat {
	return sessions.SessionStat{Status: "waiting", WaitingFor: why, Since: since}
}

func typeIn(m model, s string) model {
	for _, r := range s {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// selectLive puts the list's selection on the live session, so its message box shows.
func selectLive(m model) model {
	for i, item := range m.filtered {
		if item.conv.SessionID == "live" {
			m.cursor = i
		}
	}
	return m
}

// topRow is the screen's first row: the title.
func topRow(m model) string { return strings.Split(strip2(m.View()), "\n")[0] }

// sessionFileFor writes this process's session file.
func sessionFileFor(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(sessions.GetSessionsDir(), fmt.Sprintf("%d.json", os.Getpid()))
	os.WriteFile(path, []byte(body), 0o600)
	return path
}

// fakeStart makes ps report a fixed start time for this process; move shifts
// it, as if the pid now belonged to another process.
func fakeStart(t *testing.T) (move func(time.Duration)) {
	me, start := os.Getpid(), time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	old := proc.ProcessStartTimes
	t.Cleanup(func() { proc.ProcessStartTimes = old })
	proc.ProcessStartTimes = func(map[string]int) map[int]time.Time { return map[int]time.Time{me: start} }
	return func(d time.Duration) { start = start.Add(d) }
}

func liveTickOnce(m model) model {
	res, _ := m.Update(m.liveCmd(false)())
	return res.(model)
}

func TestWaitKeepsASessionOnlyWhileItsProcessRuns(t *testing.T) {
	good := fmt.Sprintf(`{"pid":%d,"sessionId":"live","status":"waiting","statusUpdatedAt":1790000000000,"procStart":"Fri Sep 25 10:00:00 2026"}`, os.Getpid())
	for name, gap := range map[string]func(path string){
		"unreadable": func(path string) { os.WriteFile(path, []byte(`{"sessionId":`), 0o600) }, // caught mid-write
		"missing":    func(path string) { os.Remove(path) },
	} {
		m := waitModel(t)
		fakeStart(t)
		path := sessionFileFor(t, good)
		m = liveTickOnce(m)
		gap(path)
		if m = liveTickOnce(m); !m.live["live"] {
			t.Errorf("%s: a session without its file for a tick is still live while its process runs", name)
		}
		os.WriteFile(path, []byte(good), 0o600)
		if m = liveTickOnce(m); m.chatStatus["live"].Status != "waiting" {
			t.Errorf("%s: one tick without the file must not lose its wait: %+v", name, m.chatStatus["live"])
		}
	}

	// A pid that now belongs to another process doesn't keep it.
	m := waitModel(t)
	move := fakeStart(t)
	path := sessionFileFor(t, good)
	m = liveTickOnce(m)
	os.WriteFile(path, []byte(`{"sessionId":`), 0o600)
	move(time.Hour)
	if m = liveTickOnce(m); m.live["live"] {
		t.Error("a reused pid must not keep a dead session alive")
	}

	// Its pid's file now names another session (/clear).
	m = waitModel(t)
	fakeStart(t)
	path = sessionFileFor(t, good)
	m = liveTickOnce(m)
	os.WriteFile(path, []byte(strings.Replace(good, `"live"`, `"idle"`, 1)), 0o600)
	if m = liveTickOnce(m); m.live["live"] {
		t.Error("after /clear the old session is gone")
	}

	// Without a known start time it's kept only a few ticks.
	m = waitModel(t)
	path = sessionFileFor(t, fmt.Sprintf(`{"pid":%d,"sessionId":"live","status":"idle"}`, os.Getpid()))
	m = liveTickOnce(m)
	os.WriteFile(path, []byte(`{"sessionId":`), 0o600)
	for i := 0; i <= sessions.KeepTicks; i++ {
		m = liveTickOnce(m)
	}
	if m.live["live"] {
		t.Error("an unverified session without its file is kept only keepTicks ticks")
	}
}

func TestWaitMarkersAndHeaderCount(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "permission prompt"), "asks": {Status: "idle", Since: time.Now()}})
	if head := topRow(m); !strings.Contains(head, "1 waiting") || strings.Contains(head, "^X W") {
		t.Errorf("header counts menus and prompts, not the dim ? guess, with no chord: %q", head)
	}
	for _, id := range []string{"live", "asks"} {
		for _, item := range m.filtered {
			if item.conv.SessionID == id && strings.Contains(strip2(m.formatListItem(item, false)), "●") {
				t.Errorf("%s needs no ●: its section heading says it waits", id)
			}
		}
	}
}

func TestWaitEmptyStatusChangesNothing(t *testing.T) {
	m := waitModel(t)
	stats := map[string]sessions.SessionStat{"live": waiting(time.Now(), "permission prompt")}
	m.applyStats(stats)
	m.applyStats(map[string]sessions.SessionStat{"live": {}}) // file caught without a status
	if m.chatStatus["live"].Status != "waiting" {
		t.Fatal("a status-less read changes nothing")
	}
}

func TestWaitNeverTypesIntoAMenuOrAnUncheckedPid(t *testing.T) {
	chatModel(t)
	pid := os.Getpid()
	sessionFileFor(t, `{"sessionId":"live","status":"waiting","messagingSocketPath":"/tmp/ccs-no-such.sock"}`)
	log := fakeTerminal(t)
	note, err := deliverMessage(pid, "live", "busy", "no, stop", true)
	if got, _ := os.ReadFile(log); err == nil || len(got) != 0 || !strings.Contains(note, "menu") {
		t.Fatalf("typing into a session on a menu could pick an option: note=%q err=%v typed=%q", note, err, got)
	}

	// A pid that couldn't be checked against its file may be a stranger's.
	m := selectLive(waitModel(t))
	sessionFileFor(t, fmt.Sprintf(`{"pid":%d,"sessionId":"live","status":"idle"}`, pid)) // no procStart, no socket
	m = liveTickOnce(m)
	m.chatFocus = true
	m = typeIn(m, "hello")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	res, _ := m.Update(cmd())
	if got, _ := os.ReadFile(log); len(got) != 0 || res.(model).chatInput.Value() != "hello" {
		t.Fatalf("typed into a pid that couldn't be checked: typed=%q box=%q", got, res.(model).chatInput.Value())
	}
}

func TestChatStatusLineFitsItsWidth(t *testing.T) {
	m := waitModel(t)
	m.pending = map[string]string{"live": strings.Repeat("word ", 40)}
	m.sentAt = map[string]time.Time{"live": time.Now().Add(-time.Minute)}
	var conv transcript.Conversation
	for _, it := range m.items {
		if it.conv.SessionID == "live" {
			conv = it.conv
		}
	}
	for _, w := range []int{60, 90} {
		line := strings.Split(m.chatBox(conv, m.chatInput, true, w), "\n")[0]
		if got := ansi.StringWidth(line); got > w {
			t.Errorf("status line is %d wide at width %d: %q", got, w, strip2(line))
		}
	}
}

func TestWaitAskedHeuristic(t *testing.T) {
	for txt, want := range map[string]int{
		"Shall I go ahead (yes?)": waitAsked,
		"Want me to push?**":      waitAsked,
		"Ready? ":                 waitAsked,
		"?":                       waitNone,
		"Done.":                   waitNone,
	} {
		m := waitModel(t)
		m.chatStatus = map[string]sessions.SessionStat{"asks": {Status: "idle"}}
		if got := m.waitSignal(transcript.Conversation{SessionID: "asks", Messages: []transcript.Message{{Role: "assistant", Text: txt}}}); got != want {
			t.Errorf("%q: got %d, want %d", txt, got, want)
		}
	}
}

func TestWaitFastTickFollowsTheSelection(t *testing.T) {
	m := selectLive(waitModel(t))
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "permission prompt")})
	m.chatTickOn = false
	if m.ensureChatTick() == nil {
		t.Error("the fast tick follows the selected live session")
	}
}

func TestWaitForgetsSessionsThatAreGone(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessions.SessionStat{"menu": {Status: "idle", Since: time.Now()}})
	m.live = map[string]bool{"live": true}
	m.applyStats(map[string]sessions.SessionStat{"live": {Status: "idle", Since: time.Now()}})
	if _, ok := m.chatStatus["menu"]; ok {
		t.Error("chatStatus should drop a session that's no longer live")
	}
}

func TestPasteGoesToTheFocusedInput(t *testing.T) {
	m := waitModel(t)
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return !m.live[it.conv.SessionID] }) // can be renamed
	m, _ = chord(m, "e")
	if !m.renaming {
		t.Fatal("ctrl+r should start a rename")
	}
	res, _ := m.Update(textinput.Paste()) // the clipboard's text, or an error without one
	if n := res.(model); n.textInput.Value() != "" || n.textInput.Err != nil {
		t.Errorf("a paste during a rename went to the hidden search: %q %v", n.textInput.Value(), n.textInput.Err)
	}
}

func TestWaitMainBoxTextComesBackWhenReselected(t *testing.T) {
	m := selectLive(waitModel(t))
	sessionFileFor(t, `{"sessionId":"live"}`) // no socket, no terminal: unreachable
	t.Setenv("TMUX", "")
	t.Setenv("TERM_PROGRAM", "")
	m.chatFocus = true
	m = typeIn(m, "my long answer")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return !m.live[it.conv.SessionID] }) // moved on before the send failed
	res, _ := m.Update(cmd())
	m = res.(model)
	if strings.Contains(m.chatInput.Value(), "my long answer") {
		t.Fatalf("another session's box must not get the text: %q", m.chatInput.Value())
	}
	if !strings.Contains(m.sendNote["live"], "message box") {
		t.Errorf("the note should say where the text is: %q", m.sendNote["live"])
	}
	m = selectLive(m)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyUp})
	m = selectLive(m)
	res, _ = m.Update(liveTickMsg{})
	if v := res.(model).chatInput.Value(); v != "my long answer" {
		t.Fatalf("selecting the session again brings its unsent text back: %q", v)
	}
}

func TestWaitDiscardedDraftIsReported(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessions.SessionStat{"live": {Status: "idle", Since: time.Now()}, "menu": waiting(time.Now(), "permission prompt")})
	m.chatDrafts = map[string]string{"live": "half an answer"} // the draft kept with its session
	m.live = map[string]bool{"menu": true}
	m.applyStats(map[string]sessions.SessionStat{"menu": waiting(time.Now(), "permission prompt")})
	if !strings.Contains(m.errorMsg, "discarded") {
		t.Fatalf("a kept draft thrown away when its session exits should be reported: %q", m.errorMsg)
	}
}

func TestWaitHeaderCountKeepsTheTitleRow(t *testing.T) {
	m := waitModel(t)
	m.updating, m.updateTo = true, "v9.9.9"
	m.progress = &updateProgress{step: "downloading", started: time.Now()}
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "permission prompt")})
	if top := topRow(m); !strings.Contains(top, "updating to v9.9.9") || !strings.Contains(top, "· 1 waiting") || strings.Contains(top, "^X W") || !strings.Contains(top, "quit") {
		t.Fatalf("the waiting count sits with the title row's notes and hints: %q", top)
	}
	if strings.Contains(strip2(m.View()), "waiting on you") {
		t.Error("no banner row under the search")
	}
}

func TestWaitingMultiSelectShownInPreviewPane(t *testing.T) {
	m := waitModel(t)
	m.width, m.height = 100, 40
	ask := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"AskUserQuestion","input":{"questions":[{"question":"Which checks?","multiSelect":true,"options":[{"label":"lint"},{"label":"tests"}]}]}}]}}`
	for i := range m.items {
		if m.items[i].conv.SessionID == "live" {
			transcript.ParseLine(&m.items[i].conv, []byte(ask))
		}
	}
	m.updateFilter()
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "input needed")})
	v := strip2(m.View())
	for _, want := range []string{"? Which checks?  (pick any)", "1  lint", "2  tests", "answer it in its tab"} {
		if !strings.Contains(v, want) {
			t.Errorf("bottom pane should show %q:\n%s", want, v)
		}
	}
	if n := len(strings.Split(v, "\n")); n > m.height {
		t.Errorf("view is %d rows on a %d-row screen", n, m.height)
	}
	// Its message box is hidden: a message would wait for the menu anyway.
	if strings.Contains(v, "message ") || strings.Contains(v, "╭") {
		t.Errorf("no message box while it waits:\n%s", v)
	}
	if m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.chatFocus {
		t.Error("enter can't focus a hidden box")
	}
}

func TestLongQuestionWrapsInFull(t *testing.T) {
	long := strings.Repeat("the question goes on ", 12) + "END"
	desc := strings.Repeat("a long description ", 10) + "TAIL"
	var conv transcript.Conversation
	in := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"AskUserQuestion","input":{"questions":[{"question":%q,"options":[{"label":"yes","description":%q}]}]}}]}}`, long, desc)
	if !transcript.ParseLine(&conv, []byte(in)) {
		t.Fatal("not parsed")
	}
	lines := askLines(conv, 60)
	text := strip2(strings.Join(lines, "\n"))
	for _, want := range []string{"END", "TAIL", "1  yes"} {
		if !strings.Contains(text, want) {
			t.Errorf("the whole question should show, missing %q:\n%s", want, text)
		}
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 60 {
			t.Errorf("line %d wide: %q", w, l)
		}
	}
	if strings.Contains(text, "…") {
		t.Errorf("nothing should be cut:\n%s", text)
	}
}

func TestQuestionMarkColours(t *testing.T) {
	conv := transcript.Conversation{SessionID: "s", Messages: []transcript.Message{{Role: "assistant", Text: "Want me to add that?"}}}
	m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
	m.live = map[string]bool{"s": true}
	m.chatStatus = map[string]sessions.SessionStat{"s": {Status: "idle"}}
	if c := m.titleColour(listItem{conv: conv}); c != askedColour {
		t.Errorf("idle with a question in its last message: a light magenta title, got %q", c)
	}
	m.chatStatus["s"] = sessions.SessionStat{Status: "waiting"}
	if c := m.titleColour(listItem{conv: conv}); c != waitColour {
		t.Errorf("a menu or prompt waiting: a magenta title, got %q", c)
	}
	m.hosts = map[string]string{"s": "Zed"}
	if _, marks := m.listMarks(listItem{conv: conv}); !strings.Contains(marks, "\033["+waitColour+"m○") {
		t.Errorf("one in an app keeps its hollow ○ while waiting: %q", marks)
	}
}

func TestAutoFocus(t *testing.T) {
	idle := time.Duration(0)
	var switched []int
	defer func(u func() (time.Duration, bool), f func(int) bool) { userIdle, autoFocusTab = u, f }(userIdle, autoFocusTab)
	userIdle = func() (time.Duration, bool) { return idle, true }
	autoFocusTab = func(pid int) bool { switched = append(switched, pid); return true }
	os.Remove(focusRecordPath)
	os.Remove(state.ActivityLogPath)

	now := time.Now()
	start := func() model {
		m := initialModel(nil, "", nil)
		if handled, ok := loadFocusRecord(); ok { // as main does
			m.focusHandled, m.focusNoted = handled, map[string]bool{}
		}
		m.live = map[string]bool{"a": true, "b": true, "c": true, "d": true}
		m.livePIDs = map[string]int{"a": 1, "b": 2, "c": 3, "d": 4}
		m.autoFocus = true
		return m
	}
	m := start()
	tick := func(stats map[string]sessions.SessionStat) {
		t.Helper()
		m.applyStats(stats)
		if cmd := m.autoFocusCmd(); cmd != nil {
			m.applyAutoFocus(cmd().(autoFocusMsg))
		}
	}
	wait := func(at time.Time) sessions.SessionStat { return sessions.SessionStat{Status: "waiting", Since: at} }
	busy := sessions.SessionStat{Status: "busy"}

	idle = 5 * time.Second
	tick(map[string]sessions.SessionStat{"a": wait(now), "b": busy, "c": busy})
	if len(switched) != 1 || switched[0] != 1 {
		t.Fatalf("a question already waiting at startup is switched to: %v", switched)
	}
	idle = 0
	tick(map[string]sessions.SessionStat{"a": busy, "b": wait(now.Add(time.Second)), "c": busy})
	if len(switched) != 1 {
		t.Fatal("while the user is typing, it waits rather than switch under them")
	}
	idle = 5 * time.Second
	tick(map[string]sessions.SessionStat{"a": busy, "b": wait(now.Add(time.Second)), "c": busy})
	if len(switched) != 2 || switched[1] != 2 {
		t.Fatalf("once the user is still, it switches to b: %v", switched)
	}
	tick(map[string]sessions.SessionStat{"a": busy, "b": wait(now.Add(time.Second)), "c": wait(now.Add(2 * time.Second))})
	if len(switched) != 2 {
		t.Fatalf("not again until b's question is answered: %v", switched)
	}
	tick(map[string]sessions.SessionStat{"a": busy, "b": busy, "c": wait(now.Add(2 * time.Second))})
	if len(switched) != 3 || switched[2] != 3 {
		t.Fatalf("b answered, so on to c: %v", switched)
	}
	tick(map[string]sessions.SessionStat{"a": busy, "b": busy, "c": wait(now.Add(2 * time.Second))})
	if len(switched) != 3 {
		t.Fatalf("once per wait: %v", switched)
	}

	// A restart (say an update) remembers c was dealt with, but d's question,
	// which came up meanwhile, is still news.
	m = start()
	tick(map[string]sessions.SessionStat{"a": busy, "b": busy, "c": wait(now.Add(2 * time.Second)), "d": wait(now.Add(3 * time.Second))})
	if len(switched) != 4 || switched[3] != 4 {
		t.Fatalf("after a restart: d, not c again: %v", switched)
	}

	// Each step is in the activity log, once.
	data, _ := os.ReadFile(state.ActivityLogPath)
	log := string(data)
	for _, want := range []string{"switched to a", "b is waiting", "you're typing (last key 0.0s ago); switching to b", "switched to b", "c is waiting too; holding on b", "b answered, free to switch again", "switched to c", "switched to d"} {
		if strings.Count(log, want) != 1 {
			t.Errorf("the activity log should say %q once:\n%s", want, log)
		}
	}
}

func TestAutoFocusSkipsHostedSessions(t *testing.T) {
	var switched []int
	defer func(u func() (time.Duration, bool), f func(int) bool) { userIdle, autoFocusTab = u, f }(userIdle, autoFocusTab)
	userIdle = func() (time.Duration, bool) { return time.Minute, true }
	autoFocusTab = func(pid int) bool { switched = append(switched, pid); return true }
	os.Remove(focusRecordPath)
	m := initialModel(nil, "", nil)
	m.live = map[string]bool{"t3": true, "term": true}
	m.livePIDs = map[string]int{"t3": 1, "term": 2}
	m.hosts = map[string]string{"t3": "T3 Code (Beta)"}
	m.autoFocus = true
	now := time.Now()
	m.applyStats(map[string]sessions.SessionStat{"t3": {Status: "waiting", Since: now}})
	if cmd := m.autoFocusCmd(); cmd != nil {
		t.Fatal("a session in an app has no tab to switch to")
	}
	m.applyStats(map[string]sessions.SessionStat{"t3": {Status: "waiting", Since: now}, "term": {Status: "waiting", Since: now}})
	if cmd := m.autoFocusCmd(); cmd != nil {
		m.applyAutoFocus(cmd().(autoFocusMsg))
	}
	if len(switched) != 1 || switched[0] != 2 {
		t.Errorf("the one in a terminal is still switched to: %v", switched)
	}
}

// ^T A while off opens the target picker; Esc leaves it off, a digit or
// Enter turns it on with that target, saved; ^T A while on turns it off at
// once, and the picker opens on the saved target next time.
func TestAutoFocusPicker(t *testing.T) {
	os.Remove(state.SettingsPath)
	t.Cleanup(func() { os.Remove(state.SettingsPath) })
	m := initialModel(nil, "", nil)
	m.width, m.height = 120, 40
	chord := func() {
		t.Helper()
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlT})
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	}
	chord()
	if m.autoFocus || !m.autoFocusOpen {
		t.Fatalf("^T A while off asks where: on=%v picker=%v", m.autoFocus, m.autoFocusOpen)
	}
	if v := strip2(m.View()); !strings.Contains(v, "1 ccs") || !strings.Contains(v, "2 tab") || !strings.Contains(v, "3 both") {
		t.Errorf("the picker lists ccs, tab and both: %q", v)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.autoFocus || m.autoFocusOpen {
		t.Fatal("Esc leaves it off")
	}
	chord()
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if !m.autoFocus || m.autoFocusOpen || m.focusTarget() != focusTab || !m.focusTabs() || m.focusInCCS() {
		t.Fatalf("2 turns it on for tabs: on=%v target=%q", m.autoFocus, m.autoFocusTarget)
	}
	if s := loadSettings(); !s.AutoFocus || s.AutoFocusTarget != focusTab {
		t.Errorf("saved: %+v", s)
	}
	chord()
	if m.autoFocus || m.autoFocusOpen {
		t.Fatal("^T A while on turns it off, no picker")
	}
	if s := loadSettings(); s.AutoFocus || s.AutoFocusTarget != focusTab {
		t.Errorf("off is saved, the target kept: %+v", s)
	}
	chord()
	if m.autoFocusCursor != 1 {
		t.Errorf("the picker opens on the saved target: cursor %d", m.autoFocusCursor)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.autoFocus || m.focusTarget() != focusBoth || !m.focusTabs() || !m.focusInCCS() {
		t.Errorf("↓ Enter picks both: target %q", m.autoFocusTarget)
	}
	m.autoFocusTarget = "" // saved before the choice existed: both, as auto-focus did then
	if m.focusTarget() != focusBoth {
		t.Errorf("no saved target is both: %q", m.focusTarget())
	}
}

// Tabs switch only for a target of tab or both; with tab, a prompt ccs
// holds switches its tab too (its menu is there), since ccs selects nothing.
func TestAutoFocusTarget(t *testing.T) {
	var switched []int
	defer func(u func() (time.Duration, bool), f func(int) bool) { userIdle, autoFocusTab = u, f }(userIdle, autoFocusTab)
	userIdle = func() (time.Duration, bool) { return time.Minute, true }
	autoFocusTab = func(pid int) bool { switched = append(switched, pid); return true }
	now := time.Now()
	for _, c := range []struct {
		target string
		on     bool
		want   int
	}{{focusCCS, true, 0}, {focusTab, true, 1}, {focusBoth, true, 0}, {focusTab, false, 0}} {
		os.Remove(focusRecordPath)
		switched = nil
		m := initialModel(nil, "", nil)
		m.live, m.livePIDs = map[string]bool{"h": true}, map[string]int{"h": 1}
		m.autoFocus, m.autoFocusTarget = c.on, c.target
		m.heldPrompt = map[string]mcp.HeldPrompt{"h": {ID: "p", Session: "h", Tool: "Bash", At: now}}
		m.applyStats(map[string]sessions.SessionStat{"h": {Status: "waiting", Since: now}})
		if cmd := m.autoFocusCmd(); cmd != nil {
			m.applyAutoFocus(cmd().(autoFocusMsg))
		}
		if len(switched) != c.want {
			t.Errorf("held, target %s on=%v: switched %v, want %d", c.target, c.on, switched, c.want)
		}
		m.heldPrompt = nil
		m.focusHandled, m.focusHeld = map[string]time.Time{}, ""
		switched = nil
		if cmd := m.autoFocusCmd(); cmd != nil {
			m.applyAutoFocus(cmd().(autoFocusMsg))
		}
		if want := map[bool]int{true: 1, false: 0}[c.on && c.target != focusCCS]; len(switched) != want {
			t.Errorf("waiting in its tab, target %s on=%v: switched %v, want %d", c.target, c.on, switched, want)
		}
	}
	os.Remove(focusRecordPath)
}

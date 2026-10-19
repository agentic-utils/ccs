package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// waitModel adds two live sessions to chatModel's (idle, and live, which
// waits in the tests that need it): menu, which may wait too, and asks, whose
// last message asks something.
func waitModel(t *testing.T) model {
	t.Helper()
	m := chatModel(t)
	var convs []Conversation
	for _, item := range m.items {
		convs = append(convs, item.conv)
	}
	m.items = buildItems(append(convs,
		Conversation{SessionID: "menu", Title: "Menu one", LastTimestamp: "2026-09-29T09:00:00Z", Messages: []Message{{Role: "user", Text: "go"}}},
		Conversation{SessionID: "asks", Title: "Asking one", LastTimestamp: "2026-09-29T08:00:00Z", Messages: []Message{{Role: "assistant", Text: "Which fruit do you prefer? "}}},
	))
	m.updateFilter()
	m.live = map[string]bool{"live": true, "menu": true, "asks": true}
	return m
}

func waiting(since time.Time, why string) sessionStat {
	return sessionStat{status: "waiting", waitingFor: why, since: since}
}

func typeIn(m model, s string) model {
	for _, r := range s {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// openByHand applies stats, closes any pop-up, and opens the overlay with
// Ctrl+Y, so it has the keyboard and lists every waiting session.
func openByHand(m model, stats map[string]sessionStat) model {
	m.applyStats(stats)
	m.closeWait()
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
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

// sessionFileFor writes this process's session file.
func sessionFileFor(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", os.Getpid()))
	os.WriteFile(path, []byte(body), 0o600)
	return path
}

// fakeStart makes ps report a fixed start time for this process.
func fakeStart(t *testing.T) {
	me, start := os.Getpid(), time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	old := processStartTimes
	t.Cleanup(func() { processStartTimes = old })
	processStartTimes = func(map[string]int) map[int]time.Time { return map[int]time.Time{me: start} }
}

func liveTickOnce(m model) model {
	res, _ := m.Update(m.liveCmd()())
	return res.(model)
}

func TestWaitQueueOrderAndSignals(t *testing.T) {
	m := waitModel(t)
	now := time.Now()
	m.applyStats(map[string]sessionStat{
		"live": waiting(now.Add(-time.Minute), "permission prompt"),
		"menu": waiting(now.Add(-time.Hour), "input needed"),
		"asks": {status: "idle", since: now},
	})
	ids := func(q []Conversation) string {
		var s []string
		for _, c := range q {
			s = append(s, c.SessionID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(m.waitQueue(false)); got != "menu,live" {
		t.Errorf("prompts, oldest first: got %s", got)
	}
	if got := ids(m.waitQueue(true)); got != "menu,live,asks" {
		t.Errorf("by hand the asking session comes last: got %s", got)
	}
	// Shell mode and dead sessions never wait.
	m.setStat("live", sessionStat{status: "shell"})
	m.live["menu"] = false
	if got := ids(m.waitQueue(false)); got != "" {
		t.Errorf("shell and dead sessions aren't waiting: got %s", got)
	}
}

func TestWaitPopsOnTheOldestWithoutTakingKeys(t *testing.T) {
	m := waitModel(t)
	now := time.Now()
	stats := map[string]sessionStat{"live": waiting(now.Add(-time.Minute), "permission prompt"), "menu": waiting(now.Add(-time.Hour), "input needed")}
	m.helpOpen = true
	m.applyStats(stats)
	if m.waitOpen || !m.waitDue["menu"] {
		t.Fatalf("mustn't pop over another popup, but stays due: open=%v due=%v", m.waitOpen, m.waitDue)
	}
	m.helpOpen = false
	m.applyStats(stats) // no status changed, but the pop-up is still due
	if !m.waitOpen || m.waitID != "menu" || m.waitEngaged {
		t.Fatalf("should pop on the oldest waiting session, without the keyboard: open=%v id=%q engaged=%v", m.waitOpen, m.waitID, m.waitEngaged)
	}
	view := strip2(m.View())
	for _, want := range []string{"waiting 1 of 2", "Menu one · input needed", "answer it in its tab", "^Y answer here"} {
		if !strings.Contains(view, want) {
			t.Errorf("overlay should show %q:\n%s", want, view)
		}
	}
	// Ctrl+Y takes the keyboard, Ctrl+N moves on, Ctrl+Y closes; nothing new is waiting, so it stays shut.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.waitID != "live" {
		t.Errorf("ctrl+n should show the next session, got %q", m.waitID)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	m.applyStats(stats)
	if m.waitOpen {
		t.Error("closed by hand: only a new wait pops it again")
	}
}

func TestWaitPopUpLeavesTheSearchItsKeys(t *testing.T) {
	m := waitModel(t)
	m.textInput.SetValue("fix")
	m.updateFilter()
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	m = typeIn(m, "ug")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.textInput.Value() != "fixug" || m.waitInput.Value() != "" || m.pending["live"] != "" {
		t.Fatalf("a pop-up must not take the search's keys: search=%q box=%q sent=%q cmd=%v", m.textInput.Value(), m.waitInput.Value(), m.pending["live"], cmd != nil)
	}
}

func TestWaitPopUpLeavesTheMainBoxItsKeys(t *testing.T) {
	m := selectLive(waitModel(t))
	m.chatFocus = true
	m.chatInput.SetValue("please also run the ")
	m.applyStats(map[string]sessionStat{"menu": waiting(time.Now(), "permission prompt")})
	m = typeIn(m, "tests")
	if !m.waitOpen || m.chatInput.Value() != "please also run the tests" || m.waitInput.Value() != "" {
		t.Fatalf("a pop-up must not take the main box's keys: open=%v main=%q box=%q", m.waitOpen, m.chatInput.Value(), m.waitInput.Value())
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY}) // answer here
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY}) // close
	if !m.chatFocus || m.chatInput.Value() != "please also run the tests" {
		t.Errorf("closing should leave the main box as it was: focus=%v main=%q", m.chatFocus, m.chatInput.Value())
	}
}

func TestWaitAskedNeverPopsButOpensByHand(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessionStat{"asks": {status: "idle", since: time.Now()}})
	if m.waitOpen {
		t.Error("an asking (idle) session mustn't pop up")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if !m.waitOpen || m.waitID != "asks" || !m.waitEngaged {
		t.Errorf("ctrl+y opens by hand, asking sessions included: open=%v id=%q", m.waitOpen, m.waitID)
	}
}

func TestWaitSnoozeComesBackAndEndsWithTheWait(t *testing.T) {
	m := waitModel(t)
	m.snooze = 15 * time.Minute
	stats := map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")}
	m.applyStats(stats)
	if !m.waitOpen {
		t.Fatal("should pop")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc}) // Esc snoozes even before Ctrl+Y
	if m.waitOpen || time.Until(m.snoozed["live"].Until) < 14*time.Minute {
		t.Fatalf("esc snoozes the last one and closes: open=%v snooze=%+v", m.waitOpen, m.snoozed["live"])
	}
	m.applyStats(stats)
	if m.waitOpen || len(m.waitQueue(false)) != 0 {
		t.Fatal("snoozed: no pop-up, and out of the queue")
	}
	rec := m.snoozed["live"]
	rec.Until = time.Now().Add(-time.Second) // time's up, still waiting
	m.snoozed["live"] = rec
	m.applyStats(stats)
	if !m.waitOpen || m.waitID != "live" {
		t.Fatal("a snooze that ran out while still waiting pops up again")
	}

	// Leaving the wait ends the snooze.
	m.snoozed["live"] = snoozeRec{Since: stats["live"].since, Until: time.Now().Add(time.Hour)}
	m.applyStats(map[string]sessionStat{"live": {status: "busy", since: time.Now().Add(time.Second)}})
	if _, ok := m.snoozed["live"]; ok {
		t.Error("a new status should end the snooze")
	}
}

func TestWaitWithoutATimestampBusyThenWaitingIsANewWait(t *testing.T) {
	m := waitModel(t)
	z := (sessionFile{Status: "waiting"}).stat()
	m.applyStats(map[string]sessionStat{"live": z})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc}) // snooze
	m.applyStats(map[string]sessionStat{"live": z})
	if m.waitOpen {
		t.Fatal("the same wait stays snoozed")
	}
	m.applyStats(map[string]sessionStat{"live": (sessionFile{Status: "busy"}).stat()})
	m.applyStats(map[string]sessionStat{"live": z})
	if m.snoozedNow("live") || !m.waitOpen {
		t.Fatalf("busy then waiting is a new wait, even without a timestamp: snoozed=%v open=%v", m.snoozedNow("live"), m.waitOpen)
	}
}

func TestWaitUnreadableTickKeepsTheSnooze(t *testing.T) {
	m := waitModel(t)
	fakeStart(t)
	good := fmt.Sprintf(`{"pid":%d,"sessionId":"live","status":"waiting","statusUpdatedAt":1790000000000,"procStart":"Fri Sep 25 10:00:00 2026"}`, os.Getpid())
	path := sessionFileFor(t, good)
	m.snoozed = map[string]snoozeRec{"live": {Since: time.UnixMilli(1790000000000), Until: time.Now().Add(time.Hour)}}
	m = liveTickOnce(m)
	os.WriteFile(path, []byte(`{"sessionId":`), 0o600) // caught mid-write
	m = liveTickOnce(m)
	if !m.live["live"] {
		t.Error("a session whose file was unreadable for a tick is still live while its process runs")
	}
	os.WriteFile(path, []byte(good), 0o600)
	m = liveTickOnce(m)
	if _, ok := m.snoozed["live"]; !ok || m.waitOpen {
		t.Fatalf("one unreadable tick must not end the snooze: snoozed=%v open=%v", ok, m.waitOpen)
	}
}

func TestWaitEnterSendsOrGoesToTab(t *testing.T) {
	m := waitModel(t)
	sock, got := fakeSocket(t)
	sessionFileFor(t, fmt.Sprintf(`{"sessionId":"live","messagingSocketPath":%q}`, sock))
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "input needed")})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	m = typeIn(m, "blue")
	if m.textInput.Value() != "" || m.chatInput.Value() != "" {
		t.Fatal("typing in the overlay must not reach the search or the main message box")
	}
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || m.pending["live"] != "blue" || m.waitInput.Value() != "" {
		t.Fatalf("enter should send: pending=%q", m.pending["live"])
	}
	done, ok := cmd().(sendDoneMsg)
	if !ok || done.err != nil {
		t.Fatalf("send failed: %+v", done)
	}
	if data := <-got; !strings.Contains(data, "blue") {
		t.Errorf("socket got %q", data)
	}
	res, _ := m.Update(done)
	m = res.(model)
	// Enter on the empty box goes to the session.
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || m.waitOpen {
		t.Error("enter on an empty box should close the overlay and open the session")
	}
}

func TestWaitRefusedSendGivesTheTextBack(t *testing.T) {
	m := waitModel(t)
	sessionFileFor(t, `{"sessionId":"live","status":"waiting"}`) // no socket, on a menu
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	m = typeIn(m, "use option two")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	res, _ := m.Update(cmd())
	m = res.(model)
	if m.waitInput.Value() != "use option two" || !strings.Contains(m.sendNote["live"], "menu") {
		t.Fatalf("a refused send must give the text back: box=%q note=%q", m.waitInput.Value(), m.sendNote["live"])
	}

	// The main box refuses the same way and keeps its text too.
	n := selectLive(waitModel(t))
	sessionFileFor(t, `{"sessionId":"live","status":"waiting"}`)
	n.chatFocus = true
	n = typeIn(n, "go on")
	n, cmd = key(n, tea.KeyMsg{Type: tea.KeyEnter})
	res, _ = n.Update(cmd())
	if n = res.(model); n.chatInput.Value() != "go on" {
		t.Fatalf("a refused send must give the main box its text back: box=%q", n.chatInput.Value())
	}
}

func TestWaitMarkersAndHeaderCount(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt"), "asks": {status: "idle", since: time.Now()}})
	m.closeWait() // to see the list
	view := strip2(m.View())
	if !strings.Contains(view, "2 waiting") {
		t.Errorf("header should count both signals:\n%s", view)
	}
	for _, id := range []string{"live", "asks"} {
		for _, item := range m.filtered {
			if item.conv.SessionID == id && !strings.Contains(strip2(m.formatListItem(item, false)), "? ") {
				t.Errorf("%s should show ? in place of ●", id)
			}
		}
	}
}

func TestSnoozeLength(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 15 * time.Minute, "5m": 5 * time.Minute, "junk": 15 * time.Minute, "-1m": 15 * time.Minute, "0": 15 * time.Minute} {
		if got, ok := snoozeLength(in); got != want || ok != (in == "" || in == "5m") {
			t.Errorf("snoozeLength(%q) = %v, %v", in, got, ok)
		}
	}
	if shortDuration(15*time.Minute) != "15m" || shortDuration(time.Hour) != "1h" || shortDuration(90*time.Second) != "1m30s" || shortDuration(10*time.Second) != "10s" {
		t.Error("shortDuration should drop zero tails")
	}
}

func TestWaitSnoozeSurvivesRestartAndFirstTick(t *testing.T) {
	m := waitModel(t)
	since := time.Now().Add(-time.Hour)
	stats := map[string]sessionStat{"live": waiting(since, "permission prompt")}
	m = openByHand(m, stats)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	data, _ := json.Marshal(m.uiState())
	var s uiState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	n := waitModel(t)
	n.restore(s)
	n.applyStats(stats) // the first live tick after the restart
	if n.waitOpen || len(n.waitQueue(false)) != 0 {
		t.Fatalf("a restored snooze must outlive the first tick: open=%v queue=%d", n.waitOpen, len(n.waitQueue(false)))
	}
}

func TestWaitNewPromptIsNotCoveredByOldSnooze(t *testing.T) {
	m := waitModel(t)
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now().Add(-time.Minute), "permission prompt")})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	// Answered in its tab and a new prompt came within one tick: still "waiting", new statusUpdatedAt.
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "input needed")})
	if !m.waitOpen || m.waitID != "live" {
		t.Fatalf("a new wait should pop despite the old snooze: open=%v id=%q", m.waitOpen, m.waitID)
	}
}

func TestWaitEmptyStatusChangesNothing(t *testing.T) {
	m := waitModel(t)
	stats := map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")}
	m = openByHand(m, stats)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m.applyStats(map[string]sessionStat{"live": {}}) // file caught without a status
	m.applyStats(stats)
	if m.waitOpen {
		t.Fatal("a status-less read mustn't end the snooze")
	}
}

func TestWaitCtrlYCloseStaysClosed(t *testing.T) {
	m := waitModel(t)
	stats := map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")}
	m.applyStats(stats)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY}) // answer here
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY}) // close
	m.applyStats(stats)
	if m.waitOpen {
		t.Fatal("seen and closed: it mustn't pop again for the same wait")
	}
}

func TestWaitCtrlYShowsSnoozedAndKeepsTheirSnoozes(t *testing.T) {
	m := waitModel(t)
	stats := map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")}
	m.applyStats(stats)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if _, ok := m.snoozed["live"]; !m.waitOpen || m.waitID != "live" || !ok || !strings.Contains(strip2(m.waitPopup()), "snoozed") {
		t.Fatalf("ctrl+y shows a snoozed session and keeps its snooze: open=%v snoozed=%v err=%q", m.waitOpen, ok, m.errorMsg)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	rec := m.snoozed["live"]
	rec.Until = time.Now().Add(-time.Second)
	m.snoozed["live"] = rec
	m.applyStats(stats)
	if !m.waitOpen {
		t.Error("after a look, the snooze still runs out and pops it again")
	}
}

func TestWaitDraftStaysWithItsSession(t *testing.T) {
	m := waitModel(t)
	now := time.Now()
	m = openByHand(m, map[string]sessionStat{"menu": waiting(now.Add(-time.Hour), "permission prompt"), "live": waiting(now, "permission prompt")})
	m = typeIn(m, "yes drop the table")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.waitID != "live" || m.waitInput.Value() != "" {
		t.Fatalf("the next session must not inherit the draft: id=%q box=%q", m.waitID, m.waitInput.Value())
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlP})
	if m.waitInput.Value() != "yes drop the table" {
		t.Errorf("going back should bring the draft back, got %q", m.waitInput.Value())
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc}) // snooze it: the next one shows
	if m.waitID != "live" {
		t.Fatalf("esc should move on to the next session, got %q", m.waitID)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.pending["live"] != "" {
		t.Fatalf("enter sent another session's draft: %q", m.pending["live"])
	}
}

func TestWaitNeverTypesIntoAMenu(t *testing.T) {
	chatModel(t)
	pid := os.Getpid()
	sessionFileFor(t, `{"sessionId":"live","status":"waiting","messagingSocketPath":"/tmp/ccs-no-such.sock"}`)
	log := fakeTerminal(t)
	note, err := deliverMessage(pid, "live", "busy", "no, stop")
	if got, _ := os.ReadFile(log); err == nil || len(got) != 0 || !strings.Contains(note, "menu") {
		t.Fatalf("typing into a session on a menu could pick an option: note=%q err=%v typed=%q", note, err, got)
	}
}

func TestWaitOwnsTheKeyboardAndMouse(t *testing.T) {
	m := waitModel(t)
	var msgs []Message
	for i := 0; i < 100; i++ {
		msgs = append(msgs, Message{Role: "user", Text: fmt.Sprint("line ", i)})
	}
	for i := range m.items {
		if m.items[i].conv.SessionID == "live" {
			m.items[i].conv.Messages = msgs
		}
	}
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	engaged, _ := key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	withOffer := engaged
	withOffer.updateOpen, withOffer.updateTo = true, "v9.9.9"
	for name, s := range map[string]model{"popped up": m, "engaged": engaged, "under the update offer": withOffer} {
		for _, k := range []tea.KeyType{tea.KeyCtrlL, tea.KeyCtrlT, tea.KeyCtrlG, tea.KeyCtrlO} {
			if n, _ := key(s, tea.KeyMsg{Type: k}); n.notesOpen || n.linksOpen || n.helpOpen || n.acctOpen {
				t.Errorf("%s: %v opened a popup over the overlay", name, k)
			}
		}
	}
	m.notesOpen = true // a popup above it (e.g. opened before) gets the wheel
	n := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if n.waitScroll != 0 {
		t.Error("the wheel scrolled the hidden overlay under the changelog")
	}
}

func TestWaitCtrlYWorksOnTheUsageScreen(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	m.closeWait()
	m.showUsage = true
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if !m.waitOpen {
		t.Fatal("ctrl+y should open the overlay from the usage screen too")
	}
	if prefix, _, _ := m.searchRowParts(); strings.Contains(prefix, "\x1b[7m") {
		t.Error("the search cursor must not show under the overlay")
	}
}

func TestWaitLongDraftKeepsTheOverlaySize(t *testing.T) {
	m := waitModel(t)
	res, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = openByHand(res.(model), map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	before := strings.Count(m.waitPopup(), "\n")
	m = typeIn(m, strings.Repeat("long draft ", 30))
	pop := m.waitPopup()
	if strings.Count(pop, "\n") != before {
		t.Errorf("a long draft must scroll in its box, not grow the overlay: %d rows, was %d", strings.Count(pop, "\n"), before)
	}
	for _, l := range strings.Split(pop, "\n") {
		if w := ansi.StringWidth(l); w > 100 {
			t.Errorf("overlay line %d wide on a 100-wide terminal", w)
		}
	}
}

func TestChatStatusLineFitsItsWidth(t *testing.T) {
	m := waitModel(t)
	m.pending = map[string]string{"live": strings.Repeat("word ", 40)}
	m.sentAt = map[string]time.Time{"live": time.Now().Add(-time.Minute)}
	var conv Conversation
	for _, it := range m.items {
		if it.conv.SessionID == "live" {
			conv = it.conv
		}
	}
	for _, w := range []int{60, 90} {
		line := strings.Split(m.chatBox(conv, m.waitInput, true, w), "\n")[0]
		if got := ansi.StringWidth(line); got > w {
			t.Errorf("status line is %d wide at width %d: %q", got, w, strip2(line))
		}
	}
}

func TestWaitFitsATinyTerminal(t *testing.T) {
	for _, sz := range [][2]int{{20, 8}, {40, 12}, {120, 40}, {80, 7}} {
		m := waitModel(t)
		m.width, m.height = sz[0], sz[1]
		m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
		pop := strings.Split(m.waitPopup(), "\n")
		w := 0
		for _, l := range pop {
			w = max(w, ansi.StringWidth(l))
		}
		if w > sz[0] || len(pop) > sz[1] {
			t.Errorf("%dx%d terminal: overlay is %dx%d", sz[0], sz[1], w, len(pop))
		}
		if _, _, _, hints := m.waitLayout(); hints && !strings.Contains(strip2(pop[len(pop)-2]), "^Y") {
			t.Errorf("%dx%d terminal: the key hints fell off: %q", sz[0], sz[1], strip2(pop[len(pop)-2]))
		}
	}
}

func TestWaitHintsListEveryKey(t *testing.T) {
	m := waitModel(t)
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	pop := strings.Split(strip2(m.waitPopup()), "\n")
	for _, want := range []string{"enter send", "esc snooze", "↑↓", "pgup", "^Y close"} {
		if !strings.Contains(pop[len(pop)-2], want) {
			t.Errorf("hint row should list %q: %q", want, pop[len(pop)-2])
		}
	}
}

func TestWaitWithoutATimestamp(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessionStat{
		"live": (sessionFile{Status: "waiting"}).stat(),
		"menu": waiting(time.Now().Add(-time.Minute), "input needed"),
	})
	if q := m.waitQueue(false); len(q) != 2 || q[0].SessionID != "menu" {
		t.Errorf("an unknown wait time sorts last: %v", q)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
	title := strings.Split(strip2(m.waitPopup()), "\n")[1]
	if regexp.MustCompile(`\d{4,}m`).MatchString(title) {
		t.Errorf("no age without a timestamp: %q", title)
	}
}

func TestWaitOverlayShowsOnlyItsOwnErrors(t *testing.T) {
	m := waitModel(t)
	m.errorMsg = "Busy one is no longer running"
	m.applyStats(map[string]sessionStat{"menu": waiting(time.Now(), "permission prompt")})
	if v := strip2(m.waitPopup()); strings.Contains(v, "no longer running") || !strings.Contains(v, "showing a menu") {
		t.Fatalf("a main-screen error must not replace the overlay's note:\n%s", v)
	}
}

func TestWaitEnterReportsInsteadOfDoingNothing(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TERM_PROGRAM", "")
	m := waitModel(t)
	sessionFileFor(t, `{"sessionId":"live"}`)
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	m = typeIn(m, "go on")
	m, inFlight := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeIn(m, "and more")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(strings.ToLower(strip2(m.waitPopup())), "still sending") {
		t.Errorf("enter during another send should say so in the overlay: %q", m.waitErr)
	}
	res, _ := m.Update(inFlight())
	m = res.(model)
	m.live = map[string]bool{"menu": true}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.waitErr, "has exited") {
		t.Errorf("enter after the session exited should say so, got %q", m.waitErr)
	}
	// Enter on an empty box when the session file can't be read: report, never resume a second claude.
	sessionFileFor(t, `{"sessionId":"live","pid":`)
	m.live = map[string]bool{"live": true}
	m.waitInput.SetValue("")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on an empty box should try to focus the session")
	}
	res, _ = m.Update(cmd())
	if n := res.(model); n.quitting || !strings.Contains(n.errorMsg, "right now") {
		t.Errorf("a half-written file isn't proof the session exited, and it must not be resumed again: quitting=%v err=%q", n.quitting, n.errorMsg)
	}
}

func TestWaitWarnsAboutABadSnoozeSetting(t *testing.T) {
	t.Setenv("CCS_SNOOZE", "15")
	m := initialModel(nil, "", nil)
	if !strings.Contains(m.errorMsg, "CCS_SNOOZE") || m.snooze != defaultSnooze {
		t.Errorf("a value without a unit should warn and use the default: err=%q snooze=%v", m.errorMsg, m.snooze)
	}
}

func TestWaitAskedHeuristic(t *testing.T) {
	for txt, want := range map[string]int{
		"Shall I go ahead (yes?)":   waitAsked,
		"Want me to push?**":        waitAsked,
		"Ready? ":                   waitAsked,
		"?":                         waitNone,
		"Done.":                     waitNone,
		"see https://x.com/search?": waitNone,
		"it matches `a?`":           waitNone,
	} {
		m := waitModel(t)
		m.chatStatus = map[string]sessionStat{"asks": {status: "idle"}}
		if got := m.waitSignal(Conversation{SessionID: "asks", Messages: []Message{{Role: "assistant", Text: txt}}}); got != want {
			t.Errorf("%q: got %d, want %d", txt, got, want)
		}
	}
}

func TestWaitFastTickFollowsTheOverlay(t *testing.T) {
	m := waitModel(t) // the selection is "idle", not live
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	m.chatTickOn = false
	if m.ensureChatTick() == nil {
		t.Error("the fast tick should follow the overlay's session")
	}
}

func TestWaitForgetsSessionsThatAreGone(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessionStat{"menu": {status: "idle", since: time.Now()}})
	m.live = map[string]bool{"live": true}
	m.applyStats(map[string]sessionStat{"live": {status: "idle", since: time.Now()}})
	if _, ok := m.chatStatus["menu"]; ok {
		t.Error("chatStatus should drop a session that's no longer live")
	}
}

func TestWaitUncheckedPidPopsWithANote(t *testing.T) {
	m := waitModel(t)
	fakeStart(t)
	body := func(procStart string) string {
		return fmt.Sprintf(`{"pid":%d,"sessionId":"live","status":"waiting","statusUpdatedAt":1790000000000,"procStart":%q}`, os.Getpid(), procStart)
	}
	sessionFileFor(t, body("")) // no procStart: can't check the pid
	if s, _ := scanSessions(); !s["live"].unverified || s["live"].stat.status != "waiting" {
		t.Fatalf("a file without procStart should read as unverified: %+v", s["live"])
	}
	m = liveTickOnce(m)
	if !m.waitOpen || !strings.Contains(strip2(m.waitPopup()), "unverified") {
		t.Fatalf("an unchecked pid still pops, with a note: open=%v", m.waitOpen)
	}
	sessionFileFor(t, body("Fri Sep 25 10:00:00 2026"))
	if s, _ := scanSessions(); s["live"].unverified {
		t.Error("a matching procStart is verified")
	}
	sessionFileFor(t, body("Fri Sep 25 09:00:00 2026"))
	if s, _ := scanSessions(); s["live"].pid != 0 {
		t.Error("a start time that doesn't match is a recycled pid: not live at all")
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
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

// openByHand applies stats and opens the overlay with Ctrl+Y.
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

// topRow is the screen's first row: the title, or the banner.
func topRow(m model) string { return strings.Split(strip2(m.View()), "\n")[0] }

// sessionFileFor writes this process's session file.
func sessionFileFor(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", os.Getpid()))
	os.WriteFile(path, []byte(body), 0o600)
	return path
}

// fakeStart makes ps report a fixed start time for this process; move shifts
// it, as if the pid now belonged to another process.
func fakeStart(t *testing.T) (move func(time.Duration)) {
	me, start := os.Getpid(), time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	old := processStartTimes
	t.Cleanup(func() { processStartTimes = old })
	processStartTimes = func(map[string]int) map[int]time.Time { return map[int]time.Time{me: start} }
	return func(d time.Duration) { start = start.Add(d) }
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

func TestWaitBannerShowsTheOldestAndTakesNoKeys(t *testing.T) {
	m := selectLive(waitModel(t))
	now := time.Now()
	m.applyStats(map[string]sessionStat{"live": waiting(now.Add(-time.Minute), "permission prompt"), "menu": waiting(now.Add(-time.Hour), "input needed")})
	if top := bannerRow(m); m.waitOpen || !strings.Contains(top, "Menu one is waiting on you") || !strings.Contains(top, "+1 more") {
		t.Fatalf("a pop-up is a banner on the oldest wait, not the overlay: open=%v top=%q", m.waitOpen, top)
	}
	// Every key works as without it: typing and Esc in the main box (of a
	// session that isn't waiting, the waiting one has none), then Esc in the search.
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "asks" })
	m.chatFocus = true
	m = typeIn(m, "please run the tests")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.chatFocus || m.chatInput.Value() != "please run the tests" || m.snoozedNow("menu") {
		t.Fatalf("esc in the main box goes back to the search: focus=%v main=%q snoozed=%v", m.chatFocus, m.chatInput.Value(), m.snoozedNow("menu"))
	}
	m = typeIn(m, "fix")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.textInput.Value() != "" || m.snoozedNow("menu") || m.waitOpen {
		t.Fatalf("esc clears the search: search=%q snoozed=%v", m.textInput.Value(), m.snoozedNow("menu"))
	}
	// Ctrl+Y opens the overlay on the banner's session; it owns the keyboard.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	view := strip2(m.View())
	for _, want := range []string{"waiting 1 of 2", "Menu one · input needed", "only shown in its own tab"} {
		if !strings.Contains(view, want) {
			t.Errorf("overlay should show %q:\n%s", want, view)
		}
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.waitID != "live" {
		t.Errorf("ctrl+n should show the next session, got %q", m.waitID)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if m.waitOpen || strings.Contains(bannerRow(m), "waiting on you") {
		t.Errorf("both were seen, so after closing there's no banner: open=%v top=%q", m.waitOpen, bannerRow(m))
	}
}

func TestWaitBannerNeverHidesALaterWait(t *testing.T) {
	m := waitModel(t)
	now := time.Now()
	m.applyStats(map[string]sessionStat{"live": waiting(now.Add(-time.Minute), "permission prompt")})
	// Answered in its tab; then another session starts waiting.
	m.applyStats(map[string]sessionStat{"live": {status: "busy", since: now}, "menu": waiting(now, "input needed")})
	if !strings.Contains(bannerRow(m), "Menu one is waiting on you") {
		t.Fatalf("the banner should move on to the new wait: %q", bannerRow(m))
	}
	// Seen and closed, while yet another wait arrives: that one still gets the banner.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	m.applyStats(map[string]sessionStat{"live": waiting(now.Add(time.Second), "permission prompt"), "menu": waiting(now, "input needed")})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if m.waitOpen || !strings.Contains(bannerRow(m), "Busy one is waiting on you") {
		t.Fatalf("a wait that arrived while the overlay was open was never shown, so it gets the banner: open=%v top=%q", m.waitOpen, bannerRow(m))
	}
}

func TestWaitEscOnAStaleSessionShowsTheNext(t *testing.T) {
	m := waitModel(t)
	now := time.Now()
	m = openByHand(m, map[string]sessionStat{"live": waiting(now.Add(-time.Minute), "permission prompt")})
	m.applyStats(map[string]sessionStat{"live": {status: "busy", since: now}, "menu": waiting(now, "input needed")})
	if m.waitID != "live" {
		t.Fatalf("the overlay stays on its session: %q", m.waitID)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.waitOpen || m.waitID != "menu" {
		t.Fatalf("esc on a session no longer waiting must show the next one, not skip it: open=%v id=%q", m.waitOpen, m.waitID)
	}
}

func TestWaitAskedGetsNoBannerButOpensByHand(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessionStat{"asks": {status: "idle", since: time.Now()}})
	if strings.Contains(bannerRow(m), "waiting on you") {
		t.Error("an asking (idle) session gets no banner")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if !m.waitOpen || m.waitID != "asks" {
		t.Errorf("ctrl+y opens by hand, asking sessions included: open=%v id=%q", m.waitOpen, m.waitID)
	}
}

func TestWaitSnoozeComesBackAndEndsWithTheWait(t *testing.T) {
	m := waitModel(t)
	m.snooze = 15 * time.Minute
	stats := map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")}
	m = openByHand(m, stats)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.waitOpen || time.Until(m.snoozed["live"].Until) < 14*time.Minute {
		t.Fatalf("esc snoozes the last one and closes: open=%v snooze=%+v", m.waitOpen, m.snoozed["live"])
	}
	m.applyStats(stats)
	if strings.Contains(bannerRow(m), "waiting on you") || len(m.waitQueue(false)) != 0 {
		t.Fatal("snoozed: no banner, and out of the queue")
	}
	rec := m.snoozed["live"]
	rec.Until = time.Now().Add(-time.Second) // time's up, still waiting
	m.snoozed["live"] = rec
	m.applyStats(stats)
	if !strings.Contains(bannerRow(m), "Busy one is waiting on you") {
		t.Fatal("a snooze that ran out while still waiting gets the banner again")
	}

	// Leaving the wait ends the snooze.
	m.snoozed["live"] = snoozeRec{Since: stats["live"].since, Until: time.Now().Add(time.Hour)}
	m.applyStats(map[string]sessionStat{"live": {status: "busy", since: time.Now().Add(time.Second)}})
	if _, ok := m.snoozed["live"]; ok {
		t.Error("a new status should end the snooze")
	}
}

func TestWaitWithoutATimestamp(t *testing.T) {
	m := waitModel(t)
	z := (sessionFile{Status: "waiting"}).stat()
	m.applyStats(map[string]sessionStat{"live": z, "menu": waiting(time.Now().Add(-time.Minute), "input needed")})
	if q := m.waitQueue(false); len(q) != 2 || q[0].SessionID != "menu" {
		t.Errorf("an unknown wait time sorts last: %v", q)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if title := strings.Split(strip2(m.waitPopup()), "\n")[1]; regexp.MustCompile(`\d{4,}m`).MatchString(title) {
		t.Errorf("no age without a timestamp: %q", title)
	}
	// Snoozed, the same wait stays snoozed; busy then waiting is a new one.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m.applyStats(map[string]sessionStat{"live": z})
	if !m.snoozedNow("live") {
		t.Fatal("the same wait stays snoozed")
	}
	m.applyStats(map[string]sessionStat{"live": (sessionFile{Status: "busy"}).stat()})
	m.applyStats(map[string]sessionStat{"live": z})
	if m.snoozedNow("live") || !m.waitDue["live"] {
		t.Fatalf("busy then waiting is a new wait, even without a timestamp: snoozed=%v due=%v", m.snoozedNow("live"), m.waitDue["live"])
	}
	// A snooze restored after a restart can't be matched to a wait without a timestamp, so it can't hide one.
	n := waitModel(t)
	n.snoozed = map[string]snoozeRec{"live": {Until: time.Now().Add(time.Hour)}}
	n.applyStats(map[string]sessionStat{"live": z})
	if n.snoozedNow("live") {
		t.Error("a restored snooze without a timestamp mustn't hide a wait")
	}
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
		m.snoozed = map[string]snoozeRec{"live": {Since: time.UnixMilli(1790000000000), Until: time.Now().Add(time.Hour)}}
		m = liveTickOnce(m)
		gap(path)
		if m = liveTickOnce(m); !m.live["live"] {
			t.Errorf("%s: a session without its file for a tick is still live while its process runs", name)
		}
		os.WriteFile(path, []byte(good), 0o600)
		m = liveTickOnce(m)
		if !m.snoozedNow("live") || strings.Contains(bannerRow(m), "waiting on you") {
			t.Errorf("%s: one tick without the file must not end the snooze: snoozed=%v top=%q", name, m.snoozedNow("live"), bannerRow(m))
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
	for i := 0; i <= keepTicks; i++ {
		m = liveTickOnce(m)
	}
	if m.live["live"] {
		t.Error("an unverified session without its file is kept only keepTicks ticks")
	}
}

func TestWaitEnterSendsOrGoesToTab(t *testing.T) {
	m := waitModel(t)
	sock, got := fakeSocket(t)
	sessionFileFor(t, fmt.Sprintf(`{"sessionId":"live","messagingSocketPath":%q}`, sock))
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "input needed")})
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
	if m.waitInput.Value() != "use option two" || !strings.Contains(m.sendNote["live"], "menu") || !strings.Contains(m.sendNote["live"], "put back") {
		t.Fatalf("a refused send must give the text back: box=%q note=%q", m.waitInput.Value(), m.sendNote["live"])
	}
	// A draft written meanwhile keeps both.
	m.waitInput.SetValue("")
	m = typeIn(m, "the long careful answer")
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = typeIn(m, "new start")
	res, _ = m.Update(cmd())
	if v := res.(model).waitInput.Value(); !strings.Contains(v, "the long careful answer") || !strings.Contains(v, "new start") {
		t.Fatalf("a refused send must not drop text when the box has moved on: %q", v)
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
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt"), "asks": {status: "idle", since: time.Now()}})
	m.closeWait() // seen: the title row is back
	if view := strip2(m.View()); !strings.Contains(view, "1 waiting") {
		t.Errorf("header counts menus and prompts, not the dim ? guess:\n%s", view)
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
	if strings.Contains(bannerRow(n), "waiting on you") || len(n.waitQueue(false)) != 0 {
		t.Fatalf("a restored snooze must outlive the first tick: top=%q queue=%d", bannerRow(n), len(n.waitQueue(false)))
	}
}

func TestWaitNewPromptIsNotCoveredByOldSnooze(t *testing.T) {
	m := waitModel(t)
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now().Add(-time.Minute), "permission prompt")})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	// Answered in its tab and a new prompt came within one tick: still "waiting", new statusUpdatedAt.
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "input needed")})
	if !strings.Contains(bannerRow(m), "Busy one is waiting on you") {
		t.Fatalf("a new wait gets the banner despite the old snooze: %q", bannerRow(m))
	}
}

func TestWaitEmptyStatusChangesNothing(t *testing.T) {
	m := waitModel(t)
	stats := map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")}
	m = openByHand(m, stats)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m.applyStats(map[string]sessionStat{"live": {}}) // file caught without a status
	m.applyStats(stats)
	if !m.snoozedNow("live") || m.waitDue["live"] {
		t.Fatal("a status-less read mustn't end the snooze")
	}
}

func TestWaitSeenAndClosedGetsNoBanner(t *testing.T) {
	m := waitModel(t)
	stats := map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")}
	m = openByHand(m, stats)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY}) // close
	m.applyStats(stats)
	if m.waitOpen || strings.Contains(bannerRow(m), "waiting on you") {
		t.Fatal("seen and closed: the same wait gets no banner again")
	}
}

func TestWaitCtrlYShowsSnoozedAndKeepsTheirSnoozes(t *testing.T) {
	m := waitModel(t)
	stats := map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")}
	m = openByHand(m, stats)
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
	if !strings.Contains(bannerRow(m), "Busy one is waiting on you") {
		t.Error("after a look, the snooze still runs out and the banner comes back")
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
	open := openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	withOffer := open
	withOffer.updateOpen, withOffer.updateTo = true, "v9.9.9"
	for name, s := range map[string]model{"open": open, "under the update offer": withOffer} {
		for _, k := range []tea.KeyType{tea.KeyCtrlL, tea.KeyCtrlT, tea.KeyCtrlG, tea.KeyCtrlO} {
			if n, _ := key(s, tea.KeyMsg{Type: k}); n.notesOpen || n.linksOpen || n.helpOpen || n.acctOpen {
				t.Errorf("%s: %v opened a popup over the overlay", name, k)
			}
		}
	}
	open.notesOpen = true // a popup above it (e.g. opened before) gets the wheel
	n := open.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if n.waitScroll != 0 {
		t.Error("the wheel scrolled the hidden overlay under the changelog")
	}
}

func TestWaitCtrlYWorksOnTheUsageScreen(t *testing.T) {
	m := waitModel(t)
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	m.showUsage = true
	if !strings.Contains(bannerRow(m), "Busy one is waiting on you") {
		t.Errorf("the banner shows on the usage screen too: %q", bannerRow(m))
	}
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
	for _, sz := range [][2]int{{20, waitMinHeight}, {40, 12}, {120, 40}, {80, waitMinHeight}} {
		m := waitModel(t)
		m.width, m.height = sz[0], sz[1]
		m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
		pop := strings.Split(m.waitPopup(), "\n")
		w := 0
		for _, l := range pop {
			w = max(w, ansi.StringWidth(l))
		}
		if !m.waitOpen || w > sz[0] || len(pop) > sz[1] {
			t.Errorf("%dx%d terminal: overlay is %dx%d (open=%v)", sz[0], sz[1], w, len(pop), m.waitOpen)
		}
		if !strings.Contains(strip2(pop[len(pop)-2]), "enter") {
			t.Errorf("%dx%d terminal: the key hints fell off: %q", sz[0], sz[1], strip2(pop[len(pop)-2]))
		}
	}
	// Shorter, there's only the banner, and Ctrl+Y says why.
	m := waitModel(t)
	m.height = waitMinHeight - 1
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if m.waitOpen || !strings.Contains(m.errorMsg, "taller") || !strings.Contains(bannerRow(m), "waiting on you") {
		t.Fatalf("a terminal too short for the overlay keeps the banner and says why: open=%v err=%q", m.waitOpen, m.errorMsg)
	}
	// Shrinking under it closes the overlay.
	m.height = 40
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	res, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: waitMinHeight - 1})
	if res.(model).waitOpen {
		t.Error("a window shrunk under the overlay's height closes it")
	}
}

func TestWaitHintsListEveryKey(t *testing.T) {
	m := waitModel(t)
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	pop := strings.Split(strip2(m.waitPopup()), "\n")
	for _, want := range []string{"enter open its tab", "esc snooze", "↑↓", "pgup", "^Y close", "^C quit"} {
		if !strings.Contains(pop[len(pop)-2], want) {
			t.Errorf("hint row should list %q: %q", want, pop[len(pop)-2])
		}
	}
}

func TestWaitOverlayShowsOnlyItsOwnErrors(t *testing.T) {
	m := waitModel(t)
	m = openByHand(m, map[string]sessionStat{"menu": waiting(time.Now(), "permission prompt")})
	m.errorMsg = "Busy one is no longer running"
	if v := strip2(m.waitPopup()); strings.Contains(v, "no longer running") || !strings.Contains(v, "only shown in its own tab") {
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
		"Shall I go ahead (yes?)": waitAsked,
		"Want me to push?**":      waitAsked,
		"Ready? ":                 waitAsked,
		"?":                       waitNone,
		"Done.":                   waitNone,
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

func TestWaitUncheckedPidGetsTheBannerWithANote(t *testing.T) {
	m := waitModel(t)
	fakeStart(t)
	body := func(procStart string) string {
		return fmt.Sprintf(`{"pid":%d,"sessionId":"live","status":"waiting","statusUpdatedAt":1790000000000,"procStart":%q}`, os.Getpid(), procStart)
	}
	sessionFileFor(t, body("")) // no procStart: can't check the pid
	if s := scanSessions(nil); !s["live"].unverified || s["live"].stat.status != "waiting" {
		t.Fatalf("a file without procStart should read as unverified: %+v", s["live"])
	}
	m = liveTickOnce(m)
	if !strings.Contains(bannerRow(m), "Busy one is waiting on you") {
		t.Fatalf("an unchecked pid still gets the banner: %q", bannerRow(m))
	}
	if m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY}); !strings.Contains(strip2(m.waitPopup()), "unverified") {
		t.Fatal("the overlay notes an unchecked pid")
	}
	sessionFileFor(t, body("Fri Sep 25 10:00:00 2026"))
	if s := scanSessions(nil); s["live"].unverified {
		t.Error("a matching procStart is verified")
	}
	sessionFileFor(t, body("Fri Sep 25 09:00:00 2026"))
	if s := scanSessions(nil); s["live"].pid != 0 {
		t.Error("a start time that doesn't match is a recycled pid: not live at all")
	}
}

func TestPasteGoesToTheFocusedInput(t *testing.T) {
	m := waitModel(t)
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return !m.live[it.conv.SessionID] }) // can be renamed
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlR})
	if !m.renaming {
		t.Fatal("ctrl+r should start a rename")
	}
	res, _ := m.Update(textinput.Paste()) // the clipboard's text, or an error without one
	if n := res.(model); n.textInput.Value() != "" || n.textInput.Err != nil {
		t.Errorf("a paste during a rename went to the hidden search: %q %v", n.textInput.Value(), n.textInput.Err)
	}
}

// bannerRow describes the wait due to be shown (the overlay opens on it), or "".
func bannerRow(m model) string {
	c, more, ok := m.banner()
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s is waiting on you +%d more", m.sessionName(c), more)
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
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "permission prompt"), "menu": waiting(time.Now(), "permission prompt")})
	m = typeIn(m, "half an answer")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN}) // the draft is kept with its session
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	m.live = map[string]bool{"menu": true}
	m.applyStats(map[string]sessionStat{"menu": waiting(time.Now(), "permission prompt")})
	if !strings.Contains(m.errorMsg, "discarded") {
		t.Fatalf("a kept draft thrown away when its session exits should be reported: %q", m.errorMsg)
	}
}

func TestWaitHeaderCountKeepsTheTitleRow(t *testing.T) {
	m := waitModel(t)
	m.updating, m.updateTo = true, "v9.9.9"
	m.progress = &updateProgress{step: "downloading", started: time.Now()}
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt")})
	if top := topRow(m); !strings.Contains(top, "updating to v9.9.9") || !strings.Contains(top, "1 waiting · ^Y") || !strings.Contains(top, "quit") {
		t.Fatalf("the waiting count sits with the title row's notes and hints: %q", top)
	}
	if strings.Contains(strip2(m.View()), "waiting on you") {
		t.Error("no banner row under the search")
	}
	m.helpOpen = true
	if top := topRow(m); !strings.Contains(top, "1 waiting") || strings.Contains(top, "^Y") {
		t.Errorf("while help owns the keys, Ctrl+Y does nothing, so it isn't offered: %q", top)
	}
}

func TestWaitingQuestionShownAndAnswered(t *testing.T) {
	m := waitModel(t)
	conv := &m.items[0].conv
	for i := range m.items {
		if m.items[i].conv.SessionID == "live" {
			conv = &m.items[i].conv
		}
	}
	ask := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"AskUserQuestion","input":{"questions":[{"question":"Pick a colour","options":[{"label":"red"},{"label":"green","description":"calm"}]}]}}]}}`
	if !parseLine(conv, []byte(ask)) || answerable(*conv) != 2 {
		t.Fatalf("pending = %+v", conv.pending)
	}
	m.filtered = m.items
	m = openByHand(m, map[string]sessionStat{"live": waiting(time.Now(), "input needed")})
	pop := strip2(m.waitPopup())
	for _, want := range []string{"? Pick a colour", "1  red", "2  green", "     calm", "1-2 answer"} {
		if !strings.Contains(pop, want) {
			t.Errorf("overlay should show %q:\n%s", want, pop)
		}
	}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if m = res.(model); cmd == nil || m.waitInput.Value() != "" {
		t.Error("a digit on an empty box answers, not types")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if m = res.(model); m.waitInput.Value() != "9" {
		t.Error("a digit past the options is typed")
	}
	// Its result clears it.
	parseLine(conv, []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"green"}]}}`))
	if conv.pending != nil {
		t.Error("the tool result should clear the pending question")
	}
}

func TestWaitingMultiSelectShownInPreviewPane(t *testing.T) {
	m := waitModel(t)
	m.width, m.height = 100, 40
	ask := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"AskUserQuestion","input":{"questions":[{"question":"Which checks?","multiSelect":true,"options":[{"label":"lint"},{"label":"tests"}]}]}}]}}`
	for i := range m.items {
		if m.items[i].conv.SessionID == "live" {
			parseLine(&m.items[i].conv, []byte(ask))
		}
	}
	m.updateFilter()
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "input needed")})
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
	if m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlS}); m.chatFocus {
		t.Error("^S can't focus a hidden box")
	}
}

func TestListOrdersWaitingThenIdleThenBusy(t *testing.T) {
	m := selectLive(waitModel(t))
	row := m.cursor
	m.applyStats(map[string]sessionStat{"menu": waiting(time.Now(), "input needed"), "asks": {status: "idle"}, "live": {status: "busy"}})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlG}) // any update re-sorts
	ids := func() (got []string) {
		for _, it := range m.filtered {
			got = append(got, it.conv.SessionID)
		}
		return got
	}
	// The selected session (now busy) keeps its row; the rest sort around it.
	if m.cursor != row || m.filtered[row].conv.SessionID != "live" {
		t.Errorf("the selected session should keep its row %d while selected: cursor %d, %v", row, m.cursor, ids())
	}
	rest := slices.DeleteFunc(ids(), func(id string) bool { return id == "live" })
	if len(rest) < 3 || strings.Join(rest[:2], ",") != "menu,asks" || m.live[rest[2]] {
		t.Errorf("the others: waiting, idle, then sessions not open: got %v", rest)
	}
	// Once something else is selected it sorts in: waiting, idle, busy.
	m.cursor = slices.Index(ids(), "idle")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlG})
	if got := slices.DeleteFunc(ids(), func(id string) bool { return id == "idle" }); strings.Join(got[:3], ",") != "menu,asks,live" {
		t.Errorf("waiting, idle, busy: got %v", ids())
	}
	if m.filtered[m.cursor].conv.SessionID != "idle" {
		t.Errorf("the selection stays on its session: %s", m.filtered[m.cursor].conv.SessionID)
	}
}

func TestLongQuestionWrapsInFull(t *testing.T) {
	long := strings.Repeat("the question goes on ", 12) + "END"
	desc := strings.Repeat("a long description ", 10) + "TAIL"
	var conv Conversation
	in := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"AskUserQuestion","input":{"questions":[{"question":%q,"options":[{"label":"yes","description":%q}]}]}}]}}`, long, desc)
	if !parseLine(&conv, []byte(in)) {
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

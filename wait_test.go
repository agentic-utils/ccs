package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// waitModel has three live sessions: two that may wait, one that asked.
func waitModel(t *testing.T) model {
	t.Helper()
	m := chatModel(t)
	m.items = buildItems([]Conversation{
		{SessionID: "idle", Title: "Quiet one", LastTimestamp: "2026-09-29T11:00:00Z", Messages: []Message{{Role: "user", Text: "hello"}}},
		{SessionID: "live", Title: "Busy one", LastTimestamp: "2026-09-29T10:00:00Z", Messages: []Message{{Role: "user", Text: "hi"}}},
		{SessionID: "old", Title: "Old one", LastTimestamp: "2026-09-29T09:00:00Z", Messages: []Message{{Role: "user", Text: "go"}}},
		{SessionID: "asks", Title: "Asking one", LastTimestamp: "2026-09-29T08:00:00Z", Messages: []Message{{Role: "assistant", Text: "Which fruit do you prefer? "}}},
	})
	m.updateFilter()
	m.live = map[string]bool{"live": true, "old": true, "asks": true}
	return m
}

func waiting(since time.Time, why string) sessionStat {
	return sessionStat{status: "waiting", waitingFor: why, since: since}
}

func TestWaitQueueOrderAndSignals(t *testing.T) {
	m := waitModel(t)
	now := time.Now()
	m.applyStats(map[string]sessionStat{
		"live": waiting(now.Add(-time.Minute), "permission prompt"),
		"old":  waiting(now.Add(-time.Hour), "input needed"),
		"asks": {status: "idle", since: now},
	})
	ids := func(q []Conversation) string {
		var s []string
		for _, c := range q {
			s = append(s, c.SessionID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(m.waitQueue(false)); got != "old,live" {
		t.Errorf("prompts, oldest first: got %s", got)
	}
	if got := ids(m.waitQueue(true)); got != "old,live,asks" {
		t.Errorf("by hand the asking session comes last: got %s", got)
	}
	// Shell mode and dead sessions never wait.
	m.setStat("live", sessionStat{status: "shell"})
	m.live["old"] = false
	if got := ids(m.waitQueue(false)); got != "" {
		t.Errorf("shell and dead sessions aren't waiting: got %s", got)
	}
}

func TestWaitPopsAtStartupOnOldestAndNotWhileBusy(t *testing.T) {
	m := waitModel(t)
	now := time.Now()
	stats := map[string]sessionStat{"live": waiting(now.Add(-time.Minute), "permission prompt"), "old": waiting(now.Add(-time.Hour), "input needed")}

	// Typing just now: the pop-up waits for a quiet tick.
	m.lastKey = now
	m.applyStats(stats)
	if m.waitOpen || !m.waitDue {
		t.Fatalf("mustn't pop mid-typing, but stays due: open=%v due=%v", m.waitOpen, m.waitDue)
	}
	m.lastKey = time.Time{}
	m.helpOpen = true
	m.applyStats(stats)
	if m.waitOpen {
		t.Fatal("mustn't pop over another popup")
	}
	m.helpOpen = false
	m.applyStats(stats) // no status changed, but the pop-up is still due
	if !m.waitOpen || m.waitID != "old" {
		t.Fatalf("should pop on the oldest waiting session: open=%v id=%q", m.waitOpen, m.waitID)
	}
	view := strip2(m.View())
	for _, want := range []string{"waiting 1 of 2", "Old one · input needed", "answer it in its tab"} {
		if !strings.Contains(view, want) {
			t.Errorf("overlay should show %q:\n%s", want, view)
		}
	}

	// Ctrl+N moves on, Ctrl+Y closes; nothing new is waiting, so it stays shut.
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.waitID != "live" {
		t.Errorf("ctrl+n should show the next session, got %q", m.waitID)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	m.lastKey = time.Time{}
	m.applyStats(stats)
	if m.waitOpen {
		t.Error("closed by hand: only a new wait pops it again")
	}
	// Idle sessions whose last message asks something never pop on their own.
	m = waitModel(t)
	m.applyStats(map[string]sessionStat{"asks": {status: "idle", since: now}})
	if m.waitOpen {
		t.Error("an asking (idle) session mustn't pop up")
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
	m.applyStats(stats)
	if !m.waitOpen {
		t.Fatal("should pop")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.waitOpen || time.Until(m.snoozed["live"]) < 14*time.Minute {
		t.Fatalf("esc snoozes the last one and closes: open=%v until=%v", m.waitOpen, m.snoozed["live"])
	}
	m.lastKey = time.Time{}
	m.applyStats(stats)
	if m.waitOpen || len(m.waitQueue(true)) != 0 {
		t.Fatal("snoozed: no pop-up, and out of the queue")
	}
	m.snoozed["live"] = time.Now().Add(-time.Second) // time's up, still waiting
	m.applyStats(stats)
	if !m.waitOpen || m.waitID != "live" {
		t.Fatal("a snooze that ran out while still waiting pops up again")
	}

	// Leaving the wait ends the snooze, and so does any later wait.
	m.snoozed["live"] = time.Now().Add(time.Hour)
	m.applyStats(map[string]sessionStat{"live": {status: "busy", since: time.Now()}})
	if _, ok := m.snoozed["live"]; ok {
		t.Error("a status change should end the snooze")
	}

	// Snoozes survive the post-update restart.
	m.snoozed = map[string]time.Time{"live": time.Now().Add(time.Hour)}
	n := waitModel(t)
	n.restore(m.uiState())
	if !n.snoozed["live"].Equal(m.snoozed["live"]) {
		t.Errorf("snoozes should be restored: %v", n.snoozed)
	}
}

func TestWaitEnterSendsOrGoesToTab(t *testing.T) {
	m := waitModel(t)
	sock, got := fakeSocket(t)
	os.WriteFile(filepath.Join(getSessionsDir(), fmt.Sprintf("%d.json", os.Getpid())), []byte(fmt.Sprintf(`{"sessionId":"live","messagingSocketPath":%q}`, sock)), 0o600)
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "input needed")})
	for _, r := range "blue" {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if m.textInput.Value() != "" || m.chatInput.Value() != "" {
		t.Fatal("typing in the overlay must not reach the search or the main message box")
	}
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || m.pending["live"] != "blue" || m.waitInput.Value() != "" {
		t.Fatalf("enter should send: pending=%q", m.pending["live"])
	}
	if done, ok := cmd().(sendDoneMsg); !ok || done.err != nil {
		t.Fatalf("send failed: %+v", done)
	}
	if data := <-got; !strings.Contains(data, "blue") {
		t.Errorf("socket got %q", data)
	}
	// Enter on the empty box goes to the session (resume finds it live).
	m.sending = false
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || m.waitOpen {
		t.Error("enter on an empty box should close the overlay and open the session")
	}
}

func TestWaitMarkersAndHeaderCount(t *testing.T) {
	m := waitModel(t)
	m.lastKey = time.Now() // keep the overlay shut to see the list
	m.applyStats(map[string]sessionStat{"live": waiting(time.Now(), "permission prompt"), "asks": {status: "idle", since: time.Now()}})
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
	for in, want := range map[string]time.Duration{"": 15 * time.Minute, "5m": 5 * time.Minute, "junk": 15 * time.Minute, "-1m": 15 * time.Minute} {
		if got := snoozeLength(in); got != want {
			t.Errorf("snoozeLength(%q) = %v, want %v", in, got, want)
		}
	}
	if shortDuration(15*time.Minute) != "15m" || shortDuration(time.Hour) != "1h" || shortDuration(90*time.Second) != "1m30s" || shortDuration(10*time.Second) != "10s" {
		t.Error("shortDuration should drop zero tails")
	}
}

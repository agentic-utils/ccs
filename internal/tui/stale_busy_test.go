package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// staleConv parses made-up transcript lines for id: a turn that ended
// 5 minutes before now, then the extra lines.
func staleConv(id string, now time.Time, last string, extra ...string) transcript.Conversation {
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	conv := transcript.Conversation{SessionID: id, Title: id, LastTimestamp: ts(5 * time.Minute)}
	lines := append([]string{
		fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"do the thing"}}`, ts(10*time.Minute)),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":"a","role":"assistant","content":[{"type":"text","text":%q}]}}`, ts(5*time.Minute+time.Second), last),
		fmt.Sprintf(`{"type":"system","subtype":"stop_hook_summary","timestamp":%q}`, ts(5*time.Minute)),
	}, extra...)
	for _, l := range lines {
		transcript.ParseLine(&conv, []byte(l))
	}
	return conv
}

// Claude Code can leave a session's file on busy after its turn ends. A
// turn end in the transcript after the status time, with nothing said
// since, reads idle: done, or asked when its last message asks something,
// so Tab finds it. A user line after the end is a real turn, and a wait is
// never overridden.
func TestStaleBusyReadsIdleOnceTheTurnEnded(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour) // statusUpdatedAt, left from an earlier turn
	m := waitModel(t)
	m.width, m.height = 140, 40
	m.marks = &sessionMarks{Since: now.Add(-24 * time.Hour), Seen: map[string]time.Time{}}
	var convs []transcript.Conversation
	for _, it := range m.items {
		convs = append(convs, it.conv)
	}
	convs = append(convs,
		staleConv("done", now, "All done."),
		staleConv("asked", now, "Shall I merge it?"),
		staleConv("real", now, "All done.",
			fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":"now the next thing"}}`, now.Add(-time.Minute).Format(time.RFC3339Nano))),
		staleConv("held", now, "Pick one."),
	)
	m.items = buildItems(convs)
	m.updateFilter()
	m.live = map[string]bool{"live": true, "menu": true, "asks": true, "done": true, "asked": true, "real": true, "held": true}

	busy := sessions.SessionStat{Status: "busy", Since: stale}
	for _, id := range []string{"done", "asked", "real"} {
		m.setStat(id, busy)
	}
	m.setStat("held", waiting(stale, "input needed"))
	m.setStat("menu", sessions.SessionStat{Status: "busy", Since: now}) // no turn end: really busy
	end := now.Add(-5 * time.Minute)
	for id, want := range map[string]string{"done": "idle", "asked": "idle", "real": "busy", "held": "waiting", "menu": "busy"} {
		if got := m.chatStatus[id].Status; got != want {
			t.Errorf("%s reads %s, want %s", id, got, want)
		}
	}
	if got := m.chatStatus["done"].Since; !got.Equal(end) {
		t.Errorf("idle since its turn ended %v, got %v", end, got)
	}
	if got := m.chatStatus["real"].Since; !got.Equal(stale) {
		t.Errorf("a real busy keeps the file's time: %v", got)
	}
	m.sortList()
	for id, want := range map[string]int{"done": attentionUnread, "asked": attentionAsked, "real": attentionNone, "held": attentionWaits} {
		if got := m.attention(m.items[m.indexOf(id)].conv); got != want {
			t.Errorf("%s is tier %d, want %d", id, got, want)
		}
	}
	// With the wait answered, Tab goes to the stale session that asked.
	m.setStat("held", sessions.SessionStat{Status: "busy", Since: now})
	if m = tab(m); selected(m) != "asked" {
		t.Errorf("Tab: on %s, want asked", selected(m))
	}
}

// The fast tick applies the transcript before the status, so a turn end it
// just read already counts.
func TestFastTickReadsTheTurnEndBeforeTheStatus(t *testing.T) {
	now := time.Now().UTC()
	m := waitModel(t)
	conv := staleConv("live", now, "All done.")
	stat := sessions.SessionStat{Status: "busy", Since: now.Add(-time.Hour)}
	nm, _ := m.Update(chatTickResult{id: "live", conv: &conv, stat: stat, gen: m.gen})
	m = nm.(model)
	if got := m.chatStatus["live"].Status; got != "idle" {
		t.Errorf("live reads %s, want idle", got)
	}
}

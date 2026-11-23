package transcript

import (
	"encoding/json"
	"testing"
)

// queueLines applies records, each a map marshalled to one line, to a fresh
// conversation.
func queueLines(t *testing.T, conv *Conversation, records ...map[string]any) {
	t.Helper()
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if !ParseLine(conv, b) {
			t.Fatalf("not applied: %s", b)
		}
	}
}

func qop(op, content, ts string, extra ...string) map[string]any {
	r := map[string]any{"type": "queue-operation", "operation": op, "timestamp": ts}
	if content != "" {
		r["content"] = content
	}
	for i := 0; i+1 < len(extra); i += 2 {
		r[extra[i]] = extra[i+1]
	}
	return r
}

func queueUserLine(text, ts string) map[string]any {
	return map[string]any{"type": "user", "timestamp": ts, "message": map[string]any{"role": "user", "content": text}}
}

func queuedAttachment(text, ts string) map[string]any {
	return map[string]any{"type": "attachment", "timestamp": ts, "attachment": map[string]any{"type": "queued_command", "prompt": text, "commandMode": "prompt"}}
}

func queuedTexts(c Conversation) []string {
	var out []string
	for _, q := range c.QueuedNow() {
		out = append(out, q.Text)
	}
	return out
}

func sameTexts(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A message typed mid-turn is queued until Claude takes it in: a remove
// (absorbed_mid_turn) and then its queued_command, which is the message as
// the conversation shows it.
func TestQueueTypedMidTurn(t *testing.T) {
	var c Conversation
	queueLines(t, &c,
		queueUserLine("tidy the docs", "2026-11-23T10:00:00Z"),
		qop("enqueue", "and the readme", "2026-11-23T10:00:05Z"),
		qop("enqueue", "and the changelog", "2026-11-23T10:00:06Z"),
	)
	if got := queuedTexts(c); !sameTexts(got, []string{"and the readme", "and the changelog"}) {
		t.Fatalf("queued: %q", got)
	}
	if len(c.Messages) != 1 {
		t.Errorf("a queued message isn't a message yet: %d", len(c.Messages))
	}
	queueLines(t, &c,
		qop("remove", "and the readme", "2026-11-23T10:00:09Z", "reason", "absorbed_mid_turn"),
		queuedAttachment("and the readme", "2026-11-23T10:00:05Z"),
	)
	if got := queuedTexts(c); !sameTexts(got, []string{"and the changelog"}) {
		t.Errorf("after one is taken in: %q", got)
	}
	if n := len(c.Messages); n != 2 || c.Messages[1].Text != "and the readme" {
		t.Errorf("the one taken in is a message now: %+v", c.Messages)
	}
}

// The same text queued twice: the queued_command that follows a remove is
// that message's copy, so the second stays queued.
func TestQueueSameTextTwice(t *testing.T) {
	var c Conversation
	queueLines(t, &c,
		qop("enqueue", "yes", "2026-11-23T10:00:05Z"),
		qop("enqueue", "yes", "2026-11-23T10:00:06Z"),
		qop("remove", "yes", "2026-11-23T10:00:09Z", "reason", "absorbed_mid_turn"),
		queuedAttachment("yes", "2026-11-23T10:00:05Z"),
	)
	if got := queuedTexts(c); len(got) != 1 {
		t.Errorf("the second yes should still be queued: %q", got)
	}
}

// A dequeue names no message: the user line that follows is the one taken in.
// A message from another session is taken in wrapped differently from how it
// was queued (an introduction line, attributes such as hop-chain dropped).
func TestQueueDequeuedAtTurnStart(t *testing.T) {
	queued := "<cross-session-message from=\"uds:/tmp/s/1.sock\" hop-chain=\"ab12\" from-name=\"planner\">\nrun the migration\n</cross-session-message>"
	given := "Another Claude session sent a message:\n<cross-session-message from=\"uds:/tmp/s/1.sock\" from-name=\"planner\">\nrun the migration\n</cross-session-message>"
	var c Conversation
	queueLines(t, &c,
		qop("enqueue", "check the build", "2026-11-23T10:00:05Z"),
		qop("enqueue", queued, "2026-11-23T10:00:06Z"),
	)
	if got := c.QueuedNow(); len(got) != 2 || got[1].Plain() != "from planner: run the migration" {
		t.Fatalf("queued: %+v", got)
	}
	queueLines(t, &c,
		map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 9000, "timestamp": "2026-11-23T10:00:10Z"},
		qop("dequeue", "", "2026-11-23T10:00:10Z"),
		map[string]any{"type": "user", "isMeta": true, "timestamp": "2026-11-23T10:00:10Z", "message": map[string]any{"role": "user", "content": given}},
	)
	if got := queuedTexts(c); !sameTexts(got, nil) {
		t.Errorf("hidden once its turn ended, and the peer's taken in: %q", got)
	}
	for _, q := range c.Queue {
		if q.Text == queued {
			t.Errorf("the peer's message should have left the queue: %+v", c.Queue)
		}
	}
	queueLines(t, &c,
		qop("dequeue", "", "2026-11-23T10:00:11Z"),
		queueUserLine("check the build", "2026-11-23T10:00:11Z"),
	)
	if len(c.Queue) != 0 {
		t.Errorf("both taken in: %+v", c.Queue)
	}
}

// Pulled back into the prompt to edit (popAll, popOne) or dropped (remove of
// any reason): no longer queued.
func TestQueuePoppedAndRemoved(t *testing.T) {
	var c Conversation
	queueLines(t, &c,
		qop("enqueue", "one", "2026-11-23T10:00:01Z"),
		qop("enqueue", "two", "2026-11-23T10:00:02Z"),
		qop("enqueue", "three", "2026-11-23T10:00:03Z"),
		qop("popAll", "one", "2026-11-23T10:00:04Z"),
		qop("popOne", "two", "2026-11-23T10:00:04Z"),
		qop("remove", "three", "2026-11-23T10:00:05Z", "reason", "cleared_on_cancel"),
	)
	if len(c.Queue) != 0 || len(c.queueTaken) != 0 {
		t.Errorf("queue %+v taken %q", c.Queue, c.queueTaken)
	}
	// Edited and sent again: queued again, and taken in by its user line.
	queueLines(t, &c,
		qop("enqueue", "one", "2026-11-23T10:00:06Z"),
		qop("dequeue", "", "2026-11-23T10:00:07Z"),
		queueUserLine("one", "2026-11-23T10:00:07Z"),
	)
	if len(c.Queue) != 0 {
		t.Errorf("resent and taken in: %+v", c.Queue)
	}
}

// Task notifications, channel events and other harness text are queued too,
// but Claude Code doesn't show them as queued messages.
func TestQueueSkipsHarnessText(t *testing.T) {
	var c Conversation
	queueLines(t, &c,
		qop("enqueue", "<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>", "2026-11-23T10:00:01Z"),
		qop("enqueue", `<channel source="plugin:x:pr-events" event="merged">merged</channel>`, "2026-11-23T10:00:02Z"),
		qop("enqueue", "", "2026-11-23T10:00:03Z"),
	)
	if len(c.Queue) != 0 {
		t.Errorf("harness text isn't a queued message: %+v", c.Queue)
	}
	if c.FirstTimestamp != "" || c.LastTimestamp != "" {
		t.Errorf("the queue doesn't move WHEN: %q %q", c.FirstTimestamp, c.LastTimestamp)
	}
}

// A message ccs never saw taken in (a slash command, which shows as its
// output) is hidden once a turn ends after it.
func TestQueueNowAfterTurnEnd(t *testing.T) {
	var c Conversation
	queueLines(t, &c,
		qop("enqueue", "/rename tidy", "2026-11-23T10:00:01Z"),
		map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 1000, "timestamp": "2026-11-23T10:00:02Z"},
	)
	if len(c.Queue) != 1 || len(c.QueuedNow()) != 0 {
		t.Errorf("kept but not shown: %+v %+v", c.Queue, c.QueuedNow())
	}
	queueLines(t, &c, qop("enqueue", "next", "2026-11-23T10:00:03Z"))
	if got := queuedTexts(c); !sameTexts(got, []string{"next"}) {
		t.Errorf("queued in the turn running now: %q", got)
	}
}

// Plain is the text as a list shows it: a ccs message is the user's words.
func TestQueuedMessagePlain(t *testing.T) {
	q := QueuedMessage{Text: "<cross-session-message from-name=\"the user, via ccs\">\n" + CcsNote + "ship it\n</cross-session-message>"}
	if got := q.Plain(); got != "ship it" {
		t.Errorf("ccs message: %q", got)
	}
	if got := (QueuedMessage{Text: "plain words"}).Plain(); got != "plain words" {
		t.Errorf("typed: %q", got)
	}
}

// Taking a message out never writes over the array a copy handed out
// earlier still reads.
func TestQueueRemovalCopies(t *testing.T) {
	var c Conversation
	queueLines(t, &c,
		qop("enqueue", "a", "2026-11-23T10:00:01Z"),
		qop("enqueue", "b", "2026-11-23T10:00:02Z"),
	)
	before := c
	queueLines(t, &c, qop("remove", "a", "2026-11-23T10:00:03Z"))
	if len(before.Queue) != 2 || before.Queue[0].Text != "a" || before.Queue[1].Text != "b" {
		t.Errorf("the earlier copy changed: %+v", before.Queue)
	}
}

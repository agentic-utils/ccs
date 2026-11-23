package transcript

import (
	"slices"
	"strings"
	"time"
)

// Claude Code's message queue, as its transcript records it. A message that
// arrives while a turn runs (typed in its tab, sent from ccs, or from another
// session) is held in a queue and shown under the prompt as "⎿ Queued" until
// Claude takes it in. Each change is a queue-operation line:
//
//   - enqueue, with the message as content: it joined the queue (a message
//     given to an idle session is enqueued and dequeued in the same moment)
//   - remove, with content and a reason: it left the queue, taken in mid-turn
//     (absorbed_mid_turn, then a queued_command attachment with its text),
//     handed to an agent (delivered_to_agent), or dropped
//   - popAll / popOne, with content, once per message: pulled back into the
//     prompt to edit (Up or Esc), so it is no longer queued
//   - dequeue, with no content: taken in at the start of a turn, followed by
//     a user line holding its text
//
// A dequeue names no message, so a message counts as taken in when its text
// shows up in a user line or queued_command attachment, the way it appears
// in the conversation; a remove, popAll or popOne names it by content.
// Messages are matched by queueKey, which looks past the wrapping a
// cross-session message is given when it is taken in (an "Another Claude
// session sent a message:" line, attributes such as hop-chain dropped).
//
// Codex keeps no such record: its rollouts have task_started, task_complete
// and turn_aborted, nothing for a queued message.

// QueuedMessage is a message waiting in the session's queue.
type QueuedMessage struct {
	Text string // cleaned where it enters (noteEnqueued)
	Ts   string
	key  string // queueKey of the raw text, for matching the copy that is taken in; raw, never drawn
}

// Plain is the message as the session will be given it, for a list: a
// message from another session says who sent it, one typed in ccs is the
// user's own words.
func (q QueuedMessage) Plain() string {
	from, body, ok := PeerParts(q.Text)
	switch {
	case !ok:
		return StripImageNote(q.Text)
	case from == CcsSender:
		return body
	}
	return "from " + from + ": " + body
}

// CcsSender is the from-name of a message typed in ccs and sent over a
// session's socket.
const CcsSender = "the user, via ccs"

// maxQueue caps the queue ccs keeps: messages it never sees taken in (a
// session that exited with some queued) would otherwise build up.
const maxQueue = 50

// queueKey is the part of a queued message that survives being taken in:
// a cross-session message's body, whitespace collapsed.
func queueKey(text string) string {
	if m := PeerMessage.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
		text = m[2]
	}
	return strings.Join(strings.Fields(text), " ")
}

// noteEnqueued adds a message that joined the queue. Harness text (task
// notifications, channel events, reminders) is queued too, but Claude Code
// doesn't show it as a queued message, so neither does ccs.
func noteEnqueued(conv *Conversation, text, ts string) {
	if strings.TrimSpace(text) == "" || isChannelEvent(text) {
		return
	}
	if _, _, ok := HarnessNote(text); ok && !PeerMessage.MatchString(text) {
		return
	}
	key := queueKey(text)
	if key == "" {
		return
	}
	q := conv.Queue
	if len(q) >= maxQueue {
		q = slices.Clone(q[1:])
	}
	// Cleaned here rather than by cleanSince: at the cap the queue keeps its
	// length, so a mark by length wouldn't see the new message.
	conv.Queue = append(q, QueuedMessage{Text: CleanText(text), Ts: CleanText(ts), key: key})
}

// noteRemoved drops the queued message a remove, popAll or popOne names by
// its content. One taken in mid-turn (absorbed) is followed by its
// queued_command, so its key waits in queueTaken for that copy, which then
// drops nothing: with the same text queued twice, the second stays queued.
func noteRemoved(conv *Conversation, text string, absorbed bool) {
	key := queueKey(text)
	if i := slices.IndexFunc(conv.Queue, func(q QueuedMessage) bool { return q.key == key }); key != "" && i >= 0 {
		conv.Queue = slices.Delete(slices.Clone(conv.Queue), i, i+1)
		if absorbed {
			taken := conv.queueTaken
			if len(taken) >= maxQueue {
				taken = slices.Clone(taken[1:])
			}
			conv.queueTaken = append(taken, key)
		}
	}
}

// noteUnqueued drops the first queued message text is: a user line or
// queued_command, a message as it was taken into the conversation (after a
// dequeue, which names none), unless it's the copy of one a remove already
// dropped. Always a new slice, never the old one's array, since copies of
// the conversation handed out earlier share it.
func noteUnqueued(conv *Conversation, text string) {
	if len(conv.Queue) == 0 && len(conv.queueTaken) == 0 || strings.TrimSpace(text) == "" {
		return
	}
	key := queueKey(text)
	if i := slices.IndexFunc(conv.queueTaken, func(k string) bool { return k == key }); i >= 0 {
		conv.queueTaken = slices.Delete(slices.Clone(conv.queueTaken), i, i+1)
		return
	}
	if i := slices.IndexFunc(conv.Queue, func(q QueuedMessage) bool { return q.key == key }); i >= 0 {
		conv.Queue = slices.Delete(slices.Clone(conv.Queue), i, i+1)
	}
}

// QueuedNow is what the session's queue holds now: messages enqueued since
// its last turn end. A turn that ends with messages queued starts the next
// at once by taking them in, so any older one still listed is a message ccs
// never saw taken in (a slash command, which shows as its output, or one
// queued when the session exited). Only meaningful while the session is
// running and mid-turn; an idle session's queue is empty.
func (c Conversation) QueuedNow() []QueuedMessage {
	var out []QueuedMessage
	for _, q := range c.Queue {
		t, err := time.Parse(time.RFC3339Nano, q.Ts)
		if err != nil || !t.After(c.turnEnd) {
			continue
		}
		out = append(out, q)
	}
	return out
}

package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// What a session is doing right now, worked out from its transcript and the
// start of the claude process running it: shared by the list, `ccs sessions`
// and the MCP tools.

// RunningTasks is conv's background tasks still going, for a session open
// in a claude process that started at since (zero when unknown): no end
// reported, a monitor not past its expiry, and started by that process,
// since one that exits (or is replaced by a resume) takes its tasks with it
// without reporting their end.
func RunningTasks(conv Conversation, since, now time.Time) []BgTask {
	var out []BgTask
	for _, t := range conv.Tasks {
		started, err := time.Parse(time.RFC3339, t.Started)
		if t.Status == "" && (t.Expires.IsZero() || now.Before(t.Expires)) && (err != nil || !started.Before(since)) {
			out = append(out, t)
		}
	}
	return out
}

// SubagentAlive: not finished, and started by the session's current claude
// process (since). Older Claude Code wrote no finished flag at all, so an
// old subagent without one ended with the process that ran it.
func SubagentAlive(s Subagent, since time.Time) bool {
	return !s.Finished && !s.Started.Before(since)
}

// SubagentRunning: alive and still writing, not an idle teammate.
func SubagentRunning(s Subagent, since, now time.Time) bool {
	return SubagentAlive(s, since) && now.Sub(s.LastWrite) < SubagentIdle
}

// ToolSummary is the part of a tool call's input a person would check first.
func ToolSummary(input json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(input, &in)
	for _, k := range []string{"command", "file_path", "url", "pattern", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			return CleanText(strings.Join(strings.Fields(v), " "))
		}
	}
	return ""
}

// Asks: the text ends in a question. Markdown closing it ("(yes?)", "push?**")
// still asks; a bare "?" doesn't.
func Asks(text string) bool {
	t := strings.TrimRight(strings.TrimSpace(text), ")*_` ")
	return strings.HasSuffix(t, "?") && strings.TrimRight(t, "?") != ""
}

// LastSaid is the index of the last message the session or the user said
// (not one it sent another session, nor an event), -1 if none.
func LastSaid(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "sent" && msgs[i].Role != "channel" {
			return i
		}
	}
	return -1
}

// AskedLast: the last thing said was the session's, and ends in a question:
// an idle session's guess at "asked you something".
func (c Conversation) AskedLast() bool {
	n := LastSaid(c.Messages)
	return n >= 0 && c.Messages[n].Role == "assistant" && Asks(c.Messages[n].Text)
}

// PendingAsk is what a session waiting on the user asks, as plain text: its
// AskUserQuestion menu, options numbered as claude shows them, else the tool
// call a permission prompt is about. "" with no call pending. A busy
// session's pending call is one that's running, so only a waiting session's
// is a question.
func (c Conversation) PendingAsk() string {
	if qs := c.Questions(); len(qs) > 0 {
		var b strings.Builder
		for i, q := range qs {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(CleanText(q.Question))
			if q.MultiSelect {
				b.WriteString(" (pick any)")
			}
			for j, o := range q.Options {
				fmt.Fprintf(&b, "\n  %d. %s", j+1, CleanText(o.Label))
				if o.Description != "" {
					b.WriteString(": " + CleanText(o.Description))
				}
			}
		}
		return b.String()
	}
	if c.Pending == nil {
		return ""
	}
	return strings.TrimSpace("wants to use " + c.Pending.Name + " " + ToolSummary(c.Pending.Input))
}

package transcript

import (
	"testing"
	"time"
)

func TestRunningTasksAndSubagents(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour) // the claude process running the session
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	conv := Conversation{Tasks: []BgTask{
		{ID: "run", Started: at(time.Minute)},
		{ID: "before", Started: at(2 * time.Hour)},                                // an earlier process's: gone with it
		{ID: "ended", Started: at(time.Minute), Status: "failed"},                 // reported its end
		{ID: "expired", Started: at(time.Minute), Expires: now.Add(-time.Second)}, // a monitor past its time
		{ID: "watching", Started: at(time.Minute), Expires: now.Add(time.Minute)},
		{ID: "undated", Started: ""}, // no start to compare: counted
	}}
	var ids []string
	for _, task := range RunningTasks(conv, since, now) {
		ids = append(ids, task.ID)
	}
	if len(ids) != 3 || ids[0] != "run" || ids[1] != "watching" || ids[2] != "undated" {
		t.Errorf("running tasks: %v", ids)
	}

	for _, c := range []struct {
		why            string
		s              Subagent
		alive, running bool
	}{
		{"working", Subagent{Started: now.Add(-time.Minute), LastWrite: now.Add(-time.Second)}, true, true},
		{"quiet for longer than SubagentIdle: an idle teammate", Subagent{Started: now.Add(-30 * time.Minute), LastWrite: now.Add(-SubagentIdle - time.Second)}, true, false},
		{"finished", Subagent{Started: now.Add(-time.Minute), LastWrite: now, Finished: true}, false, false},
		{"an earlier process's", Subagent{Started: now.Add(-2 * time.Hour), LastWrite: now}, false, false},
	} {
		if SubagentAlive(c.s, since) != c.alive || SubagentRunning(c.s, since, now) != c.running {
			t.Errorf("%s: alive %v, running %v", c.why, SubagentAlive(c.s, since), SubagentRunning(c.s, since, now))
		}
	}
}

func TestPendingAskAndAskedLast(t *testing.T) {
	var conv Conversation
	if conv.PendingAsk() != "" || conv.AskedLast() {
		t.Error("nothing pending and nothing said asks nothing")
	}
	ParseLine(&conv, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"rm  -rf\n build"}}]}}`))
	if got := conv.PendingAsk(); got != "wants to use Bash rm -rf build" {
		t.Errorf("a permission prompt's tool call: %q", got)
	}
	conv = Conversation{}
	ParseLine(&conv, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"AskUserQuestion","input":{"questions":[{"question":"Which?","multiSelect":true,"options":[{"label":"A","description":"first"},{"label":"B"}]},{"question":"Sure?","options":[{"label":"Yes"}]}]}}]}}`))
	if got, want := conv.PendingAsk(), "Which? (pick any)\n  1. A: first\n  2. B\nSure?\n  1. Yes"; got != want {
		t.Errorf("a question menu, numbered as claude shows it:\n%q\nwant\n%q", got, want)
	}

	conv = Conversation{Messages: []Message{{Role: "assistant", Text: "Shall I push?**"}, {Role: "sent", Text: "told them"}, {Role: "channel", Text: "an event"}}}
	if !conv.AskedLast() {
		t.Error("a question, then only a sent message and an event: still asked")
	}
	conv.Messages = append(conv.Messages, Message{Role: "user", Text: "yes?"})
	if conv.AskedLast() {
		t.Error("the user spoke last")
	}
	if Asks("?") || Asks("done.") || !Asks("ok (yes?)") {
		t.Error("asks: a bare ? doesn't, closing markdown still does")
	}
}

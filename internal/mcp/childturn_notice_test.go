package mcp

import (
	"testing"
	"time"
)

// envelope is a cross-session message as SendEnvelope writes it.
func envelope(sender, body string) string {
	return "<cross-session-message from-name=\"" + sender + "\">\n[Automatic notice from " + sender + ".]\n\n" + body + "\n</cross-session-message>"
}

func queued(at time.Time, mode, prompt string) map[string]any {
	return map[string]any{"type": "attachment", "timestamp": at.Format(time.RFC3339Nano), "attachment": map[string]any{"type": "queued_command", "commandMode": mode, "prompt": prompt}}
}

func blocksLine(at time.Time, texts ...string) map[string]any {
	var blocks []map[string]any
	for _, s := range texts {
		blocks = append(blocks, say(s))
	}
	return map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339Nano), "cwd": "/orch", "isMeta": true, "message": map[string]any{"role": "user", "content": blocks}}
}

// Every automatic sender, in every form its notice reaches a transcript:
// none of their turns is told, and a close notice's never is.
func TestChildTurnSkipsNoticeTurns(t *testing.T) {
	path, _ := childTurnFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	at := now.Add(-2 * time.Minute)
	const prefix = "Another Claude session sent a message:\n"
	taskNote := "<task-notification>\n<task-id>b1</task-id>\n<status>failed</status>\n<summary>Background command failed</summary>\n</task-notification>"
	head := []any{userLine(at, "earlier ask", false), assistantLine(at.Add(time.Second), say("earlier answer"))}
	step := 0
	check := func(name string, told bool, lines ...any) {
		t.Helper()
		step++
		writeLines(t, path, append(append([]any{}, head...), lines...)...)
		// An hour apart, so the debounce never decides.
		if _, ok := childTurn(stop(path), now.Add(time.Duration(step)*time.Hour)); ok != told {
			t.Errorf("%s: told %v, want %v", name, ok, told)
		}
	}
	done := assistantLine(at.Add(80*time.Second), say("All done."))
	for _, sender := range []string{"ccs close", "ccs watch", "ccs hook", "ccs reorg"} {
		n := envelope(sender, "notice body")
		check(sender+" as the idle prompt", false, userLine(at.Add(time.Minute), prefix+n, true), done)
		check(sender+" in text blocks after a reminder", false, blocksLine(at.Add(time.Minute), "<system-reminder>x</system-reminder>", prefix+n), done)
		check(sender+" queued into an idle turn's first line", false, userLine(at.Add(time.Minute), n, true), done)
		// The ccs-switch-agent case: a background task's notification queued
		// mid-turn used to stand in for the prompt that started the turn.
		check(sender+" then a task notification queued", false,
			userLine(at.Add(time.Minute), prefix+n, true),
			assistantLine(at.Add(65*time.Second), bash("pkill -f x")),
			queued(at.Add(70*time.Second), "task-notification", taskNote),
			done)
		check(sender+" then another notice queued", false,
			userLine(at.Add(time.Minute), prefix+n, true),
			queued(at.Add(70*time.Second), "prompt", envelope("ccs watch", "checks failing")),
			done)
	}

	// The user (or another session) joined the notice's turn: told, except
	// for a close notice, whose session is about to go anyway.
	check("watch nudge, then the user queued", true,
		userLine(at.Add(time.Minute), prefix+envelope("ccs watch", "x"), true),
		queued(at.Add(70*time.Second), "prompt", "<cross-session-message from-name=\"the user, via ccs\">\nand bump the version\n</cross-session-message>"),
		done)
	check("close notice, then the user queued", false,
		userLine(at.Add(time.Minute), prefix+envelope("ccs close", "x"), true),
		queued(at.Add(70*time.Second), "prompt", "also tidy up"),
		done)
	check("the user's turn, then a close notice queued", false,
		userLine(at.Add(time.Minute), "fix the docs", false),
		queued(at.Add(70*time.Second), "prompt", envelope("ccs close", "x")),
		done)

	// Turns nothing automatic started are still told.
	check("the user's turn, then a task notification", true,
		userLine(at.Add(time.Minute), "fix the docs", false),
		queued(at.Add(70*time.Second), "task-notification", taskNote),
		done)
	check("the user's turn, then a nudge queued", true,
		userLine(at.Add(time.Minute), "fix the docs", false),
		queued(at.Add(70*time.Second), "prompt", envelope("ccs watch", "x")),
		done)
	check("a session's message", true, userLine(at.Add(time.Minute), prefix+envelope("boss", "fix the docs"), true), done)
	check("a notice quoted mid-message", true, userLine(at.Add(time.Minute), "why did I get this?\n"+prefix+envelope("ccs close", "x"), false), done)
	// A notice's turn is over; the next one the user starts is told.
	check("the user's turn after a notice's", true,
		userLine(at.Add(time.Minute), prefix+envelope("ccs close", "x"), true),
		assistantLine(at.Add(65*time.Second), say("Stopping.")),
		userLine(at.Add(70*time.Second), "actually, keep going", false),
		done)

}

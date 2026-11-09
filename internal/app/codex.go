package app

import (
	"os"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Codex sessions: ccs launch --codex starts one, ccs claim works inside one
// (Codex gives every command it runs CODEX_THREAD_ID), and ccs watch nudges
// one with `codex queue`, which delivers into a running Codex tab. Codex's
// sandbox lets a command neither run ps nor reach a socket, so a Codex
// claim is keyed by thread id, never by process, and its liveness is worked
// out by ccs watch from outside the sandbox (codexLive).

// codexSession is the Codex session this process runs in, from the
// CODEX_THREAD_ID Codex gives every command; ok false outside Codex.
func codexSession() (prClaim, bool) {
	thread := os.Getenv("CODEX_THREAD_ID")
	if !transcript.CodexThreadID.MatchString(thread) {
		return prClaim{}, false
	}
	c := prClaim{Session: thread, Kind: "codex", Thread: thread}
	if m, ok := transcript.ReadCodexMeta(transcript.CodexRollout(thread)); ok {
		c.Name = transcript.CodexLaunchName(m)
	}
	if c.Name == "" {
		c.Name = sessions.CodexThreadName(thread)
	}
	if c.Name == "" {
		c.Name = thread
	}
	return c, true
}

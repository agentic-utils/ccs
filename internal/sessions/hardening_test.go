package sessions

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
)

// A brief that starts with a dash is still a prompt, never an option, for
// every launch path (it's a positional argument after claude's or codex's
// options).
func TestBriefIsNeverAnOption(t *testing.T) {
	for _, brief := range []string{"--dangerously-skip-permissions", "-p", "- a bullet list"} {
		got := briefText(LaunchOpts{Brief: brief, NoPreamble: true})
		if strings.HasPrefix(got, "-") || strings.TrimSpace(got) != brief {
			t.Errorf("brief %q became %q", brief, got)
		}
	}
	if got := briefText(LaunchOpts{Brief: "ship it", NoPreamble: true}); got != "ship it" {
		t.Errorf("an ordinary brief is left alone: %q", got)
	}
}

// Close and restart given a session id stop nothing once the name belongs
// to another session.
func TestCloseAndRestartCheckTheSessionID(t *testing.T) {
	write := sessionsFixture(t)
	pid := os.Getpid()
	write(strconv.Itoa(pid)+".json", pid, "worker")
	oldTTY, oldSig := proc.TTYOf, proc.SignalPID
	proc.TTYOf = func(int) string { return "/dev/ttys044" }
	proc.SignalPID = func(int, syscall.Signal) error {
		t.Error("signalled a session it was told not to")
		return errors.New("no")
	}
	t.Cleanup(func() { proc.TTYOf, proc.SignalPID = oldTTY, oldSig })

	if err := SameSession("worker", pid, "id-worker-"+strconv.Itoa(pid)); err != nil {
		t.Errorf("its own id: %v", err)
	}
	if _, err := CloseByNameID("worker", "id-old"); err == nil || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Errorf("close of a replaced session: %v", err)
	}
	if _, err := CloseGracefullyNotifyID("worker", "id-old", time.Second, nil); err == nil || !strings.Contains(err.Error(), "isn't session id-old") {
		t.Errorf("graceful close of a replaced session: %v", err)
	}
	if _, err := findRestartTarget("worker", "id-old"); err == nil || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Errorf("restart of a replaced session: %v", err)
	}
}

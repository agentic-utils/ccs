package sessions

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// restartFixture is gracefulFixture's worker with the launch, the caller
// check and the helper stubbed: launches records each resume, and the
// number of stops at the time it ran.
type restartFixture struct {
	*gracefulFixture
	launches  []LaunchOpts
	stopsSeen []int
	launchErr []error // what each launch returns, in turn; nil after
	spawned   []string
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	r := &restartFixture{gracefulFixture: newGracefulFixture(t)}
	oldLaunch, oldCaller, oldSpawn, oldRetry := launch, IsCaller, SpawnRestartHelper, RestartRetry
	t.Cleanup(func() { launch, IsCaller, SpawnRestartHelper, RestartRetry = oldLaunch, oldCaller, oldSpawn, oldRetry })
	RestartRetry = 0
	IsCaller = func(int) bool { return false }
	launch = func(o LaunchOpts) (Launched, error) {
		r.launches = append(r.launches, o)
		r.stopsSeen = append(r.stopsSeen, r.signalled)
		if len(r.launchErr) > 0 {
			err := r.launchErr[0]
			r.launchErr = r.launchErr[1:]
			if err != nil {
				return Launched{}, err
			}
		}
		if o.OnTab != nil {
			o.OnTab("/dev/ttys051")
		}
		return Launched{PID: 4242, TTY: "/dev/ttys051"}, nil
	}
	SpawnRestartHelper = func(name string, pid int, id string, timeout time.Duration) error {
		r.spawned = append(r.spawned, name)
		if pid != r.pid || id != r.id || timeout != time.Minute {
			t.Errorf("helper for pid %d, session %s, %s", pid, id, timeout)
		}
		return nil
	}
	return r
}

func TestRestartStopsAnIdleSessionThenResumesIt(t *testing.T) {
	r := newRestartFixture(t)
	var tab string
	msg, err := RestartSession(RestartOpts{Name: "worker", OnTab: func(tty string) { tab = tty }})
	if err != nil {
		t.Fatal(err)
	}
	if r.signalled != 1 || len(r.tabs) != 1 || r.tabs[0] != "/dev/ttys050" {
		t.Errorf("stopped once and its old tab closed: %d, %v", r.signalled, r.tabs)
	}
	if len(r.launches) != 1 || r.stopsSeen[0] != 1 {
		t.Fatalf("resumed once, after the stop: %d launches, stops seen %v", len(r.launches), r.stopsSeen)
	}
	o := r.launches[0]
	if o.Name != "worker" || o.Resume != r.id || o.Cwd != "/code/app" || !o.NoPreamble {
		t.Errorf("the same name, conversation and folder, through the launcher: %+v", o)
	}
	if !strings.HasPrefix(o.Brief, RestartNote) || !strings.Contains(o.Brief, "Monitors") {
		t.Errorf("its first prompt says it was restarted: %q", o.Brief)
	}
	if tab != "/dev/ttys051" {
		t.Errorf("OnTab hears of the new tab: %q", tab)
	}
	if !strings.Contains(msg, "restarted worker") || !strings.Contains(msg, "pid 4242") {
		t.Errorf("the result: %s", msg)
	}
}

func TestRestartRefusesASessionMidTurn(t *testing.T) {
	for _, status := range []string{"busy", "waiting"} {
		r := newRestartFixture(t)
		r.setStatus(status, time.Now().Add(-3*time.Minute))
		_, err := RestartSession(RestartOpts{Name: "worker"})
		if err == nil || !strings.Contains(err.Error(), "mid-turn") || !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "3m0s") {
			t.Errorf("%s: refused, saying why and how to force it: %v", status, err)
		}
		if r.signalled != 0 || len(r.launches) != 0 {
			t.Errorf("%s: nothing stopped or launched: %d, %d", status, r.signalled, len(r.launches))
		}
		if _, err := RestartSession(RestartOpts{Name: "worker", Force: true}); err != nil {
			t.Errorf("%s: forced: %v", status, err)
		}
		if r.signalled != 1 || len(r.launches) != 1 {
			t.Errorf("%s: forced, it stops and resumes: %d, %d", status, r.signalled, len(r.launches))
		}
	}
}

func TestRestartTriesTheResumeAgainThenSaysHowToBringItBack(t *testing.T) {
	r := newRestartFixture(t)
	r.launchErr = []error{errors.New("iTerm said no")}
	if _, err := RestartSession(RestartOpts{Name: "worker"}); err != nil || len(r.launches) != 2 {
		t.Errorf("a failed resume is tried once more: %v, %d launches", err, len(r.launches))
	}

	r = newRestartFixture(t)
	r.launchErr = []error{errors.New("iTerm said no"), errors.New("still no")}
	_, err := RestartSession(RestartOpts{Name: "worker"})
	if err == nil || !strings.Contains(err.Error(), "ccs launch --name worker --resume "+r.id) {
		t.Errorf("says how to bring it back: %v", err)
	}
}

func TestRestartOfItselfWaitsForTheTurnInAHelper(t *testing.T) {
	r := newRestartFixture(t)
	IsCaller = func(pid int) bool { return pid == r.pid }
	r.setStatus("busy", time.Now()) // running ccs restart is its turn
	msg, err := RestartSession(RestartOpts{Name: "worker", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.spawned) != 1 || r.signalled != 0 || len(r.launches) != 0 {
		t.Errorf("hands it to the helper and stops nothing itself: %v, %d, %d", r.spawned, r.signalled, len(r.launches))
	}
	if !strings.Contains(msg, "once this turn ends") {
		t.Errorf("says when: %s", msg)
	}
}

func TestRestartAfterTurnStopsOnceTheTurnEnds(t *testing.T) {
	r := newRestartFixture(t)
	r.setStatus("busy", time.Now().Add(-time.Second))
	done := make(chan error, 1)
	go func() {
		_, err := RestartAfterTurn("worker", r.pid, r.id, time.Minute)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if r.signalled != 0 {
		t.Fatal("stopped mid-turn")
	}
	r.setStatus("idle", time.Now().Add(time.Second))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never restarted after the turn ended")
	}
	if r.signalled != 1 || len(r.launches) != 1 || r.launches[0].Resume != r.id {
		t.Errorf("stopped, then resumed: %d, %+v", r.signalled, r.launches)
	}
}

func TestRestartAfterTurnStopsAnywayAtTheTimeout(t *testing.T) {
	r := newRestartFixture(t)
	r.setStatus("busy", time.Now())
	msg, err := RestartAfterTurn("worker", r.pid, r.id, 50*time.Millisecond)
	if err != nil || !strings.Contains(msg, "hadn't ended") || r.signalled != 1 || len(r.launches) != 1 {
		t.Errorf("stopped where it was and resumed: %q %v, %d, %d", msg, err, r.signalled, len(r.launches))
	}
}

func TestRestartAfterTurnLeavesAnotherSessionAlone(t *testing.T) {
	r := newRestartFixture(t)
	if _, err := RestartAfterTurn("worker", r.pid, "some-other-id", time.Minute); err == nil || r.signalled != 0 {
		t.Errorf("a different conversation under the name isn't stopped: %v, %d", err, r.signalled)
	}
}

func TestRestartOneAtATime(t *testing.T) {
	r := newRestartFixture(t)
	unlock, err := lockRestart("worker")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := RestartSession(RestartOpts{Name: "worker"}); err == nil || !strings.Contains(err.Error(), "already under way") || r.signalled != 0 {
		t.Errorf("a second restart is refused: %v", err)
	}
}

func TestRunRestartUsage(t *testing.T) {
	if code := RunRestart(nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("no name: %d", code)
	}
	if code := RunRestart([]string{"worker", "--after-turn", "nonsense"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("a bad --after-turn: %d", code)
	}
	var errs strings.Builder
	if code := RunRestart([]string{"nobody"}, io.Discard, &errs); code != 1 || !strings.Contains(errs.String(), "no running session named nobody") {
		t.Errorf("no such session: %d %s", code, errs.String())
	}
}

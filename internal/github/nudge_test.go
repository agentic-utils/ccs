package github

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// nudgeEnv is claimEnv plus a stubbed sendNudge recording what each session got.
func nudgeEnv(t *testing.T) (live map[int]PRClaim, sent *[]string) {
	t.Helper()
	live = claimEnv(t)
	old := SendNudge
	sent = &[]string{}
	SendNudge = func(c PRClaim, text string) error {
		*sent = append(*sent, c.Name+": "+text)
		return nil
	}
	t.Cleanup(func() { SendNudge = old })
	return live, sent
}

func intp(n int) *int { return &n }

// claimAs records a claim on pr by the live session at pid.
func claimAs(t *testing.T, live map[int]PRClaim, pid int, pr string) {
	t.Helper()
	c := live[pid]
	if err := WithClaims(func(f *ClaimsFile) bool { f.Claims[pr] = append(f.Claims[pr], c); return true }); err != nil {
		t.Fatal(err)
	}
}

func newEvents(t *testing.T, from int) []NudgeEvent {
	t.Helper()
	data, _ := os.ReadFile(PREventsPath())
	var out []NudgeEvent
	for _, l := range strings.Split(string(data[from:]), "\n") {
		var e NudgeEvent
		if json.Unmarshal([]byte(l), &e) == nil && strings.HasPrefix(e.Event, "nudge") {
			out = append(out, e)
		}
	}
	return out
}

func eventsLen() int {
	data, _ := os.ReadFile(PREventsPath())
	return len(data)
}

func TestNudgesOncePerHeadAndReason(t *testing.T) {
	live, sent := nudgeEnv(t)
	alpha := os.Getppid()
	claimAs(t, live, alpha, claimA)
	pr := &WatchedPR{URL: claimA, Repo: "o/r", Number: 1, Title: "fix it", State: "open", Head: "h1", Checks: "failing", Failing: []string{"setup", "lint"},
		Threads: intp(2), Unanswered: map[string]int64{"T1": 11, "T2": 21}}
	writeWatchState(t, time.Now(), pr)
	from := eventsLen()
	if err := NudgeTick(time.Now()); err != nil {
		t.Fatal(err)
	}
	want := "alpha: " + claimA + " (fix it) has 2 review threads waiting on a reply; checks failing: setup, lint.\nThis session claimed it with ccs claim. If it's yours, deal with it; if not, run: ccs claim " + claimA + " --release"
	if len(*sent) != 1 || (*sent)[0] != want {
		t.Fatalf("one nudge with every reason:\n got %q\nwant %q", *sent, want)
	}
	if ev := newEvents(t, from); len(ev) != 1 || ev[0].Event != "nudged" || ev[0].Session != "alpha" || strings.Join(ev[0].Reasons, ",") != "threads,checks" {
		t.Errorf("a nudged event: %+v", ev)
	}

	// The same again: nothing. A restart reads what was sent from disk.
	NudgeTick(time.Now())
	if len(*sent) != 1 {
		t.Errorf("the same state is never told twice: %q", *sent)
	}

	// Answered (one thread's last comment is now the operator's, so it left
	// the set) but unresolved: nothing. A new reviewer comment on the other,
	// and conflicts: those only.
	pr.Unanswered = map[string]int64{"T2": 21}
	writeWatchState(t, time.Now(), pr)
	NudgeTick(time.Now())
	if len(*sent) != 1 {
		t.Errorf("answered threads, still unresolved, never nudge: %q", *sent)
	}
	pr.Unanswered, pr.Conflicts = map[string]int64{"T2": 22}, true
	writeWatchState(t, time.Now(), pr)
	NudgeTick(time.Now())
	if len(*sent) != 2 || !strings.Contains((*sent)[1], "has 1 review thread waiting on a reply; merge conflicts with its base.") || strings.Contains((*sent)[1], "checks failing") {
		t.Errorf("a new reviewer comment and conflicts, only those: %q", *sent)
	}

	// A new head that still fails is told again; the same unanswered
	// threads are not (a push answers nothing, but it was told already).
	pr.Head, pr.Conflicts = "h2", false
	writeWatchState(t, time.Now(), pr)
	NudgeTick(time.Now())
	if len(*sent) != 3 || !strings.Contains((*sent)[2], "has checks failing: setup, lint.") || strings.Contains((*sent)[2], "thread") {
		t.Errorf("a new head failing again, threads not repeated: %q", *sent)
	}

	// Merged: its record goes.
	pr.State = "merged"
	writeWatchState(t, time.Now(), pr)
	NudgeTick(time.Now())
	if b, _ := os.ReadFile(NudgesPath()); strings.Contains(string(b), claimA) {
		t.Errorf("a merged PR's record is dropped: %s", b)
	}
}

func TestNudgesOnlyLiveOwners(t *testing.T) {
	live, sent := nudgeEnv(t)
	alpha := os.Getppid()
	live[4242] = PRClaim{Session: "s-beta", Name: "beta", PID: 4242, ProcStart: "Tue Nov  3 08:00:00 2026"}
	claimAs(t, live, alpha, claimA)
	claimAs(t, live, 4242, claimA)
	claimAs(t, live, 4242, claimB)
	gone := live[4242]
	gone.Session, gone.Name = "s-gamma", "gamma"
	if err := WithClaims(func(f *ClaimsFile) bool { f.Claims[claimB] = append(f.Claims[claimB], gone); return true }); err != nil {
		t.Fatal(err)
	}
	writeWatchState(t, time.Now(),
		&WatchedPR{URL: claimA, Title: "a", State: "open", Head: "h", Conflicts: true},
		&WatchedPR{URL: claimB, Title: "b", State: "open", Head: "h", Conflicts: true},
		&WatchedPR{URL: claimC, Title: "unclaimed", State: "open", Head: "h", Conflicts: true},
	)
	NudgeTick(time.Now())
	got := strings.Join(*sent, "\n")
	if strings.Count(got, "alpha: ") != 1 || strings.Count(got, "beta: ") != 2 || strings.Contains(got, "gamma") || strings.Contains(got, "unclaimed") {
		t.Errorf("each live owner of each claimed PR, nobody else:\n%s", got)
	}

	// The owner's process restarted: not live, not told (its claim no longer matches).
	*sent = nil
	delete(live, 4242)
	writeWatchState(t, time.Now(), &WatchedPR{URL: claimB, Title: "b", State: "open", Head: "h2", Conflicts: true})
	NudgeTick(time.Now())
	if len(*sent) != 0 {
		t.Errorf("a session no longer running isn't nudged: %q", *sent)
	}
}

func TestNudgeFailureIsRetried(t *testing.T) {
	live, _ := nudgeEnv(t)
	claimAs(t, live, os.Getppid(), claimA)
	writeWatchState(t, time.Now(), &WatchedPR{URL: claimA, Title: "a", State: "open", Head: "h", Conflicts: true})
	tries := 0
	SendNudge = func(PRClaim, string) error {
		tries++
		if tries == 1 {
			return errors.New("not connected")
		}
		return nil
	}
	from := eventsLen()
	NudgeTick(time.Now())
	NudgeTick(time.Now())
	ev := newEvents(t, from)
	if tries != 2 || len(ev) != 2 || ev[0].Event != "nudge_failed" || ev[0].Error != "not connected" || ev[1].Event != "nudged" {
		t.Errorf("a failed nudge is reported and tried next pass: tries=%d %+v", tries, ev)
	}
}

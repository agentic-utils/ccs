package transcript

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The first load takes what was written since its recent cutoff and what
// runs now, however old; everything else in the window is counted for the
// background scan, and nothing outside the window is either.
func TestPlanFirstLoad(t *testing.T) {
	dir := t.TempDir()
	old := GetProjectsDir
	GetProjectsDir = func() string { return dir }
	t.Cleanup(func() { GetProjectsDir = old })

	now := time.Now()
	write := func(name string, age time.Duration, size int) string {
		p := filepath.Join(dir, "-proj", name+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, make([]byte, size), 0o644)
		at := now.Add(-age)
		os.Chtimes(p, at, at)
		return p
	}
	day := 24 * time.Hour
	recent := write("recent", day, 10)
	liveOld := write("live-old", 30*day, 10)
	write("older", 20*day, 10)
	write("older2", 40*day, 10)
	write("outside", 90*day, 10) // before the window: the scan skips it, so it isn't counted
	write("huge", day, 5000)     // over maxSize: likewise
	hidden := filepath.Join(dir, ".hidden", "h.jsonl")
	os.MkdirAll(filepath.Dir(hidden), 0o755)
	os.WriteFile(hidden, []byte("x"), 0o644)

	plan, err := PlanFirstLoad(now.Add(-60*day), now.Add(-7*day), 1000, nil, map[string]bool{"live-old": true})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(plan.Files)
	if want := []string{liveOld, recent}; !slices.Equal(plan.Files, want) {
		t.Errorf("first: %v, want %v", plan.Files, want)
	}
	if len(plan.Warm) != 2 || plan.Warm[0].Size != 10 {
		t.Errorf("warm: the first part's Claude transcripts with their sizes, got %+v", plan.Warm)
	}
	if plan.Rest != 2 {
		t.Errorf("rest: older and older2, got %d", plan.Rest)
	}

	// A recent cutoff at or before the window's start puts everything first.
	plan, _ = PlanFirstLoad(now.Add(-60*day), now.Add(-60*day), 1000, nil, nil)
	if len(plan.Files) != 4 || plan.Rest != 0 {
		t.Errorf("everything first: %d files, %d rest", len(plan.Files), plan.Rest)
	}

	if _, err := PlanFirstLoad(now, now, 0, nil, nil); err != nil {
		t.Errorf("an existing folder: %v", err)
	}
	GetProjectsDir = func() string { return filepath.Join(dir, "gone") }
	if _, err := PlanFirstLoad(now, now, 0, nil, nil); err == nil {
		t.Error("a missing projects folder is an error, as GetConversations says")
	}
}

package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePS puts a ps on PATH that prints a fixed listing and counts its calls.
func fakePS(t *testing.T) (calls func() int) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho x >> " + count + "\n" +
		"printf '    1     0 ??       /sbin/launchd\\n  42     1 ttys003  codex  resume  abc\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	oldSys := sysListing
	sysListing = func() ([]psRow, error) { return nil, errors.New("off") }
	t.Cleanup(func() { sysListing = oldSys })
	Fresh()
	t.Cleanup(Fresh)
	return func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "x")
	}
}

func TestPsAllAndPsTreeShareOneListing(t *testing.T) {
	calls := fakePS(t)
	all, err := PsAll()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := PsTree()
	if err != nil {
		t.Fatal(err)
	}
	if n := calls(); n != 1 {
		t.Errorf("ps ran %d times, want 1 for both", n)
	}
	if want := "1 ?? /sbin/launchd\n42 ttys003 codex  resume  abc\n"; all != want {
		t.Errorf("PsAll = %q, want %q", all, want)
	}
	if want := "1 0 /sbin/launchd\n42 1 codex  resume  abc\n"; tree != want {
		t.Errorf("PsTree = %q, want %q", tree, want)
	}
}

func TestListingIsListedAgainAfterItsShare(t *testing.T) {
	calls := fakePS(t)
	old := ListingShare
	ListingShare = 0
	defer func() { ListingShare = old }()
	PsAll()
	PsAll()
	if n := calls(); n != 2 {
		t.Errorf("ps ran %d times, want 2 once the share has passed", n)
	}
}

func TestFreshListsAgain(t *testing.T) {
	calls := fakePS(t)
	old := ListingShare
	ListingShare = time.Hour
	defer func() { ListingShare = old }()
	PsTree()
	Fresh()
	PsTree()
	if n := calls(); n != 2 {
		t.Errorf("ps ran %d times, want 2 after Fresh", n)
	}
}

func TestProcessStartTimesMatchPS(t *testing.T) {
	pid := os.Getpid()
	got := ProcessStartTimes(map[string]int{"me": pid, "gone": 1 << 30})
	if _, ok := got[1<<30]; ok {
		t.Error("a pid that doesn't exist has no start time")
	}
	start, ok := got[pid]
	if !ok || time.Since(start) < 0 || time.Since(start) > time.Hour {
		t.Fatalf("this process's start time: %v %v", start, ok)
	}
	if start.Location() != time.UTC || start.Nanosecond() != 0 {
		t.Errorf("to the second in UTC, as ps's lstart is: %v", start)
	}
}

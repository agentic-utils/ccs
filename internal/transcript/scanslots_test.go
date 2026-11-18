package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Big scans share ScanSlots across every ccs process: with every slot
// taken, a big scan waits ScanWait and then says ccs is busy; once one
// frees, it runs. The slot is held until DropParseCache; a warm scan (the list's
// refresh, with its cache) never waits.
func TestBigScansShareTheSlots(t *testing.T) {
	oldDir, oldWait, oldPoll, oldProjects, oldMin := ScanLockDir, ScanWait, scanPoll, GetProjectsDir, scanSlotMin
	t.Cleanup(func() {
		ScanLockDir, ScanWait, scanPoll, GetProjectsDir, scanSlotMin = oldDir, oldWait, oldPoll, oldProjects, oldMin
	})
	scanSlotMin = 0 // the one fixture file counts as a big scan
	locks, projects := t.TempDir(), t.TempDir()
	ScanLockDir = func() string { return locks }
	GetProjectsDir = func() string { return projects }
	ScanWait, scanPoll = 150*time.Millisecond, 10*time.Millisecond
	os.MkdirAll(filepath.Join(projects, "p"), 0o755)
	os.WriteFile(filepath.Join(projects, "p", "aaaa1111-0000-0000-0000-000000000001.jsonl"),
		[]byte(`{"type":"user","timestamp":"2026-11-18T10:00:00Z","cwd":"/r","message":{"role":"user","content":"hello there"}}`+"\n"), 0o644)
	DropParseCache()

	// Every slot held (each its own open file, as another process's would be).
	var others []func()
	for range ScanSlots {
		r, err := acquireScanSlot()
		if err != nil {
			t.Fatalf("a free slot: %v", err)
		}
		others = append(others, r)
	}
	start := time.Now()
	if _, err := GetConversations(time.Time{}, 0, nil); !errors.Is(err, ErrScanBusy) {
		t.Fatalf("all slots taken: busy, got %v", err)
	}
	if waited := time.Since(start); waited < ScanWait {
		t.Errorf("waited %v before giving up, want at least %v", waited, ScanWait)
	}

	others[0]()
	convs, err := GetConversations(time.Time{}, 0, nil)
	if err != nil || len(convs) != 1 {
		t.Fatalf("a slot freed: the scan runs: %d %v", len(convs), err)
	}
	// The parsed transcripts are still in memory, so the slot stays held:
	// with the other one taken too, nothing is free.
	ScanWait = 0
	if _, err := acquireScanSlot(); !errors.Is(err, ErrScanBusy) {
		t.Errorf("the scan's slot is held after it returns: %v", err)
	}
	// A warm scan (the cache holds it) needs no slot of its own.
	if _, err := GetConversations(time.Time{}, 0, nil); err != nil {
		t.Errorf("a warm scan never waits: %v", err)
	}
	// DropParseCache gives it back.
	DropParseCache()
	r, err := acquireScanSlot()
	if err != nil {
		t.Errorf("DropParseCache frees the slot: %v", err)
	} else {
		r()
	}
	others[1]()
	DropParseCache()
}

// Only a scan with more than scanSlotMin files to parse takes a slot.
func TestBigScan(t *testing.T) {
	old := scanSlotMin
	t.Cleanup(func() { scanSlotMin = old })
	scanSlotMin = 2
	DropParseCache()
	if bigScan([]string{"a", "b"}) || !bigScan([]string{"a", "b", "c"}) {
		t.Error("more than scanSlotMin unparsed files is big")
	}
	ParseCacheMu.Lock()
	ParseCache["a"] = parsedFile{}
	ParseCacheMu.Unlock()
	if bigScan([]string{"a", "b", "c"}) {
		t.Error("a parsed file doesn't count")
	}
	DropParseCache()
}

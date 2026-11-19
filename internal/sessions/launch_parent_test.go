package sessions

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
)

// A launch records its parent before the tab opens, so a session slower to
// start than the wait (or a caller that gives up first) still nests.
func TestLaunchRecordsTheParentBeforeTheTabOpens(t *testing.T) {
	write := sessionsFixture(t)
	stubLaunch(t)
	callAs(t, Caller{ID: "orch-id", Name: "orchestrator"}, true)
	LaunchWait = 100 * time.Millisecond
	LauncherRunning = func(string, string) bool { return false }
	var atOpen ParentRec
	OpenLaunchTab = func(string) (string, error) {
		atOpen = LoadParents().Parents["slow"]
		return "/dev/ttys050", nil
	}
	// Nothing registers within the wait: the launch fails, the session
	// comes up later.
	if _, err := LaunchSession(LaunchOpts{Name: "slow", Brief: "x", Cwd: t.TempDir(), RecordParent: true}); err == nil {
		t.Fatal("nothing registered in time: an error")
	}
	if atOpen.Parent != "orchestrator" {
		t.Errorf("recorded before the tab opened: %+v", atOpen)
	}
	if r := LoadParents().Parents["slow"]; r.Parent != "orchestrator" || r.ParentID != "orch-id" || r.ChildID != "" {
		t.Errorf("kept after a slow start, by name: %+v", r)
	}

	// A launch that comes up in time fills in the child's session id.
	OpenLaunchTab = func(string) (string, error) {
		write("1.json", os.Getpid(), "quick")
		return "/dev/ttys051", nil
	}
	if _, err := LaunchSession(LaunchOpts{Name: "quick", Brief: "x", Cwd: t.TempDir(), RecordParent: true}); err != nil {
		t.Fatal(err)
	}
	if r := LoadParents().Parents["quick"]; r.Parent != "orchestrator" || r.ChildID != fmt.Sprintf("id-quick-%d", os.Getpid()) {
		t.Errorf("child id filled once known: %+v", r)
	}
}

// A launch refused before anything opens records nothing.
func TestARefusedLaunchRecordsNoParent(t *testing.T) {
	write := sessionsFixture(t)
	stubLaunch(t)
	callAs(t, Caller{ID: "orch-id", Name: "orchestrator"}, true)
	opened := 0
	OpenLaunchTab = func(string) (string, error) { opened++; return "/dev/ttys052", nil }
	write("1.json", os.Getpid(), "busy")
	cases := []LaunchOpts{
		{Name: "bad name!", Brief: "x", Cwd: t.TempDir(), RecordParent: true},
		{Name: "busy", Brief: "x", Cwd: t.TempDir(), RecordParent: true},                   // already running
		{Name: "gone", Resume: "no-such-session-id", Cwd: t.TempDir(), RecordParent: true}, // nothing to resume
	}
	for _, o := range cases {
		if _, err := LaunchSession(o); err == nil {
			t.Errorf("%s: refused", o.Name)
		}
		if r, ok := LoadParents().Parents[o.Name]; ok {
			t.Errorf("%s: no record for a refused launch: %+v", o.Name, r)
		}
	}
	if opened != 0 {
		t.Errorf("no tab opened: %d", opened)
	}

	// A tab that couldn't open at all drops the record it just made; one
	// that timed out may have opened, so its record stays.
	OpenLaunchTab = func(string) (string, error) { return "", errors.New("Automation denied") }
	LaunchSession(LaunchOpts{Name: "denied", Brief: "x", Cwd: t.TempDir(), RecordParent: true})
	if r, ok := LoadParents().Parents["denied"]; ok {
		t.Errorf("no tab, no record: %+v", r)
	}
	OpenLaunchTab = func(string) (string, error) { return "", fmt.Errorf("osascript: %w", proc.ErrTimedOut) }
	LaunchSession(LaunchOpts{Name: "maybe", Brief: "x", Cwd: t.TempDir(), RecordParent: true})
	if r := LoadParents().Parents["maybe"]; r.Parent != "orchestrator" {
		t.Errorf("a tab that may have opened keeps its record: %+v", r)
	}
}

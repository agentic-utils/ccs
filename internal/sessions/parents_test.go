package sessions

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// callAs makes CallingSession answer c for the rest of the test.
func callAs(t *testing.T, c Caller, ok bool) {
	t.Helper()
	old := CallingSession
	CallingSession = func() (Caller, bool) { return c, ok }
	t.Cleanup(func() { CallingSession = old })
	os.Remove(ParentsPath())
	t.Cleanup(func() { os.Remove(ParentsPath()) })
}

func TestRecordLaunchParent(t *testing.T) {
	callAs(t, Caller{ID: "orch-id", Name: "orchestrator"}, true)
	recordLaunchParent("worker", 0)
	r, ok := LoadParents().Parents["worker"]
	if !ok || r.Parent != "orchestrator" || r.ParentID != "orch-id" || r.ChildID != "" || r.At.IsZero() {
		t.Fatalf("recorded by name and id: %+v", r)
	}

	callAs(t, Caller{}, false)
	recordLaunchParent("other", 0)
	if _, ok := LoadParents().Parents["other"]; ok {
		t.Error("launched from outside any session (^X N, a shell): no parent")
	}

	callAs(t, Caller{ID: "x", Name: "same"}, true)
	recordLaunchParent("same", 0)
	if _, ok := LoadParents().Parents["same"]; ok {
		t.Error("never its own parent")
	}
}

func TestAdopt(t *testing.T) {
	callAs(t, Caller{ID: "a-id", Name: "alpha"}, true)
	recordLaunchParent("worker", 0)

	callAs(t, Caller{ID: "b-id", Name: "beta"}, true)
	setParent("worker", "", Caller{ID: "a-id", Name: "alpha"})
	if msg, err := Adopt("worker", ""); err != nil || !strings.Contains(msg, "under beta") {
		t.Fatalf("the caller takes it over: %q %v", msg, err)
	}
	if r := LoadParents().Parents["worker"]; r.Parent != "beta" || r.ParentID != "b-id" {
		t.Errorf("switched parents: %+v", r)
	}

	setParent("beta", "b-id", Caller{ID: "w-id", Name: "worker"})
	if _, err := Adopt("worker", ""); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Errorf("beta is under worker: adopting worker would loop, got %v", err)
	}

	if msg, err := Adopt("beta", "none"); err != nil || !strings.Contains(msg, "no parent now") {
		t.Errorf("detach: %q %v", msg, err)
	}
	if _, ok := LoadParents().Parents["beta"]; ok {
		t.Error("detached: its record is gone")
	}
	if _, err := Adopt("worker", "nobody-runs-this"); err == nil {
		t.Error("a parent that isn't running is refused")
	}
	if _, err := Adopt("bad name", ""); err == nil {
		t.Error("a bad name is refused")
	}

	callAs(t, Caller{}, false)
	if _, err := Adopt("worker", ""); err == nil {
		t.Error("no calling session and no --to: refused")
	}
}

func TestRunAdoptUsage(t *testing.T) {
	var out, errs bytes.Buffer
	if code := RunAdopt(nil, &out, &errs); code != 2 || !strings.Contains(errs.String(), "usage") {
		t.Errorf("%d %q", code, errs.String())
	}
	callAs(t, Caller{ID: "c", Name: "orch"}, true)
	out.Reset()
	if code := RunAdopt([]string{"kid", "--to", "none"}, &out, &errs); code != 0 || !strings.Contains(out.String(), "had no parent") {
		t.Errorf("%d %q", code, out.String())
	}
}

func TestResolveParents(t *testing.T) {
	convs := []transcript.Conversation{
		{SessionID: "c1", Title: "orchestrator", LastTimestamp: "2026-11-01T10:00:00Z"},
		{SessionID: "c2", Title: "orchestrator", LastTimestamp: "2026-11-02T10:00:00Z"}, // after a /clear
		{SessionID: "w1", Title: "worker", LastTimestamp: "2026-11-02T09:00:00Z"},
		{SessionID: "u1", LastTimestamp: "2026-11-02T09:00:00Z"}, // unnamed: by id only
		{SessionID: "x1", Title: "x", LastTimestamp: "2026-11-02T09:00:00Z"},
		{SessionID: "y1", Title: "y", LastTimestamp: "2026-11-02T09:00:00Z"},
	}
	f := ParentsFile{Parents: map[string]ParentRec{
		"worker":  {Child: "worker", Parent: "orchestrator", ParentID: "c1"},
		"unnamed": {Child: "unnamed", ChildID: "u1", Parent: "worker", ParentID: "w1"},
		"gone":    {Child: "gone", Parent: "orchestrator", ParentID: "c1"},
		"x":       {Child: "x", Parent: "y"},
		"y":       {Child: "y", Parent: "x"},
	}}
	got := ResolveParents(convs, f)
	if got["w1"] != "c2" {
		t.Errorf("the parent is the newest session with its name, after a /clear: %v", got)
	}
	if got["u1"] != "w1" {
		t.Errorf("a child with no name is found by id: %v", got)
	}
	if got["x1"] != "" && got["y1"] != "" {
		t.Errorf("a loop is broken: %v", got)
	}
	if len(got) != 3 {
		t.Errorf("a child not listed is left out: %v", got)
	}
}

// Undoing a reorg puts back only the records it wrote that nothing changed
// since, marks its backup undone so the next undo reaches the one before,
// and backs nothing up itself.
func TestUndoReorg(t *testing.T) {
	os.Remove(ParentsPath())
	t.Cleanup(func() {
		os.Remove(ParentsPath())
		for _, b := range ParentBackups() {
			os.Remove(b)
		}
		undone, _ := filepath.Glob(filepath.Join(filepath.Dir(ParentsPath()), "launch-parents-reorg-*.undone"))
		for _, u := range undone {
			os.Remove(u)
		}
	})
	SetParents([]ParentRec{{Child: "w", Parent: "hub", ParentID: "hub-id"}, {Child: "v", Parent: "hub"}})
	at := time.Now()
	backup, saved, err := BackupParents(at)
	if err != nil || saved["w"].ParentID != "hub-id" {
		t.Fatal(err, saved)
	}
	SetParentsAt([]ParentRec{{Child: "w", Parent: "orch", Source: "reorg"}, {Child: "v", Parent: "orch", Source: "reorg"}, {Child: "x", Parent: "orch", Source: "reorg"}}, at)
	SetParents([]ParentRec{{Child: "v", Parent: "elsewhere"}, {Child: "y", Parent: "hub"}}) // an adopt and a launch since
	b, _ := ReadReorgBackup(backup)
	if n := UndoCount(b); n != 2 {
		t.Errorf("w and x are still the reorg's: %d", n)
	}
	if n, err := UndoReorg(backup); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	f := LoadParents().Parents
	if f["w"].Parent != "hub" || f["w"].ParentID != "hub-id" || f["x"].Parent != "" || f["v"].Parent != "elsewhere" || f["y"].Parent != "hub" {
		t.Errorf("w put back whole, x dropped, v and y kept: %+v", f)
	}
	if len(ParentBackups()) != 0 {
		t.Errorf("the backup is marked undone and nothing new was backed up: %v", ParentBackups())
	}
	for range parentBackupKeep + 5 {
		BackupParents(time.Now())
		time.Sleep(time.Millisecond)
	}
	if n := len(ParentBackups()); n != parentBackupKeep {
		t.Errorf("keeps the newest %d: %d", parentBackupKeep, n)
	}
}

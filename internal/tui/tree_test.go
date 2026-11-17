package tui

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// treeModel lists two coordinators' trees and one unrelated session:
//
//	hub ─ worker (waiting) ─ sub ─ leaf (stalled)
//	    └ fresh (done)
//	ops ─ deploy
//	loner (no parent)
func treeModel(t *testing.T) model {
	t.Helper()
	os.Remove(sessions.ParentsPath())
	t.Cleanup(func() { os.Remove(sessions.ParentsPath()) })
	err := sessions.WithParents(func(f *sessions.ParentsFile) bool {
		for child, parent := range map[string]string{"worker": "hub", "fresh": "hub", "sub": "worker", "leaf": "sub", "deploy": "ops"} {
			f.Parents[child] = sessions.ParentRec{Child: child, Parent: parent}
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	conv := func(id string, ago time.Duration) transcript.Conversation {
		return transcript.Conversation{SessionID: id + "-id", Title: id, LastTimestamp: at(ago), Messages: []transcript.Message{{Role: "user", Text: "x"}}}
	}
	convs := []transcript.Conversation{
		conv("hub", time.Minute), conv("worker", time.Minute), conv("sub", time.Minute), conv("leaf", time.Hour),
		conv("fresh", time.Minute), conv("ops", 2*time.Minute), conv("deploy", 2*time.Minute), conv("loner", time.Minute),
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 140, 40
	m.marks = &sessionMarks{Since: now.Add(-30 * time.Minute), Seen: map[string]time.Time{"leaf-id": now, "hub-id": now, "worker-id": now, "sub-id": now, "ops-id": now, "deploy-id": now, "loner-id": now}}
	m.live = map[string]bool{}
	m.chatStatus = map[string]sessions.SessionStat{}
	for _, c := range convs {
		m.live[c.SessionID] = true
		m.chatStatus[c.SessionID] = sessions.SessionStat{Status: "idle"}
	}
	m.chatStatus["worker-id"] = sessions.SessionStat{Status: "waiting"}
	m.launchLoaded = false
	m.refilter()
	return m
}

func order(m model) []string {
	var out []string
	for _, it := range m.filtered {
		out = append(out, strings.Repeat(" ", it.depth)+it.conv.Title)
	}
	return out
}

func TestCoordinatorTrees(t *testing.T) {
	m := treeModel(t)
	got := strings.Join(order(m), ",")
	if !strings.Contains(got, "hub, worker,  sub,   leaf, fresh") && !strings.Contains(got, "hub, fresh, worker,  sub,   leaf") {
		t.Errorf("hub's tree, nested at every depth: %s", got)
	}
	if !strings.Contains(got, "ops, deploy") {
		t.Errorf("a second coordinator with its own tree: %s", got)
	}
	if !strings.HasSuffix(got, "loner") {
		t.Errorf("a session nobody launched stays in its ordinary section, below: %s", got)
	}
	for _, it := range m.filtered {
		if want := it.conv.Title != "loner"; (sectionOf(it.familyState) == coordinatorSec) != want {
			t.Errorf("%s in the coordinator section: %v", it.conv.Title, !want)
		}
	}
	hub := m.filtered[0]
	if hub.conv.Title != "hub" && m.filtered[0].conv.Title != "ops" {
		t.Fatalf("a coordinator first: %s", got)
	}
	for _, it := range m.filtered {
		switch it.conv.Title {
		case "hub":
			if it.kids != 4 || it.roll != (rollup{waiting: 1, stalled: 1, done: 1}) {
				t.Errorf("hub's counts: %d %+v", it.kids, it.roll)
			}
		case "worker":
			if it.kids != 2 || it.roll != (rollup{stalled: 1}) {
				t.Errorf("a sub-coordinator counts its own tree: %d %+v", it.kids, it.roll)
			}
		case "loner":
			if it.kids != 0 || rollupCell(it.roll, false) != "" {
				t.Errorf("no tree, no counts: %+v", it.roll)
			}
		}
	}
	v := strip2(m.View())
	if !strings.Contains(v, "coordinators 2") {
		t.Errorf("two trees: the header counts them\n%s", v)
	}
	if !strings.Contains(v, "▾ hub") || strings.Contains(v, "●1 ◦1 ✓1") {
		t.Errorf("unfolded, the coordinator's row has its fold mark and no counts (its sessions show their own)\n%s", v)
	}
}

// Every name at one depth starts in one column: a child's connector sits
// under its parent's name, a sub-coordinator's ▾ takes its connector's
// place, │ runs on past a sub-tree while siblings follow, and a session in a
// tree takes one row while the coordinator's grey row sits under its name.
func TestTreeAlignment(t *testing.T) {
	m := treeModel(t)
	m.width, m.height = 110, 40
	rows := map[string]string{}
	var lines []string
	view := strings.Split(strip2(m.View()), "\n")
	div := -1 // the sidebar's divider column, from the ┬ under the search
	for _, l := range view {
		if i := slices.Index([]rune(l), '┬'); i >= 0 {
			div = i
			break
		}
	}
	for _, l := range view {
		r := []rune(l)
		left := string(r[:min(div, len(r))])
		lines = append(lines, left)
		for _, name := range []string{"hub", "worker", "sub", "leaf", "fresh", "ops", "deploy"} {
			if strings.HasSuffix(strings.TrimRight(left, " ?✗◦●✓0123456789"), " "+name) {
				rows[name] = strings.TrimRight(left, " ")
			}
		}
	}
	col := func(name string) int {
		r := []rune(rows[name])
		for i := range r {
			if strings.HasPrefix(string(r[i:]), " "+name) {
				return i + 1
			}
		}
		return -1
	}
	want := map[string]string{
		"hub": "▾ hub", "worker": "  ▾ worker", "sub": "  │ ▾ sub", "leaf": "  │   └ leaf",
		"fresh": "  └ fresh", "ops": "▾ ops", "deploy": "  └ deploy",
	}
	for name, w := range want {
		if !strings.Contains(rows[name], w) {
			t.Errorf("%s: want %q in %q", name, w, rows[name])
		}
	}
	if col("worker") != col("fresh") || col("worker") != col("hub")+2 || col("sub") != col("worker")+2 || col("deploy") != col("worker") {
		t.Errorf("names at one depth share a column, two cells in per level:\n%s", strings.Join(lines, "\n"))
	}
	for i, l := range lines {
		if strings.Contains(l, " worker") && i+1 < len(lines) && !strings.Contains(lines[i+1], " sub") {
			t.Errorf("a session in a tree takes one row: %q then %q", l, lines[i+1])
		}
		if strings.HasSuffix(strings.TrimRight(l, " "), "▾ hub") {
			if grey := []rune(lines[i+1]); slices.Index(grey, '1') != col("hub") {
				t.Errorf("the coordinator's grey row starts under its name: %q / %q", l, grey)
			}
		}
	}
	if !strings.Contains(rows["worker"], "?") {
		t.Errorf("a child waiting on you shows ?: %q", rows["worker"])
	}
}

func TestFoldCoordinatorTree(t *testing.T) {
	m := treeModel(t)
	for i, it := range m.filtered {
		if it.conv.Title == "hub" {
			m.cursor = i
		}
	}
	m, _ = chord(m, "b")
	got := strings.Join(order(m), ",")
	if strings.Contains(got, "worker") || strings.Contains(got, "leaf") {
		t.Fatalf("^X B folds hub's tree to its row: %s", got)
	}
	if m.filtered[m.cursor].conv.Title != "hub" {
		t.Errorf("the cursor stays on hub")
	}
	hub := m.filtered[m.cursor]
	if hub.hiddenKids != 4 || hub.roll != (rollup{waiting: 1, stalled: 1, done: 1}) {
		t.Errorf("folded, its counts stay: %d %+v", hub.hiddenKids, hub.roll)
	}
	if v := strip2(m.View()); !strings.Contains(v, "▸4 hub") || !strings.Contains(v, "●1 ◦1 ✓1") {
		t.Errorf("▸ with how many it hides\n%s", v)
	}
	if !strings.Contains(strings.Join(order(m), ","), "deploy") {
		t.Error("ops's tree isn't folded")
	}
	m.sortList()
	m.sortList()
	if hub := m.filtered[m.cursor]; hub.roll.waiting != 1 {
		t.Errorf("sorting again keeps counting what the fold hides: %+v", hub.roll)
	}
	if !slices.Contains(loadMarks().Folded, "hub-id") {
		t.Error("the fold is saved")
	}

	m.textInput.SetValue("leaf")
	m.updateFilter()
	if !strings.Contains(strings.Join(order(m), ","), "leaf") {
		t.Errorf("a search shows a folded match: %v", order(m))
	}
	m.textInput.SetValue("")
	m.updateFilter()
	for i, it := range m.filtered {
		if it.conv.Title == "hub" {
			m.cursor = i
		}
	}
	m, _ = chord(m, "b")
	if !strings.Contains(strings.Join(order(m), ","), "leaf") {
		t.Errorf("^X B again unfolds: %v", order(m))
	}

	for i, it := range m.filtered {
		if it.conv.Title == "loner" {
			m.cursor = i
		}
	}
	m, _ = chord(m, "b")
	if !strings.Contains(m.errorMsg, "nothing launched") {
		t.Errorf("nothing to fold: %q", m.errorMsg)
	}
}

func TestTreeFollowsAdoption(t *testing.T) {
	m := treeModel(t)
	sessions.WithParents(func(f *sessions.ParentsFile) bool {
		f.Parents["deploy"] = sessions.ParentRec{Child: "deploy", Parent: "hub"}
		return true
	})
	os.Chtimes(sessions.ParentsPath(), time.Now().Add(time.Second), time.Now().Add(time.Second))
	m.reloadParents()
	m.sortList()
	got := strings.Join(order(m), ",")
	if strings.Contains(got, "ops, deploy") || !strings.Contains(got, " deploy") {
		t.Errorf("deploy moved under hub: %s", got)
	}
	for _, it := range m.filtered {
		if it.conv.Title == "ops" && sectionOf(it.familyState) == coordinatorSec {
			t.Errorf("ops launched nothing now: an ordinary session again")
		}
	}
}

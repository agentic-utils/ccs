package tui

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// treeModel lists two orchestrators' trees and one unrelated session:
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
		conv("fresh", 90*time.Second), conv("ops", 2*time.Minute), conv("deploy", 2*time.Minute), conv("loner", time.Minute),
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

func TestOrchestratorTrees(t *testing.T) {
	m := treeModel(t)
	got := strings.Join(order(m), ",")
	if !strings.Contains(got, "hub, worker,  sub,   leaf, fresh") && !strings.Contains(got, "hub, fresh, worker,  sub,   leaf") {
		t.Errorf("hub's tree, nested at every depth: %s", got)
	}
	if !strings.Contains(got, "ops, deploy") {
		t.Errorf("a second orchestrator with its own tree: %s", got)
	}
	if !strings.HasSuffix(got, "loner") {
		t.Errorf("a session nobody launched stays in its ordinary section, below: %s", got)
	}
	for _, it := range m.filtered {
		if want := it.conv.Title != "loner"; (sectionOf(it.familyState) == orchestratorSec) != want {
			t.Errorf("%s in the orchestrator section: %v", it.conv.Title, !want)
		}
	}
	hub := m.filtered[0]
	if hub.conv.Title != "hub" && m.filtered[0].conv.Title != "ops" {
		t.Fatalf("an orchestrator first: %s", got)
	}
	for _, it := range m.filtered {
		switch it.conv.Title {
		case "hub":
			if it.kids != 4 {
				t.Errorf("hub's tree: %d", it.kids)
			}
		case "worker":
			if it.kids != 2 {
				t.Errorf("a sub-orchestrator counts its own tree: %d", it.kids)
			}
		case "loner":
			if it.kids != 0 {
				t.Errorf("no tree: %d", it.kids)
			}
		}
	}
	v := strip2(m.View())
	if !strings.Contains(v, "orchestrators 2") {
		t.Errorf("two trees: the header counts them\n%s", v)
	}
	if !strings.Contains(v, "✻ hub") || strings.Contains(v, "●1 ◦1 ✓1") {
		t.Errorf("unfolded, the orchestrator's row has its fold mark and no counts (its sessions show their own)\n%s", v)
	}
}

// Rows in a tree have the shape of every other row, two lines each: a
// child's connector sits under its parent's agent glyph, its own glyph and
// name nest after it, │ runs on through the grey lines past a sub-tree
// while siblings follow and from a parent down to its first child, and
// every grey line starts under its name.
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
		"hub": "   ✻ hub", "worker": "   ├ ✻ worker", "sub": "   │ └ ✻ sub", "leaf": "   │   └ ✻ leaf",
		"fresh": "   └ ✻ fresh", "ops": "   ✻ ops", "deploy": "   └ ✻ deploy",
	}
	for name, w := range want {
		if !strings.Contains(rows[name], w) {
			t.Errorf("%s: want %q in %q", name, w, rows[name])
		}
	}
	if col("worker") != col("fresh") || col("worker") != col("hub")+2 || col("sub") != col("worker")+2 || col("deploy") != col("worker") {
		t.Errorf("names at one depth share a column, two cells in per level:\n%s", strings.Join(lines, "\n"))
	}
	greys := map[string]string{ // each row's grey line: the tree's lines, then its age under its name
		"hub": "   │ 1m", "worker": "   │ │ 1m", "sub": "   │   │ 1m", "leaf": "   │       1h", "fresh": "       1m",
	}
	for i, l := range lines {
		for name, g := range greys {
			if strings.TrimRight(l, " ") == rows[name] && i+1 < len(lines) {
				if grey := strings.TrimRight(lines[i+1], " ·"); !strings.HasPrefix(grey, g) || len([]rune(g))-2 != col(name) {
					t.Errorf("%s's grey line: want %q under its name (column %d), got %q", name, g, col(name), lines[i+1])
				}
			}
		}
	}
	if strings.Contains(strings.Join(lines, "\n"), "▾") {
		t.Error("no ▾ on an unfolded row")
	}
}

// The old global view (tree_view, one setting for every tree) isn't read:
// every fold starts closed, and the next save drops the key.
func TestOldGlobalViewIsDropped(t *testing.T) {
	treeModel(t)
	os.WriteFile(state.SettingsPath, []byte(`{"tree_view":1}`), 0o600)
	if s := loadSettings(); len(s.OpenClosed) != 0 {
		t.Errorf("no fold opened from the old view: %v", s.OpenClosed)
	}
	saveSettings(loadSettings())
	if b, _ := os.ReadFile(state.SettingsPath); strings.Contains(string(b), "tree_view") {
		t.Errorf("the old key goes on the next save: %s", b)
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
		if it.conv.Title == "ops" && sectionOf(it.familyState) == orchestratorSec {
			t.Errorf("ops launched nothing now: an ordinary session again")
		}
	}
}

// A launched session whose tree has nothing running waits behind its
// parent's "▸ N closed" line, at the end of the parent's branch, so it isn't
// one click from being resumed. ^X C opens only the fold the selection sits
// in, beside their live siblings under a ▾ line, and again hides them. Both
// lines name ^X C. A search shows everything.
func TestClosedSessionsBehindALine(t *testing.T) {
	m := treeModel(t)
	m.width, m.height = 110, 40
	delete(m.live, "leaf-id")
	delete(m.live, "fresh-id")
	ageItem(&m, "fresh-id", time.Hour) // past the cooldown
	m.refilter()
	titles := func() string { return strings.Join(order(m), ",") }
	view := func() string { return strip2(m.View()) }
	if got := titles(); strings.Contains(got, "leaf") || strings.Contains(got, "fresh") {
		t.Fatalf("closed sessions are hidden: %s", got)
	}
	if v := view(); !strings.Contains(v, "│   └ ▸ 1 closed · ^X C") || !strings.Contains(v, " └ ▸ 1 closed · ^X C") || !strings.Contains(v, "├ ✻ worker") {
		t.Errorf("each parent's closed line ends its branch, names ^X C, its last live child a ├:\n%s", v)
	}
	selectTitle(&m, "hub")
	m, _ = chord(m, "c")
	if got := titles(); !strings.Contains(got, "fresh") || strings.Contains(got, "leaf") {
		t.Errorf("^X C on hub shows hub's closed sessions only: %s", got)
	}
	depth := map[string]int{}
	for _, it := range m.filtered {
		depth[it.conv.Title] = it.depth
	}
	if depth["fresh"] != depth["worker"] {
		t.Errorf("closed sessions sit at their live siblings' level: %v", depth)
	}
	if v := view(); !strings.Contains(v, " ├ ▾ 1 closed · ^X C") || !strings.Contains(v, " └ ✻ fresh") || !strings.Contains(v, "│   └ ▸ 1 closed") {
		t.Errorf("shown, a ▾ line still names ^X C, above them at their level; sub's fold stays closed:\n%s", v)
	}
	if !slices.Equal(loadSettings().OpenClosed, []string{"hub-id"}) {
		t.Errorf("the open fold is saved: %v", loadSettings().OpenClosed)
	}
	if !strings.Contains(m.errorMsg, "under hub shown") {
		t.Errorf("the note names the session: %q", m.errorMsg)
	}
	m, _ = chord(m, "c")
	if got := titles(); !strings.Contains(got, "worker") || strings.Contains(got, "fresh") || !strings.Contains(view(), "▸ 1 closed · ^X C") {
		t.Errorf("^X C again hides them: %s", got)
	}
	if len(loadSettings().OpenClosed) != 0 || !strings.Contains(m.errorMsg, "under hub hidden; ^X C shows them") {
		t.Errorf("hiding is saved and the note says how to show them: %q", m.errorMsg)
	}
	m.textInput.SetValue("fresh")
	m.updateFilter()
	if got := titles(); !strings.Contains(got, "fresh") || strings.Contains(view(), "1 closed") {
		t.Errorf("a search shows a closed match where it is, no closed lines: %s", got)
	}
}

// selectTitle puts the cursor on the listed session with that title.
func selectTitle(m *model, title string) {
	for i, it := range m.filtered {
		if it.conv.Title == title {
			m.cursor = i
			return
		}
	}
}

// closedFixture is treeModel with closed sessions under two orchestrators
// and two levels: fresh under hub, leaf under sub (under worker, under hub),
// and a closed rollout under ops.
func closedFixture(t *testing.T) model {
	t.Helper()
	m := treeModel(t)
	sessions.WithParents(func(f *sessions.ParentsFile) bool {
		f.Parents["rollout"] = sessions.ParentRec{Child: "rollout", Parent: "ops"}
		return true
	})
	os.Chtimes(sessions.ParentsPath(), time.Now().Add(time.Second), time.Now().Add(time.Second))
	m.items = append(m.items, buildItems([]transcript.Conversation{{SessionID: "rollout-id", Title: "rollout", LastTimestamp: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), Messages: []transcript.Message{{Role: "user", Text: "x"}}}})...)
	m.width, m.height = 110, 40
	delete(m.live, "leaf-id")
	delete(m.live, "fresh-id")
	ageItem(&m, "fresh-id", time.Hour)
	ageItem(&m, "leaf-id", time.Hour)
	m.reloadParents()
	m.refilter()
	return m
}

func shownTitles(m model) map[string]bool {
	out := map[string]bool{}
	for _, it := range m.filtered {
		out[it.conv.Title] = true
	}
	return out
}

// ^X C opens one fold only: another orchestrator's stays closed, and so does
// a nested one; a child selected opens its parent's fold; the selection
// stays where it was; the open folds come back after a reload.
func TestClosedFoldsArePerSession(t *testing.T) {
	m := closedFixture(t)
	if got := shownTitles(m); got["fresh"] || got["leaf"] || got["rollout"] {
		t.Fatalf("every fold starts closed: %v", order(m))
	}
	selectTitle(&m, "deploy") // a child: its parent ops's fold
	m, _ = chord(m, "c")
	got := shownTitles(m)
	if !got["rollout"] || got["fresh"] || got["leaf"] {
		t.Errorf("a child selected opens its parent's fold only: %v", order(m))
	}
	if m.filtered[m.cursor].conv.Title != "deploy" {
		t.Errorf("the selection stays on deploy: %s", m.filtered[m.cursor].conv.Title)
	}
	selectTitle(&m, "worker") // no closed sessions of its own: hub's fold
	m, _ = chord(m, "c")
	if got := shownTitles(m); !got["fresh"] || got["leaf"] || !got["rollout"] {
		t.Errorf("hub's fold opens, sub's nested one doesn't, ops's stays open: %v", order(m))
	}
	selectTitle(&m, "sub")
	m, _ = chord(m, "c")
	if got := shownTitles(m); !got["leaf"] || !got["fresh"] {
		t.Errorf("sub's own fold opens: %v", order(m))
	}
	selectTitle(&m, "hub")
	m, _ = chord(m, "c")
	if got := shownTitles(m); got["fresh"] || !got["leaf"] || !got["rollout"] {
		t.Errorf("closing hub's fold leaves sub's and ops's as they were: %v", order(m))
	}

	reloaded := closedFixture(t) // a new run reads the saved folds
	for _, id := range loadSettings().OpenClosed {
		if reloaded.openClosed == nil {
			reloaded.openClosed = map[string]bool{}
		}
		reloaded.openClosed[id] = true
	}
	reloaded.refilter()
	if !slices.Equal(loadSettings().OpenClosed, []string{"ops-id", "sub-id"}) {
		t.Errorf("the open folds are saved: %v", loadSettings().OpenClosed)
	}
	if got := shownTitles(reloaded); !got["leaf"] || !got["rollout"] || got["fresh"] {
		t.Errorf("they come back after a reload: %v", order(reloaded))
	}
}

// Closing the fold the selected closed session sits in leaves the selection
// on the fold's owner, not on whatever slid into its place.
func TestClosingAFoldKeepsTheSelectionNear(t *testing.T) {
	m := closedFixture(t)
	selectTitle(&m, "ops")
	m, _ = chord(m, "c")
	selectTitle(&m, "rollout")
	m, _ = chord(m, "c") // rollout has no fold of its own: ops's closes
	if got := shownTitles(m); got["rollout"] {
		t.Fatalf("ops's fold closed: %v", order(m))
	}
	if m.filtered[m.cursor].conv.Title != "ops" {
		t.Errorf("the selection moves to the fold's owner: %s", m.filtered[m.cursor].conv.Title)
	}
	var help string
	for _, sc := range m.shortcuts() {
		if sc[0] == "^X C" {
			help = sc[1]
		}
	}
	if help != "show the closed sessions under ops" {
		t.Errorf("? help names the fold the key would open: %q", help)
	}
	selectTitle(&m, "loner")
	m, _ = chord(m, "c")
	if !strings.Contains(m.errorMsg, "no closed sessions under this one") {
		t.Errorf("a session with no fold above it says so: %q", m.errorMsg)
	}
}

// A click on a "▸ N closed" line opens that fold, as ^X C does.
func TestClickOnAClosedLineTogglesIt(t *testing.T) {
	m := closedFixture(t)
	selectTitle(&m, "loner")
	rows := m.listRows()
	listTop, listHeight, _ := m.listLayout()
	start := m.listStart(rows, listHeight)
	r := slices.IndexFunc(rows, func(row listRow) bool { return row.fold == "ops-id" })
	if r < start {
		t.Fatalf("ops's closed line is on screen: %d", r)
	}
	m = m.handleMouse(tea.MouseMsg{X: 4, Y: listTop + r - start, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if got := shownTitles(m); !got["rollout"] || got["fresh"] {
		t.Errorf("the click opens ops's fold only: %v", order(m))
	}
	if m.filtered[m.cursor].conv.Title != "loner" {
		t.Errorf("the click leaves the selection alone: %s", m.filtered[m.cursor].conv.Title)
	}
}

// Codex subagents of a headless codex exec (hidden) nest under the nearest
// session that's shown: the Claude session that ran the exec.
func TestSpawnedNestUnderNearestShownAncestor(t *testing.T) {
	os.Remove(sessions.ParentsPath())
	now := time.Now().UTC().Format(time.RFC3339)
	convs := []transcript.Conversation{
		{SessionID: "rev-id", Title: "review-keda", LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "review it"}}},
		{SessionID: "exec-id", Kind: transcript.KindCodex, CodexExec: true, Spawned: true, LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "second opinion"}}},
		{SessionID: "sub1-id", Kind: transcript.KindCodex, Spawned: true, ParentID: "exec-id", Title: "Adversarial second opinion one", LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "a"}}},
		{SessionID: "sub2-id", Kind: transcript.KindCodex, Spawned: true, ParentID: "exec-id", Title: "Adversarial second opinion two", LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "b"}}},
		{SessionID: "lost-id", Kind: transcript.KindCodex, Spawned: true, ParentID: "gone-id", Title: "orphan", LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "c"}}},
	}
	m := initialModel(buildItems(convs), "", nil)
	m.parents = map[string]string{"exec-id": "rev-id"}         // the exec was seen running under review-keda
	m.live = map[string]bool{"sub1-id": true, "sub2-id": true} // running, so not behind a closed line
	m.refilter()
	depth := map[string]int{}
	for _, it := range m.filtered {
		depth[it.conv.SessionID] = it.depth
	}
	if _, shown := depth["exec-id"]; shown {
		t.Fatal("the headless exec stays hidden")
	}
	if depth["sub1-id"] != 1 || depth["sub2-id"] != 1 || depth["rev-id"] != 0 || depth["lost-id"] != 0 {
		t.Errorf("both subagents under review-keda, an orphan stays flat: %v", depth)
	}
}

// A closed codex subagent nested under the session that ran its exec waits
// behind the closed line like a closed launched session; a running one shows.
func TestClosedSpawnedChildIsHidden(t *testing.T) {
	os.Remove(sessions.ParentsPath())
	now := time.Now().UTC().Format(time.RFC3339)
	convs := []transcript.Conversation{
		{SessionID: "rev-id", Title: "review-keda", LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "review it"}}},
		{SessionID: "exec-id", Kind: transcript.KindCodex, CodexExec: true, Spawned: true, LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "second opinion"}}},
		{SessionID: "run-id", Kind: transcript.KindCodex, Spawned: true, ParentID: "exec-id", Title: "Adversarial review running", LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "a"}}},
		{SessionID: "done-id", Kind: transcript.KindCodex, Spawned: true, ParentID: "exec-id", Title: "Adversarial review done", LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "b"}}},
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	convs[3].LastTimestamp = old // done an hour ago; run-id wrote just now, inside its exec
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 110, 30
	m.parents = map[string]string{"exec-id": "rev-id"}
	m.live = map[string]bool{"rev-id": true}
	m.refilter()
	got := strings.Join(order(m), ",")
	if !strings.Contains(got, "running") || strings.Contains(got, "review done") {
		t.Fatalf("the closed codex child is hidden, the running one shows: %s", got)
	}
	if v := strip2(m.View()); !strings.Contains(v, "└ ▸ 1 closed · ^X C") {
		t.Errorf("behind review-keda's closed line:\n%s", v)
	}
	selectTitle(&m, "review-keda")
	m, _ = chord(m, "c")
	if got := strings.Join(order(m), ","); !strings.Contains(got, "review done") {
		t.Errorf("^X C shows it: %s", got)
	}
}

// ageItem makes a listed session's last message ago old.
func ageItem(m *model, id string, ago time.Duration) {
	for i := range m.items {
		if m.items[i].conv.SessionID == id {
			m.items[i].conv.LastTimestamp = time.Now().Add(-ago).UTC().Format(time.RFC3339)
		}
	}
}

// A nested session that just closed stays in view, dimmed and marked
// "closed", for closedCooldown after its last message, and isn't counted in
// the closed line until then.
func TestJustClosedSessionCoolsDown(t *testing.T) {
	m := treeModel(t)
	m.width, m.height = 110, 40
	delete(m.live, "fresh-id")
	ageItem(&m, "fresh-id", 12*time.Minute)
	m.refilter()
	v := strip2(m.View())
	if got := strings.Join(order(m), ","); !strings.Contains(got, "fresh") {
		t.Fatalf("closed 12 minutes ago: still shown: %s", got)
	}
	if !strings.Contains(v, "closed 12m") || strings.Contains(v, " └ ▸ 1 closed") {
		t.Errorf("marked closed, not yet behind hub's line:\n%s", v)
	}
	for _, it := range m.filtered {
		if it.conv.Title == "fresh" && (!it.cooling || m.titleColour(it) != "90") {
			t.Errorf("cooling, its title dim: %v %q", it.cooling, m.titleColour(it))
		}
		if it.conv.Title == "worker" && it.cooling {
			t.Error("a running session isn't cooling")
		}
	}
	ageItem(&m, "fresh-id", closedCooldown+time.Minute)
	m.refilter()
	if got := strings.Join(order(m), ","); strings.Contains(got, "fresh") || !strings.Contains(strip2(m.View()), " └ ▸ 1 closed") {
		t.Errorf("past the cooldown it goes behind the line: %s", got)
	}
}

// A named Codex session (no SessionStart hook to record its parent) nests
// under the session whose transcript started it.
func TestCodexSessionNestsUnderItsLauncher(t *testing.T) {
	os.Remove(sessions.ParentsPath())
	now := time.Now().UTC()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	convs := []transcript.Conversation{
		{SessionID: "lead-id", Title: "lead", FirstTimestamp: at(time.Hour), LastTimestamp: at(0), Launches: []string{"codex-fix"}, Messages: []transcript.Message{{Role: "user", Text: "a"}}},
		{SessionID: "cx-id", Title: "codex-fix", Kind: transcript.KindCodex, FirstTimestamp: at(time.Minute), LastTimestamp: at(0), Messages: []transcript.Message{{Role: "user", Text: "b"}}},
		{SessionID: "late-id", Title: "late", FirstTimestamp: at(0), LastTimestamp: at(0), Launches: []string{"codex-fix"}, Messages: []transcript.Message{{Role: "user", Text: "c"}}},
	}
	m := initialModel(buildItems(convs), "", nil)
	m.live = map[string]bool{"lead-id": true, "cx-id": true, "late-id": true}
	m.refilter()
	depth := map[string]int{}
	var order []string
	for _, it := range m.filtered {
		depth[it.conv.SessionID] = it.depth
		order = append(order, it.conv.SessionID)
	}
	if depth["cx-id"] != 1 || slices.Index(order, "cx-id") != slices.Index(order, "lead-id")+1 {
		t.Errorf("under lead, which launched it before it began (late began after): %v %v", depth, order)
	}
}

// escFamily lists grandparent ─ parent ─ child, spawned sessions nested by
// the parent each names, plus an unrelated top-level session, all running.
func escFamily(t *testing.T) model {
	t.Helper()
	os.Remove(sessions.ParentsPath())
	now := time.Now().UTC().Format(time.RFC3339)
	conv := func(id, title, parent string) transcript.Conversation {
		return transcript.Conversation{SessionID: id, Title: title, ParentID: parent, Spawned: parent != "",
			LastTimestamp: now, Messages: []transcript.Message{{Role: "user", Text: "work on " + title}}}
	}
	m := initialModel(buildItems([]transcript.Conversation{
		conv("other-id", "other", ""), conv("gp-id", "grandparent", ""),
		conv("p-id", "parent", "gp-id"), conv("c-id", "child", "p-id"),
	}), "", nil)
	m.width, m.height = 120, 30
	m.live = map[string]bool{"other-id": true, "gp-id": true, "p-id": true, "c-id": true}
	m.refilter()
	depth := map[string]int{}
	for _, it := range m.filtered {
		depth[it.conv.SessionID] = it.depth
	}
	if depth["gp-id"] != 0 || depth["p-id"] != 1 || depth["c-id"] != 2 {
		t.Fatalf("the family should nest three deep: %v", depth)
	}
	return m
}

// Esc walks up the family a level a press, then does what it always did at
// the top: clear the search and go back to the top of the list.
func TestEscWalksUpToTheParentThenTheSearch(t *testing.T) {
	m := moveTo(t, escFamily(t), "c-id")
	m.textInput.SetValue("work") // matches all four: the family stays nested
	m.updateFilter()
	m = moveTo(t, m, "c-id")
	m.chatFocus = false // as after a first Esc out of its message box
	esc := tea.KeyMsg{Type: tea.KeyEsc}
	m, _ = key(m, esc)
	if selected(m) != "p-id" || m.textInput.Value() != "work" || m.chatFocus {
		t.Fatalf("Esc on the child selects its parent, keeps the search and stays out of its box: %s, %q, focus %v", selected(m), m.textInput.Value(), m.chatFocus)
	}
	m, _ = key(m, esc)
	if selected(m) != "gp-id" || m.textInput.Value() != "work" {
		t.Fatalf("Esc again selects the grandparent: %s, %q", selected(m), m.textInput.Value())
	}
	m, _ = key(m, esc)
	if m.textInput.Value() != "" || m.cursor != 0 || m.chatFocus {
		t.Errorf("at the top Esc clears the search and goes back to the top: %q, cursor %d", m.textInput.Value(), m.cursor)
	}
}

// In the message box, the first Esc only leaves the box; the next walks up.
func TestEscLeavesTheMessageBoxBeforeWalkingUp(t *testing.T) {
	m := moveTo(t, escFamily(t), "c-id")
	m.chatFocus = true
	esc := tea.KeyMsg{Type: tea.KeyEsc}
	m, _ = key(m, esc)
	if m.chatFocus || selected(m) != "c-id" {
		t.Fatalf("the first Esc leaves the box, the child still selected: focus %v, %s", m.chatFocus, selected(m))
	}
	m, _ = key(m, esc)
	if selected(m) != "p-id" {
		t.Errorf("the next Esc selects the parent: %s", selected(m))
	}
}

// A popup that owns the keyboard keeps its own Esc: it closes, and the
// selection stays where it was.
func TestEscInAPopupLeavesTheSelection(t *testing.T) {
	m := moveTo(t, escFamily(t), "c-id")
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	if !m.helpOpen {
		t.Fatal("? on an empty search opens help")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.helpOpen || selected(m) != "c-id" {
		t.Errorf("Esc closes help and leaves the child selected: open %v, %s", m.helpOpen, selected(m))
	}
}

package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// subagentFixture writes a parent session's transcript and, under its
// subagents/ folder, made-up subagents: worker (running), helper (an idle
// teammate, quiet for an hour), recent (finished a minute ago), old
// (finished two hours ago) and before (from a claude process before the
// current one). It returns the parent and when its process started.
func subagentFixture(t *testing.T) (transcript.Conversation, time.Time, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "s1.jsonl")
	os.WriteFile(path, []byte(`{"type":"user","message":{"content":"fix the flaky test"},"timestamp":"2026-11-20T09:00:00Z","cwd":"`+dir+`"}`+"\n"), 0o600)
	subs := filepath.Join(dir, "s1", "subagents")
	os.MkdirAll(subs, 0o755)
	since := time.Now().Add(-time.Hour)
	write := func(name, meta string, started, last time.Time) {
		os.WriteFile(filepath.Join(subs, "agent-"+name+".meta.json"), []byte(meta), 0o600)
		tr := filepath.Join(subs, "agent-"+name+".jsonl")
		ts := started.UTC().Format(time.RFC3339)
		os.WriteFile(tr, []byte(fmt.Sprintf(`{"type":"user","isSidechain":true,"message":{"content":"look into %s"},"timestamp":%q}`+"\n"+
			`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"on it"}]},"timestamp":%q}`+"\n", name, ts, ts)), 0o600)
		os.Chtimes(tr, last, last)
		os.Chtimes(filepath.Join(subs, "agent-"+name+".meta.json"), last, last) // written with its last line: not resumed
	}
	now := time.Now()
	write("worker", `{"name":"worker"}`, now.Add(-5*time.Minute), now)
	write("helper", `{"name":"helper"}`, now.Add(-50*time.Minute), now.Add(-45*time.Minute))
	write("recent", `{"name":"recent","finished":true}`, now.Add(-3*time.Minute), now.Add(-time.Minute))
	write("old", `{"name":"old","finished":true}`, now.Add(-55*time.Minute), now.Add(-50*time.Minute))
	write("before", `{"name":"before"}`, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	parent := transcript.Conversation{SessionID: "s1", Title: "parent", FilePath: path, Cwd: dir,
		FirstTimestamp: "2026-11-20T09:00:00Z", LastTimestamp: "2026-11-20T09:00:00Z",
		Messages: []transcript.Message{{Role: "user", Text: "fix the flaky test", Ts: "2026-11-20T09:00:00Z"}}}
	return parent, since, subs
}

func subagentModel(t *testing.T) (model, transcript.Conversation, string) {
	t.Helper()
	parent, since, subs := subagentFixture(t)
	other := transcript.Conversation{SessionID: "s2", Title: "other", FirstTimestamp: "2026-11-20T08:00:00Z", LastTimestamp: "2026-11-20T08:00:00Z",
		Messages: []transcript.Message{{Role: "user", Text: "hi", Ts: "2026-11-20T08:00:00Z"}}}
	m := initialModel(buildItems([]transcript.Conversation{parent, other}), "", nil)
	m.live = map[string]bool{"s1": true}
	m.lastScan = map[string]sessions.LiveSession{"s1": {Start: since.UTC().Format(time.ANSIC)}}
	m.width, m.height = 120, 40
	rows, st := subagentRowsFor([]transcript.Conversation{parent}, map[string]time.Time{"s1": since}, nil, time.Now())
	m.applySubagents(rows, st)
	return m, parent, subs
}

func listedIDs(m model) []string {
	var ids []string
	for _, it := range m.filtered {
		ids = append(ids, fmt.Sprintf("%s@%d", it.conv.SessionID, it.depth))
	}
	return ids
}

// A live session's subagents are rows of their own, nested under it as a
// Codex subagent is: running and idle ones, and one finished within
// closedCooldown (marked closed); one finished long ago, or from an earlier
// claude process, isn't listed.
func TestSubagentRowsNestUnderTheirSession(t *testing.T) {
	m, _, _ := subagentModel(t)
	got := strings.Join(listedIDs(m), " ")
	for _, want := range []string{"s1@0", "agent-worker@1", "agent-helper@1", "agent-recent@1", "s2@0"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s listed: %s", want, got)
		}
	}
	if strings.Contains(got, "agent-old") || strings.Contains(got, "agent-before") {
		t.Errorf("a long-finished subagent, or an earlier process's, isn't listed: %s", got)
	}
	if !strings.HasPrefix(got, "s1@0 agent-") {
		t.Errorf("the subagents sit right under their session: %s", got)
	}
	var worker, recent listItem
	for _, it := range m.filtered {
		switch it.conv.SessionID {
		case "agent-worker":
			worker = it
		case "agent-recent":
			recent = it
		}
	}
	if worker.conv.Title != "worker" || worker.conv.Kind != transcript.KindSubagent || worker.conv.ParentID != "s1" {
		t.Errorf("a row titled by its meta.json, of kind subagent, under s1: %+v", worker.conv.Title)
	}
	row := m.sidebarRow(worker, false, 48)
	if !strings.Contains(row[0], "\033["+transcript.SubagentColour+"m◆") || !strings.Contains(row[0], "\033["+amber+"mworker") {
		t.Errorf("a running subagent: the pink ◆ and an amber title: %q", row[0])
	}
	if !strings.Contains(strip2(row[1]), "subagent · ") {
		t.Errorf("its grey row says it's a subagent: %q", strip2(row[1]))
	}
	if worker.cooling || !recent.cooling || !strings.Contains(strip2(m.sidebarRow(recent, false, 48)[1]), "closed") {
		t.Errorf("only the finished one is marked closed: worker %v, recent %v", worker.cooling, recent.cooling)
	}
	if got := m.taskCell(m.filtered[0].conv, false); got != "◆1" {
		t.Errorf("the session keeps its ◆ count of running subagents: %q", got)
	}
}

// A subagent's row is read-only: it previews, has no message box, Enter
// opens its session, and the actions on a session of its own are refused.
func TestSubagentRowIsReadOnly(t *testing.T) {
	m, _, _ := subagentModel(t)
	m.cursor = m.cursorOf("agent-worker")
	m.marks = &sessionMarks{Seen: map[string]time.Time{}}
	if m.selectedLive() {
		t.Fatal("a subagent has no socket: no message box")
	}
	if p := strip2(m.renderPreview(m.filtered[m.cursor], 30)); !strings.Contains(p, "look into worker") || !strings.Contains(p, "◆ subagent") {
		t.Errorf("its transcript previews, marked a subagent:\n%s", p)
	}
	for _, k := range []string{"e", "d", "x", "p", "s"} {
		m2, _ := key(m, tea.KeyMsg{Type: tea.KeyCtrlX})
		m2, _ = key(m2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune(k[0])}})
		if m2.renaming || m2.confirmDelete || m2.confirmPrune || m2.snoozeOpen || m2.pinIndex("agent-worker") >= 0 || m2.errorMsg != subagentNote {
			t.Errorf("^X %s on a subagent is refused with a note, got %q", strings.ToUpper(k), m2.errorMsg)
		}
	}
	if up, _ := key(m, tea.KeyMsg{Type: tea.KeyEsc}); up.filtered[up.cursor].conv.SessionID != "s1" {
		t.Errorf("Esc walks from a subagent to its session, got %s", up.filtered[up.cursor].conv.SessionID)
	}
	if _, cmd := key(m, tea.KeyMsg{Type: tea.KeyCtrlF}); cmd != nil {
		t.Error("^F has nothing of its own to fork")
	}

	old := liveSessionPIDs
	t.Cleanup(func() { liveSessionPIDs = old })
	liveSessionPIDs = func() map[string]int { return map[string]int{"s1": 42} }
	_, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter opens its session")
	}
	if msg, ok := cmd().(focusDoneMsg); !ok || msg.pid != 42 {
		t.Errorf("Enter focuses the session running it (pid 42): %#v", msg)
	}
}

// Reads are the live tick's, never a frame's: drawing the list doesn't read
// a subagent's transcript, an unchanged one isn't read again on a tick, and
// a growing one is read only from where it was.
func TestSubagentRowsAreNotReadPerFrame(t *testing.T) {
	m, parent, subs := subagentModel(t)
	reads := transcript.SubagentReads.Load()
	for range 50 {
		m.View()
	}
	if transcript.SubagentReads.Load() != reads {
		t.Errorf("drawing read subagent transcripts: %d reads", transcript.SubagentReads.Load()-reads)
	}
	since := time.Now().Add(-time.Hour)
	prev := func() map[string]transcript.Conversation {
		p := map[string]transcript.Conversation{}
		for _, it := range m.items {
			if it.conv.Kind == transcript.KindSubagent {
				p[it.conv.SessionID] = it.conv
			}
		}
		return p
	}
	for range 5 {
		rows, st := subagentRowsFor([]transcript.Conversation{parent}, map[string]time.Time{"s1": since}, prev(), time.Now())
		if grew := m.applySubagents(rows, st); grew != nil {
			t.Errorf("nothing grew: %d rows", len(grew))
		}
	}
	if transcript.SubagentReads.Load() != reads {
		t.Errorf("unchanged transcripts were read again: %d reads", transcript.SubagentReads.Load()-reads)
	}
	f, _ := os.OpenFile(filepath.Join(subs, "agent-worker.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintf(f, `{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"found it"}]},"timestamp":%q}`+"\n", time.Now().UTC().Format(time.RFC3339))
	f.Close()
	rows, st := subagentRowsFor([]transcript.Conversation{parent}, map[string]time.Time{"s1": since}, prev(), time.Now())
	grew := m.applySubagents(rows, st)
	if transcript.SubagentReads.Load() != reads {
		t.Errorf("a growing transcript is read from where it was, not whole: %d reads", transcript.SubagentReads.Load()-reads)
	}
	if len(grew) != 1 || grew[0].SessionID != "agent-worker" || grew[0].Messages[len(grew[0].Messages)-1].Text != "found it" || grew[0].Kind != transcript.KindSubagent {
		t.Fatalf("the grown row goes to applyLive, still a subagent: %+v", grew)
	}
	m.applyLive(grew)
	m.sortList() // as Update does after every tick
	if i := m.cursorOf("agent-worker"); i < 0 || m.filtered[i].depth != 1 {
		t.Errorf("still nested after it grew: %v", listedIDs(m))
	}
}

// The minute's full scan doesn't list subagents, so it keeps the rows the
// live tick read, selection included.
func TestRefreshKeepsSubagentRows(t *testing.T) {
	m, _, _ := subagentModel(t)
	m.cursor = m.cursorOf("agent-helper")
	var scanned []transcript.Conversation
	for _, it := range m.items {
		if it.conv.Kind == "" {
			scanned = append(scanned, it.conv)
		}
	}
	m.applyRefresh(refreshMsg{items: buildItems(scanned)})
	if m.filtered[m.cursor].conv.SessionID != "agent-helper" {
		t.Errorf("the selected subagent stays selected: %v", listedIDs(m))
	}
	// Its session closed: the next tick lists none, and they go.
	m.live = map[string]bool{}
	rows, st := subagentRowsFor(nil, nil, nil, time.Now())
	m.applySubagents(rows, st)
	if got := strings.Join(listedIDs(m), " "); strings.Contains(got, "agent-") {
		t.Errorf("a closed session's subagents go with it: %s", got)
	}
}

// The live tick reads them: a live session's subagents appear after one.
func TestLiveTickListsSubagents(t *testing.T) {
	parent, _, _ := subagentFixture(t)
	m := initialModel(buildItems([]transcript.Conversation{parent}), "", nil)
	dir := t.TempDir()
	old := sessions.GetSessionsDir
	sessions.GetSessionsDir = func() string { return dir }
	t.Cleanup(func() { sessions.GetSessionsDir = old })
	fakeStart(t)
	sessionFileFor(t, fmt.Sprintf(`{"pid":%d,"sessionId":"s1","status":"busy","procStart":%q}`, os.Getpid(), "Fri Sep 25 10:00:00 2026"))
	m = liveTickOnce(m)
	got := strings.Join(listedIDs(m), " ")
	if !m.live["s1"] || !strings.Contains(got, "agent-worker@1") {
		t.Errorf("after a live tick the session's subagents are listed under it: %s", got)
	}
}

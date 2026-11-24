package tui

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// familyModel lists grand ─ parent ─ child, none running, with the
// records saying so; opens records each tab openResumeTab opens, by id,
// and liveIDs are the sessions liveSessionPIDs says run.
func familyModel(t *testing.T, liveIDs ...string) (model, *[]string) {
	t.Helper()
	os.Remove(sessions.ParentsPath())
	t.Cleanup(func() { os.Remove(sessions.ParentsPath()) })
	sessions.SetParents([]sessions.ParentRec{
		{Child: "child", Parent: "parent", ParentID: "parent-id"},
		{Child: "parent", Parent: "grand", ParentID: "grand-id"},
	})
	at := time.Now().UTC().Format(time.RFC3339)
	conv := func(name string) transcript.Conversation {
		return transcript.Conversation{SessionID: name + "-id", Title: name, IsCustomTitle: true, Cwd: "/p", LastTimestamp: at, Messages: []transcript.Message{{Role: "user", Text: "go"}}}
	}
	m := initialModel(buildItems([]transcript.Conversation{conv("grand"), conv("parent"), conv("child")}), "", nil)
	m.width, m.height = 140, 40
	m.live = map[string]bool{}
	for _, id := range liveIDs {
		m.live[id] = true
	}
	m.launchLoaded = false

	var mu sync.Mutex
	opens := &[]string{}
	oldOpen, oldLive, oldRes := openResumeTab, liveSessionPIDs, resumable
	openResumeTab = func(c transcript.Conversation, _ []string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		*opens = append(*opens, c.SessionID)
		return true, nil
	}
	liveSessionPIDs = func() map[string]int {
		out := map[string]int{}
		for i, id := range liveIDs {
			out[id] = 1000 + i
		}
		return out
	}
	resumable = func(string, bool) bool { return true }
	t.Cleanup(func() { openResumeTab, liveSessionPIDs, resumable = oldOpen, oldLive, oldRes })
	return m, opens
}

func child(m model) transcript.Conversation {
	for _, it := range m.items {
		if it.conv.Title == "child" {
			return it.conv
		}
	}
	return transcript.Conversation{}
}

// Resuming a nested session resumes its closed parent too, after it, in a
// tab of its own (ccs keeps the focus, so the child's comes first); one
// level only: the grandparent stays closed.
func TestResumingAChildResumesItsParent(t *testing.T) {
	m, opens := familyModel(t)
	res, _ := m.Update(m.resumeCmd(child(m), false)())
	m = res.(model)
	if got := strings.Join(*opens, ","); got != "child-id,parent-id" {
		t.Errorf("the child, then its parent, and not grand: %s", got)
	}
	if !m.live["parent-id"] || !m.live["child-id"] {
		t.Error("both count as live at once")
	}
	if !strings.Contains(m.errorMsg, "resumed parent too, as child's parent") || m.noteMsg != m.errorMsg {
		t.Errorf("a grey note says so: %q", m.errorMsg)
	}
	if log, _ := os.ReadFile(state.ActivityLogPath); !strings.Contains(string(log), "resumed parent too, as child's parent") {
		t.Error("logged to the activity log")
	}
}

// A parent already running isn't opened a second time.
func TestRunningParentIsntOpenedAgain(t *testing.T) {
	m, opens := familyModel(t, "parent-id")
	m.Update(m.resumeCmd(child(m), false)())
	if slices.Contains(*opens, "parent-id") {
		t.Errorf("parent runs already: %v", *opens)
	}
	// Live by the fresh check only (the list's view is a tick old).
	m, opens = familyModel(t)
	cmd := m.resumeCmd(child(m), false)
	liveSessionPIDs = func() map[string]int { return map[string]int{"parent-id": 7} }
	cmd()
	if slices.Contains(*opens, "parent-id") {
		t.Errorf("started meanwhile: %v", *opens)
	}
}

// A parent that can't be opened is a note; the child opened all the same.
func TestParentFailureIsANote(t *testing.T) {
	m, opens := familyModel(t)
	openResumeTab = func(c transcript.Conversation, _ []string) (bool, error) {
		*opens = append(*opens, c.SessionID)
		if c.SessionID == "parent-id" {
			return false, errors.New("tab timed out")
		}
		return true, nil
	}
	res, _ := m.Update(m.resumeCmd(child(m), false)())
	m = res.(model)
	if !m.live["child-id"] || m.live["parent-id"] {
		t.Error("the child opened; the parent didn't")
	}
	if !strings.Contains(m.errorMsg, "couldn't resume parent") || m.noteMsg != m.errorMsg {
		t.Errorf("a grey note, not an error: %q", m.errorMsg)
	}
}

// ^F forks the child alone.
func TestForkDoesntResumeTheParent(t *testing.T) {
	m, opens := familyModel(t)
	m.Update(m.resumeCmd(child(m), true)())
	if slices.Contains(*opens, "parent-id") {
		t.Errorf("a fork opens only itself: %v", *opens)
	}
}

func TestHooksOutOfDateNote(t *testing.T) {
	m, _ := familyModel(t)
	if strings.Contains(m.View(), "hooks out of date") {
		t.Error("nothing missing: no note")
	}
	res, _ := m.Update(hooksMsg{missing: []string{"Stop", "SessionStart"}})
	m = res.(model)
	if !strings.Contains(strip2(m.View()), "hooks out of date: run ccs hook install") {
		t.Error("the header says so while they're missing")
	}
	if !strings.Contains(strip2(strings.Join(m.helpLines(200), "\n")), "Stop, SessionStart are missing") {
		t.Error("? help names them")
	}
	res, _ = m.Update(hooksMsg{})
	if strings.Contains(res.(model).View(), "hooks out of date") {
		t.Error("all installed: the note goes")
	}
}

// Each scan records a parent for a session its handoff brief names, so a
// session started by open-tab.sh without the SessionStart hook nests.
func TestRefreshRecordsBriefParents(t *testing.T) {
	m, _ := familyModel(t)
	at := time.Now().UTC().Format(time.RFC3339)
	brief := transcript.Conversation{SessionID: "argo-id", Title: "argo", IsCustomTitle: true, LastTimestamp: at,
		Messages: []transcript.Message{{Role: "user", Text: "Parent session: `parent`. Report back with SendMessage to `parent` when done."}}}
	items := append(slices.Clone(m.items), buildItems([]transcript.Conversation{brief})...)
	m.reload = func() ([]listItem, error) { return items, nil }
	m.startRefresh(false)()
	if r := sessions.LoadParents().Parents["argo"]; r.Parent != "parent" || r.ParentID != "parent-id" || r.Source != "brief" {
		t.Errorf("recorded from the brief: %+v", r)
	}
}

// Focus re-checks the settings, and the TUI only ever reads them.
func TestHooksCheckOnFocusOnlyReads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	settings := filepath.Join(dir, "settings.json")
	body := []byte(`{"hooks":{"PermissionRequest":[{"matcher":"*","hooks":[{"type":"command","command":"/opt/homebrew/bin/ccs hook permission","timeout":1860}]}]}}`)
	os.WriteFile(settings, body, 0o600)
	old := hooksOutOfDate
	hooksOutOfDate = mcp.HooksOutOfDate
	t.Cleanup(func() { hooksOutOfDate = old })

	m, _ := familyModel(t)
	_, cmd := m.Update(tea.FocusMsg{})
	if cmd == nil {
		t.Fatal("focus checks the hooks")
	}
	msg, ok := cmd().(hooksMsg)
	if !ok || strings.Join(msg.missing, ",") != "Stop,SessionStart" {
		t.Errorf("read from the settings: %+v", msg)
	}
	if after, _ := os.ReadFile(settings); string(after) != string(body) {
		t.Error("the TUI never writes Claude Code's settings")
	}
	if _, err := os.Stat(settings + ".before-ccs-hook"); err == nil {
		t.Error("nor a backup of them")
	}
}

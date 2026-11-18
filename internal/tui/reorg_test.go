package tui

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestLaunchNames(t *testing.T) {
	for cmd, want := range map[string][]string{
		`ccs launch --name fix-login --brief b.md --cwd ~/x`:               {"fix-login"},
		`S=/tmp; ccs launch --codex --name="probe-1" --brief $S/b.md 2>&1`: {"probe-1"},
		`ccs launch --name a --brief x && ccs launch --name b --brief y`:   {"a", "b"},
		`echo ccs launch`:  nil,
		`ccs adopt --to x`: nil,
	} {
		if got := transcript.LaunchNames(cmd); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", cmd, got, want)
		}
	}
}

func TestReorgRowsAndPrompt(t *testing.T) {
	os.Remove(sessions.ParentsPath())
	t.Cleanup(func() { os.Remove(sessions.ParentsPath()) })
	sessions.SetParents([]sessions.ParentRec{{Child: "worker", Parent: "hub"}})
	convs := []transcript.Conversation{
		{SessionID: "aaaaaaaa-2", Title: "worker", FirstTimestamp: "2026-11-02T10:00:00Z", Messages: []transcript.Message{{Role: "user", Text: "fix the\nlogin tests"}}},
		{SessionID: "aaaaaaaa-1", Title: "hub", FirstTimestamp: "2026-11-01T10:00:00Z", Launches: []string{"worker"}},
		{SessionID: "headless", Kind: transcript.KindCodex, CodexExec: true},
	}
	rows := reorgRows(convs, map[string]sessions.LiveSession{"aaaaaaaa-1": {PID: 7}})
	if len(rows) != 2 || rows[0].Conv.Title != "hub" || rows[0].Key == rows[1].Key || !rows[0].Live || rows[1].Live || rows[1].Parent != "hub" {
		t.Fatalf("headless left out, oldest first, unique keys, live and parent known: %+v", rows)
	}
	p := reorgPrompt(rows)
	for _, want := range []string{"more than 3 topics", "\thub\tclaude\tlive\t", "\tworker\t", "\thub\t-\tfix the login tests\n", "\t-\tworker\t"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestParseReorg(t *testing.T) {
	plan, err := parseReorg([]byte(`[{"type":"system"},{"type":"result","is_error":false,"structured_output":{"topics":[{"topic":"t","orchestrator":"o","parent":"","folder":"","members":["k"]}]}}]`))
	if err != nil || len(plan.Topics) != 1 || plan.Topics[0].Members[0] != "k" {
		t.Errorf("a list of events: %+v %v", plan, err)
	}
	if _, err := parseReorg([]byte(`{"type":"result","is_error":true,"result":"overloaded"}`)); err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("a failed run says why: %v", err)
	}
	if _, err := parseReorg([]byte(`not json`)); err == nil {
		t.Error("garbage is an error")
	}
}

// A reorg launches the orchestrator a topic needs, records every member
// under its topic's orchestrator, and tells only live sessions: a moved one
// who it reports to now, an existing orchestrator what it lost.
func TestApplyReorg(t *testing.T) {
	os.Remove(sessions.ParentsPath())
	t.Cleanup(func() { os.Remove(sessions.ParentsPath()) })
	var launched []sessions.LaunchOpts
	told := map[int]string{}
	oldLaunch, oldNotify := reorgLaunch, reorgNotify
	t.Cleanup(func() { reorgLaunch, reorgNotify = oldLaunch, oldNotify })
	reorgLaunch = func(o sessions.LaunchOpts) (sessions.Launched, error) {
		launched = append(launched, o)
		return sessions.Launched{PID: 99}, nil
	}
	reorgNotify = func(pid int, text string) error {
		told[pid] = text
		return nil
	}
	row := func(key, name string, live bool, pid int, parent string) reorgRow {
		return reorgRow{Key: key, Conv: transcript.Conversation{SessionID: key + "-id", Title: name}, Live: live, PID: pid, Parent: parent}
	}
	rows := []reorgRow{
		row("h", "hub", true, 1, ""), row("a", "api-fix", true, 2, "hub"), row("b", "old-billing", false, 0, "hub"),
		row("c", "billing-bug", true, 3, ""), row("d", "infra", true, 4, "hub"),
	}
	plan := reorgPlan{Topics: []reorgTopic{
		{Topic: "infra", Orchestrator: "hub", Members: []string{"d", "h"}},
		{Topic: "billing", Orchestrator: "billing-orchestrator", Parent: "hub", Folder: "~/no/such/folder", Members: []string{"a", "b", "c", "a", "zz"}},
		{Topic: "bad", Orchestrator: "no spaces allowed", Members: []string{"d"}},
	}}
	res := applyReorg(plan, rows)

	if len(launched) != 1 || launched[0].Name != "billing-orchestrator" || launched[0].Cwd != orchestratorDir() {
		t.Fatalf("one new orchestrator, in the orchestrator folder when its folder doesn't exist: %+v", launched)
	}
	brief := launched[0].Brief
	for _, want := range []string{"Your topics: billing", "report to hub", "api-fix, billing-bug", "now closed", "old-billing"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	f := sessions.LoadParents()
	for child, parent := range map[string]string{"api-fix": "billing-orchestrator", "old-billing": "billing-orchestrator", "billing-bug": "billing-orchestrator", "billing-orchestrator": "hub"} {
		if r := f.Parents[child]; r.Parent != parent || r.Source != "reorg" {
			t.Errorf("%s under %s by reorg: %+v", child, parent, r)
		}
	}
	if _, ok := f.Parents["infra"]; ok {
		t.Error("infra stayed with hub: nothing written")
	}
	if _, ok := f.Parents["hub"]; ok {
		t.Error("an orchestrator is never in its own topic")
	}
	if !strings.Contains(told[2], "report to billing-orchestrator, not hub") || !strings.Contains(told[3], "billing-orchestrator") {
		t.Errorf("moved live sessions are told: %v", told)
	}
	if !strings.Contains(told[1], "No longer yours") || !strings.Contains(told[1], "api-fix") || strings.Contains(told[1], "old-billing") {
		t.Errorf("hub hears what it lost, live sessions only: %q", told[1])
	}
	if _, ok := told[4]; ok {
		t.Error("infra didn't move: not told")
	}
	if res.Moved != 3 || res.Told != 3 || res.Orchestrators != 2 || len(res.Errs) != 1 || !strings.Contains(res.String(), "launched billing-orchestrator") {
		t.Errorf("result: %+v %s", res, res)
	}
}

// ^X M asks the model, shows its plan in a popup that owns the keyboard,
// and nothing launches or moves before enter; esc drops it.
func TestReorgKey(t *testing.T) {
	m := treeModel(t)
	oldModel, oldLaunch, oldNotify := reorgModel, reorgLaunch, reorgNotify
	t.Cleanup(func() { reorgModel, reorgLaunch, reorgNotify = oldModel, oldLaunch, oldNotify })
	reorgModel = func(ctx context.Context, prompt string) ([]byte, error) {
		return []byte(`{"type":"result","structured_output":{"topics":[{"topic":"lonely work","orchestrator":"loner-orchestrator","parent":"hub","folder":"","members":["loner-id"]}]}}`), nil
	}
	var launched []string
	reorgLaunch = func(o sessions.LaunchOpts) (sessions.Launched, error) {
		launched = append(launched, o.Name)
		return sessions.Launched{PID: 99}, nil
	}
	reorgNotify = func(int, string) error { return nil }
	run := func(m model, cmd tea.Cmd) model {
		t.Helper()
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		select {
		case msg := <-done:
			next, _ := m.Update(msg)
			return next.(model)
		case <-time.After(10 * time.Second):
			t.Fatal("never finished")
		}
		return m
	}
	plan := func(m model) model {
		t.Helper()
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
		next, cmd = next.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
		m = next.(model)
		if m.reorgBusy != "planning" || cmd == nil || !strings.Contains(strip2(m.View()), "reorg planning") {
			t.Fatalf("^X M asks for a plan and the header says so: %q", m.reorgBusy)
		}
		m = run(m, cmd)
		v := strip2(m.View())
		if m.reorgPlan == nil || !strings.Contains(v, "lonely work → loner-orchestrator (new, under hub)") || !strings.Contains(v, "moving in: loner") {
			t.Fatalf("the plan shows in its popup:\n%s", v)
		}
		return m
	}

	m = plan(m)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.reorgPlan == nil {
		t.Fatal("the popup owns the keyboard")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.reorgPlan != nil || len(launched) > 0 || len(sessions.LoadParents().Parents) != 5 {
		t.Fatalf("esc drops it and nothing moves: launched %v", launched)
	}

	m = plan(m)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.reorgPlan != nil || m.reorgBusy != "reorganising" || cmd == nil {
		t.Fatal("enter applies it")
	}
	m = run(m, cmd)
	if m.reorgBusy != "" || !slices.Equal(launched, []string{"loner-orchestrator"}) || !strings.Contains(m.errorMsg, "1 topics") {
		t.Errorf("applied: launched %v, note %q", launched, m.errorMsg)
	}
	if r := sessions.LoadParents().Parents["loner"]; r.Parent != "loner-orchestrator" || r.Source != "reorg" {
		t.Errorf("loner moved: %+v", r)
	}
}

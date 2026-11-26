package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// orchestratorFixture is a clean state: no record, no folder, no sessions, tabs
// stubbed, and claude reporting no ccs MCP server until it's added. The
// returned slice collects each mcp command run.
func orchestratorFixture(t *testing.T) (ran *[]string) {
	t.Helper()
	dir := t.TempDir()
	oldSettings, oldMarks, oldProjects, oldRun := state.SettingsPath, state.MarksPath, transcript.GetProjectsDir, mcpRun
	state.SettingsPath = filepath.Join(dir, "ccs", "settings.json")
	state.MarksPath = filepath.Join(dir, "ccs", "sessions.json")
	transcript.GetProjectsDir = func() string { return filepath.Join(dir, "projects") }
	sessionsFixture(t)
	stubLaunch(t)
	oldBin := sessions.LookBin
	sessions.LookBin = func(name string) (string, error) {
		if name == "claude" {
			return "/bin/claude", nil
		}
		return "", errors.New("not installed")
	}
	added := false
	ran = &[]string{}
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		cmd := bin + " " + strings.Join(args, " ")
		*ran = append(*ran, cmd)
		switch {
		case strings.Contains(cmd, "mcp add"):
			added = true
			return []byte("Added"), nil
		case added:
			return []byte("Status: ✔ Connected"), nil
		}
		return []byte(`No MCP server named "ccs".`), errors.New("exit status 1")
	}
	t.Cleanup(func() {
		state.SettingsPath, state.MarksPath, transcript.GetProjectsDir, mcpRun, sessions.LookBin = oldSettings, oldMarks, oldProjects, oldRun, oldBin
	})
	return ran
}

// startsAs makes a launched tab register a live session called name, as claude
// does once started.
func startsAs(t *testing.T, name string) (scripts *[]string) {
	t.Helper()
	scripts = &[]string{}
	old := sessions.OpenLaunchTab
	sessions.OpenLaunchTab = func(script string) (string, error) {
		*scripts = append(*scripts, script)
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "1.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"started","name":"`+name+`"}`), 0o600)
		return "/dev/ttys9", nil
	}
	t.Cleanup(func() { sessions.OpenLaunchTab = old })
	return scripts
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func runOrch(args ...string) (string, int) {
	var out bytes.Buffer
	code := runOrchestrator(args, strings.NewReader(""), &out, &out, false)
	return out.String(), code
}

func TestOrchestratorSetupFromCleanState(t *testing.T) {
	ran := orchestratorFixture(t)
	scripts := startsAs(t, orchestratorName)
	var focused string
	oldFocus := sessions.FocusTTY
	sessions.FocusTTY = func(tty string, _ bool) bool { focused = tty; return true }
	defer func() { sessions.FocusTTY = oldFocus }()

	out, code := runOrch("--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"claude mcp add --scope user ccs --", "trust the folder", "connected", "CLAUDE.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !slices.ContainsFunc(*ran, func(c string) bool { return strings.HasPrefix(c, "claude mcp add") }) {
		t.Errorf("MCP not added: %v", *ran)
	}
	if focused != "/dev/ttys9" {
		t.Errorf("its tab must come to the front for the trust question, focused %q", focused)
	}
	role, err := os.ReadFile(filepath.Join(orchestratorDir(), "CLAUDE.md"))
	if err != nil || !strings.Contains(string(role), "never do the work yourself") {
		t.Errorf("role: %v %s", err, role)
	}
	if strings.Contains(string(role), "{{events}}") || !strings.Contains(string(role), github.PREventsPath()) {
		t.Errorf("the events path isn't filled in:\n%s", role)
	}
	var cfg struct {
		Permissions struct{ Allow []string }
	}
	raw, _ := os.ReadFile(filepath.Join(orchestratorDir(), ".claude", "settings.json"))
	if err := json.Unmarshal(raw, &cfg); err != nil || !slices.Contains(cfg.Permissions.Allow, "Bash(ccs launch:*)") {
		t.Errorf("settings: %v %s", err, raw)
	}
	rec := loadOrchestrator()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(rec.SessionID) || !rec.Offered {
		t.Errorf("record: %+v", rec)
	}
	script, _ := os.ReadFile((*scripts)[0])
	if !strings.Contains(string(script), "--session-id '"+rec.SessionID+"'") || !strings.Contains(string(script), "--name 'orchestrator'") {
		t.Errorf("launcher:\n%s", script)
	}
}

func TestOrchestratorAsksBeforeChangingAnything(t *testing.T) {
	ran := orchestratorFixture(t)
	scripts := startsAs(t, orchestratorName)

	out, code := runOrch()
	if code == 0 || !strings.Contains(out, "Nothing changed") || !strings.Contains(out, "claude mcp add") {
		t.Errorf("not a terminal, no --yes: shows the plan and stops, got %d:\n%s", code, out)
	}
	var b bytes.Buffer
	if code := runOrchestrator(nil, strings.NewReader("n\n"), &b, &b, true); code == 0 || !strings.Contains(b.String(), "Nothing changed") {
		t.Errorf("answered no: %d %s", code, b.String())
	}
	if slices.ContainsFunc(*ran, func(c string) bool { return strings.Contains(c, "mcp add") }) || len(*scripts) > 0 {
		t.Errorf("declined, yet ran %v and opened %v", *ran, *scripts)
	}
	if _, err := os.Stat(orchestratorDir()); err == nil {
		t.Error("declined, yet its folder exists")
	}
	if loadOrchestrator().SessionID != "" {
		t.Error("declined, yet recorded")
	}
}

func TestOrchestratorMCPAlreadyInstalledIsLeftAlone(t *testing.T) {
	orchestratorFixture(t)
	oldBin, oldRun := sessions.LookBin, mcpRun
	sessions.LookBin = func(name string) (string, error) { return "/bin/" + name, nil }
	var ran []string
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		ran = append(ran, bin+" "+strings.Join(args, " "))
		if bin == "codex" {
			return []byte(`[{"name":"other"},{"name":"ccs"}]`), nil
		}
		return []byte("Status: ✔ Connected"), nil
	}
	defer func() { sessions.LookBin, mcpRun = oldBin, oldRun }()
	p := planOrchestrator(false)
	if len(p.fixes) != 0 || len(p.notes) != 0 || slices.ContainsFunc(p.lines(), func(l string) bool { return strings.Contains(l, "mcp") }) {
		t.Errorf("nothing to do, nothing to say: %+v %v", p, p.lines())
	}
	if slices.ContainsFunc(ran, func(c string) bool { return strings.Contains(c, "mcp add") }) {
		t.Errorf("added over an existing one: %v", ran)
	}
}

func TestOrchestratorMCPBrokenIsSaidAndLeftAlone(t *testing.T) {
	ran := orchestratorFixture(t)
	old := mcpRun
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		*ran = append(*ran, strings.Join(args, " "))
		return []byte("ccs:\n  Scope: User config\n  Status: ✘ Failed to connect\n"), nil
	}
	defer func() { mcpRun = old }()
	p := planOrchestrator(false)
	if len(p.fixes) != 0 || len(p.notes) != 1 || !strings.Contains(p.notes[0], "isn't connected") {
		t.Errorf("%+v", p)
	}
}

func TestOrchestratorMCPAddedMeanwhileIsNoFailure(t *testing.T) {
	orchestratorFixture(t)
	old := mcpRun
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		if args[1] == "add" {
			return []byte("MCP server ccs already exists in user config"), errors.New("exit status 1")
		}
		return []byte("Status: ✔ Connected"), nil
	}
	defer func() { mcpRun = old }()
	done, errs := applyMCP([]mcpFix{{Bin: "claude", Args: []string{"mcp", "add", "ccs"}}})
	if len(errs) != 0 || len(done) != 1 {
		t.Errorf("%v %v", done, errs)
	}
}

func TestMCPCommandsRunFromHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := mcpRun
	mcpRun = realMCPRun
	defer func() { mcpRun = old }()
	bin := filepath.Join(t.TempDir(), "fake")
	os.WriteFile(bin, []byte("#!/bin/sh\npwd -P\n"), 0o700)
	out, err := mcpRun(bin)
	want, _ := filepath.EvalSymlinks(home)
	if err != nil || strings.TrimSpace(string(out)) != want {
		t.Errorf("%v %q want %q", err, out, want)
	}
}

func TestOrchestratorMCPCodexMissingOnly(t *testing.T) {
	orchestratorFixture(t)
	oldBin, oldRun := sessions.LookBin, mcpRun
	sessions.LookBin = func(name string) (string, error) { return "/bin/" + name, nil }
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		if bin == "codex" {
			return []byte(`[{"name":"other"}]`), nil
		}
		return []byte("Connected"), nil
	}
	defer func() { sessions.LookBin, mcpRun = oldBin, oldRun }()
	p := planOrchestrator(false)
	if len(p.fixes) != 1 || !strings.HasPrefix(p.fixes[0].String(), "codex mcp add ccs --") {
		t.Errorf("%+v", p)
	}
}

func TestOrchestratorMCPUnknownIsNotInstalled(t *testing.T) {
	orchestratorFixture(t)
	oldRun := mcpRun
	mcpRun = func(string, ...string) ([]byte, error) { return []byte("boom"), errors.New("timed out") }
	defer func() { mcpRun = oldRun }()
	p := planOrchestrator(false)
	if len(p.fixes) != 0 || len(p.notes) != 1 {
		t.Errorf("can't tell: never install, say so: %+v", p)
	}
}

func TestOrchestratorMCPFailureIsReported(t *testing.T) {
	orchestratorFixture(t)
	oldRun := mcpRun
	defer func() { mcpRun = oldRun }()
	mcpRun = func(string, ...string) ([]byte, error) { return []byte("Status: ✗ Failed"), nil }
	done, errs := applyMCP([]mcpFix{{Bin: "claude", Args: []string{"mcp", "add"}}})
	if len(done) != 0 || len(errs) != 1 || !strings.Contains(errs[0], "connected") {
		t.Errorf("added but not connected must say so: %v %v", done, errs)
	}
	mcpRun = func(string, ...string) ([]byte, error) { return []byte("nope"), errors.New("exit 1") }
	if _, errs := applyMCP([]mcpFix{{Bin: "claude", Args: []string{"mcp", "add"}}}); len(errs) != 1 || !strings.Contains(errs[0], "nope") {
		t.Errorf("%v", errs)
	}
}

func TestOrchestratorNeedsClaude(t *testing.T) {
	orchestratorFixture(t)
	old := sessions.LookBin
	sessions.LookBin = func(string) (string, error) { return "", errors.New("no") }
	defer func() { sessions.LookBin = old }()
	out, code := runOrch("--yes")
	if code == 0 || !strings.Contains(out, "needs Claude Code") {
		t.Errorf("%d %s", code, out)
	}
}

func TestOrchestratorNeverMakesASecond(t *testing.T) {
	t.Run("recorded and running", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		orchestratorRecord{SessionID: "the-id"}.save()
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"the-id","name":"renamed"}`), 0o600)
		var pid int
		old := focusPID
		focusPID = func(p int, _ bool) bool { pid = p; return true }
		defer func() { focusPID = old }()
		out, code := runOrch("--yes")
		if code != 0 || !strings.Contains(out, "Switched") || pid != os.Getpid() || len(*scripts) > 0 {
			t.Errorf("%d %s pid=%d scripts=%v", code, out, pid, *scripts)
		}
	})
	t.Run("running under its name, id changed", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		orchestratorRecord{SessionID: "before-clear"}.save()
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"orchestrator"}`), 0o600)
		old := focusPID
		focusPID = func(int, bool) bool { return true }
		defer func() { focusPID = old }()
		if _, code := runOrch("--yes"); code != 0 || len(*scripts) > 0 {
			t.Errorf("%d %v", code, *scripts)
		}
		if got := loadOrchestrator().SessionID; got != "after-clear" {
			t.Errorf("record follows it: %s", got)
		}
	})
	t.Run("started, trust question unanswered", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		orchestratorRecord{SessionID: "the-id"}.save()
		oldPs, oldFocus := proc.PsAll, focusPID
		proc.PsAll = func() (string, error) {
			return "4242 ttys1 /bin/claude hello --session-id the-id --name orchestrator\n", nil
		}
		var pid int
		focusPID = func(p int, _ bool) bool { pid = p; return true }
		defer func() { proc.PsAll, focusPID = oldPs, oldFocus }()
		out, code := runOrch("--yes")
		if code != 0 || pid != 4242 || !strings.Contains(out, "trust") || len(*scripts) > 0 {
			t.Errorf("%d %s pid=%d %v", code, out, pid, *scripts)
		}
	})
	t.Run("being resumed, no session file yet", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		orchestratorRecord{SessionID: "the-id"}.save()
		proj := filepath.Join(transcript.GetProjectsDir(), "-w")
		os.MkdirAll(proj, 0o700)
		os.WriteFile(filepath.Join(proj, "the-id.jsonl"), []byte(`{"type":"user","cwd":"/tmp","sessionId":"the-id","message":{"role":"user","content":"hi"}}`+"\n"), 0o600)
		oldPs, oldFocus := proc.PsAll, focusPID
		proc.PsAll = func() (string, error) {
			return "4242 ttys1 /bin/claude --resume the-id --name orchestrator\n", nil
		}
		var pid int
		focusPID = func(p int, _ bool) bool { pid = p; return true }
		defer func() { proc.PsAll, focusPID = oldPs, oldFocus }()
		if out, code := runOrch("--yes"); code != 0 || pid != 4242 || len(*scripts) > 0 {
			t.Errorf("%d %s pid=%d %v", code, out, pid, *scripts)
		}
	})
	t.Run("a ccs command naming the id is no claude", func(t *testing.T) {
		orchestratorFixture(t)
		old := proc.PsAll
		proc.PsAll = func() (string, error) {
			return "4242 ttys1 /opt/homebrew/bin/ccs launch --name x --resume the-id\n", nil
		}
		defer func() { proc.PsAll = old }()
		if pid := startingProcess("the-id"); pid != 0 {
			t.Errorf("pid %d", pid)
		}
	})
	t.Run("cleared: its arguments still name the old id, as claude's do", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		orchestratorRecord{SessionID: "before-clear"}.save()
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"orchestrator"}`), 0o600)
		oldPs, oldFocus := proc.PsAll, focusPID
		proc.PsAll = func() (string, error) {
			return itoa(os.Getpid()) + " ttys1 /bin/claude hello --session-id before-clear --name orchestrator\n", nil
		}
		focusPID = func(int, bool) bool { return true }
		defer func() { proc.PsAll, focusPID = oldPs, oldFocus }()
		out, code := runOrch("--yes")
		if code != 0 || strings.Contains(out, "trust") || len(*scripts) > 0 || loadOrchestrator().SessionID != "after-clear" {
			t.Errorf("%d %s %v %s", code, out, *scripts, loadOrchestrator().SessionID)
		}
	})
	t.Run("cleared and renamed: the process says which it is", func(t *testing.T) {
		orchestratorFixture(t)
		orchestratorRecord{SessionID: "before-clear"}.save()
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"hub"}`), 0o600)
		old := proc.PsAll
		proc.PsAll = func() (string, error) {
			return itoa(os.Getpid()) + " ttys1 /bin/claude hello --session-id before-clear --name orchestrator\n", nil
		}
		defer func() { proc.PsAll = old }()
		if s := findOrchestrator(); s.kind != orchLive || s.rec.SessionID != "after-clear" {
			t.Errorf("%+v", s)
		}
	})
	t.Run("an offer left open opens the one started since", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		p := planOrchestrator(false) // both ccs windows made their offers here
		orchestratorRecord{SessionID: "the-id"}.save()
		oldPs, oldFocus := proc.PsAll, focusPID
		proc.PsAll = func() (string, error) {
			return "4242 ttys1 /bin/claude hello --session-id the-id --name orchestrator\n", nil
		}
		var pid int
		focusPID = func(p int, _ bool) bool { pid = p; return true }
		defer func() { proc.PsAll, focusPID = oldPs, oldFocus }()
		msg := orchestratorSetupCmd(p)().(orchestratorDoneMsg)
		if msg.err != nil || pid != 4242 || len(*scripts) > 0 || loadOrchestrator().SessionID != "the-id" {
			t.Errorf("%+v pid=%d %v %s", msg, pid, *scripts, loadOrchestrator().SessionID)
		}
	})
	t.Run("another ccs has the turn", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		release, _, err := orchestratorTurn()
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		out, code := runOrch("--yes")
		if code != 1 || !strings.Contains(out, "another ccs") || len(*scripts) > 0 {
			t.Errorf("%d %s %v", code, out, *scripts)
		}
		if _, err := setupOrchestrator(orchestratorPlan{}, false); !errors.Is(err, errOrchestratorBusy) || len(*scripts) > 0 {
			t.Errorf("%v %v", err, *scripts)
		}
		// One that's already up can still be switched to.
		orchestratorRecord{SessionID: "the-id"}.save()
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"the-id","name":"orchestrator"}`), 0o600)
		old := focusPID
		focusPID = func(int, bool) bool { return true }
		defer func() { focusPID = old }()
		if out, code := runOrch("--yes"); code != 0 || !strings.Contains(out, "Switched") {
			t.Errorf("%d %s", code, out)
		}
	})
	t.Run("named transcript, not running", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		proj := filepath.Join(transcript.GetProjectsDir(), "-w")
		os.MkdirAll(proj, 0o700)
		os.WriteFile(filepath.Join(proj, "old-id.jsonl"), []byte(`{"type":"user","cwd":"/tmp","sessionId":"old-id","message":{"role":"user","content":"hi"}}`+"\n"), 0o600)
		old := namedTranscript
		namedTranscript = func(n string) string {
			if n == orchestratorName {
				return "old-id"
			}
			return ""
		}
		defer func() { namedTranscript = old }()
		out, code := runOrch("--yes")
		if code != 0 || !strings.Contains(out, "Resumed") || len(*scripts) != 1 {
			t.Fatalf("%d %s %v", code, out, *scripts)
		}
		script, _ := os.ReadFile((*scripts)[0])
		if !strings.Contains(string(script), "--resume 'old-id'") || strings.Contains(string(script), "--session-id") || strings.Contains(string(script), "cat ") {
			t.Errorf("a resume, no brief, no new id:\n%s", script)
		}
	})
}

func TestOrchestratorListFollowsClear(t *testing.T) {
	orchestratorFixture(t)
	orchestratorRecord{SessionID: "before-clear", Offered: true}.save()
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "before-clear", Title: "orchestrator"}, {SessionID: "after-clear", Title: "orchestrator"}}), "", nil)
	m.orchID = "before-clear"
	res, _ := m.Update(liveMsg{pids: map[string]int{"after-clear": 7}, scan: map[string]sessions.LiveSession{"after-clear": {PID: 7, Stat: sessions.SessionStat{Name: "orchestrator"}}}})
	m = res.(model)
	if m.orchID != "after-clear" {
		t.Errorf("the list follows the session: %s", m.orchID)
	}
	if rec := loadOrchestrator(); rec.SessionID != "after-clear" || !rec.Offered {
		t.Errorf("record: %+v", rec)
	}
	// Two with its name: never guess, and a closed orchestrator stays recorded.
	orchestratorRecord{SessionID: "before-clear"}.save()
	two := map[string]sessions.LiveSession{"x": {PID: 7, Stat: sessions.SessionStat{Name: "orchestrator"}}, "y": {PID: 8, Stat: sessions.SessionStat{Name: "orchestrator"}}}
	rec := loadOrchestrator()
	if followLive(&rec, two) || followLive(&rec, map[string]sessions.LiveSession{}) || rec.SessionID != "before-clear" {
		t.Errorf("%+v", rec)
	}
}

func TestOrchestratorRecordSavesLeaveNoTempFiles(t *testing.T) {
	orchestratorFixture(t)
	for range 3 {
		if err := (orchestratorRecord{SessionID: "x"}).save(); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(orchestratorPath()), "*.tmp")); len(left) != 0 {
		t.Error(left)
	}
}

func TestOrchestratorComesBackInItsOwnFolder(t *testing.T) {
	orchestratorFixture(t)
	scripts := startsAs(t, orchestratorName)
	orchestratorRecord{SessionID: "the-id"}.save()
	proj := filepath.Join(transcript.GetProjectsDir(), "-w")
	os.MkdirAll(proj, 0o700)
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(proj, "the-id.jsonl"), []byte(`{"type":"user","cwd":"`+cwd+`","sessionId":"the-id","message":{"role":"user","content":"hi"}}`+"\n"), 0o600)
	if out, code := runOrch(); code != 0 { // no --yes: a resume changes nothing, so it asks nothing
		t.Fatalf("%d %s", code, out)
	}
	script, _ := os.ReadFile((*scripts)[0])
	if !strings.Contains(string(script), "cd '"+cwd+"'") || !strings.Contains(string(script), "--resume 'the-id'") {
		t.Errorf("%s", script)
	}
}

func TestOrchestratorRecreatedAfterItsTranscriptIsGone(t *testing.T) {
	orchestratorFixture(t)
	scripts := startsAs(t, orchestratorName)
	orchestratorRecord{SessionID: "deleted", Offered: true}.save()
	if out, code := runOrch("--yes"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	script, _ := os.ReadFile((*scripts)[0])
	if strings.Contains(string(script), "--resume") || !strings.Contains(string(script), "--session-id") {
		t.Errorf("%s", script)
	}
}

func TestOrchestratorKeepsTheUsersEdits(t *testing.T) {
	orchestratorFixture(t)
	dir := orchestratorDir()
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine"), 0o600)
	if err := writeOrchestratorFolder(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != "mine" {
		t.Errorf("overwrote it: %s", b)
	}
}

func TestOrchestratorAdopt(t *testing.T) {
	orchestratorFixture(t)
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"their-id","name":"hub"}`), 0o600)
	out, code := runOrch("--adopt", "hub")
	if code != 0 || !strings.Contains(out, "now the orchestrator") {
		t.Fatalf("%d %s", code, out)
	}
	if r := loadOrchestrator(); r.SessionID != "their-id" || r.Name != "hub" || !r.Offered {
		t.Errorf("%+v", r)
	}
	if s := findOrchestrator(); s.kind != orchLive {
		t.Errorf("%+v", s)
	}
	if out, code := runOrch("--adopt", "nobody"); code == 0 || !strings.Contains(out, "no Claude Code session") {
		t.Errorf("%d %s", code, out)
	}
}

func TestOrchestratorAdoptOnlyWhatItCanBringBack(t *testing.T) {
	session := func(t *testing.T, id, title string) {
		t.Helper()
		proj := filepath.Join(transcript.GetProjectsDir(), "-w")
		os.MkdirAll(proj, 0o700)
		os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(`{"type":"user","cwd":"/tmp","sessionId":"`+id+`","timestamp":"2026-11-04T10:00:00Z","message":{"role":"user","content":"hi"}}`+"\n"+`{"type":"custom-title","customTitle":"`+title+`","sessionId":"`+id+`"}`+"\n"), 0o600)
	}
	t.Run("two running with the name: never a guess", func(t *testing.T) {
		orchestratorFixture(t)
		for i, id := range []string{"one", "two"} {
			os.WriteFile(filepath.Join(sessions.GetSessionsDir(), itoa(i)+".json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"`+id+`","name":"hub"}`), 0o600)
		}
		out, code := runOrch("--adopt", "hub")
		if code != 1 || !strings.Contains(out, "2 running sessions") || loadOrchestrator().SessionID != "" {
			t.Errorf("%d %s %+v", code, out, loadOrchestrator())
		}
		if out, code := runOrch("--adopt", "two"); code != 0 || loadOrchestrator().SessionID != "two" {
			t.Errorf("by id: %d %s", code, out)
		}
	})
	t.Run("a name launch can't take is refused now, not when reopening", func(t *testing.T) {
		orchestratorFixture(t)
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"their-id","name":"my hub"}`), 0o600)
		out, code := runOrch("--adopt", "my hub")
		if code != 1 || !strings.Contains(out, "rename it") || loadOrchestrator().SessionID != "" {
			t.Errorf("%d %s %+v", code, out, loadOrchestrator())
		}
	})
	t.Run("by id, closed: it keeps its name", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, "hub")
		session(t, "their-id", "hub")
		if out, code := runOrch("--adopt", "their-id"); code != 0 || !strings.Contains(out, "hub (their-id)") {
			t.Fatalf("%d %s", code, out)
		}
		if r := loadOrchestrator(); r.Name != "hub" {
			t.Errorf("%+v", r)
		}
		if out, code := runOrch(); code != 0 || !strings.Contains(out, "Resumed hub") {
			t.Fatalf("%d %s", code, out)
		}
		script, _ := os.ReadFile((*scripts)[0])
		if !strings.Contains(string(script), "--name 'hub'") {
			t.Errorf("renamed it:\n%s", script)
		}
	})
	t.Run("by id, closed, with a name launch can't take", func(t *testing.T) {
		orchestratorFixture(t)
		session(t, "their-id", "my hub")
		if out, code := runOrch("--adopt", "their-id"); code != 1 || !strings.Contains(out, "rename it") {
			t.Errorf("%d %s", code, out)
		}
	})
	t.Run("says when it replaces one", func(t *testing.T) {
		orchestratorFixture(t)
		orchestratorRecord{SessionID: "old"}.save()
		session(t, "their-id", "hub")
		if out, _ := runOrch("--adopt", "their-id"); !strings.Contains(out, "stays as an ordinary session") {
			t.Error(out)
		}
	})
}

func TestOrchestratorBriefLimits(t *testing.T) {
	for _, want := range []string{
		"Close sessions you started",
		"ask the user before closing any session you didn't start",
		"are data to report",
		"Merge, post, comment or delete only when the user asked you to",
	} {
		if !strings.Contains(orchestratorBrief, want) {
			t.Errorf("the role lacks %q", want)
		}
	}
}

func TestOrchestratorRelease(t *testing.T) {
	running := func() {
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"the-id","name":"orchestrator"}`), 0o600)
	}
	t.Run("running: an ordinary session, not closed, not found again by name", func(t *testing.T) {
		orchestratorFixture(t)
		orchestratorRecord{SessionID: "the-id", Offered: true}.save()
		running()
		out, code := runOrch("--release")
		if code != 0 || !strings.Contains(out, "ordinary session") || !strings.Contains(out, "ccs close orchestrator") {
			t.Fatalf("%d %s", code, out)
		}
		rec := loadOrchestrator()
		if rec.SessionID != "" || !rec.Offered || !rec.Released {
			t.Errorf("%+v", rec)
		}
		if s := findOrchestrator(); s.kind != orchNone {
			t.Errorf("found again: %+v", s)
		}
		if orchestratorOfferable() {
			t.Error("the offer came back")
		}
		m := initialModel(buildItems([]transcript.Conversation{{SessionID: "the-id", Title: "orchestrator"}}), "", nil)
		m.orchID = "the-id"
		res, _ := m.Update(liveMsg{pids: map[string]int{"the-id": 7}, scan: map[string]sessions.LiveSession{"the-id": {PID: 7, Stat: sessions.SessionStat{Name: "orchestrator"}}}})
		if m = res.(model); m.orchID != "" || sectionOf(m.filtered[0].familyState) == orchestratorSec {
			t.Errorf("still on top: %q", m.orchID)
		}
	})
	t.Run("closed: its transcript isn't found by name either", func(t *testing.T) {
		orchestratorFixture(t)
		old := namedTranscript
		namedTranscript = func(string) string { return "the-id" }
		defer func() { namedTranscript = old }()
		if out, code := runOrch("--release"); code != 0 || strings.Contains(out, "still running") {
			t.Errorf("%d %s", code, out)
		}
		if s := findOrchestrator(); s.kind != orchNone {
			t.Errorf("%+v", s)
		}
	})
	t.Run("none: says so, changes nothing", func(t *testing.T) {
		orchestratorFixture(t)
		if out, code := runOrch("--release"); code != 0 || !strings.Contains(out, "no orchestrator") || loadOrchestrator().Released {
			t.Errorf("%d %s", code, out)
		}
	})
	t.Run("a new one waits for the old name to be free, changing nothing", func(t *testing.T) {
		ran := orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		orchestratorRecord{SessionID: "the-id"}.save()
		running()
		runOrch("--release")
		out, code := runOrch("--yes")
		if code != 1 || !strings.Contains(out, "still running") || len(*scripts) > 0 {
			t.Errorf("%d %s %v", code, out, *scripts)
		}
		if slices.ContainsFunc(*ran, func(c string) bool { return strings.Contains(c, "mcp add") }) {
			t.Errorf("changed config: %v", *ran)
		}
		if rec := loadOrchestrator(); !rec.Released || rec.SessionID != "" {
			t.Errorf("%+v", rec)
		}
	})
	t.Run("setting up again clears it", func(t *testing.T) {
		orchestratorFixture(t)
		startsAs(t, orchestratorName)
		orchestratorRecord{Offered: true, Released: true}.save()
		if out, code := runOrch("--yes"); code != 0 {
			t.Fatalf("%d %s", code, out)
		}
		if rec := loadOrchestrator(); rec.Released || rec.SessionID == "" {
			t.Errorf("%+v", rec)
		}
	})
	t.Run("adopting clears it", func(t *testing.T) {
		orchestratorFixture(t)
		orchestratorRecord{Offered: true, Released: true}.save()
		os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"their-id","name":"hub"}`), 0o600)
		if out, code := runOrch("--adopt", "hub"); code != 0 {
			t.Fatalf("%d %s", code, out)
		}
		if rec := loadOrchestrator(); rec.Released || rec.SessionID != "their-id" {
			t.Errorf("%+v", rec)
		}
		if s := findOrchestrator(); s.kind != orchLive {
			t.Errorf("%+v", s)
		}
	})
	t.Run("not with --adopt", func(t *testing.T) {
		orchestratorFixture(t)
		if _, code := runOrch("--release", "--adopt", "hub"); code != 2 {
			t.Errorf("code %d", code)
		}
	})
}

func TestOrchestratorInPlaceOutsideTabs(t *testing.T) {
	orchestratorFixture(t)
	t.Setenv("ITERM_SESSION_ID", "")
	var ran string
	old := sessions.ExecScript
	sessions.ExecScript = func(s string) error { ran = s; return nil }
	defer func() { sessions.ExecScript = old }()
	if out, code := runOrch("--yes"); code != 0 || !strings.HasSuffix(ran, "launch.sh") {
		t.Errorf("%d %s %q", code, out, ran)
	}
}

func TestOrchestratorHasItsOwnSectionAbovePinned(t *testing.T) {
	orchestratorFixture(t)
	now := time.Now().UTC()
	at := now.Format(time.RFC3339)
	convs := []transcript.Conversation{
		{SessionID: "a", Title: "a pinned one", LastTimestamp: at, Messages: []transcript.Message{{Role: "user", Text: "x"}}},
		{SessionID: "b", Title: "another", LastTimestamp: at, Messages: []transcript.Message{{Role: "user", Text: "x"}}},
		{SessionID: "c", Title: orchestratorName, LastTimestamp: now.Add(-time.Hour).Format(time.RFC3339), Messages: []transcript.Message{{Role: "user", Text: "y"}}},
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 140, 30
	m.marks = &sessionMarks{Since: now, Seen: map[string]time.Time{}, Pinned: []string{"a"}}
	m.sortList()
	if strings.Contains(strip2(m.View()), "orchestrator 1") {
		t.Fatal("no record: no section")
	}
	orchestratorRecord{SessionID: "c"}.save()
	m.syncOrchestrator()
	if m.filtered[0].conv.SessionID != "c" || m.filtered[1].conv.SessionID != "a" {
		t.Fatalf("orchestrator, then pinned: %s %s", m.filtered[0].conv.SessionID, m.filtered[1].conv.SessionID)
	}
	v := strip2(m.View())
	co, pin := strings.Index(v, orchestratorName), strings.Index(v, "pinned 1")
	if co < 0 || pin < 0 || co > pin {
		t.Errorf("section order:\n%s", v)
	}
	if strings.Contains(v, "orchestrator 1") {
		t.Errorf("a lone session named orchestrator needs no orchestrator header over it:\n%s", v)
	}
	if slices.Contains(m.marks.Pinned, "c") {
		t.Errorf("it isn't a pin: %v", m.marks.Pinned)
	}
	m.cursor = 0
	m.togglePin() // pinning it changes nothing about where it's listed
	if m.filtered[0].conv.SessionID != "c" {
		t.Errorf("still first: %s", m.filtered[0].conv.SessionID)
	}
}

// TestOldHideSettingIgnored: ^X C hid the orchestrator before the trees
// landed. A saved hide_orchestrator (or hide_coordinator) no longer hides it,
// the header has no toggle for it, and a save drops the key.
func TestOldHideSettingIgnored(t *testing.T) {
	orchestratorFixture(t)
	os.MkdirAll(filepath.Dir(state.SettingsPath), 0o700)
	os.WriteFile(state.SettingsPath, []byte(`{"hide_orchestrator":true,"hide_coordinator":true}`), 0o600)
	at := time.Now().UTC().Format(time.RFC3339)
	m := initialModel(buildItems([]transcript.Conversation{
		{SessionID: "b", Title: "another", LastTimestamp: at, Messages: []transcript.Message{{Role: "user", Text: "x"}}},
		{SessionID: "c", Title: orchestratorName, LastTimestamp: at, Messages: []transcript.Message{{Role: "user", Text: "y"}}},
	}), "", nil)
	m.width, m.height = 140, 30
	orchestratorRecord{SessionID: "c"}.save()
	m.syncOrchestrator()
	if v := strip2(m.View()); len(m.filtered) != 2 || m.filtered[0].conv.SessionID != "c" || strings.Contains(v, "orchestrator on") || strings.Contains(v, "orchestrator off") {
		t.Errorf("the orchestrator stays listed first, with no toggle for it:\n%s", v)
	}
	saveSettings(loadSettings())
	if b, _ := os.ReadFile(state.SettingsPath); strings.Contains(string(b), "hide_") {
		t.Errorf("saved: %s", b)
	}
}

func TestOrchestratorOfferNeedsADeliberateAnswer(t *testing.T) {
	newList := func(t *testing.T) model {
		orchestratorFixture(t)
		startsAs(t, orchestratorName)
		m := initialModel(buildItems([]transcript.Conversation{{SessionID: "a", Title: "other", LastTimestamp: time.Now().UTC().Format(time.RFC3339)}}), "", nil)
		m.width, m.height = 140, 40
		return m
	}
	offer := func(m model, opened bool) model {
		res, _ := m.Update(orchestratorOfferMsg{opened: opened})
		return res.(model)
	}
	t.Run("a key just as it appears answers nothing", func(t *testing.T) {
		m := offer(newList(t), false)
		for _, k := range []tea.KeyType{tea.KeyEnter, tea.KeyEsc} {
			var cmd tea.Cmd
			if m, cmd = key(m, tea.KeyMsg{Type: k}); m.orchOffer == nil || cmd != nil || loadOrchestrator().Offered {
				t.Errorf("%v answered it", k)
			}
		}
		m.orchSince = time.Now().Add(-updateKeyGrace)
		if m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.orchOffer != nil || cmd == nil {
			t.Error("after the grace, enter sets up")
		}
	})
	t.Run("ctrl+c still quits at once", func(t *testing.T) {
		if _, cmd := key(offer(newList(t), false), tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
			t.Error("no quit")
		}
	})
	t.Run("waits behind a rename, then gets a fresh grace", func(t *testing.T) {
		m := newList(t)
		m.renaming = true
		m = offer(m, false)
		if strings.Contains(strip2(m.View()), "Set up an orchestrator?") {
			t.Error("shown over the prompt")
		}
		m.orchSince = time.Now().Add(-updateKeyGrace)
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter}) // the prompt's Enter
		if m.orchOffer == nil || loadOrchestrator().Offered {
			t.Fatal("enter meant for the prompt answered it")
		}
		m.renaming = false
		if !strings.Contains(strip2(m.View()), "Set up an orchestrator?") {
			t.Error("shows once the prompt is done")
		}
		if m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.orchOffer == nil {
			t.Error("the first key starts its grace")
		}
	})
	t.Run("waits behind a typed search, unless asked for", func(t *testing.T) {
		m := newList(t)
		m.textInput.SetValue("abc")
		m = offer(m, false)
		if m.offerShown() || strings.Contains(strip2(m.View()), "Set up an orchestrator?") {
			t.Error("shown over the search")
		}
		if m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.orchOffer == nil || loadOrchestrator().Offered {
			t.Error("enter on the search answered it")
		}
		m.orchOffer = nil
		if m = offer(m, true); !m.offerShown() {
			t.Error("^T O shows it")
		}
	})
	t.Run("a late check doesn't reopen what's answered", func(t *testing.T) {
		m := newList(t)
		markOrchestratorAnswered()
		if m = offer(m, false); m.orchOffer != nil {
			t.Error("offered again")
		}
		m = newList(t)
		m.orchBusy = true
		if m = offer(m, false); m.orchOffer != nil || !m.orchBusy {
			t.Error("offered during set-up")
		}
	})
	t.Run("small screens still show the plan and the keys", func(t *testing.T) {
		m := offer(newList(t), false)
		m.width, m.height = 44, 14
		m.orchOffer.fixes = []mcpFix{{Bin: "claude", Args: []string{"mcp", "add", "--scope", "user", "ccs", "--", "ccs", "mcp"}}}
		v := strings.Join(strings.Fields(strip2(m.orchestratorPopup())), " ")
		for _, want := range []string{"Set up an orchestrator?", "--scope", "enter", "esc"} {
			if !strings.Contains(v, want) {
				t.Errorf("lacks %q: %s", want, v)
			}
		}
		if lines := strings.Count(m.orchestratorPopup(), "\n") + 1; lines > 14 {
			t.Errorf("%d lines in 14", lines)
		}
		for _, l := range strings.Split(strip2(m.orchestratorPopup()), "\n") {
			if w := lipgloss.Width(l); w > 44 {
				t.Errorf("%d wide: %q", w, l)
			}
		}
	})
	t.Run("what a failed set-up did stays on screen", func(t *testing.T) {
		m := newList(t)
		res, _ := m.Update(orchestratorDoneMsg{text: "ccs MCP server added to claude", err: errors.New("couldn't open a tab")})
		if got := res.(model).errorMsg; !strings.Contains(got, "added to claude") || !strings.Contains(got, "open a tab") {
			t.Error(got)
		}
	})
}

func TestOrchestratorPromptNoAndCtrlD(t *testing.T) {
	for name, in := range map[string]string{"n": "n\n", "ctrl+d": ""} {
		t.Run(name, func(t *testing.T) {
			orchestratorFixture(t)
			scripts := startsAs(t, orchestratorName)
			var out bytes.Buffer
			code := runOrchestrator([]string{"--no-mcp"}, strings.NewReader(in), &out, &out, true)
			if code != 1 || len(*scripts) > 0 || !strings.Contains(out.String(), "Nothing changed") {
				t.Errorf("%d %s %v", code, out.String(), *scripts)
			}
			if rec := loadOrchestrator(); !rec.Offered || rec.SessionID != "" {
				t.Errorf("declined for good, nothing else: %+v", rec)
			}
		})
	}
	t.Run("enter alone is yes", func(t *testing.T) {
		orchestratorFixture(t)
		scripts := startsAs(t, orchestratorName)
		var out bytes.Buffer
		if code := runOrchestrator([]string{"--no-mcp"}, strings.NewReader("\n"), &out, &out, true); code != 0 || len(*scripts) != 1 {
			t.Errorf("%d %s %v", code, out.String(), *scripts)
		}
	})
}

func TestOrchestratorPopupAndKey(t *testing.T) {
	orchestratorFixture(t)
	startsAs(t, orchestratorName)
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "a", Title: "other", LastTimestamp: time.Now().UTC().Format(time.RFC3339)}}), "", nil)
	m.width, m.height = 140, 40

	res, _ := m.Update(orchestratorOfferMsg{plan: orchestratorPlan{fixes: []mcpFix{{Bin: "claude", Args: []string{"mcp", "add", "--scope", "user", "ccs", "--", "ccs", "mcp"}}}}})
	m = res.(model)
	m.orchSince = time.Now().Add(-updateKeyGrace)
	v := strip2(m.View())
	for _, want := range []string{"Set up an orchestrator?", "claude mcp add --scope user ccs -- ccs mcp", "create ", "trust the folder"} {
		if !strings.Contains(v, want) {
			t.Errorf("popup lacks %q:\n%s", want, v)
		}
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.orchOffer != nil || !loadOrchestrator().Offered {
		t.Error("esc answers it for good")
	}

	m, cmd := chordT(m, "o")
	if !m.orchBusy || cmd == nil {
		t.Fatalf("^T O starts the look: busy=%v", m.orchBusy)
	}
	if m2, cmd2 := chordT(m, "o"); cmd2 != nil || !strings.Contains(m2.errorMsg, "Still") {
		t.Errorf("never twice at once: %q", m2.errorMsg)
	}
	res, _ = m.Update(cmd())
	m = res.(model)
	if m.orchOffer == nil {
		t.Fatal("^T O with none offers, whatever was answered before")
	}
	m.orchSince = time.Now().Add(-updateKeyGrace)
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.orchOffer != nil || cmd == nil {
		t.Fatal("enter sets up")
	}
	res, _ = m.Update(cmd())
	m = res.(model)
	if m.orchBusy || m.errorMsg != "" && !strings.Contains(m.errorMsg, "orchestrator started") {
		t.Errorf("done: busy=%v %q", m.orchBusy, m.errorMsg)
	}
	if loadOrchestrator().SessionID == "" {
		t.Error("nothing recorded")
	}
}

func TestOrchestratorProbeOffersOnlyWhenNoneAndUnanswered(t *testing.T) {
	orchestratorFixture(t)
	msg, ok := probeOrchestrator().(orchestratorOfferMsg)
	if !ok || len(msg.plan.fixes) != 1 {
		t.Errorf("clean state: offered with the MCP step, got %+v", msg)
	}
	markOrchestratorAnswered()
	if probeOrchestrator() != nil {
		t.Error("answered: not again")
	}
	rec := loadOrchestrator()
	rec.Offered = false
	rec.save()
	os.WriteFile(filepath.Join(sessions.GetSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"x","name":"orchestrator"}`), 0o600)
	if probeOrchestrator() != nil {
		t.Error("one exists: no offer")
	}
	t.Setenv("ITERM_SESSION_ID", "")
	os.Remove(filepath.Join(sessions.GetSessionsDir(), "7.json"))
	rec = loadOrchestrator()
	rec.SessionID = ""
	rec.save()
	if probeOrchestrator() != nil {
		t.Error("nowhere to open a tab: no offer")
	}
}

func TestOrchestratorPopupOverAnEmptyList(t *testing.T) {
	orchestratorFixture(t)
	if !orchestratorOfferable() {
		t.Fatal("clean state, tabs and claude: offerable")
	}
	m := initialModel(nil, "", nil)
	m.width, m.height = 120, 40
	res, _ := m.Update(orchestratorOfferMsg{})
	m = res.(model)
	if v := strip2(m.View()); !strings.Contains(v, "Set up an orchestrator?") {
		t.Errorf("a brand-new user sees the offer:\n%s", v)
	}
	m.orchSince = time.Now().Add(-updateKeyGrace)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	for _, k := range []tea.KeyMsg{{Type: tea.KeyDown}, {Type: tea.KeyEnter}, {Type: tea.KeyUp}, {Type: tea.KeyCtrlF}, {Type: tea.KeyCtrlD}, {Type: tea.KeyTab}} {
		m, _ = key(m, k)
		m.View()
	}
	if orchestratorOfferable() {
		t.Error("answered: no more")
	}
}

func TestNoAutomaticRestartDuringOrchestratorSetUp(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.blurred = true
	if !m.awayForUpdate() {
		t.Fatal("away, nothing going on")
	}
	m.orchBusy = true
	if m.awayForUpdate() {
		t.Error("restarting now loses what the set-up says when it's done")
	}
}

func TestFirstRunRefreshIsNoFailure(t *testing.T) {
	orchestratorFixture(t)
	m := initialModel(nil, "", nil)
	res, _ := m.Update(refreshMsg{err: fmt.Errorf("scan: %w", fs.ErrNotExist)})
	if res.(model).refreshFailed {
		t.Error("no folder yet, nothing to list: not a failure")
	}
	m = initialModel(buildItems([]transcript.Conversation{{SessionID: "a", Title: "x"}}), "", nil)
	if res, _ := m.Update(refreshMsg{err: fmt.Errorf("scan: %w", fs.ErrNotExist)}); !res.(model).refreshFailed {
		t.Error("a folder that vanished under a list still is")
	}
	m = initialModel(nil, "", nil)
	if res, _ := m.Update(refreshMsg{err: errors.New("disk on fire")}); !res.(model).refreshFailed {
		t.Error("any other error still is")
	}
}

func TestEnterOnAnEmptyListWaitsForTheSetUp(t *testing.T) {
	orchestratorFixture(t)
	m := initialModel(nil, "", nil)
	m.width, m.height = 120, 40
	m.orchBusy = true
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.quitting || cmd != nil {
		t.Error("a second Enter mustn't quit while it's setting up")
	}
	m.orchBusy = false
	if m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter}); !m.quitting {
		t.Error("otherwise Enter on an empty list still leaves")
	}
}

func TestDevNullIsNotSomeoneToAsk(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("stdin from /dev/null can't answer a question, so setup must not go ahead")
	}
}

func TestMCPCommandsCaptureStderr(t *testing.T) {
	orchestratorFixture(t)
	bin := t.TempDir()
	script := filepath.Join(bin, "fake-mcp")
	os.WriteFile(script, []byte("#!/bin/sh\necho 'No MCP server named ccs' >&2\nexit 1\n"), 0o700)
	out, err := proc.RunCommand(5*time.Second, nil, true, script)
	if err == nil || !strings.Contains(string(out), "No MCP server") {
		t.Errorf("how claude says it's missing: %v %q", err, out)
	}
}

func TestNoZshChangesNothing(t *testing.T) {
	ran := orchestratorFixture(t)
	scripts := startsAs(t, orchestratorName)
	old := sessions.CheckLaunchShell
	sessions.CheckLaunchShell = func() error { return errors.New("sessions are started with zsh, and there's none") }
	defer func() { sessions.CheckLaunchShell = old }()
	out, code := runOrch("--yes")
	if code != 1 || !strings.Contains(out, "zsh") || len(*scripts) > 0 {
		t.Errorf("%d %s %v", code, out, *scripts)
	}
	if slices.ContainsFunc(*ran, func(c string) bool { return strings.Contains(c, "mcp add") }) {
		t.Errorf("changed config: %v", *ran)
	}
	if _, err := os.Stat(orchestratorDir()); err == nil {
		t.Error("wrote its folder")
	}
	if loadOrchestrator().SessionID != "" {
		t.Error("recorded a session")
	}
}

func TestOrchestratorHeaderOnlyWhenItSaysSomething(t *testing.T) {
	orchestratorFixture(t)
	at := time.Now().UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		title  string
		header bool
	}{{orchestratorName, false}, {"triage everything", true}} {
		m := initialModel(buildItems([]transcript.Conversation{
			{SessionID: "b", Title: "another", LastTimestamp: at, Messages: []transcript.Message{{Role: "user", Text: "x"}}},
			{SessionID: "c", Title: tc.title, LastTimestamp: at, Messages: []transcript.Message{{Role: "user", Text: "y"}}},
		}), "", nil)
		m.width, m.height = 140, 30
		orchestratorRecord{SessionID: "c"}.save()
		m.syncOrchestrator()
		rows := m.listRows()
		if got := rows[0].item == -1; got != tc.header {
			t.Errorf("%q: header first %v, want %v", tc.title, got, tc.header)
		}
		if v := strip2(m.View()); strings.Contains(v, "orchestrator 1") != tc.header {
			t.Errorf("%q:\n%s", tc.title, v)
		}
	}
}

// legacyRecord writes coordinator.json as a ccs from before the rename did.
func legacyRecord(t *testing.T, body string) string {
	t.Helper()
	path := state.LegacyOrchestratorPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOrchestratorCommandKeepsItsOldName(t *testing.T) {
	for arg, want := range map[string]bool{"orchestrator": true, "coordinator": true, "orchestrator-report": false, "launch": false} {
		if isOrchestratorCmd(arg) != want {
			t.Errorf("%s: %v", arg, !want)
		}
	}
}

func TestOrchestratorRecordMigratesFromCoordinator(t *testing.T) {
	orchestratorFixture(t)
	old := legacyRecord(t, `{"session_id":"the-id","offered":true}`)
	rec := loadOrchestrator()
	if rec.SessionID != "the-id" || rec.Name != "coordinator" || !rec.Offered || rec.name() != "coordinator" {
		t.Fatalf("migrated: %+v", rec)
	}
	b, err := os.ReadFile(orchestratorPath())
	if err != nil || !strings.Contains(string(b), `"the-id"`) || !strings.Contains(string(b), `"coordinator"`) {
		t.Fatalf("orchestrator.json: %s %v", b, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("coordinator.json stays for a ccs still on the old version:", err)
	}
	// Once: orchestrator.json wins over anything an old ccs writes later.
	legacyRecord(t, `{"session_id":"other","offered":true}`)
	if rec := loadOrchestrator(); rec.SessionID != "the-id" {
		t.Errorf("migrated twice: %+v", rec)
	}
}

func TestOrchestratorRecordMigrationKeepsAGivenName(t *testing.T) {
	orchestratorFixture(t)
	legacyRecord(t, `{"session_id":"the-id","name":"my-hub","offered":true}`)
	if rec := loadOrchestrator(); rec.SessionID != "the-id" || rec.Name != "my-hub" {
		t.Errorf("%+v", rec)
	}
}

func TestOrchestratorReleasedRecordMigratesUnnamed(t *testing.T) {
	orchestratorFixture(t)
	legacyRecord(t, `{"offered":true,"released":true}`)
	if rec := loadOrchestrator(); !rec.Released || rec.Name != "" {
		t.Errorf("%+v", rec)
	}
	if s := findOrchestrator(); s.kind != orchNone {
		t.Errorf("released stays released: %+v", s)
	}
}

// The running coordinator, recorded before the rename, is still found after
// a /clear by the name it had, and once renamed the record follows its name.
func TestOrchestratorFollowsTheCoordinatorSession(t *testing.T) {
	orchestratorFixture(t)
	legacyRecord(t, `{"session_id":"before-clear","offered":true}`)
	live := filepath.Join(sessions.GetSessionsDir(), "7.json")
	os.WriteFile(live, []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"coordinator"}`), 0o600)
	if s := findOrchestrator(); s.kind != orchLive || s.rec.SessionID != "after-clear" || s.rec.name() != "coordinator" {
		t.Fatalf("after /clear: %+v", s)
	}
	os.WriteFile(live, []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"orchestrator"}`), 0o600)
	if s := findOrchestrator(); s.kind != orchLive || s.rec.Name != "" {
		t.Fatalf("after /rename orchestrator: %+v", s)
	}
	if rec := loadOrchestrator(); rec.SessionID != "after-clear" || rec.Name != "" {
		t.Errorf("saved: %+v", rec)
	}
	os.WriteFile(live, []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"hub"}`), 0o600)
	if s := findOrchestrator(); s.rec.Name != "hub" || loadOrchestrator().Name != "hub" {
		t.Errorf("after /rename hub: %+v", s.rec)
	}
}

func TestFollowNameIgnoresNamesItCantRelaunch(t *testing.T) {
	rec := orchestratorRecord{SessionID: "x", Name: "coordinator"}
	if followName(&rec, "") || followName(&rec, "coordinator") || followName(&rec, "has spaces") || rec.Name != "coordinator" {
		t.Errorf("%+v", rec)
	}
}

// A coordinator folder is a running session's cwd: used where it is, never
// moved, and never joined by a second orchestrator folder.
func TestOrchestratorKeepsTheCoordinatorFolder(t *testing.T) {
	orchestratorFixture(t)
	base := filepath.Dir(state.SettingsPath)
	if got := orchestratorDir(); got != filepath.Join(base, "orchestrator") {
		t.Errorf("fresh: %s", got)
	}
	old := filepath.Join(base, "coordinator")
	os.MkdirAll(old, 0o700)
	os.WriteFile(filepath.Join(old, "CLAUDE.md"), []byte("my role"), 0o600)
	if got := orchestratorDir(); got != old {
		t.Fatalf("existing coordinator folder: %s", got)
	}
	if err := writeOrchestratorFolder(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(old, "CLAUDE.md")); string(b) != "my role" {
		t.Errorf("role overwritten: %s", b)
	}
	if _, err := os.Stat(filepath.Join(base, "orchestrator")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a second folder: %v", err)
	}
	if p := (orchestratorPlan{}).lines(); len(p) == 0 || !strings.HasSuffix(p[0], "coordinator") || !strings.HasPrefix(p[0], "use ") {
		t.Errorf("plan: %v", p)
	}
	os.MkdirAll(filepath.Join(base, "orchestrator"), 0o700)
	if got := orchestratorDir(); got != filepath.Join(base, "orchestrator") {
		t.Errorf("both: %s", got)
	}
}

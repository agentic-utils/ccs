package main

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
)

// coordinatorFixture is a clean state: no record, no folder, no sessions, tabs
// stubbed, and claude reporting no ccs MCP server until it's added. The
// returned slice collects each mcp command run.
func coordinatorFixture(t *testing.T) (ran *[]string) {
	t.Helper()
	dir := t.TempDir()
	oldSettings, oldMarks, oldProjects, oldRun := settingsPath, marksPath, getProjectsDir, mcpRun
	settingsPath = filepath.Join(dir, "ccs", "settings.json")
	marksPath = filepath.Join(dir, "ccs", "sessions.json")
	getProjectsDir = func() string { return filepath.Join(dir, "projects") }
	sessionsFixture(t)
	stubLaunch(t)
	oldBin := lookBin
	lookBin = func(name string) (string, error) {
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
		settingsPath, marksPath, getProjectsDir, mcpRun, lookBin = oldSettings, oldMarks, oldProjects, oldRun, oldBin
	})
	return ran
}

// startsAs makes a launched tab register a live session called name, as claude
// does once started.
func startsAs(t *testing.T, name string) (scripts *[]string) {
	t.Helper()
	scripts = &[]string{}
	old := openLaunchTab
	openLaunchTab = func(script string) (string, error) {
		*scripts = append(*scripts, script)
		os.WriteFile(filepath.Join(getSessionsDir(), "1.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"started","name":"`+name+`"}`), 0o600)
		return "/dev/ttys9", nil
	}
	t.Cleanup(func() { openLaunchTab = old })
	return scripts
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func runCoord(args ...string) (string, int) {
	var out bytes.Buffer
	code := runCoordinator(args, strings.NewReader(""), &out, &out, false)
	return out.String(), code
}

func TestCoordinatorSetupFromCleanState(t *testing.T) {
	ran := coordinatorFixture(t)
	scripts := startsAs(t, coordinatorName)
	var focused string
	oldFocus := focusTTY
	focusTTY = func(tty string, _ bool) bool { focused = tty; return true }
	defer func() { focusTTY = oldFocus }()

	out, code := runCoord("--yes")
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
	role, err := os.ReadFile(filepath.Join(coordinatorDir(), "CLAUDE.md"))
	if err != nil || !strings.Contains(string(role), "never do the work yourself") {
		t.Errorf("role: %v %s", err, role)
	}
	if strings.Contains(string(role), "{{events}}") || !strings.Contains(string(role), prEventsPath()) {
		t.Errorf("the events path isn't filled in:\n%s", role)
	}
	var cfg struct {
		Permissions struct{ Allow []string }
	}
	raw, _ := os.ReadFile(filepath.Join(coordinatorDir(), ".claude", "settings.json"))
	if err := json.Unmarshal(raw, &cfg); err != nil || !slices.Contains(cfg.Permissions.Allow, "Bash(ccs launch:*)") {
		t.Errorf("settings: %v %s", err, raw)
	}
	rec := loadCoordinator()
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(rec.SessionID) || !rec.Offered {
		t.Errorf("record: %+v", rec)
	}
	script, _ := os.ReadFile((*scripts)[0])
	if !strings.Contains(string(script), "--session-id '"+rec.SessionID+"'") || !strings.Contains(string(script), "--name 'coordinator'") {
		t.Errorf("launcher:\n%s", script)
	}
}

func TestCoordinatorAsksBeforeChangingAnything(t *testing.T) {
	ran := coordinatorFixture(t)
	scripts := startsAs(t, coordinatorName)

	out, code := runCoord()
	if code == 0 || !strings.Contains(out, "Nothing changed") || !strings.Contains(out, "claude mcp add") {
		t.Errorf("not a terminal, no --yes: shows the plan and stops, got %d:\n%s", code, out)
	}
	var b bytes.Buffer
	if code := runCoordinator(nil, strings.NewReader("n\n"), &b, &b, true); code == 0 || !strings.Contains(b.String(), "Nothing changed") {
		t.Errorf("answered no: %d %s", code, b.String())
	}
	if slices.ContainsFunc(*ran, func(c string) bool { return strings.Contains(c, "mcp add") }) || len(*scripts) > 0 {
		t.Errorf("declined, yet ran %v and opened %v", *ran, *scripts)
	}
	if _, err := os.Stat(coordinatorDir()); err == nil {
		t.Error("declined, yet its folder exists")
	}
	if loadCoordinator().SessionID != "" {
		t.Error("declined, yet recorded")
	}
}

func TestCoordinatorMCPAlreadyInstalledIsLeftAlone(t *testing.T) {
	coordinatorFixture(t)
	oldBin, oldRun := lookBin, mcpRun
	lookBin = func(name string) (string, error) { return "/bin/" + name, nil }
	var ran []string
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		ran = append(ran, bin+" "+strings.Join(args, " "))
		if bin == "codex" {
			return []byte(`[{"name":"other"},{"name":"ccs"}]`), nil
		}
		return []byte("Status: ✔ Connected"), nil
	}
	defer func() { lookBin, mcpRun = oldBin, oldRun }()
	p := planCoordinator(false)
	if len(p.fixes) != 0 || len(p.notes) != 0 || slices.ContainsFunc(p.lines(), func(l string) bool { return strings.Contains(l, "mcp") }) {
		t.Errorf("nothing to do, nothing to say: %+v %v", p, p.lines())
	}
	if slices.ContainsFunc(ran, func(c string) bool { return strings.Contains(c, "mcp add") }) {
		t.Errorf("added over an existing one: %v", ran)
	}
}

func TestCoordinatorMCPBrokenIsSaidAndLeftAlone(t *testing.T) {
	ran := coordinatorFixture(t)
	old := mcpRun
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		*ran = append(*ran, strings.Join(args, " "))
		return []byte("ccs:\n  Scope: User config\n  Status: ✘ Failed to connect\n"), nil
	}
	defer func() { mcpRun = old }()
	p := planCoordinator(false)
	if len(p.fixes) != 0 || len(p.notes) != 1 || !strings.Contains(p.notes[0], "isn't connected") {
		t.Errorf("%+v", p)
	}
}

func TestCoordinatorMCPAddedMeanwhileIsNoFailure(t *testing.T) {
	coordinatorFixture(t)
	old := mcpRun
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		if args[1] == "add" {
			return []byte("MCP server ccs already exists in user config"), errors.New("exit status 1")
		}
		return []byte("Status: ✔ Connected"), nil
	}
	defer func() { mcpRun = old }()
	done, errs := applyMCP([]mcpFix{{"claude", []string{"mcp", "add", "ccs"}}})
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

func TestCoordinatorMCPCodexMissingOnly(t *testing.T) {
	coordinatorFixture(t)
	oldBin, oldRun := lookBin, mcpRun
	lookBin = func(name string) (string, error) { return "/bin/" + name, nil }
	mcpRun = func(bin string, args ...string) ([]byte, error) {
		if bin == "codex" {
			return []byte(`[{"name":"other"}]`), nil
		}
		return []byte("Connected"), nil
	}
	defer func() { lookBin, mcpRun = oldBin, oldRun }()
	p := planCoordinator(false)
	if len(p.fixes) != 1 || !strings.HasPrefix(p.fixes[0].String(), "codex mcp add ccs --") {
		t.Errorf("%+v", p)
	}
}

func TestCoordinatorMCPUnknownIsNotInstalled(t *testing.T) {
	coordinatorFixture(t)
	oldRun := mcpRun
	mcpRun = func(string, ...string) ([]byte, error) { return []byte("boom"), errors.New("timed out") }
	defer func() { mcpRun = oldRun }()
	p := planCoordinator(false)
	if len(p.fixes) != 0 || len(p.notes) != 1 {
		t.Errorf("can't tell: never install, say so: %+v", p)
	}
}

func TestCoordinatorMCPFailureIsReported(t *testing.T) {
	coordinatorFixture(t)
	oldRun := mcpRun
	defer func() { mcpRun = oldRun }()
	mcpRun = func(string, ...string) ([]byte, error) { return []byte("Status: ✗ Failed"), nil }
	done, errs := applyMCP([]mcpFix{{"claude", []string{"mcp", "add"}}})
	if len(done) != 0 || len(errs) != 1 || !strings.Contains(errs[0], "connected") {
		t.Errorf("added but not connected must say so: %v %v", done, errs)
	}
	mcpRun = func(string, ...string) ([]byte, error) { return []byte("nope"), errors.New("exit 1") }
	if _, errs := applyMCP([]mcpFix{{"claude", []string{"mcp", "add"}}}); len(errs) != 1 || !strings.Contains(errs[0], "nope") {
		t.Errorf("%v", errs)
	}
}

func TestCoordinatorNeedsClaude(t *testing.T) {
	coordinatorFixture(t)
	old := lookBin
	lookBin = func(string) (string, error) { return "", errors.New("no") }
	defer func() { lookBin = old }()
	out, code := runCoord("--yes")
	if code == 0 || !strings.Contains(out, "needs Claude Code") {
		t.Errorf("%d %s", code, out)
	}
}

func TestCoordinatorNeverMakesASecond(t *testing.T) {
	t.Run("recorded and running", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		coordinatorRecord{SessionID: "the-id"}.save()
		os.WriteFile(filepath.Join(getSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"the-id","name":"renamed"}`), 0o600)
		var pid int
		old := focusPID
		focusPID = func(p int, _ bool) bool { pid = p; return true }
		defer func() { focusPID = old }()
		out, code := runCoord("--yes")
		if code != 0 || !strings.Contains(out, "Switched") || pid != os.Getpid() || len(*scripts) > 0 {
			t.Errorf("%d %s pid=%d scripts=%v", code, out, pid, *scripts)
		}
	})
	t.Run("running under its name, id changed", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		coordinatorRecord{SessionID: "before-clear"}.save()
		os.WriteFile(filepath.Join(getSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"coordinator"}`), 0o600)
		old := focusPID
		focusPID = func(int, bool) bool { return true }
		defer func() { focusPID = old }()
		if _, code := runCoord("--yes"); code != 0 || len(*scripts) > 0 {
			t.Errorf("%d %v", code, *scripts)
		}
		if got := loadCoordinator().SessionID; got != "after-clear" {
			t.Errorf("record follows it: %s", got)
		}
	})
	t.Run("started, trust question unanswered", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		coordinatorRecord{SessionID: "the-id"}.save()
		oldPs, oldFocus := psAll, focusPID
		psAll = func() (string, error) {
			return "4242 ttys1 /bin/claude hello --session-id the-id --name coordinator\n", nil
		}
		var pid int
		focusPID = func(p int, _ bool) bool { pid = p; return true }
		defer func() { psAll, focusPID = oldPs, oldFocus }()
		out, code := runCoord("--yes")
		if code != 0 || pid != 4242 || !strings.Contains(out, "trust") || len(*scripts) > 0 {
			t.Errorf("%d %s pid=%d %v", code, out, pid, *scripts)
		}
	})
	t.Run("being resumed, no session file yet", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		coordinatorRecord{SessionID: "the-id"}.save()
		proj := filepath.Join(getProjectsDir(), "-w")
		os.MkdirAll(proj, 0o700)
		os.WriteFile(filepath.Join(proj, "the-id.jsonl"), []byte(`{"type":"user","cwd":"/tmp","sessionId":"the-id","message":{"role":"user","content":"hi"}}`+"\n"), 0o600)
		oldPs, oldFocus := psAll, focusPID
		psAll = func() (string, error) {
			return "4242 ttys1 /bin/claude --resume the-id --name coordinator\n", nil
		}
		var pid int
		focusPID = func(p int, _ bool) bool { pid = p; return true }
		defer func() { psAll, focusPID = oldPs, oldFocus }()
		if out, code := runCoord("--yes"); code != 0 || pid != 4242 || len(*scripts) > 0 {
			t.Errorf("%d %s pid=%d %v", code, out, pid, *scripts)
		}
	})
	t.Run("a ccs command naming the id is no claude", func(t *testing.T) {
		coordinatorFixture(t)
		old := psAll
		psAll = func() (string, error) {
			return "4242 ttys1 /opt/homebrew/bin/ccs launch --name x --resume the-id\n", nil
		}
		defer func() { psAll = old }()
		if pid := startingProcess("the-id"); pid != 0 {
			t.Errorf("pid %d", pid)
		}
	})
	t.Run("cleared: its arguments still name the old id, as claude's do", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		coordinatorRecord{SessionID: "before-clear"}.save()
		os.WriteFile(filepath.Join(getSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"coordinator"}`), 0o600)
		oldPs, oldFocus := psAll, focusPID
		psAll = func() (string, error) {
			return itoa(os.Getpid()) + " ttys1 /bin/claude hello --session-id before-clear --name coordinator\n", nil
		}
		focusPID = func(int, bool) bool { return true }
		defer func() { psAll, focusPID = oldPs, oldFocus }()
		out, code := runCoord("--yes")
		if code != 0 || strings.Contains(out, "trust") || len(*scripts) > 0 || loadCoordinator().SessionID != "after-clear" {
			t.Errorf("%d %s %v %s", code, out, *scripts, loadCoordinator().SessionID)
		}
	})
	t.Run("cleared and renamed: the process says which it is", func(t *testing.T) {
		coordinatorFixture(t)
		coordinatorRecord{SessionID: "before-clear"}.save()
		os.WriteFile(filepath.Join(getSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"after-clear","name":"hub"}`), 0o600)
		old := psAll
		psAll = func() (string, error) {
			return itoa(os.Getpid()) + " ttys1 /bin/claude hello --session-id before-clear --name coordinator\n", nil
		}
		defer func() { psAll = old }()
		if s := findCoordinator(); s.kind != coordLive || s.rec.SessionID != "after-clear" {
			t.Errorf("%+v", s)
		}
	})
	t.Run("an offer left open opens the one started since", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		p := planCoordinator(false) // both ccs windows made their offers here
		coordinatorRecord{SessionID: "the-id"}.save()
		oldPs, oldFocus := psAll, focusPID
		psAll = func() (string, error) {
			return "4242 ttys1 /bin/claude hello --session-id the-id --name coordinator\n", nil
		}
		var pid int
		focusPID = func(p int, _ bool) bool { pid = p; return true }
		defer func() { psAll, focusPID = oldPs, oldFocus }()
		msg := coordinatorSetupCmd(p)().(coordinatorDoneMsg)
		if msg.err != nil || pid != 4242 || len(*scripts) > 0 || loadCoordinator().SessionID != "the-id" {
			t.Errorf("%+v pid=%d %v %s", msg, pid, *scripts, loadCoordinator().SessionID)
		}
	})
	t.Run("another ccs has the turn", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		release, _, err := coordinatorTurn()
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		out, code := runCoord("--yes")
		if code != 1 || !strings.Contains(out, "another ccs") || len(*scripts) > 0 {
			t.Errorf("%d %s %v", code, out, *scripts)
		}
		if _, err := setupCoordinator(coordinatorPlan{}, false); !errors.Is(err, errCoordinatorBusy) || len(*scripts) > 0 {
			t.Errorf("%v %v", err, *scripts)
		}
		// One that's already up can still be switched to.
		coordinatorRecord{SessionID: "the-id"}.save()
		os.WriteFile(filepath.Join(getSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"the-id","name":"coordinator"}`), 0o600)
		old := focusPID
		focusPID = func(int, bool) bool { return true }
		defer func() { focusPID = old }()
		if out, code := runCoord("--yes"); code != 0 || !strings.Contains(out, "Switched") {
			t.Errorf("%d %s", code, out)
		}
	})
	t.Run("named transcript, not running", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		proj := filepath.Join(getProjectsDir(), "-w")
		os.MkdirAll(proj, 0o700)
		os.WriteFile(filepath.Join(proj, "old-id.jsonl"), []byte(`{"type":"user","cwd":"/tmp","sessionId":"old-id","message":{"role":"user","content":"hi"}}`+"\n"), 0o600)
		old := namedTranscript
		namedTranscript = func(n string) string {
			if n == coordinatorName {
				return "old-id"
			}
			return ""
		}
		defer func() { namedTranscript = old }()
		out, code := runCoord("--yes")
		if code != 0 || !strings.Contains(out, "Resumed") || len(*scripts) != 1 {
			t.Fatalf("%d %s %v", code, out, *scripts)
		}
		script, _ := os.ReadFile((*scripts)[0])
		if !strings.Contains(string(script), "--resume 'old-id'") || strings.Contains(string(script), "--session-id") || strings.Contains(string(script), "cat ") {
			t.Errorf("a resume, no brief, no new id:\n%s", script)
		}
	})
}

func TestCoordinatorListFollowsClear(t *testing.T) {
	coordinatorFixture(t)
	coordinatorRecord{SessionID: "before-clear", Offered: true}.save()
	m := initialModel(buildItems([]Conversation{{SessionID: "before-clear", Title: "coordinator"}, {SessionID: "after-clear", Title: "coordinator"}}), "", nil)
	m.coordID = "before-clear"
	res, _ := m.Update(liveMsg{pids: map[string]int{"after-clear": 7}, scan: map[string]liveSession{"after-clear": {pid: 7, stat: sessionStat{name: "coordinator"}}}})
	m = res.(model)
	if m.coordID != "after-clear" {
		t.Errorf("the list follows the session: %s", m.coordID)
	}
	if rec := loadCoordinator(); rec.SessionID != "after-clear" || !rec.Offered {
		t.Errorf("record: %+v", rec)
	}
	// Two with its name: never guess, and a closed coordinator stays recorded.
	coordinatorRecord{SessionID: "before-clear"}.save()
	two := map[string]liveSession{"x": {pid: 7, stat: sessionStat{name: "coordinator"}}, "y": {pid: 8, stat: sessionStat{name: "coordinator"}}}
	rec := loadCoordinator()
	if followLive(&rec, two) || followLive(&rec, map[string]liveSession{}) || rec.SessionID != "before-clear" {
		t.Errorf("%+v", rec)
	}
}

func TestCoordinatorRecordSavesLeaveNoTempFiles(t *testing.T) {
	coordinatorFixture(t)
	for range 3 {
		if err := (coordinatorRecord{SessionID: "x"}).save(); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(coordinatorPath()), "*.tmp")); len(left) != 0 {
		t.Error(left)
	}
}

func TestCoordinatorComesBackInItsOwnFolder(t *testing.T) {
	coordinatorFixture(t)
	scripts := startsAs(t, coordinatorName)
	coordinatorRecord{SessionID: "the-id"}.save()
	proj := filepath.Join(getProjectsDir(), "-w")
	os.MkdirAll(proj, 0o700)
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(proj, "the-id.jsonl"), []byte(`{"type":"user","cwd":"`+cwd+`","sessionId":"the-id","message":{"role":"user","content":"hi"}}`+"\n"), 0o600)
	if out, code := runCoord(); code != 0 { // no --yes: a resume changes nothing, so it asks nothing
		t.Fatalf("%d %s", code, out)
	}
	script, _ := os.ReadFile((*scripts)[0])
	if !strings.Contains(string(script), "cd '"+cwd+"'") || !strings.Contains(string(script), "--resume 'the-id'") {
		t.Errorf("%s", script)
	}
}

func TestCoordinatorRecreatedAfterItsTranscriptIsGone(t *testing.T) {
	coordinatorFixture(t)
	scripts := startsAs(t, coordinatorName)
	coordinatorRecord{SessionID: "deleted", Offered: true}.save()
	if out, code := runCoord("--yes"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	script, _ := os.ReadFile((*scripts)[0])
	if strings.Contains(string(script), "--resume") || !strings.Contains(string(script), "--session-id") {
		t.Errorf("%s", script)
	}
}

func TestCoordinatorKeepsTheUsersEdits(t *testing.T) {
	coordinatorFixture(t)
	dir := coordinatorDir()
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine"), 0o600)
	if err := writeCoordinatorFolder(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(b) != "mine" {
		t.Errorf("overwrote it: %s", b)
	}
}

func TestCoordinatorAdopt(t *testing.T) {
	coordinatorFixture(t)
	os.WriteFile(filepath.Join(getSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"their-id","name":"hub"}`), 0o600)
	out, code := runCoord("--adopt", "hub")
	if code != 0 || !strings.Contains(out, "now the coordinator") {
		t.Fatalf("%d %s", code, out)
	}
	if r := loadCoordinator(); r.SessionID != "their-id" || r.Name != "hub" || !r.Offered {
		t.Errorf("%+v", r)
	}
	if s := findCoordinator(); s.kind != coordLive {
		t.Errorf("%+v", s)
	}
	if out, code := runCoord("--adopt", "nobody"); code == 0 || !strings.Contains(out, "no Claude Code session") {
		t.Errorf("%d %s", code, out)
	}
}

func TestCoordinatorAdoptOnlyWhatItCanBringBack(t *testing.T) {
	session := func(t *testing.T, id, title string) {
		t.Helper()
		proj := filepath.Join(getProjectsDir(), "-w")
		os.MkdirAll(proj, 0o700)
		os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(`{"type":"user","cwd":"/tmp","sessionId":"`+id+`","timestamp":"2026-11-04T10:00:00Z","message":{"role":"user","content":"hi"}}`+"\n"+`{"type":"custom-title","customTitle":"`+title+`","sessionId":"`+id+`"}`+"\n"), 0o600)
	}
	t.Run("two running with the name: never a guess", func(t *testing.T) {
		coordinatorFixture(t)
		for i, id := range []string{"one", "two"} {
			os.WriteFile(filepath.Join(getSessionsDir(), itoa(i)+".json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"`+id+`","name":"hub"}`), 0o600)
		}
		out, code := runCoord("--adopt", "hub")
		if code != 1 || !strings.Contains(out, "2 running sessions") || loadCoordinator().SessionID != "" {
			t.Errorf("%d %s %+v", code, out, loadCoordinator())
		}
		if out, code := runCoord("--adopt", "two"); code != 0 || loadCoordinator().SessionID != "two" {
			t.Errorf("by id: %d %s", code, out)
		}
	})
	t.Run("a name launch can't take is refused now, not when reopening", func(t *testing.T) {
		coordinatorFixture(t)
		os.WriteFile(filepath.Join(getSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"their-id","name":"my hub"}`), 0o600)
		out, code := runCoord("--adopt", "my hub")
		if code != 1 || !strings.Contains(out, "rename it") || loadCoordinator().SessionID != "" {
			t.Errorf("%d %s %+v", code, out, loadCoordinator())
		}
	})
	t.Run("by id, closed: it keeps its name", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, "hub")
		session(t, "their-id", "hub")
		if out, code := runCoord("--adopt", "their-id"); code != 0 || !strings.Contains(out, "hub (their-id)") {
			t.Fatalf("%d %s", code, out)
		}
		if r := loadCoordinator(); r.Name != "hub" {
			t.Errorf("%+v", r)
		}
		if out, code := runCoord(); code != 0 || !strings.Contains(out, "Resumed hub") {
			t.Fatalf("%d %s", code, out)
		}
		script, _ := os.ReadFile((*scripts)[0])
		if !strings.Contains(string(script), "--name 'hub'") {
			t.Errorf("renamed it:\n%s", script)
		}
	})
	t.Run("by id, closed, with a name launch can't take", func(t *testing.T) {
		coordinatorFixture(t)
		session(t, "their-id", "my hub")
		if out, code := runCoord("--adopt", "their-id"); code != 1 || !strings.Contains(out, "rename it") {
			t.Errorf("%d %s", code, out)
		}
	})
	t.Run("says when it replaces one", func(t *testing.T) {
		coordinatorFixture(t)
		coordinatorRecord{SessionID: "old"}.save()
		session(t, "their-id", "hub")
		if out, _ := runCoord("--adopt", "their-id"); !strings.Contains(out, "stays as an ordinary session") {
			t.Error(out)
		}
	})
}

func TestCoordinatorInPlaceOutsideTabs(t *testing.T) {
	coordinatorFixture(t)
	t.Setenv("ITERM_SESSION_ID", "")
	var ran string
	old := execScript
	execScript = func(s string) error { ran = s; return nil }
	defer func() { execScript = old }()
	if out, code := runCoord("--yes"); code != 0 || !strings.HasSuffix(ran, "launch.sh") {
		t.Errorf("%d %s %q", code, out, ran)
	}
}

func TestCoordinatorHasItsOwnSectionAbovePinned(t *testing.T) {
	coordinatorFixture(t)
	now := time.Now().UTC()
	at := now.Format(time.RFC3339)
	convs := []Conversation{
		{SessionID: "a", Title: "a pinned one", LastTimestamp: at, Messages: []Message{{Role: "user", Text: "x"}}},
		{SessionID: "b", Title: "another", LastTimestamp: at, Messages: []Message{{Role: "user", Text: "x"}}},
		{SessionID: "c", Title: coordinatorName, LastTimestamp: now.Add(-time.Hour).Format(time.RFC3339), Messages: []Message{{Role: "user", Text: "y"}}},
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 140, 30
	m.marks = &sessionMarks{Since: now, Seen: map[string]time.Time{}, Pinned: []string{"a"}}
	m.sortList()
	if strings.Contains(strip2(m.View()), "coordinator 1") {
		t.Fatal("no record: no section")
	}
	coordinatorRecord{SessionID: "c"}.save()
	m.syncCoordinator()
	if m.filtered[0].conv.SessionID != "c" || m.filtered[1].conv.SessionID != "a" {
		t.Fatalf("coordinator, then pinned: %s %s", m.filtered[0].conv.SessionID, m.filtered[1].conv.SessionID)
	}
	v := strip2(m.View())
	co, pin := strings.Index(v, "coordinator 1"), strings.Index(v, "pinned 1")
	if co < 0 || pin < 0 || co > pin {
		t.Errorf("section order:\n%s", v)
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

func TestCoordinatorOfferNeedsADeliberateAnswer(t *testing.T) {
	newList := func(t *testing.T) model {
		coordinatorFixture(t)
		startsAs(t, coordinatorName)
		m := initialModel(buildItems([]Conversation{{SessionID: "a", Title: "other", LastTimestamp: time.Now().UTC().Format(time.RFC3339)}}), "", nil)
		m.width, m.height = 140, 40
		return m
	}
	offer := func(m model, opened bool) model {
		res, _ := m.Update(coordinatorOfferMsg{opened: opened})
		return res.(model)
	}
	t.Run("a key just as it appears answers nothing", func(t *testing.T) {
		m := offer(newList(t), false)
		for _, k := range []tea.KeyType{tea.KeyEnter, tea.KeyEsc} {
			var cmd tea.Cmd
			if m, cmd = key(m, tea.KeyMsg{Type: k}); m.coordOffer == nil || cmd != nil || loadCoordinator().Offered {
				t.Errorf("%v answered it", k)
			}
		}
		m.coordSince = time.Now().Add(-updateKeyGrace)
		if m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.coordOffer != nil || cmd == nil {
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
		if strings.Contains(strip2(m.View()), "Set up a coordinator?") {
			t.Error("shown over the prompt")
		}
		m.coordSince = time.Now().Add(-updateKeyGrace)
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter}) // the prompt's Enter
		if m.coordOffer == nil || loadCoordinator().Offered {
			t.Fatal("enter meant for the prompt answered it")
		}
		m.renaming = false
		if !strings.Contains(strip2(m.View()), "Set up a coordinator?") {
			t.Error("shows once the prompt is done")
		}
		if m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.coordOffer == nil {
			t.Error("the first key starts its grace")
		}
	})
	t.Run("waits behind a typed search, unless asked for", func(t *testing.T) {
		m := newList(t)
		m.textInput.SetValue("abc")
		m = offer(m, false)
		if m.offerShown() || strings.Contains(strip2(m.View()), "Set up a coordinator?") {
			t.Error("shown over the search")
		}
		if m, _ = key(m, tea.KeyMsg{Type: tea.KeyEnter}); m.coordOffer == nil || loadCoordinator().Offered {
			t.Error("enter on the search answered it")
		}
		m.coordOffer = nil
		if m = offer(m, true); !m.offerShown() {
			t.Error("^X O shows it")
		}
	})
	t.Run("a late check doesn't reopen what's answered", func(t *testing.T) {
		m := newList(t)
		markCoordinatorAnswered()
		if m = offer(m, false); m.coordOffer != nil {
			t.Error("offered again")
		}
		m = newList(t)
		m.coordBusy = true
		if m = offer(m, false); m.coordOffer != nil || !m.coordBusy {
			t.Error("offered during set-up")
		}
	})
	t.Run("small screens still show the plan and the keys", func(t *testing.T) {
		m := offer(newList(t), false)
		m.width, m.height = 44, 14
		m.coordOffer.fixes = []mcpFix{{"claude", []string{"mcp", "add", "--scope", "user", "ccs", "--", "ccs", "mcp"}}}
		v := strings.Join(strings.Fields(strip2(m.coordinatorPopup())), " ")
		for _, want := range []string{"Set up a coordinator?", "--scope", "enter", "esc"} {
			if !strings.Contains(v, want) {
				t.Errorf("lacks %q: %s", want, v)
			}
		}
		if lines := strings.Count(m.coordinatorPopup(), "\n") + 1; lines > 14 {
			t.Errorf("%d lines in 14", lines)
		}
		for _, l := range strings.Split(strip2(m.coordinatorPopup()), "\n") {
			if w := lipgloss.Width(l); w > 44 {
				t.Errorf("%d wide: %q", w, l)
			}
		}
	})
	t.Run("what a failed set-up did stays on screen", func(t *testing.T) {
		m := newList(t)
		res, _ := m.Update(coordinatorDoneMsg{text: "ccs MCP server added to claude", err: errors.New("couldn't open a tab")})
		if got := res.(model).errorMsg; !strings.Contains(got, "added to claude") || !strings.Contains(got, "open a tab") {
			t.Error(got)
		}
	})
}

func TestCoordinatorPromptNoAndCtrlD(t *testing.T) {
	for name, in := range map[string]string{"n": "n\n", "ctrl+d": ""} {
		t.Run(name, func(t *testing.T) {
			coordinatorFixture(t)
			scripts := startsAs(t, coordinatorName)
			var out bytes.Buffer
			code := runCoordinator([]string{"--no-mcp"}, strings.NewReader(in), &out, &out, true)
			if code != 1 || len(*scripts) > 0 || !strings.Contains(out.String(), "Nothing changed") {
				t.Errorf("%d %s %v", code, out.String(), *scripts)
			}
			if rec := loadCoordinator(); !rec.Offered || rec.SessionID != "" {
				t.Errorf("declined for good, nothing else: %+v", rec)
			}
		})
	}
	t.Run("enter alone is yes", func(t *testing.T) {
		coordinatorFixture(t)
		scripts := startsAs(t, coordinatorName)
		var out bytes.Buffer
		if code := runCoordinator([]string{"--no-mcp"}, strings.NewReader("\n"), &out, &out, true); code != 0 || len(*scripts) != 1 {
			t.Errorf("%d %s %v", code, out.String(), *scripts)
		}
	})
}

func TestCoordinatorPopupAndKey(t *testing.T) {
	coordinatorFixture(t)
	startsAs(t, coordinatorName)
	m := initialModel(buildItems([]Conversation{{SessionID: "a", Title: "other", LastTimestamp: time.Now().UTC().Format(time.RFC3339)}}), "", nil)
	m.width, m.height = 140, 40

	res, _ := m.Update(coordinatorOfferMsg{plan: coordinatorPlan{fixes: []mcpFix{{"claude", []string{"mcp", "add", "--scope", "user", "ccs", "--", "ccs", "mcp"}}}}})
	m = res.(model)
	m.coordSince = time.Now().Add(-updateKeyGrace)
	v := strip2(m.View())
	for _, want := range []string{"Set up a coordinator?", "claude mcp add --scope user ccs -- ccs mcp", "create ", "trust the folder"} {
		if !strings.Contains(v, want) {
			t.Errorf("popup lacks %q:\n%s", want, v)
		}
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.coordOffer != nil || !loadCoordinator().Offered {
		t.Error("esc answers it for good")
	}

	m, cmd := chord(m, "o")
	if !m.coordBusy || cmd == nil {
		t.Fatalf("^X O starts the look: busy=%v", m.coordBusy)
	}
	if m2, cmd2 := chord(m, "o"); cmd2 != nil || !strings.Contains(m2.errorMsg, "Still") {
		t.Errorf("never twice at once: %q", m2.errorMsg)
	}
	res, _ = m.Update(cmd())
	m = res.(model)
	if m.coordOffer == nil {
		t.Fatal("^X O with none offers, whatever was answered before")
	}
	m.coordSince = time.Now().Add(-updateKeyGrace)
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.coordOffer != nil || cmd == nil {
		t.Fatal("enter sets up")
	}
	res, _ = m.Update(cmd())
	m = res.(model)
	if m.coordBusy || m.errorMsg != "" && !strings.Contains(m.errorMsg, "coordinator started") {
		t.Errorf("done: busy=%v %q", m.coordBusy, m.errorMsg)
	}
	if loadCoordinator().SessionID == "" {
		t.Error("nothing recorded")
	}
}

func TestCoordinatorProbeOffersOnlyWhenNoneAndUnanswered(t *testing.T) {
	coordinatorFixture(t)
	msg, ok := probeCoordinator().(coordinatorOfferMsg)
	if !ok || len(msg.plan.fixes) != 1 {
		t.Errorf("clean state: offered with the MCP step, got %+v", msg)
	}
	markCoordinatorAnswered()
	if probeCoordinator() != nil {
		t.Error("answered: not again")
	}
	rec := loadCoordinator()
	rec.Offered = false
	rec.save()
	os.WriteFile(filepath.Join(getSessionsDir(), "7.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"x","name":"coordinator"}`), 0o600)
	if probeCoordinator() != nil {
		t.Error("one exists: no offer")
	}
	t.Setenv("ITERM_SESSION_ID", "")
	os.Remove(filepath.Join(getSessionsDir(), "7.json"))
	rec = loadCoordinator()
	rec.SessionID = ""
	rec.save()
	if probeCoordinator() != nil {
		t.Error("nowhere to open a tab: no offer")
	}
}

func TestCoordinatorPopupOverAnEmptyList(t *testing.T) {
	coordinatorFixture(t)
	if !coordinatorOfferable() {
		t.Fatal("clean state, tabs and claude: offerable")
	}
	m := initialModel(nil, "", nil)
	m.width, m.height = 120, 40
	res, _ := m.Update(coordinatorOfferMsg{})
	m = res.(model)
	if v := strip2(m.View()); !strings.Contains(v, "Set up a coordinator?") {
		t.Errorf("a brand-new user sees the offer:\n%s", v)
	}
	m.coordSince = time.Now().Add(-updateKeyGrace)
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	for _, k := range []tea.KeyMsg{{Type: tea.KeyDown}, {Type: tea.KeyEnter}, {Type: tea.KeyUp}, {Type: tea.KeyCtrlF}, {Type: tea.KeyCtrlD}, {Type: tea.KeyTab}} {
		m, _ = key(m, k)
		m.View()
	}
	if coordinatorOfferable() {
		t.Error("answered: no more")
	}
}

func TestNoAutomaticRestartDuringCoordinatorSetUp(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.blurred = true
	if !m.awayForUpdate() {
		t.Fatal("away, nothing going on")
	}
	m.coordBusy = true
	if m.awayForUpdate() {
		t.Error("restarting now loses what the set-up says when it's done")
	}
}

func TestFirstRunRefreshIsNoFailure(t *testing.T) {
	coordinatorFixture(t)
	m := initialModel(nil, "", nil)
	res, _ := m.Update(refreshMsg{err: fmt.Errorf("scan: %w", fs.ErrNotExist)})
	if res.(model).refreshFailed {
		t.Error("no folder yet, nothing to list: not a failure")
	}
	m = initialModel(buildItems([]Conversation{{SessionID: "a", Title: "x"}}), "", nil)
	if res, _ := m.Update(refreshMsg{err: fmt.Errorf("scan: %w", fs.ErrNotExist)}); !res.(model).refreshFailed {
		t.Error("a folder that vanished under a list still is")
	}
	m = initialModel(nil, "", nil)
	if res, _ := m.Update(refreshMsg{err: errors.New("disk on fire")}); !res.(model).refreshFailed {
		t.Error("any other error still is")
	}
}

func TestEnterOnAnEmptyListWaitsForTheSetUp(t *testing.T) {
	coordinatorFixture(t)
	m := initialModel(nil, "", nil)
	m.width, m.height = 120, 40
	m.coordBusy = true
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.quitting || cmd != nil {
		t.Error("a second Enter mustn't quit while it's setting up")
	}
	m.coordBusy = false
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
	coordinatorFixture(t)
	bin := t.TempDir()
	script := filepath.Join(bin, "fake-mcp")
	os.WriteFile(script, []byte("#!/bin/sh\necho 'No MCP server named ccs' >&2\nexit 1\n"), 0o700)
	out, err := runCommand(5*time.Second, nil, true, script)
	if err == nil || !strings.Contains(string(out), "No MCP server") {
		t.Errorf("how claude says it's missing: %v %q", err, out)
	}
}

func TestLaunchOpensTabsInTmuxToo(t *testing.T) {
	t.Setenv("TMUX", "/tmp/s,1,0")
	if !canOpenTabs() {
		t.Error("tmux can open a window")
	}
	t.Setenv("TMUX", "")
	if canOpenTabs() {
		t.Error("nothing to open a tab in")
	}
}

func TestTmuxRunsTheLauncherDirectly(t *testing.T) {
	args := tmuxWindowArgs("/home/a b/launch.sh")
	// Everything after the options must be more than one word, or tmux hands it to a shell.
	i := slices.Index(args, "#{pane_tty}") + 1
	if got := args[i:]; len(got) < 2 || got[len(got)-1] != "/home/a b/launch.sh" {
		t.Errorf("%q", got)
	}
}

func TestNoZshChangesNothing(t *testing.T) {
	ran := coordinatorFixture(t)
	scripts := startsAs(t, coordinatorName)
	old := checkLaunchShell
	checkLaunchShell = func() error { return errors.New("sessions are started with zsh, and there's none") }
	defer func() { checkLaunchShell = old }()
	out, code := runCoord("--yes")
	if code != 1 || !strings.Contains(out, "zsh") || len(*scripts) > 0 {
		t.Errorf("%d %s %v", code, out, *scripts)
	}
	if slices.ContainsFunc(*ran, func(c string) bool { return strings.Contains(c, "mcp add") }) {
		t.Errorf("changed config: %v", *ran)
	}
	if _, err := os.Stat(coordinatorDir()); err == nil {
		t.Error("wrote its folder")
	}
	if loadCoordinator().SessionID != "" {
		t.Error("recorded a session")
	}
}

func TestCloseCountsAWindowGoneWithItsSessionAsClosed(t *testing.T) {
	t.Setenv("TMUX", "/tmp/s,1,0")
	for _, k := range []string{"TERM_PROGRAM", "LC_TERMINAL", "ITERM_SESSION_ID"} {
		t.Setenv(k, "")
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho '/dev/ttys1 other:0.0'\n"), 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	// Reads the real closeTab, which TestMain stubs.
	if !realCloseTab("/dev/ttys9") {
		t.Error("no pane on that tty in tmux: its window closed itself")
	}
}

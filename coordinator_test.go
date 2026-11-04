package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

func TestCoordinatorPopupAndKey(t *testing.T) {
	coordinatorFixture(t)
	startsAs(t, coordinatorName)
	m := initialModel(buildItems([]Conversation{{SessionID: "a", Title: "other", LastTimestamp: time.Now().UTC().Format(time.RFC3339)}}), "", nil)
	m.width, m.height = 140, 40

	res, _ := m.Update(coordinatorOfferMsg{plan: coordinatorPlan{fixes: []mcpFix{{"claude", []string{"mcp", "add", "--scope", "user", "ccs", "--", "ccs", "mcp"}}}}})
	m = res.(model)
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
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	for _, k := range []tea.KeyMsg{{Type: tea.KeyDown}, {Type: tea.KeyEnter}, {Type: tea.KeyUp}, {Type: tea.KeyCtrlF}, {Type: tea.KeyCtrlD}, {Type: tea.KeyTab}} {
		m, _ = key(m, k)
		m.View()
	}
	if coordinatorOfferable() {
		t.Error("answered: no more")
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

package tui

import (
	"bufio"
	"cmp"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// The orchestrator: one Claude Code session, in its own section atop the list, that starts a named session
// per ask, tracks their PRs and closes them. `ccs orchestrator` and ^X O open
// it, resume it, or set it up, and never make a second one.

// orchestratorBrief is the role, written once as the folder's CLAUDE.md.
//
//go:embed orchestrator.md
var orchestratorBrief string

const (
	orchestratorName   = "orchestrator"
	orchestratorPrompt = "You are the orchestrator. Follow CLAUDE.md: do its start-up steps, then ask what to work on."
	// trustNote is why the new tab comes to the front: Claude Code registers no
	// session until its folder-trust question is answered, and the highlighted
	// answer isn't yes.
	trustNote = "Claude asks once whether to trust the folder: choose \"Yes, I trust this folder\", so its ccs commands run without asking."
)

// orchestratorWait leaves time to answer the trust question.
var orchestratorWait = 3 * time.Minute

// orchestratorSettings pre-allows only what the role needs, for this folder only.
const orchestratorSettings = `{
  "permissions": {
    "allow": [
      "Bash(ccs launch:*)",
      "Bash(ccs close:*)",
      "Bash(ccs restart:*)",
      "Bash(ccs prs:*)",
      "Bash(ccs sessions:*)",
      "Bash(ccs depends:*)",
      "Bash(ccs claim:*)",
      "Bash(ccs watch:*)",
      "Bash(tail -n0 -F:*)",
      "Edit(./briefs/**)",
      "Edit(./needs-you.md)",
      "mcp__ccs"
    ]
  }
}
`

// orchestratorRecord is kept in a file of its own, not sessions.json, which a
// running ccs holds in memory and would write over. Name is set when an
// adopted session is called something else.
type orchestratorRecord struct {
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Offered   bool   `json:"offered,omitempty"`  // the one-time popup was answered
	Released  bool   `json:"released,omitempty"` // --release: no looking for one by its name
}

func orchestratorPath() string { return state.OrchestratorPath() }

// orchestratorDir is its working folder, holding its CLAUDE.md and settings, so
// every way of resuming it (Enter, the restart popup, plain claude --resume)
// gets them with nothing special. A coordinator folder from before the rename
// is used where it is, never moved: it is a running session's cwd, and Claude
// Code files that session's transcripts under its path.
func orchestratorDir() string {
	dir := filepath.Join(filepath.Dir(state.SettingsPath), orchestratorName)
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	old := filepath.Join(filepath.Dir(state.SettingsPath), state.LegacyOrchestratorName)
	if fi, err := os.Stat(old); err == nil && fi.IsDir() {
		return old
	}
	return dir
}

// isOrchestratorCmd is `ccs orchestrator`, or `ccs coordinator`, its name
// before the rename, which still works.
func isOrchestratorCmd(arg string) bool {
	return arg == orchestratorName || arg == state.LegacyOrchestratorName
}

// loadOrchestrator reads the record. A coordinator.json from before the
// rename is copied to orchestrator.json once (and left in place for a ccs
// still running the old version), its session id kept, and its name pinned
// to "coordinator" when it had none, since that is what ccs called the
// session then and what findOrchestrator follows it by after a /clear.
func loadOrchestrator() (r orchestratorRecord) {
	b, legacy, err := state.ReadOrchestrator()
	if err != nil {
		return r
	}
	json.Unmarshal(b, &r)
	if legacy {
		if r.Name == "" && !r.Released {
			r.Name = state.LegacyOrchestratorName
		}
		r.save()
	}
	return r
}

func (r orchestratorRecord) name() string { return cmp.Or(r.Name, orchestratorName) }

func (r orchestratorRecord) save() error {
	data, _ := json.Marshal(r)
	if err := os.MkdirAll(filepath.Dir(orchestratorPath()), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(orchestratorPath()), "orchestrator-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), orchestratorPath())
}

// errOrchestratorBusy: another ccs is setting it up or resuming it right now.
var errOrchestratorBusy = errors.New("another ccs is opening the orchestrator right now: look for its tab, or try again in a moment")

// orchestratorTurn is one ccs's turn to decide and start: looking and launching
// are one step, under a lock, since a starting session is invisible until its
// process is up. Held until release (exec and exit let go too). When another
// ccs has the turn, an orchestrator that's already up can still be switched to;
// anything else is errOrchestratorBusy.
func orchestratorTurn() (release func(), s orchestratorState, err error) {
	release = func() {}
	if err := os.MkdirAll(filepath.Dir(orchestratorPath()), 0o700); err != nil {
		return release, s, err
	}
	f, err := os.OpenFile(orchestratorPath()+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return release, s, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if s = findOrchestrator(); s.kind == orchLive || s.kind == orchStarting {
			return release, s, nil
		}
		return release, s, errOrchestratorBusy
	}
	return func() { f.Close() }, findOrchestrator(), nil
}

// setupOrchestrator carries out an accepted plan, unless an orchestrator has
// appeared since it was offered: then that one is opened instead.
func setupOrchestrator(p orchestratorPlan, inPlace bool) (string, error) {
	release, s, err := orchestratorTurn()
	defer release()
	if err != nil {
		return "", err
	}
	if s.kind != orchNone {
		return openOrchestrator(s, inPlace)
	}
	return createOrchestrator(p, inPlace)
}

// writeOrchestratorFolder creates its folder. What's there already is the
// user's and stays.
func writeOrchestratorFolder() error {
	dir := orchestratorDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "briefs"), 0o700); err != nil {
		return err
	}
	for path, text := range map[string]string{
		filepath.Join(dir, "CLAUDE.md"):                strings.ReplaceAll(orchestratorBrief, "{{events}}", github.PREventsPath()),
		filepath.Join(dir, ".claude", "settings.json"): orchestratorSettings,
	} {
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// The orchestrator's states, from findOrchestrator.
const (
	orchNone      = iota
	orchLive      // running: pid
	orchStarting  // its process is up, waiting on the trust question: pid
	orchResumable // not running, with a transcript
)

type orchestratorState struct {
	kind int
	rec  orchestratorRecord
	pid  int
}

// findOrchestrator looks for one: the recorded session, running or starting,
// then a session with its name (a /clear changes a session's id, and one may
// predate the record). A session found another way becomes the record.
func findOrchestrator() orchestratorState {
	rec := loadOrchestrator()
	if rec.Released {
		return orchestratorState{kind: orchNone, rec: rec}
	}
	live := sessions.ScanSessions(nil)
	if rec.SessionID != "" {
		if s, ok := live[rec.SessionID]; ok {
			if followName(&rec, s.Stat.Name) {
				rec.save()
			}
			return orchestratorState{orchLive, rec, s.PID}
		}
	}
	if followLive(&rec, live) {
		rec.save()
		return orchestratorState{orchLive, rec, live[rec.SessionID].PID}
	}
	if rec.SessionID != "" {
		if pid := startingProcess(rec.SessionID); pid != 0 {
			// Its arguments keep the id it started with, so after /clear (with a
			// new name too) it's this process that says which session it is now.
			for id, s := range live {
				if s.PID == pid {
					rec.SessionID = id
					followName(&rec, s.Stat.Name)
					rec.save()
					return orchestratorState{orchLive, rec, pid}
				}
			}
			return orchestratorState{orchStarting, rec, pid}
		}
	}
	if rec.SessionID != "" && sessions.Resumable(rec.SessionID, false) {
		return orchestratorState{orchResumable, rec, 0}
	}
	if id := namedTranscript(rec.name()); id != "" {
		rec.SessionID = id
		rec.save()
		return orchestratorState{orchResumable, rec, 0}
	}
	return orchestratorState{kind: orchNone, rec: rec}
}

// followLive points rec at the one running session with its name, when the
// recorded id isn't running: /clear gives a session a new id and keeps its name.
func followLive(rec *orchestratorRecord, live map[string]sessions.LiveSession) bool {
	if _, ok := live[rec.SessionID]; ok && rec.SessionID != "" {
		return false
	}
	found := ""
	for id, s := range live {
		if s.Stat.Name == rec.name() && !s.Codex {
			if found != "" {
				return false
			}
			found = id
		}
	}
	if found == "" {
		return false
	}
	rec.SessionID = found
	return true
}

// followName keeps the record's name the recorded session's own once it runs
// under another (/rename, or ^X E), so that a later /clear, which gives it a
// new id, is followed by its new name. The default name is kept as no name.
func followName(rec *orchestratorRecord, name string) bool {
	if name == "" || name == rec.name() || !state.SessionName.MatchString(name) {
		return false
	}
	rec.Name = name
	if name == orchestratorName {
		rec.Name = ""
	}
	return true
}

// startingProcess is the pid of a claude started with this --session-id or
// --resume: until any question it opens with is answered, it has no session
// file to find it by.
func startingProcess(id string) int {
	out, err := proc.PsAll()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || filepath.Base(f[2]) == "ccs" || !strings.Contains(line, "--session-id "+id) && !strings.Contains(line, "--resume "+id) {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			return pid
		}
	}
	return 0
}

// namedTranscript is the newest Claude session named name, "" if none.
var namedTranscript = func(name string) string {
	convs, _ := transcript.GetConversations(time.Now().AddDate(0, 0, -60), 1024*1024*1024, nil)
	id, newest := "", ""
	for _, c := range convs {
		if c.Kind != transcript.KindCodex && c.IsCustomTitle && c.Title == name && c.LastTimestamp >= newest {
			id, newest = c.SessionID, c.LastTimestamp
		}
	}
	return id
}

// sessionCwd is the folder a session ran in.
func sessionCwd(id string) string {
	path := transcript.TranscriptPath(id)
	if path == "" {
		return ""
	}
	c, err := transcript.ParseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil || c.Cwd == "unknown" {
		return ""
	}
	return c.Cwd
}

// sessionTitle is the name a session was given, "" if none.
func sessionTitle(id string) string {
	path := transcript.TranscriptPath(id)
	if path == "" {
		return ""
	}
	c, err := transcript.ParseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil || !c.IsCustomTitle {
		return ""
	}
	return c.Title
}

// mcpRun runs claude's or codex's mcp commands, detached so it can't prompt
// and with stderr, where claude says a server is missing. From the home folder,
// not wherever ccs was started: a repo's own ccs server is no use to the
// orchestrator, whose folder is elsewhere. Stubbed in tests.
var mcpRun = realMCPRun

func realMCPRun(bin string, args ...string) ([]byte, error) {
	dir, _ := os.UserHomeDir()
	return proc.RunCommandIn(dir, 30*time.Second, nil, true, bin, args...)
}

// mcpFix is one CLI's missing ccs MCP server: the command that adds it.
type mcpFix struct {
	bin  string
	args []string
}

func (f mcpFix) String() string { return f.bin + " " + strings.Join(f.args, " ") }

// ccsCommand is how an MCP config should start ccs.
func ccsCommand() string {
	if _, err := exec.LookPath("ccs"); err == nil {
		return "ccs"
	}
	if self, err := os.Executable(); err == nil {
		return self
	}
	return "ccs"
}

// missingMCP says which installed CLIs lack the ccs MCP server. A CLI that
// can't say is left alone, with a note: never install over an unknown.
func missingMCP() (fixes []mcpFix, notes []string) {
	if _, err := sessions.LookBin("claude"); err == nil {
		out, err := mcpRun("claude", "mcp", "get", "ccs")
		switch {
		case err == nil:
			if !strings.Contains(string(out), "Connected") {
				notes = append(notes, "Claude Code has a ccs MCP server that isn't connected, left as it is; the orchestrator can use the ccs commands instead (claude mcp get ccs says why)")
			}
		case strings.Contains(string(out), "No MCP server"):
			fixes = append(fixes, mcpFix{"claude", []string{"mcp", "add", "--scope", "user", "ccs", "--", ccsCommand(), "mcp"}})
		default:
			notes = append(notes, fmt.Sprintf("couldn't tell whether Claude Code has the ccs MCP server: %v", err))
		}
	}
	if _, err := sessions.LookBin("codex"); err == nil {
		out, err := mcpRun("codex", "mcp", "list", "--json")
		var servers []struct {
			Name string `json:"name"`
		}
		switch {
		case err != nil || json.Unmarshal(out, &servers) != nil:
			notes = append(notes, "couldn't tell whether Codex has the ccs MCP server")
		default:
			found := false
			for _, s := range servers {
				found = found || s.Name == "ccs"
			}
			if !found {
				fixes = append(fixes, mcpFix{"codex", []string{"mcp", "add", "ccs", "--", ccsCommand(), "mcp"}})
			}
		}
	}
	return fixes, notes
}

// applyMCP adds what's missing and checks it took: Claude Code must report it
// connected, Codex (which doesn't test servers) must list it.
func applyMCP(fixes []mcpFix) (done []string, errs []string) {
	for _, f := range fixes {
		// Already there means another ccs got there since the plan: check it as any other.
		if out, err := mcpRun(f.bin, f.args...); err != nil && !strings.Contains(string(out), "already exists") {
			errs = append(errs, fmt.Sprintf("%s failed: %v: %s", f, err, strings.TrimSpace(string(out))))
			continue
		}
		args := []string{"mcp", "get", "ccs"}
		out, err := mcpRun(f.bin, args...)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("added it to %s, but %s %s failed: %v", f.bin, f.bin, strings.Join(args, " "), err))
		case f.bin == "claude" && !strings.Contains(string(out), "Connected"):
			errs = append(errs, "added the ccs MCP server to claude, but it doesn't report as connected: "+strings.TrimSpace(string(out)))
		case f.bin == "claude":
			done = append(done, "ccs MCP server added to claude and connected")
		default:
			done = append(done, "ccs MCP server added to "+f.bin)
		}
	}
	return done, errs
}

// orchestratorPlan is what setting one up will change.
type orchestratorPlan struct {
	fixes []mcpFix
	notes []string
}

func planOrchestrator(noMCP bool) orchestratorPlan {
	if noMCP {
		return orchestratorPlan{}
	}
	fixes, notes := missingMCP()
	return orchestratorPlan{fixes, notes}
}

// lines are the changes, each as the user can check it.
func (p orchestratorPlan) lines() []string {
	verb := "create "
	if _, err := os.Stat(orchestratorDir()); err == nil {
		verb = "use "
	}
	out := []string{verb + sessions.TildePath(orchestratorDir())}
	for _, f := range p.fixes {
		out = append(out, "run "+f.String())
	}
	return out
}

// newSessionID is a random (version 4) uuid, the form claude's --session-id takes.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// createOrchestrator carries out a plan: the MCP server first, so the session
// has it from its first turn. A failed MCP step is reported, not fatal: the
// role works through the ccs commands alone.
func createOrchestrator(p orchestratorPlan, inPlace bool) (string, error) {
	if _, err := sessions.LookBin("claude"); err != nil {
		return "", errors.New("the orchestrator needs Claude Code, since Codex sessions can't receive messages from other sessions (they can still work under it): install claude first")
	}
	if err := sessions.CheckLaunchShell(); err != nil {
		return "", err
	}
	// Only a released one can still be running here: findOrchestrator would have
	// found any other. Before anything changes, and it'd be taken for the new one.
	if pids, err := sessions.Named(orchestratorName); err == nil && len(pids) > 0 {
		return "", fmt.Errorf("a session named %s is still running (pid %d): close or rename it first (ccs close %s)", orchestratorName, pids[0], orchestratorName)
	}
	done, errs := applyMCP(p.fixes)
	if err := writeOrchestratorFolder(); err != nil {
		return "", err
	}
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	rec := loadOrchestrator()
	rec.SessionID, rec.Name, rec.Offered, rec.Released = id, "", true, false
	if err := rec.save(); err != nil {
		return "", err
	}
	o := sessions.LaunchOpts{Name: orchestratorName, Brief: orchestratorPrompt, Cwd: orchestratorDir(), SessionID: id, Focus: true, Wait: orchestratorWait, InPlace: inPlace, NoPreamble: true}
	if inPlace && len(done)+len(errs) > 0 {
		fmt.Fprintln(os.Stderr, strings.Join(append(done, errs...), "\n"))
	}
	if _, err := sessions.LaunchSession(o); err != nil {
		return strings.Join(append(done, errs...), "; "), err
	}
	return strings.Join(append(append(done, errs...), "orchestrator started; its role is in "+sessions.TildePath(filepath.Join(orchestratorDir(), "CLAUDE.md"))), "; "), nil
}

// openOrchestrator goes to the one that exists: its tab if it's running,
// resumed under its name if not.
func openOrchestrator(s orchestratorState, inPlace bool) (string, error) {
	name := s.rec.name()
	switch s.kind {
	case orchLive, orchStarting:
		if focusPID(s.pid, true) {
			if s.kind == orchStarting {
				return "Switched to " + name + ", which is still starting: it may be asking whether to trust its folder", nil
			}
			return "Switched to " + name, nil
		}
		return "", fmt.Errorf("%s is running (pid %d), but ccs can't switch to its tab from here", name, s.pid)
	case orchResumable:
		o := sessions.LaunchOpts{Name: name, Resume: s.rec.SessionID, Cwd: sessionCwd(s.rec.SessionID), Focus: true, Wait: orchestratorWait, InPlace: inPlace}
		if _, err := sessions.LaunchSession(o); err != nil {
			return "", err
		}
		return "Resumed " + name, nil
	}
	return "", errors.New("no orchestrator to open")
}

// focusPID is focusSession, stubbed in tests.
var focusPID = sessions.FocusSession

// adoptOrchestrator makes an existing session the orchestrator, by its name or
// id. Its folder and role stay its own.
func adoptOrchestrator(who string) (string, error) {
	release, _, err := orchestratorTurn()
	defer release()
	if err != nil {
		return "", err
	}
	rec := loadOrchestrator()
	id, name, matches := "", "", 0
	for sid, s := range sessions.ScanSessions(nil) {
		if !s.Codex && (s.Stat.Name == who || sid == who) {
			id, name = sid, s.Stat.Name
			matches++
		}
	}
	if matches > 1 {
		return "", fmt.Errorf("%d running sessions are named %q: give the id of the one you mean", matches, who)
	}
	if id == "" && sessions.Resumable(who, false) {
		id, name = who, sessionTitle(who)
	}
	if id == "" {
		id = namedTranscript(who)
		name = who
	}
	if id == "" {
		return "", fmt.Errorf("no Claude Code session named %q, and no session with that id", who)
	}
	// Coming back goes through launch, which takes only names like these.
	if name != "" && !state.SessionName.MatchString(name) {
		return "", fmt.Errorf("ccs couldn't reopen a session named %q: rename it with letters, digits, '.', '_' and '-' first (/rename there, or Ctrl+X E in the list)", name)
	}
	replaced := rec.SessionID != "" && rec.SessionID != id
	rec.SessionID, rec.Offered, rec.Released = id, true, false
	rec.Name = ""
	if name != "" && name != orchestratorName {
		rec.Name = name
	}
	if err := rec.save(); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("%s (%s) is now the orchestrator", cmp.Or(name, id), id)
	if replaced {
		msg += "; the one before it stays as an ordinary session"
	}
	return msg, nil
}

// releaseOrchestrator makes the orchestrator an ordinary session again: its
// conversation, name and folder stay, nothing is closed, and ccs stops looking
// for one until asked to set one up or adopt one.
func releaseOrchestrator() (string, error) {
	release, s, err := orchestratorTurn()
	defer release()
	if err != nil {
		return "", err
	}
	if s.kind == orchNone {
		return "There's no orchestrator to release.", nil
	}
	rec := orchestratorRecord{Offered: true, Released: true}
	if err := rec.save(); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("%s (%s) is an ordinary session now.\n^X O or ccs orchestrator sets one up again.", s.rec.name(), s.rec.SessionID)
	if s.kind == orchLive || s.kind == orchStarting {
		msg += fmt.Sprintf("\nIt's still running: ccs close %s stops it.", s.rec.name())
	}
	return msg, nil
}

// runOrchestrator is `ccs orchestrator`.
func runOrchestrator(args []string, stdin io.Reader, stdout, stderr io.Writer, isTerminal bool) int {
	fs := flag.NewFlagSet(orchestratorName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "set it up without asking")
	noMCP := fs.Bool("no-mcp", false, "don't install the ccs MCP server")
	adopt := fs.String("adopt", "", "make this existing session (a name or id) the orchestrator")
	releaseIt := fs.Bool("release", false, "make the orchestrator an ordinary session again; it keeps running")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs orchestrator [--yes] [--no-mcp] [--adopt <session> | --release]\n\nOpen the orchestrator: its tab if it's running, resumed if not, set up if there is none.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	if *releaseIt && *adopt != "" {
		fs.Usage()
		return 2
	}
	if *releaseIt {
		msg, err := releaseOrchestrator()
		if err != nil {
			fmt.Fprintln(stderr, "ccs orchestrator:", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	if *adopt != "" {
		msg, err := adoptOrchestrator(*adopt)
		if err != nil {
			fmt.Fprintln(stderr, "ccs orchestrator:", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	inPlace := !sessions.CanOpenTabs()
	release, s, err := orchestratorTurn()
	defer release()
	if err != nil {
		fmt.Fprintln(stderr, "ccs orchestrator:", err)
		return 1
	}
	if s.kind != orchNone {
		msg, err := openOrchestrator(s, inPlace)
		if err != nil {
			fmt.Fprintln(stderr, "ccs orchestrator:", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	release() // not across the question: setupOrchestrator looks again
	p := planOrchestrator(*noMCP)
	fmt.Fprintln(stdout, "Set up an orchestrator: a session at the top of the list that starts one named session per task, tracks their PRs and merge order, and closes them when they finish.\n\nThis will:")
	for _, l := range p.lines() {
		fmt.Fprintln(stdout, "  "+l)
	}
	for _, n := range p.notes {
		fmt.Fprintln(stdout, "  note: "+n)
	}
	if !*yes {
		if !isTerminal {
			fmt.Fprintln(stderr, "\nNothing changed: not a terminal, so run it again with --yes to go ahead.")
			return 1
		}
		fmt.Fprint(stdout, "\nGo ahead? [Y/n] ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		// Enter alone is yes; Ctrl+D is nobody saying so.
		if a := strings.ToLower(strings.TrimSpace(line)); err != nil || a != "" && a != "y" && a != "yes" {
			markOrchestratorAnswered()
			fmt.Fprintln(stdout, "\nNothing changed.")
			return 1
		}
	}
	fmt.Fprintln(stdout, "\n"+trustNote)
	msg, err := setupOrchestrator(p, inPlace)
	if msg != "" {
		fmt.Fprintln(stdout, msg)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccs orchestrator:", err)
		return 1
	}
	return 0
}

// stdinIsTerminal: someone can answer a question.
func stdinIsTerminal() bool { return isTerminal(os.Stdin) }

// isTerminal, not a character-device check: /dev/null is one too.
func isTerminal(f *os.File) bool { return term.IsTerminal(f.Fd()) }

// The list's side.

type orchestratorOfferMsg struct {
	plan   orchestratorPlan
	opened bool // asked for with ^X O, so it opens whatever was answered before and at once
}

// offerShown: the offer is on screen and takes keys. One nobody asked for waits
// behind anything being typed (the search, the message box, a prompt), so what
// was meant for those can't answer it.
func (m model) offerShown() bool {
	if m.orchOffer == nil || len(m.reopen) > 0 || m.prompting() {
		return false
	}
	return m.orchAsked || !m.chatFocus && m.textInput.Value() == ""
}

type orchestratorDoneMsg struct {
	text string
	err  error
	id   string
}

// orchestratorProbe runs at startup: the offer's check, in the background.
// Stubbed in tests.
var orchestratorProbe = probeOrchestrator

func probeOrchestrator() tea.Msg {
	if !orchestratorOfferable() {
		return nil
	}
	if findOrchestrator().kind != orchNone {
		return nil
	}
	return orchestratorOfferMsg{plan: planOrchestrator(false)}
}

// orchestratorOfferable: the popup hasn't been answered, and could set one up here.
func orchestratorOfferable() bool {
	if loadOrchestrator().Offered || !sessions.CanOpenTabs() {
		return false
	}
	_, err := sessions.LookBin("claude")
	return err == nil
}

// orchestratorKeyCmd is ^X O, off the UI goroutine: open it, or offer to set
// one up.
func orchestratorKeyCmd() tea.Msg {
	release, s, err := orchestratorTurn()
	defer release()
	if err != nil {
		return orchestratorDoneMsg{err: err}
	}
	if s.kind == orchNone {
		return orchestratorOfferMsg{plan: planOrchestrator(false), opened: true}
	}
	text, err := openOrchestrator(s, false)
	return orchestratorDoneMsg{text, err, s.rec.SessionID}
}

// orchestratorSetupCmd is the popup's Enter.
func orchestratorSetupCmd(p orchestratorPlan) tea.Cmd {
	return func() tea.Msg {
		text, err := setupOrchestrator(p, false)
		return orchestratorDoneMsg{text, err, loadOrchestrator().SessionID}
	}
}

// markOrchestratorAnswered stops the offer. A failed save only means one more
// offer, so it isn't reported.
func markOrchestratorAnswered() {
	rec := loadOrchestrator()
	rec.Offered = true
	rec.save()
}

// syncOrchestrator follows the record, which `ccs orchestrator` may change while
// the list is open, and the session itself, whose id /clear changes: that
// session gets the section above all others.
func (m *model) syncOrchestrator() {
	rec := loadOrchestrator()
	if rec.SessionID != "" && followLive(&rec, m.lastScan) {
		rec.save()
	}
	if rec.SessionID != m.orchID {
		m.orchID = rec.SessionID
		m.sortList()
	}
}

// selectedSession is the selected session's id, "" with nothing listed.
func (m model) selectedSession() string {
	if len(m.filtered) == 0 {
		return ""
	}
	return m.filtered[m.cursor].conv.SessionID
}

// refilter runs the filter again, keeping the cursor on its session where it's
// still listed.
func (m *model) refilter() {
	sel := ""
	if len(m.filtered) > 0 {
		sel = m.filtered[m.cursor].conv.SessionID
	}
	m.lastFilterQuery = "" // narrowing from the shown list would miss what it hid
	m.updateFilter()
	if i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == sel }); i >= 0 {
		m.cursor = i
	}
}

// orchestratorPopup offers to set one up, with everything it will change.
//
// What it changes and its keys always show: on a small screen the lines wrap
// and the rest goes (the trust question is explained again on accepting).
func (m model) orchestratorPopup() string {
	compact := m.width > 0 && m.width < 70 || m.height > 0 && m.height < 24
	wrap := func(s string) string {
		if m.width > 0 {
			return ansi.Wrap(s, max(m.width-8, 20), " ")
		}
		return s
	}
	var b strings.Builder
	b.WriteString("\033[1mSet up an orchestrator?\033[0m\n")
	if !compact {
		b.WriteString("\nA session at the top of the list that starts one\nnamed session per task, tracks their PRs and merge order,\nand closes them when they finish.\n\n")
	}
	b.WriteString("This will:\n")
	for _, l := range m.orchOffer.lines() {
		b.WriteString(wrap("  "+l) + "\n")
	}
	for _, n := range m.orchOffer.notes {
		b.WriteString("\033[90m" + wrap("  "+n) + "\033[0m\n")
	}
	if !compact {
		b.WriteString("\n\033[90mClaude asks once whether to trust the folder:\033[0m\n\033[90mchoose \"Yes, I trust this folder\".\033[0m\n")
	}
	if !compact {
		b.WriteString("\n")
	}
	b.WriteString("\033[90m" + wrap(hints(keyEnter, "set up", keyEsc, "not now, ^X O later")) + "\033[0m")
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("39")).Padding(1, 3)
	if compact {
		style = style.Padding(0, 3)
	}
	return style.Render(b.String())
}

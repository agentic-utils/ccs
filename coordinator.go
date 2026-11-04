package main

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
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// The coordinator: one Claude Code session, in its own section atop the list, that starts a named session
// per ask, tracks their PRs and closes them. `ccs coordinator` and ^X O open
// it, resume it, or set it up, and never make a second one.

// coordinatorBrief is the role, written once as the folder's CLAUDE.md.
//
//go:embed coordinator.md
var coordinatorBrief string

const (
	coordinatorName   = "coordinator"
	coordinatorPrompt = "You are the coordinator. Follow CLAUDE.md: do its start-up steps, then ask what to work on."
	// trustNote is why the new tab comes to the front: Claude Code registers no
	// session until its folder-trust question is answered, and the highlighted
	// answer isn't yes.
	trustNote = "Claude asks once whether to trust the folder: choose \"Yes, I trust this folder\", so its ccs commands run without asking."
)

// coordinatorWait leaves time to answer the trust question.
var coordinatorWait = 3 * time.Minute

// coordinatorSettings pre-allows only what the role needs, for this folder only.
const coordinatorSettings = `{
  "permissions": {
    "allow": [
      "Bash(ccs launch:*)",
      "Bash(ccs close:*)",
      "Bash(ccs prs:*)",
      "Bash(ccs claim:*)",
      "Bash(ccs watch:*)",
      "Bash(tail -n0 -F:*)",
      "Edit(./briefs/**)",
      "mcp__ccs"
    ]
  }
}
`

// coordinatorRecord is kept in a file of its own, not sessions.json, which a
// running ccs holds in memory and would write over. Name is set when an
// adopted session is called something else.
type coordinatorRecord struct {
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Offered   bool   `json:"offered,omitempty"` // the one-time popup was answered
}

func coordinatorPath() string { return filepath.Join(filepath.Dir(settingsPath), "coordinator.json") }

// coordinatorDir is its working folder, holding its CLAUDE.md and settings, so
// every way of resuming it (Enter, the restart popup, plain claude --resume)
// gets them with nothing special.
func coordinatorDir() string { return filepath.Join(filepath.Dir(settingsPath), coordinatorName) }

func loadCoordinator() (r coordinatorRecord) {
	if b, err := os.ReadFile(coordinatorPath()); err == nil {
		json.Unmarshal(b, &r)
	}
	return r
}

func (r coordinatorRecord) name() string { return cmp.Or(r.Name, coordinatorName) }

func (r coordinatorRecord) save() error {
	data, _ := json.Marshal(r)
	if err := os.MkdirAll(filepath.Dir(coordinatorPath()), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(coordinatorPath()), "coordinator-*.tmp")
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
	return os.Rename(tmp.Name(), coordinatorPath())
}

// errCoordinatorBusy: another ccs is setting it up or resuming it right now.
var errCoordinatorBusy = errors.New("another ccs is opening the coordinator right now: look for its tab, or try again in a moment")

// coordinatorTurn is one ccs's turn to decide and start: looking and launching
// are one step, under a lock, since a starting session is invisible until its
// process is up. Held until release (exec and exit let go too). When another
// ccs has the turn, a coordinator that's already up can still be switched to;
// anything else is errCoordinatorBusy.
func coordinatorTurn() (release func(), s coordinatorState, err error) {
	release = func() {}
	if err := os.MkdirAll(filepath.Dir(coordinatorPath()), 0o700); err != nil {
		return release, s, err
	}
	f, err := os.OpenFile(coordinatorPath()+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return release, s, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if s = findCoordinator(); s.kind == coordLive || s.kind == coordStarting {
			return release, s, nil
		}
		return release, s, errCoordinatorBusy
	}
	return func() { f.Close() }, findCoordinator(), nil
}

// setupCoordinator carries out an accepted plan, unless a coordinator has
// appeared since it was offered: then that one is opened instead.
func setupCoordinator(p coordinatorPlan, inPlace bool) (string, error) {
	release, s, err := coordinatorTurn()
	defer release()
	if err != nil {
		return "", err
	}
	if s.kind != coordNone {
		return openCoordinator(s, inPlace)
	}
	return createCoordinator(p, inPlace)
}

// writeCoordinatorFolder creates its folder. What's there already is the
// user's and stays.
func writeCoordinatorFolder() error {
	dir := coordinatorDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "briefs"), 0o700); err != nil {
		return err
	}
	for path, text := range map[string]string{
		filepath.Join(dir, "CLAUDE.md"):                strings.ReplaceAll(coordinatorBrief, "{{events}}", prEventsPath()),
		filepath.Join(dir, ".claude", "settings.json"): coordinatorSettings,
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

// The coordinator's states, from findCoordinator.
const (
	coordNone      = iota
	coordLive      // running: pid
	coordStarting  // its process is up, waiting on the trust question: pid
	coordResumable // not running, with a transcript
)

type coordinatorState struct {
	kind int
	rec  coordinatorRecord
	pid  int
}

// findCoordinator looks for one: the recorded session, running or starting,
// then a session with its name (a /clear changes a session's id, and one may
// predate the record). A session found another way becomes the record.
func findCoordinator() coordinatorState {
	rec := loadCoordinator()
	live := scanSessions(nil)
	if rec.SessionID != "" {
		if s, ok := live[rec.SessionID]; ok {
			return coordinatorState{coordLive, rec, s.pid}
		}
	}
	if followLive(&rec, live) {
		rec.save()
		return coordinatorState{coordLive, rec, live[rec.SessionID].pid}
	}
	if rec.SessionID != "" {
		if pid := startingProcess(rec.SessionID); pid != 0 {
			// Its arguments keep the id it started with, so after /clear (with a
			// new name too) it's this process that says which session it is now.
			for id, s := range live {
				if s.pid == pid {
					rec.SessionID = id
					rec.save()
					return coordinatorState{coordLive, rec, pid}
				}
			}
			return coordinatorState{coordStarting, rec, pid}
		}
	}
	if rec.SessionID != "" && resumable(rec.SessionID, false) {
		return coordinatorState{coordResumable, rec, 0}
	}
	if id := namedTranscript(rec.name()); id != "" {
		rec.SessionID = id
		rec.save()
		return coordinatorState{coordResumable, rec, 0}
	}
	return coordinatorState{kind: coordNone, rec: rec}
}

// followLive points rec at the one running session with its name, when the
// recorded id isn't running: /clear gives a session a new id and keeps its name.
func followLive(rec *coordinatorRecord, live map[string]liveSession) bool {
	if _, ok := live[rec.SessionID]; ok && rec.SessionID != "" {
		return false
	}
	found := ""
	for id, s := range live {
		if s.stat.name == rec.name() && !s.codex {
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

// startingProcess is the pid of a claude started with this --session-id or
// --resume: until any question it opens with is answered, it has no session
// file to find it by.
func startingProcess(id string) int {
	out, err := psAll()
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
	convs, _ := getConversations(time.Now().AddDate(0, 0, -60), 1024*1024*1024, nil)
	id, newest := "", ""
	for _, c := range convs {
		if c.Kind != kindCodex && c.IsCustomTitle && c.Title == name && c.LastTimestamp >= newest {
			id, newest = c.SessionID, c.LastTimestamp
		}
	}
	return id
}

// sessionCwd is the folder a session ran in.
func sessionCwd(id string) string {
	paths, _ := filepath.Glob(filepath.Join(getProjectsDir(), "*", id+".jsonl"))
	if len(paths) == 0 {
		return ""
	}
	c, err := parseConversationFile(paths[0], time.Time{}, 0)
	if err != nil || c == nil || c.Cwd == "unknown" {
		return ""
	}
	return c.Cwd
}

// sessionTitle is the name a session was given, "" if none.
func sessionTitle(id string) string {
	paths, _ := filepath.Glob(filepath.Join(getProjectsDir(), "*", id+".jsonl"))
	if len(paths) == 0 {
		return ""
	}
	c, err := parseConversationFile(paths[0], time.Time{}, 0)
	if err != nil || c == nil || !c.IsCustomTitle {
		return ""
	}
	return c.Title
}

// mcpRun runs claude's or codex's mcp commands, detached so it can't prompt
// and with stderr, where claude says a server is missing. Stubbed in tests.
var mcpRun = func(bin string, args ...string) ([]byte, error) {
	return runCommand(30*time.Second, nil, true, bin, args...)
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
	if _, err := lookBin("claude"); err == nil {
		out, err := mcpRun("claude", "mcp", "get", "ccs")
		switch {
		case err == nil:
		case strings.Contains(string(out), "No MCP server"):
			fixes = append(fixes, mcpFix{"claude", []string{"mcp", "add", "--scope", "user", "ccs", "--", ccsCommand(), "mcp"}})
		default:
			notes = append(notes, fmt.Sprintf("couldn't tell whether Claude Code has the ccs MCP server: %v", err))
		}
	}
	if _, err := lookBin("codex"); err == nil {
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
		if out, err := mcpRun(f.bin, f.args...); err != nil {
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

// coordinatorPlan is what setting one up will change.
type coordinatorPlan struct {
	fixes []mcpFix
	notes []string
}

func planCoordinator(noMCP bool) coordinatorPlan {
	if noMCP {
		return coordinatorPlan{}
	}
	fixes, notes := missingMCP()
	return coordinatorPlan{fixes, notes}
}

// lines are the changes, each as the user can check it.
func (p coordinatorPlan) lines() []string {
	verb := "create "
	if _, err := os.Stat(coordinatorDir()); err == nil {
		verb = "use "
	}
	out := []string{verb + tildePath(coordinatorDir())}
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

// createCoordinator carries out a plan: the MCP server first, so the session
// has it from its first turn. A failed MCP step is reported, not fatal: the
// role works through the ccs commands alone.
func createCoordinator(p coordinatorPlan, inPlace bool) (string, error) {
	if _, err := lookBin("claude"); err != nil {
		return "", errors.New("the coordinator needs Claude Code, since Codex sessions can't receive messages from other sessions (they can still work under it): install claude first")
	}
	if err := checkLaunchShell(); err != nil {
		return "", err
	}
	done, errs := applyMCP(p.fixes)
	if err := writeCoordinatorFolder(); err != nil {
		return "", err
	}
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	rec := loadCoordinator()
	rec.SessionID, rec.Name, rec.Offered = id, "", true
	if err := rec.save(); err != nil {
		return "", err
	}
	o := launchOpts{name: coordinatorName, brief: coordinatorPrompt, cwd: coordinatorDir(), sessionID: id, focus: true, wait: coordinatorWait, inPlace: inPlace}
	if inPlace && len(done)+len(errs) > 0 {
		fmt.Fprintln(os.Stderr, strings.Join(append(done, errs...), "\n"))
	}
	if _, err := launchSession(o); err != nil {
		return strings.Join(append(done, errs...), "; "), err
	}
	return strings.Join(append(append(done, errs...), "coordinator started; its role is in "+tildePath(filepath.Join(coordinatorDir(), "CLAUDE.md"))), "; "), nil
}

// openCoordinator goes to the one that exists: its tab if it's running,
// resumed under its name if not.
func openCoordinator(s coordinatorState, inPlace bool) (string, error) {
	name := s.rec.name()
	switch s.kind {
	case coordLive, coordStarting:
		if focusPID(s.pid, true) {
			if s.kind == coordStarting {
				return "Switched to " + name + ", which is still starting: it may be asking whether to trust its folder", nil
			}
			return "Switched to " + name, nil
		}
		return "", fmt.Errorf("%s is running (pid %d), but ccs can't switch to its tab from here", name, s.pid)
	case coordResumable:
		o := launchOpts{name: name, resume: s.rec.SessionID, cwd: sessionCwd(s.rec.SessionID), focus: true, wait: coordinatorWait, inPlace: inPlace}
		if _, err := launchSession(o); err != nil {
			return "", err
		}
		return "Resumed " + name, nil
	}
	return "", errors.New("no coordinator to open")
}

// focusPID is focusSession, stubbed in tests.
var focusPID = focusSession

// adoptCoordinator makes an existing session the coordinator, by its name or
// id. Its folder and role stay its own.
func adoptCoordinator(who string) (string, error) {
	release, _, err := coordinatorTurn()
	defer release()
	if err != nil {
		return "", err
	}
	rec := loadCoordinator()
	id, name, matches := "", "", 0
	for sid, s := range scanSessions(nil) {
		if !s.codex && (s.stat.name == who || sid == who) {
			id, name = sid, s.stat.name
			matches++
		}
	}
	if matches > 1 {
		return "", fmt.Errorf("%d running sessions are named %q: give the id of the one you mean", matches, who)
	}
	if id == "" && resumable(who, false) {
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
	if name != "" && !sessionName.MatchString(name) {
		return "", fmt.Errorf("ccs couldn't reopen a session named %q: rename it with letters, digits, '.', '_' and '-' first (/rename there, or Ctrl+X E in the list)", name)
	}
	replaced := rec.SessionID != "" && rec.SessionID != id
	rec.SessionID, rec.Offered = id, true
	rec.Name = ""
	if name != "" && name != coordinatorName {
		rec.Name = name
	}
	if err := rec.save(); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("%s (%s) is now the coordinator", cmp.Or(name, id), id)
	if replaced {
		msg += "; the one before it stays as an ordinary session"
	}
	return msg, nil
}

// runCoordinator is `ccs coordinator`.
func runCoordinator(args []string, stdin io.Reader, stdout, stderr io.Writer, isTerminal bool) int {
	fs := flag.NewFlagSet(coordinatorName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "set it up without asking")
	noMCP := fs.Bool("no-mcp", false, "don't install the ccs MCP server")
	adopt := fs.String("adopt", "", "make this existing session (a name or id) the coordinator")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs coordinator [--yes] [--no-mcp] [--adopt <session>]\n\nOpen the coordinator: its tab if it's running, resumed if not, set up if there is none.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	if *adopt != "" {
		msg, err := adoptCoordinator(*adopt)
		if err != nil {
			fmt.Fprintln(stderr, "ccs coordinator:", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	inPlace := !canOpenTabs()
	release, s, err := coordinatorTurn()
	defer release()
	if err != nil {
		fmt.Fprintln(stderr, "ccs coordinator:", err)
		return 1
	}
	if s.kind != coordNone {
		msg, err := openCoordinator(s, inPlace)
		if err != nil {
			fmt.Fprintln(stderr, "ccs coordinator:", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	release() // not across the question: setupCoordinator looks again
	p := planCoordinator(*noMCP)
	fmt.Fprintln(stdout, "Set up a coordinator: a session at the top of the list that starts one named session per task, tracks their PRs and merge order, and closes them when they finish.\n\nThis will:")
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
			markCoordinatorAnswered()
			fmt.Fprintln(stdout, "\nNothing changed.")
			return 1
		}
	}
	fmt.Fprintln(stdout, "\n"+trustNote)
	msg, err := setupCoordinator(p, inPlace)
	if msg != "" {
		fmt.Fprintln(stdout, msg)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccs coordinator:", err)
		return 1
	}
	return 0
}

// stdinIsTerminal: someone can answer a question.
func stdinIsTerminal() bool { return isTerminal(os.Stdin) }

// isTerminal, not a character-device check: /dev/null is one too.
func isTerminal(f *os.File) bool { return term.IsTerminal(f.Fd()) }

// The list's side.

type coordinatorOfferMsg struct {
	plan   coordinatorPlan
	opened bool // asked for with ^X O, so it opens whatever was answered before and at once
}

// offerShown: the offer is on screen and takes keys. One nobody asked for waits
// behind anything being typed (the search, the message box, a prompt), so what
// was meant for those can't answer it.
func (m model) offerShown() bool {
	if m.coordOffer == nil || len(m.reopen) > 0 || m.prompting() {
		return false
	}
	return m.coordAsked || !m.chatFocus && m.textInput.Value() == ""
}

type coordinatorDoneMsg struct {
	text string
	err  error
	id   string
}

// coordinatorProbe runs at startup: the offer's check, in the background.
// Stubbed in tests.
var coordinatorProbe = probeCoordinator

func probeCoordinator() tea.Msg {
	if !coordinatorOfferable() {
		return nil
	}
	if findCoordinator().kind != coordNone {
		return nil
	}
	return coordinatorOfferMsg{plan: planCoordinator(false)}
}

// coordinatorOfferable: the popup hasn't been answered, and could set one up here.
func coordinatorOfferable() bool {
	if loadCoordinator().Offered || !canOpenTabs() {
		return false
	}
	_, err := lookBin("claude")
	return err == nil
}

// coordinatorKeyCmd is ^X O, off the UI goroutine: open it, or offer to set
// one up.
func coordinatorKeyCmd() tea.Msg {
	release, s, err := coordinatorTurn()
	defer release()
	if err != nil {
		return coordinatorDoneMsg{err: err}
	}
	if s.kind == coordNone {
		return coordinatorOfferMsg{plan: planCoordinator(false), opened: true}
	}
	text, err := openCoordinator(s, false)
	return coordinatorDoneMsg{text, err, s.rec.SessionID}
}

// coordinatorSetupCmd is the popup's Enter.
func coordinatorSetupCmd(p coordinatorPlan) tea.Cmd {
	return func() tea.Msg {
		text, err := setupCoordinator(p, false)
		return coordinatorDoneMsg{text, err, loadCoordinator().SessionID}
	}
}

// markCoordinatorAnswered stops the offer. A failed save only means one more
// offer, so it isn't reported.
func markCoordinatorAnswered() {
	rec := loadCoordinator()
	rec.Offered = true
	rec.save()
}

// syncCoordinator follows the record, which `ccs coordinator` may change while
// the list is open, and the session itself, whose id /clear changes: that
// session gets the section above all others.
func (m *model) syncCoordinator() {
	rec := loadCoordinator()
	if rec.SessionID != "" && followLive(&rec, m.lastScan) {
		rec.save()
	}
	if rec.SessionID != m.coordID {
		m.coordID = rec.SessionID
		m.sortList()
	}
}

// coordinatorPopup offers to set one up, with everything it will change.
//
// What it changes and its keys always show: on a small screen the lines wrap
// and the rest goes (the trust question is explained again on accepting).
func (m model) coordinatorPopup() string {
	compact := m.width > 0 && m.width < 70 || m.height > 0 && m.height < 24
	wrap := func(s string) string {
		if m.width > 0 {
			return ansi.Wrap(s, max(m.width-8, 20), " ")
		}
		return s
	}
	var b strings.Builder
	b.WriteString("\033[1mSet up a coordinator?\033[0m\n")
	if !compact {
		b.WriteString("\nA session at the top of the list that starts one\nnamed session per task, tracks their PRs and merge order,\nand closes them when they finish.\n\n")
	}
	b.WriteString("This will:\n")
	for _, l := range m.coordOffer.lines() {
		b.WriteString(wrap("  "+l) + "\n")
	}
	for _, n := range m.coordOffer.notes {
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

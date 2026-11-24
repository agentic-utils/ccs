package sessions

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// A switch carries a session on in another agent (Claude, Codex or agy): a
// new session in that agent, with the same name and folder, briefed with a
// handover built from the old one's transcript, takes the old one's place
// in the tree and its PR claims. The old one is closed first (gracefully,
// when it's a Claude Code session that can be asked to write a handover
// note), since two sessions can't run under one name and a running session
// can't be renamed from outside: Claude Code re-appends its own title, and
// Codex and agy take their name from the launch folder. The new agent gets
// only the brief, never the old one's memory.

// Agent is a coding CLI ccs can launch: its id (a transcript kind, "claude"
// for Claude Code), the name shown for it and the binary it runs.
type Agent struct{ ID, Label, Bin string }

// Agents are the agents ccs launches, in the order it offers them. Gemini is
// the Antigravity CLI, agy.
var Agents = []Agent{
	{"claude", "Claude", "claude"},
	{transcript.KindCodex, "Codex", "codex"},
	{transcript.KindAgy, "Gemini", "agy"},
}

// AgentByID is the agent called id: claude, codex, or agy (gemini too).
func AgentByID(id string) (Agent, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "gemini" {
		id = transcript.KindAgy
	}
	for _, a := range Agents {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

// AgentOf is the agent a transcript of kind belongs to.
func AgentOf(kind string) Agent {
	if a, ok := AgentByID(kind); ok && kind != "" {
		return a
	}
	return Agents[0]
}

// SwitchPRs is a conversation's PRs for the brief: a line for each (open
// ones with their status) and the open ones' addresses. The github package
// sets it (it imports this one); nil lists none.
var SwitchPRs func(conv transcript.Conversation) (lines, open []string)

// ClaimUnowned claims, for the running session called to, each of prs that
// no running session owns, returning those claimed. Set by the github
// package; nil claims nothing.
var ClaimUnowned func(prs []string, to string) []string

// SwitchOpts says what to switch and to which agent.
type SwitchOpts struct {
	Name    string
	ID      string // the session it should be (required for one not running and with no launch record)
	To      string // claude, codex or agy
	Force   bool   // a running Codex or agy session mid-turn: stop it where it is
	Timeout time.Duration
	OnTab   func(tty string) // called once the new tab is open
}

// switchSource is the session a switch replaces.
type switchSource struct {
	name, id, kind string
	path, cwd      string
	running        bool
	pid            int
	start          string // its process's start (ANSIC), for its running tasks
	status         string
	conv           *transcript.Conversation
}

// kindOfSession is the kind of transcript id has, and its path.
func kindOfSession(id string) (kind, path string, ok bool) {
	if p := transcript.TranscriptPath(id); p != "" && !strings.ContainsAny(id, `/\*?[`) {
		return "", p, true
	}
	if p := transcript.CodexRollout(id); p != "" {
		return transcript.KindCodex, p, true
	}
	if p := transcript.AgyTranscript(id); p != "" {
		return transcript.KindAgy, p, true
	}
	return "", "", false
}

// parseKind parses the transcript at path as kind's.
func parseKind(kind, path string) (*transcript.Conversation, error) {
	switch kind {
	case transcript.KindCodex:
		return transcript.ParseCodexFile(path, time.Time{}, 0)
	case transcript.KindAgy:
		return transcript.ParseAgyFile(path, time.Time{}, 0)
	}
	return transcript.ParseConversationFile(path, time.Time{}, 0)
}

// findSwitchSource is the session called name: the one running under it,
// else the session id given or recorded for the name by its launch.
func findSwitchSource(name, id string) (switchSource, error) {
	s := switchSource{name: name}
	pids, err := Named(name)
	if err != nil {
		return s, err
	}
	switch len(pids) {
	case 0:
		if id == "" {
			id = LoadParents().Parents[name].ChildID
		}
		if id == "" {
			return s, fmt.Errorf("no running session named %s, and no launch recorded one: give its session id", name)
		}
	case 1:
		s.running, s.pid = true, pids[0]
		got := ""
		for sid, ls := range ScanSessions(nil) {
			if ls.PID == s.pid {
				got, s.start, s.status = sid, ls.Start, ls.Stat.Status
				break
			}
		}
		if got == "" {
			if got, err = IdentityOf(s.pid); err != nil {
				return s, fmt.Errorf("%s (pid %d): %v", name, s.pid, err)
			}
		}
		if id != "" && got != id {
			return s, fmt.Errorf("%s (pid %d) isn't session %s any more: nothing was changed", name, s.pid, id)
		}
		id = got
		if proc.TTYOf(s.pid) == "" {
			return s, fmt.Errorf("%s (pid %d) has no terminal: an app or claude --bg started it, so ccs can't close it to switch", name, s.pid)
		}
	default:
		return s, fmt.Errorf("%d running sessions are named %s (pids %v): switch one by hand", len(pids), name, pids)
	}
	kind, path, ok := kindOfSession(id)
	if !ok {
		return s, fmt.Errorf("no transcript for session %s, so there is nothing to hand over", id)
	}
	conv, err := parseKind(kind, path)
	if err != nil || conv == nil {
		return s, fmt.Errorf("couldn't read %s's transcript %s: %v", name, path, err)
	}
	s.id, s.kind, s.path, s.conv = id, kind, path, conv
	s.cwd = conv.Cwd
	if s.running && kind == "" {
		if f, err := ReadSessionFile(s.pid); err == nil && f.Cwd != "" {
			s.cwd = f.Cwd
		}
	}
	if s.cwd == "" || s.cwd == "unknown" {
		return s, fmt.Errorf("%s's transcript doesn't say which folder it ran in", name)
	}
	return s, nil
}

// SwitchName is a valid session name made from title: a session with no
// name of its own is switched under one made from its topic.
func SwitchName(title string) string {
	if state.SessionName.MatchString(title) {
		return title
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_'
		switch {
		case ok:
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		name = "switched"
	}
	return name
}

// switchNoticeText is what a running Claude Code session is asked before
// it's closed: a handover note the new session's brief carries.
func switchNoticeText(timeout time.Duration, ref, name string, to Agent) string {
	return fmt.Sprintf("This session's work is moving to a new %[1]s session that takes its place under the same name, %[2]s (notice %[3]s). "+
		"It gets only a brief built from this transcript, not your memory. Stop at a safe point now and start nothing new. "+
		"Commit work in progress on its branch; keep your worktrees, branches and open PRs as they are, since the new session carries on in them; stop the background tasks you started. "+
		"Then write one message in this conversation as a handover to the new session: the task, what you finished (with PR URLs), what's left in the order you'd do it, "+
		"and anything only you know (worktree paths, branches, gotchas, who is waiting on what). It is copied into the new session's brief. "+
		"The session is stopped as soon as you go idle after this, or in %[4]s at the latest.", to.Label, name, ref, timeout)
}

// SwitchNote heads the new session's brief.
const SwitchNote = "[Automatic notice from ccs switch, run by the user or by the session that coordinates theirs. Not typed by the user.]\n\n"

// Brief limits, in characters: the whole brief stays far below what any of
// the agents takes as a first prompt on the command line.
const (
	briefFirstCap  = 4000  // the old session's first prompt
	briefNoteCap   = 6000  // its handover note
	briefMsgCap    = 1500  // each recent message
	briefRecentCap = 16000 // the recent messages together
	briefRecentMax = 40    // and at most this many
)

// cutRunes is s cut to n characters, marked when cut.
func cutRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + " […cut]"
	}
	return s
}

// briefIn is what a switch brief is made of.
type briefIn struct {
	name             string
	from, to         Agent
	oldID, path, cwd string
	git              []string
	first, note      string
	noteMissing      string // why there is no handover note, when the old session was asked for one
	prs              []string
	tasks            []string
	recent           []transcript.Message
}

// briefMessages splits conv's messages for the brief: its first prompt, the
// handover note (its replies after the notice marked ref) and the recent
// conversation before that, harness notes and the notice left out.
func briefMessages(conv *transcript.Conversation, ref string) (first, note string, recent []transcript.Message) {
	var kept []transcript.Message
	noticeAt := -1
	for _, m := range conv.Messages {
		if m.Role != "user" && m.Role != "assistant" || strings.TrimSpace(m.Text) == "" {
			continue
		}
		if ref != "" && m.Role == "user" && strings.Contains(m.Text, ref) {
			noticeAt = len(kept)
			continue
		}
		if m.Role == "user" {
			if _, _, ok := transcript.HarnessNote(m.Text); ok {
				continue
			}
		}
		kept = append(kept, m)
	}
	firstAt := -1
	for i, m := range kept {
		if m.Role == "user" {
			first, firstAt = m.Text, i
			break
		}
	}
	if noticeAt >= 0 {
		var parts []string
		for _, m := range kept[noticeAt:] {
			if m.Role == "assistant" {
				parts = append(parts, strings.TrimSpace(m.Text))
			}
		}
		note = strings.Join(parts, "\n\n")
		kept = kept[:noticeAt]
	}
	budget := briefRecentCap
	start := len(kept)
	for start > firstAt+1 && len(kept)-start < briefRecentMax { // the first prompt has its own section
		n := len([]rune(cutRunes(kept[start-1].Text, briefMsgCap)))
		if n > budget {
			break
		}
		budget -= n
		start--
	}
	return first, note, kept[start:]
}

// withoutPreamble drops the launch preamble from the start of a first
// prompt: the new session's brief gets it again on its own.
func withoutPreamble(first string) string {
	pre, err := os.ReadFile(PreamblePath())
	if p := strings.TrimSpace(string(pre)); err == nil && p != "" {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(first), p); ok {
			return strings.TrimSpace(rest)
		}
	}
	return first
}

// quote is text as a markdown quote, so headings in it (an earlier switch's
// brief) don't read as the brief's own.
func quote(text string) string {
	return "> " + strings.ReplaceAll(text, "\n", "\n> ")
}

// switchBrief is the new session's first prompt.
func switchBrief(in briefIn) string {
	var b strings.Builder
	b.WriteString(SwitchNote)
	fmt.Fprintf(&b, "You are taking over the work of %s, a %s session (id %s) that ccs has just switched to %s. "+
		"That session is closed; you take its place under the same name and in the same folder, with its parent, its child sessions and its PR claims. "+
		"You don't have its memory, only this brief, built from its transcript. If you need more, its whole transcript is %s.\n",
		in.name, in.from.Label, in.oldID, in.to.Label, in.path)
	section := func(title string) { b.WriteString("\n## " + title + "\n\n") }
	section("Folder")
	b.WriteString(in.cwd + "\n")
	for _, l := range in.git {
		b.WriteString(l + "\n")
	}
	if in.first != "" {
		section("Its first prompt, quoted")
		b.WriteString(quote(cutRunes(in.first, briefFirstCap)) + "\n")
	}
	switch {
	case in.note != "":
		section("Its handover note, written for you just before it closed")
		b.WriteString(cutRunes(in.note, briefNoteCap) + "\n")
	case in.noteMissing != "":
		section("Its handover note")
		b.WriteString("None: " + in.noteMissing + ". Work out what's left from the conversation below.\n")
	}
	section("Its PRs")
	if len(in.prs) == 0 {
		b.WriteString("None found in its transcript.\n")
	}
	for _, p := range in.prs {
		b.WriteString("- " + p + "\n")
	}
	if len(in.tasks) > 0 {
		section("Background tasks it was running, which stopped with it")
		for _, t := range in.tasks {
			b.WriteString("- " + t + "\n")
		}
		b.WriteString("Start again the ones still needed.\n")
	}
	if len(in.recent) > 0 {
		section(fmt.Sprintf("The last %d messages, oldest first", len(in.recent)))
		for _, m := range in.recent {
			who := "User"
			if m.Role == "assistant" {
				who = in.from.Label
			}
			fmt.Fprintf(&b, "**%s:** %s\n\n", who, cutRunes(m.Text, briefMsgCap))
		}
	}
	b.WriteString("\n## Now\n\nCarry on from where it left off. If you own open PRs above, keep watching them as it did. If what's left isn't clear, say so in one line and wait.")
	return b.String()
}

// gitLines says which branch and worktree cwd is on, and whether it has
// uncommitted changes. Stubbed in tests.
var gitLines = func(cwd string) []string {
	run := func(args ...string) string {
		out, err := proc.RunBounded(5*time.Second, []string{"GIT_TERMINAL_PROMPT=0"}, "git", append([]string{"-C", cwd}, args...)...)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	top := run("rev-parse", "--show-toplevel")
	if top == "" {
		return []string{"Not a git repository."}
	}
	lines := []string{"Git: branch " + run("rev-parse", "--abbrev-ref", "HEAD") + " in " + top}
	if common, dir := run("rev-parse", "--path-format=absolute", "--git-common-dir"), run("rev-parse", "--path-format=absolute", "--git-dir"); common != "" && dir != "" && common != dir {
		lines = append(lines, "It is a git worktree of "+filepath.Dir(common)+".")
	}
	if st := run("status", "--porcelain"); st != "" {
		lines = append(lines, fmt.Sprintf("%d uncommitted change(s): git status shows them.", len(strings.Split(st, "\n"))))
	} else {
		lines = append(lines, "No uncommitted changes.")
	}
	return lines
}

// taskLines is conv's background tasks still running in the process that
// started at start (ANSIC, UTC).
func taskLines(conv *transcript.Conversation, start string) []string {
	since, err := time.ParseInLocation(time.ANSIC, start, time.UTC)
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range transcript.RunningTasks(*conv, since, time.Now()) {
		out = append(out, strings.TrimSpace(t.Kind+": "+t.Desc))
	}
	return out
}

// retarget puts the new session (newID, of agent to) where the old one
// (oldID) was in the tree: its own record now names it, and its children's
// records name it as their parent. Returns the children moved.
func retarget(name, oldID, newID string, to Agent) []string {
	kind := to.ID
	if kind == "claude" {
		kind = ""
	}
	if newID != "" {
		err := WithParents(func(f *ParentsFile) bool {
			r, ok := f.Parents[name]
			if !ok {
				return false
			}
			r.ChildID, r.Source, r.At = newID, "switch", time.Now().UTC()
			f.Parents[name] = r
			return true
		})
		if err != nil {
			state.LogActivity("switch %s: couldn't record it under its parent: %v", name, err)
		}
	}
	return moveChildren(name, oldID, HandoverParent{Name: name, ID: newID}, kind, "switch")
}

// switchLaunch is LaunchSession, stubbed in tests.
var switchLaunch = LaunchSession

// SwitchSession carries the session called o.Name on in agent o.To.
func SwitchSession(o SwitchOpts) (string, error) {
	to, ok := AgentByID(o.To)
	if !ok {
		return "", fmt.Errorf("no agent %q: use claude, codex or agy", o.To)
	}
	if o.Timeout <= 0 {
		o.Timeout = GracefulTimeout
	}
	if !state.SessionName.MatchString(o.Name) {
		return "", fmt.Errorf("name %q: use letters, digits, '.', '_' and '-' (at most 64)", o.Name)
	}
	src, err := findSwitchSource(o.Name, o.ID)
	if err != nil {
		return "", err
	}
	from := AgentOf(src.kind)
	if from.ID == to.ID {
		return "", fmt.Errorf("%s is already a %s session", o.Name, to.Label)
	}
	// Every refusal comes before anything is closed.
	if _, err := LookBin(to.Bin); err != nil {
		return "", fmt.Errorf("%s isn't installed (%s): nothing was changed", to.Label, to.Bin)
	}
	if !CanOpenTabs() {
		return "", errors.New("not in iTerm or tmux: a switch opens a tab there")
	}
	if err := CheckLaunchShell(); err != nil {
		return "", err
	}
	if info, err := os.Stat(src.cwd); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s's folder %s is gone: nothing was changed", o.Name, src.cwd)
	}
	if !proc.ShellSafe(src.cwd) {
		return "", fmt.Errorf("folder %q can't go into a shell command", src.cwd)
	}
	if src.running && IsCaller(src.pid) {
		return "", fmt.Errorf("%s can't switch itself: closing it would end this call. Ask the session that coordinates it, or run ccs switch from another tab", o.Name)
	}
	if src.running && src.kind != "" && midTurn(src.status) && !o.Force {
		return "", fmt.Errorf("%s is mid-turn, and a %s session can't be asked to wrap up first: wait for it to go idle, or force it to stop it where it is", o.Name, from.Label)
	}
	unlock, err := lockRestart(o.Name)
	if err != nil {
		return "", fmt.Errorf("%s: %v", o.Name, err)
	}
	defer unlock()

	// Close it first: the name can't run twice, and a running session can't
	// be renamed from outside.
	ref, closed, noteMissing := "", "", ""
	if src.running {
		if src.kind == "" {
			msg, g, err := closeGraceful(o.Name, src.id, o.Timeout, nil, func(r string) string { return switchNoticeText(o.Timeout, r, o.Name, to) })
			if err != nil {
				return "", fmt.Errorf("couldn't close %s, so nothing was switched: %v", o.Name, err)
			}
			ref, closed = g.ref, msg
			if !strings.Contains(g.how, "went idle") {
				noteMissing = g.how
			}
		} else {
			msg, err := CloseSession(o.Name, src.pid, src.id)
			if err != nil {
				return "", fmt.Errorf("couldn't close %s, so nothing was switched: %v", o.Name, err)
			}
			closed = msg
			noteMissing = "a " + from.Label + " session can't be asked for one, so it was closed as it was"
		}
		state.LogActivity("switch %s: %s", o.Name, closed)
		if c, err := parseKind(src.kind, src.path); err == nil && c != nil {
			src.conv = c // now with the handover note
		}
	}

	first, note, recent := briefMessages(src.conv, ref)
	in := briefIn{
		name: o.Name, from: from, to: to, oldID: src.id, path: src.path, cwd: src.cwd,
		git: gitLines(src.cwd), first: withoutPreamble(first), note: note, recent: recent,
	}
	if note == "" && src.running {
		in.noteMissing = noteMissing
		if in.noteMissing == "" {
			in.noteMissing = "it said nothing after being asked"
		}
	}
	var open []string
	if SwitchPRs != nil {
		in.prs, open = SwitchPRs(*src.conv)
	}
	if src.running {
		in.tasks = taskLines(src.conv, src.start)
	}
	lo := LaunchOpts{Name: o.Name, Brief: switchBrief(in), Cwd: src.cwd, Codex: to.ID == transcript.KindCodex, Agy: to.ID == transcript.KindAgy, OnTab: o.OnTab}
	l, err := switchLaunch(lo)
	if err != nil && l.TTY == "" {
		state.LogActivity("switch %s: launch failed (%v), trying again in %s", o.Name, err, RestartRetry)
		Sleep(RestartRetry)
		l, err = switchLaunch(lo)
	}
	if err != nil {
		back := fmt.Sprintf("ccs launch --name %s --resume %s --cwd %s", o.Name, src.id, proc.ShellQuote(src.cwd))
		if src.kind != "" {
			back = fmt.Sprintf("ccs launch --%s --name %s --resume %s --cwd %s", src.kind, o.Name, src.id, proc.ShellQuote(src.cwd))
		}
		return "", fmt.Errorf("couldn't start %s in %s: %v. Its brief is in %s; bring the old one back with: %s", o.Name, to.Label, err, filepath.Join(state.LaunchDir(), o.Name, "brief.md"), back)
	}
	newID := l.thread
	if newID == "" && l.PID > 0 {
		if f, err := ReadSessionFile(l.PID); err == nil {
			newID = f.SessionID
		}
	}
	children := retarget(o.Name, src.id, newID, to)
	var claims []string
	if !l.Starting {
		if MoveClaims != nil {
			moved, err := MoveClaims(src.id, o.Name)
			if err != nil {
				state.LogActivity("switch %s: couldn't move its PR claims: %v", o.Name, err)
			}
			claims = append(claims, moved...)
		}
		if ClaimUnowned != nil {
			claims = append(claims, ClaimUnowned(open, o.Name)...)
		}
	}
	state.LogActivity("switch %s: %s to %s (%s); children %v, claims %v", o.Name, from.Label, to.Label, l.Text(o.Name), children, claims)
	var b strings.Builder
	fmt.Fprintf(&b, "switched %s from %s to %s: %s", o.Name, from.Label, to.Label, l.Text(o.Name))
	if closed != "" {
		b.WriteString("\nold session: " + closed)
	} else {
		b.WriteString("\nold session: wasn't running, left as it was")
	}
	if l.Starting {
		b.WriteString("\nits PR claims move once it's up: run ccs claim from it")
	}
	fmt.Fprintf(&b, "\nmoved to it: %d PR claim(s), %d child session(s); its brief: %s", len(claims), len(children), filepath.Join(state.LaunchDir(), o.Name, "brief.md"))
	return b.String(), nil
}

// RunSwitch is `ccs switch`.
func RunSwitch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "the agent to carry on in: claude, codex or agy")
	id := fs.String("id", "", "the session id it should be; needed for one that isn't running and that ccs launch didn't start")
	force := fs.Bool("force", false, "a running Codex or agy session mid-turn: stop it where it is")
	timeout := fs.Duration("timeout", GracefulTimeout, "a running Claude Code session: how long to wait for its handover note before closing it anyway")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs switch <name> --to claude|codex|agy [--id <session id>] [--force] [--timeout 2m]\n\n"+
			"Carry a session on in another agent: close it (a running Claude Code session first writes a handover note), then start a new session in that agent "+
			"with the same name and folder, briefed with its first prompt, the note, its PRs, the git state and its last messages. "+
			"The new session takes its place under its parent, over its children, and takes its PR claims. It gets only the brief, not the old session's memory.")
		fs.PrintDefaults()
	}
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		name = ""
	}
	if name == "" || *to == "" {
		fs.Usage()
		return 2
	}
	msg, err := SwitchSession(SwitchOpts{Name: name, ID: *id, To: *to, Force: *force, Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(stderr, "ccs switch:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

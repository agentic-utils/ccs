package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ccs reorg regroups the sessions by topic so no orchestrator looks after
// more than reorgMaxTopics of them. A model (Sonnet, one claude -p call with
// no tools) reads one line per session and returns topics, each with its
// orchestrator and the sessions in it. Once the user accepts it, ccs
// launches the new orchestrators it names, records every member under its
// topic's orchestrator (launch-parents.json, source "reorg"), and tells each
// live orchestrator what it gained or lost and each live member that moved
// who it reports to now, in ccs reorg's own envelope. Closed sessions are only
// regrouped in the tree, never resumed or told. It runs only when the user
// asks, ^X M or `ccs reorg`, and shows the plan first: nothing launches or
// moves until they accept it. Each run is logged to the activity log.
// See docs/design/relay.md for why that's the line.

const (
	reorgMaxTopics   = 3
	reorgSender      = "ccs reorg"
	reorgNote        = "[Automatic notice from ccs reorg, which regroups the user's sessions by topic so no orchestrator looks after too many. Not typed by the user and not from another session: there is no one to reply to.]\n\n"
	reorgTimeout     = 10 * time.Minute
	reorgPromptRunes = 200
)

// reorgRow is one session as the model sees it, keyed by a short id.
type reorgRow struct {
	Key    string
	Conv   transcript.Conversation
	Live   bool
	PID    int
	Parent string // its recorded parent's name, before the reorg
}

func (r reorgRow) name() string { return cmp.Or(r.Conv.Title, r.Conv.SessionID) }

// reorgTopic is one topic of the model's plan.
type reorgTopic struct {
	Topic        string   `json:"topic"`
	Orchestrator string   `json:"orchestrator"`
	Parent       string   `json:"parent"`
	Folder       string   `json:"folder"`
	Members      []string `json:"members"`
}

type reorgPlan struct {
	Topics []reorgTopic `json:"topics"`
}

const reorgSchema = `{"type":"object","properties":{"topics":{"type":"array","items":{"type":"object","properties":{` +
	`"topic":{"type":"string"},"orchestrator":{"type":"string"},"parent":{"type":"string"},"folder":{"type":"string"},` +
	`"members":{"type":"array","items":{"type":"string"}}},"required":["topic","orchestrator","parent","folder","members"]}}},"required":["topics"]}`

const reorgSystem = "You organise a user's coding-agent sessions into topics under orchestrator sessions. Answer only with the structured result."

// reorgRows is every session the model sees: the listed ones that aren't
// headless runs, oldest first, each with a unique short key.
func reorgRows(convs []transcript.Conversation, live map[string]sessions.LiveSession) []reorgRow {
	parents := sessions.ParentNames(convs)
	var rows []reorgRow
	keys := map[string]bool{}
	for _, c := range convs {
		if c.Headless() {
			continue
		}
		key := c.SessionID
		for n := 8; n < len(c.SessionID); n++ {
			if k := c.SessionID[:n]; !keys[k] {
				key = k
				break
			}
		}
		keys[key] = true
		s, ok := live[c.SessionID]
		rows = append(rows, reorgRow{Key: key, Conv: c, Live: ok, PID: s.PID, Parent: parents[c.SessionID]})
	}
	slices.SortStableFunc(rows, func(a, b reorgRow) int { return cmp.Compare(a.Conv.FirstTimestamp, b.Conv.FirstTimestamp) })
	return rows
}

// firstPrompt is a session's first user message on one line, cut short.
func firstPrompt(c transcript.Conversation) string {
	for _, msg := range c.Messages {
		if msg.Role == "user" {
			t := strings.Join(strings.Fields(msg.Text), " ")
			if r := []rune(t); len(r) > reorgPromptRunes {
				t = string(r[:reorgPromptRunes]) + "…"
			}
			return t
		}
	}
	return ""
}

func reorgPrompt(rows []reorgRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, `Regroup these sessions by topic so that no orchestrator looks after more than %d topics.

- A topic is one area of work, such as one repository's feature or one investigation. Put every session that is clearly about a topic in its members, by key. Leave out sessions with no clear topic, such as one-off questions.
- Each topic has an orchestrator, named in "orchestrator": an existing session (by name) that already runs that work (see "launched" and "parent"), or a new session for you to name: short plain words ending in -orchestrator. For a new one, "folder" is where it should start: the folder most of its members work in. For an existing one, "folder" is "".
- No orchestrator may have more than %d topics. Where one would, split its topics between new orchestrators placed under it.
- "parent" is the orchestrator above this topic's orchestrator, by name, or "" at the top. Keep the existing top orchestrator (the one that launched most and has no parent) at the top.
- "parent" and "launched" in the rows are records of who started whom: weigh them heavily, but move a session where its topic clearly belongs to another orchestrator.
- An orchestrator is never a member of its own topic, and the tree has no loops. Use every key at most once.

Rows, oldest first, tab-separated: key, name, kind, live or closed, folder, started, last active, recorded parent, sessions it launched, first prompt.
`, reorgMaxTopics, reorgMaxTopics)
	for _, r := range rows {
		c := r.Conv
		state := "closed"
		if r.Live {
			state = "live"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Key, cmp.Or(c.Title, "-"), cmp.Or(c.Kind, "claude"), state,
			sessions.TildePath(groupKey(c.Cwd)), shortTime(c.FirstTimestamp), shortTime(c.LastTimestamp), cmp.Or(r.Parent, "-"),
			cmp.Or(strings.Join(c.Launches, ","), "-"), firstPrompt(c))
	}
	return b.String()
}

func shortTime(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UTC().Format("2006-01-02T15:04")
	}
	return "-"
}

// reorgModel runs the model on prompt: claude -p with Sonnet, no tools and
// no MCP servers, nothing saved, the plan as structured output. Stubbed in
// tests.
var reorgModel = func(ctx context.Context, prompt string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", "sonnet", "--tools", "", "--strict-mcp-config",
		"--no-session-persistence", "--system-prompt", reorgSystem, "--output-format", "json", "--json-schema", reorgSchema)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = 5 * time.Second
	return cmd.Output()
}

// parseReorg reads claude -p's JSON output (a list of events, or the result
// alone) into the plan.
func parseReorg(out []byte) (reorgPlan, error) {
	type result struct {
		Type       string          `json:"type"`
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
	}
	var events []result
	if json.Unmarshal(out, &events) != nil {
		var one result
		if err := json.Unmarshal(out, &one); err != nil {
			return reorgPlan{}, fmt.Errorf("the model's output isn't JSON: %w", err)
		}
		events = []result{one}
	}
	for _, e := range slices.Backward(events) {
		if e.Type != "result" {
			continue
		}
		if e.IsError {
			return reorgPlan{}, fmt.Errorf("the model failed: %s", e.Result)
		}
		raw := e.Structured
		if len(raw) == 0 {
			raw = json.RawMessage(e.Result)
		}
		var p reorgPlan
		if err := json.Unmarshal(raw, &p); err != nil {
			return reorgPlan{}, fmt.Errorf("the model's plan: %w", err)
		}
		return p, nil
	}
	return reorgPlan{}, errors.New("the model gave no result")
}

// reorgLaunch starts a new orchestrator; reorgNotify sends a live session a
// notice in ccs reorg's envelope, with no typing fallback. Stubbed in tests.
var (
	reorgLaunch = sessions.LaunchSession
	reorgNotify = func(pid int, text string) error {
		f, err := sessions.ReadSessionFile(pid)
		if err != nil {
			return err
		}
		if f.Socket == "" {
			return errors.New("it has no message socket")
		}
		_, err = sessions.SendEnvelope(f.Socket, sessions.PeerToken(pid, f.Socket), reorgSender, reorgNote, text, false)
		return err
	}
)

// reorgResult is what a reorg did, for the note, the CLI and the marker.
type reorgResult struct {
	Topics, Orchestrators int
	Launched              []string
	Moved, Told           int
	Errs                  []string
}

func (r reorgResult) String() string {
	s := fmt.Sprintf("%d topics under %d orchestrators", r.Topics, r.Orchestrators)
	if len(r.Launched) > 0 {
		s += "; launched " + strings.Join(r.Launched, ", ")
	}
	s += fmt.Sprintf("; %d sessions moved, %d live ones told", r.Moved, r.Told)
	if len(r.Errs) > 0 {
		s += "; problems: " + strings.Join(r.Errs, "; ")
	}
	return s
}

// orchestratorBriefFor is a new topic orchestrator's brief.
func orchestratorBriefFor(name, parent string, topics []string, live, closed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, an orchestrator that ccs reorg started so that no orchestrator looks after more than %d topics. ", name, reorgMaxTopics)
	fmt.Fprintf(&b, "Your topics: %s.\n\n", strings.Join(topics, "; "))
	fmt.Fprintf(&b, "Follow the orchestrator role in %s for how to run sessions, with these differences:\n", sessions.TildePath(filepath.Join(orchestratorDir(), "CLAUDE.md")))
	if parent != "" {
		fmt.Fprintf(&b, "- You report to %s: send it a short status with SendMessage when something needs the user, and take new asks on your topics from it.\n", parent)
	}
	b.WriteString("- Don't start `ccs watch` if one is already running.\n")
	b.WriteString("- Look after only the sessions below and the ones you launch.\n\n")
	if len(live) > 0 {
		fmt.Fprintf(&b, "Running sessions now yours (they've been told to report to you): %s. Read `ccs sessions --json` for what each is doing.\n", strings.Join(live, ", "))
	}
	if len(closed) > 0 {
		fmt.Fprintf(&b, "Earlier sessions on your topics, now closed (read them with read_session if you need their history; don't resume them unasked): %s.\n", strings.Join(closed, ", "))
	}
	return b.String()
}

// applyReorg carries out plan over rows: launches the new orchestrators,
// records every member under its orchestrator, and tells the live sessions
// whose place changed.
func applyReorg(plan reorgPlan, rows []reorgRow) reorgResult {
	var res reorgResult
	byKey, byName := map[string]reorgRow{}, map[string]reorgRow{}
	for _, r := range rows {
		byKey[r.Key] = r
		if r.Conv.Title == "" {
			continue
		}
		if old, ok := byName[r.Conv.Title]; !ok || (r.Live && !old.Live) || (r.Live == old.Live && r.Conv.LastTimestamp > old.Conv.LastTimestamp) {
			byName[r.Conv.Title] = r
		}
	}
	// The plan, cleaned: valid names, each key once, never in its own topic.
	type orch struct {
		name, parent, folder string
		topics               []string
		members              []reorgRow
	}
	orchs, order := map[string]*orch{}, []string{}
	used := map[string]bool{}
	for _, t := range plan.Topics {
		if !state.SessionName.MatchString(t.Orchestrator) {
			res.Errs = append(res.Errs, fmt.Sprintf("skipped topic %q: bad orchestrator name %q", t.Topic, t.Orchestrator))
			continue
		}
		o := orchs[t.Orchestrator]
		if o == nil {
			o = &orch{name: t.Orchestrator}
			orchs[t.Orchestrator] = o
			order = append(order, t.Orchestrator)
		}
		o.parent, o.folder = cmp.Or(o.parent, t.Parent), cmp.Or(o.folder, t.Folder)
		o.topics = append(o.topics, t.Topic)
		for _, k := range t.Members {
			r, ok := byKey[k]
			if !ok || used[k] || r.Conv.Title == t.Orchestrator {
				continue
			}
			used[k] = true
			o.members = append(o.members, r)
		}
	}
	res.Topics, res.Orchestrators = len(plan.Topics), len(orchs)

	// Who each orchestrator is: a running session by that name, else one
	// launched now.
	callers := map[string]sessions.Caller{}
	gains, losses := map[string][]string{}, map[string][]string{}
	for _, name := range order {
		o := orchs[name]
		if r, ok := byName[name]; ok && r.Live {
			callers[name] = sessions.Caller{ID: r.Conv.SessionID, Name: name, Kind: r.Conv.Kind}
			continue
		}
		var live, closed []string
		for _, m := range o.members {
			if m.Live {
				live = append(live, m.name())
			} else {
				closed = append(closed, m.name())
			}
		}
		folder := o.folder
		if strings.HasPrefix(folder, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				folder = filepath.Join(home, folder[2:])
			}
		}
		if info, err := os.Stat(folder); folder == "" || err != nil || !info.IsDir() {
			folder = orchestratorDir()
		}
		l, err := reorgLaunch(sessions.LaunchOpts{Name: name, Cwd: folder, Brief: orchestratorBriefFor(name, o.parent, o.topics, live, closed)})
		if err != nil && l.PID == 0 {
			res.Errs = append(res.Errs, fmt.Sprintf("couldn't launch %s: %v", name, err))
			continue
		}
		res.Launched = append(res.Launched, name)
		id := ""
		for sid, s := range sessions.ScanSessions(nil) {
			if s.PID == l.PID && l.PID > 0 {
				id = sid
			}
		}
		callers[name] = sessions.Caller{ID: id, Name: name}
	}

	// The records: each orchestrator under its parent, each member under its
	// orchestrator, where that changes anything.
	var recs []sessions.ParentRec
	rec := func(childName, childID string, p sessions.Caller) {
		recs = append(recs, sessions.ParentRec{Child: childName, ChildID: childID, Parent: p.Name, ParentID: p.ID, ParentKind: p.Kind, Source: "reorg"})
	}
	for _, name := range order {
		o := orchs[name]
		me, ok := callers[name]
		if !ok {
			continue
		}
		if p, ok := callers[o.parent]; ok && o.parent != name {
			rec(name, me.ID, p)
		} else if r, ok := byName[o.parent]; ok && o.parent != name {
			rec(name, me.ID, sessions.Caller{ID: r.Conv.SessionID, Name: o.parent, Kind: r.Conv.Kind})
		}
		for _, m := range o.members {
			if m.Parent == name {
				continue
			}
			rec(m.Conv.Title, m.Conv.SessionID, me)
			res.Moved++
			if m.Live {
				gains[name] = append(gains[name], m.name())
				if m.Parent != "" {
					losses[m.Parent] = append(losses[m.Parent], m.name())
				}
				if err := reorgNotify(m.PID, fmt.Sprintf("ccs reorg regrouped the user's sessions by topic. From now on you report to %s, not %s: send it your status and questions for the user.", name, cmp.Or(m.Parent, "no one"))); err != nil {
					res.Errs = append(res.Errs, fmt.Sprintf("couldn't tell %s: %v", m.name(), err))
				} else {
					res.Told++
				}
			}
		}
	}
	if err := sessions.SetParents(recs); err != nil {
		res.Errs = append(res.Errs, "couldn't save the new parents: "+err.Error())
	}

	// Existing live orchestrators hear what changed for them.
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		seen := map[string]bool{}
		for _, m := range []map[string][]string{gains, losses} {
			for k := range m {
				if !seen[k] && !slices.Contains(res.Launched, k) {
					seen[k] = true
					if !yield(k) {
						return
					}
				}
			}
		}
	}) {
		r, ok := byName[name]
		if !ok || !r.Live {
			continue
		}
		var b strings.Builder
		b.WriteString("ccs reorg regrouped the user's sessions by topic, so no orchestrator looks after more than " + fmt.Sprint(reorgMaxTopics) + " topics.")
		if o := orchs[name]; o != nil {
			b.WriteString(" Your topics: " + strings.Join(o.topics, "; ") + ".")
		}
		if g := gains[name]; len(g) > 0 {
			b.WriteString(" Now yours (told to report to you): " + strings.Join(g, ", ") + ".")
		}
		if l := losses[name]; len(l) > 0 {
			b.WriteString(" No longer yours, now with another orchestrator: " + strings.Join(l, ", ") + ". Leave them to it.")
		}
		if err := reorgNotify(r.PID, b.String()); err != nil {
			res.Errs = append(res.Errs, fmt.Sprintf("couldn't tell %s: %v", name, err))
		} else {
			res.Told++
		}
	}
	return res
}

// reorgProposal is a plan and the rows it was made from, shown before
// anything moves.
type reorgProposal struct {
	plan reorgPlan
	rows []reorgRow
}

// planReorg asks the model for a plan over convs. Nothing changes yet.
func planReorg(convs []transcript.Conversation) (reorgProposal, error) {
	rows := reorgRows(convs, sessions.ScanSessions(nil))
	ctx, cancel := context.WithTimeout(context.Background(), reorgTimeout)
	defer cancel()
	out, err := reorgModel(ctx, reorgPrompt(rows))
	if err != nil {
		state.LogActivity("reorg: claude -p failed: %v", err)
		return reorgProposal{}, fmt.Errorf("claude -p: %w", err)
	}
	plan, err := parseReorg(out)
	if err != nil {
		state.LogActivity("reorg: %v", err)
	}
	return reorgProposal{plan, rows}, err
}

// reorgLines is the plan as the user sees it before saying yes: each topic,
// its orchestrator (new or not, and under whom), and the sessions moving in.
func reorgLines(p reorgProposal) []string {
	byKey := map[string]reorgRow{}
	live := map[string]bool{}
	for _, r := range p.rows {
		byKey[r.Key] = r
		if r.Live && r.Conv.Title != "" {
			live[r.Conv.Title] = true
		}
	}
	var out []string
	for _, t := range p.plan.Topics {
		head := t.Topic + " → " + t.Orchestrator
		var notes []string
		if !live[t.Orchestrator] {
			notes = append(notes, "new")
		}
		if t.Parent != "" {
			notes = append(notes, "under "+t.Parent)
		}
		if len(notes) > 0 {
			head += " (" + strings.Join(notes, ", ") + ")"
		}
		out = append(out, head)
		var moving []string
		stay := 0
		for _, k := range t.Members {
			r, ok := byKey[k]
			if !ok || r.Conv.Title == t.Orchestrator {
				continue
			}
			if r.Parent == t.Orchestrator {
				stay++
				continue
			}
			what := r.name()
			if r.Parent != "" {
				what += " (from " + r.Parent + ")"
			}
			if !r.Live {
				what += " (closed)"
			}
			moving = append(moving, what)
		}
		if len(moving) > 0 {
			out = append(out, "  moving in: "+strings.Join(moving, ", "))
		}
		if stay > 0 {
			out = append(out, fmt.Sprintf("  staying: %d", stay))
		}
	}
	if len(out) == 0 {
		out = []string{"nothing to regroup"}
	}
	return out
}

// applyProposal carries out an accepted plan: one reorg at a time across
// processes (reorg.lock beside the marks), with who is live read again, since
// the plan may have sat on screen a while. Logged to the activity log.
func applyProposal(p reorgProposal) (reorgResult, error) {
	dir := filepath.Dir(state.MarksPath)
	os.MkdirAll(dir, 0o700)
	lock, err := os.OpenFile(filepath.Join(dir, "reorg.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return reorgResult{}, err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return reorgResult{}, errors.New("another reorg is running")
	}
	live := sessions.ScanSessions(nil)
	rows := slices.Clone(p.rows)
	for i := range rows {
		s, ok := live[rows[i].Conv.SessionID]
		rows[i].Live, rows[i].PID = ok, s.PID
	}
	res := applyReorg(p.plan, rows)
	plan, _ := json.Marshal(p.plan)
	state.LogActivity("reorg: %s; plan %s", res, plan)
	return res, nil
}

// runReorgCLI is `ccs reorg [--dry-run] [--yes]`: the plan, then y/N.
func runReorgCLI(args []string, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	dry, yes := false, false
	for _, a := range args {
		switch a {
		case "--dry-run", "-n":
			dry = true
		case "--yes", "-y":
			yes = true
		default:
			fmt.Fprintf(stderr, "usage: ccs reorg [--dry-run] [--yes]\n  regroups every session by topic with one Sonnet call, so no orchestrator looks after more than %d topics.\n  Shows the plan and asks before it launches orchestrators and tells live sessions who they report to.\n  --dry-run shows the plan only; --yes applies it without asking\n", reorgMaxTopics)
			return 2
		}
	}
	convs, err := transcript.GetConversations(time.Now().AddDate(0, 0, -60), 1024*1024*1024, []string{"observer-sessions"})
	if err != nil {
		fmt.Fprintln(stderr, "ccs reorg:", err)
		return 1
	}
	fmt.Fprintln(stderr, "Asking Sonnet to group", len(convs), "sessions by topic...")
	p, err := planReorg(convs)
	if err != nil {
		fmt.Fprintln(stderr, "ccs reorg:", err)
		return 1
	}
	fmt.Fprintln(stdout, strings.Join(reorgLines(p), "\n"))
	if dry || len(p.plan.Topics) == 0 {
		return 0
	}
	if !yes {
		if !interactive {
			fmt.Fprintln(stderr, "ccs reorg: not applied; run it in a terminal to answer, or with --yes")
			return 1
		}
		fmt.Fprint(stdout, "\nApply it? It may launch orchestrators and tell live sessions. [y/N] ")
		var answer string
		fmt.Fscanln(stdin, &answer)
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Not applied.")
			return 0
		}
	}
	res, err := applyProposal(p)
	if err != nil {
		fmt.Fprintln(stderr, "ccs reorg:", err)
		return 1
	}
	fmt.Fprintln(stdout, res)
	return 0
}

// reorgPlannedMsg is the model's plan, to show; reorgDoneMsg a finished one.
type (
	reorgPlannedMsg struct {
		p   reorgProposal
		err error
	}
	reorgDoneMsg struct {
		res reorgResult
		err error
	}
)

// reorgPlanCmd asks for a plan over the listed sessions, off the UI goroutine.
func (m model) reorgPlanCmd() tea.Cmd {
	convs := make([]transcript.Conversation, len(m.items))
	for i := range m.items {
		convs[i] = m.items[i].conv
	}
	return func() tea.Msg {
		p, err := planReorg(convs)
		return reorgPlannedMsg{p, err}
	}
}

func reorgApplyCmd(p reorgProposal) tea.Cmd {
	return func() tea.Msg {
		res, err := applyProposal(p)
		return reorgDoneMsg{res, err}
	}
}

// reorgPopup shows the plan; it owns the keyboard: enter applies, esc drops it.
func (m model) reorgPopup() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\033[1mMove sessions into topics, at most %d per orchestrator?\033[0m\n\n", reorgMaxTopics)
	lines := reorgLines(*m.reorgPlan)
	room := max(m.height-12, 3)
	width := max(min(m.width-12, 110), 30)
	for i, l := range lines {
		if i == room && len(lines) > room+1 {
			fmt.Fprintf(&b, "\033[90m… %d more lines (ccs reorg --dry-run lists them all)\033[0m\n", len(lines)-room)
			break
		}
		if strings.HasPrefix(l, "  ") {
			l = "\033[90m" + transcript.Truncate(l, width) + "\033[0m"
		} else {
			l = transcript.Truncate(l, width)
		}
		b.WriteString(l + "\n")
	}
	b.WriteString("\n\033[90mNothing moves or launches until you press enter.\n" + hints(keyEnter, "apply", keyEsc, "cancel") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(b.String())
}

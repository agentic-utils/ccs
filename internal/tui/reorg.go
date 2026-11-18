package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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
- "parent" and "launched" in the rows are records of who started whom: weigh them heavily. Leave a session where it is unless its orchestrator has too many topics or its topic clearly belongs to another orchestrator, so running this again changes little.
- An existing orchestrator is a live session others are under; never make a working session an orchestrator. Never move the top orchestrator.
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

// reorgModel runs the model on prompt: claude -p with Sonnet, kept apart
// from the user's setup as far as the CLI allows without dropping their
// login (--bare would): no tools, MCP servers, settings files (so no
// CLAUDE.md), hooks or commands, nothing saved, the plan as structured
// output. cmd.Output keeps stderr for the error. Stubbed in tests.
var reorgModel = func(ctx context.Context, prompt string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", "sonnet", "--tools", "", "--strict-mcp-config",
		"--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "--disable-slash-commands",
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

// orchestratorBriefFor is a new topic orchestrator's brief. Its topics are
// already cleaned (cleanTopic), so model text can't add lines to it.
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

// reorgTopicRunes caps a topic's text: it goes into a brief and the popup.
const reorgTopicRunes = 60

// cleanTopic is a topic's text as ccs uses it: no control characters or
// escape codes, one line, at most reorgTopicRunes.
func cleanTopic(t string) string {
	t = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, t)
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > reorgTopicRunes {
		t = string(r[:reorgTopicRunes]) + "…"
	}
	return t
}

// reorgOrch is one orchestrator of a checked plan: exactly what is shown and
// what applies.
type reorgOrch struct {
	Name    string
	New     bool       // launched by the reorg
	Parent  string     // the orchestrator it goes under; "" keeps its place (an existing one) or makes it a top one (a new one)
	Folder  string     // where a new one starts: one of its members' folders, else the orchestrator's
	Topics  []string   // at most reorgMaxTopics, cleaned
	Moving  []reorgRow // sessions whose orchestrator changes to this one
	Staying int        // members already under it
}

// reorgProposal is a checked plan, the rows it was made from and what the
// checks dropped, shown before anything moves.
type reorgProposal struct {
	plan    reorgPlan // as the model gave it, for the log
	orchs   []reorgOrch
	rows    []reorgRow
	dropped []string
}

// checkPlan turns the model's plan into what ccs will do, enforcing what the
// prompt only asks for:
//   - an orchestrator is a new session (a valid name nobody has) or a live
//     session that already has others under it, never a working session;
//   - the top orchestrators (those with others under them and no parent of
//     their own) never move, and no orchestrator is a member anywhere;
//   - an orchestrator's parent is a top one, so the tree is at most two
//     orchestrators deep, and nothing makes a loop;
//   - at most reorgMaxTopics topics an orchestrator, each key used once;
//   - a new orchestrator starts in one of its members' folders.
//
// What it drops is listed, so the popup is exactly what applies.
func checkPlan(plan reorgPlan, rows []reorgRow) reorgProposal {
	p := reorgProposal{plan: plan, rows: rows}
	byKey, byName := map[string]reorgRow{}, map[string]reorgRow{}
	hasKids := map[string]bool{}
	for _, r := range rows {
		byKey[r.Key] = r
		if r.Parent != "" {
			hasKids[r.Parent] = true
		}
		if r.Conv.Title == "" {
			continue
		}
		if old, ok := byName[r.Conv.Title]; !ok || (r.Live && !old.Live) || (r.Live == old.Live && r.Conv.LastTimestamp > old.Conv.LastTimestamp) {
			byName[r.Conv.Title] = r
		}
	}
	isOrch := func(name string) bool {
		r, ok := byName[name]
		return ok && (hasKids[name] || len(r.Conv.Launches) > 0)
	}
	isTop := func(name string) bool { return isOrch(name) && byName[name].Parent == "" && byName[name].Live }
	drop := func(format string, a ...any) { p.dropped = append(p.dropped, fmt.Sprintf(format, a...)) }

	// Which orchestrators the plan may use, and their topics.
	idx := map[string]int{}
	for _, t := range plan.Topics {
		topic := cleanTopic(t.Topic)
		name := t.Orchestrator
		switch r, exists := byName[name]; {
		case topic == "":
			drop("a topic with no name")
			continue
		case !state.SessionName.MatchString(name):
			drop("%q: %q isn't a session name", topic, cleanTopic(name))
			continue
		case exists && !r.Live:
			drop("%q: %s isn't running", topic, name)
			continue
		case exists && !isOrch(name):
			drop("%q: %s is a working session, not an orchestrator", topic, name)
			continue
		}
		i, ok := idx[name]
		if !ok {
			_, exists := byName[name]
			p.orchs = append(p.orchs, reorgOrch{Name: name, New: !exists, Parent: t.Parent})
			i = len(p.orchs) - 1
			idx[name] = i
		}
		if len(p.orchs[i].Topics) >= reorgMaxTopics {
			drop("%q: %s already has %d topics", topic, name, reorgMaxTopics)
			continue
		}
		p.orchs[i].Topics = append(p.orchs[i].Topics, topic)
	}

	// Parents: a top orchestrator, never itself; a top one keeps its place.
	for i := range p.orchs {
		o := &p.orchs[i]
		want := o.Parent
		o.Parent = ""
		switch {
		case !o.New && isTop(o.Name):
			// a top orchestrator never moves
		case want == "" || want == o.Name:
		case isTop(want):
			o.Parent = want
		default:
			drop("%s under %s: only a top orchestrator can have orchestrators under it", o.Name, cleanTopic(want))
		}
	}

	// Members: once each, never an orchestrator, moving only if it changes.
	used := map[string]bool{}
	for _, t := range plan.Topics {
		i, ok := idx[t.Orchestrator]
		if !ok || !slices.Contains(p.orchs[i].Topics, cleanTopic(t.Topic)) {
			continue
		}
		o := &p.orchs[i]
		for _, k := range t.Members {
			r, ok := byKey[k]
			switch {
			case !ok || used[k]:
				continue
			case r.Conv.Title != "" && (isOrch(r.Conv.Title) || slices.ContainsFunc(p.orchs, func(o reorgOrch) bool { return o.Name == r.Conv.Title })):
				if r.Conv.Title != o.Name {
					drop("%s is an orchestrator: it moves only as one", r.name())
				}
				continue
			}
			used[k] = true
			if r.Parent == o.Name {
				o.Staying++
			} else {
				o.Moving = append(o.Moving, r)
			}
		}
	}

	// Loops: walk each orchestrator's new chain of parents.
	final := map[string]string{}
	for _, r := range rows {
		if r.Conv.Title != "" && r.Parent != "" {
			final[r.Conv.Title] = r.Parent
		}
	}
	for i := range p.orchs {
		if o := &p.orchs[i]; o.Parent != "" {
			final[o.Name] = o.Parent
		}
	}
	for i := range p.orchs {
		o := &p.orchs[i]
		seen := map[string]bool{o.Name: true}
		for at := final[o.Name]; at != ""; at = final[at] {
			if seen[at] {
				drop("%s under %s: that would make a loop", o.Name, o.Parent)
				o.Parent = ""
				delete(final, o.Name)
				break
			}
			seen[at] = true
		}
	}

	// A new orchestrator's folder: the model's if a member works there, else
	// where most of its members do, else the orchestrator's own.
	for i := range p.orchs {
		o := &p.orchs[i]
		if !o.New {
			continue
		}
		count := map[string]int{}
		for _, r := range o.Moving {
			if f := groupKey(r.Conv.Cwd); f != "" {
				if info, err := os.Stat(f); err == nil && info.IsDir() {
					count[f]++
				}
			}
		}
		want := groupKey(expandHome(byKeyFolder(plan, o.Name)))
		switch {
		case count[want] > 0:
			o.Folder = want
		case len(count) > 0:
			o.Folder = slices.MaxFunc(slices.Sorted(maps.Keys(count)), func(a, b string) int { return cmp.Compare(count[a], count[b]) })
		default:
			o.Folder = orchestratorDir()
		}
	}

	// An orchestrator left with no topics, or a new one with nothing to look
	// after, goes.
	p.orchs = slices.DeleteFunc(p.orchs, func(o reorgOrch) bool {
		if len(o.Topics) == 0 {
			return true
		}
		if o.New && len(o.Moving) == 0 && o.Staying == 0 {
			drop("%s: nothing to look after", o.Name)
			return true
		}
		return false
	})
	return p
}

// byKeyFolder is the folder the model gave name's first topic.
func byKeyFolder(plan reorgPlan, name string) string {
	for _, t := range plan.Topics {
		if t.Orchestrator == name && t.Folder != "" {
			return t.Folder
		}
	}
	return ""
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// applyReorg carries out a checked plan: launches the new orchestrators,
// saves every change, then (only once that's saved) tells the live sessions
// whose place changed.
func applyReorg(p reorgProposal) reorgResult {
	var res reorgResult
	byName := map[string]reorgRow{}
	for _, r := range p.rows {
		if r.Conv.Title != "" && (r.Live || !byName[r.Conv.Title].Live) {
			byName[r.Conv.Title] = r
		}
	}
	res.Orchestrators = len(p.orchs)
	callers := map[string]sessions.Caller{}
	for _, o := range p.orchs {
		res.Topics += len(o.Topics)
		if !o.New {
			r := byName[o.Name]
			callers[o.Name] = sessions.Caller{ID: r.Conv.SessionID, Name: o.Name, Kind: r.Conv.Kind}
			continue
		}
		var live, closed []string
		for _, m := range o.Moving {
			if m.Live {
				live = append(live, m.name())
			} else {
				closed = append(closed, m.name())
			}
		}
		l, err := reorgLaunch(sessions.LaunchOpts{Name: o.Name, Cwd: o.Folder, Brief: orchestratorBriefFor(o.Name, o.Parent, o.Topics, live, closed)})
		if err != nil && l.PID == 0 {
			res.Errs = append(res.Errs, fmt.Sprintf("couldn't launch %s: %v", o.Name, err))
			continue
		}
		res.Launched = append(res.Launched, o.Name)
		id := ""
		for sid, s := range sessions.ScanSessions(nil) {
			if l.PID > 0 && s.PID == l.PID {
				id = sid
			}
		}
		callers[o.Name] = sessions.Caller{ID: id, Name: o.Name}
	}

	// Save first: nobody hears of a move that wasn't recorded.
	var recs []sessions.ParentRec
	type notice struct {
		pid  int
		text string
		who  string
	}
	var notices []notice
	gains, losses := map[string][]string{}, map[string][]string{}
	for _, o := range p.orchs {
		me, ok := callers[o.Name]
		if !ok {
			continue // its launch failed: its sessions stay where they were
		}
		if o.Parent != "" {
			pr := byName[o.Parent]
			recs = append(recs, sessions.ParentRec{Child: o.Name, ChildID: me.ID, Parent: o.Parent, ParentID: pr.Conv.SessionID, ParentKind: pr.Conv.Kind, Source: "reorg"})
		}
		for _, m := range o.Moving {
			recs = append(recs, sessions.ParentRec{Child: m.Conv.Title, ChildID: m.Conv.SessionID, Parent: o.Name, ParentID: me.ID, ParentKind: me.Kind, Source: "reorg"})
			res.Moved++
			if !m.Live {
				continue
			}
			gains[o.Name] = append(gains[o.Name], m.name())
			if m.Parent != "" {
				losses[m.Parent] = append(losses[m.Parent], m.name())
			}
			notices = append(notices, notice{m.PID, fmt.Sprintf("ccs reorg regrouped the user's sessions by topic. From now on you report to %s, not %s: send it your status and questions for the user.", o.Name, cmp.Or(m.Parent, "no one")), m.name()})
		}
	}
	if err := sessions.SetParents(recs); err != nil {
		res.Errs = append(res.Errs, "couldn't save the new parents, so nobody was told: "+err.Error())
		res.Moved = 0
		return res
	}

	topics := map[string][]string{}
	for _, o := range p.orchs {
		topics[o.Name] = o.Topics
	}
	names := slices.Sorted(maps.Keys(gains))
	for name := range losses {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		r, ok := byName[name]
		if !ok || !r.Live || slices.Contains(res.Launched, name) {
			continue
		}
		var b strings.Builder
		b.WriteString("ccs reorg regrouped the user's sessions by topic, so no orchestrator looks after more than " + fmt.Sprint(reorgMaxTopics) + " topics.")
		if t := topics[name]; len(t) > 0 {
			b.WriteString(" Your topics: " + strings.Join(t, "; ") + ".")
		}
		if g := gains[name]; len(g) > 0 {
			b.WriteString(" Now yours (told to report to you): " + strings.Join(g, ", ") + ".")
		}
		if l := losses[name]; len(l) > 0 {
			b.WriteString(" No longer yours, now with another orchestrator: " + strings.Join(l, ", ") + ". Leave them to it.")
		}
		notices = append(notices, notice{r.PID, b.String(), name})
	}
	for _, n := range notices {
		if err := reorgNotify(n.pid, n.text); err != nil {
			res.Errs = append(res.Errs, fmt.Sprintf("couldn't tell %s: %v", n.who, err))
		} else {
			res.Told++
		}
	}
	return res
}

// planReorg asks the model for a plan over convs and checks it. Nothing
// changes yet.
func planReorg(convs []transcript.Conversation) (reorgProposal, error) {
	rows := reorgRows(convs, sessions.ScanSessions(nil))
	ctx, cancel := context.WithTimeout(context.Background(), reorgTimeout)
	defer cancel()
	out, err := reorgModel(ctx, reorgPrompt(rows))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			tail := strings.TrimSpace(string(ee.Stderr))
			if r := []rune(tail); len(r) > 300 {
				tail = "…" + string(r[len(r)-300:])
			}
			err = fmt.Errorf("%w: %s", err, tail)
		}
		state.LogActivity("reorg: claude -p failed: %v", err)
		return reorgProposal{}, fmt.Errorf("claude -p: %w", err)
	}
	plan, err := parseReorg(out)
	if err != nil {
		state.LogActivity("reorg: %v", err)
		return reorgProposal{}, err
	}
	return checkPlan(plan, rows), nil
}

// reorgLines is the checked plan as the user sees it before saying yes:
// every orchestrator (new or not, where it starts, under whom), its topics
// and the sessions moving to it, then what the checks dropped. Nothing is
// left out: the popup scrolls instead.
func reorgLines(p reorgProposal) []string {
	var out []string
	for _, o := range p.orchs {
		head := strings.Join(o.Topics, "; ") + " → " + o.Name
		var notes []string
		if o.New {
			notes = append(notes, "new, in "+sessions.TildePath(o.Folder))
		}
		if o.Parent != "" {
			notes = append(notes, "under "+o.Parent)
		}
		if len(notes) > 0 {
			head += " (" + strings.Join(notes, ", ") + ")"
		}
		out = append(out, head)
		var moving []string
		for _, r := range o.Moving {
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
		if o.Staying > 0 {
			out = append(out, fmt.Sprintf("  staying: %d", o.Staying))
		}
	}
	if len(out) == 0 {
		out = []string{"nothing to regroup"}
	}
	for _, d := range p.dropped {
		out = append(out, "  left out: "+d)
	}
	return out
}

// moves counts the sessions a plan moves and the orchestrators it launches.
func (p reorgProposal) moves() int {
	n := 0
	for _, o := range p.orchs {
		n += len(o.Moving)
		if o.New {
			n++
		}
	}
	return n
}

// stale says what changed since the plan was made: a session moving (or an
// orchestrator in it) that started or stopped running, or got another parent.
func stale(p reorgProposal, live map[string]sessions.LiveSession) string {
	convs := make([]transcript.Conversation, len(p.rows))
	for i, r := range p.rows {
		convs[i] = r.Conv
	}
	parents := sessions.ParentNames(convs)
	check := func(r reorgRow) string {
		if _, ok := live[r.Conv.SessionID]; ok != r.Live {
			return r.name() + map[bool]string{true: " stopped", false: " started"}[r.Live]
		}
		if parents[r.Conv.SessionID] != r.Parent {
			return r.name() + " moved"
		}
		return ""
	}
	byName := map[string]reorgRow{}
	for _, r := range p.rows {
		if r.Conv.Title != "" && (r.Live || !byName[r.Conv.Title].Live) {
			byName[r.Conv.Title] = r
		}
	}
	for _, o := range p.orchs {
		if r, ok := byName[o.Name]; ok {
			if s := check(r); s != "" {
				return s
			}
		} else if names, _ := sessions.Named(o.Name); len(names) > 0 {
			return o.Name + " started"
		}
		for _, r := range o.Moving {
			if s := check(r); s != "" {
				return s
			}
		}
	}
	return ""
}

// applyProposal carries out an accepted plan: one reorg at a time across
// processes (reorg.lock beside the marks), and only if nothing it touches
// changed since it was shown. Logged to the activity log.
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
	if what := stale(p, live); what != "" {
		return reorgResult{}, fmt.Errorf("not applied: %s since the plan was made, so ask for a new one", what)
	}
	for i := range p.rows { // live sessions' pids, for the notices
		p.rows[i].PID = live[p.rows[i].Conv.SessionID].PID
	}
	for i := range p.orchs {
		for j := range p.orchs[i].Moving {
			p.orchs[i].Moving[j].PID = live[p.orchs[i].Moving[j].Conv.SessionID].PID
		}
	}
	res := applyReorg(p)
	plan, _ := json.Marshal(p.plan)
	state.LogActivity("reorg: %s; left out %v; plan %s", res, p.dropped, plan)
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
	if dry || p.moves() == 0 {
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

// reorgPopupLines is the popup's text wrapped to its width, every line of
// the plan; reorgRoom how many fit on screen at once.
func (m model) reorgPopupLines() []string {
	width := max(min(m.width-12, 110), 30)
	var out []string
	for _, l := range reorgLines(*m.reorgPlan) {
		grey := strings.HasPrefix(l, "  ")
		for _, w := range strings.Split(ansi.Wrap(l, width, ""), "\n") {
			if grey {
				w = "\033[90m" + w + "\033[0m"
			}
			out = append(out, w)
		}
	}
	return out
}

func (m model) reorgRoom() int { return max(m.height-14, 3) }

// reorgAtEnd: the plan's last line has been on screen, so enter may apply it.
func (m model) reorgAtEnd() bool {
	return m.reorgScroll+m.reorgRoom() >= len(m.reorgPopupLines())
}

// scrollReorg moves the popup's view by n lines, within the plan.
func (m *model) scrollReorg(n int) {
	m.reorgScroll = max(min(m.reorgScroll+n, len(m.reorgPopupLines())-m.reorgRoom()), 0)
}

// reorgPopup shows the whole checked plan, scrolling when it's taller than
// the screen; it owns the keyboard: enter applies (once the end has been
// shown, and not in the first updateKeyGrace), esc drops it.
func (m model) reorgPopup() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\033[1mMove sessions into topics, at most %d per orchestrator?\033[0m\n\n", reorgMaxTopics)
	lines := m.reorgPopupLines()
	room := m.reorgRoom()
	end := min(m.reorgScroll+room, len(lines))
	for _, l := range lines[m.reorgScroll:end] {
		b.WriteString(l + "\n")
	}
	keys := []string{keyEnter, "apply", keyEsc, "cancel"}
	if len(lines) > room {
		fmt.Fprintf(&b, "\033[90mlines %d-%d of %d\033[0m\n", m.reorgScroll+1, end, len(lines))
		keys = append([]string{"↑↓ pgup/dn", "scroll"}, keys...)
		if !m.reorgAtEnd() {
			keys[3] = "show the rest"
		}
	}
	b.WriteString("\n\033[90mNothing moves or launches until you press enter.\n" + hints(keys...) + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(b.String())
}

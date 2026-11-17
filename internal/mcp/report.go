package mcp

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// `ccs orchestrator-report` and orchestrator_report: each orchestrator's
// scorecard over a period, from what is already on disk: pr-events.jsonl
// (PR events and the watcher's session_* events), launch-parents.json (who
// launched whom) and the orchestrator's own transcript. Every number says how
// it is counted, so the orchestrator can check it against the evidence.

// Span limits: more direct sessions than maxSpan, or a tree deeper than
// maxDepth, is a breach the scorecard flags.
const (
	maxSpan  = 6
	maxDepth = 2
)

// responseCap is how long after an event the orchestrator's first mention of
// it still counts as its response; later, or never, it was missed.
const responseCap = 4 * time.Hour

// topName is what the orchestrator recorded in orchestrator.json is called
// when no session name for it is known.
const topName = "orchestrator"

// Dist is a set of durations, in minutes.
type Dist struct {
	N      int     `json:"n"`
	P50    float64 `json:"p50_minutes"`
	P90    float64 `json:"p90_minutes"`
	Missed int     `json:"missed,omitempty"` // response: events never acted on within responseCap
	Open   int     `json:"open,omitempty"`   // done to close: still done at the end of the period
}

func dist(mins []float64) Dist {
	d := Dist{N: len(mins)}
	if len(mins) == 0 {
		return d
	}
	s := slices.Clone(mins)
	sort.Float64s(s)
	at := func(q float64) float64 {
		i := int(math.Ceil(q*float64(len(s)))) - 1
		return math.Round(s[max(i, 0)]*10) / 10
	}
	d.P50, d.P90 = at(0.5), at(0.9)
	return d
}

// Scorecard is one orchestrator's period.
type Scorecard struct {
	Orchestrator string    `json:"orchestrator"`
	SessionID    string    `json:"session_id,omitempty"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`

	// The tree: sessions under it (all depths), its direct ones running
	// now, its depth, and breaches of maxSpan/maxDepth.
	Sessions int      `json:"sessions"`
	Direct   int      `json:"direct_live"`
	Depth    int      `json:"depth"`
	Breaches []string `json:"breaches,omitempty"`

	// Response: minutes from an event that needs it (a session stalled or
	// done, a PR ready, merged early or blocked from auto-merge, a failing
	// or conflicting PR nobody owns) to its first message or tool call
	// naming the session or PR.
	Response Dist `json:"response"`
	// Stalled minutes summed over its sessions (session_stalled to
	// session_active or session_ended, clipped to the period), and the worst.
	StalledMinutes float64 `json:"stalled_minutes"`
	WorstStall     string  `json:"worst_stall,omitempty"`
	// DoneToClose: session_done to session_ended.
	DoneToClose Dist `json:"done_to_close"`

	// The user's messages to it: how many, how many nudge it about idle
	// sessions or PRs (a failure each) and how many correct it, with the
	// lines that matched.
	UserMessages int      `json:"user_messages"`
	IdleNudges   int      `json:"idle_nudges"`
	Corrections  int      `json:"corrections"`
	Quotes       []string `json:"quotes,omitempty"`

	// Its own tool calls: how many, how many were hands-on work it should
	// have handed off (reading or editing outside its folder, builds, git,
	// cluster and cloud commands, web fetches), the commonest of those, and
	// ccs commands run through Bash that an MCP tool covers.
	ToolCalls  int      `json:"tool_calls"`
	HandsOn    int      `json:"hands_on"`
	HandsOnTop []string `json:"hands_on_top,omitempty"`
	CCSViaBash int      `json:"ccs_via_bash"`

	// PRs of its sessions merged in the period, and minutes from ready to merged.
	Merged       int  `json:"merged"`
	ReadyToMerge Dist `json:"ready_to_merge"`

	// Now (only when the period ends now): its sessions running, and how
	// many of them are stalled, done or idle with nothing pending.
	Live     int `json:"live,omitempty"`
	Inactive int `json:"inactive,omitempty"`

	// Prev is the previous saved scorecard's numbers, for comparison.
	Prev *Scorecard `json:"previous,omitempty"`
}

// reportEvent is the part of a pr-events.jsonl line the report reads.
type reportEvent struct {
	At        time.Time `json:"at"`
	Event     string    `json:"event"`
	PR        string    `json:"pr"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Session   string    `json:"session"`
	SessionID string    `json:"session_id"`
	OpenedBy  struct {
		Name    string `json:"name"`
		Session string `json:"session"`
	} `json:"opened_by"`
	From string `json:"from"`
}

func readReportEvents(path string) ([]reportEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []reportEvent
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		var e reportEvent
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &e) == nil && !e.At.IsZero() {
			out = append(out, e)
		}
		if err != nil {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// orgNode is one orchestrator: its name, session id and the names of the
// sessions it launched.
type orgNode struct {
	Name, ID string
	Kids     []string
}

// reportTopID is the orchestrator ccs orchestrator set up, from
// orchestrator.json beside the settings (or coordinator.json, from before the
// rename, until ccs has copied it); "" when there is none.
var reportTopID = func() (id, name string) {
	var r struct {
		SessionID string `json:"session_id"`
		Name      string `json:"name"`
	}
	data, legacy, err := state.ReadOrchestrator()
	if err == nil {
		json.Unmarshal(data, &r)
	}
	if legacy && r.Name == "" {
		r.Name = state.LegacyOrchestratorName // ccs called it that before the rename
	}
	return r.SessionID, r.Name
}

// orgTree is every orchestrator (a session that launched another, and the
// one in orchestrator.json) and, for each name, its parent's.
func orgTree() (nodes map[string]*orgNode, parentOf map[string]string, top string) {
	nodes, parentOf = map[string]*orgNode{}, map[string]string{}
	for name, p := range sessions.LoadParents().Parents {
		child := cmp.Or(p.Child, name)
		if p.Parent == "" || child == p.Parent {
			continue
		}
		parentOf[child] = p.Parent
		n := nodes[p.Parent]
		if n == nil {
			n = &orgNode{Name: p.Parent}
			nodes[p.Parent] = n
		}
		n.ID = cmp.Or(n.ID, p.ParentID)
		n.Kids = append(n.Kids, child)
	}
	id, name := reportTopID()
	if id != "" {
		for _, n := range nodes {
			if n.ID == id {
				top = n.Name
			}
		}
		if top == "" {
			top = cmp.Or(strings.TrimSpace(name), topName)
			if nodes[top] == nil {
				nodes[top] = &orgNode{Name: top}
			}
			nodes[top].ID = id
		}
	}
	for _, n := range nodes {
		slices.Sort(n.Kids)
	}
	return nodes, parentOf, top
}

// subtree is every name under name, with the depth below it.
func subtree(nodes map[string]*orgNode, name string) (names map[string]bool, depth int) {
	names = map[string]bool{}
	var walk func(n string, d int)
	walk = func(n string, d int) {
		node := nodes[n]
		if node == nil {
			return
		}
		for _, k := range node.Kids {
			if names[k] || k == name {
				continue
			}
			names[k] = true
			depth = max(depth, d+1)
			walk(k, d+1)
		}
	}
	walk(name, 0)
	return names, depth
}

// User message patterns, matched on the lowercased text. idleNudge: the user
// pointing at sessions or PRs sitting idle; correction: the user telling the
// orchestrator it did something wrong or should have done it otherwise.
var (
	idleNudge  = regexp.MustCompile(`\b\d+ idle\b|\bidle (sessions?|prs?|tabs?)\b|\b(piling|pile) up\b|\bsessions? (are |is )?(sitting|idle|stuck|stalled)\b|\bstalled\b|\bany(thing)? (sessions? )?reapable\b|\bwhy (is|are) .{0,40}\b(idle|stuck|waiting|sitting)\b|\bnobody('s| is) (on|watching|picking)\b`)
	correction = regexp.MustCompile(`^(no|nope|wrong)\b|\b(that's|thats|that is|this is) (wrong|not (right|true|what))\b|\bi (said|told you|asked you)\b|\bwhy did you\b|\byou (shouldn't|should not|should've|should have|didn't|did not|forgot|missed)\b|\b(don't|do not|stop) (do(ing)?|run(ning)?|launch(ing)?|investigat(e|ing)) (it|this|that|them)? ?yourself\b|\bdo(n't| not) do (the work|it) yourself\b|\b(hand|delegate) (it|this|that) (off|to)\b|\buse the (ccs )?mcp\b|\bnot bash\b|\b(far|way|much) too (long|many|much)\b|\btoo many (sessions|tabs|prs)\b|\b(that's|thats|it's|its|this is) too long\b|\bjust ask( me)?\b|\bask me (a )?questions?\b|\bnot what i (asked|wanted|meant)\b|\bi didn't (say|ask|approve)\b|\bthat was (me|my go)\b|\bdid (u|you) (actually|ever|even) (check|look|read|test|verify|confirm)\b|\b(ur|you're|youre|you are) forgetting\b|\bforgetting to hand\b|\breload your (system )?instructions\b|\bwhy (are|r) (u|you) not\b|\b(a lot|lots|too much) (of things )?that needs? me\b|\btoo many resumed\b|\b\d+ resumed sounds like too many\b`)
)

// viaCCS is a message the user typed in ccs's message box, as it arrives:
// the cross-session wrapper ccs sends as "the user, via ccs", its bracketed
// note first.
var viaCCS = regexp.MustCompile(`(?s)^<cross-session-message from-name="the user, via ccs"[^>]*>\s*(?:\[[^\]]*\]\s*)?(.*?)\s*</cross-session-message>`)

// typedByUser is the user's own text of a transcript user line, "" for tool
// results, harness notes, other sessions' messages, channel events and
// compaction summaries.
func typedByUser(content json.RawMessage) string {
	var text string
	var s string
	if json.Unmarshal(content, &s) == nil {
		text = s
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(content, &blocks) != nil {
			return ""
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return ""
			}
			if b.Type == "text" {
				text += b.Text
			}
		}
	}
	t := strings.TrimSpace(text)
	// Typed in ccs's message box: the user's, inside a cross-session wrapper.
	if m := viaCCS.FindStringSubmatch(strings.TrimSpace(strings.TrimPrefix(t, "Another Claude session sent a message:"))); m != nil {
		return strings.TrimSpace(m[1])
	}
	for _, p := range []string{"<", "[Request interrupted", "Another Claude session", "This session is being continued", "Caveat:", "Standing rules for this session"} {
		if strings.HasPrefix(t, p) {
			return ""
		}
	}
	return t
}

// toolUse is one of the orchestrator's tool calls.
type toolUse struct {
	At    time.Time
	Name  string
	Input json.RawMessage
}

// mention is something the orchestrator said or did: its text or a tool
// call's input, lowercased, for finding its first response to an event.
type mention struct {
	At   time.Time
	Text string
}

// orchTranscript is what the report reads from an orchestrator's transcript.
type orchTranscript struct {
	Cwd      string
	User     []mention // the user's own messages, lowercased
	Tools    []toolUse
	Mentions []mention
}

// readOrchTranscript reads the lines of path stamped within [from, to),
// skipping the others before parsing them (a long-running orchestrator's
// transcript runs to hundreds of MB).
func readOrchTranscript(path string, from, to time.Time) (orchTranscript, error) {
	var t orchTranscript
	f, err := os.Open(path)
	if err != nil {
		return t, err
	}
	defer f.Close()
	lo, hi := from.UTC().Format("2006-01-02T15:04:05"), to.UTC().Format("2006-01-02T15:04:05")
	key := []byte(`"timestamp":"`)
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if i := bytes.Index(line, key); i >= 0 && len(line) >= i+len(key)+19 {
			ts := string(line[i+len(key) : i+len(key)+19])
			if ts >= lo && ts < hi {
				t.add(line)
			}
		}
		if err != nil {
			break
		}
	}
	return t, nil
}

func (t *orchTranscript) add(line []byte) {
	var l struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Cwd       string    `json:"cwd"`
		IsMeta    bool      `json:"isMeta"`
		IsSide    bool      `json:"isSidechain"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Attachment struct {
			Type   string          `json:"type"`
			Prompt json.RawMessage `json:"prompt"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &l) != nil || l.IsSide {
		return
	}
	if l.Cwd != "" {
		t.Cwd = l.Cwd
	}
	switch l.Type {
	case "attachment":
		// A message that arrived mid-turn: typed in its tab, or in ccs.
		if l.Attachment.Type == "queued_command" {
			if s := typedByUser(l.Attachment.Prompt); s != "" {
				t.User = append(t.User, mention{l.Timestamp, s})
			}
		}
	case "user":
		// Harness lines are meta, and so is what the user types in ccs once
		// it is handed over: only the latter counts.
		if s := typedByUser(l.Message.Content); s != "" && (!l.IsMeta || bytes.Contains(l.Message.Content, []byte(`from-name=\"the user, via ccs\"`))) {
			t.User = append(t.User, mention{l.Timestamp, s})
		}
	case "assistant":
		var blocks []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				t.Mentions = append(t.Mentions, mention{l.Timestamp, strings.ToLower(b.Text)})
			case "tool_use":
				t.Tools = append(t.Tools, toolUse{l.Timestamp, b.Name, b.Input})
				t.Mentions = append(t.Mentions, mention{l.Timestamp, strings.ToLower(string(b.Input))})
			}
		}
	}
}

// Hands-on Bash: commands that do work rather than coordinate it. Checking
// a claim with gh pr view or gh pr checks is coordination; reading a diff,
// code or logs, building, committing or touching a cluster is not.
var (
	handsOnBash = regexp.MustCompile(`^(git|go|make|npm|pnpm|yarn|uv|python3?|pytest|terraform|tofu|kubectl|helm|gcloud|gsutil|bq|curl|wget|grep|rg|sed|awk|cat|head|find|jq|psql|docker)\b|^gh (pr diff|run view|api [^ ]*/(contents|pulls/\d+/files|actions/jobs))`)
	ccsBash     = regexp.MustCompile(`(^|[;&|(]\s*|\bnohup\s+)ccs (sessions|launch|close|restart|prs|claim|depends)\b`)
	envPrefix   = regexp.MustCompile(`^(\s*(cd\s+[^;&]+(&&|;)|[A-Z_]+=\S+\s+))*\s*`)
)

// handsOn says whether a tool call is work the orchestrator should have
// handed off, and a short label for it.
func handsOn(u toolUse, cwd string) (bool, string) {
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		URL      string `json:"url"`
	}
	json.Unmarshal(u.Input, &in)
	switch u.Name {
	case "Read", "Grep", "Glob", "Edit", "Write", "MultiEdit", "NotebookEdit":
		p := cmp.Or(in.FilePath, in.Path)
		if p == "" || (cwd != "" && (p == cwd || strings.HasPrefix(p, cwd+"/"))) {
			return false, ""
		}
		return true, u.Name
	case "WebFetch", "WebSearch":
		return true, u.Name
	case "Bash":
		cmd := envPrefix.ReplaceAllString(strings.TrimSpace(in.Command), "")
		if m := handsOnBash.FindString(cmd); m != "" {
			return true, "Bash " + m
		}
	}
	return false, ""
}

// eventKeys is how the orchestrator would name what an event is about: the
// PR's address and repo#N, or the session's name and id.
func eventKeys(e reportEvent) []string {
	if e.PR != "" {
		keys := []string{strings.ToLower(e.PR)}
		if e.Repo != "" && e.Number > 0 {
			repo := strings.ToLower(e.Repo)
			keys = append(keys, fmt.Sprintf("%s#%d", repo, e.Number), fmt.Sprintf("%s/pull/%d", repo, e.Number))
			if i := strings.LastIndex(repo, "/"); i >= 0 {
				keys = append(keys, fmt.Sprintf("%s#%d", repo[i+1:], e.Number))
			}
		}
		return keys
	}
	var keys []string
	if len(e.Session) >= 4 {
		keys = append(keys, strings.ToLower(e.Session))
	}
	if len(e.SessionID) >= 8 {
		keys = append(keys, strings.ToLower(e.SessionID[:8]))
	}
	return keys
}

// needsResponse: the events an orchestrator must act on. A failing or
// conflicting PR is its owner's to fix (ccs watch nudges it), so only one
// nobody owns counts.
func needsResponse(e reportEvent, owned bool) bool {
	switch e.Event {
	case "session_stalled", "session_done", "ready", "merged_early", "blocked_automerge":
		return true
	case "checks_failing", "conflicts":
		return !owned
	}
	return false
}

// firstMention is the minutes from at to the first mention containing a
// key, or -1 within responseCap.
func firstMention(ms []mention, at time.Time, keys []string) float64 {
	i := sort.Search(len(ms), func(i int) bool { return !ms[i].At.Before(at) })
	for ; i < len(ms) && ms[i].At.Sub(at) <= responseCap; i++ {
		for _, k := range keys {
			if strings.Contains(ms[i].Text, k) {
				return ms[i].At.Sub(at).Minutes()
			}
		}
	}
	return -1
}

// ReportOpts chooses the period and the orchestrators.
type ReportOpts struct {
	From, To     time.Time
	Orchestrator string // "" for every one
	Now          time.Time
}

// errNoOrchestrator: nothing to report on.
var errNoOrchestrator = errors.New("no orchestrator found: none in orchestrator.json, and no session has launched another through ccs launch or launch_session")

// BuildReport is every orchestrator's scorecard for the period, the top one
// first.
func BuildReport(o ReportOpts) ([]Scorecard, error) {
	nodes, _, top := orgTree()
	if len(nodes) == 0 {
		return nil, errNoOrchestrator
	}
	events, err := readReportEvents(github.PREventsPath())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		if o.Orchestrator == "" || n == o.Orchestrator {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no orchestrator named %q", o.Orchestrator)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == top) != (names[j] == top) {
			return names[i] == top
		}
		return names[i] < names[j]
	})
	// The session that owns each PR, by name: who opened it, else who claimed it.
	ownerOf := map[string]string{}
	idName := map[string]string{}
	for _, e := range events {
		if e.SessionID != "" && e.Session != "" {
			idName[e.SessionID] = e.Session
		}
		switch {
		case e.Event == "new" && e.OpenedBy.Name != "":
			ownerOf[e.PR] = e.OpenedBy.Name
		case e.Event == "claimed" && e.Session != "" && ownerOf[e.PR] == "":
			ownerOf[e.PR] = e.Session
		}
	}
	var live []TriageRow
	if o.Now.Sub(o.To).Abs() < 5*time.Minute {
		l := liveSessions()
		live = TriageRows(liveConversations(l), l, o.Now, DefaultStallAfter)
	}
	var out []Scorecard
	for _, name := range names {
		node := nodes[name]
		members, depth := subtree(nodes, name)
		all := name == top // the top one answers for every session and every PR
		in := func(session, id string) bool {
			if session == name || (id != "" && id == node.ID) {
				return false
			}
			if all {
				return true
			}
			return members[cmp.Or(session, idName[id])]
		}
		sc := Scorecard{Orchestrator: name, SessionID: node.ID, From: o.From, To: o.To, Sessions: len(members), Depth: depth}
		var tr orchTranscript
		if p := transcript.TranscriptPath(node.ID); p != "" {
			tr, _ = readOrchTranscript(p, o.From, o.To.Add(responseCap))
		}
		sc.score(events, tr, in, ownerOf, all)
		kids := map[string]bool{}
		for _, k := range node.Kids {
			kids[k] = true
		}
		for _, r := range live {
			if !in(r.Name, r.Session) {
				continue
			}
			sc.Live++
			if r.Bucket == BucketStalled || r.Bucket == BucketDone || r.Bucket == BucketIdle {
				sc.Inactive++
			}
			if kids[r.Name] || (all && !hasParentIn(r.Name, nodes, name)) {
				sc.Direct++
			}
		}
		if sc.Direct > maxSpan {
			sc.Breaches = append(sc.Breaches, fmt.Sprintf("%d direct sessions, over %d: put a workstream under a sub-orchestrator", sc.Direct, maxSpan))
		}
		if depth > maxDepth {
			sc.Breaches = append(sc.Breaches, fmt.Sprintf("tree %d deep, over %d", depth, maxDepth))
		}
		out = append(out, sc)
	}
	return out, nil
}

// hasParentIn: name was launched by some orchestrator other than top.
func hasParentIn(name string, nodes map[string]*orgNode, top string) bool {
	for n, node := range nodes {
		if n != top && slices.Contains(node.Kids, name) {
			return true
		}
	}
	return false
}

// score fills in the numbers counted from the events and the transcript.
func (sc *Scorecard) score(events []reportEvent, tr orchTranscript, in func(session, id string) bool, ownerOf map[string]string, all bool) {
	within := func(t time.Time) bool { return !t.Before(sc.From) && t.Before(sc.To) }
	prIn := func(pr string) bool {
		if o := ownerOf[pr]; o != "" {
			return in(o, "")
		}
		return all
	}

	// Response.
	var resp []float64
	missed := 0
	for _, e := range events {
		if !within(e.At) {
			continue
		}
		owned := ownerOf[e.PR] != ""
		if e.PR != "" && !prIn(e.PR) || e.PR == "" && !in(e.Session, e.SessionID) || !needsResponse(e, owned) {
			continue
		}
		keys := eventKeys(e)
		if len(keys) == 0 {
			continue
		}
		if m := firstMention(tr.Mentions, e.At, keys); m >= 0 {
			resp = append(resp, m)
		} else {
			missed++
		}
	}
	sc.Response = dist(resp)
	sc.Response.Missed = missed

	// Stalls and finishes, per session id.
	type open struct {
		at   time.Time
		name string
	}
	stalls, dones := map[string]open{}, map[string]open{}
	perSession := map[string]float64{}
	clip := func(a, b time.Time) float64 {
		a, b = maxTime(a, sc.From), minTime(b, sc.To)
		if !b.After(a) {
			return 0
		}
		return b.Sub(a).Minutes()
	}
	var closeMins []float64
	for _, e := range events {
		if e.At.After(sc.To) || e.SessionID == "" || !in(e.Session, e.SessionID) {
			continue
		}
		id := e.SessionID
		endStall := func() {
			if s, ok := stalls[id]; ok {
				perSession[s.name] += clip(s.at, e.At)
				delete(stalls, id)
			}
		}
		switch e.Event {
		case "session_stalled":
			endStall()
			stalls[id] = open{e.At, e.Session}
		case "session_done":
			endStall()
			dones[id] = open{e.At, e.Session}
		case "session_active", "session_started":
			endStall()
			delete(dones, id)
		case "session_ended":
			endStall()
			if d, ok := dones[id]; ok {
				if within(e.At) {
					closeMins = append(closeMins, e.At.Sub(d.at).Minutes())
				}
				delete(dones, id)
			}
		}
	}
	for _, s := range stalls {
		perSession[s.name] += clip(s.at, sc.To)
	}
	worst, worstMin := "", 0.0
	for name, m := range perSession {
		sc.StalledMinutes += m
		if m > worstMin || m == worstMin && name < worst {
			worst, worstMin = name, m
		}
	}
	sc.StalledMinutes = math.Round(sc.StalledMinutes)
	if worst != "" && worstMin >= 1 {
		sc.WorstStall = fmt.Sprintf("%s %.0fm", worst, worstMin)
	}
	sc.DoneToClose = dist(closeMins)
	for _, d := range dones {
		if d.at.Before(sc.To) {
			sc.DoneToClose.Open++
		}
	}

	// Merges, and ready to merged.
	lastReady := map[string]time.Time{}
	var merge []float64
	for _, e := range events {
		if e.At.After(sc.To) || e.PR == "" {
			continue
		}
		switch e.Event {
		case "ready":
			lastReady[e.PR] = e.At
		case "merged":
			if !within(e.At) || !prIn(e.PR) {
				continue
			}
			sc.Merged++
			if r, ok := lastReady[e.PR]; ok {
				merge = append(merge, e.At.Sub(r).Minutes())
			}
		}
	}
	sc.ReadyToMerge = dist(merge)

	// The user's messages.
	for _, u := range tr.User {
		if !within(u.At) {
			continue
		}
		sc.UserMessages++
		low := strings.ToLower(u.Text)
		hit := false
		if idleNudge.MatchString(low) {
			sc.IdleNudges++
			hit = true
		} else if correction.MatchString(low) {
			sc.Corrections++
			hit = true
		}
		if hit && len(sc.Quotes) < 8 {
			sc.Quotes = append(sc.Quotes, u.At.Local().Format("15:04")+" "+transcript.Truncate(strings.Join(strings.Fields(u.Text), " "), 100))
		}
	}

	// Its tool calls.
	counts := map[string]int{}
	for _, u := range tr.Tools {
		if !within(u.At) {
			continue
		}
		sc.ToolCalls++
		if ok, label := handsOn(u, tr.Cwd); ok {
			sc.HandsOn++
			counts[label]++
		}
		if u.Name == "Bash" {
			var in struct {
				Command string `json:"command"`
			}
			json.Unmarshal(u.Input, &in)
			if ccsBash.MatchString(in.Command) {
				sc.CCSViaBash++
			}
		}
	}
	labels := make([]string, 0, len(counts))
	for l := range counts {
		labels = append(labels, l)
	}
	sort.Slice(labels, func(i, j int) bool {
		if counts[labels[i]] != counts[labels[j]] {
			return counts[labels[i]] > counts[labels[j]]
		}
		return labels[i] < labels[j]
	})
	for i, l := range labels {
		if i == 3 {
			break
		}
		sc.HandsOnTop = append(sc.HandsOnTop, fmt.Sprintf("%s ×%d", l, counts[l]))
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// reportDir is where scorecards are saved, one file per orchestrator per
// day: reports/<orchestrator>/<YYYY-MM-DD>.json beside the settings.
var reportDir = func() string { return filepath.Join(filepath.Dir(state.SettingsPath), "reports") }

func reportFile(name string, day time.Time) string {
	return filepath.Join(reportDir(), safeName(name), day.Local().Format("2006-01-02")+".json")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func safeName(s string) string { return cmp.Or(unsafeName.ReplaceAllString(s, "_"), "_") }

// previousScorecard is the newest saved scorecard for name from a day before day.
func previousScorecard(name string, day time.Time) *Scorecard {
	dir := filepath.Join(reportDir(), safeName(name))
	entries, _ := os.ReadDir(dir)
	cut := day.Local().Format("2006-01-02") + ".json"
	best := ""
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".json") && n < cut && n > best {
			best = n
		}
	}
	if best == "" {
		return nil
	}
	var sc Scorecard
	data, err := os.ReadFile(filepath.Join(dir, best))
	if err != nil || json.Unmarshal(data, &sc) != nil {
		return nil
	}
	sc.Prev = nil
	return &sc
}

// saveScorecard writes sc as its day's file (the day its period ends).
func saveScorecard(sc Scorecard) error {
	sc.Prev = nil
	path := reportFile(sc.Orchestrator, sc.To)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return github.WriteJSON(path, sc)
}

// withPrevious attaches each scorecard's previous day and, with save, saves it.
func withPrevious(cards []Scorecard, save bool) []Scorecard {
	for i := range cards {
		cards[i].Prev = previousScorecard(cards[i].Orchestrator, cards[i].To)
		if save {
			if err := saveScorecard(cards[i]); err != nil {
				state.LogActivity("orchestrator report: couldn't save %s: %v", cards[i].Orchestrator, err)
			}
		}
	}
	return cards
}

// trend is ▲ when the change is an improvement, ▼ when worse, blank when
// the same or not comparable; lower says whether lower is better.
func trend(now, prev float64, lower bool) string {
	switch {
	case now == prev:
		return ""
	case (now < prev) == lower:
		return "▲"
	default:
		return "▼"
	}
}

// formatScorecard is the text scorecard: one line per metric, the previous
// day's value in brackets, ▲ better or ▼ worse.
func formatScorecard(sc Scorecard) string {
	var b strings.Builder
	head := sc.Orchestrator
	if sc.Prev != nil {
		head += fmt.Sprintf(" · %s vs %s", sc.To.Local().Format("2006-01-02 15:04"), sc.Prev.To.Local().Format("2006-01-02"))
	} else {
		head += fmt.Sprintf(" · %s to %s", sc.From.Local().Format("2006-01-02 15:04"), sc.To.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintln(&b, head)
	p := sc.Prev
	row := func(label, val string, now, prev float64, lower bool, note string) {
		was, arrow := "", ""
		if p != nil {
			was = fmt.Sprintf("(%s)", strings.TrimSpace(fmt.Sprint(prev)))
			arrow = trend(now, prev, lower)
		}
		fmt.Fprintf(&b, "  %-18s %-8s %-8s %-2s %s\n", label, val, was, arrow, note)
	}
	pv := func(f func(*Scorecard) float64) float64 {
		if p == nil {
			return 0
		}
		return f(p)
	}
	mins := func(f float64) string { return fmt.Sprintf("%.0fm", f) }
	row("response p50", mins(sc.Response.P50), sc.Response.P50, pv(func(s *Scorecard) float64 { return s.Response.P50 }), true,
		fmt.Sprintf("%d events, p90 %s, %d missed", sc.Response.N+sc.Response.Missed, mins(sc.Response.P90), sc.Response.Missed))
	row("stalled minutes", fmt.Sprintf("%.0f", sc.StalledMinutes), sc.StalledMinutes, pv(func(s *Scorecard) float64 { return s.StalledMinutes }), true, cmp.Or(sc.WorstStall, ""))
	row("done→close p50", mins(sc.DoneToClose.P50), sc.DoneToClose.P50, pv(func(s *Scorecard) float64 { return s.DoneToClose.P50 }), true,
		fmt.Sprintf("%d closed, %d still done", sc.DoneToClose.N, sc.DoneToClose.Open))
	nudge := ""
	if sc.IdleNudges > 0 {
		nudge = "✗ each is a failure"
	}
	row("your idle nudges", fmt.Sprint(sc.IdleNudges), float64(sc.IdleNudges), pv(func(s *Scorecard) float64 { return float64(s.IdleNudges) }), true, nudge)
	row("corrections/100", fmt.Sprintf("%.0f", per100(sc.Corrections, sc.UserMessages)), per100(sc.Corrections, sc.UserMessages), pv(func(s *Scorecard) float64 { return per100(s.Corrections, s.UserMessages) }), true,
		fmt.Sprintf("%d of your %d messages", sc.Corrections, sc.UserMessages))
	row("hands-on calls", fmt.Sprintf("%.0f%%", pct(sc.HandsOn, sc.ToolCalls)), pct(sc.HandsOn, sc.ToolCalls), pv(func(s *Scorecard) float64 { return pct(s.HandsOn, s.ToolCalls) }), true,
		fmt.Sprintf("%d of %d; %s", sc.HandsOn, sc.ToolCalls, strings.Join(sc.HandsOnTop, ", ")))
	row("ccs via bash", fmt.Sprint(sc.CCSViaBash), float64(sc.CCSViaBash), pv(func(s *Scorecard) float64 { return float64(s.CCSViaBash) }), true, "")
	row("merged", fmt.Sprint(sc.Merged), float64(sc.Merged), pv(func(s *Scorecard) float64 { return float64(s.Merged) }), false, "")
	row("ready→merge p50", mins(sc.ReadyToMerge.P50), sc.ReadyToMerge.P50, pv(func(s *Scorecard) float64 { return s.ReadyToMerge.P50 }), true, fmt.Sprintf("%d with a ready event", sc.ReadyToMerge.N))
	if sc.Live > 0 {
		fmt.Fprintf(&b, "  %-18s %-8s %s\n", "sessions now", fmt.Sprintf("%d/%d", sc.Inactive, sc.Live), fmt.Sprintf("stalled, done or idle of running; %d direct", sc.Direct))
	}
	for _, br := range sc.Breaches {
		fmt.Fprintf(&b, "  ✗ %s\n", br)
	}
	for _, q := range sc.Quotes {
		fmt.Fprintf(&b, "  » %s\n", q)
	}
	return b.String()
}

func per100(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return math.Round(float64(n) * 100 / float64(of))
}

func pct(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return math.Round(float64(n) * 100 / float64(of))
}

// reportJSON is the MCP and --json answer.
func reportJSON(cards []Scorecard) string {
	data, _ := json.MarshalIndent(map[string]any{"scorecards": cards}, "", " ")
	return string(data)
}

// RunReport is `ccs orchestrator-report`.
func RunReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orchestrator-report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON: {\"scorecards\": [...]}")
	since := fs.Duration("since", 24*time.Hour, "the period: this long up to now")
	name := fs.String("orchestrator", "", "only this orchestrator (a session name); default every one")
	noSave := fs.Bool("no-save", false, "don't save the scorecards as today's")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs orchestrator-report [--since 24h] [--orchestrator name] [--json] [--no-save]\n\nEach orchestrator's scorecard (the session in orchestrator.json, and any session that launched\nanother): response time to events, stalled minutes, done-to-close time, your idle nudges and\ncorrections, its hands-on tool calls, merges, and its sessions now, against the previous saved\nday. Saved to "+reportDir()+".")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	now := time.Now()
	cards, err := BuildReport(ReportOpts{From: now.Add(-*since), To: now, Orchestrator: *name, Now: now})
	if err != nil {
		fmt.Fprintln(stderr, "ccs orchestrator-report:", err)
		return 1
	}
	cards = withPrevious(cards, !*noSave)
	if *asJSON {
		fmt.Fprintln(stdout, reportJSON(cards))
		return 0
	}
	for i, sc := range cards {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprint(stdout, formatScorecard(sc))
	}
	return 0
}

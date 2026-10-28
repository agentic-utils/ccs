package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// bgTask is a background task a session started: a Bash command or a
// Monitor. Subagents, async or teammates, are counted apart from their own
// meta.json (subagentsOf), so they never show twice.
type bgTask struct {
	id, kind, desc, started string
	status                  string    // "" while running; completed, failed or stopped from its notification
	expires                 time.Time // a monitor's own end, if it never reports one
}

var (
	taskEnd     = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>.*?<status>([^<]+)</status>`)
	monitorLife = regexp.MustCompile(`expires in (\d+)m`)
)

// noteCall remembers a call that may start a background task; its result decides.
func noteCall(conv *Conversation, call toolCall, ts string) {
	if call.Name != "Bash" && call.Name != "Monitor" {
		return
	}
	var in struct{ Description, Command string }
	json.Unmarshal(call.Input, &in)
	desc := in.Description
	if desc == "" {
		desc = in.Command
	}
	conv.calls = append(slices.Clone(conv.calls), bgTask{id: call.ID, kind: call.Name, desc: strings.Join(strings.Fields(desc), " "), started: ts})
}

// noteResult makes a call a task when its result says it went to the background.
func noteResult(conv *Conversation, id string, content json.RawMessage) {
	i := slices.IndexFunc(conv.calls, func(t bgTask) bool { return t.id == id })
	if i < 0 {
		return
	}
	t := conv.calls[i]
	conv.calls = slices.Delete(slices.Clone(conv.calls), i, i+1)
	// Claude Code starts the result with it; anywhere else it's only output
	// that mentions it.
	s := resultText(content)
	switch {
	case t.kind == "Bash" && strings.HasPrefix(s, "Command running in background"):
		t.kind = "bash"
	case t.kind == "Monitor" && strings.HasPrefix(s, "Monitor started"):
		t.kind = "monitor"
		if m := monitorLife.FindStringSubmatch(s); m != nil {
			if start, err := time.Parse(time.RFC3339, t.started); err == nil {
				mins, _ := strconv.Atoi(m[1])
				t.expires = start.Add(time.Duration(mins) * time.Minute)
			}
		}
	default:
		return
	}
	conv.tasks = append(slices.Clone(conv.tasks), t)
}

// resultText is a tool result's text: a string, or its first text block.
func resultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct{ Text string }
	if json.Unmarshal(content, &blocks) == nil && len(blocks) > 0 {
		return blocks[0].Text
	}
	return ""
}

// noteEnds marks the tasks a line's <task-notification> reports as ended.
func noteEnds(conv *Conversation, line []byte) {
	if len(conv.tasks) == 0 || !bytes.Contains(line, []byte("<task-notification>")) {
		return
	}
	for _, m := range taskEnd.FindAllSubmatch(line, -1) {
		if i := slices.IndexFunc(conv.tasks, func(t bgTask) bool { return t.id == string(m[1]) }); i >= 0 && conv.tasks[i].status == "" {
			conv.tasks = slices.Clone(conv.tasks)
			conv.tasks[i].status = string(m[2])
		}
	}
}

// runningTasks is conv's tasks still going: only while it's open in claude,
// and only those its current claude process started. A process that exits
// (or is replaced by a resume) takes its tasks with it, without reporting
// their end.
func (m model) runningTasks(conv Conversation) []bgTask {
	if !m.live[conv.SessionID] {
		return nil
	}
	since, _ := time.Parse(time.ANSIC, strings.Join(strings.Fields(m.lastScan[conv.SessionID].start), " "))
	now := time.Now()
	var out []bgTask
	for _, t := range conv.tasks {
		started, err := time.Parse(time.RFC3339, t.started)
		if t.status == "" && (t.expires.IsZero() || now.Before(t.expires)) && (err != nil || !started.Before(since)) {
			out = append(out, t)
		}
	}
	return out
}

// taskColour is the tasks' own colour, a light blue no other mark uses
// (PRs are green, amber, red and purple; HITS cyan; busy amber), and
// subagentColour, pink, theirs.
const (
	taskColour     = "38;5;75"
	subagentColour = "38;5;211"
)

// taskCell is the list's counts of running background tasks (⧗) and
// subagents (◆), "" when neither.
func (m model) taskCell(conv Conversation, coloured bool) string {
	var cells []string
	add := func(n int, glyph, colour string) {
		switch {
		case n == 0:
		case coloured:
			cells = append(cells, fmt.Sprintf("\033[%sm%s%d\033[0m", colour, glyph, n))
		default:
			cells = append(cells, fmt.Sprintf("%s%d", glyph, n))
		}
	}
	add(len(m.runningTasks(conv)), "⧗", taskColour)    // background commands and monitors
	add(m.runningSubagents(conv), "◆", subagentColour) // subagents
	return strings.Join(cells, " ")
}

// maxTaskLines is how many running tasks the preview header lists.
const maxTaskLines = 5

// taskLines is the preview header's tasks, width wide: one line per running
// task (at most maxTaskLines), then one counting the ended ones.
func (m model) taskLines(conv Conversation, width int) []string {
	if len(conv.tasks) == 0 {
		return nil
	}
	running := m.runningTasks(conv)
	var lines []string
	for _, t := range running[:min(len(running), maxTaskLines)] {
		age := strings.TrimSuffix(formatAgo(t.started, time.Now()), " ago")
		lines = append(lines, fmt.Sprintf("\033[%sm⧗\033[0m %s \033[90m· %s · %s\033[0m", taskColour, t.desc, t.kind, age))
	}
	ended := map[string]int{}
	for _, t := range conv.tasks {
		switch {
		case t.status != "":
			ended[t.status]++
		case !slices.ContainsFunc(running, func(r bgTask) bool { return r.id == t.id }):
			ended["ended with the session"]++ // or a monitor past its time
		}
	}
	var rest []string
	if more := len(running) - maxTaskLines; more > 0 {
		rest = append(rest, fmt.Sprintf("+%d more running", more))
	}
	for _, k := range []string{"completed", "failed", "stopped", "ended with the session"} {
		if ended[k] > 0 {
			rest = append(rest, fmt.Sprintf("%d %s", ended[k], k))
		}
	}
	if len(rest) > 0 {
		lines = append(lines, "\033[90m"+strings.Join(rest, " · ")+"\033[0m")
	}
	for i, l := range lines {
		lead := "         "
		if i == 0 {
			lead = "\033[1;33mTasks:\033[0m   "
		}
		lines[i] = ansi.Truncate(lead+l, max(width, 20), "…") + "\033[0m"
	}
	return lines
}

// subagent is one of a session's subagents (Agent tool, teammates
// included), from the meta.json Claude Code writes beside its transcript in
// <session>/subagents/.
type subagent struct {
	name     string
	started  time.Time
	finished bool
}

// subagentsTTL is how long subagentsOf trusts what it last read.
const subagentsTTL = 2 * time.Second

var subagentCache = struct {
	sync.Mutex
	m map[string]subagentsRead
}{m: map[string]subagentsRead{}}

type subagentsRead struct {
	at   time.Time
	subs []subagent
}

// subagentsOf is conv's subagents, running first then newest, read at most
// every subagentsTTL per session (the list asks on every frame).
func subagentsOf(conv Conversation) []subagent {
	dir := filepath.Join(strings.TrimSuffix(conv.FilePath, ".jsonl"), "subagents")
	subagentCache.Lock()
	defer subagentCache.Unlock()
	if r, ok := subagentCache.m[dir]; ok && time.Since(r.at) < subagentsTTL {
		return r.subs
	}
	metas, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
	var subs []subagent
	for _, path := range metas {
		var meta struct {
			Name, Description string
			Finished          bool
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &meta) != nil {
			continue
		}
		s := subagent{name: meta.Name, finished: meta.Finished}
		if s.name == "" {
			s.name = meta.Description
		}
		s.started = firstTimestamp(strings.TrimSuffix(path, ".meta.json") + ".jsonl")
		subs = append(subs, s)
	}
	slices.SortStableFunc(subs, func(a, b subagent) int {
		if a.finished != b.finished {
			if a.finished {
				return 1
			}
			return -1
		}
		return b.started.Compare(a.started)
	})
	subagentCache.m[dir] = subagentsRead{time.Now(), subs}
	return subs
}

// firstTimestamp is when a transcript's first line was written.
func firstTimestamp(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadBytes('\n')
	var rec struct{ Timestamp time.Time }
	json.Unmarshal(line, &rec)
	return rec.Timestamp
}

// runningSubagents counts conv's subagents still going: only while it's
// open in claude, whose exit takes them with it.
func (m model) runningSubagents(conv Conversation) int {
	if !m.live[conv.SessionID] {
		return 0
	}
	n := 0
	for _, s := range subagentsOf(conv) {
		if !s.finished {
			n++
		}
	}
	return n
}

// subagentLines is the preview header's subagents, width wide: one line per
// running one (at most maxTaskLines), then one counting the rest.
func (m model) subagentLines(conv Conversation, width int) []string {
	subs := subagentsOf(conv)
	if len(subs) == 0 {
		return nil
	}
	live := m.live[conv.SessionID]
	var lines []string
	running, finished, ended := 0, 0, 0
	for _, s := range subs {
		switch {
		case s.finished:
			finished++
		case !live:
			ended++ // its session exited with it running
		default:
			running++
			if running <= maxTaskLines {
				age := strings.TrimSuffix(formatAgo(s.started.Format(time.RFC3339), time.Now()), " ago")
				lines = append(lines, fmt.Sprintf("\033[%sm◆\033[0m %s \033[90m· %s\033[0m", subagentColour, s.name, age))
			}
		}
	}
	var rest []string
	if more := running - maxTaskLines; more > 0 {
		rest = append(rest, fmt.Sprintf("+%d more running", more))
	}
	if finished > 0 {
		rest = append(rest, fmt.Sprintf("%d finished", finished))
	}
	if ended > 0 {
		rest = append(rest, fmt.Sprintf("%d ended with the session", ended))
	}
	if len(rest) > 0 {
		lines = append(lines, "\033[90m"+strings.Join(rest, " · ")+"\033[0m")
	}
	for i, l := range lines {
		lead := "           "
		if i == 0 {
			lead = "\033[1;33mSubagents:\033[0m "
		}
		lines[i] = ansi.Truncate(lead+l, max(width, 20), "…") + "\033[0m"
	}
	return lines
}

package app

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// runningTasks is conv's tasks still going: only while it's open in claude,
// and only those its current claude process started. A process that exits
// (or is replaced by a resume) takes its tasks with it, without reporting
// their end.
func (m model) runningTasks(conv transcript.Conversation) []transcript.BgTask {
	if !m.live[conv.SessionID] {
		return nil
	}
	since := m.procStart(conv)
	now := time.Now()
	var out []transcript.BgTask
	for _, t := range conv.Tasks {
		started, err := time.Parse(time.RFC3339, t.Started)
		if t.Status == "" && (t.Expires.IsZero() || now.Before(t.Expires)) && (err != nil || !started.Before(since)) {
			out = append(out, t)
		}
	}
	return out
}

// taskCell is the list's counts of running background tasks (⧗) and
// subagents (◆), "" when neither.
func (m model) taskCell(conv transcript.Conversation, coloured bool) string {
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
	add(len(m.runningTasks(conv)), "⧗", transcript.TaskColour)    // background commands and monitors
	add(m.runningSubagents(conv), "◆", transcript.SubagentColour) // subagents
	return strings.Join(cells, " ")
}

// taskLines is the preview header's tasks, width wide: one line per running
// task (at most maxTaskLines), then one counting the ended ones.
func (m model) taskLines(conv transcript.Conversation, width int) []string {
	if len(conv.Tasks) == 0 {
		return nil
	}
	running := m.runningTasks(conv)
	var lines []string
	for _, t := range running[:min(len(running), transcript.MaxTaskLines)] {
		age := strings.TrimSuffix(transcript.FormatAgo(t.Started, time.Now()), " ago")
		lines = append(lines, fmt.Sprintf("\033[%sm⧗\033[0m %s \033[90m· %s · %s\033[0m", transcript.TaskColour, t.Desc, t.Kind, age))
	}
	ended := map[string]int{}
	for _, t := range conv.Tasks {
		switch {
		case t.Status != "":
			ended[t.Status]++
		case !slices.ContainsFunc(running, func(r transcript.BgTask) bool { return r.Id == t.Id }):
			ended["ended with the session"]++ // or a monitor past its time
		}
	}
	var rest []string
	if more := len(running) - transcript.MaxTaskLines; more > 0 {
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

// procStart is when the claude process now running conv started (zero when
// unknown): anything it started before then died with an earlier one.
func (m model) procStart(conv transcript.Conversation) time.Time {
	t, _ := time.Parse(time.ANSIC, strings.Join(strings.Fields(m.lastScan[conv.SessionID].Start), " "))
	return t
}

// subagentAlive: not finished, and started by the session's current claude
// process. Older Claude Code wrote no finished flag at all, so an old
// subagent without one ended with the process that ran it.
func (m model) subagentAlive(conv transcript.Conversation, s transcript.Subagent) bool {
	return !s.Finished && m.live[conv.SessionID] && !s.Started.Before(m.procStart(conv))
}

// subagentRunning: alive and still writing, not an idle teammate.
func (m model) subagentRunning(conv transcript.Conversation, s transcript.Subagent) bool {
	return m.subagentAlive(conv, s) && time.Since(s.LastWrite) < transcript.SubagentIdle
}

// runningSubagents counts conv's subagents still going: only while it's
// open in claude, whose exit takes them with it.
func (m model) runningSubagents(conv transcript.Conversation) int {
	if !m.live[conv.SessionID] {
		return 0
	}
	n := 0
	for _, s := range transcript.SubagentsOf(conv) {
		if m.subagentRunning(conv, s) {
			n++
		}
	}
	return n
}

// subagentLines is the preview header's subagents, width wide: one line per
// running one (at most maxTaskLines), then one counting the rest.
func (m model) subagentLines(conv transcript.Conversation, width int) []string {
	subs := transcript.SubagentsOf(conv)
	if len(subs) == 0 {
		return nil
	}
	var lines []string
	running, idle, finished, ended := 0, 0, 0, 0
	for _, s := range subs {
		switch {
		case s.Finished:
			finished++
		case !m.subagentAlive(conv, s):
			ended++ // its claude exited (or was resumed) while it ran
		case !m.subagentRunning(conv, s):
			idle++
		default:
			running++
			if running <= transcript.MaxTaskLines {
				age := strings.TrimSuffix(transcript.FormatAgo(s.Started.Format(time.RFC3339), time.Now()), " ago")
				lines = append(lines, fmt.Sprintf("\033[%sm◆\033[0m %s \033[90m· %s\033[0m", transcript.SubagentColour, s.Name, age))
			}
		}
	}
	var rest []string
	if more := running - transcript.MaxTaskLines; more > 0 {
		rest = append(rest, fmt.Sprintf("+%d more running", more))
	}
	if idle > 0 {
		rest = append(rest, fmt.Sprintf("%d idle", idle))
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

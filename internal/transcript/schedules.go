package transcript

import (
	"bytes"
	"cmp"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Schedule is a wake-up a session set for itself: a ScheduleWakeup (the
// dynamic /loop's next turn) or a CronCreate task. Claude Code keeps them in
// the process (unless durable), so a session's process ending takes them
// with it unreported, as with background tasks.
type Schedule struct {
	ID        string    // the cron task's id; "" for a wakeup
	Kind      string    // "wakeup" or "cron"
	Desc      string    // the wakeup's reason, or the cron's schedule and prompt
	Set       string    // when it was set (RFC3339)
	Next      time.Time // when it fires next; zero for a recurring cron (Claude Code doesn't say)
	Recurring bool
	Durable   bool // written to disk: it outlives the process
	Ended     bool // stopped, replaced by a later wakeup, or deleted
}

// schedCall is a ScheduleWakeup, CronCreate or CronDelete call awaiting its result.
type schedCall struct {
	ID, Name string
	Input    json.RawMessage
}

// scheduleTools are the calls noteSchedule reads.
var scheduleTools = map[string]bool{"ScheduleWakeup": true, "CronCreate": true, "CronDelete": true}

// For a result line without toolUseResult (ccs prune strips it), the
// result's text: CronCreate's "Scheduled recurring job 3572d5a8 (Every 3
// minutes). Next run: Oct 24 10:37am." (older Claude Code said "task"), its
// next run in local time with no year; ScheduleWakeup's "Next wakeup
// scheduled for 10:07:00 (in 1844s)".
var (
	cronTaskID = regexp.MustCompile(`\b(?:job|task) ([0-9a-f]{6,})\b`)
	cronNext   = regexp.MustCompile(`Next run: ([A-Z][a-z]{2} \d{1,2} \d{1,2}:\d{2}[ap]m)`)
	wakeIn     = regexp.MustCompile(`wakeup scheduled for [^(]*\(in (\d+)s\)`)
)

// nextRunAfter is the text's "Next run" as a time: the first such date
// (local time, its year unsaid) not before set; zero when it has none.
func nextRunAfter(text string, set time.Time) time.Time {
	m := cronNext.FindStringSubmatch(text)
	if m == nil || set.IsZero() {
		return time.Time{}
	}
	local := set.In(time.Local)
	t, err := time.ParseInLocation("Jan 2 3:04pm", m[1], time.Local)
	if err != nil {
		return time.Time{}
	}
	t = time.Date(local.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
	if t.Before(local.Add(-time.Minute)) {
		t = t.AddDate(1, 0, 0)
	}
	return t.UTC()
}

// noteScheduleCall remembers a call whose result sets or ends a schedule.
func noteScheduleCall(conv *Conversation, call toolCall) {
	if scheduleTools[call.Name] {
		conv.schedCalls = append(slices.Clone(conv.schedCalls), schedCall{call.ID, call.Name, call.Input})
	}
}

// schedResult is the part of a schedule call's toolUseResult ccs reads.
type schedResult struct {
	ScheduledFor int64  `json:"scheduledFor"`
	Stopped      bool   `json:"stopped"`
	ID           string `json:"id"`
	Human        string `json:"humanSchedule"`
	Recurring    bool   `json:"recurring"`
	Durable      bool   `json:"durable"`
	NextFireAt   int64  `json:"nextFireAt"`
}

// noteScheduleResult applies the result of a call noteScheduleCall kept: from
// the line's toolUseResult (read only for these results), else the input and
// the result's text.
func noteScheduleResult(conv *Conversation, id string, content json.RawMessage, isError bool, line []byte, ts string) {
	i := slices.IndexFunc(conv.schedCalls, func(c schedCall) bool { return c.ID == id })
	if i < 0 {
		return
	}
	call := conv.schedCalls[i]
	conv.schedCalls = slices.Delete(slices.Clone(conv.schedCalls), i, i+1)
	if isError {
		return
	}
	var r struct {
		ToolUseResult *schedResult `json:"toolUseResult"`
	}
	if bytes.Contains(line, []byte(`"toolUseResult"`)) {
		json.Unmarshal(line, &r)
	}
	res := r.ToolUseResult
	known := res != nil // Claude Code's own record of what it scheduled; else the text decides
	if !known {
		res = &schedResult{}
	}
	text := ResultText(content)
	var in struct {
		Delay     int64  `json:"delaySeconds"`
		Reason    string `json:"reason"`
		Prompt    string `json:"prompt"`
		Stop      bool   `json:"stop"`
		Cron      string `json:"cron"`
		Recurring *bool  `json:"recurring"`
		Durable   bool   `json:"durable"`
		ID        string `json:"id"`
	}
	json.Unmarshal(call.Input, &in)
	set, _ := time.Parse(time.RFC3339Nano, ts)
	end := func(match func(s Schedule) bool) {
		for j, s := range conv.Schedules {
			if !s.Ended && match(s) {
				conv.Schedules = slices.Clone(conv.Schedules)
				conv.Schedules[j].Ended = true
			}
		}
	}
	switch call.Name {
	case "ScheduleWakeup":
		// One dynamic wakeup at a time: a new one replaces the last, and a
		// stop ends it.
		end(func(s Schedule) bool { return s.Kind == "wakeup" })
		if in.Stop || res.Stopped {
			return
		}
		// Only a time Claude Code says it set counts: a zero scheduledFor
		// means it scheduled nothing (a loop at its limit, say), and the
		// requested delay is never a stand-in for one.
		var next time.Time
		switch {
		case res.ScheduledFor > 0:
			next = time.UnixMilli(res.ScheduledFor)
		case !known && !set.IsZero():
			if m := wakeIn.FindStringSubmatch(text); m != nil {
				secs, _ := strconv.Atoi(m[1])
				next = set.Add(time.Duration(secs) * time.Second)
			}
		}
		if next.IsZero() {
			return
		}
		conv.Schedules = append(slices.Clone(conv.Schedules), Schedule{Kind: "wakeup", Desc: oneLine(cmp.Or(in.Reason, in.Prompt)), Set: ts, Next: next.UTC()})
	case "CronCreate":
		s := Schedule{ID: res.ID, Kind: "cron", Set: ts, Recurring: res.Recurring, Durable: res.Durable}
		if res.NextFireAt > 0 {
			s.Next = time.UnixMilli(res.NextFireAt).UTC()
		}
		if !known {
			m := cronTaskID.FindStringSubmatch(text)
			if m == nil {
				return
			}
			s.ID = m[1]
			s.Recurring = in.Recurring == nil || *in.Recurring
			s.Durable = in.Durable && !strings.Contains(text, "Session-only")
			s.Next = nextRunAfter(text, set)
		}
		if s.ID == "" {
			return
		}
		s.Desc = oneLine(strings.TrimSpace(cmp.Or(res.Human, in.Cron) + ": " + in.Prompt))
		conv.Schedules = append(slices.Clone(conv.Schedules), s)
	case "CronDelete":
		end(func(s Schedule) bool { return s.Kind == "cron" && s.ID == in.ID })
	}
}

// PendingSchedules is conv's wake-ups still to come, for a session open in a
// claude process that started at since (zero when unknown): not ended, set
// by that process unless durable, and a one-shot or wakeup with its time
// still ahead. A recurring cron stays pending until deleted; once its first
// recorded time has passed its next isn't known (Next zero). Soonest first,
// those with no known time last.
func PendingSchedules(conv Conversation, since, now time.Time) []Schedule {
	var out []Schedule
	for _, s := range conv.Schedules {
		set, err := time.Parse(time.RFC3339Nano, s.Set)
		if s.Ended || (!s.Durable && err == nil && set.Before(since)) {
			continue
		}
		if !s.Next.IsZero() && !now.Before(s.Next) {
			if !s.Recurring {
				continue // it has fired
			}
			s.Next = time.Time{}
		}
		if s.Next.IsZero() && !s.Recurring {
			continue
		}
		out = append(out, s)
	}
	slices.SortStableFunc(out, func(a, b Schedule) int {
		switch {
		case a.Next.IsZero() != b.Next.IsZero():
			if a.Next.IsZero() {
				return 1
			}
			return -1
		}
		return a.Next.Compare(b.Next)
	})
	return out
}

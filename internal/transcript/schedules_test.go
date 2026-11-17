package transcript

import (
	"fmt"
	"testing"
	"time"
)

// scheduleLines are a call and its result as Claude Code writes them.
func scheduleLines(conv *Conversation, id, name, input, ts, result, toolUseResult string) {
	ParseLine(conv, fmt.Appendf(nil, `{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`, ts, id, name, input))
	tur := ""
	if toolUseResult != "" {
		tur = `,"toolUseResult":` + toolUseResult
	}
	ParseLine(conv, fmt.Appendf(nil, `{"type":"user","timestamp":%q,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}%s}`, ts, id, result, tur))
}

func TestSchedules(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	ts := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	var conv Conversation

	// A wakeup, then another that replaces it.
	scheduleLines(&conv, "w1", "ScheduleWakeup", `{"delaySeconds":1800,"reason":"watching CI","prompt":"/loop check"}`, ts(10*time.Minute), "Scheduled.", fmt.Sprintf(`{"scheduledFor":%d,"clampedDelaySeconds":1800,"wasClamped":false}`, ms(20*time.Minute)))
	scheduleLines(&conv, "w2", "ScheduleWakeup", `{"delaySeconds":600,"reason":"waiting on review"}`, ts(5*time.Minute), "Scheduled.", fmt.Sprintf(`{"scheduledFor":%d}`, ms(5*time.Minute)))
	// A one-shot cron with its next fire time, a recurring one without, and one deleted.
	scheduleLines(&conv, "c1", "CronCreate", `{"cron":"13 22 16 11 *","recurring":false,"prompt":"Read-only check\nof the deploy"}`, ts(4*time.Minute), "Scheduled one-shot task 235f1434 (13 22 16 11 *). Next run: Nov 16 10:13pm.", fmt.Sprintf(`{"id":"235f1434","humanSchedule":"13 22 16 11 *","recurring":false,"durable":false,"nextFireAt":%d}`, ms(10*time.Hour)))
	scheduleLines(&conv, "c2", "CronCreate", `{"cron":"*/3 * * * *","prompt":"poll"}`, ts(3*time.Minute), "Scheduled recurring job 3572d5a8 (Every 3 minutes). Session-only (not written to disk, dies when Claude exits).", fmt.Sprintf(`{"id":"3572d5a8","humanSchedule":"Every 3 minutes","recurring":true,"durable":false,"nextFireAt":%d}`, ms(-time.Minute)))
	scheduleLines(&conv, "c3", "CronCreate", `{"cron":"*/4 * * * *","prompt":"poll more"}`, ts(3*time.Minute), "Scheduled recurring job 07229c48 (Every 4 minutes).", `{"id":"07229c48","humanSchedule":"Every 4 minutes","recurring":true,"durable":false}`)
	scheduleLines(&conv, "d1", "CronDelete", `{"id":"07229c48"}`, ts(2*time.Minute), "Deleted.", "")
	// A failed call sets nothing; an earlier process's cron died with it; one already fired is past.
	scheduleLines(&conv, "c4", "CronCreate", `{"cron":"bad","prompt":"x"}`, ts(time.Minute), "Invalid cron", "")
	conv.Schedules = append(conv.Schedules, Schedule{ID: "old", Kind: "cron", Recurring: true, Set: ts(2 * time.Hour)},
		Schedule{ID: "fired", Kind: "cron", Set: ts(time.Minute), Next: now.Add(-time.Second)},
		Schedule{ID: "kept", Kind: "cron", Durable: true, Recurring: true, Set: ts(3 * time.Hour)})

	var got []string
	for _, s := range PendingSchedules(conv, since, now) {
		got = append(got, s.Kind+":"+s.ID+":"+s.Desc)
	}
	want := []string{"wakeup::waiting on review", "cron:235f1434:13 22 16 11 *: Read-only check of the deploy", "cron:3572d5a8:Every 3 minutes: poll", "cron:kept:"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("pending:\n%q\nwant\n%q", got, want)
	}
	if p := PendingSchedules(conv, since, now); !p[0].Next.Equal(now.Add(5 * time.Minute).Truncate(time.Millisecond)) {
		t.Errorf("the soonest is the wakeup: %v", p[0].Next)
	}

	// A stop ends the wakeup; the past it fired at no longer counts.
	scheduleLines(&conv, "w3", "ScheduleWakeup", `{"stop":true}`, ts(0), "Loop stopped", `{"scheduledFor":0,"stopped":true,"cancelledWakeups":1}`)
	for _, s := range PendingSchedules(conv, since, now) {
		if s.Kind == "wakeup" {
			t.Errorf("stopped wakeup still pending: %+v", s)
		}
	}

	// Without toolUseResult (ccs prune strips it): times and ids from the
	// result text as Claude Code words it; a reply with no time sets nothing.
	conv = Conversation{}
	scheduleLines(&conv, "w4", "ScheduleWakeup", `{"delaySeconds":1800,"reason":"soon"}`, ts(0), "Next wakeup scheduled for 12:30:44 (in 1844s). If you owe the user a status update…", "")
	next := now.Add(3 * time.Hour).In(time.Local)
	scheduleLines(&conv, "c5", "CronCreate", `{"cron":"0 9 * * *","prompt":"once","recurring":false,"durable":true}`, ts(0),
		"Scheduled one-shot job abc12345 (0 9 * * *). Next run: "+next.Format("Jan 2 3:04pm")+". Session-only (not written to disk, dies when Claude exits).", "")
	scheduleLines(&conv, "c6", "CronCreate", `{"cron":"*/5 * * * *","prompt":"poll"}`, ts(0), "Scheduled recurring task def67890 (Every 5 minutes).", "")
	p := PendingSchedules(conv, since, now)
	if len(p) != 3 || !p[0].Next.Equal(now.Add(1844*time.Second)) ||
		p[1].ID != "abc12345" || p[1].Recurring || p[1].Durable || !p[1].Next.Equal(next.Truncate(time.Minute).UTC()) ||
		p[2].ID != "def67890" || !p[2].Recurring {
		t.Errorf("from the result text: %+v", p)
	}

	// scheduledFor 0 without a stop: nothing was scheduled, whatever was asked.
	conv = Conversation{}
	scheduleLines(&conv, "w5", "ScheduleWakeup", `{"delaySeconds":600,"reason":"later"}`, ts(0), "Wakeup not scheduled: the loop reached its maximum duration.", `{"scheduledFor":0,"clampedDelaySeconds":0,"wasClamped":false}`)
	scheduleLines(&conv, "w6", "ScheduleWakeup", `{"delaySeconds":600,"reason":"later"}`, ts(0), "Scheduled.", "")
	// Durable asked for, but Claude Code's record says it isn't: the record wins.
	scheduleLines(&conv, "c7", "CronCreate", `{"cron":"0 9 * * *","prompt":"daily","durable":true}`, ts(3*time.Hour), "Scheduled.", `{"id":"aaa11111","humanSchedule":"Every day at 9:00 AM","recurring":true,"durable":false}`)
	if p := PendingSchedules(conv, since, now); len(p) != 0 {
		t.Errorf("no wakeup without a time Claude Code set, and a non-durable cron dies with its process: %+v", p)
	}
}

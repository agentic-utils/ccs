package transcript

// Untrusted text is cleaned once, where it enters ccs. A transcript is
// written by Claude Code and Codex from tool output, other sessions'
// messages and web pages, so any string read from it can carry terminal
// control sequences. Each parsed line's new records, and the conversation's
// own display fields, go through CleanText as soon as the line is applied,
// so nothing drawn from a Conversation needs to remember to clean it.
//
// Left raw on purpose, because code matches them rather than draws them:
// SessionID, FilePath, PeerRef.Key and .Addr (the other end's copy and
// socket path are compared byte for byte), Pending's raw tool input (decoded
// where it is drawn, and cleaned after decoding: DecodeClean), PRs
// (validated as github.com addresses before use), and a queued message's key
// and queueTaken (matched against the copy taken in; its Text is cleaned by
// noteEnqueued, not cleanSince, since the capped queue can keep its length).

// ingestMark is how far each record list had got before a line was applied:
// everything at or past it is new and still raw.
type ingestMark struct {
	messages, tools, tasks, peers, asks, schedules, spawns, launches int
}

func markOf(c *Conversation) ingestMark {
	return ingestMark{len(c.Messages), len(c.Tools), len(c.Tasks), len(c.Peers), len(c.Asks), len(c.Schedules), len(c.Spawns), len(c.Launches)}
}

// cleanSince cleans what a line added after m, and the scalar display
// fields, which any line may set. CleanText returns clean text as it is,
// without allocating, so text already clean costs one scan.
func cleanSince(c *Conversation, m ingestMark) {
	for _, p := range []*string{&c.Title, &c.Team, &c.Cwd, &c.firstCwd, &c.lastCwd, &c.Model, &c.ActiveModel, &c.LastError, &c.LastErrorTs, &c.FirstTimestamp, &c.LastTimestamp} {
		*p = CleanText(*p)
	}
	for i := m.messages; i < len(c.Messages); i++ {
		msg := &c.Messages[i]
		msg.Role, msg.Text, msg.Ts, msg.To, msg.Note = CleanText(msg.Role), CleanText(msg.Text), CleanText(msg.Ts), CleanText(msg.To), CleanText(msg.Note)
	}
	for i := m.tools; i < len(c.Tools); i++ {
		t := &c.Tools[i]
		t.Name, t.Arg, t.Ts = CleanText(t.Name), CleanText(t.Arg), CleanText(t.Ts)
	}
	for i := m.tasks; i < len(c.Tasks); i++ {
		t := &c.Tasks[i]
		t.Kind, t.Desc, t.Started, t.Status = CleanText(t.Kind), CleanText(t.Desc), CleanText(t.Started), CleanText(t.Status)
	}
	for i := m.peers; i < len(c.Peers); i++ {
		c.Peers[i].Name = CleanText(c.Peers[i].Name)
	}
	for i := m.asks; i < len(c.Asks); i++ {
		cleanAsk(&c.Asks[i])
	}
	for i := m.schedules; i < len(c.Schedules); i++ {
		s := &c.Schedules[i]
		s.Kind, s.Desc, s.Set = CleanText(s.Kind), CleanText(s.Desc), CleanText(s.Set)
	}
	for i := m.spawns; i < len(c.Spawns); i++ {
		c.Spawns[i].Text, c.Spawns[i].Ts = CleanText(c.Spawns[i].Text), CleanText(c.Spawns[i].Ts)
	}
	for i := m.launches; i < len(c.Launches); i++ {
		c.Launches[i] = CleanText(c.Launches[i])
	}
}

func cleanAsk(a *Ask) {
	a.Ts, a.AnswerTs, a.Result = CleanText(a.Ts), CleanText(a.AnswerTs), CleanText(a.Result)
	for j := range a.Questions {
		q := &a.Questions[j]
		q.Question = CleanText(q.Question)
		for k := range q.Options {
			o := &q.Options[k]
			o.Label, o.Description = CleanText(o.Label), CleanText(o.Description)
		}
	}
	for j := range a.Answers {
		x := &a.Answers[j]
		x.Question, x.Answer, x.Notes = CleanText(x.Question), CleanText(x.Answer), CleanText(x.Notes)
	}
}

// DecodeClean is how text decoded from transcript text is cleaned: decoding
// (an HTML entity such as &#27;, a JSON \u001b) can make a control the
// first cleaning never saw, so whatever decodes transcript text cleans the
// result after decoding.
func DecodeClean(decoded string) string { return CleanText(decoded) }

package transcript

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// Ask is one AskUserQuestion call and how the user answered it. Claude Code
// keeps the menu and the choice out of the conversation's text (a tool call
// and its result), so they're kept here beside the messages, placed by At as
// Tools are, and counts, times and the list's search ignore them; read_session
// shows them.
type Ask struct {
	ID        string
	At        int    // drawn after message At-1
	Ts        string // when it was asked
	Questions []AskQuestion
	// Answered is set once its result arrives; AnswerTs is when, AnswerAt where.
	Answered bool
	AnswerTs string
	AnswerAt int // drawn after message AnswerAt-1: something may arrive while the menu waits
	// Answers is each question's choice and notes, from the result's
	// toolUseResult. Result is the result's text, for one without them (a
	// pruned transcript, a declined menu, an older Claude Code).
	Answers []AskAnswer
	Result  string
	Failed  bool // the result was an error: the menu was declined or interrupted
	// HookAllowed: a PermissionRequest hook allowed it (ccs hook permission,
	// answered from ccs), as the transcript's hook_permission_decision says.
	HookAllowed bool
}

// noteAskHook records a PermissionRequest hook allowing an AskUserQuestion
// call, which Claude Code writes as an attachment before the call's result.
func noteAskHook(conv *Conversation, id, decision, event string) {
	if decision != "allow" || event != "PermissionRequest" || id == "" {
		return
	}
	for i := len(conv.Asks) - 1; i >= 0; i-- {
		if conv.Asks[i].ID == id {
			if !conv.Asks[i].HookAllowed {
				conv.Asks = slices.Clone(conv.Asks) // copies of the conversation share the slice
				conv.Asks[i].HookAllowed = true
			}
			return
		}
	}
}

// AskQuestion is one question of an AskUserQuestion call, as asked.
type AskQuestion struct {
	Question    string
	Header      string // the short chip Claude Code shows above the question
	MultiSelect bool
	Options     []AskOption
}

// AskOption is one option of a question. Preview is the text Claude Code
// shows beside it while it's highlighted (a draft, a snippet, a mockup),
// line breaks kept.
type AskOption struct{ Label, Description, Preview string }

// AskAnswer is the user's choice for one question, and any notes they added.
type AskAnswer struct{ Question, Answer, Notes string }

// noteAsk records an AskUserQuestion call, and its place among the tool
// calls, which the preview draws as the questions; false when its input has
// none, for the caller to note it as any other tool call.
func noteAsk(conv *Conversation, call toolCall, ts string) bool {
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	if json.Unmarshal(call.Input, &in) != nil || len(in.Questions) == 0 {
		return false
	}
	a := Ask{ID: call.ID, At: len(conv.Messages), Ts: ts}
	for _, q := range in.Questions {
		aq := AskQuestion{Question: q.Question, Header: q.Header, MultiSelect: q.MultiSelect}
		for _, o := range q.Options {
			aq.Options = append(aq.Options, AskOption{o.Label, o.Description, o.Preview})
		}
		a.Questions = append(a.Questions, aq)
	}
	conv.Asks = append(conv.Asks, a)
	conv.Tools = append(conv.Tools, ToolUse{At: a.At, Name: call.Name, Arg: oneLine(a.Questions[0].Question), Ts: ts, Ask: len(conv.Asks)})
	return true
}

// askAwaiting is the index in conv.Asks of the unanswered call id, or -1.
func askAwaiting(conv *Conversation, id string) int {
	for i := len(conv.Asks) - 1; i >= 0; i-- {
		if conv.Asks[i].ID == id {
			if conv.Asks[i].Answered {
				return -1
			}
			return i
		}
	}
	return -1
}

// noteAskResult records the answer to an AskUserQuestion call: from the
// line's toolUseResult (read only for such a result, since others can be
// large), else the result's text.
func noteAskResult(conv *Conversation, i int, content json.RawMessage, isError bool, line []byte, ts string) {
	conv.Asks = slices.Clone(conv.Asks) // copies of the conversation share the slice
	a := &conv.Asks[i]
	// An earlier ask, so cleanSince doesn't see it: cleaned here, and the
	// answers looked up by the cleaned question its record already holds.
	a.Answered, a.AnswerTs, a.AnswerAt, a.Failed = true, CleanText(ts), len(conv.Messages), isError
	a.Result = CleanText(strings.TrimSpace(ResultText(content)))
	// The answer is drawn where it arrived, as an entry of its own.
	conv.Tools = append(conv.Tools, ToolUse{At: a.AnswerAt, Name: "AskUserQuestion", Ts: a.AnswerTs, Ask: i + 1, Answer: true})
	if !bytes.Contains(line, []byte(`"answers"`)) {
		return
	}
	var r struct {
		ToolUseResult struct {
			Answers     map[string]string `json:"answers"`
			Annotations map[string]struct {
				Notes string `json:"notes"`
			} `json:"annotations"`
		} `json:"toolUseResult"`
	}
	if json.Unmarshal(line, &r) != nil {
		return
	}
	answers, notesOf := map[string]string{}, map[string]string{}
	for q, v := range r.ToolUseResult.Answers {
		answers[CleanText(q)] = CleanText(v)
	}
	for q, v := range r.ToolUseResult.Annotations {
		notesOf[CleanText(q)] = CleanText(v.Notes)
	}
	for _, q := range a.Questions { // in the order asked
		ans, ok := answers[q.Question]
		notes := strings.TrimSpace(notesOf[q.Question])
		if ok || notes != "" {
			a.Answers = append(a.Answers, AskAnswer{q.Question, ans, notes})
		}
	}
}

// Text is the question as read_session shows it: the question, then its
// options numbered as Claude Code shows them.
func (q AskQuestion) Text() string {
	var b strings.Builder
	b.WriteString(q.Question)
	if q.MultiSelect {
		b.WriteString(" (pick any)")
	}
	for i, o := range q.Options {
		b.WriteString("\n" + strconv.Itoa(i+1) + ". " + o.Label)
		if o.Description != "" {
			b.WriteString(": " + o.Description)
		}
		if o.Preview != "" {
			b.WriteString("\n   preview:")
			for _, l := range strings.Split(o.Preview, "\n") {
				b.WriteString("\n   | " + l)
			}
		}
	}
	return b.String()
}

// Text is the answer as read_session shows it.
func (a AskAnswer) Text() string {
	s := a.Question + "\n→ " + a.Answer
	if a.Answer == "" {
		s = a.Question + "\n→ (no option chosen)"
	}
	if a.Notes != "" {
		s += "\nNotes: " + a.Notes
	}
	return s
}

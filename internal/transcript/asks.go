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
}

// AskQuestion is one question of an AskUserQuestion call, as asked.
type AskQuestion struct {
	Question    string
	MultiSelect bool
	Options     []AskOption
}

// AskOption is one option of a question.
type AskOption struct{ Label, Description string }

// AskAnswer is the user's choice for one question, and any notes they added.
type AskAnswer struct{ Question, Answer, Notes string }

// noteAsk records an AskUserQuestion call.
func noteAsk(conv *Conversation, call toolCall, ts string) {
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	if json.Unmarshal(call.Input, &in) != nil || len(in.Questions) == 0 {
		return
	}
	a := Ask{ID: call.ID, At: len(conv.Messages), Ts: ts}
	for _, q := range in.Questions {
		aq := AskQuestion{Question: q.Question, MultiSelect: q.MultiSelect}
		for _, o := range q.Options {
			aq.Options = append(aq.Options, AskOption{o.Label, o.Description})
		}
		a.Questions = append(a.Questions, aq)
	}
	conv.Asks = append(conv.Asks, a)
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
	a.Answered, a.AnswerTs, a.AnswerAt, a.Failed = true, ts, len(conv.Messages), isError
	a.Result = strings.TrimSpace(ResultText(content))
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
	for _, q := range a.Questions { // in the order asked
		ans, ok := r.ToolUseResult.Answers[q.Question]
		notes := strings.TrimSpace(r.ToolUseResult.Annotations[q.Question].Notes)
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

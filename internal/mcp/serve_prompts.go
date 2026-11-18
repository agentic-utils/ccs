package mcp

// Prompts held for the page. `ccs hook permission` (hook.go) connects to
// hookSockPath for each permission prompt or AskUserQuestion menu, and the
// prompt waits here until the page answers it, the hook goes away, the wait
// (--hook-wait) runs out, or (not in --preview) a key is pressed on the Mac,
// which means the user is back there: then it's let go to its tab. Letting
// go sends no decision, so Claude Code shows the prompt as usual; nothing
// here ever denies or allows on its own. The answer goes back on the hook's
// own connection, so it settles that prompt and no other.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// keyIdle is sessions.KeyIdle, stubbed in tests.
var keyIdle = sessions.KeyIdle

// keyPoll is how often a held prompt checks for a key press on the Mac.
var keyPoll = 3 * time.Second

type heldPrompt struct {
	id    string
	nonce string // the hook's, echoed on each line back
	req   hookRequest
	at    time.Time
	reply chan hookReply // buffered: one answer
}

type promptBook struct {
	sync.Mutex
	m map[string]*heldPrompt
}

func (b *promptBook) add(p *heldPrompt) {
	b.Lock()
	defer b.Unlock()
	if b.m == nil {
		b.m = map[string]*heldPrompt{}
	}
	b.m[p.id] = p
}

// take removes the prompt, so it's answered at most once.
func (b *promptBook) take(id string) *heldPrompt {
	b.Lock()
	defer b.Unlock()
	p := b.m[id]
	delete(b.m, id)
	return p
}

func (b *promptBook) get(id string) *heldPrompt {
	b.Lock()
	defer b.Unlock()
	return b.m[id]
}

// list is the prompts held, oldest first, for session (all when "").
func (b *promptBook) list(session string) []*heldPrompt {
	b.Lock()
	defer b.Unlock()
	var out []*heldPrompt
	for _, p := range b.m {
		if session == "" || p.req.SessionID == session {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// listenHooks makes this ccs serve the owner of the hold (both sockets),
// taking over from a TUI that holds them; another ccs serve refuses. It
// returns a stop function. listenControl is the same: the holder opens both.
func (s *server) listenHooks() (func(), error) { return s.ownHold() }

func (s *server) listenControl() (func(), error) { return s.ownHold() }

func (s *server) ownHold() (func(), error) {
	if s.hold.Owns() {
		return func() {}, nil
	}
	if err := s.hold.Listen(true); err != nil {
		if errors.Is(err, ErrHeldElsewhere) {
			return nil, fmt.Errorf("another ccs serve already answers prompts on %s (%v)", hookSockPath(), err)
		}
		return nil, err
	}
	return func() { s.hold.Close("ccs serve stopped") }, nil
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// promptView is a held prompt as the page shows it.
type promptView struct {
	ID, Session, Name, Tool, Summary, Input, Age string
	Questions                                    []questionView // AskUserQuestion's; none for a permission prompt
}

type questionView struct {
	Key, Question, Header string
	Multi                 bool
	Options               []optionView
}

type optionView struct{ Label, Description string }

type askInput struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

func (s *server) promptViews(session string, names map[string]string) []promptView {
	var out []promptView
	for _, p := range s.prompts.list(session) {
		v := promptView{ID: p.id, Session: p.req.SessionID, Name: names[p.req.SessionID], Tool: p.req.ToolName,
			Age: s.now().Sub(p.at).Round(time.Second).String()}
		if v.Name == "" {
			v.Name = shortID(p.req.SessionID)
		}
		if p.req.ToolName == "AskUserQuestion" {
			var in askInput
			json.Unmarshal(p.req.ToolInput, &in)
			for i, q := range in.Questions {
				qv := questionView{Key: "q" + strconv.Itoa(i), Question: transcript.CleanText(q.Question), Header: transcript.CleanText(q.Header), Multi: q.MultiSelect}
				for _, o := range q.Options {
					qv.Options = append(qv.Options, optionView{transcript.CleanText(o.Label), transcript.CleanText(o.Description)})
				}
				v.Questions = append(v.Questions, qv)
			}
		} else {
			v.Summary = transcript.CleanText(transcript.ToolSummary(p.req.ToolInput))
			var pretty any
			if json.Unmarshal(p.req.ToolInput, &pretty) == nil {
				b, _ := json.MarshalIndent(pretty, "", "  ")
				v.Input = transcript.CleanText(string(b))
			}
		}
		out = append(out, v)
	}
	return out
}

// promptDecision turns the page's choice into the reply: allow (for an
// AskUserQuestion, with the chosen labels, each one of its options), deny,
// or let it go to the Mac (no decision). The hook checks the answers again
// against its own input and builds what it gives Claude Code itself.
func promptDecision(p *heldPrompt, choice string, answers map[string][]string, who string) (hookReply, string, error) {
	switch choice {
	case "mac":
		return hookReply{}, "Let it go to its tab on the Mac, where it shows as usual.", nil
	case "deny":
		return hookReply{Behavior: "deny", Message: "Denied from ccs serve by " + who}, fmt.Sprintf("Deny %s.", p.req.ToolName), nil
	case "allow":
		if p.req.ToolName == "AskUserQuestion" {
			return hookReply{}, "", errors.New("a question is answered by choosing its options")
		}
		return hookReply{Behavior: "allow"}, fmt.Sprintf("Allow %s: %s", p.req.ToolName, transcript.ToolSummary(p.req.ToolInput)), nil
	case "answer":
		if p.req.ToolName != "AskUserQuestion" {
			return hookReply{}, "", errors.New("only a question is answered with options")
		}
		var in askInput
		if json.Unmarshal(p.req.ToolInput, &in) != nil || len(in.Questions) == 0 {
			return hookReply{}, "", errors.New("couldn't read the question")
		}
		out := map[string]string{}
		var said []string
		for i, q := range in.Questions {
			got := answers["q"+strconv.Itoa(i)]
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			if len(got) == 0 || (!q.MultiSelect && len(got) > 1) {
				return hookReply{}, "", fmt.Errorf("pick an answer for %q", q.Question)
			}
			for _, g := range got {
				if !slices.Contains(labels, g) {
					return hookReply{}, "", fmt.Errorf("%q isn't one of the options for %q", g, q.Question)
				}
			}
			out[q.Question] = strings.Join(got, ", ")
			said = append(said, fmt.Sprintf("%s → %s", q.Question, strings.Join(got, ", ")))
		}
		return hookReply{Behavior: "allow", Answers: out}, "Answer:\n" + strings.Join(said, "\n"), nil
	}
	return hookReply{}, "", fmt.Errorf("unknown choice %q", choice)
}

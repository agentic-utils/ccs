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
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
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

// listenHooks opens the socket hooks connect to, refusing when another ccs
// serve already answers there. It returns a stop function.
func (s *server) listenHooks() (func(), error) {
	path := hookSockPath()
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("another ccs serve already answers prompts on %s", path)
	}
	os.Remove(path) // a socket left by one that's gone
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.holdPrompt(c)
		}
	}()
	return func() { ln.Close(); os.Remove(path) }, nil
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}

// holdPrompt keeps one hook's prompt until it's answered or let go.
func (s *server) holdPrompt(c net.Conn) {
	defer c.Close()
	// Only this user's own hooks: the socket's 0600 should already see to it.
	if uid, err := peerUID(c); err != nil || uid != uint32(os.Getuid()) {
		state.LogActivity("serve: refused a hook connection from uid %d (%v)", uid, err)
		return
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	if !sc.Scan() {
		return
	}
	var hello hookHello
	if json.Unmarshal(sc.Bytes(), &hello) != nil || hello.Nonce == "" || hello.Request.SessionID == "" || hello.Request.ToolName == "" {
		return
	}
	req := hello.Request
	c.SetReadDeadline(time.Time{})
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if json.NewEncoder(c).Encode(hookReply{Nonce: hello.Nonce, Held: true}) != nil {
		return
	}
	p := &heldPrompt{id: newPromptID(), nonce: hello.Nonce, req: req, at: s.now(), reply: make(chan hookReply, 1)}
	s.prompts.add(p)
	defer s.prompts.take(p.id)
	state.LogActivity("serve: holding %s's %s prompt %s for the page", shortID(req.SessionID), req.ToolName, p.id)

	gone := make(chan struct{}) // the hook went away: Claude Code killed it, or the session ended
	go func() {
		buf := make([]byte, 1)
		c.Read(buf)
		close(gone)
	}()
	var back <-chan time.Time
	if !s.cfg.preview { // in a preview the user is at the Mac by design
		t := time.NewTicker(keyPoll)
		defer t.Stop()
		back = t.C
	}
	limit := time.NewTimer(s.cfg.hookWait)
	defer limit.Stop()
	letGo := func(why string) {
		state.LogActivity("serve: let %s's prompt %s go to its tab: %s", shortID(req.SessionID), p.id, why)
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		json.NewEncoder(c).Encode(hookReply{Nonce: p.nonce})
	}
	for {
		select {
		case r := <-p.reply:
			r.Nonce = p.nonce
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := json.NewEncoder(c).Encode(r); err != nil {
				state.LogActivity("serve: answering prompt %s failed: %v", p.id, err)
			}
			return
		case <-gone:
			state.LogActivity("serve: %s's prompt %s ended before an answer", shortID(req.SessionID), p.id)
			return
		case <-limit.C:
			letGo(fmt.Sprintf("no answer within %s", s.cfg.hookWait))
			return
		case <-back:
			if idle, ok := keyIdle(); ok && idle < s.now().Sub(p.at) {
				letGo("a key was pressed on the Mac")
				return
			}
		}
	}
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

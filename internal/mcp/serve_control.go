package mcp

// The control socket: how the ccs TUI on this Mac lists the prompts ccs
// serve holds for the page (serve_prompts.go) and answers one by its id,
// never by typing into a terminal. It lives beside the hook socket and is
// guarded the same way on both ends: a 0600 socket in this user's private
// folder (trustedSocket), and the kernel's word that the other end runs as
// this user (peerUID). An answer carries only a choice and a question's
// labels; it goes through promptDecision like the page's, and the hook
// builds what it hands Claude Code from its own input.

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// ControlSockPath is the control socket, beside the hook socket.
var ControlSockPath = func() string { return filepath.Join(filepath.Dir(hookSockPath()), "serve-control.sock") }

// tuiUser is who the activity log names for an answer from the TUI.
const tuiUser = "tui (this Mac)"

// HeldPrompt is a prompt ccs serve holds, as the TUI shows it.
type HeldPrompt struct {
	ID        string         `json:"id"`
	Session   string         `json:"session"`
	Tool      string         `json:"tool"`
	At        time.Time      `json:"at"` // when it was held: the oldest is answered first
	Summary   string         `json:"summary,omitempty"`
	Questions []HeldQuestion `json:"questions,omitempty"` // an AskUserQuestion's; none for a permission prompt
	// Options are a permission prompt's menu, as Claude Code's: Yes, a
	// "don't ask again" per suggestion ccs offers, No. None from an older
	// ccs serve: then Yes and No.
	Options []HeldOption `json:"options,omitempty"`
}

// HeldQuestion is one question of a held AskUserQuestion.
type HeldQuestion struct {
	Question     string   `json:"question"`
	Header       string   `json:"header,omitempty"`
	Multi        bool     `json:"multi,omitempty"`
	Options      []string `json:"options"`
	Descriptions []string `json:"descriptions,omitempty"` // each option's, in order
}

// controlRequest is a line the TUI sends. It has no field for tool input:
// unknown fields are dropped when it's read, so nothing can be smuggled in.
type controlRequest struct {
	Op      string              `json:"op"` // list, answer, yield
	Prompt  string              `json:"prompt,omitempty"`
	Choice  string              `json:"choice,omitempty"`  // allow, always:<n>, deny, mac, answer
	Answers map[string][]string `json:"answers,omitempty"` // q<i> → labels, or one free-text answer when Other[q<i>]
	Other   map[string]bool     `json:"other,omitempty"`   // q<i> answered with free text ("Other…")
	Notes   map[string]string   `json:"notes,omitempty"`   // q<i>'s note on its answer; a permission prompt's deny takes q0's as what to tell Claude
}

type controlReply struct {
	Prompts []HeldPrompt `json:"prompts,omitempty"`
	Text    string       `json:"text,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// heldList is a book's prompts as the TUI shows them.
func heldList(b *promptBook) []HeldPrompt {
	var out []HeldPrompt
	for _, p := range b.list("") {
		h := HeldPrompt{ID: p.id, Session: p.req.SessionID, Tool: p.req.ToolName, At: p.at}
		if p.req.ToolName == "AskUserQuestion" {
			var in askInput
			json.Unmarshal(p.req.ToolInput, &in)
			for _, q := range in.Questions {
				hq := HeldQuestion{Question: transcript.CleanText(q.Question), Header: transcript.CleanText(q.Header), Multi: q.MultiSelect}
				for _, o := range q.Options {
					hq.Options = append(hq.Options, transcript.CleanText(o.Label))
					hq.Descriptions = append(hq.Descriptions, transcript.CleanText(o.Description))
				}
				h.Questions = append(h.Questions, hq)
			}
		} else {
			h.Summary = transcript.CleanText(transcript.ToolSummary(p.req.ToolInput))
			h.Options = permOptions(p.req)
		}
		out = append(out, h)
	}
	return out
}

// ErrNoServe: no ccs serve is holding prompts.
var ErrNoServe = errors.New("no ccs serve is holding prompts")

// ErrOutcomeUnknown: the request reached ccs serve but its reply didn't
// come back, so an answer may or may not have gone through.
var ErrOutcomeUnknown = errors.New("sent, but ccs serve's reply never came: the outcome is unknown")

// controlCall sends one request to ccs serve's control socket, trusting it
// only as the hook does: this user's private socket, and a listener the
// kernel says runs as this user.
func controlCall(req controlRequest) (controlReply, error) {
	path := ControlSockPath()
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return controlReply{}, ErrNoServe
	}
	if err := trustedSocket(path); err != nil {
		return controlReply{}, err
	}
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return controlReply{}, ErrNoServe
	}
	defer c.Close()
	if uid, err := peerUID(c); err != nil || uid != uint32(os.Getuid()) {
		return controlReply{}, errors.New("the control socket isn't ccs serve's (another user's process)")
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	line, _ := json.Marshal(req)
	if _, err := c.Write(append(line, '\n')); err != nil {
		return controlReply{}, err
	}
	// From here the request was sent: ccs serve may already have settled the
	// prompt, so a missing or unreadable reply is never "failed".
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	if !sc.Scan() {
		return controlReply{}, ErrOutcomeUnknown
	}
	var reply controlReply
	if err := json.Unmarshal(sc.Bytes(), &reply); err != nil {
		return controlReply{}, ErrOutcomeUnknown
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

// HeldPrompts lists the prompts ccs serve holds; ErrNoServe when none
// runs. A var so TUI tests stub it.
var HeldPrompts = func() ([]HeldPrompt, error) {
	r, err := controlCall(controlRequest{Op: "list"})
	return r.Prompts, err
}

// AnswerHeld answers a held prompt by its id: choice allow, always:<n> (a
// "don't ask again" from its Options), deny (notes[0] what to tell Claude),
// mac (show it in its tab) or answer, with a question's chosen labels by its index,
// or, for one in other, its free-text answer.
// It says what was done. A var so TUI tests stub it.
var AnswerHeld = func(id, choice string, labels map[int][]string, other map[int]bool, notes map[int]string) (string, error) {
	r, err := controlCall(controlRequest{Op: "answer", Prompt: id, Choice: choice, Answers: labelAnswers(labels), Other: otherFlags(other), Notes: noteFields(notes)})
	return r.Text, err
}

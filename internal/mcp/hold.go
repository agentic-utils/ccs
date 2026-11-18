package mcp

// The holder: whoever owns serve-hook.sock (and serve-control.sock with it)
// holds the prompts `ccs hook permission` hands over until they're answered
// or let go to their tabs. Ownership is an flock on serve-hook.lock beside
// the sockets, held for as long as it owns them, so two owners can never
// bind at once and a crashed owner's lock goes with its process.
//
// `ccs serve --allow-write` always wins: finding the lock taken, it asks the
// owner to yield over the control socket; a TUI owner lets every held
// prompt go to its tab (no decision), closes both sockets and unlocks; a
// serve owner refuses. A TUI takes the lock only when it's free, and is
// otherwise a client of the owner, answering through the control socket.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// HoldConfig says how a holder holds.
type HoldConfig struct {
	Who        string               // the activity log's name for it: "serve" or "tui"
	Wait       func() time.Duration // how long a prompt is held before it goes to its tab
	KeyRelease func() bool          // let it go when a key is pressed on the Mac
	MayHold    func() bool          // nil: always; false lets a new prompt go at once
	Yields     bool                 // a TUI yields to ccs serve; ccs serve never does
	Now        func() time.Time
	OnYield    func() // after it has yielded to ccs serve
}

// Holder holds prompts for whoever answers them: ccs serve's page, or the
// ccs TUI.
type Holder struct {
	cfg  HoldConfig
	book *promptBook

	mu      sync.Mutex
	rel     chan struct{} // closed to let every held prompt go
	lock    *os.File      // the flock; nil when not owning
	stops   []func()
	closing bool
}

// NewHolder makes a holder; Listen makes it the owner.
func NewHolder(cfg HoldConfig) *Holder {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = func() time.Duration { return maxHookWait }
	}
	if cfg.KeyRelease == nil {
		cfg.KeyRelease = func() bool { return false }
	}
	return &Holder{cfg: cfg, book: &promptBook{}, rel: make(chan struct{})}
}

// ErrHeldElsewhere: another holder owns the sockets (and wouldn't yield).
var ErrHeldElsewhere = errors.New("another ccs already holds prompts")

func holdLockPath() string { return filepath.Join(filepath.Dir(hookSockPath()), "serve-hook.lock") }

// tryLock takes the ownership lock without waiting.
func tryLock() (*os.File, error) {
	path := holdLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrHeldElsewhere
	}
	return f, nil
}

// Listen makes h the owner: it takes the lock (with takeOver, asking a TUI
// owner to yield first and waiting up to 3s for it) and opens both
// sockets. ErrHeldElsewhere when another owns them.
func (h *Holder) Listen(takeOver bool) error {
	lock, err := tryLock()
	if errors.Is(err, ErrHeldElsewhere) && takeOver {
		if _, yerr := controlCall(controlRequest{Op: "yield"}); yerr != nil {
			return fmt.Errorf("%w: %v", ErrHeldElsewhere, yerr)
		}
		for deadline := time.Now().Add(3 * time.Second); errors.Is(err, ErrHeldElsewhere) && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
			lock, err = tryLock()
		}
	}
	if err != nil {
		return err
	}
	// Owning the lock, any socket files left are a crashed owner's.
	stopHook, err := listenUnix(hookSockPath(), h.holdPrompt)
	if err != nil {
		lock.Close()
		return err
	}
	stopControl, err := listenUnix(ControlSockPath(), h.serveControl)
	if err != nil {
		stopHook()
		lock.Close()
		return err
	}
	h.mu.Lock()
	h.lock, h.stops, h.closing = lock, []func(){stopControl, stopHook}, false
	h.mu.Unlock()
	state.LogActivity("%s: holding prompts from ccs hook permission", h.cfg.Who)
	return nil
}

// Owns says whether h owns the sockets now.
func (h *Holder) Owns() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lock != nil
}

func listenUnix(path string, handle func(net.Conn)) (func(), error) {
	os.Remove(path)
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
			go handle(c)
		}
	}()
	return func() { ln.Close(); os.Remove(path) }, nil
}

// ReleaseAll lets every held prompt go to its tab, with no decision.
func (h *Holder) ReleaseAll(why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.book.list("")) > 0 {
		state.LogActivity("%s: letting every held prompt go to its tab: %s", h.cfg.Who, why)
	}
	close(h.rel)
	h.rel = make(chan struct{})
}

// Close lets every held prompt go, closes both sockets and unlocks.
func (h *Holder) Close(why string) {
	h.ReleaseAll(why)
	h.mu.Lock()
	stops, lock := h.stops, h.lock
	h.stops, h.lock, h.closing = nil, nil, true
	h.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
	if lock != nil {
		lock.Close() // drops the flock
	}
}

func (h *Holder) release() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rel
}

// holdPrompt keeps one hook's prompt until it's answered or let go.
func (h *Holder) holdPrompt(c net.Conn) {
	defer c.Close()
	// Only this user's own hooks: the socket's 0600 should already see to it.
	if uid, err := peerUID(c); err != nil || uid != uint32(os.Getuid()) {
		state.LogActivity("%s: refused a hook connection from uid %d (%v)", h.cfg.Who, uid, err)
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
	letGo := func(why string) {
		state.LogActivity("%s: let %s's %s prompt go to its tab: %s", h.cfg.Who, shortID(req.SessionID), req.ToolName, why)
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		json.NewEncoder(c).Encode(hookReply{Nonce: hello.Nonce})
	}
	if h.cfg.MayHold != nil && !h.cfg.MayHold() {
		letGo("not holding now")
		return
	}
	rel := h.release()
	p := &heldPrompt{id: newPromptID(), nonce: hello.Nonce, req: req, at: h.cfg.Now(), reply: make(chan hookReply, 1)}
	h.book.add(p)
	defer h.book.take(p.id)
	state.LogActivity("%s: holding %s's %s prompt %s", h.cfg.Who, shortID(req.SessionID), req.ToolName, p.id)

	gone := make(chan struct{}) // the hook went away: Claude Code killed it, or the session ended
	go func() {
		buf := make([]byte, 1)
		c.Read(buf)
		close(gone)
	}()
	var back <-chan time.Time
	if h.cfg.KeyRelease() {
		t := time.NewTicker(keyPoll)
		defer t.Stop()
		back = t.C
	}
	wait := h.cfg.Wait()
	limit := time.NewTimer(wait)
	defer limit.Stop()
	for {
		select {
		case r := <-p.reply:
			r.Nonce = p.nonce
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := json.NewEncoder(c).Encode(r); err != nil {
				state.LogActivity("%s: answering prompt %s failed: %v", h.cfg.Who, p.id, err)
			}
			return
		case <-gone:
			state.LogActivity("%s: %s's prompt %s ended before an answer", h.cfg.Who, shortID(req.SessionID), p.id)
			return
		case <-rel:
			// Answered meanwhile? The answer wins over letting it go.
			select {
			case r := <-p.reply:
				r.Nonce = p.nonce
				json.NewEncoder(c).Encode(r)
			default:
				letGo("released")
			}
			return
		case <-limit.C:
			letGo(fmt.Sprintf("no answer within %s", wait))
			return
		case <-back:
			if idle, ok := keyIdle(); ok && idle < h.cfg.Now().Sub(p.at) {
				letGo("a key was pressed on the Mac")
				return
			}
		}
	}
}

// serveControl answers one control request: list, answer or yield.
func (h *Holder) serveControl(c net.Conn) {
	defer c.Close()
	if uid, err := peerUID(c); err != nil || uid != uint32(os.Getuid()) {
		state.LogActivity("%s: refused a control connection from uid %d (%v)", h.cfg.Who, uid, err)
		return
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 4096), 64<<10)
	if !sc.Scan() {
		return
	}
	var req controlRequest
	var reply controlReply
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
		reply.Error = "bad request"
	} else if req.Op == "yield" {
		if !h.cfg.Yields {
			reply.Error = "ccs serve holds prompts here and doesn't yield"
		} else {
			reply.Text = "yielding"
		}
	} else {
		reply = h.control(req, tuiUser)
	}
	json.NewEncoder(c).Encode(reply)
	if req.Op == "yield" && h.cfg.Yields {
		h.Close("ccs serve took over holding prompts")
		if h.cfg.OnYield != nil {
			h.cfg.OnYield()
		}
	}
}

// control is a list or an answer, from the control socket or, for a TUI
// that owns the hold, called directly (Answer, List).
func (h *Holder) control(req controlRequest, who string) controlReply {
	switch req.Op {
	case "list":
		return controlReply{Prompts: heldList(h.book)}
	case "answer":
		gone := "that prompt isn't waiting any more (answered, shown in its tab, or ended): nothing was sent"
		p := h.book.get(req.Prompt)
		if p == nil {
			return controlReply{Error: gone}
		}
		reply, what, err := promptDecision(p, req.Choice, req.Answers, who)
		if err != nil {
			return controlReply{Error: err.Error()}
		}
		if p = h.book.take(req.Prompt); p == nil { // another answer got there first
			return controlReply{Error: gone}
		}
		p.reply <- reply
		state.LogActivity("%s: %s settled %s's %s prompt %s: %s", h.cfg.Who, who, shortID(p.req.SessionID), p.req.ToolName, p.id, req.Choice)
		return controlReply{Text: what}
	}
	return controlReply{Error: fmt.Sprintf("unknown op %q", req.Op)}
}

// List is the prompts h holds, for a TUI that owns the hold.
func (h *Holder) List() []HeldPrompt { return heldList(h.book) }

// Answer settles a prompt h holds, as the control socket's answer does.
func (h *Holder) Answer(id, choice string, labels map[int][]string) (string, error) {
	r := h.control(controlRequest{Op: "answer", Prompt: id, Choice: choice, Answers: labelAnswers(labels)}, tuiUser)
	if r.Error != "" {
		return "", errors.New(r.Error)
	}
	return r.Text, nil
}

func labelAnswers(labels map[int][]string) map[string][]string {
	answers := map[string][]string{}
	for i, ls := range labels {
		answers["q"+strconv.Itoa(i)] = ls
	}
	return answers
}

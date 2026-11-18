package mcp

// ccs serve: the sessions, their triage, transcripts and PRs as a small web
// page for a phone, with sending, answering, launching, closing and
// restarting behind --allow-write. It listens on loopback only and is meant
// to sit behind a Cloudflare Tunnel with a Cloudflare Access policy; it
// checks Access's signed token on every request itself (access.go), so it
// never trusts the network in front of it. Read docs/remote.md first.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// The hooks ccs serve acts through, stubbed in tests.
var (
	sendEnvelope    = sessions.SendEnvelope
	readSessionFile = sessions.ReadSessionFile
	deliverCodex    = sessions.DeliverCodex
	peerToken       = sessions.PeerToken
	serveCall       = Call
)

// remoteSender is the from-name on a message sent from ccs serve: the same
// as the TUI's, since the user typed it, so every reader (the preview,
// orchestrator_report) treats it as theirs. Where it came from is in the
// activity log.
const remoteSender = "the user, via ccs"

// maxRemoteMessage bounds a message's length (runes).
const maxRemoteMessage = 8000

type serveConfig struct {
	addr       string // loopback only
	host       string // the public hostname the tunnel serves
	allowWrite bool
	access     *accessVerifier
	load       mcpSource
}

type server struct {
	cfg     serveConfig
	csrfKey []byte
	limits  *rateLimits
	now     func() time.Time

	answerMu sync.Mutex      // one answer at a time
	answered map[string]bool // session id + question id: each question is answered once from here
}

func newServer(cfg serveConfig) *server {
	key := make([]byte, 32)
	rand.Read(key)
	return &server{cfg: cfg, csrfKey: key, limits: newRateLimits(), now: time.Now, answered: map[string]bool{}}
}

// RunServe is `ccs serve`.
func RunServe(args []string, stdout, stderr io.Writer, load func() ([]transcript.Conversation, error)) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:7467", "loopback address to listen on")
	host := fs.String("host", "", "the public hostname the tunnel serves, e.g. ccs.example.com (required)")
	team := fs.String("team", "", "Cloudflare Access team domain, e.g. me.cloudflareaccess.com (required)")
	aud := fs.String("aud", "", "the Access application's AUD tag (required)")
	emails := fs.String("email", "", "comma-separated emails allowed in (required)")
	write := fs.Bool("allow-write", false, "allow sending, answering, launching, closing and restarting (read-only without it)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs serve --host <hostname> --team <team> --aud <tag> --email <you@example.com> [--addr 127.0.0.1:7467] [--allow-write]")
		fmt.Fprintln(stderr, "Serves ccs's sessions to a browser through a Cloudflare Tunnel with Access in front. See docs/remote.md.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *host == "" || *team == "" || *aud == "" || *emails == "" {
		fmt.Fprintln(stderr, "ccs serve: --host, --team, --aud and --email are all required: it never runs without Cloudflare Access in front")
		return 2
	}
	if err := loopbackOnly(*addr); err != nil {
		fmt.Fprintln(stderr, "ccs serve:", err)
		return 2
	}
	s := newServer(serveConfig{
		addr: *addr, host: strings.ToLower(*host), allowWrite: *write,
		access: newAccessVerifier(*team, *aud, strings.Split(*emails, ",")),
		load:   load,
	})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(stderr, "ccs serve:", err)
		return 1
	}
	mode := "read-only"
	if *write {
		mode = "read and write"
	}
	fmt.Fprintf(stdout, "ccs serve: %s on http://%s for https://%s (Access team %s)\n", mode, ln.Addr(), s.cfg.host, s.cfg.access.team)
	state.LogActivity("serve: started, %s, on %s for %s", mode, ln.Addr(), s.cfg.host)
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(stderr, "ccs serve:", err)
		return 1
	}
	return 0
}

// loopbackOnly refuses any address but a loopback one: the tunnel connects
// from this Mac, and nothing else should reach the port.
func loopbackOnly(addr string) error {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad --addr %q: %v", addr, err)
	}
	if h == "localhost" {
		return nil
	}
	if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--addr must be a loopback address (127.0.0.1 or ::1), not %q", h)
	}
	return nil
}

type ctxKey struct{}

// handler wraps every route in the checks, in order: the Host, Access's
// token, the rate limit, then for a POST its origin and CSRF token. Each
// request is logged with who made it and what came of it.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.triagePage)
	mux.HandleFunc("GET /session", s.sessionPage)
	mux.HandleFunc("GET /prs", s.textPage("PRs", "list_prs", nil))
	mux.HandleFunc("GET /launch", s.launchPage)
	mux.HandleFunc("GET /api/triage", s.triageJSON)
	mux.HandleFunc("GET /api/session", s.sessionJSON)
	mux.HandleFunc("GET /api/prs", s.textJSON("list_prs"))
	mux.HandleFunc("POST /act", s.act)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h := rec.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(rec, r.Body, 64<<10)
		email, err := s.check(r)
		if err != nil {
			http.Error(rec, err.msg, err.code)
			state.LogActivity("serve: refused %s %s from %s: %d %s (%s)", r.Method, r.URL.Path, orDash(email), err.code, err.msg, err.detail)
			return
		}
		mux.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), ctxKey{}, email)))
		// Every request, a write's own line (what it did) besides.
		state.LogActivity("serve: %s %s %s %d", email, r.Method, r.URL.RequestURI(), rec.status)
	})
}

type checkErr struct {
	code        int
	msg, detail string
}

func (s *server) check(r *http.Request) (string, *checkErr) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != s.cfg.host {
		return "", &checkErr{http.StatusMisdirectedRequest, "wrong host", "host " + r.Host}
	}
	token := r.Header.Get("Cf-Access-Jwt-Assertion")
	if token == "" {
		return "", &checkErr{http.StatusForbidden, "forbidden: no Cloudflare Access token", "no token"}
	}
	email, err := s.cfg.access.verify(token)
	if err != nil {
		return "", &checkErr{http.StatusForbidden, "forbidden", err.Error()}
	}
	if r.Method == http.MethodPost {
		if r.Header.Get("Origin") != "https://"+s.cfg.host {
			return email, &checkErr{http.StatusForbidden, "forbidden: cross-site request", "origin " + r.Header.Get("Origin")}
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			return email, &checkErr{http.StatusForbidden, "forbidden: cross-site request", "sec-fetch-site " + site}
		}
	}
	// Only a confirmed action spends the write allowance; a confirm page
	// costs a read.
	write := r.Method == http.MethodPost && r.PostFormValue("confirm") == "yes"
	if !s.limits.allow(email, write, s.now()) {
		return email, &checkErr{http.StatusTooManyRequests, "too many requests, slow down", "rate limit"}
	}
	return email, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func emailOf(r *http.Request) string {
	e, _ := r.Context().Value(ctxKey{}).(string)
	return e
}

// csrf is the form token for this email, valid until ccs serve restarts.
func (s *server) csrf(email string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(email))
	return hex.EncodeToString(m.Sum(nil))
}

// Rate limits per email: reads in a burst of 60 then one a second, writes
// in a burst of 5 then one every 12 seconds.
type bucket struct {
	tokens float64
	last   time.Time
}

type rateLimits struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

func newRateLimits() *rateLimits { return &rateLimits{buckets: map[string]*bucket{}} }

func (l *rateLimits) allow(email string, write bool, now time.Time) bool {
	size, per := 60.0, time.Second
	key := "r:" + email
	if write {
		size, per, key = 5, 12*time.Second, "w:"+email
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: size, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(size, b.tokens+now.Sub(b.last).Seconds()/per.Seconds())
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Pages.

type pageData struct {
	Title, CSRF string
	Write       bool
	Body        any
}

func (s *server) render(w http.ResponseWriter, r *http.Request, name string, body any) {
	s.renderCode(w, r, http.StatusOK, name, body)
}

func (s *server) renderCode(w http.ResponseWriter, r *http.Request, code int, name string, body any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := pages.ExecuteTemplate(w, name, pageData{Title: name, CSRF: s.csrf(emailOf(r)), Write: s.cfg.allowWrite, Body: body}); err != nil {
		state.LogActivity("serve: rendering %s: %v", name, err)
	}
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.renderCode(w, r, code, "message", map[string]string{"Heading": "Couldn't do that", "Text": msg})
}

func (s *server) triage() ([]TriageRow, error) {
	convs, err := s.cfg.load()
	if err != nil {
		return nil, err
	}
	return TriageRows(convs, liveSessions(), s.now(), DefaultStallAfter), nil
}

func (s *server) triagePage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.triage()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "couldn't read sessions: "+err.Error())
		return
	}
	s.render(w, r, "triage", rows)
}

func (s *server) triageJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.triage()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rows)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// sessionView is one session's page: its messages as read_session gives
// them, and, while it runs, its question and the forms that act on it.
type sessionView struct {
	ID, Name, Text, Status string
	Offset, Prev, Next     int
	HasNext, Live          bool
	Ask                    *askView
}

type askView struct {
	ID, Question string
	Options      []string
	Answerable   bool // one single-choice question: a digit in its terminal picks it
}

func (s *server) session(r *http.Request) (sessionView, int, error) {
	id := r.URL.Query().Get("id")
	if id == "" {
		return sessionView{}, http.StatusBadRequest, errors.New("which session? (id)")
	}
	offset := -40
	if o := r.URL.Query().Get("offset"); o != "" {
		n, err := strconv.Atoi(o)
		if err != nil {
			return sessionView{}, http.StatusBadRequest, errors.New("bad offset")
		}
		offset = n
	}
	args, _ := json.Marshal(map[string]any{"session_id": id, "offset": offset, "limit": 40})
	text, err := serveCall("read_session", args, s.cfg.load)
	if err != nil {
		return sessionView{}, http.StatusNotFound, err
	}
	// A negative offset counts from the end, so -40 is the latest page and
	// nothing is newer than it.
	v := sessionView{ID: id, Text: text, Offset: offset, Prev: offset - 40, Next: offset + 40, HasNext: offset >= 0 || offset+40 < 0}
	if offset >= 0 {
		v.Prev = max(0, offset-40)
	}
	if rows, err := s.triage(); err == nil {
		for _, row := range rows {
			if row.Session == id {
				v.Name, v.Status, v.Live = row.Name, row.Status, row.PID > 0
			}
		}
	}
	if v.Live {
		if conv, ok := s.conv(id); ok {
			if qs := conv.Questions(); len(qs) > 0 && conv.Pending != nil {
				a := &askView{ID: conv.Pending.ID, Question: transcript.CleanText(qs[0].Question), Answerable: len(qs) == 1 && !qs[0].MultiSelect}
				for i, o := range qs[0].Options {
					if i < 9 {
						a.Options = append(a.Options, transcript.CleanText(o.Label))
					}
				}
				if len(qs) > 1 {
					a.Question = conv.PendingAsk()
				}
				v.Ask = a
			}
		}
	}
	return v, http.StatusOK, nil
}

func (s *server) conv(id string) (transcript.Conversation, bool) {
	convs, err := s.cfg.load()
	if err != nil {
		return transcript.Conversation{}, false
	}
	for _, c := range convs {
		if c.SessionID == id {
			return c, true
		}
	}
	return transcript.Conversation{}, false
}

func (s *server) sessionPage(w http.ResponseWriter, r *http.Request) {
	v, code, err := s.session(r)
	if err != nil {
		s.fail(w, r, code, err.Error())
		return
	}
	s.render(w, r, "session", v)
}

func (s *server) sessionJSON(w http.ResponseWriter, r *http.Request) {
	v, code, err := s.session(r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, v)
}

func (s *server) textPage(title, tool string, args json.RawMessage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		text, err := serveCall(tool, args, s.cfg.load)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		s.render(w, r, "text", map[string]string{"Heading": title, "Text": text})
	}
}

func (s *server) textJSON(tool string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		text, err := serveCall(tool, nil, s.cfg.load)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"text": text})
	}
}

func (s *server) launchPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.allowWrite {
		s.fail(w, r, http.StatusForbidden, "ccs serve is read-only: start it with --allow-write to launch sessions")
		return
	}
	s.render(w, r, "launch", nil)
}

// Writes. Every one is a POST to /act with the CSRF token. Without
// confirm=yes it only shows what it would do, with a button that sends the
// same form confirmed; with it, it acts, logs who did what, and says how it
// went.

type action struct {
	Kind, ID, Name, Text, Ask, Option, Cwd, Brief string
	Codex, Graceful                               bool
}

func formAction(r *http.Request) action {
	f := r.PostFormValue
	return action{
		Kind: f("action"), ID: f("id"), Name: f("name"), Text: f("text"), Ask: f("ask"),
		Option: f("option"), Cwd: f("cwd"), Brief: f("brief"),
		Codex: f("codex") == "on", Graceful: f("graceful") == "on",
	}
}

// describe says in plain words what the action will do.
func (a action) describe() (string, error) {
	switch a.Kind {
	case "send":
		return fmt.Sprintf("Send this message to %s, as you:\n\n%s", a.Name, a.Text), nil
	case "answer":
		return fmt.Sprintf("Answer %s's question with option %s, by pressing %s in its terminal.", a.Name, a.Option, a.Option), nil
	case "launch":
		kind := "Claude Code"
		if a.Codex {
			kind = "Codex"
		}
		return fmt.Sprintf("Launch a new %s session named %s in %s, in a new tab on the Mac, with this brief:\n\n%s", kind, a.Name, a.Cwd, a.Brief), nil
	case "close":
		if a.Graceful {
			return fmt.Sprintf("Ask %s to wrap up, then stop it and close its tab.", a.Name), nil
		}
		return fmt.Sprintf("Stop %s now and close its tab.", a.Name), nil
	case "restart":
		return fmt.Sprintf("Stop %s and resume the same conversation in a new tab.", a.Name), nil
	}
	return "", fmt.Errorf("unknown action %q", a.Kind)
}

func (s *server) act(w http.ResponseWriter, r *http.Request) {
	email := emailOf(r)
	if !s.cfg.allowWrite {
		state.LogActivity("serve: %s refused %s: read-only", email, r.PostFormValue("action"))
		s.fail(w, r, http.StatusForbidden, "ccs serve is read-only: start it with --allow-write to act on sessions")
		return
	}
	if !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(s.csrf(email))) {
		state.LogActivity("serve: %s refused %s: bad form token", email, r.PostFormValue("action"))
		s.fail(w, r, http.StatusForbidden, "the form is out of date (ccs serve restarted?): reload the page and try again")
		return
	}
	a := formAction(r)
	what, err := a.describe()
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if r.PostFormValue("confirm") != "yes" {
		s.render(w, r, "confirm", map[string]any{"What": what, "A": a})
		return
	}
	text, err := s.do(a)
	state.LogActivity("serve: %s %s %s: %s", email, a.Kind, orDash(a.Name), logResult(text, err))
	if err != nil {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	s.render(w, r, "message", map[string]string{"Heading": "Done", "Text": text, "Back": a.ID})
}

func logResult(text string, err error) string {
	if err != nil {
		return "failed: " + err.Error()
	}
	return text
}

func (s *server) do(a action) (string, error) {
	if a.Name == "" {
		return "", errors.New("which session? (name)")
	}
	switch a.Kind {
	case "send":
		return s.send(a)
	case "answer":
		return s.answer(a)
	case "launch":
		args, _ := json.Marshal(map[string]any{"name": a.Name, "cwd": a.Cwd, "brief": a.Brief, "codex": a.Codex})
		return serveCall("launch_session", args, s.cfg.load)
	case "close":
		if err := s.sameSession(a); err != nil {
			return "", err
		}
		args, _ := json.Marshal(map[string]any{"name": a.Name, "graceful": a.Graceful})
		return serveCall("close_session", args, s.cfg.load)
	case "restart":
		if err := s.sameSession(a); err != nil {
			return "", err
		}
		args, _ := json.Marshal(map[string]any{"name": a.Name})
		return serveCall("restart_session", args, s.cfg.load)
	}
	return "", fmt.Errorf("unknown action %q", a.Kind)
}

// sameSession refuses a close or restart unless the name, which the tools
// act on, is still that of exactly one running session, the one the page
// showed: another session may have taken the name since.
func (s *server) sameSession(a action) error {
	rows, err := s.triage()
	if err != nil {
		return err
	}
	var ids []string
	for _, r := range rows {
		if r.Name == a.Name && r.PID > 0 {
			ids = append(ids, r.Session)
		}
	}
	if len(ids) != 1 || ids[0] != a.ID {
		return fmt.Errorf("%s isn't the session that page showed any more: reload it", a.Name)
	}
	return nil
}

// liveByID is the running session with this id.
func liveByID(id string) (sessions.LiveSession, bool) {
	ls, ok := liveSessions()[id]
	return ls, ok && ls.PID > 0
}

// send delivers a message over the session's own message socket (Codex: its
// queue). It never types into a terminal: from afar nobody can see what's
// on the screen it would land in.
func (s *server) send(a action) (string, error) {
	text := strings.TrimSpace(a.Text)
	switch {
	case text == "":
		return "", errors.New("the message is empty")
	case len([]rune(text)) > maxRemoteMessage:
		return "", fmt.Errorf("the message is over %d characters", maxRemoteMessage)
	case strings.ContainsFunc(text, func(r rune) bool { return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f }):
		return "", errors.New("the message contains control characters")
	}
	ls, ok := liveByID(a.ID)
	if !ok {
		return "", fmt.Errorf("%s isn't running", a.Name)
	}
	if ls.Codex {
		return deliverCodex(a.ID, a.Name, text)
	}
	f, err := readSessionFile(ls.PID)
	if err != nil || f.SessionID != a.ID {
		return "", fmt.Errorf("%s's session has changed: nothing was sent", a.Name)
	}
	if f.Socket == "" {
		return "", fmt.Errorf("%s has no message socket, and ccs serve never types into terminals: nothing was sent", a.Name)
	}
	if _, err := sendEnvelope(f.Socket, peerToken(ls.PID, f.Socket), remoteSender, transcript.CcsNote, text, false); err != nil {
		if errors.Is(err, sessions.ErrNotConnected) {
			return "", fmt.Errorf("couldn't reach %s's message socket: nothing was sent", a.Name)
		}
		return "", fmt.Errorf("sending to %s may have failed: %v", a.Name, err)
	}
	note := "Sent to " + a.Name + " via its message socket. It shows in the conversation once the session takes it."
	if f.Status == "waiting" {
		note += " It's on a menu, so the message waits until that's answered."
	}
	return note, nil
}

// answer picks an option of a waiting session's question by pressing its
// digit in the terminal, as the TUI's overlay does, only while it's still
// waiting on the same question the page showed, and only for one
// single-choice question (a permission prompt or a multi-part menu is never
// answered from afar).
func (s *server) answer(a action) (string, error) {
	n, err := strconv.Atoi(a.Option)
	if err != nil || n < 1 || n > 9 {
		return "", errors.New("pick an option from 1 to 9")
	}
	s.answerMu.Lock()
	defer s.answerMu.Unlock()
	once := a.ID + " " + a.Ask
	if s.answered[once] {
		return "", fmt.Errorf("that question of %s's was already answered from here", a.Name)
	}
	ls, ok := liveByID(a.ID)
	if !ok || ls.Codex {
		return "", fmt.Errorf("%s isn't a running Claude Code session", a.Name)
	}
	if ls.Unverified {
		return "", fmt.Errorf("%s's process couldn't be checked against its session file, so no keys were pressed", a.Name)
	}
	conv, ok := s.conv(a.ID)
	if !ok || conv.Pending == nil || conv.Pending.ID != a.Ask {
		return "", fmt.Errorf("%s isn't asking that question any more: reload its page", a.Name)
	}
	qs := conv.Questions()
	if len(qs) != 1 || qs[0].MultiSelect || n > len(qs[0].Options) {
		return "", fmt.Errorf("%s's question can only be answered in its tab", a.Name)
	}
	f, err := readSessionFile(ls.PID)
	if err != nil || f.SessionID != a.ID || f.Status != "waiting" {
		return "", fmt.Errorf("%s isn't waiting on its menu now: no keys were pressed", a.Name)
	}
	s.answered[once] = true // before the keys: a second press must never reach whatever comes next
	where, err := sessions.TypeKeys(ls.PID, strconv.Itoa(n), false)
	switch {
	case where == "":
		return "", fmt.Errorf("couldn't find %s's terminal (tmux or iTerm): no keys were pressed", a.Name)
	case err != nil:
		return "", fmt.Errorf("pressing %d in %s's %s may have failed: %v", n, a.Name, where, err)
	}
	return fmt.Sprintf("Pressed %d in %s's %s: %s", n, a.Name, where, qs[0].Options[n-1].Label), nil
}

// ServeMain is `ccs serve` from the command line.
func ServeMain(args []string, load func() ([]transcript.Conversation, error)) {
	os.Exit(RunServe(args, os.Stdout, os.Stderr, load))
}

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}).Parse(pageTemplates))

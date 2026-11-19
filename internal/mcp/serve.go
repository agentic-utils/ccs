package mcp

// ccs serve: the sessions, their triage, transcripts and PRs as a small web
// page for a phone, with sending, launching, closing and
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/proc"
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
	hookWait   time.Duration // how long a prompt from `ccs hook permission` is held for the page
	preview    bool          // --preview: this Mac's browser only, no Access; writes only with --allow-write, same-origin
	token      string        // --preview: the secret in its link's path, /p/<token>/, on every page and form
	access     *accessVerifier
	load       mcpSource
}

type server struct {
	cfg     serveConfig
	csrfKey []byte
	limits  *rateLimits
	prompts *promptBook // the holder's: prompts `ccs hook permission` hands over, held for the page
	hold    *Holder
	now     func() time.Time

	loadMu   sync.Mutex
	inflight *loadCall     // the scan running now, which concurrent requests share
	slots    chan struct{} // requests being served at once
}

// serveSlots is how many requests are served at once; serveWait how long
// one waits for a slot before a 503.
const (
	serveSlots = 4
	serveWait  = 15 * time.Second
)

type loadCall struct {
	done  chan struct{}
	convs []transcript.Conversation
	err   error
}

// load is the sessions, from one scan shared by every request that asks
// while it runs, so a burst of requests never parses the same transcripts
// side by side.
//
// The scan runs on its own (the transcript parser takes no context), and
// every request waiting for it, the one that started it included, stops
// waiting when its client goes away, freeing its slot; the scan finishes
// for whoever asks next.
func (s *server) load(ctx context.Context) ([]transcript.Conversation, error) {
	s.loadMu.Lock()
	c := s.inflight
	if c == nil {
		c = &loadCall{done: make(chan struct{})}
		s.inflight = c
		go func() {
			c.convs, c.err = s.cfg.load()
			s.loadMu.Lock()
			s.inflight = nil
			s.loadMu.Unlock()
			close(c.done)
		}()
	}
	s.loadMu.Unlock()
	select {
	case <-c.done:
		return c.convs, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// loader is load for a tool call (an mcpSource), bound to ctx.
func (s *server) loader(ctx context.Context) mcpSource {
	return func() ([]transcript.Conversation, error) { return s.load(ctx) }
}

func newServer(cfg serveConfig) *server {
	key := make([]byte, 32)
	rand.Read(key)
	s := &server{cfg: cfg, csrfKey: key, limits: newRateLimits(), now: time.Now, slots: make(chan struct{}, serveSlots)}
	s.hold = NewHolder(HoldConfig{
		Who:        "serve",
		Wait:       func() time.Duration { return s.cfg.hookWait },
		KeyRelease: func() bool { return !s.cfg.preview }, // in a preview the user is at the Mac by design
		Now:        func() time.Time { return s.now() },
	})
	s.prompts = s.hold.book
	return s
}

// defaultServeAddr is where ccs serve listens unless --addr says otherwise.
const defaultServeAddr = "127.0.0.1:7467"

// serveFlags are `ccs serve`'s flags, checked: what RunServe runs with and
// what `ccs serve install` writes into its launchd agent.
type serveFlags struct {
	addr, host, team, aud, emails string
	write, preview                bool
	hookWait                      time.Duration
}

// parseServeFlags parses and checks `ccs serve`'s flags, refusing what
// RunServe would never run with; code is 0 when they're good, else the
// exit code (the reason is on stderr).
func parseServeFlags(args []string, stderr io.Writer) (f serveFlags, code int) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.addr, "addr", defaultServeAddr, "loopback address to listen on")
	fs.StringVar(&f.host, "host", "", "the public hostname the tunnel serves, e.g. ccs.example.com (required)")
	fs.StringVar(&f.team, "team", "", "Cloudflare Access team domain, e.g. me.cloudflareaccess.com (required)")
	fs.StringVar(&f.aud, "aud", "", "the Access application's AUD tag (required)")
	fs.StringVar(&f.emails, "email", "", "comma-separated emails allowed in (required)")
	fs.BoolVar(&f.write, "allow-write", false, "allow sending, launching, closing and restarting (read-only without it)")
	fs.DurationVar(&f.hookWait, "hook-wait", maxHookWait, "with --allow-write: how long a prompt from `ccs hook permission` waits for the page before it shows in its tab (at most 30m)")
	fs.BoolVar(&f.preview, "preview", false, "a preview for this Mac's browser at http://127.0.0.1:<port>, with no Cloudflare Access (read-only unless --allow-write)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs serve --host <hostname> --team <team> --aud <tag> --email <you@example.com> [--addr 127.0.0.1:7467] [--allow-write]")
		fmt.Fprintln(stderr, "       ccs serve --preview [--allow-write] [--addr 127.0.0.1:7467]")
		fmt.Fprintln(stderr, "       ccs serve install <the same flags> | uninstall | start | stop | status   (a launchd agent)")
		fmt.Fprintln(stderr, "       ccs serve qr   the installed agent's address as a QR code, for a phone's camera")
		fmt.Fprintln(stderr, "Serves ccs's sessions to a browser through a Cloudflare Tunnel with Access in front. See docs/remote.md.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return f, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "ccs serve: unexpected %q\n", fs.Arg(0))
		return f, 2
	}
	if f.preview {
		// This Mac only, with no Access: the Access flags would suggest
		// otherwise, so they're refused.
		if f.host != "" || f.team != "" || f.aud != "" || f.emails != "" {
			fmt.Fprintln(stderr, "ccs serve: --preview is for this Mac only, with no Cloudflare Access: it takes no --host, --team, --aud or --email")
			return f, 2
		}
	} else if f.host == "" || f.team == "" || f.aud == "" || f.emails == "" {
		fmt.Fprintln(stderr, "ccs serve: --host, --team, --aud and --email are all required: it never runs without Cloudflare Access in front (or try --preview, on this Mac only)")
		return f, 2
	}
	if err := loopbackOnly(f.addr); err != nil {
		fmt.Fprintln(stderr, "ccs serve:", err)
		return f, 2
	}
	if f.hookWait <= 0 || f.hookWait > maxHookWait {
		fmt.Fprintln(stderr, "ccs serve: --hook-wait must be more than 0 and at most", maxHookWait)
		return f, 2
	}
	if !f.preview {
		if err := accessFlagsOK(f); err != nil {
			fmt.Fprintln(stderr, "ccs serve:", err)
			return f, 2
		}
	}
	return f, 0
}

// accessFlagsOK refuses Access values that can't be right: a hostname with
// a scheme, path, port or space; a team or AUD tag with a space or slash;
// an email list with an entry that isn't an address.
func accessFlagsOK(f serveFlags) error {
	if strings.ContainsAny(f.host, " /:\t") || strings.HasPrefix(f.host, ".") || strings.HasSuffix(f.host, ".") {
		return fmt.Errorf("--host is the bare hostname the tunnel serves, such as ccs.example.com, not %q", f.host)
	}
	if strings.ContainsAny(f.team, " /:\t") {
		return fmt.Errorf("--team is your Access team, such as me or me.cloudflareaccess.com, not %q", f.team)
	}
	if strings.ContainsAny(f.aud, " /\t") {
		return fmt.Errorf("--aud is the Access application's AUD tag, a string of letters and digits, not %q", f.aud)
	}
	for _, e := range strings.Split(f.emails, ",") {
		if e = strings.TrimSpace(e); e != "" && (!strings.Contains(e, "@") || strings.ContainsAny(e, " /")) {
			return fmt.Errorf("--email takes email addresses, separated by commas: %q isn't one", e)
		}
	}
	return nil
}

// RunServe is `ccs serve`, and `ccs serve install|uninstall|start|stop|status`
// for its launchd agent (service.go).
func RunServe(args []string, stdout, stderr io.Writer, load func() ([]transcript.Conversation, error)) int {
	if len(args) > 0 && isServiceCommand(args[0]) {
		return RunService(args, stdout, stderr)
	}
	f, code := parseServeFlags(args, stderr)
	if code != 0 {
		return code
	}
	addr, host, team, aud, emails, write, preview := &f.addr, &f.host, &f.team, &f.aud, &f.emails, &f.write, &f.preview
	cfg := serveConfig{addr: *addr, host: strings.ToLower(*host), allowWrite: *write, hookWait: f.hookWait, load: load}
	if *preview {
		cfg.preview = true
		cfg.token = newToken()
	} else {
		cfg.access = newAccessVerifier(*team, *aud, strings.Split(*emails, ","))
	}
	s := newServer(cfg)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(stderr, "ccs serve:", err)
		return 1
	}
	if link := s.announce(stdout, ln.Addr().String()); link != "" {
		go openPreview(link)
	}
	// Prompts are only held for a page that can answer them.
	if *write {
		stop, err := s.listenHooks()
		if err != nil {
			ln.Close()
			fmt.Fprintln(stderr, "ccs serve:", err)
			return 1
		}
		defer stop()
		stopControl, err := s.listenControl()
		if err != nil {
			ln.Close()
			fmt.Fprintln(stderr, "ccs serve:", err)
			return 1
		}
		defer stopControl()
		fmt.Fprintf(stdout, "ccs serve: holding prompts from `ccs hook permission` for up to %s each\n", f.hookWait)
	}
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

// announce says where ccs serve is: a preview's secret link (returned, to
// open in the browser), never a QR code, since 127.0.0.1 on a phone is the
// phone; the remote mode's address with a QR code of https://<host>/ for a
// phone's camera.
func (s *server) announce(w io.Writer, addr string) (link string) {
	mode := "read-only"
	if s.cfg.allowWrite {
		mode = "read and write"
	}
	if s.cfg.preview {
		link = fmt.Sprintf("http://%s%s/", addr, s.base())
		fmt.Fprintf(w, "ccs serve: %s preview, this Mac only (^C stops it). Open it with this link, which only you have:\n  %s\n", mode, link)
		state.LogActivity("serve: started, %s preview, on %s", mode, addr)
		return link
	}
	fmt.Fprintf(w, "ccs serve: %s on http://%s for https://%s (Access team %s)\n", mode, addr, s.cfg.host, s.cfg.access.team)
	printQR(w, "https://"+s.cfg.host+"/")
	state.LogActivity("serve: started, %s, on %s for %s", mode, addr, s.cfg.host)
	return ""
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

// newToken is a random secret for the preview's link.
func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// openPreview opens the preview's link in the browser; stubbed in tests.
var openPreview = func(link string) { proc.RunBounded(10*time.Second, nil, "open", link) }

// previewUser is who the activity log names for a --preview request.
const previewUser = "preview (this Mac)"

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
	mux.HandleFunc("GET /manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /icon.svg", s.iconSVG)
	mux.HandleFunc("GET /icon/{size}", s.iconPNG)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK, base: s.base()}
		h := rec.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; manifest-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: under no-referrer a browser sends
		// "Origin: null" on the page's own form posts, which the origin
		// check below refuses. Other sites still get no referrer.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(rec, r.Body, 64<<10)
		if s.cfg.preview && !s.previewPath(r) {
			// Never the path itself: it may hold a mistyped token.
			http.NotFound(rec, r)
			state.LogActivity("serve: refused a preview request without its link's token (%s)", r.Method)
			return
		}
		email, err := s.check(r)
		if err != nil {
			http.Error(rec, err.msg, err.code)
			state.LogActivity("serve: refused %s %s from %s: %d %s (%s)", r.Method, r.URL.Path, orDash(email), err.code, err.msg, err.detail)
			return
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-r.Context().Done():
			return
		case <-time.After(serveWait):
			http.Error(rec, "busy, try again", http.StatusServiceUnavailable)
			state.LogActivity("serve: %s %s %s 503 busy", email, r.Method, r.URL.RequestURI())
			return
		}
		mux.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), ctxKey{}, email)))
		// Every request, a write's own line (what it did) besides.
		state.LogActivity("serve: %s %s %s %d", email, r.Method, r.URL.RequestURI(), rec.status)
	})
}

// base is the path every page and form lives under: "" in the remote
// mode, /p/<token> in a preview.
func (s *server) base() string {
	if s.cfg.preview {
		return "/p/" + s.cfg.token
	}
	return ""
}

// previewPath strips a preview's /p/<token> from the path, compared in
// constant time, and says whether it was there. Loopback is shared by every
// user on this Mac, so this is the preview's login. It lives in the path,
// not a cookie: browsers send a 127.0.0.1 cookie to every port, so another
// user's service on another port would get it, while the path stays with
// this port (the pages' Referrer-Policy is same-origin).
func (s *server) previewPath(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/p/")
	if !ok {
		return false
	}
	tok, after, _ := strings.Cut(rest, "/")
	if !hmac.Equal([]byte(tok), []byte(s.cfg.token)) {
		return false
	}
	r.URL.Path = "/" + after
	r.URL.RawPath = ""
	return true
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
	if s.cfg.preview {
		// This Mac's browser only: the Host check stops DNS rebinding, and
		// with no writes nothing here acts on a session.
		if host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "[::1]" {
			return "", &checkErr{http.StatusMisdirectedRequest, "wrong host", "host " + r.Host}
		}
		if r.Method == http.MethodPost {
			// The same as the remote mode's: only this page's own forms,
			// never another site's (the CSRF token is checked in act).
			if r.Header.Get("Origin") != "http://"+r.Host {
				return previewUser, &checkErr{http.StatusForbidden, "forbidden: cross-site request", "origin " + r.Header.Get("Origin")}
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
				return previewUser, &checkErr{http.StatusForbidden, "forbidden: cross-site request", "sec-fetch-site " + site}
			}
		} else if r.Method != http.MethodGet {
			return previewUser, &checkErr{http.StatusMethodNotAllowed, "method not allowed", "preview " + r.Method}
		}
		write := r.Method == http.MethodPost && r.PostFormValue("confirm") == "yes"
		if !s.limits.allow(previewUser, write, s.now()) {
			return previewUser, &checkErr{http.StatusTooManyRequests, "too many requests, slow down", "rate limit"}
		}
		return previewUser, nil
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
	base   string // a preview's /p/<token>, put back on a redirect's Location
}

// WriteHeader records the status, and in a preview puts the secret path
// back in front of a redirect to a path on this site (the mux's own
// redirects, and http.Redirect, see the path with the prefix stripped), so
// a redirect never lands on a 404.
func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	if loc := w.Header().Get("Location"); w.base != "" && strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") && !strings.HasPrefix(loc, w.base+"/") {
		w.Header().Set("Location", w.base+loc)
	}
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
	Preview     bool
	Base        string // what every link and form starts with (base)
	Waiting     int    // sessions waiting on the user, for the bar's badge (-1 unknown)
	Updated     string // when the page was drawn, hh:mm
	Refresh     bool   // the page reloads itself (refreshes)
	Body        any
}

func (s *server) render(w http.ResponseWriter, r *http.Request, name string, body any) {
	s.renderCode(w, r, http.StatusOK, name, body)
}

func (s *server) renderCode(w http.ResponseWriter, r *http.Request, code int, name string, body any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	waiting := -1
	if rows, err := s.triage(r.Context()); err == nil {
		waiting = waitingOnYou(rows)
	}
	d := pageData{Title: pageTitle(name), CSRF: s.csrf(emailOf(r)), Write: s.cfg.allowWrite, Preview: s.cfg.preview, Base: s.base(),
		Waiting: waiting, Updated: s.now().Local().Format("15:04"), Refresh: refreshes(name, body), Body: body}
	if err := pages.ExecuteTemplate(w, name, d); err != nil {
		state.LogActivity("serve: rendering %s: %v", name, err)
	}
}

func pageTitle(name string) string {
	switch name {
	case "triage":
		return "Sessions"
	case "session":
		return "Session"
	case "text":
		return "PRs"
	case "launch":
		return "Launch"
	case "confirm":
		return "Confirm"
	}
	return "ccs"
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.renderCode(w, r, code, "message", map[string]string{"Heading": "Couldn't do that", "Text": msg})
}

func (s *server) triage(ctx context.Context) ([]TriageRow, error) {
	convs, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	return TriageRows(convs, liveSessions(), s.now(), DefaultStallAfter), nil
}

func (s *server) triagePage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.triage(r.Context())
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "couldn't read sessions: "+err.Error())
		return
	}
	names := map[string]string{}
	for _, row := range rows {
		names[row.Session] = row.Name
	}
	s.render(w, r, "triage", map[string]any{"Rows": rows, "Buckets": buckets(rows), "Prompts": s.promptViews("", names)})
}

func (s *server) triageJSON(w http.ResponseWriter, r *http.Request) {
	rows, err := s.triage(r.Context())
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
	HasNext, Live, Codex   bool
	HasPrev                bool
	Ask                    *askView
	Prompts                []promptView // held for the page by `ccs hook permission`
	Messages               []msgView    `json:"-"` // the page's bubbles; the API keeps read_session's Text
	Total                  int          // messages in the conversation
}

// askView is a waiting session's pending question, shown but never answered
// from here: typing a digit into its terminal could land on whatever is on
// screen by then (a permission prompt), and Claude Code has no way yet for
// ccs to answer a prompt by its id.
type askView struct {
	ID, Question string
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
	text, err := serveCall("read_session", args, s.loader(r.Context()))
	if err != nil {
		return sessionView{}, http.StatusNotFound, err
	}
	// A negative offset counts from the end, so -40 is the latest page and
	// nothing is newer than it.
	v := sessionView{ID: id, Text: text, Offset: offset, Prev: offset - 40, Next: offset + 40, HasNext: offset >= 0 || offset+40 < 0}
	if offset >= 0 {
		v.Prev = max(0, offset-40)
	}
	if rows, err := s.triage(r.Context()); err == nil {
		for _, row := range rows {
			if row.Session == id {
				v.Name, v.Status, v.Live, v.Codex = row.Name, row.Status, row.PID > 0, row.Kind == transcript.KindCodex
				v.Prompts = s.promptViews(id, map[string]string{id: row.Name})
			}
		}
	}
	if conv, ok := s.conv(r.Context(), id); ok {
		start, end := pageOf(len(conv.Messages), offset)
		v.Messages, v.Total = messageViews(conv, start, end), len(conv.Messages)
		v.Prev, v.HasPrev = max(0, start-msgPage), start > 0
		v.Next, v.HasNext = end, end < len(conv.Messages)
		if v.Live && len(conv.Questions()) > 0 && conv.Pending != nil {
			v.Ask = &askView{ID: conv.Pending.ID, Question: conv.PendingAsk()}
		}
	}
	return v, http.StatusOK, nil
}

func (s *server) conv(ctx context.Context, id string) (transcript.Conversation, bool) {
	convs, err := s.load(ctx)
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
		text, err := serveCall(tool, args, s.loader(r.Context()))
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		s.render(w, r, "text", map[string]string{"Heading": title, "Text": text})
	}
}

func (s *server) textJSON(tool string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		text, err := serveCall(tool, nil, s.loader(r.Context()))
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
	Kind, ID, Name, Text, Cwd, Brief string
	Codex, Graceful                  bool
	Prompt, Choice                   string              // a held prompt and what to do with it
	Answers                          map[string][]string // its questions' chosen labels, by q<i>, or one free-text answer
	Other                            map[string]bool     // q<i> answered with free text (the page's "Other" choice)
	Notes                            map[string]string   // q<i>'s note on its answer (the page's note field)
}

// otherChoice is the page's "Other" radio or checkbox value; the text is in
// q<i>_other.
const otherChoice = "__other__"

// AnswerFields are the answers as hidden form fields, for the confirm page.
func (a action) AnswerFields() [][2]string {
	var out [][2]string
	keys := make([]string, 0, len(a.Answers))
	for k := range a.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a.Other[k] {
			out = append(out, [2]string{k, otherChoice})
			for _, v := range a.Answers[k] {
				out = append(out, [2]string{k + "_other", v})
			}
			continue
		}
		for _, v := range a.Answers[k] {
			out = append(out, [2]string{k, v})
		}
	}
	nkeys := make([]string, 0, len(a.Notes))
	for k := range a.Notes {
		nkeys = append(nkeys, k)
	}
	sort.Strings(nkeys)
	for _, k := range nkeys {
		out = append(out, [2]string{k + "_note", a.Notes[k]})
	}
	return out
}

func formAction(r *http.Request) action {
	f := r.PostFormValue
	answers, other, notes := answerFields(r)
	return action{
		Kind: f("action"), ID: f("id"), Name: f("name"), Text: f("text"),
		Cwd: f("cwd"), Brief: f("brief"),
		Codex: f("codex") == "on", Graceful: f("graceful") == "on",
		Prompt: f("prompt"), Choice: f("choice"), Answers: answers, Other: other, Notes: notes,
	}
}

// answerFields are the form's q<i> fields (several for a multi-select).
func answerFields(r *http.Request) (map[string][]string, map[string]bool, map[string]string) {
	out, other, notes := map[string][]string{}, map[string]bool{}, map[string]string{}
	for k, vs := range r.PostForm {
		if q, ok := strings.CutSuffix(k, "_note"); ok && len(q) > 1 && q[0] == 'q' && strings.Trim(q[1:], "0123456789") == "" {
			if n := strings.TrimSpace(r.PostFormValue(k)); n != "" {
				notes[q] = n
			}
			continue
		}
		if len(k) > 1 && k[0] == 'q' && strings.Trim(k[1:], "0123456789") == "" {
			if slices.Contains(vs, otherChoice) { // "Other": its text is the answer
				out[k], other[k] = []string{r.PostFormValue(k + "_other")}, true
				continue
			}
			out[k] = vs
		}
	}
	return out, other, notes
}

// describe says in plain words what the action will do.
func (a action) describe() (string, error) {
	switch a.Kind {
	case "send":
		return fmt.Sprintf("Send this message to %s, as you:\n\n%s", a.Name, a.Text), nil
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
	if a.Kind == "prompt" {
		s.actPrompt(w, r, a, email)
		return
	}
	what, err := a.describe()
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if r.PostFormValue("confirm") != "yes" {
		s.render(w, r, "confirm", map[string]any{"What": what, "A": a})
		return
	}
	text, err := s.do(r.Context(), a)
	state.LogActivity("serve: %s %s %s: %s", email, a.Kind, orDash(a.Name), logResult(text, err))
	if err != nil {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	s.render(w, r, "message", map[string]string{"Heading": "Done", "Text": text, "Back": a.ID})
}

// actPrompt answers a held prompt: shown first, then, confirmed, sent back
// on that prompt's own hook connection. A prompt answered, let go or ended
// meanwhile is gone, so nothing is sent.
func (s *server) actPrompt(w http.ResponseWriter, r *http.Request, a action, email string) {
	gone := "that prompt isn't waiting here any more (answered, shown in its tab, or ended): nothing was sent"
	p := s.prompts.get(a.Prompt)
	if p == nil {
		s.fail(w, r, http.StatusConflict, gone)
		return
	}
	reply, what, err := promptDecision(p, a.Choice, a.Answers, a.Other, a.Notes, email)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	what = "In " + orDash(a.Name) + ":\n" + what
	if r.PostFormValue("confirm") != "yes" {
		s.render(w, r, "confirm", map[string]any{"What": what, "A": a})
		return
	}
	if p = s.prompts.take(a.Prompt); p == nil {
		s.fail(w, r, http.StatusConflict, gone)
		return
	}
	p.reply <- reply
	state.LogActivity("serve: %s settled %s's %s prompt %s: %s", email, orDash(a.Name), p.req.ToolName, p.id, a.Choice)
	s.render(w, r, "message", map[string]string{"Heading": "Done", "Text": what, "Back": p.req.SessionID})
}

func logResult(text string, err error) string {
	if err != nil {
		return "failed: " + err.Error()
	}
	return text
}

func (s *server) do(ctx context.Context, a action) (string, error) {
	if a.Name == "" {
		return "", errors.New("which session? (name)")
	}
	switch a.Kind {
	case "send":
		return s.send(a)
	case "launch":
		args, _ := json.Marshal(map[string]any{"name": a.Name, "cwd": a.Cwd, "brief": a.Brief, "codex": a.Codex})
		return serveCall("launch_session", args, s.loader(ctx))
	case "close":
		if err := s.sameSession(ctx, a); err != nil {
			return "", err
		}
		args, _ := json.Marshal(map[string]any{"name": a.Name, "session_id": a.ID, "graceful": a.Graceful})
		return serveCall("close_session", args, s.loader(ctx))
	case "restart":
		if err := s.sameSession(ctx, a); err != nil {
			return "", err
		}
		args, _ := json.Marshal(map[string]any{"name": a.Name, "session_id": a.ID})
		return serveCall("restart_session", args, s.loader(ctx))
	}
	return "", fmt.Errorf("unknown action %q", a.Kind)
}

// sameSession refuses a close or restart unless the name, which the tools
// act on, is still that of exactly one running session, the one the page
// showed: another session may have taken the name since.
func (s *server) sameSession(ctx context.Context, a action) error {
	rows, err := s.triage(ctx)
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

// ServeMain is `ccs serve` from the command line.
func ServeMain(args []string, load func() ([]transcript.Conversation, error)) {
	os.Exit(RunServe(args, os.Stdout, os.Stderr, load))
}

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"inc":    func(i int) int { return i + 1 },
	"prName": github.PRName,
	// promptArgs is what the "prompt" template needs for one held prompt.
	"promptArgs": func(p promptView, base, csrf, sid, sname string) map[string]any {
		return map[string]any{"P": p, "Base": base, "CSRF": csrf, "SID": sid, "SName": sname}
	},
}).Parse(pageTemplates))

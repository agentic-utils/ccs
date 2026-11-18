package mcp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

const (
	testTeam = "me.cloudflareaccess.com"
	testAUD  = "aud-tag"
	testHost = "ccs.example.com"
	testUser = "me@example.com"
)

var testKey, _ = rsa.GenerateKey(rand.Reader, 2048)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signJWT makes an Access-style token with these claims, signed by key.
func signJWT(t *testing.T, key *rsa.PrivateKey, alg string, claims map[string]any) string {
	t.Helper()
	head, _ := json.Marshal(map[string]string{"alg": alg, "kid": "k1", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	signing := b64(head) + "." + b64(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func goodClaims() map[string]any {
	return map[string]any{
		"aud": []string{testAUD}, "iss": "https://" + testTeam, "email": "Me@Example.com",
		"exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
	}
}

// testServer is a ccs serve over these sessions, Access's certs holding
// testKey, writes allowed or not.
func testServer(t *testing.T, write bool, convs []transcript.Conversation) (*server, *int) {
	t.Helper()
	fetches := 0
	v := newAccessVerifier("me", testAUD, []string{testUser})
	v.fetch = func(u string) ([]byte, error) {
		fetches++
		if u != "https://"+testTeam+"/cdn-cgi/access/certs" {
			return nil, errors.New("wrong certs url " + u)
		}
		e := big.NewInt(int64(testKey.E)).Bytes()
		return json.Marshal(map[string]any{"keys": []map[string]string{{"kid": "k1", "kty": "RSA", "alg": "RS256", "n": b64(testKey.N.Bytes()), "e": b64(e)}}})
	}
	load := func() ([]transcript.Conversation, error) { return convs, nil }
	return newServer(serveConfig{host: testHost, allowWrite: write, access: v, load: load}), &fetches
}

func request(s *server, method, path, token string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, "https://"+testHost+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, "https://"+testHost+path, nil)
	}
	if token != "" {
		req.Header.Set("Cf-Access-Jwt-Assertion", token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	return w
}

func activityLog(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(state.ActivityLogPath)
	return string(b)
}

func TestLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:7467": true, "[::1]:7467": true, "localhost:1": true, "0.0.0.0:7467": false, ":7467": false, "192.168.1.2:80": false, "nonsense": false} {
		if err := loopbackOnly(addr); (err == nil) != ok {
			t.Errorf("loopbackOnly(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestServeRefusesWithoutAccess(t *testing.T) {
	var out, errb strings.Builder
	if code := RunServe([]string{"--host", "h"}, &out, &errb, nil); code != 2 || !strings.Contains(errb.String(), "never runs without Cloudflare Access") {
		t.Errorf("code %d: %s", code, errb.String())
	}
	if code := RunServe([]string{"--host", "h", "--team", "t", "--aud", "a", "--email", "e", "--addr", "0.0.0.0:1"}, &out, &errb, nil); code != 2 || !strings.Contains(errb.String(), "loopback") {
		t.Errorf("a public address must be refused: %d %s", code, errb.String())
	}
}

func TestAccessTokenChecks(t *testing.T) {
	s, fetches := testServer(t, false, nil)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	with := func(k string, v any) map[string]any { c := goodClaims(); c[k] = v; return c }
	cases := map[string]string{
		"no token":       "",
		"garbage":        "a.b.c",
		"alg none":       signJWT(t, testKey, "none", goodClaims()),
		"wrong key":      signJWT(t, other, "RS256", goodClaims()),
		"wrong audience": signJWT(t, testKey, "RS256", with("aud", "someone-else")),
		"wrong issuer":   signJWT(t, testKey, "RS256", with("iss", "https://evil.cloudflareaccess.com")),
		"expired":        signJWT(t, testKey, "RS256", with("exp", time.Now().Add(-time.Hour).Unix())),
		"not yet":        signJWT(t, testKey, "RS256", with("nbf", time.Now().Add(time.Hour).Unix())),
		"no expiry":      signJWT(t, testKey, "RS256", with("exp", 0)),
		"someone else":   signJWT(t, testKey, "RS256", with("email", "stranger@example.com")),
		"no email":       signJWT(t, testKey, "RS256", with("email", "")),
	}
	for name, tok := range cases {
		if w := request(s, "GET", "/", tok, nil, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, w.Code)
		}
	}
	good := signJWT(t, testKey, "RS256", goodClaims())
	if w := request(s, "GET", "/", good, nil, nil); w.Code != http.StatusOK {
		t.Errorf("a good token: %d %s", w.Code, w.Body)
	}
	if *fetches != 1 {
		t.Errorf("certs fetched %d times, want once (cached, refetches rate-limited)", *fetches)
	}
	// The wrong host is refused before the token is even looked at.
	req := httptest.NewRequest("GET", "https://attacker.example/", nil)
	req.Header.Set("Cf-Access-Jwt-Assertion", good)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("wrong host: %d", w.Code)
	}
	if log := activityLog(t); !strings.Contains(log, "serve: refused GET / from -: 403") || !strings.Contains(log, "serve: me@example.com GET / 200") {
		t.Errorf("requests should be logged with who and how they went:\n%s", log)
	}
}

func TestServeTriageEscapesAndHasSecurityHeaders(t *testing.T) {
	convs := []transcript.Conversation{{SessionID: "s-1", Title: `<script>alert(1)</script>`, Cwd: "/r", LastTimestamp: time.Now().UTC().Format(time.RFC3339),
		Messages: []transcript.Message{{Role: "user", Text: "hello", Ts: time.Now().UTC().Format(time.RFC3339)}}}}
	s, _ := testServer(t, false, convs)
	liveSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{"s-1": {PID: 42, Stat: sessions.SessionStat{Status: "idle"}}}
	}
	defer func() { liveSessions = func() map[string]sessions.LiveSession { return nil } }()
	w := request(s, "GET", "/", signJWT(t, testKey, "RS256", goodClaims()), nil, nil)
	body := w.Body.String()
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("titles must be escaped:\n%s", body)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("security headers missing: %v", w.Header())
	}
	if strings.Contains(body, `action="/act"`) || !strings.Contains(body, "read-only") {
		t.Errorf("a read-only server shows no forms")
	}
	w = request(s, "GET", "/api/triage", signJWT(t, testKey, "RS256", goodClaims()), nil, nil)
	var rows []TriageRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Session != "s-1" {
		t.Errorf("api/triage: %v %s", err, w.Body)
	}
}

// postAct posts an /act form the way the page does: same origin, with the
// CSRF token unless the form already has one.
func postAct(t *testing.T, s *server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if _, ok := form["csrf"]; !ok {
		form.Set("csrf", s.csrf(testUser))
	}
	return request(s, "POST", "/act", signJWT(t, testKey, "RS256", goodClaims()), form, map[string]string{"Origin": "https://" + testHost, "Sec-Fetch-Site": "same-origin"})
}

// testSince is the stub session file's statusUpdatedAt: the wait a page shows.
const testSince = int64(1762000000000)

// waitingFor is what Claude Code writes beside a waiting status for a
// question menu (AskUserQuestion).
func waitingFor(status string) string {
	if status == "waiting" {
		return answerableWait
	}
	return ""
}

type sent struct{ socket, token, sender, note, text string }

func stubSend(t *testing.T, status string) *[]sent {
	t.Helper()
	var got []sent
	oldSend, oldRead, oldToken, oldLive := sendEnvelope, readSessionFile, peerToken, liveSessions
	sendEnvelope = func(path, token, sender, note, text string, replyTo bool) (string, error) {
		if replyTo {
			t.Error("ccs serve serves no receipts, so it must not ask for them")
		}
		got = append(got, sent{path, token, sender, note, text})
		return "id", nil
	}
	readSessionFile = func(pid int) (sessions.SessionFile, error) {
		return sessions.SessionFile{SessionID: "s-1", Status: status, WaitingFor: waitingFor(status), StatusUpdatedAt: testSince, Socket: "/tmp/cc-socks/42.sock"}, nil
	}
	peerToken = func(int, string) string { return "tok" }
	liveSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{"s-1": {PID: 42, Stat: sessions.SessionStat{Status: status, Since: time.UnixMilli(testSince)}}}
	}
	t.Cleanup(func() { sendEnvelope, readSessionFile, peerToken, liveSessions = oldSend, oldRead, oldToken, oldLive })
	return &got
}

func TestServeWritesNeedAllowWrite(t *testing.T) {
	s, _ := testServer(t, false, nil)
	got := stubSend(t, "idle")
	w := postAct(t, s, url.Values{"action": {"send"}, "id": {"s-1"}, "name": {"w"}, "text": {"hi"}, "confirm": {"yes"}})
	if w.Code != http.StatusForbidden || len(*got) != 0 {
		t.Errorf("read-only must refuse writes: %d, sent %v", w.Code, *got)
	}
	if w := request(s, "GET", "/launch", signJWT(t, testKey, "RS256", goodClaims()), nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("read-only has no launch form: %d", w.Code)
	}
}

func TestServeSendChecksOriginCSRFAndConfirm(t *testing.T) {
	s, _ := testServer(t, true, nil)
	got := stubSend(t, "idle")
	form := func() url.Values {
		return url.Values{"action": {"send"}, "id": {"s-1"}, "name": {"worker"}, "text": {"ship it"}}
	}
	tok := signJWT(t, testKey, "RS256", goodClaims())
	// Cross-site: no origin, another origin, or a cross-site fetch.
	for _, hdr := range []map[string]string{{}, {"Origin": "https://evil.example"}, {"Origin": "https://" + testHost, "Sec-Fetch-Site": "cross-site"}} {
		f := form()
		f.Set("csrf", s.csrf(testUser))
		f.Set("confirm", "yes")
		if w := request(s, "POST", "/act", tok, f, hdr); w.Code != http.StatusForbidden {
			t.Errorf("%v: got %d, want 403", hdr, w.Code)
		}
	}
	f := form()
	f.Set("csrf", "forged")
	f.Set("confirm", "yes")
	if w := postAct(t, s, f); w.Code != http.StatusForbidden {
		t.Errorf("a bad CSRF token: %d", w.Code)
	}
	// Unconfirmed: shows what it would do, sends nothing.
	w := postAct(t, s, form())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Send this message to worker") || len(*got) != 0 {
		t.Fatalf("an unconfirmed send must only ask: %d sent=%v\n%s", w.Code, *got, w.Body)
	}
	f = form()
	f.Set("confirm", "yes")
	w = postAct(t, s, f)
	if w.Code != http.StatusOK || len(*got) != 1 {
		t.Fatalf("confirmed send: %d %v\n%s", w.Code, *got, w.Body)
	}
	if g := (*got)[0]; g.socket != "/tmp/cc-socks/42.sock" || g.token != "tok" || g.sender != remoteSender || g.note != transcript.CcsNote || g.text != "ship it" {
		t.Errorf("sent %+v", g)
	}
	if log := activityLog(t); !strings.Contains(log, "serve: me@example.com send worker: Sent to worker") {
		t.Errorf("a write must be logged with who did it:\n%s", log)
	}
	// Control characters never go.
	f = form()
	f.Set("text", "a\x1b[2Jb")
	f.Set("confirm", "yes")
	if w := postAct(t, s, f); w.Code != http.StatusConflict || len(*got) != 1 {
		t.Errorf("control characters: %d", w.Code)
	}
}

func TestServeSendNeverTypes(t *testing.T) {
	s, _ := testServer(t, true, nil)
	stubSend(t, "idle")
	readSessionFile = func(int) (sessions.SessionFile, error) { return sessions.SessionFile{SessionID: "s-1"}, nil } // no socket
	typed := false
	stubTerminal(t, func(sessions.Terminal, string, bool) error { typed = true; return nil })
	w := postAct(t, s, url.Values{"action": {"send"}, "id": {"s-1"}, "name": {"w"}, "text": {"hi"}, "confirm": {"yes"}})
	if w.Code != http.StatusConflict || typed || !strings.Contains(w.Body.String(), "never types") {
		t.Errorf("no socket must refuse, never type: %d typed=%v %s", w.Code, typed, w.Body)
	}
}

func waitingConv(t *testing.T, multi bool) transcript.Conversation {
	t.Helper()
	conv := transcript.Conversation{SessionID: "s-1", Title: "worker", Cwd: "/r", LastTimestamp: "2026-11-01T10:01:00Z"}
	ms := "false"
	if multi {
		ms = "true"
	}
	for _, l := range []string{
		`{"type":"user","timestamp":"2026-11-01T10:00:00Z","message":{"content":"go"}}`,
		`{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","message":{"content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"Merge now?","multiSelect":` + ms + `,"options":[{"label":"Merge"},{"label":"Wait"}]}]}}]}}`,
	} {
		if !transcript.ParseLine(&conv, []byte(l)) {
			t.Fatalf("unparsed: %s", l)
		}
	}
	return conv
}

func TestServeAnswer(t *testing.T) {
	s, _ := testServer(t, true, []transcript.Conversation{waitingConv(t, false)})
	stubSend(t, "waiting")
	var pressed []string
	stubTerminal(t, func(term sessions.Terminal, text string, enter bool) error {
		if enter || term.Target != "%42" {
			t.Errorf("terminal %v enter %v", term, enter)
		}
		pressed = append(pressed, text)
		return nil
	})

	page := request(s, "GET", "/session?id=s-1", signJWT(t, testKey, "RS256", goodClaims()), nil, nil).Body.String()
	if !strings.Contains(page, "Merge now?") || !strings.Contains(page, `name="ask" value="q1"`) || !strings.Contains(page, `name="since" value="1762000000000"`) || !strings.Contains(page, "1. Merge") {
		t.Errorf("the page should offer the question's options:\n%s", page)
	}
	ans := func(ask, opt string) *httptest.ResponseRecorder {
		return postAct(t, s, url.Values{"action": {"answer"}, "id": {"s-1"}, "name": {"worker"}, "ask": {ask}, "since": {strconv.FormatInt(testSince, 10)}, "option": {opt}, "confirm": {"yes"}})
	}
	if w := ans("q0", "1"); w.Code != http.StatusConflict || len(pressed) != 0 {
		t.Errorf("a stale question must not be answered: %d", w.Code)
	}
	if w := ans("q1", "3"); w.Code != http.StatusConflict || len(pressed) != 0 {
		t.Errorf("an option it doesn't have: %d", w.Code)
	}
	// Not waiting any more (answered in its tab meanwhile): no keys.
	waiting := readSessionFile
	readSessionFile = func(int) (sessions.SessionFile, error) {
		return sessions.SessionFile{SessionID: "s-1", Status: "busy"}, nil
	}
	if w := ans("q1", "1"); w.Code != http.StatusConflict || len(pressed) != 0 {
		t.Errorf("not waiting: %d", w.Code)
	}
	s.limits = newRateLimits() // past the write burst; the limit has its own test
	// A wait it can't name (no waitingFor, or anything but a question menu),
	// or another wait than the page showed, gets no keys either.
	for _, f := range []sessions.SessionFile{
		{SessionID: "s-1", Status: "waiting", StatusUpdatedAt: testSince},
		{SessionID: "s-1", Status: "waiting", WaitingFor: "dialog open", StatusUpdatedAt: testSince},
		{SessionID: "s-1", Status: "waiting", WaitingFor: answerableWait, StatusUpdatedAt: testSince + 1},
	} {
		readSessionFile = func(int) (sessions.SessionFile, error) { return f, nil }
		if w := ans("q1", "1"); w.Code != http.StatusConflict || len(pressed) != 0 {
			t.Errorf("%+v: %d", f, w.Code)
		}
	}
	readSessionFile = waiting
	s.limits = newRateLimits() // past the write burst; the limit has its own test
	if w := postAct(t, s, url.Values{"action": {"answer"}, "id": {"s-1"}, "name": {"worker"}, "ask": {"q1"}, "option": {"1"}, "confirm": {"yes"}}); w.Code != http.StatusConflict || len(pressed) != 0 {
		t.Errorf("an answer that doesn't say which wait it's for: %d", w.Code)
	}
	if w := ans("q1", "2"); w.Code != http.StatusOK || len(pressed) != 1 || pressed[0] != "2" {
		t.Errorf("answer: %d %v %s", w.Code, pressed, w.Body)
	}
	// Each question once: a second press (a double tap, or two confirm pages)
	// could land on whatever menu comes next.
	if w := ans("q1", "1"); w.Code != http.StatusConflict || len(pressed) != 1 || !strings.Contains(w.Body.String(), "already answered") {
		t.Errorf("answered twice: %d %v", w.Code, pressed)
	}
}

// stubTerminal makes every session's terminal tmux pane %<pid> and sends its
// keys to send.
func stubTerminal(t *testing.T, send func(sessions.Terminal, string, bool) error) {
	t.Helper()
	oldFind, oldSend := sessions.FindTerminal, sessions.SendKeys
	sessions.FindTerminal = func(pid int) sessions.Terminal {
		return sessions.Terminal{Kind: "tmux pane", Target: "%" + strconv.Itoa(pid)}
	}
	sessions.SendKeys = send
	t.Cleanup(func() { sessions.FindTerminal, sessions.SendKeys = oldFind, oldSend })
}

// The session file is read again after the terminal lookup and the scan,
// the last thing before the keys: a permission prompt up instead, or the
// menu answered in its tab and another wait begun, gets no keys.
func TestServeAnswerChecksAgainAfterTheLookup(t *testing.T) {
	s, _ := testServer(t, true, []transcript.Conversation{waitingConv(t, false)})
	stubTerminal(t, func(sessions.Terminal, string, bool) error { t.Error("pressed keys"); return nil })
	ans := func() *httptest.ResponseRecorder {
		return postAct(t, s, url.Values{"action": {"answer"}, "id": {"s-1"}, "name": {"worker"}, "ask": {"q1"}, "since": {strconv.FormatInt(testSince, 10)}, "option": {"1"}, "confirm": {"yes"}})
	}
	find := sessions.FindTerminal
	for want, f := range map[string]sessions.SessionFile{
		"not a question menu":        {SessionID: "s-1", Status: "waiting", WaitingFor: "permission prompt", StatusUpdatedAt: testSince},
		"has changed since the page": {SessionID: "s-1", Status: "waiting", WaitingFor: answerableWait, StatusUpdatedAt: testSince + 5000},
		"waiting on its menu now":    {SessionID: "s-1", Status: "busy", StatusUpdatedAt: testSince + 5000},
		"session has changed":        {SessionID: "s-2", Status: "waiting", WaitingFor: answerableWait, StatusUpdatedAt: testSince},
	} {
		stubSend(t, "waiting")
		sessions.FindTerminal = func(pid int) sessions.Terminal {
			readSessionFile = func(int) (sessions.SessionFile, error) { return f, nil }
			return find(pid)
		}
		if w := ans(); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s", want, w.Code, w.Body)
		}
	}
}

// Requests at once share one scan of the transcripts.
func TestServeSharesOneLoad(t *testing.T) {
	s, _ := testServer(t, false, nil)
	release := make(chan struct{})
	var loads atomic.Int32
	s.cfg.load = func() ([]transcript.Conversation, error) {
		loads.Add(1)
		<-release
		return nil, nil
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); s.load(context.Background()) }()
	}
	for loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the others join the scan in flight
	close(release)
	wg.Wait()
	if n := loads.Load(); n != 1 {
		t.Errorf("%d scans for 3 requests at once, want 1", n)
	}
}

// A request whose client goes away stops waiting for the scan, freeing its
// slot; the scan finishes for whoever asks next.
func TestServeLoadStopsWaitingWhenTheClientLeaves(t *testing.T) {
	s, _ := testServer(t, false, nil)
	release := make(chan struct{})
	s.cfg.load = func() ([]transcript.Conversation, error) {
		<-release
		return []transcript.Conversation{{SessionID: "x"}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.load(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled request kept waiting for the scan")
	}
	close(release)
	if convs, err := s.load(context.Background()); err != nil || len(convs) != 1 {
		t.Errorf("the next request: %v %v", convs, err)
	}
}

func TestServeAnswerRefusesMultiSelect(t *testing.T) {
	s, _ := testServer(t, true, []transcript.Conversation{waitingConv(t, true)})
	stubSend(t, "waiting")
	stubTerminal(t, func(sessions.Terminal, string, bool) error { t.Error("pressed keys"); return nil })
	w := postAct(t, s, url.Values{"action": {"answer"}, "id": {"s-1"}, "name": {"worker"}, "ask": {"q1"}, "since": {strconv.FormatInt(testSince, 10)}, "option": {"1"}, "confirm": {"yes"}})
	if w.Code != http.StatusConflict {
		t.Errorf("multi-select: %d", w.Code)
	}
}

func TestServeSessionActionsGoThroughTheTools(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	s, _ := testServer(t, true, []transcript.Conversation{
		{SessionID: "s-1", Title: "worker", Cwd: "/r", LastTimestamp: now},
		{SessionID: "s-2", Title: "other", Cwd: "/r", LastTimestamp: now},
	})
	oldLive := liveSessions
	liveSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{"s-1": {PID: 42}, "s-2": {PID: 43}}
	}
	defer func() { liveSessions = oldLive }()
	var calls []string
	old := serveCall
	serveCall = func(name string, raw json.RawMessage, _ mcpSource) (string, error) {
		calls = append(calls, name+" "+string(raw))
		return "ok", nil
	}
	defer func() { serveCall = old }()
	for _, f := range []url.Values{
		{"action": {"launch"}, "name": {"new"}, "cwd": {"/r"}, "brief": {"do it"}, "codex": {"on"}},
		{"action": {"close"}, "id": {"s-1"}, "name": {"worker"}, "graceful": {"on"}},
		{"action": {"restart"}, "id": {"s-1"}, "name": {"worker"}},
	} {
		f.Set("confirm", "yes")
		if w := postAct(t, s, f); w.Code != http.StatusOK {
			t.Errorf("%v: %d %s", f, w.Code, w.Body)
		}
	}
	want := []string{
		`launch_session {"brief":"do it","codex":true,"cwd":"/r","name":"new"}`,
		`close_session {"graceful":true,"name":"worker","session_id":"s-1"}`,
		`restart_session {"name":"worker","session_id":"s-1"}`,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", strings.Join(calls, "\n"))
	}
	if w := postAct(t, s, url.Values{"action": {"rm -rf"}, "name": {"x"}, "confirm": {"yes"}}); w.Code != http.StatusBadRequest {
		t.Errorf("unknown action: %d", w.Code)
	}
	// A page for one session must never close another that took its name.
	s.limits = newRateLimits()
	for _, f := range []url.Values{
		{"action": {"close"}, "id": {"s-old"}, "name": {"worker"}},
		{"action": {"restart"}, "id": {"s-2"}, "name": {"worker"}},
	} {
		f.Set("confirm", "yes")
		if w := postAct(t, s, f); w.Code != http.StatusConflict || len(calls) != 3 {
			t.Errorf("%v: %d, calls %d", f, w.Code, len(calls))
		}
	}
}

func TestAccessKeysExpire(t *testing.T) {
	s, fetches := testServer(t, false, nil)
	v := s.cfg.access
	clock := time.Now()
	v.now = func() time.Time { return clock }
	good := func() string {
		c := goodClaims()
		c["exp"], c["nbf"] = clock.Add(time.Hour).Unix(), clock.Add(-time.Minute).Unix()
		return signJWT(t, testKey, "RS256", c)
	}
	if _, err := v.verify(good()); err != nil || *fetches != 1 {
		t.Fatalf("first: %v, fetches %d", err, *fetches)
	}
	// Past accessKeysFresh the certs are fetched again; a key Cloudflare
	// dropped stops working.
	clock = clock.Add(accessKeysFresh + time.Minute)
	fetch := v.fetch
	v.fetch = func(string) ([]byte, error) {
		*fetches++
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		return json.Marshal(map[string]any{"keys": []map[string]string{{"kid": "k2", "kty": "RSA", "n": b64(other.N.Bytes()), "e": "AQAB"}}})
	}
	if _, err := v.verify(good()); err == nil || *fetches != 2 {
		t.Errorf("a retired key must be refused once the certs are refetched: %v, fetches %d", err, *fetches)
	}
	// A failing fetch keeps keys younger than accessKeysStale, then refuses.
	v.fetch = fetch
	clock = clock.Add(accessRefetch)
	v.verify(good()) // fetches k1 again
	v.fetch = func(string) ([]byte, error) { *fetches++; return nil, errors.New("down") }
	clock = clock.Add(accessKeysFresh + time.Minute)
	if _, err := v.verify(good()); err != nil {
		t.Errorf("keys under accessKeysStale should still work while Cloudflare is down: %v", err)
	}
	clock = clock.Add(accessKeysStale)
	if _, err := v.verify(good()); err == nil {
		t.Error("keys past accessKeysStale must be refused")
	}
}

func TestServeRateLimits(t *testing.T) {
	l := newRateLimits()
	now := time.Now()
	for i := range 5 {
		if !l.allow("a", true, now) {
			t.Fatalf("write %d refused within the burst", i)
		}
	}
	if l.allow("a", true, now) {
		t.Error("the sixth write at once should be refused")
	}
	if !l.allow("a", true, now.Add(12*time.Second)) {
		t.Error("a write 12s later should be allowed")
	}
	if !l.allow("b", true, now) || !l.allow("a", false, now) {
		t.Error("limits are per email and per kind")
	}
}

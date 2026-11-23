package mcp

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

const askHook = `{"session_id":"s-1","cwd":"/r","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Which colour?","header":"Colour","multiSelect":false,"options":[{"label":"Red","description":"warm"},{"label":"Blue","description":"cool"}]}]}}`
const bashHook = `{"session_id":"s-1","cwd":"/r","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"clean the build"}}`

// runHook runs `ccs hook permission` on input in the background; its output
// arrives on the channel once it exits.
func runHook(t *testing.T, input string) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		var b strings.Builder
		permissionHook(strings.NewReader(input), &b)
		out <- b.String()
	}()
	return out
}

func hookOutput(t *testing.T, out <-chan string) string {
	t.Helper()
	select {
	case s := <-out:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the hook never returned")
	}
	return ""
}

// hookServer is a read-write ccs serve over a running session s-1 called
// worker, holding prompts on the test socket.
func hookServer(t *testing.T, wait time.Duration) *server {
	t.Helper()
	conv := transcript.Conversation{SessionID: "s-1", Title: "worker", Cwd: "/r", LastTimestamp: time.Now().UTC().Format(time.RFC3339)}
	s, _ := testServer(t, true, []transcript.Conversation{conv})
	s.cfg.hookWait = wait
	old := liveSessions
	hookLive.Store(liveFn(nil))
	liveSessions = func() map[string]sessions.LiveSession {
		if f := hookLive.Load().(liveFn); f != nil {
			return f()
		}
		return map[string]sessions.LiveSession{"s-1": {PID: 42, Stat: sessions.SessionStat{Status: "waiting"}}}
	}
	stop, err := s.listenHooks()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(); liveSessions = old; hookLive.Store(liveFn(nil)) })
	return s
}

// hookLive overrides hookServer's scan while ccs serve's own loop reads it:
// set through this, never by reassigning liveSessions, which that loop
// reads concurrently.
type liveFn func() map[string]sessions.LiveSession

var hookLive atomic.Value

func waitHeld(t *testing.T, s *server) *heldPrompt {
	t.Helper()
	for range 500 {
		if ps := s.prompts.list(""); len(ps) > 0 {
			return ps[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no prompt was held")
	return nil
}

func decisionOf(t *testing.T, out string) map[string]any {
	t.Helper()
	var v struct {
		HookSpecificOutput struct {
			HookEventName string         `json:"hookEventName"`
			Decision      map[string]any `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.HookSpecificOutput.HookEventName != "PermissionRequest" {
		t.Fatalf("not a PermissionRequest decision: %q (%v)", out, err)
	}
	return v.HookSpecificOutput.Decision
}

func TestHookWithNoServePrintsNothing(t *testing.T) {
	if out := hookOutput(t, runHook(t, bashHook)); out != "" {
		t.Errorf("no ccs serve: the terminal asks as usual, so nothing is printed: %q", out)
	}
	if out := hookOutput(t, runHook(t, `not json`)); out != "" {
		t.Errorf("bad input: %q", out)
	}
}

// ccs serve going away mid-wait (a crash) lets the prompt show in its tab:
// the hook prints nothing, never a denial.
func TestHookFallsBackWhenServeGoesAway(t *testing.T) {
	ln, err := net.Listen("unix", hookSockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(hookSockPath())
	go func() {
		c, err := ln.Accept()
		if err == nil {
			buf := make([]byte, 4096)
			c.Read(buf)
			c.Close() // dies holding it
		}
		ln.Close()
	}()
	if out := hookOutput(t, runHook(t, bashHook)); out != "" {
		t.Errorf("a ccs serve that died: %q", out)
	}
}

func TestHookAnswersAQuestionFromThePage(t *testing.T) {
	s := hookServer(t, time.Minute)
	out := runHook(t, askHook)
	p := waitHeld(t, s)

	tok := signJWT(t, testKey, "RS256", goodClaims())
	page := request(s, "GET", "/session?id=s-1", tok, nil, nil).Body.String()
	for _, want := range []string{"Waiting for you here: AskUserQuestion", `name="q0" value="Red"`, "Which colour?", "in its tab too"} {
		if !strings.Contains(page, want) {
			t.Errorf("the session page lacks %q", want)
		}
	}
	if strings.Contains(page, "Show it in its tab instead") || strings.Contains(page, `value="mac"`) {
		t.Error("the page has no send-back button: the menu is in its tab too")
	}
	if home := request(s, "GET", "/", tok, nil, nil).Body.String(); !strings.Contains(home, "Waiting for you here") || !strings.Contains(home, "worker") {
		t.Error("the sessions page lists the held prompt")
	}
	form := func(extra url.Values) url.Values {
		f := url.Values{"action": {"prompt"}, "choice": {"answer"}, "prompt": {p.id}, "id": {"s-1"}, "name": {"worker"}}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	if w := postAct(t, s, form(url.Values{"q0": {"Green"}, "confirm": {"yes"}})); w.Code != http.StatusBadRequest {
		t.Errorf("an answer that isn't an option: %d", w.Code)
	}
	if w := postAct(t, s, form(url.Values{"confirm": {"yes"}})); w.Code != http.StatusBadRequest {
		t.Errorf("no answer: %d", w.Code)
	}
	w := postAct(t, s, form(url.Values{"q0": {"Red"}}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Which colour? → Red") || !strings.Contains(w.Body.String(), `name="q0" value="Red"`) {
		t.Fatalf("the confirm page: %d %s", w.Code, w.Body)
	}
	if s.prompts.get(p.id) == nil {
		t.Fatal("confirming hasn't happened yet: the prompt is still held")
	}
	if w := postAct(t, s, form(url.Values{"q0": {"Red"}, "confirm": {"yes"}})); w.Code != http.StatusOK {
		t.Fatalf("confirmed: %d %s", w.Code, w.Body)
	}
	d := decisionOf(t, hookOutput(t, out))
	in, _ := d["updatedInput"].(map[string]any)
	answers, _ := in["answers"].(map[string]any)
	if d["behavior"] != "allow" || answers["Which colour?"] != "Red" || in["questions"] == nil {
		t.Errorf("decision %v", d)
	}
	if w := postAct(t, s, form(url.Values{"q0": {"Blue"}, "confirm": {"yes"}})); w.Code != http.StatusConflict {
		t.Errorf("a second answer to the same prompt: %d", w.Code)
	}
}

func TestHookAllowsDeniesOrLetsGoAPermissionPrompt(t *testing.T) {
	for choice, want := range map[string]string{"allow": "allow", "deny": "deny", "mac": ""} {
		t.Run(choice, func(t *testing.T) { checkPromptChoice(t, choice, want) })
	}
}

func checkPromptChoice(t *testing.T, choice, want string) {
	{
		s := hookServer(t, time.Minute)
		out := runHook(t, bashHook)
		p := waitHeld(t, s)
		if choice == "allow" {
			page := request(s, "GET", "/session?id=s-1", signJWT(t, testKey, "RS256", goodClaims()), nil, nil).Body.String()
			if !strings.Contains(page, "rm -rf build") || !strings.Contains(page, ">Allow<") || !strings.Contains(page, ">Deny<") {
				t.Errorf("the page shows the command and both buttons:\n%s", page)
			}
		}
		w := postAct(t, s, url.Values{"action": {"prompt"}, "choice": {choice}, "prompt": {p.id}, "id": {"s-1"}, "name": {"worker"}, "confirm": {"yes"}})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", choice, w.Code, w.Body)
		}
		got := hookOutput(t, out)
		if want == "" {
			if got != "" {
				t.Errorf("mac: the prompt shows in its tab, so nothing is printed: %q", got)
			}
			return
		}
		if d := decisionOf(t, got); d["behavior"] != want {
			t.Errorf("%s: %v", choice, d)
		}
	}
}

// A key pressed on the Mac after the prompt arrived lets it go to its tab;
// so does the wait running out. Neither decides anything.
func TestHeldPromptsGoBackToTheirTab(t *testing.T) {
	oldPoll, oldIdle := keyPoll, keyIdle
	t.Cleanup(func() { keyPoll, keyIdle = oldPoll, oldIdle })
	keyPoll = 10 * time.Millisecond

	t.Run("a key on the Mac", func(t *testing.T) {
		var mu sync.Mutex
		idle := time.Hour // no key since long before the prompt
		keyIdle = func() (time.Duration, bool) { mu.Lock(); defer mu.Unlock(); return idle, true }
		s := hookServer(t, time.Minute)
		out := runHook(t, bashHook)
		waitHeld(t, s)
		time.Sleep(50 * time.Millisecond)
		if len(s.prompts.list("")) != 1 {
			t.Fatal("no key pressed since: it stays held")
		}
		mu.Lock()
		idle = 0 // a key just now
		mu.Unlock()
		if got := hookOutput(t, out); got != "" {
			t.Errorf("a key on the Mac: %q", got)
		}
	})
	t.Run("the wait runs out", func(t *testing.T) {
		keyIdle = func() (time.Duration, bool) { return time.Hour, true }
		hookServer(t, 50*time.Millisecond)
		if got := hookOutput(t, runHook(t, bashHook)); got != "" {
			t.Errorf("the wait ran out: %q", got)
		}
	})
	t.Run("a preview", func(t *testing.T) {
		// The user is at the Mac by design: keys don't let it go.
		keyIdle = func() (time.Duration, bool) { return 0, true }
		s := hookServer(t, time.Minute)
		s.cfg.preview = true
		runHook(t, bashHook)
		waitHeld(t, s)
		time.Sleep(50 * time.Millisecond)
		if len(s.prompts.list("")) != 1 {
			t.Error("a preview keeps it held through key presses")
		}
	})
}

func TestOneServeHoldsPrompts(t *testing.T) {
	hookServer(t, time.Minute)
	s2, _ := testServer(t, true, nil)
	if _, err := s2.listenHooks(); err == nil || !strings.Contains(err.Error(), "another ccs serve") {
		t.Errorf("a second ccs serve: %v", err)
	}
}

func TestHookInstall(t *testing.T) {
	path := claudeSettingsPath()
	os.WriteFile(path, []byte(`{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"guard"}]}]}}`), 0o600)
	t.Cleanup(func() { os.Remove(path); os.Remove(path + backupSuffix); os.Remove(path + removedSuffix) })
	if msg, err := editHookSettings(path, true); err != nil || !strings.Contains(msg, "added") {
		t.Fatalf("%q %v", msg, err)
	}
	var got map[string]any
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &got)
	hooks := got["hooks"].(map[string]any)
	pr, _ := hooks["PermissionRequest"].([]any)
	if got["model"] != "opus" || hooks["PreToolUse"] == nil || len(pr) != 1 || !isOurHook(pr[0]) {
		t.Errorf("settings after install: %s", b)
	}
	if !strings.Contains(string(b), `"timeout": 1860`) {
		t.Errorf("the hook's timeout must outlast the longest wait: %s", b)
	}
	if old, _ := os.ReadFile(path + ".before-ccs-hook"); !strings.Contains(string(old), "guard") {
		t.Error("the old settings are kept beside them")
	}
	if msg, _ := editHookSettings(path, true); !strings.Contains(msg, "already") {
		t.Errorf("installing twice: %q", msg)
	}
	if msg, err := editHookSettings(path, false); err != nil || !strings.Contains(msg, "removed") {
		t.Fatalf("%q %v", msg, err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "PermissionRequest") || !strings.Contains(string(b), "guard") {
		t.Errorf("settings after uninstall: %s", b)
	}
	os.WriteFile(path, []byte(`{not json`), 0o600)
	if _, err := editHookSettings(path, true); err == nil {
		t.Error("invalid settings are left alone")
	}
}

// fakeServe listens on the hook socket and answers each hook with reply,
// given the hook's hello; mode is the socket's permissions.
func fakeServe(t *testing.T, mode os.FileMode, reply func(hookHello) []string) {
	t.Helper()
	path := hookSockPath()
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, mode)
	t.Cleanup(func() { ln.Close(); os.Remove(path) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				if !sc.Scan() {
					return
				}
				var h hookHello
				json.Unmarshal(sc.Bytes(), &h)
				for _, l := range reply(h) {
					c.Write([]byte(l + "\n"))
				}
				time.Sleep(100 * time.Millisecond)
			}()
		}
	}()
}

func line(r hookReply) string { b, _ := json.Marshal(r); return string(b) }

// Anything posing as ccs serve gets nothing through: a socket others can
// use, no acknowledgement in time, replies for another prompt (a replay),
// an allow carrying answers for a permission prompt, answers that aren't
// the question's options. Each lets the prompt show in its tab.
func TestHookTrustsOnlyServe(t *testing.T) {
	old := hookAckWait
	hookAckWait = 200 * time.Millisecond
	t.Cleanup(func() { hookAckWait = old })
	ok := func(h hookHello, r hookReply) []string {
		r.Nonce = h.Nonce
		return []string{line(hookReply{Nonce: h.Nonce, Held: true}), line(r)}
	}
	cases := []struct {
		name  string
		mode  os.FileMode
		input string
		reply func(hookHello) []string
	}{
		{"a socket others can use", 0o666, bashHook, func(h hookHello) []string { return ok(h, hookReply{Behavior: "allow"}) }},
		{"no acknowledgement", 0o600, bashHook, func(hookHello) []string { return nil }},
		{"an acknowledgement for another prompt", 0o600, bashHook, func(h hookHello) []string {
			return []string{line(hookReply{Nonce: "replayed", Held: true}), line(hookReply{Nonce: "replayed", Behavior: "allow"})}
		}},
		{"an answer for another prompt", 0o600, bashHook, func(h hookHello) []string {
			return []string{line(hookReply{Nonce: h.Nonce, Held: true}), line(hookReply{Nonce: "replayed", Behavior: "allow"})}
		}},
		{"answers on a permission prompt", 0o600, bashHook, func(h hookHello) []string {
			return ok(h, hookReply{Behavior: "allow", Answers: map[string]string{"x": "y"}})
		}},
		{"an answer that isn't an option", 0o600, askHook, func(h hookHello) []string {
			return ok(h, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Green"}})
		}},
		{"a question left unanswered", 0o600, askHook, func(h hookHello) []string { return ok(h, hookReply{Behavior: "allow"}) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeServe(t, c.mode, c.reply)
			start := time.Now()
			if out := hookOutput(t, runHook(t, c.input)); out != "" {
				t.Errorf("printed %q", out)
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("took %s: a silent listener must be given up on fast", time.Since(start))
			}
		})
	}
	// The input it allows is always the hook's own: serve can't send any.
	t.Run("the hook builds the input", func(t *testing.T) {
		fakeServe(t, 0o600, func(h hookHello) []string {
			return ok(h, hookReply{Behavior: "allow", Answers: map[string]string{"Which colour?": "Blue"}})
		})
		d := decisionOf(t, hookOutput(t, runHook(t, askHook)))
		in := d["updatedInput"].(map[string]any)
		if in["answers"].(map[string]any)["Which colour?"] != "Blue" || in["questions"] == nil || len(in) != 2 {
			t.Errorf("decision %v", d)
		}
	})
	t.Run("a folder others can write", func(t *testing.T) {
		fakeServe(t, 0o600, func(h hookHello) []string { return ok(h, hookReply{Behavior: "allow"}) })
		dir := filepath.Dir(hookSockPath())
		os.Chmod(dir, 0o777)
		defer os.Chmod(dir, 0o700)
		if out := hookOutput(t, runHook(t, bashHook)); out != "" {
			t.Errorf("printed %q", out)
		}
	})
	t.Run("a symlinked socket", func(t *testing.T) {
		real := filepath.Join(filepath.Dir(hookSockPath()), "real.sock")
		ln, err := net.Listen("unix", real)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		os.Remove(hookSockPath())
		os.Symlink(real, hookSockPath())
		defer os.Remove(hookSockPath())
		if err := trustedSocket(hookSockPath()); err == nil {
			t.Error("a symlink isn't a socket of its own")
		}
	})
}

// The preview holds prompts only behind its link: without its /p/<token>
// path no page, so no prompt id or form token, can be read.
func TestPreviewPromptsNeedItsLink(t *testing.T) {
	s := hookServer(t, time.Minute)
	s.cfg.preview, s.cfg.token = true, "tok"
	runHook(t, bashHook)
	waitHeld(t, s)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7467"+path, nil))
		return w
	}
	if w := get("/session?id=s-1"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "Waiting for you") {
		t.Errorf("without the link's path: %d", w.Code)
	}
	w := get("/p/tok/session?id=s-1")
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "Waiting for you") || strings.Contains(body, `action="/act"`) || !strings.Contains(body, `action="/p/tok/act"`) {
		t.Errorf("with the link's path, the held prompt's forms carry it: %d", w.Code)
	}
}

// Uninstall takes out ccs's hook alone, leaving others in its group.
func TestHookUninstallKeepsOtherHooks(t *testing.T) {
	path := claudeSettingsPath()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + backupSuffix); os.Remove(path + removedSuffix) })
	os.WriteFile(path, []byte(`{"hooks":{"PermissionRequest":[{"matcher":"*","hooks":[{"type":"command","command":"policy-guard"},{"type":"command","command":"/opt/homebrew/bin/ccs hook permission","timeout":1860}]},{"matcher":"Bash","hooks":[{"type":"command","command":"/x/notccs hook permission"}]}]}}`), 0o600)
	if msg, err := editHookSettings(path, false); err != nil || !strings.Contains(msg, "removed") {
		t.Fatalf("%q %v", msg, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "/opt/homebrew/bin/ccs") || !strings.Contains(string(b), "policy-guard") || !strings.Contains(string(b), "notccs") {
		t.Errorf("after uninstall: %s", b)
	}
	// A quoted path with spaces is still ccs's own.
	os.WriteFile(path, []byte(`{"hooks":{"PermissionRequest":[{"matcher":"*","hooks":[{"type":"command","command":"'/Users/me/My Tools/ccs' hook permission"}]}]}}`), 0o600)
	if msg, _ := editHookSettings(path, false); !strings.Contains(msg, "removed") {
		t.Errorf("a quoted path with spaces: %q", msg)
	}

}

// The kernel's word on the listener: this user's own process passes.
func TestPeerUIDIsOurs(t *testing.T) {
	path := filepath.Join(filepath.Dir(hookSockPath()), "peer.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			time.Sleep(100 * time.Millisecond)
			c.Close()
		}
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if uid, err := peerUID(c); err != nil || uid != uint32(os.Getuid()) {
		t.Errorf("peer uid %d, %v", uid, err)
	}
}

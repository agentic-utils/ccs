package mcp

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// chatConv is a running session with a short exchange: a question from the
// user, a reply with a code block, a long reply, and a message whose text
// tries to be markup.
func chatConv() transcript.Conversation {
	ts := func(m int) string { return time.Date(2026, 11, 18, 10, m, 0, 0, time.UTC).Format(time.RFC3339) }
	long := strings.Repeat("a line of the long reply\n", 20)
	return transcript.Conversation{SessionID: "s-1", Title: "worker", Cwd: "/r", LastTimestamp: ts(5), Messages: []transcript.Message{
		{Role: "user", Text: "what's the status?", Ts: ts(1)},
		{Role: "assistant", Text: "Here it is:\n```go\nfmt.Println(\"hi\")\n```\nAll good.", Ts: ts(2)},
		{Role: "assistant", Text: long, Ts: ts(3)},
		{Role: "user", Text: "try <script>alert(1)</script> here", Ts: ts(4)},
	}}
}

func liveAs(t *testing.T, status string) {
	t.Helper()
	old := liveSessions
	liveSessions = func() map[string]sessions.LiveSession {
		return map[string]sessions.LiveSession{"s-1": {PID: 42, Stat: sessions.SessionStat{Status: status}}}
	}
	t.Cleanup(func() { liveSessions = old })
}

func TestServeSessionShowsBubbles(t *testing.T) {
	s, _ := testServer(t, true, []transcript.Conversation{chatConv()})
	liveAs(t, "idle")
	page := request(s, "GET", "/session?id=s-1", signJWT(t, testKey, "RS256", goodClaims()), nil, nil).Body.String()
	for _, want := range []string{
		`class="msg you"`, `class="msg claude"`, // who said it
		"what&#39;s the status?",                // the user's text, escaped
		"<pre>fmt.Println(&#34;hi&#34;)</pre>",  // a code block, its language line dropped
		"Show all 21 lines",                     // a long reply folded
		"&lt;script&gt;alert(1)&lt;/script&gt;", // markup stays text
		`class="composer"`,                      // idle and writable: the composer
		"4 messages",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the session page lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(page), "<script") {
		t.Error("no script, ever")
	}
	if strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("an idle session's page doesn't refresh: it would wipe a message being typed")
	}
}

// The sessions page, and a session's page while it waits on you, refresh
// themselves; a waiting session's composer is hidden (a message would only
// be held until the menu is answered).
func TestServePagesRefreshOnlyWhileSomethingWaits(t *testing.T) {
	s, _ := testServer(t, true, []transcript.Conversation{chatConv()})
	tok := signJWT(t, testKey, "RS256", goodClaims())
	liveAs(t, "waiting")
	refresh := `<meta http-equiv="refresh" content="10">`
	if home := request(s, "GET", "/", tok, nil, nil).Body.String(); !strings.Contains(home, refresh) || !strings.Contains(home, "1 waiting on you") {
		t.Errorf("the sessions page refreshes and counts what waits")
	}
	page := request(s, "GET", "/session?id=s-1", tok, nil, nil).Body.String()
	if !strings.Contains(page, refresh) || strings.Contains(page, `class="composer"`) {
		t.Errorf("a waiting session's page refreshes, with no composer")
	}
	if l := request(s, "GET", "/launch", tok, nil, nil).Body.String(); strings.Contains(l, refresh) {
		t.Error("the launch form never refreshes")
	}
}

// No page has a script, and every page's links and forms in a preview,
// held prompts and the confirm page included, carry the secret path.
func TestServePreviewPagesCarryThePath(t *testing.T) {
	s := hookServer(t, time.Minute)
	s.cfg.preview, s.cfg.token = true, "tok"
	s.cfg.load = func() ([]transcript.Conversation, error) { return []transcript.Conversation{chatConv()}, nil }
	runHook(t, askHook)
	p := waitHeld(t, s)
	get := func(path string) string {
		req := httptest.NewRequest("GET", "http://127.0.0.1:7467/p/tok"+path, nil)
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, req)
		return w.Body.String()
	}
	form := url.Values{"action": {"prompt"}, "choice": {"answer"}, "prompt": {p.id}, "id": {"s-1"}, "name": {"worker"}, "q0": {"Red"}, "csrf": {s.csrf(previewUser)}}
	req := httptest.NewRequest("POST", "http://127.0.0.1:7467/p/tok/act", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:7467")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	pages := map[string]string{"/": get("/"), "/session": get("/session?id=s-1"), "/launch": get("/launch"), "confirm": w.Body.String()}
	for name, body := range pages {
		if strings.Contains(strings.ToLower(body), "<script") {
			t.Errorf("%s has a script", name)
		}
		for _, attr := range []string{`href="/`, `action="/`} {
			if n, m := strings.Count(body, attr), strings.Count(body, attr+"p/tok/"); n != m || n == 0 && attr == `href="/` {
				t.Errorf("%s: %d %s, %d under the secret path", name, n, attr, m)
			}
		}
	}
	if !strings.Contains(pages["/session"], `action="/p/tok/act"`) || !strings.Contains(pages["/session"], `class="opt"`) {
		t.Error("the held prompt's options are big rows posting under the path")
	}
	if !strings.Contains(pages["confirm"], `name="q0" value="Red"`) {
		t.Errorf("the confirm page carries the answer:\n%s", pages["confirm"])
	}
}

// The manifest and icons are behind the same checks as any page.
func TestServeManifestAndIcons(t *testing.T) {
	s, _ := testServer(t, false, nil)
	for _, path := range []string{"/manifest.webmanifest", "/icon.svg", "/icon/180"} {
		if w := request(s, "GET", path, "", nil, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s without Access: %d", path, w.Code)
		}
	}
	tok := signJWT(t, testKey, "RS256", goodClaims())
	w := request(s, "GET", "/manifest.webmanifest", tok, nil, nil)
	var m struct {
		StartURL string `json:"start_url"`
		Display  string `json:"display"`
		Icons    []map[string]string
	}
	json.Unmarshal(w.Body.Bytes(), &m)
	if w.Header().Get("Content-Type") != "application/manifest+json" || m.StartURL != "/" || m.Display != "standalone" || len(m.Icons) != 3 {
		t.Errorf("manifest: %s %s", w.Header(), w.Body)
	}
	for _, n := range []string{"180", "192", "512"} {
		w := request(s, "GET", "/icon/"+n, tok, nil, nil)
		img, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
		if err != nil || img.Bounds().Dx() != map[string]int{"180": 180, "192": 192, "512": 512}[n] {
			t.Errorf("icon %s: %v", n, err)
		}
	}
	if w := request(s, "GET", "/icon/64", tok, nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("an icon size it doesn't draw: %d", w.Code)
	}
	if w := request(s, "GET", "/", tok, nil, nil); !strings.Contains(w.Body.String(), `rel="manifest" href="/manifest.webmanifest" crossorigin="use-credentials"`) {
		t.Error("pages link the manifest, with credentials so Access lets it through")
	}

	pv := newServer(serveConfig{preview: true, token: "tok", load: func() ([]transcript.Conversation, error) { return nil, nil }})
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		pv.handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7467"+path, nil))
		return w
	}
	if w := get("/manifest.webmanifest"); w.Code != http.StatusNotFound {
		t.Errorf("a preview's manifest without the path: %d", w.Code)
	}
	if w := get("/p/tok/manifest.webmanifest"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"start_url":"/p/tok/"`) {
		t.Errorf("a preview's manifest: %d %s", w.Code, w.Body)
	}
	if w := get("/p/tok/icon/192"); w.Code != http.StatusOK {
		t.Errorf("a preview's icon: %d", w.Code)
	}
	// The mux's own redirects keep the secret path.
	if w := get("/p/tok//session?id=s-1"); w.Code/100 != 3 || !strings.HasPrefix(w.Header().Get("Location"), "/p/tok/session") {
		t.Errorf("a redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestSegmentsAndPaging(t *testing.T) {
	got := segments("before\n```sh\nls -la\n```\nafter")
	if len(got) != 3 || got[0] != (segment{Text: "before"}) || got[1] != (segment{Code: true, Text: "ls -la"}) || got[2] != (segment{Text: "after"}) {
		t.Errorf("segments: %+v", got)
	}
	// An unclosed fence stays prose: nothing is lost.
	if got := segments("see ```half"); len(got) != 2 || got[0].Code || got[1].Code || got[1].Text != "half" {
		t.Errorf("an unclosed fence: %+v", got)
	}
	for _, c := range []struct{ n, off, start, end int }{{100, -40, 60, 100}, {10, -40, 0, 10}, {100, 0, 0, 40}, {100, 80, 80, 100}, {100, 500, 100, 100}} {
		if s, e := pageOf(c.n, c.off); s != c.start || e != c.end {
			t.Errorf("pageOf(%d, %d) = %d, %d", c.n, c.off, s, e)
		}
	}
}

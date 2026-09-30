package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCollectUsageBucketsAndDedupes(t *testing.T) {
	dir := t.TempDir()
	old := getProjectsDir
	getProjectsDir = func() string { return dir }
	defer func() { getProjectsDir = old }()
	usageFilesMu.Lock()
	usageFiles = make(map[string]usageFile)
	usageFilesMu.Unlock()

	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	proj := filepath.Join(dir, "-p")
	sub := filepath.Join(proj, "sess", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string, mod time.Time) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, mod, mod)
	}
	recent := now.Add(-30 * time.Minute) // last bucket hour
	earlier := now.Add(-6 * time.Hour)
	write(filepath.Join(proj, "sess.jsonl"),
		usageTestLine("m1", "", recent, 10, 300, 100, 200, 1000, 50)+
			usageTestLine("m1", "", recent, 10, 300, 100, 200, 1000, 50)+ // same reply, second line
			usageTestLine("", "r1", earlier, 5, 20, 20, 0, 0, 7)+ // no message id: requestId; read 0 = cache miss
			usageTestLine("m0", "", now.Add(-13*time.Hour), 999, 0, 0, 0, 0, 999)+ // outside the window
			`{"type":"user","message":{"content":"hi"}}`+"\n",
		now)
	write(filepath.Join(sub, "agent-a.jsonl"), usageTestLine("s1", "", recent, 1, 40, 40, 0, 500, 3), now)
	write(filepath.Join(proj, "stale.jsonl"), usageTestLine("x", "", recent, 1e6, 0, 0, 0, 0, 1e6), now.Add(-24*time.Hour)) // file untouched for a day

	d := collectUsage(now, nil)
	var agg usageBucketTotals
	for _, b := range d.buckets {
		agg.Uncached += b.Uncached
		agg.C5m += b.C5m
		agg.C1h += b.C1h
		agg.Read += b.Read
		agg.New += b.New
		agg.Miss += b.Miss
		agg.Output += b.Output
		agg.Responses += b.Responses
	}
	want := usageBucketTotals{Uncached: 16, C5m: 160, C1h: 200, Read: 1500, New: 310 + 41, Miss: 25, Output: 60, Responses: 3}
	if agg != want {
		t.Errorf("totals = %+v, want %+v", agg, want)
	}
	if last := d.buckets[usageBuckets-6]; last.Responses != 2 { // 30 minutes ago = 6 buckets from the end
		t.Errorf("recent replies should land in the bucket 30m ago, got %+v", last)
	}
	eff1h := tokenUsage{Input: 10, Cache5m: 100, Cache1h: 200, CacheRead: 1000, Output: 50}.effective() +
		tokenUsage{Input: 1, Cache5m: 40, CacheRead: 500, Output: 3}.effective()
	eff12h := eff1h + tokenUsage{Input: 5, Cache5m: 20, Output: 7}.effective()
	if d.eff1h != eff1h || d.eff12h != eff12h {
		t.Errorf("effective 1h=%v 12h=%v", d.eff1h, d.eff12h)
	}
}

func TestRenderUsageChart(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local)
	buckets := make([]usageBucketTotals, usageBuckets)
	buckets[10] = usageBucketTotals{Output: 1000}
	buckets[20] = usageBucketTotals{Output: 500}
	merged := resampleBuckets(buckets, 72)
	if len(merged) != 72 || merged[5].Output != 1000 {
		t.Fatalf("resampleBuckets: %d cols", len(merged))
	}
	lines := renderUsageChart("Output", []usageSeries{{"output", "output tokens"}}, merged, 4, now)
	strip := func(s string) string { return regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(s, "") }
	if len(lines) != 1+4+2 {
		t.Fatalf("want title + 4 rows + baseline + labels, got %d lines", len(lines))
	}
	top := []rune(strip(lines[1]))
	if top[usageMargin+5] != '█' || top[usageMargin+10] == '█' {
		t.Errorf("tallest bar should reach the top row, half bar shouldn't: %q", strip(lines[1]))
	}
	if !strings.Contains(strip(lines[1]), "1k") {
		t.Errorf("y-axis should label the scale: %q", strip(lines[1]))
	}
	if !strings.Contains(strip(lines[len(lines)-1]), ":00") {
		t.Errorf("x-axis should have hourly labels: %q", strip(lines[len(lines)-1]))
	}
}

func TestFetchAllowance(t *testing.T) {
	var gotAuth, gotBeta string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		w.WriteHeader(status)
		io.WriteString(w, `{"limits":[{"kind":"session","percent":62,"resets_at":"2026-09-29T14:10:00+00:00"},{"kind":"weekly_all","percent":21.5,"resets_at":"2026-10-05T09:00:00+00:00"}]}`)
	}))
	defer srv.Close()
	oldURL, oldRead := allowanceURL, keychainRead
	defer func() { allowanceURL, keychainRead = oldURL, oldRead }()
	allowanceURL = srv.URL
	now := time.Now()
	creds := func(tok string, exp time.Time) {
		keychainRead = func() ([]byte, error) {
			return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, tok, exp.UnixMilli())), nil
		}
	}

	creds("tok", now.Add(time.Hour))
	limits, err := fetchAllowance(now)
	if err != nil || len(limits) != 2 || limits[0].Kind != "session" || limits[1].Percent != 21.5 {
		t.Fatalf("limits=%+v err=%v", limits, err)
	}
	if gotAuth != "Bearer tok" || gotBeta != "oauth-2025-04-20" {
		t.Errorf("headers: auth=%q beta=%q", gotAuth, gotBeta)
	}

	creds("tok", now.Add(-time.Minute)) // expired: never refreshed by ccs
	if _, err := fetchAllowance(now); !errors.Is(err, errAllowanceLogin) {
		t.Errorf("expired token should ask for a claude login, got %v", err)
	}
	keychainRead = func() ([]byte, error) { return nil, nil } // no credential stored
	if _, err := fetchAllowance(now); !errors.Is(err, errAllowanceLogin) {
		t.Errorf("missing credential should ask for a claude login, got %v", err)
	}
	creds("tok", now.Add(time.Hour))
	status = http.StatusUnauthorized
	if _, err := fetchAllowance(now); !errors.Is(err, errAllowanceLogin) {
		t.Errorf("401 should ask for a claude login, got %v", err)
	}
	status = http.StatusInternalServerError
	if _, err := fetchAllowance(now); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("server error should be reported, got %v", err)
	}
}

func TestKeychainServiceName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if keychainService() != "Claude Code-credentials" {
		t.Errorf("default profile: %q", keychainService())
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/me/.claude-work")
	sum := sha256.Sum256([]byte("/Users/me/.claude-work"))
	if want := "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]; keychainService() != want {
		t.Errorf("per-profile: %q, want %q", keychainService(), want)
	}
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "") // set but empty: the default item
	if keychainService() != "Claude Code-credentials" {
		t.Errorf("empty override: %q", keychainService())
	}
}

func TestUsageScreenToggleAndPanels(t *testing.T) {
	m := initialModel(buildItems([]Conversation{{SessionID: "a", Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 170, 50
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = res.(model)
	if !m.showUsage || cmd == nil || !m.usageLoading || !m.allowanceLoading {
		t.Fatal("tab should open the usage screen and start loading")
	}
	// Typing on the usage screen mustn't edit the hidden search.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m = res.(model); m.textInput.Value() != "" {
		t.Error("keys other than tab/ctrl+c are ignored on the usage screen")
	}
	buckets := make([]usageBucketTotals, usageBuckets)
	buckets[100] = usageBucketTotals{Uncached: 10, Read: 90, New: 10, Output: 5, Responses: 1}
	res, _ = m.Update(usageMsg{usageData{now: time.Now(), buckets: buckets, eff1h: 1000, eff12h: 2000}})
	m = res.(model)
	res, _ = m.Update(allowanceMsg{limits: []allowanceLimit{{Kind: "session", Percent: 62, ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)}}})
	m = res.(model)
	v := regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(m.View(), "")
	for _, want := range []string{"Input · cache write disposition", "Context assembly", "Output", "SUMMARY", "responses", "1 ", "ALLOWANCE", "5-hour session", "62%", "read from cache", "90.0%"} {
		if !strings.Contains(v, want) {
			t.Errorf("usage screen missing %q", want)
		}
	}
	res, _ = m.Update(allowanceMsg{err: errAllowanceLogin})
	if m = res.(model); !strings.Contains(m.View(), "62%") || !strings.Contains(m.View(), "open claude") {
		t.Error("a failed refresh keeps the last gauges and shows why")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m = res.(model); m.showUsage {
		t.Error("tab again returns to the session list")
	}
}

func TestAllowanceInSearchRowAndUsageScreenHidesSearch(t *testing.T) {
	m := initialModel(buildItems([]Conversation{{SessionID: "s", Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 160, 40
	res, _ := m.Update(allowanceMsg{account: "me@example.com", limits: []allowanceLimit{
		{Kind: "session", Percent: 14, ResetsAt: time.Now().Add(2 * time.Hour).Format(time.RFC3339)},
		{Kind: "weekly_all", Percent: 22.4},
	}})
	m = res.(model)
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	lines := strings.Split(strip.ReplaceAllString(m.View(), ""), "\n")
	row := lines[1]
	for _, want := range []string{"type to search", "me@example.com", "5h 14%", "ends ", "week 22%", "(1/1)"} {
		if !strings.Contains(row, want) {
			t.Errorf("search row missing %q: %q", want, row)
		}
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = res.(model)
	if v := strip.ReplaceAllString(m.View(), ""); strings.Contains(v, "type to search") || !strings.Contains(v, "tab back") {
		t.Error("the usage screen should replace the search box with a back hint")
	}
	if v := strip.ReplaceAllString(m.View(), ""); strings.Contains(v, "^S msg") || !strings.Contains(v, "tab back  ^O account  ^L changelog  ^C quit") {
		t.Error("the usage screen header should list only its own keys")
	}
}

func TestAllowanceSummaryAsksToOpenClaudeWhenLoggedOut(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.allowanceErr = errAllowanceLogin
	if !strings.Contains(m.allowanceSummary(), "open claude") {
		t.Error("no token should say how to get usage")
	}
}

func TestAccountEmailFromClaudeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"me@example.com"},"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := accountEmail(); got != "me@example.com" {
		t.Errorf("got %q", got)
	}
	alt := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", alt)
	if err := os.WriteFile(filepath.Join(alt, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"other@example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := accountEmail(); got != "other@example.com" {
		t.Errorf("CLAUDE_CONFIG_DIR profile: got %q", got)
	}
}

func TestResampleBucketsFillsAnyWidth(t *testing.T) {
	in := make([]usageBucketTotals, usageBuckets)
	for i := range in {
		in[i].Output = 1
	}
	for _, cols := range []int{20, 72, 144, 200, 400} {
		out := resampleBuckets(in, cols)
		if len(out) != cols {
			t.Errorf("cols %d: got %d columns", cols, len(out))
		}
		var total int64
		for _, b := range out {
			total += b.Output
			if b.Output == 0 {
				t.Errorf("cols %d: gap in the chart", cols)
				break
			}
		}
		if cols <= usageBuckets && total != int64(usageBuckets) {
			t.Errorf("cols %d: downsampling must keep the total, got %d", cols, total)
		}
	}
}

func TestUsageChartsSpanScreenWidth(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.usage = usageData{now: time.Now(), buckets: make([]usageBucketTotals, usageBuckets)}
	for i := range m.usage.buckets {
		m.usage.buckets[i].Output = int64(i + 1)
	}
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	for _, w := range []int{100, 170, 260} {
		m.width, m.height = w, 50
		for _, line := range strings.Split(strip.ReplaceAllString(m.usageView(46), ""), "\n") {
			if strings.Contains(line, "└") {
				if got := utf8.RuneCountInString(line); got != w-1 {
					t.Errorf("width %d: chart baseline is %d wide, want %d", w, got, w-1)
				}
				break
			}
		}
	}
}

func TestUsagePanelsInThreeEqualColumns(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.usage = usageData{now: time.Now(), buckets: make([]usageBucketTotals, usageBuckets)}
	m.usage.buckets[0] = usageBucketTotals{Read: 90, New: 10, Output: 5, Responses: 1}
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	for _, w := range []int{120, 200} {
		m.width = w
		for _, line := range strings.Split(strip.ReplaceAllString(m.usageView(40), ""), "\n") {
			if !strings.Contains(line, "SUMMARY") {
				continue
			}
			s, c, a := strings.Index(line, "SUMMARY"), strings.Index(line, "CACHE MIX"), strings.Index(line, "ALLOWANCE")
			if s < 0 || c < 0 || a < 0 || c-s != a-c {
				t.Errorf("width %d: panels not in equal columns: %q", w, line)
			}
		}
	}
}

func TestCswapUsageLabel(t *testing.T) {
	pct := func(f float64) *float64 { return &f }
	live := &cswapUsage{}
	live.FiveHour = &struct {
		Pct *float64 `json:"pct"`
	}{pct(4)}
	if got := cswapUsageLabel(cswapAccount{Usage: live}); got != "5h 4%" {
		t.Errorf("got %q", got)
	}
	if got := cswapUsageLabel(cswapAccount{UsageStatus: "relogin_required", Usage: live}); got != "re-login" {
		t.Errorf("relogin: got %q", got)
	}
	if got := cswapUsageLabel(cswapAccount{LastGoodUsage: live}); got != "5h 4%" {
		t.Errorf("falls back to the last good reading: got %q", got)
	}
}

func TestAccountSwitcherListsAndSwitches(t *testing.T) {
	calls := fakeCswap(t)
	m := initialModel(nil, "", nil)
	m.width, m.height = 140, 40

	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.acctOpen || len(m.accts) != 3 {
		t.Fatalf("ctrl+o should open and list 3 accounts, open=%v n=%d msg=%q", m.acctOpen, len(m.accts), m.acctMsg)
	}
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	v := strip.ReplaceAllString(m.View(), "")
	for _, want := range []string{"Switch Claude account", "● 1  me@work.example", "5h 21% · 7d 24%", "me@home.example", "5h 4% · 7d 10%", "re-login"} {
		if !strings.Contains(v, want) {
			t.Errorf("popup missing %q", want)
		}
	}

	// The popup owns the keyboard: letters don't reach the search box.
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.textInput.Value() != "" {
		t.Error("typing leaked into the search box")
	}
	// Choosing the active account doesn't run cswap.
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	// Switch with a digit.
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if !strings.Contains(m.acctMsg, "Switched to me@home.example") {
		t.Errorf("status after switch: %q", m.acctMsg)
	}
	// Switch with arrows + Enter, and add.
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("+")})
	got, _ := os.ReadFile(calls)
	want := "list --json\nswitch 2\nlist --json\nswitch 3\nlist --json\nadd\nlist --json\n"
	if string(got) != want {
		t.Errorf("cswap calls:\n%s\nwant:\n%s", got, want)
	}
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.acctOpen {
		t.Error("esc should close the switcher")
	}
}

func TestAccountSwitchFailureIsShown(t *testing.T) {
	fakeCswap(t)
	t.Setenv("CSWAP_FAIL", "1")
	m := initialModel(nil, "", nil)
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyCtrlO})
	m = runAcct(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if !strings.Contains(m.acctMsg, "needs re-login") {
		t.Errorf("failure should show cswap's reason, got %q", m.acctMsg)
	}
}

func TestSwitchRefetchesAllowance(t *testing.T) {
	fakeCswap(t)
	m := initialModel(nil, "", nil)
	m.allowanceAt = time.Now() // fetched recently: normally no refetch for a minute
	m.allowanceLoading = true  // and one is in flight
	res, _ := m.Update(acctActionMsg{what: "switching", done: "Switched"})
	m = res.(model)
	if !m.allowanceStale {
		t.Fatal("an in-flight fetch for the old account should be marked stale")
	}
	res, cmd := m.Update(allowanceMsg{account: "old@example.com"})
	if m = res.(model); cmd == nil || !m.allowanceLoading {
		t.Error("a stale allowance result should trigger a fresh fetch")
	}
}

func TestAccountSwitcherWithoutCswap(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m := initialModel(nil, "", nil)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if m = res.(model); m.acctOpen || cmd != nil || !strings.Contains(m.errorMsg, "install cswap") {
		t.Errorf("without cswap, ctrl+o should explain, got open=%v msg=%q", m.acctOpen, m.errorMsg)
	}
}

func TestClickingAccountEmailOpensSwitcher(t *testing.T) {
	fakeCswap(t)
	m := initialModel(nil, "", nil)
	m.width, m.height = 160, 40
	m.account = "me@work.example"
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	row := strip.ReplaceAllString(strings.Split(m.View(), "\n")[1], "")
	x := strings.Index(row, "me@work.example")
	if x < 0 {
		t.Fatalf("email not in search row: %q", row)
	}
	// Clicking elsewhere on the row does nothing.
	m = runAcct(t, m, tea.MouseMsg{X: 3, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.acctOpen {
		t.Fatal("clicking the search box shouldn't open the switcher")
	}
	m = runAcct(t, m, tea.MouseMsg{X: utf8.RuneCountInString(row[:x]) + 2, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.acctOpen || len(m.accts) != 3 {
		t.Errorf("clicking the email should open and list accounts, open=%v n=%d", m.acctOpen, len(m.accts))
	}
}

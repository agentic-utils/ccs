package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		tag, cur string
		want     bool
	}{
		{"v0.25.0", "0.24.1", true},
		{"v0.24.2", "0.24.1", true},
		{"v1.0.0", "0.99.99", true},
		{"v0.24.1", "0.24.1", false},
		{"v0.9.0", "0.24.1", false}, // numeric, not lexical
		{"v0.25.0", "dev", false},
		{"garbage", "0.24.1", false},
	}
	for _, c := range cases {
		if got := newerVersion(c.tag, c.cur); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.tag, c.cur, got, c.want)
		}
	}
}

func TestUpdatePopupFlow(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.24.1"
	upgraded := false
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.upgrade = &upgrader{install: func(_ string, step func(string)) (string, error) {
		step("brew upgrade")
		upgraded = true
		return "/bin/ccs", nil
	}}

	res, cmd := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	if !m.updateOpen || cmd == nil {
		t.Fatal("newer release should open the popup and schedule the next check")
	}
	if !strings.Contains(m.View(), "v0.25.0 is available") {
		t.Error("popup should render in the view")
	}

	// Keys straight after it opens are swallowed, so typing can't answer it.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); !m.updateOpen || m.updating {
		t.Fatal("enter inside the grace period must be ignored")
	}

	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); !m.updating || cmd == nil {
		t.Fatal("enter should start the upgrade")
	}
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); !upgraded || m.restart != "/bin/ccs" || !m.quitting {
		t.Error("a successful upgrade should quit for restart")
	}
}

func TestUpdatePopupLaterAndFailure(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.24.1"
	m := initialModel(nil, "", nil)
	m.upgrade = &upgrader{install: func(string, func(string)) (string, error) { return "", fmt.Errorf("network down") }}

	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m = res.(model); m.updateOpen || m.quitting {
		t.Fatal("esc should dismiss the popup, not quit ccs")
	}
	res, _ = m.Update(latestMsg{tag: "v0.25.0"})
	if m = res.(model); m.updateOpen {
		t.Error("a dismissed version should not pop up again this session")
	}
	res, _ = m.Update(latestMsg{tag: "v0.26.0"})
	if m = res.(model); !m.updateOpen {
		t.Error("an even newer version should pop up again")
	}

	m.updateShownAt = time.Now().Add(-2 * updateKeyGrace)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); m.restart != "" || !m.updateOpen || !strings.Contains(m.updateErr, "network down") {
		t.Errorf("failed upgrade should reopen the popup with the error, got open=%v err=%q", m.updateOpen, m.updateErr)
	}
	m.width, m.height = 100, 30
	if v := m.View(); !strings.Contains(v, "network down") || !strings.Contains(v, "retry") {
		t.Error("popup should show the failure and offer a retry")
	}
}

func TestUpdatePopupWithoutBrew(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.24.1"
	m := initialModel(nil, "", nil)
	m.width, m.height = 100, 30
	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	if !strings.Contains(m.View(), "package manager") {
		t.Error("non-Homebrew installs should be told to update themselves")
	}
}

func TestReleaseTagFromURL(t *testing.T) {
	if tag, err := releaseTagFromURL("https://github.com/agentic-utils/ccs/releases/tag/v0.24.1"); err != nil || tag != "v0.24.1" {
		t.Errorf("got %q, %v", tag, err)
	}
	for _, bad := range []string{"", "https://github.com/agentic-utils/ccs/releases"} {
		if _, err := releaseTagFromURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestReplaceBinary(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	if _, err := chooseUpgrader(exe).Install("v9.9.9", func(string) {}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	info, _ := os.Stat(exe)
	if string(got) != "new binary" || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("binary not replaced/executable: %q %v", got, info.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".ccs-update-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestReplaceBinaryRejectsBadChecksum(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("evil"), true)
	if _, err := chooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Error("a failed checksum must leave the binary untouched")
	}
}

func TestReplaceBinaryUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	fakeRelease(t, "v9.9.9", []byte("new"), false)
	if _, err := chooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "can't write") {
		t.Fatalf("want a clear permission error, got %v", err)
	}
}

func TestChooseUpgrader(t *testing.T) {
	if chooseUpgrader("/nix/store/abc-ccs/bin/ccs") != nil {
		t.Error("nix store is read-only: notify only")
	}
	if chooseUpgrader(filepath.Join(t.TempDir(), "ccs")) == nil {
		t.Error("a plain install should self-update")
	}
	// A Homebrew keg is never replaced in place: it gets brew or nothing.
	up := chooseUpgrader("/opt/homebrew/Cellar/ccs/0.25.0/bin/ccs")
	if _, err := exec.LookPath("brew"); err != nil && up != nil {
		t.Error("Cellar install without brew on PATH must not self-replace")
	}
}

func TestUpgraderInstallWaitsForPrepare(t *testing.T) {
	release := make(chan struct{})
	var order []string
	var mu sync.Mutex
	log := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	u := &upgrader{
		prepare: func(string) error { <-release; log("prepared"); return nil },
		install: func(string, func(string)) (string, error) { log("installed"); return "", nil },
	}
	u.Prepare("v1")
	u.Prepare("v1") // second call is a no-op
	var steps []string
	done := make(chan struct{})
	go func() { u.Install("v1", func(s string) { steps = append(steps, s) }); close(done) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done
	if strings.Join(order, ",") != "prepared,installed" || len(steps) == 0 || steps[0] != "finishing download" {
		t.Errorf("install must wait for prepare: order=%v steps=%v", order, steps)
	}
}

func TestPreparedReleaseSkipsDownloadOnInstall(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	u := chooseUpgrader(exe)
	u.Prepare("v9.9.9")
	u.mu.Lock()
	done := u.pending["v9.9.9"]
	u.mu.Unlock()
	<-done
	releaseDownloadURL = "http://127.0.0.1:1" // any download after prepare would now fail
	var steps []string
	if _, err := u.Install("v9.9.9", func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatalf("install should use the prepared download: %v (steps %v)", err, steps)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary" {
		t.Errorf("binary not replaced: %q", got)
	}
}

func TestUpdatingHeaderShowsStep(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.width, m.height = 140, 30
	m.updating, m.updateTo = true, "v0.27.0"
	m.progress = &updateProgress{step: "brew upgrade", started: time.Now().Add(-5 * time.Second)}
	if v := m.View(); !strings.Contains(v, "updating to v0.27.0: brew upgrade (5s)") {
		t.Errorf("header should show step and elapsed time")
	}
}

func TestUpdatePopupWaitsBehindPrompts(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.27.1"
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s", Title: "old"}}}, "", nil)
	m.width, m.height = 100, 30
	m.upgrade = &upgrader{install: func(string, func(string)) (string, error) { return "", nil }}
	m.renaming = true
	m.renameInput = textinput.New()
	m.renameInput.Focus()

	res, _ := m.Update(latestMsg{tag: "v0.27.2"})
	m = res.(model)
	m.updateShownAt = time.Now().Add(-time.Hour) // grace long gone
	if strings.Contains(m.View(), "is available") {
		t.Error("popup must not show over the rename prompt")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m = res.(model); !strings.HasSuffix(m.renameInput.Value(), "x") {
		t.Error("typing should reach the rename input, not the popup")
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m = res.(model); m.renaming || !m.updateOpen {
		t.Fatal("esc should cancel the rename and leave the update pending")
	}
	if !strings.Contains(m.View(), "is available") {
		t.Error("popup should show once the prompt closes")
	}
	// First key after it appears is swallowed by a fresh grace period.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m = res.(model); m.updating {
		t.Error("enter straight after the prompt closed must not start the update")
	}
}

func TestUpdateCheckFailureShowsInHeader(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.width, m.height = 160, 30
	res, _ := m.Update(latestMsg{err: fmt.Errorf("dns")})
	if m = res.(model); !strings.Contains(m.View(), "update check failed") {
		t.Error("a failed update check should be visible")
	}
	res, _ = m.Update(latestMsg{tag: "v0.0.1"})
	if m = res.(model); strings.Contains(m.View(), "update check failed") {
		t.Error("a later successful check clears it")
	}
}

func TestQuitBlockedDuringUpdate(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.updating = true
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m = res.(model); m.quitting || cmd != nil || m.errorMsg == "" {
		t.Error("ctrl+c during an update must not quit")
	}
}

func TestUpdateLogRecordsCommandsAndDownloads(t *testing.T) {
	old := updateLogPath
	updateLogPath = filepath.Join(t.TempDir(), "logs", "update.log")
	defer func() { updateLogPath = old }()

	if _, err := runCommand(5*time.Second, nil, true, "sh", "-c", "echo downloading; echo boom >&2; exit 3"); err == nil {
		t.Fatal("want failure")
	}
	fakeRelease(t, "v9.9.9", []byte("bin"), false)
	if _, err := download(releaseDownloadURL + "/v9.9.9/checksums.txt"); err != nil {
		t.Fatal(err)
	}
	logUpdate("install v9.9.9: %v in %s", errOrOK(nil), time.Second)

	got, err := os.ReadFile(updateLogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"$ sh -c", "exit status 3", "  | downloading", "  | boom", "GET http://", "last connection 127.0.0.1:", "install v9.9.9: ok"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

func TestFailedUpdatePopupPointsAtLog(t *testing.T) {
	m := initialModel(nil, "", nil)
	m.width, m.height = 120, 30
	m.updateTo, m.updateErr, m.updateOpen = "v9.9.9", "brew upgrade: curl: (35) Recv failure", true
	m.upgrade = &upgrader{install: func(string, func(string)) (string, error) { return "", nil }}
	if !strings.Contains(m.View(), updateLogPath) {
		t.Error("a failed update should point at the update log")
	}
}

func TestDownloadFallsBackPastStalledAddress(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "payload") }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	// An address that accepts TCP but never answers the TLS handshake.
	stall, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stall.Close()
	go func() {
		for {
			c, err := stall.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	oldResolve, oldDial, oldTLS, oldTimeout := resolveHost, dialAddr, downloadTLSConfig, addrAttemptTimeout
	defer func() {
		resolveHost, dialAddr, downloadTLSConfig, addrAttemptTimeout = oldResolve, oldDial, oldTLS, oldTimeout
	}()
	failedAddrs.Range(func(k, _ any) bool { failedAddrs.Delete(k); return true })
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	downloadTLSConfig = &tls.Config{RootCAs: pool}
	addrAttemptTimeout = 300 * time.Millisecond
	resolveHost = func(context.Context, string) ([]string, error) { return []string{"192.0.2.1", "192.0.2.2"}, nil }
	var tried []string
	dialAddr = func(ctx context.Context, network, addr string) (net.Conn, error) {
		tried = append(tried, addr)
		if strings.HasPrefix(addr, "192.0.2.1:") {
			return (&net.Dialer{}).DialContext(ctx, network, stall.Addr().String())
		}
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port)
	}
	downloadTransport.CloseIdleConnections()

	start := time.Now()
	body, err := download("https://example.com:" + port + "/x")
	if err != nil || string(body) != "payload" {
		t.Fatalf("download = %q, %v", body, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("fallback took %v; the stalled address should cost ~one attempt limit", d)
	}
	if _, bad := failedAddrs.Load("192.0.2.1"); !bad {
		t.Error("the stalled address should be remembered as failed")
	}
	// Next time the failed address goes last.
	tried = nil
	downloadTransport.CloseIdleConnections()
	if _, err := download("https://example.com:" + port + "/x"); err != nil {
		t.Fatal(err)
	}
	if len(tried) == 0 || !strings.HasPrefix(tried[0], "192.0.2.2:") {
		t.Errorf("known-bad address should be tried last, got %v", tried)
	}
}

func TestSeedFile(t *testing.T) {
	fakeRelease(t, "v1.0.0", []byte("bin"), false)
	url := releaseDownloadURL + "/v1.0.0/checksums.txt"
	body, err := download(url)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256Hex(body)
	path := filepath.Join(t.TempDir(), "cached.tar.gz")

	if err := seedFile(path, url, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); sha256Hex(got) != want {
		t.Fatal("seeded file has the wrong content")
	}
	// Already valid: no download (an unreachable URL proves it isn't fetched).
	if err := seedFile(path, "http://127.0.0.1:1/unreachable", want); err != nil {
		t.Errorf("valid cache should be kept without downloading: %v", err)
	}
	// Checksum mismatch: error, and nothing replaces the cache file.
	other := filepath.Join(t.TempDir(), "other.tar.gz")
	if err := seedFile(other, url, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("want checksum mismatch, got %v", err)
	}
	if fileExists(other) {
		t.Error("a mismatched download must not be written to the cache")
	}
}

func TestUpdatePopupOverlaysEveryScreen(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.36.0"
	m := initialModel(buildItems([]Conversation{{SessionID: "s", Title: "Some session", Messages: []Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 140, 40
	m.upgrade = &upgrader{install: func(string, func(string)) (string, error) { return "", nil }}
	m.usage = usageData{now: time.Now(), buckets: make([]usageBucketTotals, usageBuckets)}
	res, _ := m.Update(latestMsg{tag: "v0.37.0"})
	m = res.(model)
	strip := regexp.MustCompile("\033\\[[0-9;]*m")
	for _, usage := range []bool{false, true} {
		m.showUsage = usage
		v := strip.ReplaceAllString(m.View(), "")
		if !strings.Contains(v, "v0.37.0 is available") {
			t.Errorf("usage=%v: popup not shown", usage)
		}
		if !strings.Contains(v, "claude code search") {
			t.Errorf("usage=%v: the screen behind the popup should stay visible", usage)
		}
		for i, line := range strings.Split(v, "\n") {
			if !strings.ContainsAny(line, "│╭╰") { // only the rows the popup covers
				continue
			}
			if w := utf8.RuneCountInString(line); w > m.width {
				t.Errorf("usage=%v: line %d is %d wide, over the %d-column screen", usage, i, w, m.width)
			}
		}
	}
}

func TestUIStateSurvivesRestart(t *testing.T) {
	mk := func(id, text string) Conversation {
		c := Conversation{SessionID: id, LastTimestamp: "2026-09-29T10:00:00Z"}
		for i := 0; i < 50; i++ {
			c.Messages = append(c.Messages, Message{Role: "user", Text: text + fmt.Sprint(i)})
		}
		return c
	}
	before := initialModel(buildItems([]Conversation{mk("a", "apple"), mk("b", "banana"), mk("c", "bandana")}), "", nil)
	before.width, before.height = 120, 40
	before.textInput.SetValue("band")
	before.textInput.SetCursor(2)
	before.updateFilter()
	before.cursor = 0 // "band" matches only "c" (bandana)
	before.previewScroll = 7
	before.showUsage = true
	saved, err := json.Marshal(before.uiState())
	if err != nil {
		t.Fatal(err)
	}

	// The new version's list comes back in a different order.
	after := initialModel(buildItems([]Conversation{mk("c", "bandana"), mk("b", "banana"), mk("a", "apple")}), "", nil)
	var state uiState
	if err := json.Unmarshal(saved, &state); err != nil {
		t.Fatal(err)
	}
	after.restore(state)
	if after.textInput.Value() != "band" || after.textInput.Position() != 2 {
		t.Errorf("query/cursor = %q/%d", after.textInput.Value(), after.textInput.Position())
	}
	if len(after.filtered) != 1 || after.filtered[after.cursor].conv.SessionID != "c" {
		t.Errorf("selection not restored: %+v", after.filtered)
	}
	if after.previewScroll != 7 || !after.showUsage || after.screenName() != "usage" {
		t.Errorf("scroll=%d usage=%v", after.previewScroll, after.showUsage)
	}
	if after.Init() == nil {
		t.Error("restored onto the usage screen, Init should load the usage data")
	}

	// A conversation that no longer exists is skipped, not an error.
	gone := initialModel(buildItems([]Conversation{mk("z", "zebra")}), "", nil)
	gone.restore(uiState{Screen: "list", Selected: "missing"})
	if gone.cursor != 0 || gone.showUsage {
		t.Error("restoring a missing selection should leave a sane default")
	}
}

func TestParseChangelog(t *testing.T) {
	feed := `<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom">
<entry><title>v0.44.0</title><content type="html">&lt;ul&gt;&lt;li&gt;feat: not yet offered&lt;/li&gt;&lt;/ul&gt;</content></entry>
<entry><title>v0.43.1</title><content type="html">&lt;h2&gt;Changelog&lt;/h2&gt;&lt;ul&gt;
&lt;li&gt;&lt;a href=&quot;x&quot;&gt;&lt;tt&gt;4cde904&lt;/tt&gt;&lt;/a&gt; Merge pull request &lt;a&gt;#66&lt;/a&gt; from x&lt;/li&gt;
&lt;li&gt;&lt;a href=&quot;x&quot;&gt;&lt;tt&gt;7d1e6c2&lt;/tt&gt;&lt;/a&gt; fix: clearer &amp;quot;status&amp;quot; line&lt;/li&gt;&lt;/ul&gt;</content></entry>
<entry><title>v0.43.0</title><content type="html">&lt;ul&gt;&lt;li&gt;feat: readable preview&lt;/li&gt;&lt;/ul&gt;</content></entry>
<entry><title>v0.42.0</title><content type="html">&lt;ul&gt;&lt;li&gt;feat: already installed&lt;/li&gt;&lt;/ul&gt;</content></entry>
</feed>`
	got, err := parseChangelog(strings.NewReader(feed), "0.42.0", "v0.43.1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v0.43.1", `  • fix: clearer "status" line`, "v0.43.0", "  • feat: readable preview"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestUpdatePopupShowsChangelog(t *testing.T) {
	old := version
	version = "0.24.1"
	defer func() { version = old }()
	m := model{width: 100, fetchChangelog: func(tag string) ([]string, error) {
		lines := []string{tag}
		for i := range 20 {
			lines = append(lines, fmt.Sprintf("  fix: change %d", i))
		}
		return lines, nil
	}}
	nm, cmd := m.Update(latestMsg{tag: "v9.0.0"})
	m = nm.(model)
	if cmd == nil {
		t.Fatal("expected a changelog fetch")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("expected the changelog fetch alongside the next check")
	}
	nm, _ = m.Update(batch[0]()) // the fetch; batch[1] is the 2-minute tick
	m = nm.(model)
	v := strip2(m.updatePopup())
	if !strings.Contains(v, "fix: change 0") || !strings.Contains(v, "… and 10 more") || strings.Contains(v, "change 19") {
		t.Errorf("popup should list the changes, capped:\n%s", v)
	}
}

func TestChangelogPopup(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "0.2.0"
	var lines []string
	for v := 30; v > 0; v-- {
		lines = append(lines, fmt.Sprintf("v0.%d.0", v), fmt.Sprintf("  feat: thing %d", v))
	}
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.fetchNotes = func() ([]string, error) { return lines, nil }
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyCtrlL})
	if !m.notesOpen || cmd == nil {
		t.Fatal("Ctrl+L should open the changelog and fetch it")
	}
	if v := strip2(m.View()); !strings.Contains(v, "fetching…") {
		t.Errorf("should say it's fetching:\n%s", v)
	}
	nm, _ := m.Update(cmd())
	m = nm.(model)
	v := strip2(m.View())
	if !strings.Contains(v, "v0.30.0 (new)") || !strings.Contains(v, "feat: thing 30") || strings.Contains(v, "thing 1\n") {
		t.Errorf("should show the newest releases first, marked:\n%s", v)
	}
	for range 100 {
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if want := len(lines) - m.notesRows(); m.notesScroll != want {
		t.Errorf("scroll should stop at the end: %d, want %d", m.notesScroll, want)
	}
	if v := strip2(m.View()); !strings.Contains(v, "v0.2.0 (installed)") || !strings.Contains(v, "feat: thing 1") {
		t.Errorf("scrolled to the end should show the oldest, installed marked:\n%s", v)
	}
	nm, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if nm.(model).notesScroll != m.notesScroll-3 {
		t.Error("the wheel should scroll the changelog")
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.notesOpen {
		t.Error("esc should close it")
	}
	if m, cmd = key(m, tea.KeyMsg{Type: tea.KeyCtrlL}); cmd != nil || !m.notesOpen || m.notesScroll != 0 {
		t.Error("reopening should reuse the fetched changelog, from the top")
	}
}

func TestChangelogRetry(t *testing.T) {
	calls := 0
	m := initialModel([]listItem{{conv: Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.fetchNotes = func() ([]string, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("timed out")
		}
		return []string{"v1.0.0", "  • fix: x"}, nil
	}
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyCtrlL})
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlL}) // close
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlL}) // reopen while the fetch is in flight
	nm, _ := m.Update(cmd())
	m = nm.(model)
	if calls != 1 {
		t.Errorf("reopening while a fetch is in flight shouldn't start another: %d fetches", calls)
	}
	if v := strip2(m.View()); !strings.Contains(v, "couldn't fetch the changelog: timed out") || !strings.Contains(v, "enter retry") {
		t.Errorf("should say it failed and offer a retry:\n%s", v)
	}
	m, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should retry")
	}
	nm, _ = m.Update(cmd())
	if v := strip2(nm.(model).View()); !strings.Contains(v, "fix: x") {
		t.Errorf("retry should show the changelog:\n%s", v)
	}
}

func TestParseAPIChangelog(t *testing.T) {
	body := `[{"tag_name":"v0.3.0","body":"## Changelog\n*  fix: newer\n"},
{"tag_name":"v0.2.1","body":"## Changelog\n* 35cdc1b016d93b529a853c93a980acb55be9c985 chore: rename org\n* 4cde904 Merge pull request #1 from x\n\n"},
{"tag_name":"v0.2.0","body":"## Changelog\n* f15ec5c Add GoReleaser config\n"}]`
	got, err := parseAPIChangelog(strings.NewReader(body), "0.0.0", "v0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v0.2.1", "  • chore: rename org", "v0.2.0", "  • Add GoReleaser config"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestChangelogScrollHintOnlyWhenScrollable(t *testing.T) {
	m := model{width: 100, height: 30, notesOpen: true, notes: []string{"v1.0.0", "  • fix: x"}}
	if v := strip2(m.notesPopup()); strings.Contains(v, "scroll") {
		t.Errorf("a changelog that fits shouldn't offer scrolling:\n%s", v)
	}
	for range 40 {
		m.notes = append(m.notes, "  • more")
	}
	if v := strip2(m.notesPopup()); !strings.Contains(v, "↑↓ scroll") {
		t.Errorf("a long changelog should offer scrolling:\n%s", v)
	}
}

func TestChangelogWidthSteady(t *testing.T) {
	m := model{width: 120, height: 20, notesOpen: true}
	for i := range 30 {
		m.notes = append(m.notes, fmt.Sprintf("v0.%d.0", i), "  • "+strings.Repeat("x", i*3))
	}
	width := func() int { return ansi.StringWidth(strings.Split(m.notesPopup(), "\n")[0]) }
	w := width()
	for range 40 {
		m.scrollNotes(1)
		if got := width(); got != w {
			t.Fatalf("popup width changed while scrolling: %d then %d", w, got)
		}
	}
}

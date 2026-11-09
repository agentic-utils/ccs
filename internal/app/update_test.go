package app

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/selfupdate"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func TestUpdatePopupFlow(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	upgraded := false
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(_ string, step func(string)) (string, error) {
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
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	m := initialModel(nil, "", nil)
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { return "", fmt.Errorf("network down") }}

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
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	m := initialModel(nil, "", nil)
	m.width, m.height = 100, 30
	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	if !strings.Contains(m.View(), "package manager") {
		t.Error("non-Homebrew installs should be told to update themselves")
	}
}

func TestReplaceBinary(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	if _, err := selfupdate.ChooseUpgrader(exe).Install("v9.9.9", func(string) {}); err != nil {
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
	if _, err := selfupdate.ChooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Error("a failed checksum must leave the binary untouched")
	}
}

func TestSignedSums(t *testing.T) {
	fakeRelease(t, "v9.9.9", []byte("bin"), false)
	if _, err := selfupdate.SignedSums("v9.9.9"); err != nil {
		t.Fatalf("a release CI signed should pass: %v", err)
	}

	// Signed for another tag, by another key, or no signature at all: refused.
	sums := []byte("abc  ccs_x.tar.gz\n")
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	cases := map[string][]byte{
		"other tag": ed25519.Sign(testReleaseKey, selfupdate.SignedMessage("v1.0.0", sums)),
		"other key": ed25519.Sign(other, selfupdate.SignedMessage("v2.0.0", sums)),
		"junk":      []byte("not a signature"),
	}
	for name, sig := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ".sig") {
				w.Write(sig)
				return
			}
			w.Write(sums)
		}))
		selfupdate.ReleaseDownloadURL = srv.URL
		if _, err := selfupdate.SignedSums("v2.0.0"); err == nil || !strings.Contains(err.Error(), "isn't signed by ccs's CI") {
			t.Errorf("%s: want a signature error, got %v", name, err)
		}
		srv.Close()
	}
	selfupdate.ReleaseDownloadURL = "http://127.0.0.1:1"
	if _, err := selfupdate.SignedSums("v2.0.0"); err == nil {
		t.Error("no signature to download must fail")
	}
}

func TestReplaceBinaryRejectsUnsignedRelease(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("evil"), false)
	defer func(k ed25519.PublicKey) { selfupdate.ReleaseKey = k }(selfupdate.ReleaseKey)
	selfupdate.ReleaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if _, err := selfupdate.ChooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "isn't signed") {
		t.Fatalf("want a signature error, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Error("an unsigned release must leave the binary untouched")
	}
}

// TestSignReleaseScript checks CI's signing script and ccs agree on what's signed.
func TestSignReleaseScript(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl")
	}
	dir := t.TempDir()
	der, err := x509.MarshalPKCS8PrivateKey(testReleaseKey)
	if err != nil {
		t.Fatal(err)
	}
	sums := filepath.Join(dir, "checksums.txt")
	os.WriteFile(sums, []byte("abc  ccs_linux_amd64.tar.gz\n"), 0o644)
	cmd := exec.Command(".github/sign-release.sh", "v3.2.1", sums, sums+".sig")
	cmd.Env = append(os.Environ(), "RELEASE_SIGNING_KEY="+string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("openssl can't sign Ed25519 here: %v %s", err, out)
	}
	body, _ := os.ReadFile(sums)
	sig, _ := os.ReadFile(sums + ".sig")
	if !ed25519.Verify(testReleaseKey.Public().(ed25519.PublicKey), selfupdate.SignedMessage("v3.2.1", body), sig) {
		t.Error("ccs must accept what sign-release.sh signs")
	}

	cmd = exec.Command(".github/sign-release.sh", "v3.2.1", sums, sums+".sig")
	cmd.Env = append(os.Environ(), "RELEASE_SIGNING_KEY=")
	if err := cmd.Run(); err == nil {
		t.Error("signing without a key must fail the release")
	}
}

// TestCheckFormula: brew installs only a formula whose archive CI signed.
func TestCheckFormula(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "1.0.0"
	fakeRelease(t, "v9.9.9", []byte("bin"), false)
	sums, err := selfupdate.SignedSums("v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	asset := fmt.Sprintf("ccs_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	good := selfupdate.ChecksumFor(string(sums), asset)
	brew := filepath.Join(t.TempDir(), "brew")
	formula := func(url, sum string) {
		info := fmt.Sprintf(`{"formulae":[{"urls":{"stable":{"url":%q,"checksum":%q}}}]}`, url, sum)
		os.WriteFile(brew, []byte("#!/bin/sh\necho '"+info+"'\n"), 0o755)
	}
	cases := []struct {
		name, url, sum, want string
	}{
		{"signed", selfupdate.ReleaseDownloadURL + "/v9.9.9/" + asset, good, ""},
		{"other checksum", selfupdate.ReleaseDownloadURL + "/v9.9.9/" + asset, strings.Repeat("0", 64), "isn't the one"},
		{"not newer", selfupdate.ReleaseDownloadURL + "/v0.9.0/" + asset, good, "not newer"},
		{"elsewhere", "https://example.com/v9.9.9/" + asset, good, "not a ccs release"},
	}
	for _, c := range cases {
		formula(c.url, c.sum)
		err := selfupdate.CheckFormula(brew)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
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
	if _, err := selfupdate.ChooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "can't write") {
		t.Fatalf("want a clear permission error, got %v", err)
	}
}

func TestPreparedReleaseSkipsDownloadOnInstall(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	u := selfupdate.ChooseUpgrader(exe)
	u.Prepare("v9.9.9")
	u.Mu.Lock()
	done := u.Pending["v9.9.9"]
	u.Mu.Unlock()
	<-done
	selfupdate.ReleaseDownloadURL = "http://127.0.0.1:1" // any download after prepare would now fail
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
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.27.1"
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s", Title: "old"}}}, "", nil)
	m.width, m.height = 100, 30
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { return "", nil }}
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
	old := state.UpdateLogPath
	state.UpdateLogPath = filepath.Join(t.TempDir(), "logs", "update.log")
	defer func() { state.UpdateLogPath = old }()

	if _, err := proc.RunCommand(5*time.Second, nil, true, "sh", "-c", "echo downloading; echo boom >&2; exit 3"); err == nil {
		t.Fatal("want failure")
	}
	fakeRelease(t, "v9.9.9", []byte("bin"), false)
	if _, err := selfupdate.Download(selfupdate.ReleaseDownloadURL + "/v9.9.9/checksums.txt"); err != nil {
		t.Fatal(err)
	}
	state.LogUpdate("install v9.9.9: %v in %s", state.ErrOrOK(nil), time.Second)

	got, err := os.ReadFile(state.UpdateLogPath)
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
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { return "", nil }}
	if !strings.Contains(m.View(), state.UpdateLogPath) {
		t.Error("a failed update should point at the update log")
	}
}

func TestSeedFile(t *testing.T) {
	fakeRelease(t, "v1.0.0", []byte("bin"), false)
	url := selfupdate.ReleaseDownloadURL + "/v1.0.0/checksums.txt"
	body, err := selfupdate.Download(url)
	if err != nil {
		t.Fatal(err)
	}
	want := selfupdate.Sha256Hex(body)
	path := filepath.Join(t.TempDir(), "cached.tar.gz")

	if err := selfupdate.SeedFile(path, url, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); selfupdate.Sha256Hex(got) != want {
		t.Fatal("seeded file has the wrong content")
	}
	// Already valid: no download (an unreachable URL proves it isn't fetched).
	if err := selfupdate.SeedFile(path, "http://127.0.0.1:1/unreachable", want); err != nil {
		t.Errorf("valid cache should be kept without downloading: %v", err)
	}
	// Checksum mismatch: error, and nothing replaces the cache file.
	other := filepath.Join(t.TempDir(), "other.tar.gz")
	if err := selfupdate.SeedFile(other, url, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("want checksum mismatch, got %v", err)
	}
	if selfupdate.FileExists(other) {
		t.Error("a mismatched download must not be written to the cache")
	}
}

func TestUpdatePopupOverlaysEveryScreen(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.36.0"
	m := initialModel(buildItems([]transcript.Conversation{{SessionID: "s", Title: "Some session", Messages: []transcript.Message{{Role: "user", Text: "x"}}}}), "", nil)
	m.width, m.height = 140, 40
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { return "", nil }}
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
		if !strings.Contains(v, "coding cli sessions") {
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
	mk := func(id, text string) transcript.Conversation {
		c := transcript.Conversation{SessionID: id, LastTimestamp: "2026-09-29T10:00:00Z"}
		for i := 0; i < 50; i++ {
			c.Messages = append(c.Messages, transcript.Message{Role: "user", Text: text + fmt.Sprint(i)})
		}
		return c
	}
	before := initialModel(buildItems([]transcript.Conversation{mk("a", "apple"), mk("b", "banana"), mk("c", "bandana")}), "", nil)
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
	after := initialModel(buildItems([]transcript.Conversation{mk("c", "bandana"), mk("b", "banana"), mk("a", "apple")}), "", nil)
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
	gone := initialModel(buildItems([]transcript.Conversation{mk("z", "zebra")}), "", nil)
	gone.restore(uiState{Screen: "list", Selected: "missing"})
	if gone.cursor != 0 || gone.showUsage {
		t.Error("restoring a missing selection should leave a sane default")
	}
}

func TestUpdatePopupShowsChangelog(t *testing.T) {
	old := state.Version
	state.Version = "0.24.1"
	defer func() { state.Version = old }()
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
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.2.0"
	var lines []string
	for v := 30; v > 0; v-- {
		lines = append(lines, fmt.Sprintf("v0.%d.0", v), fmt.Sprintf("  feat: thing %d", v))
	}
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
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
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
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

func TestAutoUpdateInstallsWhileAway(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	installs := 0
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.autoUpdate = true
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { installs++; return "/bin/ccs", nil }}

	res, cmd := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	if m.updateOpen || cmd == nil || m.updateTo != "v0.25.0" {
		t.Fatal("with auto-update on, a new version is prepared without an offer")
	}

	m.lastKeyAt = time.Now() // typing just now
	res, cmd = m.Update(autoUpdateTickMsg{})
	if m = res.(model); m.updating || cmd == nil {
		t.Fatal("not while you're at it: check again later")
	}

	m.blurred = true
	m.chatDrafts = map[string]string{"s": "half a thought"}
	res, _ = m.Update(autoUpdateTickMsg{})
	if m = res.(model); m.updating {
		t.Fatal("never while a message box holds unsent text")
	}
	m.chatDrafts = nil

	res, cmd = m.Update(autoUpdateTickMsg{})
	if m = res.(model); !m.updating || !m.autoInstalling {
		t.Fatal("away and nothing to lose: install")
	}
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); installs != 1 || m.restart != "/bin/ccs" {
		t.Error("installed and restarting")
	}
}

func TestAutoUpdateFailureIsOffered(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	m := initialModel(nil, "", nil)
	m.autoUpdate, m.blurred = true, true
	m.upgrade = &selfupdate.Upgrader{InstallFunc: func(string, func(string)) (string, error) { return "", fmt.Errorf("network down") }}
	res, _ := m.Update(latestMsg{tag: "v0.25.0"})
	m = res.(model)
	res, cmd := m.Update(autoUpdateTickMsg{})
	m = res.(model)
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); !m.updateOpen || m.autoFailed != "v0.25.0" {
		t.Fatal("a failed automatic install shows the offer with the error")
	}
	res, _ = m.Update(latestMsg{tag: "v0.25.0"})
	if m = res.(model); m.updating {
		t.Error("and isn't retried on its own")
	}
}

func TestUpdatedPopupAfterRestart(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.25.0"
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 100, 30
	m.updatedFrom, m.updatedOpen = "0.24.1", true
	res, _ := m.Update(updatedChangelogMsg{[]string{"v0.25.0", "  feat: something new"}})
	m = res.(model)
	if v := strip2(m.View()); !strings.Contains(v, "ccs updated itself from v0.24.1 to v0.25.0") || !strings.Contains(v, "something new") {
		t.Errorf("says it updated, with what changed:\n%s", v)
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m = res.(model); m.updatedOpen {
		t.Error("esc closes it")
	}
}

func TestAutoUpdateToggle(t *testing.T) {
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.autoUpdate = true
	m, _ = chord(m, "i")
	if m.autoUpdate || loadSettings().autoUpdateOn() {
		t.Error("^X I turns it off, and that's saved")
	}
	m, _ = chord(m, "i")
	if !m.autoUpdate || !loadSettings().autoUpdateOn() {
		t.Error("again turns it back on")
	}
}

func TestHeaderShowsAutoUpdate(t *testing.T) {
	m := initialModel([]listItem{{conv: transcript.Conversation{SessionID: "s"}}}, "", nil)
	m.width, m.height = 200, 30
	m.checkLatest = func() (string, error) { return "", nil }
	m.autoUpdate = true
	if v := strip2(m.View()); !strings.Contains(v, "^X I auto-update on") {
		t.Errorf("the header says auto-update is on:\n%s", v)
	}
	m.autoUpdate = false
	if v := strip2(m.View()); !strings.Contains(v, "^X I auto-update off") {
		t.Errorf("or off:\n%s", v)
	}
}

func TestAutoUpdateOptIn(t *testing.T) {
	defer func(p string) { state.SettingsPath = p }(state.SettingsPath)
	state.SettingsPath = t.TempDir() + "/settings.json"
	for _, c := range []struct {
		name, file string
		missing    bool
		want       bool
	}{
		{"new install, no file", "", true, false},
		{"older install, never turned off", `{"auto_focus":true}`, false, true},
		{"older install, turned off", `{"manual_update":true}`, false, false},
		{"chose on", `{"auto_update":true}`, false, true},
		{"chose off", `{"auto_update":false}`, false, false},
	} {
		os.Remove(state.SettingsPath)
		if !c.missing {
			os.WriteFile(state.SettingsPath, []byte(c.file), 0o600)
		}
		if got := loadSettings().autoUpdateOn(); got != c.want {
			t.Errorf("%s: auto-update %v, want %v", c.name, got, c.want)
		}
	}
	on := true
	saveSettings(settings{AutoUpdate: &on})
	if b, _ := os.ReadFile(state.SettingsPath); !strings.Contains(string(b), `"auto_update":true`) {
		t.Errorf("the choice is saved explicitly: %s", b)
	}
	off := false
	saveSettings(settings{AutoUpdate: &off})
	if b, _ := os.ReadFile(state.SettingsPath); !strings.Contains(string(b), `"auto_update":false`) {
		t.Errorf("off is saved too, so it isn't read as an older install: %s", b)
	}
}

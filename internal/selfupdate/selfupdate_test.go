package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
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
		if got := NewerVersion(c.tag, c.cur); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.tag, c.cur, got, c.want)
		}
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

func TestChooseUpgrader(t *testing.T) {
	if ChooseUpgrader("/nix/store/abc-ccs/bin/ccs") != nil {
		t.Error("nix store is read-only: notify only")
	}
	if ChooseUpgrader(filepath.Join(t.TempDir(), "ccs")) == nil {
		t.Error("a plain install should self-update")
	}
	// A Homebrew keg is never replaced in place: it gets brew or nothing.
	up := ChooseUpgrader("/opt/homebrew/Cellar/ccs/0.25.0/bin/ccs")
	if _, err := exec.LookPath("brew"); err != nil && up != nil {
		t.Error("Cellar install without brew on PATH must not self-replace")
	}
}

func TestUpgraderInstallWaitsForPrepare(t *testing.T) {
	release := make(chan struct{})
	var order []string
	var mu sync.Mutex
	log := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	u := &Upgrader{
		prepare:     func(string) error { <-release; log("prepared"); return nil },
		InstallFunc: func(string, func(string)) (string, error) { log("installed"); return "", nil },
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
	body, err := Download("https://example.com:" + port + "/x")
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
	if _, err := Download("https://example.com:" + port + "/x"); err != nil {
		t.Fatal(err)
	}
	if len(tried) == 0 || !strings.HasPrefix(tried[0], "192.0.2.2:") {
		t.Errorf("known-bad address should be tried last, got %v", tried)
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

// testReleaseKey stands in for CI's signing key: TestMain makes ccs trust it.
var testReleaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

// fakeRelease serves tag's checksums.txt, signed with testReleaseKey, and this
// platform's archive holding binary as "ccs"; tamper corrupts the published
// checksum, re-signed, as a release built outside CI can't be.
func fakeRelease(t *testing.T, tag string, binary []byte, tamper bool) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{"README.md": []byte("readme"), "ccs": binary} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	gz.Close()
	asset := fmt.Sprintf("ccs_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(buf.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	if tamper {
		sums = strings.Repeat("0", 64) + "  " + asset + "\n"
	}
	sig := ed25519.Sign(testReleaseKey, SignedMessage(tag, []byte(sums)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tag + "/checksums.txt":
			io.WriteString(w, sums)
		case "/" + tag + "/checksums.txt.sig":
			w.Write(sig)
		case "/" + tag + "/" + asset:
			w.Write(buf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := ReleaseDownloadURL
	ReleaseDownloadURL = srv.URL
	t.Cleanup(func() { ReleaseDownloadURL = old })
}

func TestReplaceBinary(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	if _, err := ChooseUpgrader(exe).Install("v9.9.9", func(string) {}); err != nil {
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
	if _, err := ChooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Error("a failed checksum must leave the binary untouched")
	}
}

func TestSignedSums(t *testing.T) {
	fakeRelease(t, "v9.9.9", []byte("bin"), false)
	if _, err := SignedSums("v9.9.9"); err != nil {
		t.Fatalf("a release CI signed should pass: %v", err)
	}

	// Signed for another tag, by another key, or no signature at all: refused.
	sums := []byte("abc  ccs_x.tar.gz\n")
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	cases := map[string][]byte{
		"other tag": ed25519.Sign(testReleaseKey, SignedMessage("v1.0.0", sums)),
		"other key": ed25519.Sign(other, SignedMessage("v2.0.0", sums)),
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
		ReleaseDownloadURL = srv.URL
		if _, err := SignedSums("v2.0.0"); err == nil || !strings.Contains(err.Error(), "isn't signed by ccs's CI") {
			t.Errorf("%s: want a signature error, got %v", name, err)
		}
		srv.Close()
	}
	ReleaseDownloadURL = "http://127.0.0.1:1"
	if _, err := SignedSums("v2.0.0"); err == nil {
		t.Error("no signature to download must fail")
	}
}

func TestReplaceBinaryRejectsUnsignedRelease(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("evil"), false)
	defer func(k ed25519.PublicKey) { ReleaseKey = k }(ReleaseKey)
	ReleaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if _, err := ChooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "isn't signed") {
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
	if !ed25519.Verify(testReleaseKey.Public().(ed25519.PublicKey), SignedMessage("v3.2.1", body), sig) {
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
	sums, err := SignedSums("v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	asset := fmt.Sprintf("ccs_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	good := ChecksumFor(string(sums), asset)
	brew := filepath.Join(t.TempDir(), "brew")
	formula := func(url, sum string) {
		info := fmt.Sprintf(`{"formulae":[{"urls":{"stable":{"url":%q,"checksum":%q}}}]}`, url, sum)
		os.WriteFile(brew, []byte("#!/bin/sh\necho '"+info+"'\n"), 0o755)
	}
	cases := []struct {
		name, url, sum, want string
	}{
		{"signed", ReleaseDownloadURL + "/v9.9.9/" + asset, good, ""},
		{"other checksum", ReleaseDownloadURL + "/v9.9.9/" + asset, strings.Repeat("0", 64), "isn't the one"},
		{"not newer", ReleaseDownloadURL + "/v0.9.0/" + asset, good, "not newer"},
		{"elsewhere", "https://example.com/v9.9.9/" + asset, good, "not a ccs release"},
	}
	for _, c := range cases {
		formula(c.url, c.sum)
		err := CheckFormula(brew)
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
	if _, err := ChooseUpgrader(exe).Install("v9.9.9", func(string) {}); err == nil || !strings.Contains(err.Error(), "can't write") {
		t.Fatalf("want a clear permission error, got %v", err)
	}
}

func TestPreparedReleaseSkipsDownloadOnInstall(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ccs")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRelease(t, "v9.9.9", []byte("new binary"), false)
	u := ChooseUpgrader(exe)
	u.Prepare("v9.9.9")
	u.Mu.Lock()
	done := u.Pending["v9.9.9"]
	u.Mu.Unlock()
	<-done
	ReleaseDownloadURL = "http://127.0.0.1:1" // any download after prepare would now fail
	var steps []string
	if _, err := u.Install("v9.9.9", func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatalf("install should use the prepared download: %v (steps %v)", err, steps)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary" {
		t.Errorf("binary not replaced: %q", got)
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
	if _, err := Download(ReleaseDownloadURL + "/v9.9.9/checksums.txt"); err != nil {
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

func TestSeedFile(t *testing.T) {
	fakeRelease(t, "v1.0.0", []byte("bin"), false)
	url := ReleaseDownloadURL + "/v1.0.0/checksums.txt"
	body, err := Download(url)
	if err != nil {
		t.Fatal(err)
	}
	want := Sha256Hex(body)
	path := filepath.Join(t.TempDir(), "cached.tar.gz")

	if err := SeedFile(path, url, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); Sha256Hex(got) != want {
		t.Fatal("seeded file has the wrong content")
	}
	// Already valid: no download (an unreachable URL proves it isn't fetched).
	if err := SeedFile(path, "http://127.0.0.1:1/unreachable", want); err != nil {
		t.Errorf("valid cache should be kept without downloading: %v", err)
	}
	// Checksum mismatch: error, and nothing replaces the cache file.
	other := filepath.Join(t.TempDir(), "other.tar.gz")
	if err := SeedFile(other, url, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("want checksum mismatch, got %v", err)
	}
	if FileExists(other) {
		t.Error("a mismatched download must not be written to the cache")
	}
}

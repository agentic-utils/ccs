package selfupdate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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

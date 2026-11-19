package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Hostnames are what the browser's Host header carries: an IDN in punycode,
// lowercase, a trailing dot dropped.
func TestNormHost(t *testing.T) {
	for in, want := range map[string]string{
		"ccs.example.com":   "ccs.example.com",
		"CCS.Example.COM.":  "ccs.example.com",
		"bücher.example":    "xn--bcher-kva.example",
		" ccs.example.com ": "ccs.example.com",
	} {
		if got, err := normHost(in); err != nil || got != want {
			t.Errorf("normHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := normHost("a..b"); err == nil {
		t.Error("an empty label is refused")
	}
	var errb strings.Builder
	f, code := parseServeFlags([]string{"--host", "Bücher.Example.", "--team", "me", "--aud", "abc", "--email", "me@example.com"}, &errb)
	if code != 0 || f.host != "xn--bcher-kva.example" {
		t.Errorf("parseServeFlags normalises the host: %q, %d %s", f.host, code, errb.String())
	}
}

// Emails go through net/mail: a legal address with a slash is fine; a
// display name, two @s or no @ are not.
func TestPlainAddress(t *testing.T) {
	for e, ok := range map[string]bool{
		"me@example.com":            true,
		"a/b@example.com":           true,
		"first.last+tag@example.co": true,
		"Me <me@example.com>":       false,
		"a@b@example.com":           false,
		"nope":                      false,
		"me@":                       false,
	} {
		if got := plainAddress(e); got != ok {
			t.Errorf("plainAddress(%q) = %v, want %v", e, got, ok)
		}
	}
}

// The preview's link is opened through a private redirect page, never on a
// command line: the folder 0700, the page 0600, the link escaped in it, and
// the page gone after cleanup.
func TestPreviewRedirect(t *testing.T) {
	link := `http://127.0.0.1:7467/p/abc"def/`
	page, cleanup, err := previewRedirect(link)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(page)
	di, _ := os.Stat(filepath.Dir(page))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("modes: page %v, folder %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	b, _ := os.ReadFile(page)
	if !strings.Contains(string(b), `url=http://127.0.0.1:7467/p/abc&#34;def/`) || strings.Contains(string(b), `abc"def`) {
		t.Errorf("the link, escaped:\n%s", b)
	}
	if strings.Contains(page, "abc") {
		t.Error("the page's path must not hold the token")
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(page)); !os.IsNotExist(err) {
		t.Error("cleanup removes it")
	}
}

// A preview started with --parent-pid stops once that pid isn't its parent.
func TestWatchParent(t *testing.T) {
	old := parentPoll
	parentPoll = time.Millisecond
	t.Cleanup(func() { parentPoll = old })
	calls := 0
	getppid := func() int {
		calls++
		if calls < 3 {
			return 42
		}
		return 1 // adopted by launchd: ccs is gone
	}
	gone := make(chan struct{})
	go watchParent(42, getppid, func() { close(gone) })
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("the child never noticed its parent going")
	}
	var errb strings.Builder
	if f, code := parseServeFlags([]string{"--preview", "--parent-pid", "42"}, &errb); code != 0 || f.parentPID != 42 {
		t.Errorf("--parent-pid parses: %d %d %s", f.parentPID, code, errb.String())
	}
}

// A malformed plist read back for editing is an error, never a panic or a
// half-filled form.
func TestReadServeSetupMalformed(t *testing.T) {
	dir := t.TempDir()
	old := LaunchAgentsDir
	LaunchAgentsDir = func() string { return dir }
	t.Cleanup(func() { LaunchAgentsDir = old })
	for _, body := range []string{"", "not a plist", "<plist><dict><key>ProgramArguments</key><array><string>/bin/ccs</string></array></dict></plist>",
		"<plist><dict><key>ProgramArguments</key><array><string>/bin/ccs</string><string>serve</string><string>--host</string></array></dict></plist>"} {
		os.WriteFile(ServePlistPath(), []byte(body), 0o600)
		if s, err := ReadServeSetup(); err == nil {
			t.Errorf("%q read as %+v", body, s)
		}
	}
}

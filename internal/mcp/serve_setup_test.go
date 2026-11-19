package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func goodSetup() ServeSetup {
	return ServeSetup{Host: "ccs.example.com", Team: "me", AUD: "abc123", Email: "me@example.com"}
}

// The form's values are checked by parseServeFlags, as the command line's
// are: missing values, a hostname with a scheme or path, an email that
// isn't one.
func TestCheckServeSetup(t *testing.T) {
	if err := CheckServeSetup(goodSetup()); err != nil {
		t.Fatalf("a good setup: %v", err)
	}
	for _, c := range []struct {
		edit func(*ServeSetup)
		want string
	}{
		{func(s *ServeSetup) { s.AUD = "" }, "all required"},
		{func(s *ServeSetup) { s.Host = "https://ccs.example.com/" }, "bare hostname"},
		{func(s *ServeSetup) { s.Team = "me team" }, "--team"},
		{func(s *ServeSetup) { s.Email = "me@example.com, nobody" }, "isn't one"},
		{func(s *ServeSetup) { s.Extra = []string{"--addr=0.0.0.0:1"} }, "loopback"},
	} {
		s := goodSetup()
		c.edit(&s)
		if err := CheckServeSetup(s); err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "usage") {
			t.Errorf("%+v: %v, want %q", s, err, c.want)
		}
	}
	// The command line gets the same checks.
	var out, errb strings.Builder
	if code := RunServe([]string{"install", "--host", "https://x", "--team", "t", "--aud", "a", "--email", "me@x.com"}, &out, &errb, nil); code != 2 || !strings.Contains(errb.String(), "bare hostname") {
		t.Errorf("install refuses it too: %d %q", code, errb.String())
	}
}

// The default setup installs read-only; ticking write adds --allow-write.
// Install then start bootstraps the agent with those flags.
func TestInstallServeSetupWritesAndStarts(t *testing.T) {
	os.RemoveAll(LaunchAgentsDir())
	state := ""
	calls := stubLaunchctl(t, &state)
	oldBin := serveBinary
	serveBinary = func() string { return "/opt/homebrew/bin/ccs" }
	t.Cleanup(func() { serveBinary = oldBin })

	if _, err := InstallServeSetup(goodSetup()); err != nil {
		t.Fatal(err)
	}
	plist, _ := os.ReadFile(ServePlistPath())
	args := plistArgs(plist)
	want := []string{"/opt/homebrew/bin/ccs", "serve", "--host=ccs.example.com", "--team=me", "--aud=abc123", "--email=me@example.com"}
	if !slices.Equal(args, want) {
		t.Errorf("read-only by default: %q", args)
	}
	if _, err := StartServeService(); err != nil || !slices.ContainsFunc(*calls, func(c string) bool { return strings.HasPrefix(c, "bootstrap ") }) {
		t.Fatalf("started: %v %v", err, *calls)
	}
	s := goodSetup()
	s.Write = true
	if _, err := InstallServeSetup(s); err != nil {
		t.Fatal(err)
	}
	plist, _ = os.ReadFile(ServePlistPath())
	if args := plistArgs(plist); args[len(args)-1] != "--allow-write" {
		t.Errorf("ticked: %q", args)
	}
}

// A preview never becomes the agent, from the form either.
func TestInstallServeSetupNeverPreview(t *testing.T) {
	os.RemoveAll(LaunchAgentsDir())
	state := ""
	stubLaunchctl(t, &state)
	s := goodSetup()
	s.Extra = []string{"--preview"}
	if _, err := InstallServeSetup(s); err == nil {
		t.Fatal("--preview refused")
	}
	if _, err := InstallServeService(serveFlags{preview: true}, []string{"--preview"}); err == nil {
		t.Fatal("--preview refused by InstallServeService")
	}
	if _, err := os.Stat(ServePlistPath()); err == nil {
		t.Error("nothing written")
	}
}

// The installed plist reads back as the form's setup, keeping flags the
// form doesn't show.
func TestReadServeSetup(t *testing.T) {
	os.RemoveAll(LaunchAgentsDir())
	os.MkdirAll(LaunchAgentsDir(), 0o755)
	os.WriteFile(ServePlistPath(), servePlist("/bin/ccs", []string{"--host", "ccs.example.com", "--team", "me", "--aud", "a1", "--email", "me@example.com", "--allow-write", "--addr", "127.0.0.1:9000"}), 0o600)
	s, err := ReadServeSetup()
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "ccs.example.com" || s.Team != "me" || s.AUD != "a1" || s.Email != "me@example.com" || !s.Write || !slices.Equal(s.Extra, []string{"--addr=127.0.0.1:9000"}) {
		t.Errorf("%+v", s)
	}
}

// stubTunnel points the tunnel check at a temp folder, with cloudflared
// on PATH or not.
func stubTunnel(t *testing.T, onPath bool) string {
	t.Helper()
	dir := t.TempDir()
	oldLook, oldDir := lookPath, cloudflaredDir
	lookPath = func(string) (string, error) {
		if onPath {
			return "/opt/homebrew/bin/cloudflared", nil
		}
		return "", errors.New("not found")
	}
	cloudflaredDir = func() string { return dir }
	t.Cleanup(func() { lookPath, cloudflaredDir = oldLook, oldDir })
	return dir
}

func TestServeTunnelCheck(t *testing.T) {
	dir := stubTunnel(t, false)
	got := strings.Join(ServeTunnelCheck(goodSetup()), "\n")
	for _, want := range []string{"brew install cloudflared", "cloudflared tunnel login", "no ~/.cloudflared/config.yml", "cloudflared tunnel route dns ccs ccs.example.com", "service: http://127.0.0.1:7467"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	stubTunnel(t, true)
	cloudflaredDir = func() string { return dir }
	os.WriteFile(filepath.Join(dir, "config.yml"), []byte("tunnel: ccs\ningress:\n  - hostname: other.example.com\n    service: http://127.0.0.1:7467\n  - service: http_status:404\n"), 0o600)
	if got := strings.Join(ServeTunnelCheck(goodSetup()), "\n"); !strings.Contains(got, "no ingress rule for ccs.example.com") || strings.Contains(got, "brew install") {
		t.Errorf("no rule for the host:\n%s", got)
	}
	os.WriteFile(filepath.Join(dir, "config.yml"), []byte("ingress:\n  - hostname: \"CCS.example.com\" # mine\n    service: http://127.0.0.1:8080\n"), 0o600)
	if got := strings.Join(ServeTunnelCheck(goodSetup()), "\n"); !strings.Contains(got, "to http://127.0.0.1:8080, not to ccs serve") {
		t.Errorf("the wrong service:\n%s", got)
	}
	os.Remove(filepath.Join(dir, "config.yml"))
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("ingress:\n  - hostname: ccs.example.com\n    service: http://localhost:7467/\n  - service: http_status:404\n"), 0o600)
	if got := ServeTunnelCheck(goodSetup()); len(got) != 0 {
		t.Errorf("all set (config.yaml, localhost): %q", got)
	}
}

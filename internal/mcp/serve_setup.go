package mcp

// The TUI's ^X B setup form, as flags for `ccs serve install`: one code
// path, since ServeSetup's flags go through parseServeFlags and
// InstallServeService as the command line's do. ServeTunnelCheck looks for
// the cloudflared tunnel docs/remote.md sets up, so the form can say what's
// missing before the agent is installed.

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ServeSetup is what the setup form asks for: the Access application's
// values and whether the phone may write. Extra keeps flags the form
// doesn't show (--addr, --hook-wait) from an installed plist.
type ServeSetup struct {
	Host, Team, AUD, Email string
	Write                  bool
	Extra                  []string
}

// Args are the setup's flags for `ccs serve` (in --flag=value form, so a
// value starting with a dash is a value, never a flag).
func (s ServeSetup) Args() []string {
	args := []string{
		"--host=" + strings.TrimSpace(s.Host),
		"--team=" + strings.TrimSpace(s.Team),
		"--aud=" + strings.TrimSpace(s.AUD),
		"--email=" + strings.TrimSpace(s.Email),
	}
	args = append(args, s.Extra...)
	if s.Write {
		args = append(args, "--allow-write")
	}
	return args
}

// check parses the setup's flags as `ccs serve install` does: its error is
// parseServeFlags's reason, without the "ccs serve: " in front.
func (s ServeSetup) check() (serveFlags, error) {
	var errb bytes.Buffer
	f, code := parseServeFlags(s.Args(), &errb)
	if code == 0 {
		return f, nil
	}
	msg := strings.TrimSpace(errb.String())
	if first, _, ok := strings.Cut(msg, "\n"); ok {
		msg = first // a flag error is followed by the usage, which the form doesn't need
	}
	return f, errors.New(strings.TrimPrefix(msg, "ccs serve: "))
}

// CheckServeSetup says why `ccs serve install` would refuse the setup, or nil.
func CheckServeSetup(s ServeSetup) error {
	_, err := s.check()
	return err
}

// InstallServeSetup checks the setup as `ccs serve install` does and writes
// the agent's plist with its flags.
func InstallServeSetup(s ServeSetup) (string, error) {
	f, err := s.check()
	if err != nil {
		return "", err
	}
	return InstallServeService(f, s.Args())
}

// ReadServeSetup is the installed agent's setup, from its plist: what the
// form shows to edit it.
func ReadServeSetup() (ServeSetup, error) {
	raw, err := os.ReadFile(ServePlistPath())
	if err != nil {
		return ServeSetup{}, err
	}
	args := plistArgs(raw)
	if len(args) < 2 || args[1] != "serve" {
		return ServeSetup{}, errors.New("the agent's plist doesn't run ccs serve")
	}
	var errb bytes.Buffer
	f, code := parseServeFlags(args[2:], &errb)
	if code != 0 {
		return ServeSetup{}, errors.New("the agent's flags don't parse: " + strings.TrimSpace(errb.String()))
	}
	s := ServeSetup{Host: f.host, Team: f.team, AUD: f.aud, Email: f.emails, Write: f.write}
	if f.addr != defaultServeAddr {
		s.Extra = append(s.Extra, "--addr="+f.addr)
	}
	if f.hookWait != maxHookWait {
		s.Extra = append(s.Extra, "--hook-wait="+f.hookWait.String())
	}
	return s, nil
}

// lookPath and cloudflaredDir find cloudflared and its config; stubbed in
// tests.
var (
	lookPath       = exec.LookPath
	cloudflaredDir = func() string {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".cloudflared")
	}
)

// ServeTunnelCheck looks for what docs/remote.md's steps set up for the
// setup's hostname: cloudflared on PATH, and an ingress rule in
// ~/.cloudflared/config.yml (or .yaml) sending the hostname to the agent's
// address. It returns what's missing, with the commands that fix it, or
// nothing. It only reads; a setup that fails its own check returns nothing
// here (CheckServeSetup says why).
func ServeTunnelCheck(s ServeSetup) []string {
	f, err := s.check()
	if err != nil {
		return nil
	}
	host, want := strings.ToLower(f.host), "http://"+f.addr
	var out []string
	if _, err := lookPath("cloudflared"); err != nil {
		out = append(out, "cloudflared isn't on PATH. Install it and log in:",
			"  brew install cloudflared",
			"  cloudflared tunnel login")
	}
	var cfg, path string
	for _, name := range []string{"config.yml", "config.yaml"} {
		p := filepath.Join(cloudflaredDir(), name)
		if raw, err := os.ReadFile(p); err == nil {
			cfg, path = string(raw), p
			break
		}
	}
	fix := []string{
		"  cloudflared tunnel create ccs",
		"  cloudflared tunnel route dns ccs " + host,
		"then in ~/.cloudflared/config.yml, under ingress:",
		"  - hostname: " + host,
		"    service: " + want,
	}
	switch svc, found := ingressFor(cfg, host); {
	case path == "":
		out = append(out, "There's no ~/.cloudflared/config.yml. Create the tunnel and its hostname:")
		out = append(out, fix...)
	case !found:
		out = append(out, path+" has no ingress rule for "+host+". Add one:")
		out = append(out, fix...)
	case !sameService(svc, f.addr):
		out = append(out, fmt.Sprintf("%s sends %s to %s, not to ccs serve at %s. Change its service to:", path, host, svc, want),
			"    service: "+want)
	}
	return out
}

// ingressFor finds the service an ingress rule sends host to, reading the
// few YAML shapes cloudflared's config uses: list items of hostname and
// service keys.
func ingressFor(cfg, host string) (svc string, found bool) {
	cur := ""
	for _, line := range strings.Split(cfg, "\n") {
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(t, "-"); ok { // a new list item
			cur, t = "", strings.TrimSpace(rest)
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "hostname":
			cur = strings.ToLower(v)
		case "service":
			if cur == host {
				return v, true
			}
		}
	}
	return "", false
}

// sameService says whether an ingress service reaches addr: http to the
// same port, with localhost and 127.0.0.1 alike.
func sameService(svc, addr string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(svc)), "http://")
	if !ok {
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	norm := func(a string) string {
		h, p, err := net.SplitHostPort(a)
		if err != nil {
			return a
		}
		if h == "localhost" {
			h = "127.0.0.1"
		}
		return net.JoinHostPort(h, p)
	}
	return norm(rest) == norm(addr)
}

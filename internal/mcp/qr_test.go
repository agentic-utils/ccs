package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each line is two module rows, ▀ with the top one in front and the bottom
// one behind; dark is black (16), light white (231), with a light border.
func TestRenderQR(t *testing.T) {
	out := renderQR([][]bool{{true, false}, {false, true}})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != (2+2*qrQuiet+1)/2 {
		t.Fatalf("%d lines: %q", len(lines), out)
	}
	cell := func(top, bottom string) string { return "\033[38;5;" + top + ";48;5;" + bottom + "m▀" }
	w, b := "231", "16"
	// The line holding module rows 0 and 1 (quiet rows above it).
	mid := lines[qrQuiet/2]
	want := strings.Repeat(cell(w, w), qrQuiet) + cell(b, w) + cell(w, b) + strings.Repeat(cell(w, w), qrQuiet) + "\033[0m"
	if mid != want {
		t.Errorf("got  %q\nwant %q", mid, want)
	}
	if strings.Count(lines[0], "▀") != 2+2*qrQuiet {
		t.Errorf("each line is as wide as the code and its border: %q", lines[0])
	}
}

func TestQRCodeEncodes(t *testing.T) {
	text, w, err := QRCode("https://ccs.example.com/")
	if err != nil || w < 21+2*qrQuiet || strings.Count(strings.SplitN(text, "\n", 2)[0], "▀") != w {
		t.Errorf("%d %v", w, err)
	}
}

func TestServeQRFromThePlist(t *testing.T) {
	dir := t.TempDir()
	old := LaunchAgentsDir
	LaunchAgentsDir = func() string { return dir }
	t.Cleanup(func() { LaunchAgentsDir = old })
	var out, errb strings.Builder
	if code := RunService([]string{"qr"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "ccs serve install") {
		t.Errorf("no agent: %d %q", code, errb.String())
	}
	os.WriteFile(filepath.Join(dir, ServeLabel+".plist"), servePlist("/bin/ccs", []string{"--host", "ccs.example.com", "--team", "t", "--aud", "a", "--email", "me@example.com"}), 0o600)
	out.Reset()
	if code := RunService([]string{"qr"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "▀") || !strings.Contains(out.String(), "https://ccs.example.com/") {
		t.Errorf("with an agent: %d %q", code, out.String())
	}
	if h := plistHost(servePlist("/bin/ccs", []string{"--host=x.example.com"})); h != "x.example.com" {
		t.Errorf("--host=: %q", h)
	}
	os.WriteFile(filepath.Join(dir, ServeLabel+".plist"), servePlist("/bin/ccs", []string{"--preview"}), 0o600)
	if _, err := ServeURL(); err == nil || !strings.Contains(err.Error(), "--preview") {
		t.Errorf("a preview agent has no address for a phone: %v", err)
	}
}

// A preview prints no QR code (127.0.0.1 on a phone is the phone); the
// remote mode does, of https://<host>/.
func TestServeStartPrintsQROnlyForTheTunnel(t *testing.T) {
	var out strings.Builder
	pv := newServer(serveConfig{preview: true, token: "tok"})
	if link := pv.announce(&out, "127.0.0.1:7467"); link != "http://127.0.0.1:7467/p/tok/" || strings.ContainsAny(out.String(), "▀▄█") {
		t.Errorf("the preview prints its link and no QR code: %q\n%s", link, out.String())
	}
	out.Reset()
	rm := newServer(serveConfig{host: "ccs.example.com", access: newAccessVerifier("me", "aud", []string{"me@example.com"})})
	if link := rm.announce(&out, "127.0.0.1:7467"); link != "" || !strings.ContainsAny(out.String(), "▀▄█") || !strings.Contains(out.String(), "https://ccs.example.com/") {
		t.Errorf("the remote start prints the tunnel's QR code: %q\n%s", link, out.String())
	}
}

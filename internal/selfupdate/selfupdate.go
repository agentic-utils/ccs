// Package selfupdate is how ccs updates itself: finding the latest release,
// verifying its signed checksums, preparing and installing it (Homebrew or
// the binary in place), the downloads behind that, and release notes.
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
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Upgrader updates this install. prepare (optional) does the slow,
// side-effect-free part (refresh the tap, download and verify) while the popup
// is still up; install does the rest once the user confirms, waiting for a
// prepare still in flight so the two never run brew at the same time.
type Upgrader struct {
	prepare     func(tag string) error
	InstallFunc func(tag string, step func(string)) (string, error)

	Mu      sync.Mutex
	Pending map[string]chan struct{}
}

// Prepare starts preparing tag in the background, once per tag.
func (u *Upgrader) Prepare(tag string) {
	if u.prepare == nil {
		return
	}
	u.Mu.Lock()
	defer u.Mu.Unlock()
	if u.Pending == nil {
		u.Pending = make(map[string]chan struct{})
	}
	if _, ok := u.Pending[tag]; ok {
		return
	}
	done := make(chan struct{})
	u.Pending[tag] = done
	go func() {
		defer close(done)
		defer transcript.RecoverWorker()
		start := time.Now()
		err := u.prepare(tag) // failures are retried by install
		state.LogUpdate("prepare %s: %v in %s", tag, state.ErrOrOK(err), time.Since(start).Round(time.Millisecond))
	}()
}

func (u *Upgrader) Install(tag string, step func(string)) (string, error) {
	u.Mu.Lock()
	done := u.Pending[tag]
	u.Mu.Unlock()
	if done != nil {
		step("finishing download")
		// install redoes whatever prepare didn't finish, so a stuck prepare
		// only delays it.
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
		}
	}
	return u.InstallFunc(tag, step)
}

// ReleaseChangelog lists what changed in every release after from up to tag,
// newest first. The REST API has the whole history in one call; if it fails
// (its 60/hour unauthenticated limit, say) the Atom feed still has the
// latest 10 releases.
func ReleaseChangelog(from, tag string) ([]string, error) {
	// downloadTransport moves on from an address that stalls (5s each).
	client := http.Client{Timeout: 20 * time.Second, Transport: downloadTransport}
	lines, err := apiChangelog(client, from, tag)
	if err == nil {
		return lines, nil
	}
	state.LogUpdate("changelog from the API: %v; trying the feed", err)
	resp, err := client.Get("https://github.com/agentic-utils/ccs/releases.atom")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases feed: %s", resp.Status)
	}
	return parseChangelog(io.LimitReader(resp.Body, 1<<20), from, tag)
}

func apiChangelog(client http.Client, from, tag string) ([]string, error) {
	resp, err := client.Get("https://api.github.com/repos/agentic-utils/ccs/releases?per_page=100")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases API: %s", resp.Status)
	}
	return parseAPIChangelog(io.LimitReader(resp.Body, 4<<20), from, tag)
}

// parseAPIChangelog is parseChangelog for the REST API's release list, whose
// bodies are GoReleaser's markdown ("* <hash> fix: …").
func parseAPIChangelog(r io.Reader, current, tag string) ([]string, error) {
	var releases []struct {
		Tag  string `json:"tag_name"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r).Decode(&releases); err != nil {
		return nil, err
	}
	var out []string
	for _, rel := range releases {
		if !NewerVersion(rel.Tag, current) || NewerVersion(rel.Tag, strings.TrimPrefix(tag, "v")) {
			continue
		}
		out = append(out, rel.Tag)
		for _, l := range strings.Split(rel.Body, "\n") {
			l = strings.TrimSpace(l)
			if !strings.HasPrefix(l, "* ") && !strings.HasPrefix(l, "- ") {
				continue
			}
			text := commitHash.ReplaceAllString(strings.TrimSpace(l[2:]), "")
			if text != "" && !strings.HasPrefix(text, "Merge ") {
				out = append(out, "  • "+text)
			}
		}
	}
	return out, nil
}

var (
	liItem     = regexp.MustCompile(`(?s)<li>(.*?)</li>`)
	commitHash = regexp.MustCompile(`^[0-9a-f]{7,40}\s+`)
)

// parseChangelog turns the feed's entries newer than current, up to and
// including tag, into "v0.43.1" headings each followed by "  fix: …" commit
// subjects (merge commits dropped).
func parseChangelog(r io.Reader, current, tag string) ([]string, error) {
	var feed struct {
		Entries []struct {
			Title   string `xml:"title"`
			Content string `xml:"content"`
		} `xml:"entry"`
	}
	if err := xml.NewDecoder(r).Decode(&feed); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range feed.Entries {
		if !NewerVersion(e.Title, current) || NewerVersion(e.Title, strings.TrimPrefix(tag, "v")) {
			continue
		}
		out = append(out, e.Title)
		for _, li := range liItem.FindAllStringSubmatch(e.Content, -1) {
			text := strings.TrimSpace(html.UnescapeString(anyTag.ReplaceAllString(li[1], "")))
			text = commitHash.ReplaceAllString(text, "")
			if text != "" && !strings.HasPrefix(text, "Merge ") {
				out = append(out, "  • "+text)
			}
		}
	}
	return out, nil
}

// NewerVersion reports whether release tag (e.g. "v0.25.0") is newer than
// current (e.g. "0.24.1"). Non-release builds ("dev") never update.
func NewerVersion(tag, current string) bool {
	parse := func(v string) ([3]int, bool) {
		var p [3]int
		parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
		if len(parts) != 3 {
			return p, false
		}
		for i, s := range parts {
			n, err := strconv.Atoi(s)
			if err != nil {
				return p, false
			}
			p[i] = n
		}
		return p, true
	}
	a, ok1 := parse(tag)
	b, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// LatestRelease finds the newest ccs release tag from where GitHub's
// releases/latest page redirects (.../releases/tag/v0.25.0). The web redirect
// isn't subject to the REST API's 60/hour unauthenticated limit, which shared
// office IPs exhaust.
func LatestRelease() (string, error) {
	client := http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Head("https://github.com/agentic-utils/ccs/releases/latest")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	return releaseTagFromURL(resp.Header.Get("Location"))
}

func releaseTagFromURL(loc string) (string, error) {
	_, tag, ok := strings.Cut(loc, "/releases/tag/")
	if !ok || tag == "" {
		return "", fmt.Errorf("unexpected releases/latest redirect %q", loc)
	}
	return tag, nil
}

// ChooseUpgrader picks how to update the binary at exe. Homebrew installs are
// only ever upgraded through brew, never overwritten, so ccs can't fight the
// formula; Nix store paths are read-only; anything else (go install, a
// downloaded release) is replaced in place from the GitHub release.
func ChooseUpgrader(exe string) *Upgrader {
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	switch {
	case strings.Contains(exe, "/Cellar/"):
		brew, err := exec.LookPath("brew")
		if err != nil {
			return nil
		}
		return &Upgrader{
			prepare: func(string) error {
				refreshTap(brew, func(string) {})
				preseedBrewCache(brew)
				return runBrew(brew, "fetch", "agentic-utils/tap/ccs") // into brew's download cache
			},
			InstallFunc: func(_ string, step func(string)) (string, error) {
				if err := refreshTap(brew, step); err != nil {
					return "", err
				}
				step("verifying")
				if err := CheckFormula(brew); err != nil {
					return "", err
				}
				preseedBrewCache(brew)
				step("brew upgrade")
				if err := runBrew(brew, "upgrade", "agentic-utils/tap/ccs"); err != nil {
					return "", err
				}
				// brew cleans up the old keg, so restart via the linked binary on PATH.
				// Restart the ccs this brew manages, not whatever ccs is first on PATH.
				if exe := filepath.Join(filepath.Dir(brew), "ccs"); FileExists(exe) {
					return exe, nil
				}
				return exec.LookPath("ccs")
			},
		}
	case strings.HasPrefix(exe, "/nix/"):
		return nil
	}
	var fetched sync.Map // tag -> verified binary, filled by prepare
	return &Upgrader{
		prepare: func(tag string) error {
			bin, err := fetchRelease(tag, func(string) {})
			if err == nil {
				fetched.Store(tag, bin)
			}
			return err
		},
		InstallFunc: func(tag string, step func(string)) (string, error) {
			bin, ok := fetched.Load(tag)
			if !ok {
				b, err := fetchRelease(tag, step)
				if err != nil {
					return "", err
				}
				bin = b
			}
			step("installing")
			return exe, installBinary(exe, bin.([]byte))
		},
	}
}

// refreshTap pulls only ccs's tap: a full `brew update` fetches every tap and
// can take minutes. Falls back to it if the tap isn't a plain git checkout.
func refreshTap(brew string, step func(string)) error {
	step("refreshing tap")
	tap, err := proc.RunCommand(30*time.Second, nil, true, brew, "--repository", "agentic-utils/tap")
	if err == nil {
		// Never prompt: a credential or passphrase request fails the pull
		// (falling back to brew update) instead of waiting on a hidden prompt.
		_, err = proc.RunCommand(time.Minute, []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes"}, true,
			"git", "-C", strings.TrimSpace(string(tap)), "pull", "--ff-only", "--quiet")
		if err == nil {
			return nil
		}
	}
	step("brew update")
	return runBrew(brew, "update", "--quiet")
}

// runBrew runs brew without its own auto-update (we refresh the tap
// ourselves), returning brew's last output line as the error.
func runBrew(brew string, args ...string) error {
	env := []string{"HOMEBREW_NO_AUTO_UPDATE=1", "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes"}
	if args[0] == "fetch" || args[0] == "upgrade" {
		// Makes curl print each address it tries and how it failed, for the
		// update log (brew's own --verbose doesn't show that).
		env = append(env, "HOMEBREW_CURL_VERBOSE=1")
	}
	if out, err := proc.RunCommand(10*time.Minute, env, true, brew, args...); err != nil {
		if errors.Is(err, proc.ErrTimedOut) {
			return fmt.Errorf("brew %s: %w", args[0], err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return fmt.Errorf("brew %s: %s", args[0], lines[len(lines)-1])
	}
	return nil
}

// ReleaseDownloadURL is the base for release assets; a var so tests can
// serve their own.
var ReleaseDownloadURL = "https://github.com/agentic-utils/ccs/releases/download"

// fetchRelease downloads tag's release archive for this OS/arch, checks it
// against the release's checksums.txt, and returns the ccs binary inside.
func fetchRelease(tag string, step func(string)) ([]byte, error) {
	asset := fmt.Sprintf("ccs_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	step("downloading")
	sums, err := SignedSums(tag)
	if err != nil {
		return nil, err
	}
	archive, err := Download(ReleaseDownloadURL + "/" + tag + "/" + asset)
	if err != nil {
		return nil, err
	}
	step("verifying")
	sum := sha256.Sum256(archive)
	if want := ChecksumFor(string(sums), asset); want == "" || want != hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("checksum mismatch for %s", asset)
	}
	return binaryFromTarGz(archive, "ccs")
}

// ReleaseKey is the public half of the Ed25519 key that ci.yaml signs each
// release's checksums.txt with (sign-release.sh; the private half is a secret
// of the "release" environment, which only main can use). A release uploaded
// any other way, a leaked token included, has no valid signature and is
// refused. A var so tests can sign with their own.
var ReleaseKey = mustPublicKey(`-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAOEu0VgLUDjlp6LuJPmFxNZneB3ohrAddcRXqEzwWaZQ=
-----END PUBLIC KEY-----`)

func mustPublicKey(pemText string) ed25519.PublicKey {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		panic("release key isn't PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	return key.(ed25519.PublicKey)
}

// SignedMessage is what sign-release.sh signs: the tag first, so an old
// release's signed checksums can't be passed off under a newer tag.
func SignedMessage(tag string, sums []byte) []byte {
	return append([]byte("ccs "+tag+"\n"), sums...)
}

// SignedSums downloads tag's checksums.txt and returns it only once
// checksums.txt.sig proves CI signed it for that tag.
func SignedSums(tag string) ([]byte, error) {
	sums, err := Download(ReleaseDownloadURL + "/" + tag + "/checksums.txt")
	if err != nil {
		return nil, err
	}
	sig, err := Download(ReleaseDownloadURL + "/" + tag + "/checksums.txt.sig")
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(ReleaseKey, SignedMessage(tag, sums), sig) {
		state.LogUpdate("signature check for %s failed", tag)
		return nil, fmt.Errorf("%s isn't signed by ccs's CI", tag)
	}
	return sums, nil
}

// installBinary atomically swaps bin in place of exe.
func installBinary(exe string, bin []byte) error {
	// Temp file beside exe so the rename is atomic (same filesystem).
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".ccs-update-*")
	if err != nil {
		return fmt.Errorf("can't write to %s: %w", filepath.Dir(exe), err)
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // a crash mustn't leave a half-written ccs
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Size caps for self-update downloads: generous for a ~7MB release, but a
// bogus or hostile asset can't exhaust memory before the user says yes.
const (
	maxDownload = 100 << 20
	maxBinary   = 200 << 20
)

func Download(url string) (body []byte, err error) {
	start := time.Now()
	var remote string
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { remote = i.Conn.RemoteAddr().String() }}
	defer func() {
		state.LogUpdate("GET %s\n  -> %v, %d bytes in %s (last connection %s)", url, state.ErrOrOK(err), len(body), time.Since(start).Round(time.Millisecond), remote)
	}()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 2 * time.Minute, Transport: downloadTransport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err == nil && len(body) > maxDownload {
		return nil, fmt.Errorf("download %s: larger than %dMB", url, maxDownload>>20)
	}
	return body, err
}

// preseedBrewCache puts the formula's download into brew's cache ourselves,
// using the address-fallback transport, so brew fetch/upgrade find it cached
// instead of downloading with curl. It uses only the URL and sha256 brew
// reports; brew still verifies the file. Any problem is logged and leaves
// brew to download as usual.
func preseedBrewCache(brew string) {
	if err := preseed(brew); err != nil {
		state.LogUpdate("preseed skipped, brew will download itself: %v", err)
	}
}

// formulaDownload is the archive URL and sha256 the tap's formula installs.
func formulaDownload(brew string) (url, sum string, err error) {
	out, err := proc.RunCommand(30*time.Second, []string{"HOMEBREW_NO_AUTO_UPDATE=1"}, true, brew, "info", "--json=v2", "agentic-utils/tap/ccs")
	if err != nil {
		return "", "", err
	}
	var info struct {
		Formulae []struct {
			URLs struct {
				Stable struct{ URL, Checksum string } `json:"stable"`
			} `json:"urls"`
		} `json:"formulae"`
	}
	if json.Unmarshal(out, &info) != nil || len(info.Formulae) != 1 {
		return "", "", errors.New("unexpected brew info output")
	}
	url, sum = info.Formulae[0].URLs.Stable.URL, info.Formulae[0].URLs.Stable.Checksum
	if len(sum) != 64 {
		return "", "", fmt.Errorf("unexpected formula checksum %q", sum)
	}
	return url, sum, nil
}

// CheckFormula refuses a tap formula whose archive isn't in its release's
// signed checksums.txt: the tap is pushed by CI too, so it's checked the same way.
func CheckFormula(brew string) error {
	url, sum, err := formulaDownload(brew)
	if err != nil {
		return err
	}
	rest, ok := strings.CutPrefix(url, ReleaseDownloadURL+"/")
	tag, asset, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || strings.Contains(asset, "/") {
		return fmt.Errorf("formula downloads from %s, not a ccs release", url)
	}
	if !NewerVersion(tag, state.Version) {
		return fmt.Errorf("tap's formula is %s, not newer than v%s", tag, state.Version)
	}
	sums, err := SignedSums(tag)
	if err != nil {
		return err
	}
	if ChecksumFor(string(sums), asset) != sum {
		return fmt.Errorf("formula's checksum for %s isn't the one ccs's CI signed", asset)
	}
	return nil
}

func preseed(brew string) error {
	url, want, err := formulaDownload(brew)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("unexpected formula url %q", url)
	}
	env := []string{"HOMEBREW_NO_AUTO_UPDATE=1"}
	cacheOut, err := proc.RunCommand(30*time.Second, env, true, brew, "--cache", "agentic-utils/tap/ccs")
	if err != nil {
		return err
	}
	return SeedFile(strings.TrimSpace(string(cacheOut)), url, want)
}

// SeedFile makes path hold url's content with sha256 want, downloading only
// if it isn't already there. Written atomically beside path.
func SeedFile(path, url, want string) error {
	if data, err := os.ReadFile(path); err == nil && Sha256Hex(data) == want {
		return nil
	}
	body, err := Download(url)
	if err != nil {
		return err
	}
	if got := Sha256Hex(body); got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s", url, got)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ccs-seed-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { // as brew writes its own downloads
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func Sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// downloadTransport tries every address a host resolves to, each with a short
// limit covering TCP and the TLS handshake, and moves on when one fails, so a
// single unreachable address costs seconds rather than the whole download.
// Addresses that failed are tried last for the rest of the process.
var downloadTransport = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	DialTLSContext:      dialTLSAnyAddr,
	TLSHandshakeTimeout: 10 * time.Second,
}

var (
	addrAttemptTimeout = 5 * time.Second
	downloadTLSConfig  = &tls.Config{} // tests swap in their own roots
	resolveHost        = func(ctx context.Context, host string) ([]string, error) {
		return net.DefaultResolver.LookupHost(ctx, host)
	}
	dialAddr    = (&net.Dialer{}).DialContext
	failedAddrs sync.Map
)

func dialTLSAnyAddr(ctx context.Context, network, hostport string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	addrs, err := resolveHost(ctx, host)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(addrs, func(i, j int) bool { // known-bad addresses last
		_, bi := failedAddrs.Load(addrs[i])
		_, bj := failedAddrs.Load(addrs[j])
		return !bi && bj
	})
	var lastErr error
	for _, a := range addrs {
		start := time.Now()
		conn, err := tlsDialOne(ctx, network, net.JoinHostPort(a, port), host)
		if err == nil {
			state.LogUpdate("connect %s via %s: ok in %s", host, a, time.Since(start).Round(time.Millisecond))
			return conn, nil
		}
		failedAddrs.Store(a, true)
		state.LogUpdate("connect %s via %s: %v after %s, trying next address", host, a, err, time.Since(start).Round(time.Millisecond))
		lastErr = err
	}
	return nil, fmt.Errorf("no address for %s worked: %w", host, lastErr)
}

func tlsDialOne(ctx context.Context, network, addr, serverName string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, addrAttemptTimeout)
	defer cancel()
	raw, err := dialAddr(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	cfg := downloadTLSConfig.Clone()
	cfg.ServerName = serverName
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}

// ChecksumFor finds name's sha256 in a checksums.txt ("<hex>  <name>" lines).
func ChecksumFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			return f[0]
		}
	}
	return ""
}

func binaryFromTarGz(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	// Caps everything decompressed, including entries skipped on the way.
	tr := tar.NewReader(io.LimitReader(gz, maxBinary+maxDownload))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in release archive", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == name {
			if h.Size > maxBinary {
				return nil, fmt.Errorf("%s in release archive is %dMB, over the %dMB limit", name, h.Size>>20, maxBinary>>20)
			}
			return io.ReadAll(io.LimitReader(tr, maxBinary))
		}
	}
}

const MaxChangelogLines = 12

// anyTag matches an HTML tag; the release notes are HTML. A copy of the
// pattern of the same name in internal/transcript (where HarnessNote uses it).
var anyTag = regexp.MustCompile(`</?[a-z][a-z0-9-]*[^>]*>`)

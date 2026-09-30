package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// How often ccs asks GitHub for a newer release (one ~5KB HEAD request to the
// releases/latest redirect), and how long it waits after a failed check, so
// a throttled or offline check doesn't retry every interval from a shared IP.
const (
	updateCheckInterval = 2 * time.Minute
	updateCheckBackoff  = 10 * time.Minute
)

func updatingTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return updatingTickMsg{} })
}

// updateKeyGrace is how long a freshly opened update popup ignores keys.
const updateKeyGrace = time.Second

type updateCheckTickMsg struct{}

type updatingTickMsg struct{}

// upgrader updates this install. prepare (optional) does the slow,
// side-effect-free part (refresh the tap, download and verify) while the popup
// is still up; install does the rest once the user confirms, waiting for a
// prepare still in flight so the two never run brew at the same time.
type upgrader struct {
	prepare func(tag string) error
	install func(tag string, step func(string)) (string, error)

	mu      sync.Mutex
	pending map[string]chan struct{}
}

// Prepare starts preparing tag in the background, once per tag.
func (u *upgrader) Prepare(tag string) {
	if u.prepare == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pending == nil {
		u.pending = make(map[string]chan struct{})
	}
	if _, ok := u.pending[tag]; ok {
		return
	}
	done := make(chan struct{})
	u.pending[tag] = done
	go func() {
		defer close(done)
		defer recoverWorker()
		start := time.Now()
		err := u.prepare(tag) // failures are retried by install
		logUpdate("prepare %s: %v in %s", tag, errOrOK(err), time.Since(start).Round(time.Millisecond))
	}()
}

func (u *upgrader) Install(tag string, step func(string)) (string, error) {
	u.mu.Lock()
	done := u.pending[tag]
	u.mu.Unlock()
	if done != nil {
		step("finishing download")
		// install redoes whatever prepare didn't finish, so a stuck prepare
		// only delays it.
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
		}
	}
	return u.install(tag, step)
}

// updateProgress is what the header shows while an upgrade runs. The upgrade
// runs off the UI goroutine, hence the lock.
type updateProgress struct {
	mu      sync.Mutex
	step    string
	started time.Time
}

func (p *updateProgress) set(step string) {
	p.mu.Lock()
	p.step = step
	p.mu.Unlock()
}

func (p *updateProgress) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("%s (%ds)", p.step, int(time.Since(p.started).Seconds()))
}

type latestMsg struct {
	tag string
	err error
}

type notesMsg struct {
	lines []string
	err   error
}

type changelogMsg struct {
	tag   string
	lines []string
}

type upgradeDoneMsg struct {
	path string
	err  error
}

func (m model) checkUpdateCmd() tea.Cmd {
	check := m.checkLatest
	return func() tea.Msg {
		tag, err := check()
		return latestMsg{tag, err}
	}
}

// releaseChangelog lists what changed in every release after from up to tag,
// newest first. The REST API has the whole history in one call; if it fails
// (its 60/hour unauthenticated limit, say) the Atom feed still has the
// latest 10 releases.
func releaseChangelog(from, tag string) ([]string, error) {
	// downloadTransport moves on from an address that stalls (5s each).
	client := http.Client{Timeout: 20 * time.Second, Transport: downloadTransport}
	lines, err := apiChangelog(client, from, tag)
	if err == nil {
		return lines, nil
	}
	logUpdate("changelog from the API: %v; trying the feed", err)
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
		if !newerVersion(rel.Tag, current) || newerVersion(rel.Tag, strings.TrimPrefix(tag, "v")) {
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
		if !newerVersion(e.Title, current) || newerVersion(e.Title, strings.TrimPrefix(tag, "v")) {
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

// newerVersion reports whether release tag (e.g. "v0.25.0") is newer than
// current (e.g. "0.24.1"). Non-release builds ("dev") never update.
func newerVersion(tag, current string) bool {
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

// latestRelease finds the newest ccs release tag from where GitHub's
// releases/latest page redirects (.../releases/tag/v0.25.0). The web redirect
// isn't subject to the REST API's 60/hour unauthenticated limit, which shared
// office IPs exhaust.
func latestRelease() (string, error) {
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

// chooseUpgrader picks how to update the binary at exe. Homebrew installs are
// only ever upgraded through brew, never overwritten, so ccs can't fight the
// formula; Nix store paths are read-only; anything else (go install, a
// downloaded release) is replaced in place from the GitHub release.
func chooseUpgrader(exe string) *upgrader {
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	switch {
	case strings.Contains(exe, "/Cellar/"):
		brew, err := exec.LookPath("brew")
		if err != nil {
			return nil
		}
		return &upgrader{
			prepare: func(string) error {
				refreshTap(brew, func(string) {})
				preseedBrewCache(brew)
				return runBrew(brew, "fetch", "agentic-utils/tap/ccs") // into brew's download cache
			},
			install: func(_ string, step func(string)) (string, error) {
				if err := refreshTap(brew, step); err != nil {
					return "", err
				}
				preseedBrewCache(brew)
				step("brew upgrade")
				if err := runBrew(brew, "upgrade", "agentic-utils/tap/ccs"); err != nil {
					return "", err
				}
				// brew cleans up the old keg, so restart via the linked binary on PATH.
				// Restart the ccs this brew manages, not whatever ccs is first on PATH.
				if exe := filepath.Join(filepath.Dir(brew), "ccs"); fileExists(exe) {
					return exe, nil
				}
				return exec.LookPath("ccs")
			},
		}
	case strings.HasPrefix(exe, "/nix/"):
		return nil
	}
	var fetched sync.Map // tag -> verified binary, filled by prepare
	return &upgrader{
		prepare: func(tag string) error {
			bin, err := fetchRelease(tag, func(string) {})
			if err == nil {
				fetched.Store(tag, bin)
			}
			return err
		},
		install: func(tag string, step func(string)) (string, error) {
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
	tap, err := runCommand(30*time.Second, nil, true, brew, "--repository", "agentic-utils/tap")
	if err == nil {
		// Never prompt: a credential or passphrase request fails the pull
		// (falling back to brew update) instead of waiting on a hidden prompt.
		_, err = runCommand(time.Minute, []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes"}, true,
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
	if out, err := runCommand(10*time.Minute, env, true, brew, args...); err != nil {
		if errors.Is(err, errTimedOut) {
			return fmt.Errorf("brew %s: %w", args[0], err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return fmt.Errorf("brew %s: %s", args[0], lines[len(lines)-1])
	}
	return nil
}

// releaseDownloadURL is the base for release assets; a var so tests can
// serve their own.
var releaseDownloadURL = "https://github.com/agentic-utils/ccs/releases/download"

// fetchRelease downloads tag's release archive for this OS/arch, checks it
// against the release's checksums.txt, and returns the ccs binary inside.
func fetchRelease(tag string, step func(string)) ([]byte, error) {
	asset := fmt.Sprintf("ccs_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	step("downloading")
	sums, err := download(releaseDownloadURL + "/" + tag + "/checksums.txt")
	if err != nil {
		return nil, err
	}
	archive, err := download(releaseDownloadURL + "/" + tag + "/" + asset)
	if err != nil {
		return nil, err
	}
	step("verifying")
	sum := sha256.Sum256(archive)
	if want := checksumFor(string(sums), asset); want == "" || want != hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("checksum mismatch for %s", asset)
	}
	return binaryFromTarGz(archive, "ccs")
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Size caps for self-update downloads: generous for a ~7MB release, but a
// bogus or hostile asset can't exhaust memory before the user says yes.
const (
	maxDownload = 100 << 20
	maxBinary   = 200 << 20
)

func download(url string) (body []byte, err error) {
	start := time.Now()
	var remote string
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { remote = i.Conn.RemoteAddr().String() }}
	defer func() {
		logUpdate("GET %s\n  -> %v, %d bytes in %s (last connection %s)", url, errOrOK(err), len(body), time.Since(start).Round(time.Millisecond), remote)
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
		logUpdate("preseed skipped, brew will download itself: %v", err)
	}
}

func preseed(brew string) error {
	env := []string{"HOMEBREW_NO_AUTO_UPDATE=1"}
	out, err := runCommand(30*time.Second, env, true, brew, "info", "--json=v2", "agentic-utils/tap/ccs")
	if err != nil {
		return err
	}
	var info struct {
		Formulae []struct {
			URLs struct {
				Stable struct{ URL, Checksum string } `json:"stable"`
			} `json:"urls"`
		} `json:"formulae"`
	}
	if json.Unmarshal(out, &info) != nil || len(info.Formulae) != 1 {
		return errors.New("unexpected brew info output")
	}
	url, want := info.Formulae[0].URLs.Stable.URL, info.Formulae[0].URLs.Stable.Checksum
	if !strings.HasPrefix(url, "https://") || len(want) != 64 {
		return fmt.Errorf("unexpected formula url/checksum %q %q", url, want)
	}
	cacheOut, err := runCommand(30*time.Second, env, true, brew, "--cache", "agentic-utils/tap/ccs")
	if err != nil {
		return err
	}
	return seedFile(strings.TrimSpace(string(cacheOut)), url, want)
}

// seedFile makes path hold url's content with sha256 want, downloading only
// if it isn't already there. Written atomically beside path.
func seedFile(path, url, want string) error {
	if data, err := os.ReadFile(path); err == nil && sha256Hex(data) == want {
		return nil
	}
	body, err := download(url)
	if err != nil {
		return err
	}
	if got := sha256Hex(body); got != want {
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

func sha256Hex(b []byte) string {
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
			logUpdate("connect %s via %s: ok in %s", host, a, time.Since(start).Round(time.Millisecond))
			return conn, nil
		}
		failedAddrs.Store(a, true)
		logUpdate("connect %s via %s: %v after %s, trying next address", host, a, err, time.Since(start).Round(time.Millisecond))
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

// checksumFor finds name's sha256 in a checksums.txt ("<hex>  <name>" lines).
func checksumFor(sums, name string) string {
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

// restoreEnv carries uiState across the restart after a self-update.
const restoreEnv = "CCS_RESTORE_STATE"

// uiState is where the user was: which screen, the search text and cursor
// position within it, the selected conversation and the preview scroll.
// Screen is a name, so a screen added later only needs a case in
// screenName and restore.
type uiState struct {
	Screen        string `json:"screen"`
	Query         string `json:"query"`
	QueryCursor   int    `json:"query_cursor"`
	Selected      string `json:"selected"` // SessionID, so it survives a reordered list
	PreviewScroll int    `json:"preview_scroll"`
}

func (m model) screenName() string {
	if m.showUsage {
		return "usage"
	}
	return "list"
}

func (m model) uiState() uiState {
	s := uiState{
		Screen:        m.screenName(),
		Query:         m.textInput.Value(),
		QueryCursor:   m.textInput.Position(),
		PreviewScroll: m.previewScroll,
	}
	if len(m.filtered) > 0 {
		s.Selected = m.filtered[m.cursor].conv.SessionID
	}
	return s
}

// restore puts the user back where uiState says; anything that no longer
// applies (a conversation that's gone) is skipped.
func (m *model) restore(s uiState) {
	m.textInput.SetValue(s.Query)
	m.textInput.SetCursor(s.QueryCursor)
	m.updateFilter()
	for i, item := range m.filtered {
		if item.conv.SessionID == s.Selected {
			m.cursor = i
			break
		}
	}
	// Not clamped here: the window size isn't known yet at startup; the
	// preview clamps it when drawing.
	m.previewScroll = max(0, s.PreviewScroll)
	m.showUsage = s.Screen == "usage"
}

// notesCmd fetches the changelog off the UI goroutine.
func (m *model) notesCmd() tea.Cmd {
	m.notesFetching = true
	f := m.fetchNotes
	return func() tea.Msg { lines, err := f(); return notesMsg{lines, err} }
}

// notesRows is how many changelog lines the popup shows at once.
func (m model) notesRows() int { return max(m.height-10, 3) }

func (m *model) scrollNotes(by int) {
	m.notesScroll = min(max(m.notesScroll+by, 0), max(len(m.notes)-m.notesRows(), 0))
}

// notesPopup is the Ctrl+L changelog: every release in the feed, newest first,
// the installed one and any newer marked, scrolled by notesScroll.
func (m model) notesPopup() string {
	width := min(max(m.width-12, 30), 90)
	var body []string
	switch {
	case m.notesErr != "":
		body = []string{"couldn't fetch the changelog: " + truncate(m.notesErr, width-30), "", "\033[90m" + hints(keyEnter, "retry") + "\033[0m"}
	case m.notes == nil:
		body = []string{"fetching…"}
	default:
		end := min(m.notesScroll+m.notesRows(), len(m.notes))
		for _, l := range m.notes[m.notesScroll:end] {
			if strings.HasPrefix(l, " ") {
				body = append(body, ansi.Truncate(l, width, "…"))
				continue
			}
			mark := ""
			if newerVersion(l, version) {
				mark = " \033[32m(new)\033[0m"
			} else if strings.TrimPrefix(l, "v") == version {
				mark = " \033[90m(installed)\033[0m"
			}
			body = append(body, ansi.Truncate("\033[1m"+l+"\033[22m"+mark, width, "…"))
		}
	}
	for len(body) < min(m.notesRows(), max(len(m.notes), 1)) {
		body = append(body, "") // a steady height while scrolling
	}
	pos, keys := "", hints("^L/"+keyEsc, "close")
	if len(m.notes) > m.notesRows() {
		keys = hints("↑↓", "scroll", "^L/"+keyEsc, "close")
		pos = fmt.Sprintf("  %d–%d of %d", m.notesScroll+1, min(m.notesScroll+m.notesRows(), len(m.notes)), len(m.notes))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Width(width + 6). // fixed, so scrolling past longer or shorter lines doesn't resize it
		Render("\033[1mchangelog\033[0m\n\n" + strings.Join(body, "\n") +
			"\n\n\033[90m" + keys + pos + "\033[0m")
}

// updatePopup renders the update offer; View overlays it on any screen.
func (m model) updatePopup() string {
	body := fmt.Sprintf("ccs %s is available (you have v%s).\n\n", m.updateTo, version)
	if len(m.changelog) > 0 {
		body += m.changelogBlock() + "\n\n"
	}
	if m.updateErr != "" {
		body = fmt.Sprintf("Updating to %s failed:\n%s\nDetails: %s\n\n", m.updateTo, truncate(m.updateErr, 60), updateLogPath)
	}
	if m.upgrade != nil && m.updateErr != "" {
		body += hints(keyEnter, "retry", keyEsc, "later")
	} else if m.upgrade != nil {
		body += hints(keyEnter, "update and restart", keyEsc, "later", "^L", "changelog")
	} else {
		body += "Update with your package manager.    " + hints(keyEsc, "close")
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(1, 3).
		Render(body)
	return box
}

// changelogBlock fits the changelog into the popup: lines cut to the popup's
// width, and at most maxChangelogLines of them, then how many more.
func (m model) changelogBlock() string {
	width := min(max(m.width-12, 30), 80)
	lines := m.changelog
	var more string
	if len(lines) > maxChangelogLines {
		more = fmt.Sprintf("\n  … and %d more", len(lines)-maxChangelogLines+1)
		lines = lines[:maxChangelogLines-1]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Truncate(l, width, "…")
		if !strings.HasPrefix(l, " ") {
			out[i] = "\033[1m" + out[i] + "\033[22m"
		}
	}
	return strings.Join(out, "\n") + more
}

const maxChangelogLines = 12

// updateLogPath is where self-update steps are logged: each git/brew
// command with its full output, exit and duration, and each download with
// the address it connected to. Kept under 1MB (the previous file is .1).
var updateLogPath = func() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", "ccs", "update.log")
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "ccs", "update.log")
	}
	return filepath.Join(os.TempDir(), "ccs-update.log")
}()

var updateLogMu sync.Mutex

func logUpdate(format string, args ...any) {
	updateLogMu.Lock()
	defer updateLogMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(updateLogPath), 0o755); err != nil {
		return
	}
	if info, err := os.Stat(updateLogPath); err == nil && info.Size() > 1<<20 {
		os.Rename(updateLogPath, updateLogPath+".1")
	}
	f, err := os.OpenFile(updateLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s ccs %s: %s\n", time.Now().Format(time.RFC3339), version, fmt.Sprintf(format, args...))
}

func errOrOK(err error) any {
	if err == nil {
		return "ok"
	}
	return err
}

// indent prefixes each line of command output for the log.
func indent(out []byte) string {
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return ""
	}
	return "  | " + strings.ReplaceAll(text, "\n", "\n  | ")
}

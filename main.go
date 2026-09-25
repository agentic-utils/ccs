package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var version = "dev"

// Message represents a conversation message
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Ts   string `json:"ts"`
}

// Conversation represents a parsed conversation
type Conversation struct {
	SessionID      string    `json:"session_id"`
	Title          string    `json:"title"`           // custom-title (user-set) or ai-title
	IsCustomTitle  bool      `json:"is_custom_title"` // true only when Title came from a user-set custom-title
	Spawned        bool      `json:"spawned"`         // started by a script/another session (sdk-cli) or a team lead, not typed by you
	Cwd            string    `json:"cwd"`
	FirstTimestamp string    `json:"first_timestamp"`
	LastTimestamp  string    `json:"last_timestamp"`
	Messages       []Message `json:"messages"`
	FilePath       string    `json:"file_path"`      // Full path to the .jsonl file
	Size           int64     `json:"size"`           // .jsonl file size in bytes
	ContextTokens  int       `json:"context_tokens"` // conversation size as of the last reply (input + cache reads/writes)

	// Built once at parse and shared through parseCache, so a refresh
	// doesn't rebuild the search text of unchanged conversations.
	searchText, searchLower string

	// Incremental parsing state: bytes consumed up to the last complete line,
	// and whether a user line was seen (it decides Spawned).
	parsedBytes int64
	sawUser     bool
	tailApplied bool // an unterminated last line parsed as a record, so resuming at parsedBytes would repeat it

	// readAt is when this copy was read from disk (the stat before the read).
	// Between a full scan and the live tick, the later read wins; file size
	// can't decide it, since a prune legitimately shrinks a file.
	readAt time.Time
}

// RawMessage represents the JSON structure in conversation files
type RawMessage struct {
	Type    string `json:"type"`
	Cwd     string `json:"cwd"`
	Message struct {
		Content json.RawMessage `json:"content"`
		Usage   struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Timestamp   string `json:"timestamp"`
	IsMeta      bool   `json:"isMeta"`     // harness-injected (e.g. the local-command caveat), not typed
	Entrypoint  string `json:"entrypoint"` // cli / claude-desktop = interactive, sdk-cli = claude -p or SDK
	TeamName    string `json:"teamName"`   // set on teammate transcripts spawned by a team lead
	CustomTitle string `json:"customTitle"`
	AiTitle     string `json:"aiTitle"`
}

// TextContent for parsing content arrays
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// listItem holds display and search data for a conversation
type listItem struct {
	conv        Conversation
	searchText  string // All searchable content
	searchLower string // searchText lowercased once, for case-insensitive filtering
}

// selectedStyle highlights the cursor row. The rest of the UI is rendered with
// raw ANSI escapes in View/formatListItem/renderPreview.
var selectedStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("62")).
	Foreground(lipgloss.Color("230")).
	Bold(true)

// model is the bubbletea application state
type model struct {
	items           []listItem
	filtered        []listItem
	textInput       textinput.Model
	cursor          int
	previewScroll   int
	width           int
	height          int
	listHeight      int // Calculated visible list height
	selected        *Conversation
	fork            bool // exec the selected conversation with --fork-session
	quitting        bool
	claudeFlags     []string
	confirmDelete   bool  // Are we in delete confirmation mode?
	deleteIndex     int   // Index of item to delete
	confirmPrune    bool  // Are we in prune confirmation mode?
	pruneIndex      int   // Index of item to prune
	pruneSaved      int64 // Bytes the pending prune would reclaim (measured on Ctrl+X)
	renaming        bool  // Are we typing a new name?
	resuming        bool  // an Enter/Ctrl+F open is in flight
	renameIndex     int   // Index of item being renamed
	renameInput     textinput.Model
	errorMsg        string                     // Show deletion/prune errors
	preview         *previewCache              // memoised preview lines for the selected conversation
	hits            *hitCounter                // memoised per-query hit counts, keyed by SessionID
	lastFilterQuery string                     // lowercased query the current m.filtered was built from
	live            map[string]bool            // SessionIDs attached to a running claude process
	reload          func() ([]listItem, error) // re-scans conversations; nil disables auto-refresh
	gen             int                        // bumped by delete/prune/rename so an older in-flight refresh can't undo them
	refreshStarted  time.Time                  // when the in-flight scan began; zero when none
	lastRefresh     time.Time                  // when the list last matched disk (startup or a completed scan)
	lastKick        time.Time                  // last full scan started early because an unknown session went live
	refreshFailed   bool                       // the last scan errored; the list is from lastRefresh

	// Self-update. checkLatest nil disables the check (tests, dev builds);
	// upgrade installs tag and returns the binary to restart; nil means ccs
	// can't update this install itself (e.g. Nix), so only notify.
	checkLatest       func() (string, error)
	upgrade           *upgrader
	progress          *updateProgress // step + start time, written by the upgrade goroutine
	updateTo          string          // newer release tag found, "" if none
	updateShownAt     time.Time       // popup ignores keys for a moment so in-flight typing can't answer it
	updateOpen        bool
	updateErr         string // last install failure, shown in the popup with a retry
	updateCheckFailed bool
	updateHeld        bool // popup waited behind another prompt; restart its key grace when it shows
	updating          bool
	dismissed         string // tag the user said "later" to
	restart           string // after an upgrade: binary to exec once the TUI exits
}

// updateCheckInterval is how often ccs asks GitHub for a newer release. The
// unauthenticated API allows 60 requests/hour per IP, so not every refresh.
const updateCheckInterval = time.Hour

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
		u.prepare(tag) // failures are retried by install
		close(done)
	}()
}

func (u *upgrader) Install(tag string, step func(string)) (string, error) {
	u.mu.Lock()
	done := u.pending[tag]
	u.mu.Unlock()
	if done != nil {
		step("finishing download")
		<-done
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
				return runBrew(brew, "fetch", "agentic-utils/tap/ccs") // into brew's download cache
			},
			install: func(_ string, step func(string)) (string, error) {
				if err := refreshTap(brew, step); err != nil {
					return "", err
				}
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
	tap, err := exec.Command(brew, "--repository", "agentic-utils/tap").Output()
	if err == nil && exec.Command("git", "-C", strings.TrimSpace(string(tap)), "pull", "--ff-only", "--quiet").Run() == nil {
		return nil
	}
	step("brew update")
	return runBrew(brew, "update", "--quiet")
}

// runBrew runs brew without its own auto-update (we refresh the tap
// ourselves), returning brew's last output line as the error.
func runBrew(brew string, args ...string) error {
	cmd := exec.Command(brew, args...)
	cmd.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
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

func download(url string) ([]byte, error) {
	client := http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err == nil && len(body) > maxDownload {
		return nil, fmt.Errorf("download %s: larger than %dMB", url, maxDownload>>20)
	}
	return body, err
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

// refreshInterval is how often ccs re-scans conversations and live sessions.
// Changed files are reparsed whole, and active sessions are often 100MB+.
const refreshInterval = time.Minute

type refreshTickMsg struct{}

// liveInterval is how often ccs re-reads which sessions are live and picks
// up new lines in live (and just-exited) sessions. Cheap: ~10 small files
// plus one stat per live transcript, and only appended lines are parsed.
const liveInterval = 2 * time.Second

type liveTickMsg struct{}

type focusDoneMsg struct {
	pid   int
	found bool
}

// resumeDoneMsg reports how Enter/Ctrl+F opened a conversation: in a new
// tab/window (ccs stays up), or not at all (exec in place after quitting).
type resumeDoneMsg struct {
	conv   Conversation
	fork   bool
	opened bool
	err    error
}

type liveMsg struct {
	live    map[string]bool
	updated []Conversation // live conversations whose transcript grew
	unknown bool           // a live session isn't in the list yet
	gen     int
}

// liveReads marks transcripts with a live-tick read in flight.
var liveReads sync.Map

func liveTick() tea.Cmd {
	return tea.Tick(liveInterval, func(time.Time) tea.Msg { return liveTickMsg{} })
}

// liveCmd refreshes liveness and the content of live sessions off the UI
// goroutine. Sessions that were live last tick are checked too, so a
// session's final lines land promptly after claude exits.
func (m model) liveCmd() tea.Cmd {
	byID := make(map[string]Conversation, len(m.live))
	for _, item := range m.items {
		byID[item.conv.SessionID] = item.conv
	}
	wasLive, gen := maps.Clone(m.live), m.gen
	return func() tea.Msg {
		live := readLiveSessions()
		msg := liveMsg{live: live, gen: gen}
		check := make(map[string]bool, len(live)+len(wasLive))
		for id := range live {
			check[id] = true
		}
		for id := range wasLive {
			check[id] = true
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for id := range check {
			conv, ok := byID[id]
			if !ok {
				msg.unknown = msg.unknown || live[id]
				continue
			}
			// A read still stuck from an earlier tick (e.g. a hung mount)
			// isn't started again; the rest carry on.
			if _, busy := liveReads.LoadOrStore(conv.FilePath, true); busy {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer liveReads.Delete(conv.FilePath)
				if c, err := parseAppended(&conv); err == nil && c != nil && c.Size != conv.Size {
					mu.Lock()
					msg.updated = append(msg.updated, *c)
					mu.Unlock()
				}
			}()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(liveInterval): // deliver what's ready; stragglers land next tick
		}
		mu.Lock()
		defer mu.Unlock()
		msg.updated = slices.Clone(msg.updated)
		return msg
	}
}

// applyLive swaps updated conversations into the list, re-sorted by last
// activity, keeping the cursor on the same conversation. Unlike a full
// refresh it only re-matches and re-counts the changed rows, so a streaming
// session doesn't rebuild every cache on the UI goroutine each tick.
func (m *model) applyLive(updated []Conversation) {
	byID := make(map[string]listItem, len(updated))
	for _, item := range buildItems(updated) {
		byID[item.conv.SessionID] = item
	}
	items := make([]listItem, len(m.items))
	newMessages := make(map[string]bool, len(updated))
	for i, item := range m.items {
		// A full scan may have read this file after the live tick did.
		if u, ok := byID[item.conv.SessionID]; ok && !item.conv.readAt.After(u.conv.readAt) {
			newMessages[item.conv.SessionID] = len(u.conv.Messages) != len(item.conv.Messages)
			item = u
		} else {
			delete(byID, item.conv.SessionID)
		}
		items[i] = item
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].conv.LastTimestamp > items[j].conv.LastTimestamp
	})

	var selectedID string
	if len(m.filtered) > 0 {
		selectedID = m.filtered[m.cursor].conv.SessionID
	}
	shown := make(map[string]bool, len(m.filtered))
	for _, item := range m.filtered {
		shown[item.conv.SessionID] = true
	}
	query := strings.ToLower(m.textInput.Value())
	filtered := make([]listItem, 0, len(m.filtered)+len(updated))
	for _, item := range items {
		match := shown[item.conv.SessionID]
		if _, changed := byID[item.conv.SessionID]; changed {
			match = query == "" || strings.Contains(item.searchLower, query)
			// Most appends are tool calls: HITS only moves with new messages.
			if m.hits != nil && newMessages[item.conv.SessionID] {
				delete(m.hits.byID, item.conv.SessionID)
			}
		}
		if match {
			filtered = append(filtered, item)
		}
	}
	m.items, m.filtered = items, filtered
	m.cursor = min(m.cursor, max(0, len(filtered)-1))
	found := false
	for i, item := range filtered {
		if item.conv.SessionID == selectedID {
			m.cursor, found = i, true
			break
		}
	}
	// The selected preview can shrink (or be a different conversation), so a
	// stored scroll offset past its end would make Ctrl+K seem stuck.
	if _, changed := byID[selectedID]; changed || !found {
		m.previewScroll = min(m.previewScroll, m.maxPreviewScroll())
	}
}

type refreshMsg struct {
	items []listItem
	err   error // scan failed: keep the current list
	live  map[string]bool
	gen   int  // m.gen when the scan started
	early bool // started by the live tick, outside the one-minute schedule
}

func refreshTick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

// getSessionsDir returns where Claude Code records running sessions, one
// <pid>.json per process. Declared as a variable so tests can override it.
var getSessionsDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "sessions")
}

// readLiveSessions returns the SessionIDs of conversations currently open in a
// running claude process. A session file whose pid is gone is a stale leftover.
// ponytail: a recycled pid can make a stale file look live until claude cleans it up.
func readLiveSessions() map[string]bool {
	live := make(map[string]bool)
	for id := range liveSessionPIDs() {
		live[id] = true
	}
	return live
}

// liveSessionPIDs maps each live SessionID to the pid of its claude process.
func liveSessionPIDs() map[string]int {
	live := make(map[string]int)
	starts := make(map[string]string)
	files, _ := filepath.Glob(filepath.Join(getSessionsDir(), "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			Pid       int    `json:"pid"`
			SessionID string `json:"sessionId"`
			ProcStart string `json:"procStart"` // UTC, e.g. "Wed Sep 23 16:52:33 2026"
		}
		if json.Unmarshal(data, &s) != nil || s.SessionID == "" || s.Pid <= 0 {
			continue
		}
		// Signal 0 probes the pid; EPERM still means the process exists.
		if err := syscall.Kill(s.Pid, 0); err == nil || err == syscall.EPERM {
			live[s.SessionID] = s.Pid
			starts[s.SessionID] = s.ProcStart
		}
	}
	// A pid recycled by an unrelated process started at a different time.
	actual := processStartTimes(live)
	for id, pid := range live {
		if !sameStart(starts[id], actual[pid]) {
			delete(live, id)
		}
	}
	return live
}

// processStartTimes returns each pid's start time from one ps call. Pids ps
// can't report are left out.
var processStartTimes = func(live map[string]int) map[int]time.Time {
	out := make(map[int]time.Time)
	if len(live) == 0 {
		return out
	}
	pids := make([]string, 0, len(live))
	for _, pid := range live {
		pids = append(pids, strconv.Itoa(pid))
	}
	// Fixed lstart layout, in UTC like procStart: local time is ambiguous in
	// the hour clocks go back.
	raw, _ := runBounded(2*time.Second, []string{"LC_ALL=C", "TZ=UTC"}, "ps", "-o", "pid=,lstart=", "-p", strings.Join(pids, ","))
	for _, line := range strings.Split(string(raw), "\n") {
		pidStr, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(pidStr)
		if !ok || err != nil {
			continue
		}
		if t, err := time.Parse(time.ANSIC, strings.Join(strings.Fields(rest), " ")); err == nil {
			out[pid] = t
		}
	}
	return out
}

// sameStart compares the session file's procStart (UTC) with the process's
// actual start. Unknown on either side counts as a match, so a format change
// or ps failure never hides a live session.
func sameStart(recorded string, actual time.Time) bool {
	want, err := time.Parse(time.ANSIC, strings.Join(strings.Fields(recorded), " "))
	if err != nil || actual.IsZero() {
		return true
	}
	d := actual.Sub(want)
	return d > -2*time.Second && d < 2*time.Second
}

// refreshStalled reports a scan running far longer than normal (e.g. blocked
// on a hung network mount under ~/.claude/projects). It can't be cancelled,
// so the header just says so rather than showing a silently frozen list.
// refreshNote says what the background refresh is doing, for the header:
// "refreshing…", "refreshed 20s ago", "refresh failed 3m ago". Empty when
// auto-refresh is off, or when a stall is reported instead.
func (m model) refreshNote() string {
	switch {
	case m.reload == nil || m.refreshStalled():
		return ""
	case !m.refreshStarted.IsZero():
		return " · refreshing…"
	case m.lastRefresh.IsZero():
		return ""
	}
	ago := time.Since(m.lastRefresh)
	when := fmt.Sprintf("%ds", int(ago.Seconds()))
	if ago >= time.Minute {
		when = fmt.Sprintf("%dm", int(ago.Minutes()))
	}
	if m.refreshFailed {
		return " · refresh failed, list from " + when + " ago"
	}
	return " · refreshed " + when + " ago"
}

// prompting reports an open rename, delete or prune prompt.
func (m model) prompting() bool {
	return m.renaming || m.confirmDelete || m.confirmPrune
}

func (m model) refreshStalled() bool {
	return !m.refreshStarted.IsZero() && time.Since(m.refreshStarted) > 5*time.Minute
}

// isLive re-reads the session files so a prune/rename decision never trusts a
// liveness snapshot up to a refresh interval old. A session already marked live
// (e.g. one ccs just opened, before claude writes its session file) stays live
// until the next refresh replaces m.live.
func (m *model) isLive(id string) bool {
	fresh := readLiveSessions()
	if m.live[id] {
		fresh[id] = true
	}
	m.live = fresh
	return fresh[id]
}

// keepNewer returns scanned, but where the list holds a copy of a
// conversation read from disk after the scan read it (the live tick got
// there later), keeps that copy, so a slow scan can't roll it back.
func keepNewer(current, scanned []listItem) []listItem {
	have := make(map[string]listItem, len(current))
	for _, item := range current {
		have[item.conv.SessionID] = item
	}
	out := make([]listItem, len(scanned))
	for i, item := range scanned {
		if cur, ok := have[item.conv.SessionID]; ok && cur.conv.readAt.After(item.conv.readAt) {
			item = cur
		}
		out[i] = item
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].conv.LastTimestamp > out[j].conv.LastTimestamp
	})
	return out
}

// startRefresh begins a full scan off the UI goroutine.
func (m *model) startRefresh(early bool) tea.Cmd {
	m.refreshStarted = time.Now()
	reload, gen := m.reload, m.gen
	return func() tea.Msg {
		items, err := reload()
		return refreshMsg{items: items, err: err, live: readLiveSessions(), gen: gen, early: early}
	}
}

// applyRefresh swaps in freshly loaded items, keeping the cursor on the same
// conversation and re-running the current filter.
func (m *model) applyRefresh(msg refreshMsg) {
	if msg.err != nil {
		return
	}
	var selectedID string
	if len(m.filtered) > 0 {
		selectedID = m.filtered[m.cursor].conv.SessionID
	}
	prevScroll := m.previewScroll
	m.items = msg.items
	m.lastFilterQuery = "" // force a full rescan, not incremental narrowing
	m.hits = &hitCounter{byID: make(map[string]int)}
	m.preview = &previewCache{}
	m.updateFilter()
	for i, item := range m.filtered {
		if item.conv.SessionID == selectedID {
			m.cursor = i
			m.previewScroll = min(prevScroll, m.maxPreviewScroll())
			break
		}
	}
}

// previewCache memoises buildPreviewLines for the selected conversation so the
// preview isn't rebuilt (scanning every message) on every frame. It lives behind
// a pointer so it survives the value-receiver copies of model that View makes.
type previewCache struct {
	key   string
	lines []string
}

// hitCounter memoises HITS (messages containing the query) per conversation for
// the current query, so formatListItem doesn't rescan every visible row's
// messages on every frame. Pointer-held so it survives model value copies.
type hitCounter struct {
	query string
	byID  map[string]int
}

// countHits is the number of a conversation's messages containing query.
func countHits(conv Conversation, query string) int {
	queryLower := strings.ToLower(query)
	n := 0
	for _, msg := range conv.Messages {
		if strings.Contains(strings.ToLower(msg.Text), queryLower) {
			n++
		}
	}
	return n
}

// hitCount returns the memoised hit count for item under the current query.
func (m model) hitCount(item listItem) int {
	query := m.textInput.Value()
	if query == "" {
		return 0
	}
	if m.hits == nil { // model built without initialModel (e.g. tests)
		return countHits(item.conv, query)
	}
	if m.hits.query != query {
		m.hits.query = query
		m.hits.byID = make(map[string]int)
	}
	id := item.conv.SessionID
	if h, ok := m.hits.byID[id]; ok {
		return h
	}
	h := countHits(item.conv, query)
	m.hits.byID[id] = h
	return h
}

func initialModel(items []listItem, filterQuery string, claudeFlags []string) model {
	ti := textinput.New()
	ti.Placeholder = "type to search..."
	ti.Prompt = "> "
	ti.Focus()
	ti.SetValue(filterQuery)
	ti.Width = 40

	m := model{
		items:       items,
		textInput:   ti,
		claudeFlags: claudeFlags,
		preview:     &previewCache{},
		hits:        &hitCounter{byID: make(map[string]int)},
	}
	m.updateFilter()
	return m
}

// previewLines returns the preview lines for the selected conversation,
// rebuilding only when the selection or query changes. Keyed by SessionID (not
// cursor index) so it stays correct when the filtered list shifts.
func (m model) previewLines() []string {
	if len(m.filtered) == 0 {
		return nil
	}
	conv := m.filtered[m.cursor].conv
	query := m.textInput.Value()
	if m.preview == nil { // model built without initialModel (e.g. tests)
		return buildPreviewLines(conv, query)
	}
	key := fmt.Sprintf("%s\x00%d\x00%s", conv.SessionID, conv.Size, query)
	if m.preview.key != key {
		m.preview.key = key
		m.preview.lines = buildPreviewLines(conv, query)
	}
	return m.preview.lines
}

func (m *model) updateFilter() {
	queryLower := strings.ToLower(m.textInput.Value())
	if queryLower == "" {
		// Make a copy to avoid sharing backing array with m.items
		m.filtered = make([]listItem, len(m.items))
		copy(m.filtered, m.items)
	} else {
		// Incremental narrowing: if the new query contains the previous one, every
		// item matching the new query already matched the old one, so filter the
		// previous (smaller) result set instead of rescanning every conversation.
		source := m.items
		if m.lastFilterQuery != "" && strings.Contains(queryLower, m.lastFilterQuery) {
			source = m.filtered
		}
		next := make([]listItem, 0, len(source))
		for _, item := range source {
			if strings.Contains(item.searchLower, queryLower) {
				next = append(next, item)
			}
		}
		m.filtered = next
	}
	m.lastFilterQuery = queryLower
	// Keep cursor in bounds
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.previewScroll = 0
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if m.reload != nil {
		cmds = append(cmds, refreshTick(), liveTick())
	}
	if m.checkLatest != nil {
		cmds = append(cmds, m.checkUpdateCmd())
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Calculate visible list height
		m.listHeight = m.height * 30 / 100
		if m.listHeight < 3 {
			m.listHeight = 3
		}
		// Clear so a shrink doesn't leave wider stale rows behind.
		return m, tea.ClearScreen

	case refreshTickMsg:
		if !m.refreshStarted.IsZero() { // an early scan is running; keep the schedule
			return m, refreshTick()
		}
		return m, m.startRefresh(false)

	case resumeDoneMsg:
		m.resuming = false
		switch {
		case msg.err != nil:
			m.errorMsg = fmt.Sprintf("Opening a new tab timed out or failed (it may still have opened): %v", msg.err)
		case msg.opened && !msg.fork:
			// Live before claude writes its session file. Copied, not mutated:
			// an in-flight live tick may be reading the old map.
			live := maps.Clone(m.live)
			if live == nil {
				live = make(map[string]bool)
			}
			live[msg.conv.SessionID] = true
			m.live = live
		case !msg.opened:
			conv := msg.conv
			m.selected, m.fork, m.quitting = &conv, msg.fork, true
			return m, tea.Quit
		}
		return m, nil

	case focusDoneMsg:
		m.resuming = false
		if !msg.found {
			m.errorMsg = "Session is open in claude but its terminal wasn't found - Ctrl+F forks it"
		}
		return m, nil

	case liveTickMsg:
		return m, m.liveCmd()

	case liveMsg:
		m.live = msg.live
		if len(msg.updated) > 0 && msg.gen == m.gen && !m.prompting() {
			m.applyLive(msg.updated)
		}
		cmds := []tea.Cmd{liveTick()}
		// A session that went live but isn't listed (a brand-new conversation):
		// scan now rather than at the next minute, at most every 15s.
		if msg.unknown && m.refreshStarted.IsZero() && time.Since(m.lastKick) > 15*time.Second {
			m.lastKick = time.Now()
			cmds = append(cmds, m.startRefresh(true))
		}
		return m, tea.Batch(cmds...)

	case refreshMsg:
		m.refreshStarted = time.Time{}
		m.refreshFailed = msg.err != nil
		m.live = msg.live // liveness is independent of the list, never stale-dropped
		// Delete/prune/rename prompts hold an index into m.filtered, so don't
		// reshuffle it under them; and a scan that started before a delete,
		// prune or rename would undo it. The next tick catches up.
		if msg.gen == m.gen && !m.prompting() && msg.err == nil {
			m.applyRefresh(refreshMsg{items: keepNewer(m.items, msg.items), live: msg.live})
			m.lastRefresh = time.Now()
		}
		if msg.early {
			return m, nil // the one-minute chain is still ticking on its own
		}
		return m, refreshTick()

	case updateCheckTickMsg:
		return m, m.checkUpdateCmd()

	case latestMsg:
		m.updateCheckFailed = msg.err != nil
		if newerVersion(msg.tag, version) && msg.tag != m.dismissed && !m.updating {
			if msg.tag != m.updateTo {
				m.updateOpen = true
				m.updateShownAt = time.Now()
			}
			m.updateTo = msg.tag
			if m.upgrade != nil {
				m.upgrade.Prepare(msg.tag) // download now so Enter only has to install
			}
		}
		return m, tea.Tick(updateCheckInterval, func(time.Time) tea.Msg { return updateCheckTickMsg{} })

	case updatingTickMsg:
		if m.updating {
			return m, updatingTick() // redraw so the step and seconds stay live
		}
		return m, nil

	case upgradeDoneMsg:
		m.updating = false
		if msg.err != nil {
			// Keep the offer open with the reason, so the failure can't vanish
			// on the next keypress and Enter retries.
			m.updateErr = msg.err.Error()
			m.updateOpen = true
			m.updateShownAt = time.Now()
			return m, nil
		}
		m.restart = msg.path
		m.quitting = true
		return m, tea.Quit

	case tea.KeyMsg:
		// The popup waits behind rename/delete/prune prompts, so it never takes
		// their keys; once they close, it gets a fresh grace period.
		if m.updateOpen && m.prompting() {
			m.updateHeld = true
		} else if m.updateOpen {
			if m.updateHeld {
				m.updateHeld = false
				m.updateShownAt = time.Now()
			}
			if time.Since(m.updateShownAt) < updateKeyGrace {
				return m, nil
			}
			switch msg.String() {
			case "enter":
				m.updateOpen = false
				if m.upgrade == nil {
					return m, nil
				}
				m.updating = true
				m.updateErr = ""
				m.progress = &updateProgress{step: "starting", started: time.Now()}
				upgrade, tag, progress := m.upgrade, m.updateTo, m.progress
				return m, tea.Batch(func() tea.Msg {
					path, err := upgrade.Install(tag, progress.set)
					return upgradeDoneMsg{path, err}
				}, updatingTick())
			case "esc":
				m.updateOpen = false
				m.dismissed = m.updateTo
				m.updateTo = ""
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			}
			return m, nil
		}
		if m.renaming {
			switch msg.String() {
			case "enter":
				if m.renameIndex < len(m.filtered) && m.isLive(m.filtered[m.renameIndex].conv.SessionID) {
					m.renaming = false
					m.errorMsg = "Session is open in claude - use /rename there"
					return m, nil
				}
				m.renameConversation(strings.TrimSpace(m.renameInput.Value()))
				return m, nil
			case "esc", "ctrl+c":
				m.renaming = false
				return m, nil
			}
			var cmd tea.Cmd
			m.renameInput, cmd = m.renameInput.Update(msg)
			return m, cmd
		}

		// Handle delete confirmation mode
		if m.confirmDelete {
			switch msg.String() {
			case "y", "Y":
				m.deleteConversation()
				return m, nil
			case "n", "N", "esc":
				m.confirmDelete = false
				return m, nil
			}
			return m, nil // Ignore all other keys
		}

		// Handle prune confirmation mode
		if m.confirmPrune {
			switch msg.String() {
			case "y", "Y":
				if m.pruneIndex < len(m.filtered) && m.isLive(m.filtered[m.pruneIndex].conv.SessionID) {
					m.confirmPrune = false
					m.errorMsg = "Session is open in claude - quit it before pruning"
					return m, nil
				}
				m.pruneConversation()
				return m, nil
			case "n", "N", "esc":
				m.confirmPrune = false
				return m, nil
			}
			return m, nil // Ignore all other keys
		}

		// Clear error message on any keypress in normal mode
		if m.errorMsg != "" {
			m.errorMsg = ""
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit

		case "enter":
			if len(m.filtered) == 0 {
				m.quitting = true
				return m, tea.Quit
			}
			return m, m.resumeCmd(m.filtered[m.cursor].conv, false)

		case "ctrl+f":
			// Fork: resume into a new session id, leaving the original as is.
			if len(m.filtered) == 0 {
				return m, nil
			}
			return m, m.resumeCmd(m.filtered[m.cursor].conv, true)

		case "ctrl+d":
			if len(m.filtered) > 0 {
				m.confirmDelete = true
				m.deleteIndex = m.cursor
			}
			return m, nil

		case "ctrl+r":
			if len(m.filtered) > 0 {
				conv := m.filtered[m.cursor].conv
				// A running claude re-appends its own title and would undo ours.
				if m.isLive(conv.SessionID) {
					m.errorMsg = "Session is open in claude - use /rename there"
					return m, nil
				}
				m.renaming = true
				m.renameIndex = m.cursor
				m.renameInput = textinput.New()
				m.renameInput.Prompt = "Rename: "
				m.renameInput.Width = 50
				m.renameInput.SetValue(conv.Title)
				m.renameInput.Focus()
				return m, textinput.Blink
			}
			return m, nil

		case "ctrl+x":
			if len(m.filtered) > 0 {
				// Prune rewrites the file; a running claude's appends during
				// the rewrite would be lost.
				if m.isLive(m.filtered[m.cursor].conv.SessionID) {
					m.errorMsg = "Session is open in claude - quit it before pruning"
					return m, nil
				}
				// Measure the projected saving so the prompt can show it.
				// ponytail: reads the file once now (and again on confirm) - a
				// multi-GB file briefly blocks, acceptable for a manual action.
				st, err := pruneFile(m.filtered[m.cursor].conv.FilePath, false, pruneOpts{dropSnapshots: true, stripToolResults: true})
				if err != nil {
					m.errorMsg = fmt.Sprintf("Prune preview failed: %v", err)
					return m, nil
				}
				m.confirmPrune = true
				m.pruneIndex = m.cursor
				m.pruneSaved = st.bytesIn - st.bytesOut
			}
			return m, nil

		case "up", "ctrl+p":
			if m.cursor > 0 {
				m.cursor--
				m.previewScroll = 0
			}
			return m, nil

		case "down", "ctrl+n":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m.previewScroll = 0
			}
			return m, nil

		case "pgup", "ctrl+k":
			m.previewScroll = max(0, m.previewScroll-10)
			return m, nil

		case "pgdown", "ctrl+j":
			m.previewScroll = min(m.previewScroll+10, m.maxPreviewScroll())
			return m, nil

		case "ctrl+u":
			m.textInput.SetValue("")
			m.updateFilter()
			return m, nil
		}
	}

	// Update text input
	var cmd tea.Cmd
	prevValue := m.textInput.Value()
	m.textInput, cmd = m.textInput.Update(msg)
	if m.textInput.Value() != prevValue {
		m.updateFilter()
	}
	return m, cmd
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	var b strings.Builder

	// The list spans the full terminal width; TOPIC flexes to fill it.
	tableWidth := m.width

	// Title line with help right-aligned
	note := m.refreshNote()
	if m.updateCheckFailed && m.updateTo == "" {
		note += " · update check failed"
	}
	status := ""
	if m.refreshStalled() {
		status = fmt.Sprintf(" · refresh stalled %dm", int(time.Since(m.refreshStarted).Minutes()))
	}
	if m.updating {
		status = fmt.Sprintf(" · updating to %s: %s...", m.updateTo, m.progress)
	}
	title := fmt.Sprintf("ccs · claude code search · %s%s%s", version, note, status)
	help := "Resume:Enter Fork:Ctrl+F Rename:Ctrl+R Delete:Ctrl+D Prune:Ctrl+X Scroll:Ctrl+J/K Exit:Esc"
	titlePadding := tableWidth - 2 - len(title) - len(help)
	if titlePadding < 1 {
		titlePadding = 1
	}
	b.WriteString(fmt.Sprintf("  \033[1;36mccs\033[0m \033[90m· claude code search · %s%s\033[0m\033[33m%s\033[0m\033[90m%s%s\033[0m\n",
		version, note, status, strings.Repeat(" ", titlePadding), help))

	// Search line or delete confirmation
	var sections []string
	var inputSection string
	if m.renaming {
		sections = append(sections, "  "+m.renameInput.View()+"  \033[90mEnter:save Esc:cancel\033[0m")
	} else if m.confirmPrune {
		conv := m.filtered[m.pruneIndex].conv
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")). // Amber
			Render(fmt.Sprintf("Prune \"%s\"? %s -> %s, saves %s (keeps dialogue). [y/N]",
				truncate(getTopic(conv), 32), formatBytes(conv.Size), formatBytes(conv.Size-m.pruneSaved), formatBytes(m.pruneSaved)))
		sections = append(sections, "  "+inputSection)
	} else if m.confirmDelete {
		conv := m.filtered[m.deleteIndex].conv
		topic := getTopic(conv)
		liveWarning := ""
		if m.live[conv.SessionID] {
			liveWarning = " It is open in claude."
		}
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // Red
			Render(fmt.Sprintf("Delete conversation \"%s\"?%s [y/N]", truncate(topic, 50), liveWarning))
		sections = append(sections, "  "+inputSection)
	} else {
		count := fmt.Sprintf("(%d/%d)", len(m.filtered), len(m.items))
		searchPadding := tableWidth - 2 - 2 - 40 - len(count) - 1 // 2 for indent, 2 for "> ", 40 for textInput, -1 to shift left
		if searchPadding < 1 {
			searchPadding = 1
		}
		inputSection = fmt.Sprintf("  %s%s\033[90m%s\033[0m", m.textInput.View(), strings.Repeat(" ", searchPadding), count)
		sections = append(sections, inputSection)
	}

	// Show error message if set
	if m.errorMsg != "" {
		errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
		sections = append(sections, "  "+errorStyle.Render(m.errorMsg))
	}

	b.WriteString(strings.Join(sections, "\n"))
	b.WriteString("\n\n")

	// Calculate heights
	listHeight := m.height * 30 / 100
	if listHeight < 3 {
		listHeight = 3
	}
	previewHeight := m.height - listHeight - 6 // 6 for title + search + blank + header + borders

	// Column headers
	b.WriteString(fmt.Sprintf("  \033[90m%-*s  %-*s  %-*s  %*s  %*s  %*s  %*s\033[0m\n",
		colWhen, "WHEN", colProject, "PROJECT", m.topicColWidth(), strings.Repeat(" ", colMarks)+"TOPIC", colSize, "SIZE", colCtx, "CTX", colMsgs, "MSGS", colHits, "HITS"))
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	visibleItems := listHeight
	start := 0
	if m.cursor >= visibleItems {
		start = m.cursor - visibleItems + 1
	}

	for i := start; i < min(start+visibleItems, len(m.filtered)); i++ {
		item := m.filtered[i]
		isSelected := i == m.cursor
		line := m.formatListItem(item, isSelected)

		if isSelected {
			// Pad to full width for selection highlight
			line = padRight("> "+line, m.width)
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString("  " + line)
		}
		b.WriteString("\n")
	}

	// Fill remaining list space
	for i := len(m.filtered) - start; i < visibleItems; i++ {
		b.WriteString("\n")
	}

	// Preview section
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	if m.updateOpen && !m.prompting() {
		b.WriteString(m.updatePopup())
	} else if len(m.filtered) > 0 {
		preview := m.renderPreview(m.filtered[m.cursor], previewHeight)
		b.WriteString(preview)
	}

	return b.String()
}

// updatePopup renders the update offer in place of the preview pane.
func (m model) updatePopup() string {
	body := fmt.Sprintf("ccs %s is available (you have v%s).\n\n", m.updateTo, version)
	if m.updateErr != "" {
		body = fmt.Sprintf("Updating to %s failed:\n%s\n\n", m.updateTo, truncate(m.updateErr, 60))
	}
	if m.upgrade != nil && m.updateErr != "" {
		body += "Enter: retry    Esc: later"
	} else if m.upgrade != nil {
		body += "Enter: update and restart    Esc: later"
	} else {
		body += "Update with your package manager.    Esc: close"
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
		Padding(1, 3).
		Render(body)
	return "\n" + lipgloss.PlaceHorizontal(m.width, lipgloss.Center, box)
}

// Fixed list column widths. TOPIC is the flex column - it absorbs the rest of
// the terminal width (see topicColWidth).
const (
	colWhen    = 8 // longest is "12mo ago"
	colProject = 22
	colCtx     = 5
	colMsgs    = 5
	colHits    = 4
	colSize    = 6
	colGap     = 2 // spaces between columns
	listIndent = 2 // leading "  " / "> " on each row
	numGaps    = 6
	colMarks   = 3 // status icons (● ⚙) packed right, then a space
)

// topicColWidth flexes the TOPIC column to fill the terminal width.
func (m model) topicColWidth() int {
	used := listIndent + colWhen + colProject + colCtx + colMsgs + colHits + colSize + numGaps*colGap
	if w := m.width - used; w > 10 {
		return w
	}
	return 10
}

func (m model) formatListItem(item listItem, selected bool) string {
	project := item.conv.Cwd
	if idx := strings.LastIndex(project, "/"); idx >= 0 {
		project = project[idx+1:]
	}
	project = truncate(project, colProject)

	// Status icons, packed right against the topic so titles stay aligned:
	// ● open in a running claude, ⚙ started by a script or another session.
	// ponytail: the glyphs are ambiguous-width, so a marked row may sit one
	// cell off on CJK-width terminals - cosmetic only.
	var icons, colouredIcons string
	n := 0
	for _, s := range []struct {
		on           bool
		glyph, color string
	}{{m.live[item.conv.SessionID], "●", "32"}, {item.conv.Spawned, "⚙", "90"}} {
		if !s.on {
			continue
		}
		n++
		icons += s.glyph
		colouredIcons += "\033[" + s.color + "m" + s.glyph + "\033[0m"
	}
	pad := strings.Repeat(" ", colMarks-1-n)
	marks, colouredMarks := pad+icons+" ", pad+colouredIcons+" "
	tw := m.topicColWidth()
	// A trailing ✍ marks a name you set (not the ai-title Claude gives almost
	// every session); the title is cut short so it always stays visible.
	topic := truncate(getTopic(item.conv), tw-colMarks)
	if item.conv.IsCustomTitle {
		topic = truncate(getTopic(item.conv), tw-colMarks-2) + " ✍"
	}

	// Message count
	msgs := len(item.conv.Messages)

	// Number of messages containing the query (memoised per query).
	hits := m.hitCount(item)

	size := formatBytes(item.conv.Size)

	when := formatAgo(item.conv.LastTimestamp, time.Now())

	ctx := formatTokens(item.conv.ContextTokens)

	// Format: when | project | topic | size | ctx | msgs | hits (aligned columns)
	if selected {
		return fmt.Sprintf("%-*s  %-*s  %s%-*s  %*s  %*s  %*d  %*d",
			colWhen, when, colProject, project, marks, tw-colMarks, topic, colSize, size, colCtx, ctx, colMsgs, msgs, colHits, hits)
	}
	// Pad before colouring so the escape codes don't eat into the column width.
	topic = colouredMarks + padRight(topic, tw-colMarks)
	return fmt.Sprintf("\033[90m%-*s\033[0m  \033[1;33m%-*s\033[0m  %s  \033[35m%*s\033[0m  \033[34m%*s\033[0m  %*d  \033[36m%*d\033[0m",
		colWhen, when, colProject, project, topic, colSize, size, colCtx, ctx, colMsgs, msgs, colHits, hits)
}

// buildPreviewLines builds the scrollable message lines of a conversation
// preview (everything below the fixed header). Shared by renderPreview and
// maxPreviewScroll so the render and the scroll-clamp can never disagree on how
// far the preview can scroll.
func buildPreviewLines(conv Conversation, query string) []string {
	var msgLines []string

	// Find messages containing the query
	queryLower := strings.ToLower(query)
	matchSet := make(map[int]bool)
	if query != "" {
		for i, msg := range conv.Messages {
			if strings.Contains(strings.ToLower(msg.Text), queryLower) {
				matchSet[i] = true
			}
		}
	}

	// Build set of indices to show
	showSet := make(map[int]bool)

	// Always show first 2 and last 2 messages
	for i := 0; i < 2 && i < len(conv.Messages); i++ {
		showSet[i] = true
	}
	for i := len(conv.Messages) - 2; i < len(conv.Messages); i++ {
		if i >= 0 {
			showSet[i] = true
		}
	}

	// Add matches with context
	for idx := range matchSet {
		if idx > 0 {
			showSet[idx-1] = true
		}
		showSet[idx] = true
		if idx < len(conv.Messages)-1 {
			showSet[idx+1] = true
		}
	}

	// Display messages with gaps
	lastShown := -1
	for i := 0; i < len(conv.Messages); i++ {
		if !showSet[i] {
			continue
		}

		if lastShown >= 0 && i > lastShown+1 {
			skipped := i - lastShown - 1
			msgLines = append(msgLines, fmt.Sprintf("\033[90m    ... %d messages ...\033[0m", skipped))
			msgLines = append(msgLines, "")
		} else if lastShown == -1 && i > 0 {
			msgLines = append(msgLines, fmt.Sprintf("\033[90m    ... %d earlier messages\033[0m", i))
			msgLines = append(msgLines, "")
		}

		msg := conv.Messages[i]
		ts := formatTimestamp(msg.Ts)
		var prefix string
		if matchSet[i] {
			if msg.Role == "user" {
				prefix = fmt.Sprintf("\033[1;32m>>> %s User:\033[0m", ts) // Bold green
			} else {
				prefix = fmt.Sprintf("\033[1;34m>>> %s Claude:\033[0m", ts) // Bold blue
			}
		} else {
			if msg.Role == "user" {
				prefix = fmt.Sprintf("\033[32m    %s User:\033[0m", ts) // Green
			} else {
				prefix = fmt.Sprintf("\033[34m    %s Claude:\033[0m", ts) // Blue
			}
		}

		msgLines = append(msgLines, prefix)
		text := msg.Text
		if r := []rune(text); len(r) > 500 {
			text = string(r[:500]) + "... (truncated)" // slice on runes, not bytes
		}
		for _, line := range strings.Split(text, "\n") {
			msgLines = append(msgLines, "    "+highlight(line, query))
		}
		msgLines = append(msgLines, "")

		lastShown = i
	}

	if lastShown < len(conv.Messages)-1 {
		remaining := len(conv.Messages) - lastShown - 1
		msgLines = append(msgLines, fmt.Sprintf("\033[90m    ... %d more messages\033[0m", remaining))
	}

	return msgLines
}

// maxPreviewScroll is the furthest the preview of the current selection can
// scroll - one line short of the rendered message-line count.
func (m model) maxPreviewScroll() int {
	if len(m.filtered) == 0 {
		return 0
	}
	return max(0, len(m.previewLines())-1)
}

func (m model) renderPreview(item listItem, height int) string {
	query := m.textInput.Value()
	conv := item.conv

	// Fixed header (always visible)
	var header []string
	header = append(header, "\033[1;33mProject:\033[0m "+highlight(conv.Cwd, query))
	if conv.Title != "" {
		header = append(header, "\033[1;33mName:\033[0m    "+highlight(conv.Title, query))
	}
	header = append(header, "\033[1;33mSession:\033[0m "+highlight(conv.SessionID, query))
	header = append(header, "")

	msgLines := m.previewLines() // memoised; item is always the selected conversation

	// Apply scroll to messages only (header stays fixed). Clamp locally for this
	// render; the persisted m.previewScroll is bounded in Update via
	// maxPreviewScroll (this method has a value receiver, so a write here would
	// be discarded).
	msgHeight := height - len(header)
	if msgHeight < 1 {
		msgHeight = 1
	}
	scroll := min(m.previewScroll, max(0, len(msgLines)-1))
	end := min(scroll+msgHeight, len(msgLines))
	visibleMsgLines := msgLines[scroll:end]

	// Combine header + scrolled messages
	allLines := append(header, visibleMsgLines...)
	return strings.Join(allLines, "\n")
}

func highlight(text, query string) string {
	if query == "" {
		return text
	}
	tr := []rune(text)
	lr := []rune(strings.ToLower(text))
	queryLower := strings.ToLower(query)
	qr := []rune(queryLower)

	// Match on runes so multibyte text (CJK, emoji) is never sliced mid-rune.
	// ponytail: a handful of runes change length when lowercased (İ, Kelvin K),
	// which breaks the lr/tr index alignment - bail to plain text rather than
	// emit corrupted bytes. Highlighting those is not worth the complexity.
	if len(lr) != len(tr) || len(qr) == 0 {
		return text
	}

	var result strings.Builder
	for i := 0; i < len(tr); {
		if i+len(qr) <= len(tr) && string(lr[i:i+len(qr)]) == queryLower {
			// Yellow background, black text for highlight
			result.WriteString("\033[43;30m")
			result.WriteString(string(tr[i : i+len(qr)]))
			result.WriteString("\033[0m")
			i += len(qr)
		} else {
			result.WriteRune(tr[i])
			i++
		}
	}
	return result.String()
}

func padRight(s string, length int) string {
	r := []rune(s)
	if len(r) >= length {
		return string(r[:length])
	}
	// ponytail: pads by rune count, not display width; CJK/emoji rows can still
	// look a cell narrow. Swap in go-runewidth if column alignment matters.
	return s + strings.Repeat(" ", length-len(r))
}

// ============================================================================
// Data loading (preserved from original)
// ============================================================================

// getProjectsDir returns the path to the Claude projects directory
// Declared as a variable so it can be overridden in tests
var getProjectsDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func extractText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str
	}

	var arr []TextContent
	if err := json.Unmarshal(content, &arr); err == nil {
		var parts []string
		for _, item := range arr {
			if item.Type == "text" && item.Text != "" {
				parts = append(parts, item.Text)
			}
		}
		return strings.Join(parts, " ")
	}

	return ""
}

func parseConversationFile(path string, cutoff time.Time, maxSize int64) (*Conversation, error) {
	readAt := time.Now()
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(info.Name(), "agent-") {
		return nil, nil
	}

	// Skip files larger than maxSize (0 means no limit)
	if maxSize > 0 && info.Size() > maxSize {
		return nil, nil
	}

	// Skip files not modified since cutoff (file mtime check)
	if !cutoff.IsZero() && info.ModTime().Before(cutoff) {
		return nil, nil
	}

	parseCacheMu.Lock()
	cached, ok := parseCache[path]
	parseCacheMu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		c := *cached.conv // copy: the cached one is shared across goroutines
		c.readAt = readAt
		return &c, nil
	}
	conv, err := parseConversationUncached(path, info)
	if conv != nil {
		conv.readAt = readAt
	}
	if err == nil {
		parseCacheMu.Lock()
		parseCache[path] = parsedFile{info.Size(), info.ModTime(), conv}
		parseCacheMu.Unlock()
	}
	return conv, err
}

// parseCache lets auto-refresh reparse only files that changed since the last
// scan. ponytail: entries for files deleted outside ccs are never evicted;
// negligible for a session-long TUI.
type parsedFile struct {
	size    int64
	modTime time.Time
	conv    *Conversation
}

var (
	parseCacheMu sync.Mutex
	parseCache   = make(map[string]parsedFile)
)

func parseConversationUncached(path string, info os.FileInfo) (*Conversation, error) {
	conv := &Conversation{
		SessionID: strings.TrimSuffix(info.Name(), ".jsonl"),
		FilePath:  path,
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	complete, total, tail, err := consumeLines(conv, file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	conv.parsedBytes, conv.Size, conv.tailApplied = complete, total, tail
	if len(conv.Messages) == 0 {
		return nil, nil
	}
	finishParse(conv)
	conv.searchText = searchTextOf(*conv)
	conv.searchLower = strings.ToLower(conv.searchText)
	return conv, nil
}

// parseAppended brings prev up to date with its file, parsing only the lines
// appended since prev was parsed, so a live 100MB transcript costs only its
// new lines. A line caught mid-write wasn't applied, so parsing resumes at it.
// Falls back to a full parse when resuming isn't safe: the file shrank (e.g.
// a prune) or an unterminated last line was applied as a record. Returns prev
// itself when the file hasn't grown.
func parseAppended(prev *Conversation) (*Conversation, error) {
	readAt := time.Now()
	info, err := os.Stat(prev.FilePath)
	if err != nil {
		return nil, err
	}
	if info.Size() == prev.Size {
		return prev, nil
	}
	if info.Size() < prev.Size || prev.tailApplied || prev.parsedBytes > info.Size() {
		return parseConversationFile(prev.FilePath, time.Time{}, 0)
	}
	file, err := os.Open(prev.FilePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(prev.parsedBytes, io.SeekStart); err != nil {
		return nil, err
	}
	c := *prev
	c.Messages = slices.Clip(prev.Messages) // appends must not write into prev's array
	if c.Cwd == "unknown" {
		c.Cwd = ""
	}
	complete, total, tail, err := consumeLines(&c, file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", prev.FilePath, err)
	}
	c.parsedBytes = prev.parsedBytes + complete
	c.Size = prev.parsedBytes + total
	c.tailApplied = tail
	c.readAt = readAt
	finishParse(&c)
	// Rebuilt exactly as a full parse would, and only when something it
	// covers changed: most appends are tool calls with no searchable text.
	c.searchText, c.searchLower = appendedSearchText(prev, &c)
	if info, err := file.Stat(); err == nil {
		parseCacheMu.Lock()
		parseCache[c.FilePath] = parsedFile{c.Size, info.ModTime(), &c}
		parseCacheMu.Unlock()
	}
	return &c, nil
}

// appendedSearchText returns c's search text, identical to searchTextOf(c).
// When only messages were added it extends prev's instead: searchTextOf ends
// with the last timestamp, so swap that suffix for the new messages and the
// new timestamp, lowercasing just the new part. Anything else rebuilds.
func appendedSearchText(prev, c *Conversation) (string, string) {
	if len(c.Messages) == len(prev.Messages) && c.Title == prev.Title && c.Cwd == prev.Cwd && c.FirstTimestamp == prev.FirstTimestamp {
		return prev.searchText, prev.searchLower
	}
	oldTail := " " + formatTimestamp(prev.LastTimestamp)
	oldTailLower := strings.ToLower(oldTail)
	if len(c.Messages) > len(prev.Messages) && c.Title == prev.Title && c.Cwd == prev.Cwd && c.FirstTimestamp == prev.FirstTimestamp &&
		strings.HasSuffix(prev.searchText, oldTail) && strings.HasSuffix(prev.searchLower, oldTailLower) {
		parts := make([]string, 0, len(c.Messages)-len(prev.Messages)+1)
		for _, msg := range c.Messages[len(prev.Messages):] {
			parts = append(parts, msg.Text)
		}
		parts = append(parts, formatTimestamp(c.LastTimestamp))
		add := " " + strings.Join(parts, " ")
		return strings.TrimSuffix(prev.searchText, oldTail) + add,
			strings.TrimSuffix(prev.searchLower, oldTailLower) + strings.ToLower(add)
	}
	text := searchTextOf(*c)
	return text, strings.ToLower(text)
}

// consumeLines parses JSONL records from r into conv. complete counts bytes
// up to the last newline; total includes a trailing unterminated line. That
// line is still parsed (a finished file may lack the final newline); tail
// reports whether it held a whole record, as opposed to a write in progress.
func consumeLines(conv *Conversation, r io.Reader) (complete, total int64, tail bool, err error) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			total += int64(len(line))
			applied := parseLine(conv, line)
			if line[len(line)-1] == '\n' {
				complete = total
			} else {
				tail = applied
			}
		}
		if err == io.EOF {
			return complete, total, tail, nil
		}
		if err != nil {
			return complete, total, tail, err
		}
	}
}

// parseLine applies one JSONL record to conv; false if it isn't valid JSON.
func parseLine(conv *Conversation, line []byte) bool {
	var raw RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return false
	}
	switch raw.Type {
	case "custom-title":
		conv.Title = raw.CustomTitle // user-set name wins over ai-title
		conv.IsCustomTitle = raw.CustomTitle != ""
	case "ai-title":
		if conv.Title == "" {
			conv.Title = raw.AiTitle
		}
	case "user":
		if !conv.sawUser {
			// The first user line says how the session was started.
			conv.sawUser = true
			conv.Spawned = raw.Entrypoint == "sdk-cli" || raw.TeamName != ""
		}
		if conv.Cwd == "" {
			conv.Cwd = raw.Cwd
		}
		// isMeta lines are harness-injected (e.g. the local-command caveat).
		if text := extractText(raw.Message.Content); !raw.IsMeta && strings.TrimSpace(text) != "" {
			if conv.FirstTimestamp == "" {
				conv.FirstTimestamp = raw.Timestamp
			}
			conv.Messages = append(conv.Messages, Message{Role: "user", Text: text, Ts: raw.Timestamp})
		}
	case "assistant":
		// Each reply's usage counts the whole conversation it was sent, so
		// the last one is the current context. Zero-usage lines are
		// placeholders (e.g. API errors) and would read as an empty context.
		u := raw.Message.Usage
		if ctx := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens; ctx > 0 {
			conv.ContextTokens = ctx
		}
		if text := extractText(raw.Message.Content); strings.TrimSpace(text) != "" {
			conv.Messages = append(conv.Messages, Message{Role: "assistant", Text: text, Ts: raw.Timestamp})
		}
	}
	return true
}

func finishParse(conv *Conversation) {
	if len(conv.Messages) > 0 {
		conv.LastTimestamp = conv.Messages[len(conv.Messages)-1].Ts
	}
	if conv.Cwd == "" {
		conv.Cwd = "unknown"
	}
}

func getConversations(cutoff time.Time, maxSize int64, excludeDirs []string) ([]Conversation, error) {
	projectsDir := getProjectsDir()
	// Walk swallows a root error, so a vanished dir would read as "no
	// conversations" and a refresh would wipe the list.
	if _, err := os.Stat(projectsDir); err != nil {
		return nil, err
	}

	var files []string
	err := filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && info.Name() == "subagents" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			for _, exc := range excludeDirs {
				if strings.Contains(info.Name(), exc) {
					return filepath.SkipDir
				}
			}
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") && !strings.HasPrefix(info.Name(), "agent-") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Worker pool to limit concurrent file operations
	const numWorkers = 8
	jobs := make(chan string, len(files))
	results := make(chan *Conversation, len(files))

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				conv, err := parseConversationFile(path, cutoff, maxSize)
				if err == nil && conv != nil {
					results <- conv
				}
			}
		}()
	}

	for _, file := range files {
		jobs <- file
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	var conversations []Conversation
	for conv := range results {
		conversations = append(conversations, *conv)
	}

	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].LastTimestamp > conversations[j].LastTimestamp
	})

	return conversations, nil
}

func formatTimestamp(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if len(ts) >= 16 {
			return ts[:16]
		}
		return ts
	}
	return t.Local().Format("2006-01-02 15:04")
}

// formatAgo renders how long before now ts was, for the WHEN column:
// "now", "5m ago", "3h ago", "2d ago", "3w ago", "4mo ago", "1y ago". The
// full timestamp stays searchable and shows on each message in the preview.
func formatAgo(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	var n int
	var unit string
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		n, unit = int(d.Minutes()), "m"
	case d < 24*time.Hour:
		n, unit = int(d.Hours()), "h"
	case d < 14*24*time.Hour:
		n, unit = int(d.Hours()/24), "d"
	case d < 60*24*time.Hour:
		n, unit = int(d.Hours()/24/7), "w"
	case d < 365*24*time.Hour:
		n, unit = int(d.Hours()/24/30), "mo"
	default:
		n, unit = int(d.Hours()/24/365), "y"
	}
	return fmt.Sprintf("%d%s ago", n, unit)
}

// formatBytes renders a byte count compactly (fits the 6-wide SIZE column).
// formatTokens renders a token count for the 5-wide CTX column: 950, 12k,
// 281k, 1.2M. Blank when unknown (no reply with usage yet).
func formatTokens(n int) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dMB", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%dKB", n/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func truncate(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	if maxLen < 3 {
		return string(r[:maxLen]) // no room for the ellipsis
	}
	return string(r[:maxLen-3]) + "..."
}

// getTopic returns the session name (custom/ai title), else first user message, else session ID
func getTopic(conv Conversation) string {
	if conv.Title != "" {
		return conv.Title
	}
	for _, msg := range conv.Messages {
		if msg.Role != "user" {
			continue
		}
		// Harness text (command echoes and output, task notifications, teammate
		// messages) is tag-wrapped. ponytail: a real prompt starting with "<"
		// is treated as harness text too.
		if !strings.HasPrefix(msg.Text, "<") {
			return msg.Text
		}
		// Slash and ! commands: show the command itself.
		if cmd := tagText(msg.Text, "command-name"); cmd != "" {
			return cmd
		}
		if cmd := tagText(msg.Text, "bash-input"); cmd != "" {
			return "! " + cmd
		}
	}
	return conv.SessionID
}

// tagText returns the trimmed text inside the first <tag>...</tag> of s, or "".
func tagText(s, tag string) string {
	_, rest, ok := strings.Cut(s, "<"+tag+">")
	if !ok {
		return ""
	}
	inner, _, ok := strings.Cut(rest, "</"+tag+">")
	if !ok {
		return ""
	}
	return strings.TrimSpace(inner)
}

// renameConversation sets a custom title the same way /rename does: by
// appending a custom-title line to the transcript.
func (m *model) renameConversation(name string) {
	m.renaming = false
	if name == "" || m.renameIndex >= len(m.filtered) {
		return
	}
	conv := m.filtered[m.renameIndex].conv
	line, _ := json.Marshal(map[string]string{"type": "custom-title", "customTitle": name, "sessionId": conv.SessionID})
	if err := appendLine(conv.FilePath, line); err != nil {
		m.errorMsg = fmt.Sprintf("Rename failed: %v", err)
		return
	}
	m.gen++
	// The file grew, so the next live tick or scan reparses it; drop the
	// cached copy rather than let anything build on it.
	parseCacheMu.Lock()
	delete(parseCache, conv.FilePath)
	parseCacheMu.Unlock()
	c := conv
	c.Title, c.IsCustomTitle = name, true
	c.searchText = "" // rebuilt by buildItems, exactly as a full parse would
	renamed := buildItems([]Conversation{c})[0]
	for _, items := range [][]listItem{m.items, m.filtered} {
		for i := range items {
			if items[i].conv.SessionID == conv.SessionID {
				items[i] = renamed
			}
		}
	}
	m.errorMsg = ""
}

// appendLine appends one JSONL line, first adding a newline if the file lacks
// a trailing one so the new line can't fuse with the last record.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// deleteConversation removes the selected conversation from disk and UI
func (m *model) deleteConversation() {
	if m.deleteIndex >= len(m.filtered) {
		return
	}

	conv := m.filtered[m.deleteIndex].conv

	// Delete the file (ignore if already deleted)
	if err := os.Remove(conv.FilePath); err != nil && !os.IsNotExist(err) {
		m.errorMsg = fmt.Sprintf("Delete failed: %v", err)
		m.confirmDelete = false
		return
	}
	m.gen++
	parseCacheMu.Lock()
	delete(parseCache, conv.FilePath)
	parseCacheMu.Unlock()

	// Remove from filtered slice
	m.filtered = append(m.filtered[:m.deleteIndex], m.filtered[m.deleteIndex+1:]...)

	// Remove from items slice (find by SessionID)
	for i, item := range m.items {
		if item.conv.SessionID == conv.SessionID {
			m.items = append(m.items[:i], m.items[i+1:]...)
			break
		}
	}

	// Adjust cursor
	if len(m.filtered) == 0 {
		m.cursor = 0
	} else if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	// Otherwise cursor stays at same position (shows next item)

	// Exit confirmation mode
	m.confirmDelete = false
	m.errorMsg = ""
}

// pruneConversation prunes the selected conversation file in place and refreshes
// its displayed size. The conversation stays in the list (only shrunk).
// ponytail: synchronous - a multi-GB file briefly blocks the UI, same as delete.
func (m *model) pruneConversation() {
	m.confirmPrune = false
	if m.pruneIndex >= len(m.filtered) {
		return
	}
	conv := m.filtered[m.pruneIndex].conv

	_, err := pruneFile(conv.FilePath, true, pruneOpts{dropSnapshots: true, stripToolResults: true})
	if err != nil {
		m.errorMsg = fmt.Sprintf("Prune failed: %v", err)
		return
	}
	m.gen++

	// Re-read the pruned file so its parse state (read position, size, read
	// time) matches what's on disk; resuming from the old offset would skip
	// lines appended later.
	parseCacheMu.Lock()
	delete(parseCache, conv.FilePath)
	parseCacheMu.Unlock()
	fresh, err := parseConversationFile(conv.FilePath, time.Time{}, 0)
	if err != nil || fresh == nil {
		m.errorMsg = fmt.Sprintf("Pruned, but re-reading failed: %v", err)
		return
	}
	item := buildItems([]Conversation{*fresh})[0]
	for _, items := range [][]listItem{m.items, m.filtered} {
		for i := range items {
			if items[i].conv.SessionID == conv.SessionID {
				items[i] = item
			}
		}
	}
	m.errorMsg = ""
}

// buildItems creates list items from conversations
func buildItems(conversations []Conversation) []listItem {
	items := make([]listItem, 0, len(conversations))
	for _, conv := range conversations {
		if conv.searchText == "" { // not from parseConversationFile (e.g. tests)
			conv.searchText = searchTextOf(conv)
			conv.searchLower = strings.ToLower(conv.searchText)
		}
		items = append(items, listItem{conv: conv, searchText: conv.searchText, searchLower: conv.searchLower})
	}
	return items
}

// searchTextOf joins everything a conversation can be found by.
func searchTextOf(conv Conversation) string {
	parts := []string{conv.SessionID, conv.Title, conv.Cwd, formatTimestamp(conv.FirstTimestamp)}
	// Include assistant messages too so a conversation is findable by what
	// Claude said, matching the HITS column and preview.
	for _, msg := range conv.Messages {
		parts = append(parts, msg.Text)
	}
	// Last, so new messages can be appended without rebuilding (parseAppended).
	parts = append(parts, formatTimestamp(conv.LastTimestamp))
	return strings.Join(parts, " ")
}

// ============================================================================
// Prune - shrink conversation files by removing duplicate / redundant data
// ============================================================================

// pruneOpts selects which categories of redundant data to remove. Conversation
// (user/assistant) messages are never touched.
type pruneOpts struct {
	dropSnapshots    bool // drop file-history-snapshot lines (rewind/checkpoint backups)
	stripToolResults bool // remove the toolUseResult field (a copy of the tool_result already in message.content)
}

type pruneStats struct {
	bytesIn          int64
	bytesOut         int64
	droppedSnapshots int
	strippedResults  int
	convLinesIn      int // user/assistant lines seen
	convLinesOut     int // ... and kept (invariant: must equal convLinesIn)
}

// pruneLine applies the transforms to one JSONL line. It returns the output
// bytes (nil = drop the line), the line's "type", and whether it was
// dropped/stripped. Unparseable lines pass through verbatim.
func pruneLine(line []byte, opts pruneOpts) (out []byte, typ string, dropped, stripped bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		return line, "", false, false
	}
	if raw, ok := obj["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}
	if opts.dropSnapshots && typ == "file-history-snapshot" {
		return nil, typ, true, false
	}
	if opts.stripToolResults {
		if _, ok := obj["toolUseResult"]; ok {
			delete(obj, "toolUseResult")
			b, err := json.Marshal(obj)
			if err != nil {
				return line, typ, false, false // keep original on marshal error
			}
			return b, typ, false, true
		}
	}
	return line, typ, false, false
}

// pruneStream reads JSONL from r and writes the pruned version to w (w may be
// nil to only measure). It never drops or modifies user/assistant lines.
func pruneStream(r io.Reader, w io.Writer, opts pruneOpts) (pruneStats, error) {
	var st pruneStats
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		st.bytesIn += int64(len(line)) + 1
		out, typ, dropped, stripped := pruneLine(line, opts)
		isConv := typ == "user" || typ == "assistant"
		if isConv {
			st.convLinesIn++
		}
		if dropped {
			st.droppedSnapshots++
			continue
		}
		if stripped {
			st.strippedResults++
		}
		if isConv {
			st.convLinesOut++
		}
		if w != nil {
			if _, err := w.Write(out); err != nil {
				return st, err
			}
			if _, err := w.Write([]byte{'\n'}); err != nil {
				return st, err
			}
		}
		st.bytesOut += int64(len(out)) + 1
	}
	return st, scanner.Err()
}

// pruneFile prunes one conversation file. With write=true it streams to
// <path>.pruned and atomically replaces path, aborting (no replace) if the
// conversation line count would change. With write=false it only measures.
func pruneFile(path string, write bool, opts pruneOpts) (pruneStats, error) {
	in, err := os.Open(path)
	if err != nil {
		return pruneStats{}, err
	}
	defer in.Close()

	if !write {
		return pruneStream(in, nil, opts)
	}

	tmpPath := path + ".pruned"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return pruneStats{}, err
	}
	bw := bufio.NewWriter(tmp)
	st, err := pruneStream(in, bw, opts)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && st.convLinesIn != st.convLinesOut {
		err = fmt.Errorf("integrity check failed: %d conversation lines in, %d out", st.convLinesIn, st.convLinesOut)
	}
	if err != nil {
		os.Remove(tmpPath)
		return st, err
	}
	return st, os.Rename(tmpPath, path)
}

// findPrunableFiles returns .jsonl files at or above minSize, largest first.
func findPrunableFiles(minSize int64) ([]string, error) {
	type fileSize struct {
		path string
		size int64
	}
	var found []fileSize
	err := filepath.Walk(getProjectsDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") && info.Size() >= minSize {
			found = append(found, fileSize{path, info.Size()})
		}
		return nil
	})
	sort.Slice(found, func(i, j int) bool { return found[i].size > found[j].size })
	paths := make([]string, len(found))
	for i, f := range found {
		paths[i] = f.path
	}
	return paths, err
}

// shortPath shows the project dir + filename for readable reporting.
func shortPath(p string) string {
	return filepath.Join(filepath.Base(filepath.Dir(p)), filepath.Base(p))
}

func runPrune(args []string) {
	apply, yes := false, false // dry run by default; --apply to actually rewrite
	minSizeMB := int64(50)
	opts := pruneOpts{dropSnapshots: true, stripToolResults: true}
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			printPruneHelp()
			return
		case a == "--apply":
			apply = true
		case a == "--dry-run":
			apply = false // explicit; this is already the default
		case a == "-y" || a == "--yes":
			yes = true
		case a == "--no-snapshots":
			opts.dropSnapshots = false
		case a == "--no-tool-results":
			opts.stripToolResults = false
		case strings.HasPrefix(a, "--min-size="):
			fmt.Sscanf(strings.TrimPrefix(a, "--min-size="), "%d", &minSizeMB)
		default:
			fmt.Fprintf(os.Stderr, "unknown prune flag: %s (try ccs prune --help)\n", a)
			os.Exit(2)
		}
	}
	if !opts.dropSnapshots && !opts.stripToolResults {
		fmt.Fprintln(os.Stderr, "nothing to prune: both categories disabled")
		os.Exit(2)
	}

	files, err := findPrunableFiles(minSizeMB * 1024 * 1024)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error scanning conversations: %v\n", err)
		os.Exit(1)
	}
	if len(files) == 0 {
		fmt.Printf("No conversations >= %dMB to prune.\n", minSizeMB)
		return
	}

	report := func(st pruneStats, path string) {
		saved := st.bytesIn - st.bytesOut
		fmt.Printf("  %-46s %8s -> %8s  (-%s)\n", shortPath(path), formatBytes(st.bytesIn), formatBytes(st.bytesOut), formatBytes(saved))
	}

	if !apply {
		fmt.Printf("Dry run (no changes). Conversations >= %dMB:\n\n", minSizeMB)
		var in, out int64
		for _, f := range files {
			st, err := pruneFile(f, false, opts)
			if err != nil {
				fmt.Printf("  %-46s error: %v\n", shortPath(f), err)
				continue
			}
			report(st, f)
			in += st.bytesIn
			out += st.bytesOut
		}
		fmt.Printf("\nWould reclaim %s across %d files. Re-run with --apply to prune.\n", formatBytes(in-out), len(files))
		return
	}

	if !yes {
		var total int64
		for _, f := range files {
			if info, e := os.Stat(f); e == nil {
				total += info.Size()
			}
		}
		fmt.Printf("Prune %d conversations (%s)? Rewrites them in place, removing duplicate tool\nresults and snapshot backups - dialogue is preserved. [y/N] ", len(files), formatBytes(total))
		var resp string
		fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" {
			fmt.Println("Aborted.")
			return
		}
	}

	var in, out int64
	for _, f := range files {
		st, err := pruneFile(f, true, opts)
		if err != nil {
			fmt.Printf("  %-46s FAILED: %v\n", shortPath(f), err)
			continue
		}
		report(st, f)
		in += st.bytesIn
		out += st.bytesOut
	}
	fmt.Printf("\nReclaimed %s across %d files.\n", formatBytes(in-out), len(files))
}

func printPruneHelp() {
	fmt.Print(`ccs prune - shrink conversation files by removing redundant data

Removes data that duplicates content kept elsewhere, so pruned conversations
still resume with full dialogue:
  - toolUseResult fields (a copy of the tool_result already in message.content)
  - file-history-snapshot lines (rewind/checkpoint backups; pruning loses
    rewind history, not the conversation)

User and assistant messages are never modified. Each file is rewritten only if
its conversation line count is unchanged.

By default this is a dry run that only previews savings - pass --apply to
actually rewrite the files.

Usage: ccs prune [flags]

Flags:
  --apply              Actually rewrite files (default is a dry-run preview)
  --min-size=N         Only consider files >= N MB (default: 50)
  --no-tool-results    Keep toolUseResult fields
  --no-snapshots       Keep file-history-snapshot lines
  -y, --yes            Skip the confirmation prompt (with --apply)
  -h, --help           Show this help

Examples:
  ccs prune                        Preview savings across files >= 50MB
  ccs prune --apply                Prune files >= 50MB (after confirmation)
  ccs prune --apply --min-size=200 Prune files >= 200MB
  ccs prune --apply --no-tool-results -y   Only drop snapshot backups, no prompt
`)
}

func printHelp() {
	fmt.Printf(`ccs v%s - Claude Code Search

Search and resume Claude Code conversations.

Usage: ccs [filter] [-- claude-flags...]
       ccs prune [flags]    Shrink large conversations (see ccs prune --help)

Arguments:
  filter           Initial search query (optional)
  -- claude-flags  Flags to pass to 'claude --resume' (after --)

Flags:
  -h, --help       Show this help message
  -v, --version    Show version
  --max-age=N      Only search last N days (default: 60, 0 = no limit)
  --max-size=N     Max file size in MB (default: 1024, 0 = no limit)
  --all            Include everything (same as --max-age=0 --max-size=0)
  --exclude=a,b    Exclude dirs containing these strings (default: observer-sessions)
  --dump [query]   Debug: print all search items (with optional highlighting)

Examples:
  ccs                                Search last 60 days, files <1GB (default)
  ccs --max-age=7                    Search last 7 days only
  ccs --all                          Search everything (all time, all files)
  ccs buyer                          Search with initial query "buyer"
  ccs -- --plan                      Resume with plan mode
  ccs buyer -- --plan                Search "buyer", resume with plan mode

Key bindings:
  ↑/↓, Ctrl+P/N   Navigate list
  Enter           Select and resume conversation
  Ctrl+D          Delete conversation (with confirmation)
  Ctrl+F          Fork conversation (resume into a new session id)
  Ctrl+R          Rename conversation (not while it's open in claude)
  Ctrl+X          Prune conversation - shrink it losslessly (with confirmation)
  Ctrl+J/K        Scroll preview
  Ctrl+U          Clear search
  Esc, Ctrl+C     Quit

`, version)
}

func main() {
	args := os.Args[1:]

	if len(args) > 0 && args[0] == "prune" {
		runPrune(args[1:])
		return
	}

	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			printHelp()
			return
		}
		if arg == "-v" || arg == "--version" {
			fmt.Printf("ccs v%s\n", version)
			return
		}
	}

	// Parse flags
	maxAgeDays := 60         // Default to 60 days
	maxSizeMB := int64(1024) // Default to 1GB
	excludeDirs := []string{"observer-sessions"}
	for _, arg := range args {
		if arg == "--all" {
			maxAgeDays = 0
			maxSizeMB = 0
		} else if strings.HasPrefix(arg, "--max-age=") {
			val := strings.TrimPrefix(arg, "--max-age=")
			fmt.Sscanf(val, "%d", &maxAgeDays)
		} else if strings.HasPrefix(arg, "--max-size=") {
			val := strings.TrimPrefix(arg, "--max-size=")
			fmt.Sscanf(val, "%d", &maxSizeMB)
		} else if strings.HasPrefix(arg, "--exclude=") {
			val := strings.TrimPrefix(arg, "--exclude=")
			excludeDirs = strings.Split(val, ",")
		}
	}

	// Convert to bytes (0 means no limit)
	maxSize := maxSizeMB * 1024 * 1024

	// Calculate cutoff time (0 means no limit)
	var cutoff time.Time
	if maxAgeDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -maxAgeDays)
	}

	// Debug mode - dump search lines
	for i, arg := range args {
		if arg == "--dump" {
			filter := ""
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				filter = args[i+1]
			}
			conversations, _ := getConversations(cutoff, maxSize, excludeDirs)
			items := buildItems(conversations)
			for _, item := range items {
				line := item.searchText
				if filter != "" {
					line = highlight(line, filter)
				}
				fmt.Println(line)
			}
			return
		}
	}

	// Parse args: positional arg is filter, args after -- go to claude
	var claudeFlags []string
	var filterQuery string
	for i, arg := range args {
		if arg == "--" {
			claudeFlags = args[i+1:]
			break
		}
		// Skip our flags when looking for filter query
		if arg == "--all" || strings.HasPrefix(arg, "--max-age=") || strings.HasPrefix(arg, "--max-size=") || strings.HasPrefix(arg, "--exclude=") {
			continue
		}
		if !strings.HasPrefix(arg, "-") && filterQuery == "" {
			filterQuery = arg
		}
	}

	projectsDir := getProjectsDir()
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Projects directory not found: %s\n", projectsDir)
		fmt.Fprintf(os.Stderr, "Make sure Claude Code is installed and has been used at least once.\n")
		os.Exit(1)
	}

	fmt.Fprint(os.Stderr, "Loading conversations...")
	conversations, err := getConversations(cutoff, maxSize, excludeDirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\rError loading conversations: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprint(os.Stderr, "\r                         \r")

	if len(conversations) == 0 {
		fmt.Fprintf(os.Stderr, "No conversations found\n")
		os.Exit(1)
	}

	items := buildItems(conversations)
	if len(items) == 0 {
		fmt.Fprintf(os.Stderr, "No searchable messages found\n")
		os.Exit(1)
	}

	// Run TUI. Mouse reporting is intentionally NOT enabled: under a heavy
	// frame the terminal emits mouse-wheel reports faster than bubbletea reads
	// them, and the fragmented sequences leak into the search box as text.
	// Scrolling is keyboard-only (arrows / Ctrl+J/K / PgUp/PgDn).
	m := initialModel(items, filterQuery, claudeFlags)
	m.live = readLiveSessions()
	m.lastRefresh = time.Now()
	if version != "dev" {
		m.checkLatest = latestRelease
		if exe, err := os.Executable(); err == nil {
			m.upgrade = chooseUpgrader(exe)
		}
	}
	m.reload = func() ([]listItem, error) {
		convs, err := getConversations(cutoff, maxSize, excludeDirs)
		if err != nil {
			return nil, err
		}
		return buildItems(convs), nil
	}
	p := tea.NewProgram(m, tea.WithAltScreen())

	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	final := finalModel.(model)
	if final.restart != "" {
		err := syscall.Exec(final.restart, append([]string{"ccs"}, os.Args[1:]...), os.Environ())
		fmt.Fprintf(os.Stderr, "Updated; run ccs again (%v)\n", err)
		os.Exit(1)
	}
	if final.selected == nil {
		return
	}

	conv := final.selected
	cwd := conv.Cwd
	if cwd == "" || cwd == "unknown" {
		cwd = "."
	}

	// Change directory before announcing the resume, and fail loudly rather
	// than launching claude in the wrong directory (which silently gives it the
	// wrong project config / MCP servers).
	if err := os.Chdir(cwd); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot resume in %s: %v\n", cwd, err)
		os.Exit(1)
	}

	fmt.Printf("\033[1mResuming conversation %s in %s...\033[0m\n", conv.SessionID, cwd)
	if len(claudeFlags) > 0 {
		fmt.Printf("\033[90mFlags: %s\033[0m\n", strings.Join(claudeFlags, " "))
	}
	fmt.Println()

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude not found in PATH\n")
		os.Exit(1)
	}

	execArgs := []string{"claude", "--resume", conv.SessionID}
	execArgs = append(execArgs, claudeFlags...)
	if final.fork {
		execArgs = append(execArgs, "--fork-session")
	}

	syscall.Exec(claudePath, execArgs, os.Environ())
}

// openResumeTab tries to resume conv in a new tmux window or iTerm tab. Returns
// false when that isn't possible, leaving the caller to exec claude in place.
func openResumeTab(conv Conversation, claudeFlags []string) (bool, error) {
	cwd := conv.Cwd
	if cwd == "" || cwd == "unknown" {
		cwd = "."
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return false, nil
	}
	args := append([]string{claudePath, "--resume", conv.SessionID}, claudeFlags...)
	// tmux first: inside tmux the iTerm tab would land outside the session.
	if handled, err := resumeInTmuxWindow(cwd, args); handled {
		return true, err
	}
	return resumeInITermTab(cwd, args)
}

// resumeInTmuxWindow opens args in a new background tmux window. handled is
// false outside tmux. An error after trying means the window may or may not
// have opened, so the caller must not launch another copy.
func resumeInTmuxWindow(cwd string, args []string) (handled bool, err error) {
	if os.Getenv("TMUX") == "" {
		return false, nil
	}
	// -d leaves the current window (ccs) focused. tmux expands formats in -c,
	// including #(command), so a '#' in the path must be escaped.
	tmuxArgs := append([]string{"new-window", "-d", "-c", strings.ReplaceAll(cwd, "#", "##")}, args...)
	_, err = runBounded(5*time.Second, nil, "tmux", tmuxArgs...)
	return true, err
}

// resumeCmd opens conv off the UI goroutine. Liveness is checked afresh, so
// a session resumed elsewhere since the last live tick is focused, not
// resumed a second time. Forks always open a new session.
func (m *model) resumeCmd(conv Conversation, fork bool) tea.Cmd {
	if m.resuming {
		return nil // one at a time: a double Enter mustn't open two tabs
	}
	m.resuming = true
	flags := slices.Clone(m.claudeFlags)
	if fork {
		flags = append(flags, "--fork-session")
	}
	return func() tea.Msg {
		if !fork {
			if pid, ok := liveSessionPIDs()[conv.SessionID]; ok {
				return focusDoneMsg{pid: pid, found: focusSession(pid)}
			}
		}
		opened, err := openResumeTab(conv, flags)
		return resumeDoneMsg{conv: conv, fork: fork, opened: opened, err: err}
	}
}

// runBounded runs an external command with a deadline, so a hung ps, tmux
// server or unresponsive iTerm can't stall ccs indefinitely.
func runBounded(timeout time.Duration, env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Without this, a child that inherits the output pipe keeps Output()
	// waiting past the deadline.
	cmd.WaitDelay = time.Second
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.Output()
}

// focusSession brings the terminal running pid to the front: its tmux pane
// when ccs is in tmux, else its iTerm tab. Matched by the process's tty.
func focusSession(pid int) bool {
	out, err := runBounded(2*time.Second, nil, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	tty := strings.TrimSpace(string(out))
	if err != nil || tty == "" || tty == "??" {
		return false
	}
	tty = "/dev/" + tty
	if os.Getenv("TMUX") != "" {
		panes, err := runBounded(5*time.Second, nil, "tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}")
		if target := tmuxPaneForTTY(string(panes), tty); err == nil && target != "" {
			_, err := runBounded(5*time.Second, nil, "tmux", "switch-client", "-t", target, ";", "select-window", "-t", target, ";", "select-pane", "-t", target)
			return err == nil
		}
	}
	if os.Getenv("TERM_PROGRAM") != "iTerm.app" {
		return false
	}
	script := fmt.Sprintf(`tell application "iTerm2"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is %q then
					select w
					tell t to select
					tell s to select
					activate
					return "found"
				end if
			end repeat
		end repeat
	end repeat
end tell`, tty)
	out, err = runBounded(5*time.Second, nil, "osascript", "-e", script)
	return err == nil && strings.TrimSpace(string(out)) == "found"
}

// tmuxPaneForTTY picks the pane target whose tty matches from
// `tmux list-panes -a -F '#{pane_tty} <target>'` output.
func tmuxPaneForTTY(panes, tty string) string {
	for _, line := range strings.Split(panes, "\n") {
		if t, target, ok := strings.Cut(line, " "); ok && t == tty {
			return target
		}
	}
	return ""
}

// shellQuote wraps s for /bin/sh single-quoted use.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resumeInITermTab opens the resume command in a new iTerm tab. Returns false
// if we're not in iTerm or osascript failed, so the caller can exec in place.
// ponytail: osascript, not the iTerm python API - no deps, no daemon.
func resumeInITermTab(cwd string, args []string) (handled bool, err error) {
	if os.Getenv("TERM_PROGRAM") != "iTerm.app" {
		return false, nil
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	cmd := "cd " + shellQuote(cwd) + " && exec " + strings.Join(quoted, " ")
	// Focus stays on the ccs tab: creating a tab selects it, so select the old
	// one back. No "activate", so iTerm doesn't jump to the front either.
	script := fmt.Sprintf(`tell application "iTerm2"
	tell current window
		set oldTab to current tab
		create tab with default profile
		tell current session to write text %q
		select oldTab
	end tell
end tell`, cmd)
	_, err = runBounded(5*time.Second, nil, "osascript", "-e", script)
	return true, err
}

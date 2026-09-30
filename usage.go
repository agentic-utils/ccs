package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ============================================================================
// Usage screen (Tab): 12h token charts, summary and allowance, ported from
// claude-dashboard (https://github.com/agentic-utils/claude-dashboard)
// ============================================================================

const (
	usageWindow  = 12 * time.Hour
	usageBucket  = 5 * time.Minute
	usageBuckets = int(usageWindow / usageBucket) // 144
	usageMargin  = 8                              // left gutter for the y-axis scale
)

// usageRec is one reply's usage, as claude-dashboard's collect() reads it.
type usageRec struct {
	ts                            time.Time
	id                            string // message.id or requestId, for de-duplication
	inp, c5, c1, read, fresh, out int64
}

// usageBucketTotals is one 5-minute bucket. Chart 1 stacks uncached/c5m/c1h
// (how fresh input was cached), chart 2 read/new/miss (how each prompt was
// assembly: miss = a turn that read nothing from cache), chart 3 output.
type usageBucketTotals struct {
	Uncached, C5m, C1h, Read, New, Miss, Output, Responses int64
}

func (b *usageBucketTotals) add(r usageRec) {
	b.Uncached += r.inp
	b.C5m += r.c5
	b.C1h += r.c1
	b.Read += r.read
	if r.read > 0 {
		b.New += r.fresh
	} else {
		b.Miss += r.fresh
	}
	b.Output += r.out
	b.Responses++
}

type usageData struct {
	now           time.Time
	buckets       []usageBucketTotals // oldest first, usageBuckets long
	eff1h, eff12h float64
	err           error
}

type usageMsg struct{ data usageData }

// usageFiles caches each transcript's usage records by size+mtime, so a
// refresh only re-reads files that changed.
var (
	usageFilesMu sync.Mutex
	usageFiles   = make(map[string]usageFile)
)

type usageFile struct {
	size    int64
	modTime time.Time
	recs    []usageRec
}

// usageLine is the subset of a transcript line the usage screen needs.
type usageLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreation            struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// readUsageRecs reads every usage-bearing reply in one transcript.
func readUsageRecs(path string) ([]usageRec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []usageRec
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		// Cheap pre-filter: only assistant lines carry usage.
		if len(line) > 0 && bytes.Contains(line, []byte(`"usage"`)) {
			var l usageLine
			if json.Unmarshal(line, &l) == nil && l.Message.Usage != nil {
				if ts, perr := time.Parse(time.RFC3339, l.Timestamp); perr == nil {
					u := l.Message.Usage
					id := l.Message.ID
					if id == "" {
						id = l.RequestID
					}
					recs = append(recs, usageRec{
						ts: ts, id: id, inp: u.InputTokens,
						c5: u.CacheCreation.Ephemeral5m, c1: u.CacheCreation.Ephemeral1h,
						read: u.CacheReadInputTokens, fresh: u.InputTokens + u.CacheCreationInputTokens,
						out: u.OutputTokens,
					})
				}
			}
		}
		if err == io.EOF {
			return recs, nil
		}
		if err != nil {
			return recs, err
		}
	}
}

// collectUsage buckets the last 12h of usage across every transcript,
// subagent transcripts included, de-duplicating replies by id (one reply is
// written as several lines). Mirrors claude-dashboard's collect().
func collectUsage(now time.Time, excludeDirs []string) usageData {
	cutoff := now.Add(-usageWindow)
	lastHour := now.Add(-time.Hour)
	d := usageData{now: now, buckets: make([]usageBucketTotals, usageBuckets)}
	seen := make(map[string]bool)
	live := make(map[string]bool)
	err := filepath.WalkDir(getProjectsDir(), func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() {
			for _, exc := range excludeDirs {
				if strings.Contains(e.Name(), exc) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		live[path] = true
		usageFilesMu.Lock()
		cached, ok := usageFiles[path]
		usageFilesMu.Unlock()
		if !ok || cached.size != info.Size() || !cached.modTime.Equal(info.ModTime()) {
			recs, rerr := readUsageRecs(path)
			if rerr != nil {
				return nil
			}
			cached = usageFile{info.Size(), info.ModTime(), recs}
			usageFilesMu.Lock()
			usageFiles[path] = cached
			usageFilesMu.Unlock()
		}
		for _, r := range cached.recs {
			if r.ts.Before(cutoff) || r.ts.After(now) {
				continue
			}
			if r.id != "" {
				if seen[r.id] {
					continue
				}
				seen[r.id] = true
			}
			idx := min(max(int(r.ts.Sub(cutoff)/usageBucket), 0), usageBuckets-1)
			d.buckets[idx].add(r)
			eff := tokenUsage{Input: r.inp, Cache5m: r.c5, Cache1h: r.c1, CacheRead: r.read, Output: r.out}.effective()
			d.eff12h += eff
			if !r.ts.Before(lastHour) {
				d.eff1h += eff
			}
		}
		return nil
	})
	d.err = err
	// Drop cache entries for files that aged out of the window or vanished.
	usageFilesMu.Lock()
	for path := range usageFiles {
		if !live[path] {
			delete(usageFiles, path)
		}
	}
	usageFilesMu.Unlock()
	return d
}

func (m *model) usageCmd() tea.Cmd {
	if m.usageLoading {
		return nil
	}
	m.usageLoading = true
	exclude := m.usageExclude
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				recoverWorkerValue(r)
				msg = usageMsg{usageData{now: time.Now(), err: fmt.Errorf("internal error, logged to %s", workerPanicLog)}}
			}
		}()
		return usageMsg{collectUsage(time.Now(), exclude)}
	}
}

// ---- allowance (GET /api/oauth/usage, the same numbers `/usage` shows) ----

var (
	allowanceURL = "https://api.anthropic.com/api/oauth/usage"
	// keychainRead returns Claude Code's stored OAuth credential JSON, nil if
	// there is none. A var so tests can inject it. Read-only: ccs never
	// refreshes or writes the credential.
	keychainRead = readClaudeCredential
)

const allowanceRefresh = time.Minute

// errAllowanceLogin means there's no usable token; Claude Code refreshes it.
var errAllowanceLogin = errors.New("open claude to refresh the allowance")

type allowanceLimit struct {
	Kind     string  `json:"kind"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
}

type allowanceMsg struct {
	limits  []allowanceLimit
	err     error
	account string
}

// accountEmail is the logged-in Claude account, as Claude Code records it in
// its config (.claude.json in the home folder, or in CLAUDE_CONFIG_DIR).
var accountEmail = func() string {
	path := filepath.Join(claudeConfigHome(), ".claude.json")
	if os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".claude.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		OauthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return cfg.OauthAccount.EmailAddress
}

// claudeConfigHome is where Claude Code keeps its profile (CLAUDE_CONFIG_DIR
// moves it, e.g. cswap).
func claudeConfigHome() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// keychainService names this profile's Keychain item the way Claude Code
// does: a hash of the raw CLAUDE_CONFIG_DIR (or CLAUDE_SECURESTORAGE_CONFIG_DIR
// when set, empty meaning the default). ponytail: Claude Code NFC-normalises
// the path first; that needs golang.org/x/text, so a non-NFC path (rare: only
// decomposed accents) would read the wrong item and show the login message.
func keychainService() string {
	const base = "Claude Code-credentials"
	cfg, ok := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	if !ok {
		cfg = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if cfg == "" {
		return base
	}
	sum := sha256.Sum256([]byte(cfg))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// keychainAccount mirrors Claude Code's username lookup: $USER, else the OS user.
func keychainAccount() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "user"
}

// readClaudeCredential reads the credential from the login Keychain on macOS,
// falling back to the profile's .credentials.json (where Claude Code writes
// when the Keychain is unusable, and on other platforms).
func readClaudeCredential() ([]byte, error) {
	if runtime.GOOS == "darwin" {
		out, err := runBounded(5*time.Second, nil, "/usr/bin/security", "find-generic-password",
			"-s", keychainService(), "-a", keychainAccount(), "-w")
		if err == nil {
			return out, nil
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 44 { // 44: item not found
			return nil, fmt.Errorf("keychain: %w", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(claudeConfigHome(), ".credentials.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// fetchAllowance reads the stored token and asks Anthropic for the live
// 5-hour and weekly utilisation. An expired or missing token is reported as
// errAllowanceLogin rather than refreshed: refreshing would mean writing
// Claude Code's credential, which ccs never does.
func fetchAllowance(now time.Time) ([]allowanceLimit, error) {
	raw, err := keychainRead()
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errAllowanceLogin
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"` // epoch ms
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &creds); err != nil {
		return nil, fmt.Errorf("reading the stored credential: %w", err)
	}
	oa := creds.ClaudeAiOauth
	if oa.AccessToken == "" || (oa.ExpiresAt > 0 && now.UnixMilli() >= oa.ExpiresAt) {
		return nil, errAllowanceLogin
	}
	req, err := http.NewRequest(http.MethodGet, allowanceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+oa.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "claude-cli/cache-monitor")
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errAllowanceLogin
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("allowance: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Limits []allowanceLimit `json:"limits"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("allowance: %w", err)
	}
	return body.Limits, nil
}

// allowanceCmd fetches the allowance at most once a minute.
func (m *model) allowanceCmd() tea.Cmd {
	if m.allowanceLoading || time.Since(m.allowanceAt) < allowanceRefresh {
		return nil
	}
	m.allowanceLoading = true
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				recoverWorkerValue(r)
				msg = allowanceMsg{err: fmt.Errorf("internal error, logged to %s", workerPanicLog)}
			}
		}()
		limits, err := fetchAllowance(time.Now())
		return allowanceMsg{limits, err, accountEmail()}
	}
}

// ---- rendering ----

// Truecolour palette, as claude-dashboard's.
var usageColours = map[string]string{
	"uncached": "84;160;255", "c5m": "170;120;255", "c1h": "214;150;255",
	"read": "52;224;150", "new": "84;160;255", "miss": "255;88;96", "output": "255;205;82",
}

func rgbText(rgb, s string) string { return "\033[38;2;" + rgb + "m" + s + "\033[0m" }

type usageSeries struct{ key, label string }

var usageCharts = []struct {
	title  string
	series []usageSeries
}{
	{"Input · cache write disposition", []usageSeries{{"uncached", "uncached"}, {"c5m", "5m · subagent"}, {"c1h", "1h · main"}}},
	{"Context assembly", []usageSeries{{"read", "from cache"}, {"new", "new input"}, {"miss", "cache miss"}}},
	{"Output", []usageSeries{{"output", "output tokens"}}},
}

func (b usageBucketTotals) value(key string) int64 {
	switch key {
	case "uncached":
		return b.Uncached
	case "c5m":
		return b.C5m
	case "c1h":
		return b.C1h
	case "read":
		return b.Read
	case "new":
		return b.New
	case "miss":
		return b.Miss
	case "output":
		return b.Output
	}
	return 0
}

// resampleBuckets maps the 5-minute buckets onto exactly `cols` columns, so
// the charts span the screen: on a narrow screen a column sums the buckets
// it covers; on a wide one a bucket spans several columns.
func resampleBuckets(in []usageBucketTotals, cols int) []usageBucketTotals {
	out := make([]usageBucketTotals, cols)
	n := len(in)
	for j := range out {
		lo, hi := j*n/cols, (j+1)*n/cols
		if hi <= lo { // wider than the data: repeat the bucket under this column
			out[j] = in[min(lo, n-1)]
			continue
		}
		for _, x := range in[lo:hi] {
			out[j].Uncached += x.Uncached
			out[j].C5m += x.C5m
			out[j].C1h += x.C1h
			out[j].Read += x.Read
			out[j].New += x.New
			out[j].Miss += x.Miss
			out[j].Output += x.Output
			out[j].Responses += x.Responses
		}
	}
	return out
}

var blockChars = []string{" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

// renderUsageChart draws one stacked bar chart: a title with legend, `height`
// rows of bars with a token scale on every other row, a baseline and hourly
// labels. Bars are scaled to the tallest column in eighths of a cell; each
// cell takes the colour of the series it mostly covers.
func renderUsageChart(title string, series []usageSeries, buckets []usageBucketTotals, height int, now time.Time) []string {
	totals := make([]int64, len(buckets))
	var maxT int64
	for i, b := range buckets {
		for _, s := range series {
			totals[i] += b.value(s.key)
		}
		maxT = max(maxT, totals[i])
	}
	legend := make([]string, len(series))
	for i, s := range series {
		legend[i] = rgbText(usageColours[s.key], "■") + " \033[90m" + s.label + "\033[0m"
	}
	lines := []string{"  \033[1;36m▸\033[0m \033[1m" + title + "\033[0m   " + strings.Join(legend, "  ")}
	units := height * 8
	for row := height - 1; row >= 0; row-- {
		label := strings.Repeat(" ", usageMargin)
		if row%2 == 1 && maxT > 0 {
			label = "\033[90m" + fmt.Sprintf("%*s", usageMargin-2, formatTokens(int(maxT*int64(row+1)/int64(height)))) + "\033[0m  "
		}
		var cells strings.Builder
		for i, b := range buckets {
			if totals[i] <= 0 || maxT <= 0 {
				cells.WriteString(" ")
				continue
			}
			filled := min(max(int(float64(totals[i])/float64(maxT)*float64(units)+0.5), 1), units)
			n := min(max(filled-row*8, 0), 8)
			if n == 0 {
				cells.WriteString(" ")
				continue
			}
			// Colour: the series covering the middle of this cell's filled part.
			mid := float64(row*8) + float64(n)/2
			cum, colour := 0.0, usageColours[series[len(series)-1].key]
			for _, s := range series {
				cum += float64(b.value(s.key)) / float64(totals[i]) * float64(filled)
				if mid <= cum {
					colour = usageColours[s.key]
					break
				}
			}
			cells.WriteString(rgbText(colour, blockChars[n]))
		}
		lines = append(lines, label+cells.String())
	}
	// Baseline and hourly tick labels.
	nb := len(buckets)
	axis := []rune(strings.Repeat(" ", nb))
	cut := now.Add(-usageWindow).Local()
	span := usageWindow / time.Duration(max(nb, 1)) // time covered by one column
	tick := cut.Truncate(time.Hour)
	if tick.Before(cut) {
		tick = tick.Add(time.Hour)
	}
	step := time.Hour
	if nb < 60 { // too narrow for a label every hour
		step = 3 * time.Hour
	}
	for ; !tick.After(now); tick = tick.Add(step) {
		lab := fmt.Sprintf("%d:00", tick.Hour())
		pos := int(tick.Sub(cut) / span)
		start := min(pos, nb-len(lab))
		for j, ch := range lab {
			if k := start + j; k >= 0 && k < nb {
				axis[k] = ch
			}
		}
	}
	lines = append(lines,
		"\033[90m"+fmt.Sprintf("%*s", usageMargin-1, "0")+" └"+strings.Repeat("─", max(nb-1, 0))+"\033[0m",
		strings.Repeat(" ", usageMargin)+"\033[90m"+string(axis)+"\033[0m")
	return lines
}

// usageSummary is the SUMMARY panel: 12h totals, effective tokens and the
// cache mix (shares of all input).
// usageSummary returns the SUMMARY and CACHE MIX panels, each `width` wide.
func usageSummary(d usageData, width int) (summary, cache []string) {
	var agg usageBucketTotals
	for _, b := range d.buckets {
		agg.Uncached += b.Uncached
		agg.C5m += b.C5m
		agg.C1h += b.C1h
		agg.Read += b.Read
		agg.New += b.New
		agg.Miss += b.Miss
		agg.Output += b.Output
		agg.Responses += b.Responses
	}
	totalIn := agg.Read + agg.New + agg.Miss
	pct := func(part int64) string {
		if totalIn == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.1f%%", 100*float64(part)/float64(totalIn))
	}
	// row puts label on the left and value flush right within the column.
	row := func(label, value string) string {
		return label + strings.Repeat(" ", max(width-lipgloss.Width(label)-lipgloss.Width(value), 1)) + value
	}
	chip := func(key, label, v string) string {
		return row(rgbText(usageColours[key], "■")+" "+label, v)
	}
	summary = []string{
		"\033[1;36mSUMMARY\033[0m \033[90m(last 12h)\033[0m",
		row("\033[90minput\033[0m", formatCount(totalIn)),
		row("\033[90moutput\033[0m", formatCount(agg.Output)),
		row("\033[90mresponses\033[0m", formatCount(agg.Responses)),
		row("\033[90meffective 1h / 12h\033[0m", formatTokens(int(d.eff1h))+" / "+formatTokens(int(d.eff12h))),
	}
	cache = []string{
		"\033[1;36mCACHE MIX\033[0m \033[90m(share of input)\033[0m",
		chip("c5m", "5m cache · subagent", pct(agg.C5m)),
		chip("c1h", "1h cache · main", pct(agg.C1h)),
		chip("read", "read from cache", pct(agg.Read)),
		chip("miss", "cache miss", pct(agg.Miss)),
	}
	return summary, cache
}

// formatCount renders n with thousands separators.
func formatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// gaugeColour grades an allowance gauge like claude-dashboard: green <= 70,
// yellow <= 80, amber <= 90, red <= 95, flashing red above.
func gaugeColour(pct float64) (string, bool) {
	switch {
	case pct > 95:
		return "1;31", true
	case pct > 90:
		return "31", false
	case pct > 80:
		return "38;5;208", false
	case pct > 70:
		return "33", false
	}
	return "32", false
}

// resetLabel is a reset time as "ends 14:10" today or "ends Mon 09:00".
func resetLabel(iso string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	lt := t.Local()
	if lt.Format("2006-01-02") == now.Local().Format("2006-01-02") {
		return "ends " + lt.Format("15:04")
	}
	return "ends " + lt.Format("Mon 15:04")
}

// ---- account switcher (Ctrl+O), backed by the cswap CLI ----

// cswapAccount is one entry of `cswap list --json`.
type cswapAccount struct {
	Number           int         `json:"number"`
	Email            string      `json:"email"`
	OrganizationName string      `json:"organizationName"`
	Active           bool        `json:"active"`
	UsageStatus      string      `json:"usageStatus"`
	Usage            *cswapUsage `json:"usage"`
	LastGoodUsage    *cswapUsage `json:"lastGoodUsage"`
}

type cswapUsage struct {
	FiveHour *struct {
		Pct *float64 `json:"pct"`
	} `json:"fiveHour"`
	SevenDay *struct {
		Pct *float64 `json:"pct"`
	} `json:"sevenDay"`
}

type acctListMsg struct {
	accts []cswapAccount
	err   error
}

type acctActionMsg struct {
	what, done string
	err        error
}

// cswapPath finds the cswap binary; "" means the switcher isn't available.
// A var so tests can point it at a fake.
var cswapPath = func() string {
	p, _ := exec.LookPath("cswap")
	return p
}

// cswapUsageLabel is "5h 20% · 7d 56%" for an account, as claude-dashboard
// shows it: live usage, else the last good reading, "re-login" if cswap needs
// a fresh login to read it.
func cswapUsageLabel(a cswapAccount) string {
	if a.UsageStatus == "relogin_required" {
		return "re-login"
	}
	u := a.Usage
	if u == nil {
		u = a.LastGoodUsage
	}
	if u == nil {
		return ""
	}
	var parts []string
	if u.FiveHour != nil && u.FiveHour.Pct != nil {
		parts = append(parts, fmt.Sprintf("5h %.0f%%", *u.FiveHour.Pct))
	}
	if u.SevenDay != nil && u.SevenDay.Pct != nil {
		parts = append(parts, fmt.Sprintf("7d %.0f%%", *u.SevenDay.Pct))
	}
	return strings.Join(parts, " · ")
}

func (m model) openAccounts() (tea.Model, tea.Cmd) {
	if cswapPath() == "" {
		m.errorMsg = "install cswap to switch accounts"
		return m, nil
	}
	m.acctOpen, m.acctMsg, m.acctCursor = true, "", 0
	return m, m.acctListCmd()
}

// acctListCmd reads `cswap list --json` off the UI goroutine.
func (m *model) acctListCmd() tea.Cmd {
	bin := cswapPath()
	m.acctBusy = true
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				recoverWorkerValue(r)
				msg = acctListMsg{err: fmt.Errorf("internal error, logged to %s", workerPanicLog)}
			}
		}()
		out, err := runBounded(15*time.Second, nil, bin, "list", "--json")
		if err != nil {
			return acctListMsg{err: err}
		}
		var list struct {
			ActiveAccountNumber int            `json:"activeAccountNumber"`
			Accounts            []cswapAccount `json:"accounts"`
		}
		if err := json.Unmarshal(out, &list); err != nil {
			return acctListMsg{err: fmt.Errorf("unexpected output: %w", err)}
		}
		for i := range list.Accounts { // either marker means active
			if list.Accounts[i].Number == list.ActiveAccountNumber {
				list.Accounts[i].Active = true
			}
		}
		return acctListMsg{accts: list.Accounts}
	}
}

// cswapActionCmd runs a cswap command that changes the live login (switch,
// add), detached so it can't prompt behind the TUI.
func (m *model) cswapActionCmd(what, done string, args ...string) tea.Cmd {
	bin := cswapPath()
	m.acctBusy, m.acctMsg = true, what+"…"
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				recoverWorkerValue(r)
				msg = acctActionMsg{what: what, err: fmt.Errorf("internal error, logged to %s", workerPanicLog)}
			}
		}()
		out, err := runCommand(60*time.Second, nil, true, bin, args...)
		if err != nil && !errors.Is(err, errTimedOut) {
			if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); lines[len(lines)-1] != "" {
				err = errors.New(ansiCodes.ReplaceAllString(lines[len(lines)-1], ""))
			}
		}
		return acctActionMsg{what: what, done: done, err: err}
	}
}

// acctKey handles keys while the switcher is open; it owns the keyboard.
func (m model) acctKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch {
	case key == "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case key == "esc" || key == "ctrl+o":
		m.acctOpen = false
		return m, nil
	case m.acctBusy: // one cswap command at a time
		return m, nil
	case key == "up" || key == "ctrl+p":
		m.acctCursor = max(m.acctCursor-1, 0)
	case key == "down" || key == "ctrl+n":
		m.acctCursor = min(m.acctCursor+1, max(len(m.accts)-1, 0))
	case key == "enter":
		if m.acctCursor < len(m.accts) {
			return m, m.switchTo(m.accts[m.acctCursor])
		}
	case key == "+":
		return m, m.cswapActionCmd("adding the current login", "Added the current login", "add")
	case len(key) == 1 && key[0] >= '1' && key[0] <= '9':
		n := int(key[0] - '0')
		for i, a := range m.accts {
			if a.Number == n {
				m.acctCursor = i
				return m, m.switchTo(a)
			}
		}
		m.acctMsg = fmt.Sprintf("✗ no account %d", n)
	}
	return m, nil
}

func (m *model) switchTo(a cswapAccount) tea.Cmd {
	if a.Active {
		m.acctMsg = a.Email + " is already active"
		return nil
	}
	return m.cswapActionCmd("switching to "+a.Email, "Switched to "+a.Email, "switch", strconv.Itoa(a.Number))
}

// acctPopup renders the switcher; View overlays it on any screen.
func (m model) acctPopup() string {
	var b strings.Builder
	b.WriteString("\033[1mSwitch Claude account\033[0m\n\n")
	emailW, orgW := 0, 0
	for _, a := range m.accts {
		emailW = max(emailW, lipgloss.Width(a.Email))
		orgW = max(orgW, lipgloss.Width(truncate(a.OrganizationName, 30)))
	}
	for i, a := range m.accts {
		mark := "  "
		if a.Active {
			mark = "\033[32m●\033[0m "
		}
		row := fmt.Sprintf("%s%d  %s  \033[90m%s\033[0m  %s", mark, a.Number,
			padRight(a.Email, emailW), padRight(truncate(a.OrganizationName, 30), orgW), cswapUsageLabel(a))
		if i == m.acctCursor {
			row = "\033[7m" + ansiCodes.ReplaceAllString(row, "") + "\033[0m"
		}
		b.WriteString(row + "\n")
	}
	switch {
	case len(m.accts) == 0 && m.acctBusy:
		b.WriteString("\033[90mreading accounts…\033[0m\n")
	case len(m.accts) == 0:
		b.WriteString("\033[90mno accounts yet: + adds the current login\033[0m\n")
	}
	if m.acctMsg != "" {
		b.WriteString("\n" + m.acctMsg + "\n")
	}
	b.WriteString("\n\033[90m" + hints("1-9 / ↑↓ "+keyEnter, "switch", "+", "add current login", keyEsc, "close") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(b.String())
}

// searchRowParts splits the search row into the search box plus padding, the
// account/allowance summary, and the match count, so a click can tell where
// the account email is.
func (m model) searchRowParts() (prefix, usage, count string) {
	count = fmt.Sprintf("(%d/%d)", len(m.filtered), len(m.items))
	usage = m.allowanceSummary()
	if usage != "" {
		usage += "   "
	}
	pad := m.width - 2 - 2 - 40 - lipgloss.Width(usage) - len(count) - 1 // 2 for indent, 2 for "> ", 40 for textInput, -1 to shift left
	search := m.textInput
	if m.chatFocus {
		search.Blur() // typing goes to the message box; one cursor on screen
	}
	return "  " + search.View() + strings.Repeat(" ", max(pad, 1)), usage, count
}

// accountSpan is the screen columns [start, end) of the account email in the
// search row, or ok=false when it isn't shown.
func (m model) accountSpan() (start, end int, ok bool) {
	if m.account == "" || m.showUsage || m.prompting() {
		return 0, 0, false
	}
	prefix, _, _ := m.searchRowParts()
	start = lipgloss.Width(prefix)
	return start, start + lipgloss.Width(m.account), true
}

// allowanceSummary is the one-line account and allowance shown in the search
// row: "you@example.com · 5h 14% ends 18:15 · week 22%". Tab shows the rest.
func (m model) allowanceSummary() string {
	var parts []string
	if m.account != "" {
		parts = append(parts, "\033[90m"+m.account+"\033[0m")
	}
	byKind := make(map[string]allowanceLimit)
	for _, l := range m.allowance {
		byKind[l.Kind] = l
	}
	now := time.Now()
	for _, k := range []struct{ kind, label string }{{"session", "5h"}, {"weekly_all", "week"}} {
		l, ok := byKind[k.kind]
		if !ok {
			continue
		}
		p := min(max(l.Percent, 0), 100)
		code, flash := gaugeColour(p)
		if flash && now.Unix()%2 == 1 {
			code = "2;31"
		}
		part := fmt.Sprintf("\033[90m%s\033[0m \033[%sm%.0f%%\033[0m", k.label, code, p)
		if k.kind == "session" {
			if r := resetLabel(l.ResetsAt, now); r != "" {
				part += " \033[90m" + r + "\033[0m"
			}
		}
		parts = append(parts, part)
	}
	if len(m.allowance) == 0 && errors.Is(m.allowanceErr, errAllowanceLogin) {
		parts = append(parts, "\033[33mopen claude to see usage\033[0m")
	}
	return strings.Join(parts, " \033[90m·\033[0m ")
}

// allowancePanel is the ALLOWANCE panel: 5-hour session and weekly gauges.
func (m model) allowancePanel(width int) []string {
	lines := []string{"\033[1;36mALLOWANCE\033[0m \033[90m(live /usage)\033[0m"}
	switch {
	case m.allowanceErr != nil && len(m.allowance) == 0:
		colour := "31"
		if errors.Is(m.allowanceErr, errAllowanceLogin) {
			colour = "33"
		}
		return append(lines, "\033["+colour+"m"+truncate(m.allowanceErr.Error(), width)+"\033[0m")
	case len(m.allowance) == 0:
		return append(lines, "\033[90mloading…\033[0m")
	}
	byKind := make(map[string]allowanceLimit)
	for _, l := range m.allowance {
		byKind[l.Kind] = l
	}
	barW := max(width-7, 10)
	now := time.Now()
	for _, k := range []struct{ kind, label string }{{"session", "5-hour session"}, {"weekly_all", "weekly"}} {
		l, ok := byKind[k.kind]
		if !ok {
			continue
		}
		p := min(max(l.Percent, 0), 100)
		code, flash := gaugeColour(p)
		if flash && now.Unix()%2 == 1 {
			code = "2;31"
		}
		fill := int(p/100*float64(barW) + 0.5)
		lines = append(lines,
			k.label,
			"\033["+code+"m"+strings.Repeat("█", fill)+"\033[0m\033[90m"+strings.Repeat("░", barW-fill)+"\033[0m \033[1;"+strings.TrimPrefix(code, "1;")+"m"+fmt.Sprintf("%3.0f%%", p)+"\033[0m",
			"\033[90m"+resetLabel(l.ResetsAt, now)+"\033[0m")
	}
	if m.allowanceErr != nil { // showing last-good gauges, but the latest fetch failed
		lines = append(lines, "\033[31m⚠ "+truncate(m.allowanceErr.Error(), width-2)+"\033[0m")
	}
	return lines
}

// usageView renders the usage screen in `height` rows below the header.
func (m model) usageView(height int) string {
	if m.usage.now.IsZero() {
		return "  \033[90mReading the last 12h of usage…\033[0m"
	}
	d := m.usage
	chartCols := max(m.width-usageMargin-1, 20)
	buckets := resampleBuckets(d.buckets, chartCols)
	// Three charts share what's left after the panels (10 rows); each chart
	// has 3 rows of title, baseline and labels around its bars.
	barH := max((height-11)/3-3, 2)
	var out []string
	for _, c := range usageCharts {
		out = append(out, renderUsageChart(c.title, c.series, buckets, barH, d.now)...)
	}
	out = append(out, "")
	// Three equal columns across the screen: summary, cache mix, allowance.
	const gap = 4
	colW := max((m.width-2-2*gap)/3, 24)
	summary, cache := usageSummary(d, colW)
	panels := [][]string{summary, cache, m.allowancePanel(colW)}
	rows := 0
	for _, p := range panels {
		rows = max(rows, len(p))
	}
	for i := 0; i < rows; i++ {
		var line strings.Builder
		line.WriteString("  ")
		for j, p := range panels {
			cell := ""
			if i < len(p) {
				cell = p[i]
			}
			if j < len(panels)-1 { // pad all but the last column
				cell += strings.Repeat(" ", max(colW-lipgloss.Width(cell), 0)+gap)
			}
			line.WriteString(cell)
		}
		out = append(out, line.String())
	}
	if d.err != nil {
		out = append(out, "  \033[31mreading transcripts: "+d.err.Error()+"\033[0m")
	}
	return strings.Join(out, "\n")
}

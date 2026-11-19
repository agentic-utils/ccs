// Package stats counts which ccs features get used, on this machine only, so
// the ones nobody reaches can be made easier to find or dropped.
//
// An event is the feature's id from the registry below, the time, the ccs
// version, its context (tui, cli, mcp, watch) and, in the TUI, whether a key
// or the mouse reached it. Nothing else can be written: Record drops any id
// not in the registry and any via not in its enum, and Event has no other
// field, so no session name, prompt, path, repo, PR or message text can get
// in. Events go to one JSONL file a month beside settings.json; this package
// never touches the network.
//
// On by default. Off with "feature_stats": false in settings.json (ccs stats
// --off) or CCS_NO_STATS=1.
package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// Feature is a registry id: "<context>.<name>".
type Feature string

// Via says how a TUI feature was reached.
type Via string

const (
	Key   Via = "key"
	Mouse Via = "mouse"
	Auto  Via = "auto" // ccs did it on its own (auto-focus, auto-update)
)

var vias = map[Via]bool{"": true, Key: true, Mouse: true, Auto: true}

// Event is one line of a month's file. Only these fields exist.
type Event struct {
	T   int64   `json:"t"`
	F   Feature `json:"f"`
	C   string  `json:"c"`
	V   string  `json:"v"`
	Via Via     `json:"via,omitempty"`
}

// Info describes a registered feature for the report.
type Info struct {
	ID   Feature
	What string
}

// Registry is every feature ccs counts, in the order the report lists them.
var Registry = []Info{
	// TUI: actions, never navigation or typing.
	{"tui.open", "Enter: resume a session, or go to its tab"},
	{"tui.box", "Enter/^S/click on a live session: type in its message box"},
	{"tui.held_pane", "Enter on a session whose prompt ccs serve holds: answer it in the bottom pane"},
	{"tui.send", "send a message from the message box"},
	{"tui.run_command", "run a ! command from the message box, in ccs, in the session's folder"},
	{"tui.slash_command", "type a / slash command from the message box into the session's terminal"},
	{"tui.paste_image", "^V paste a screenshot into a message"},
	{"tui.fork", "^F fork a session"},
	{"tui.search", "type a search"},
	{"tui.hit_jump", "^] / ^\\ jump to the next or previous search hit"},
	{"tui.links", "^O the links popup"},
	{"tui.link_open", "open a link (from ^O or a click)"},
	{"tui.details", "^X F or click the summary line: preview details"},
	{"tui.refresh", "^R refresh now"},
	{"tui.usage_screen", "Tab (nothing waiting) or ^X Y: token use and allowance"},
	{"tui.next_waiting", "Tab while something waits: select the next session waiting on you"},
	{"tui.changelog", "^L changelog"},
	{"tui.help", "? help"},
	{"tui.pin", "^X P pin or unpin"},
	{"tui.pin_move", "Alt+Up/Down move a pinned session"},
	{"tui.snooze", "^X S open the snooze picker"},
	{"tui.snooze_settle", "^X S settle until it does something"},
	{"tui.snooze_1h", "^X S snooze an hour"},
	{"tui.snooze_4h", "^X S snooze 4 hours"},
	{"tui.snooze_tomorrow", "^X S snooze until tomorrow"},
	{"tui.wake", "^X S on a put-away session: wake it"},
	{"tui.rename", "^X E rename"},
	{"tui.prune", "^X X prune a transcript"},
	{"tui.kill", "^X K stop a session and close its tab"},
	{"tui.restart", "^X R restart a session"},
	{"tui.delete", "^X D delete a transcript"},
	{"tui.new_session", "^X N start a new session"},
	{"tui.orchestrator", "^X O open the orchestrator"},
	{"tui.orchestrator_setup", "accept the offer to set up an orchestrator"},
	{"tui.tree_view", "^X C cycle closed sessions / folded trees"},
	{"tui.group", "^X G group by folder"},
	{"tui.reorg", "^X M plan a reorg into topics"},
	{"tui.reorg_apply", "apply a reorg plan"},
	{"tui.table", "^X T table layout"},
	{"tui.hide_list", "^X L hide the session list"},
	{"tui.headless", "^X H show or hide headless runs"},
	{"tui.auto_focus", "^X A toggle auto-focus"},
	{"tui.auto_focus_switch", "auto-focus switched to a waiting session's tab"},
	{"tui.auto_update", "^X I toggle automatic updates"},
	{"tui.update_install", "install an offered update"},
	{"tui.update_auto", "an update installed itself while away"},
	{"tui.reopen", "reopen sessions after a restart"},
	{"tui.profile", "^X Q record a CPU profile"},
	{"tui.serve", "^X B start or stop ccs serve (its launchd agent)"},
	{"tui.account", "^X U or click: the account switcher"},
	{"tui.account_switch", "switch Claude account"},
	{"tui.account_add", "add a Claude account"},

	// CLI subcommands and flags (never their values).
	{"cli.tui", "ccs: the list"},
	{"cli.all", "ccs --all"},
	{"cli.max_age", "ccs --max-age"},
	{"cli.claude_args", "ccs -- <claude flags>"},
	{"cli.dump", "ccs --dump"},
	{"cli.help", "ccs --help"},
	{"cli.version", "ccs --version"},
	{"cli.prune", "ccs prune"},
	{"cli.launch", "ccs launch"},
	{"cli.orchestrator", "ccs orchestrator"},
	{"cli.reorg", "ccs reorg"},
	{"cli.adopt", "ccs adopt"},
	{"cli.close", "ccs close"},
	{"cli.restart", "ccs restart"},
	{"cli.link", "ccs link"},
	{"cli.watch", "ccs watch"},
	{"cli.claim", "ccs claim"},
	{"cli.review", "ccs review (gone)"},
	{"cli.prs", "ccs prs"},
	{"cli.orchestrator_report", "ccs orchestrator-report"},
	{"cli.depends", "ccs depends"},
	{"cli.sessions", "ccs sessions"},
	{"cli.mcp", "ccs mcp"},
	{"cli.stats", "ccs stats"},

	// MCP tools.
	{"mcp.search_sessions", "search_sessions"},
	{"mcp.list_sessions", "list_sessions"},
	{"mcp.session_triage", "session_triage"},
	{"mcp.read_session", "read_session"},
	{"mcp.launch_session", "launch_session"},
	{"mcp.close_session", "close_session"},
	{"mcp.adopt_session", "adopt_session"},
	{"mcp.restart_session", "restart_session"},
	{"mcp.list_prs", "list_prs"},
	{"mcp.claim_pr", "claim_pr"},
	{"mcp.depends", "depends"},
	{"mcp.orchestrator_report", "orchestrator_report"},
	{"mcp.child_notices", "child_notices"},

	// ccs watch events and nudges.
	{"watch.watch_started", "watch started"},
	{"watch.new", "a new PR"},
	{"watch.ready", "a PR ready to merge"},
	{"watch.needs_review", "a PR needs review"},
	{"watch.review_not_needed", "a PR stopped needing review"},
	{"watch.checks_failing", "checks failing"},
	{"watch.checks_passed", "checks passed"},
	{"watch.conflicts", "merge conflicts"},
	{"watch.approved", "approved"},
	{"watch.changes_requested", "changes requested"},
	{"watch.threads_unresolved", "unresolved review threads"},
	{"watch.threads_resolved", "review threads resolved"},
	{"watch.threads_unanswered", "unanswered review threads"},
	{"watch.blocked", "blocked by a dependency (ccs depends)"},
	{"watch.unblocked", "unblocked"},
	{"watch.blocked_automerge", "auto-merge on while blocked"},
	{"watch.merged_early", "merged before its dependency"},
	{"watch.merged", "merged"},
	{"watch.closed", "closed"},
	{"watch.claimed", "a PR claimed"},
	{"watch.session_stalled", "a session stalled"},
	{"watch.session_done", "a session done"},
	{"watch.session_active", "a session active again"},
	{"watch.session_started", "a session started"},
	{"watch.session_ended", "a session ended"},
	{"watch.child_turn", "an orchestrator told a child finished a turn"},
	{"watch.nudged", "a session nudged about its PR"},
	{"watch.nudge_failed", "a nudge failed"},
}

var known = func() map[Feature]bool {
	m := make(map[Feature]bool, len(Registry))
	for _, i := range Registry {
		m[i.ID] = true
	}
	return m
}()

// Known reports whether f is in the registry.
func Known(f Feature) bool { return known[f] }

// Context is the part of an id before its dot.
func (f Feature) Context() string {
	c, _, _ := strings.Cut(string(f), ".")
	return c
}

// Dir holds the month files, beside settings.json.
var Dir = func() string { return filepath.Join(filepath.Dir(state.SettingsPath), "stats") }

// MaxFile is the size past which a month's file takes no more events.
const MaxFile = 5 << 20

// Keep is how many months of files are kept.
const Keep = 12

const maxBuffered = 1000

var (
	mu       sync.Mutex
	buf      []Event
	started  bool
	pruned   bool
	now      = time.Now
	interval = 5 * time.Second
)

var versionRe = regexp.MustCompile(`^(dev|\d+(\.\d+){0,3})$`)

// version is the running release, or "unknown" for anything that doesn't
// look like one, so not even a build string can carry text.
func version() string {
	v := strings.TrimPrefix(state.Version, "v")
	if versionRe.MatchString(v) {
		return v
	}
	return "unknown"
}

// Record notes one use of f. It never blocks on disk: the event is buffered
// and written by a background flush (or Flush). Unknown ids and vias are
// dropped.
func Record(f Feature, via Via) {
	if !known[f] || !vias[via] {
		return
	}
	if f.Context() != "tui" {
		via = ""
	}
	e := Event{T: now().Unix(), F: f, C: f.Context(), V: version(), Via: via}
	mu.Lock()
	defer mu.Unlock()
	if len(buf) < maxBuffered {
		buf = append(buf, e)
	}
	if !started {
		started = true
		go func() {
			for range time.Tick(interval) {
				Flush()
			}
		}()
	}
}

// Enabled reports whether stats are on: CCS_NO_STATS unset (or 0) and
// settings.json not saying "feature_stats": false.
func Enabled() bool {
	if v := os.Getenv("CCS_NO_STATS"); v != "" && v != "0" {
		return false
	}
	b, err := os.ReadFile(state.SettingsPath)
	if err != nil {
		return true
	}
	var s struct {
		On *bool `json:"feature_stats"`
	}
	if json.Unmarshal(b, &s) != nil || s.On == nil {
		return true
	}
	return *s.On
}

// Flush writes what's buffered, in one append to this month's file. Call it
// before the process execs or exits. Off, it drops the buffer unwritten.
func Flush() {
	mu.Lock()
	events := buf
	buf = nil
	doPrune := !pruned
	pruned = true
	mu.Unlock()
	if len(events) == 0 || !Enabled() {
		return
	}
	dir := Dir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	if doPrune {
		pruneOld(dir)
	}
	var b strings.Builder
	for _, e := range events {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, time.Unix(events[0].T, 0).Format("2006-01")+".jsonl")
	if fi, err := os.Stat(path); err == nil && fi.Size() >= MaxFile {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.WriteString(b.String()) // one write, so processes never interleave
	f.Close()
}

var monthFile = regexp.MustCompile(`^\d{4}-\d{2}\.jsonl$`)

// months lists the month files in dir, oldest first.
func months(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if monthFile.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// pruneOld removes month files beyond the newest Keep.
func pruneOld(dir string) {
	names := months(dir)
	for len(names) > Keep {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// SetEnabled writes "feature_stats" into settings.json, keeping every other
// key as it is.
func SetEnabled(on bool) error {
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(state.SettingsPath); err == nil {
		json.Unmarshal(b, &raw)
	}
	raw["feature_stats"], _ = json.Marshal(on)
	data, _ := json.Marshal(raw)
	if err := os.MkdirAll(filepath.Dir(state.SettingsPath), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(state.SettingsPath), "settings-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o600)
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), state.SettingsPath)
}

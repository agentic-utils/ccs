package stats

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// setup points settings.json (and so the stats folder) at a temp dir, with
// stats on and an empty buffer.
func setup(t *testing.T) string {
	t.Helper()
	oldSettings, oldVersion, oldNow := state.SettingsPath, state.Version, now
	dir := t.TempDir()
	state.SettingsPath = filepath.Join(dir, "settings.json")
	state.Version = "1.2.3"
	t.Setenv("CCS_NO_STATS", "")
	mu.Lock()
	buf, started, pruned = nil, true, false // no background flusher in tests: Flush is called by hand
	mu.Unlock()
	t.Cleanup(func() {
		Flush()
		state.SettingsPath, state.Version, now = oldSettings, oldVersion, oldNow
	})
	return filepath.Join(dir, "stats")
}

func readLines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, name := range months(dir) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
			if len(l) == 0 {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(l, &m); err != nil {
				t.Fatalf("bad line %q: %v", l, err)
			}
			out = append(out, m)
		}
	}
	return out
}

func TestRecordWritesOnlyTheSchema(t *testing.T) {
	dir := setup(t)
	Record("tui.pin", Key)
	Record("cli.prs", Mouse) // via is only kept in the TUI
	Flush()
	lines := readLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	var keys []string
	for k := range lines[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"c", "f", "t", "v", "via"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys %v, want %v", keys, want)
	}
	if lines[0]["f"] != "tui.pin" || lines[0]["c"] != "tui" || lines[0]["v"] != "1.2.3" || lines[0]["via"] != "key" {
		t.Errorf("first line %v", lines[0])
	}
	if _, ok := lines[1]["via"]; ok || lines[1]["c"] != "cli" {
		t.Errorf("cli line %v: want no via", lines[1])
	}
	// The struct itself can hold nothing else.
	var fields []string
	for i := 0; i < reflect.TypeOf(Event{}).NumField(); i++ {
		fields = append(fields, reflect.TypeOf(Event{}).Field(i).Tag.Get("json"))
	}
	if want := []string{"t", "f", "c", "v", "via,omitempty"}; !reflect.DeepEqual(fields, want) {
		t.Errorf("Event fields %v, want %v: a new field needs the same review as the registry", fields, want)
	}
}

// Nothing sensitive can be written: an id or via that isn't registered is
// dropped, and a version that isn't one is replaced.
func TestNoSensitiveFieldCanBeWritten(t *testing.T) {
	dir := setup(t)
	secrets := []string{
		"/Users/someone/Code/secret-repo",
		"https://github.com/acme/secret/pull/42",
		"acme/secret#42",
		"fix-login-flow",
		"please rotate the prod key",
		"tui.pin /Users/someone",
		"mcp.read_session\n{\"f\":\"x\"}",
	}
	for _, s := range secrets {
		Record(Feature(s), Key)
		Record("tui.pin", Via(s))
		Record(Feature("mcp."+s), "")
		Record(Feature("watch."+s), "")
	}
	state.Version = "1.0.0 /Users/someone/secret"
	Record("tui.help", Key)
	Flush()
	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want only the help one: %v", len(lines), lines)
	}
	if lines[0]["v"] != "unknown" {
		t.Errorf("version %v written, want unknown", lines[0]["v"])
	}
	raw := ""
	for _, name := range months(dir) {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		raw += string(b)
	}
	for _, s := range append(secrets, "someone", "secret", "acme") {
		if strings.Contains(raw, s) {
			t.Errorf("%q reached the file: %s", s, raw)
		}
	}
}

func TestOffSwitch(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		dir := setup(t)
		t.Setenv("CCS_NO_STATS", "1")
		Record("tui.pin", Key)
		Flush()
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("stats folder made with CCS_NO_STATS=1: %v", err)
		}
	})
	t.Run("settings", func(t *testing.T) {
		dir := setup(t)
		os.WriteFile(state.SettingsPath, []byte(`{"table":true}`), 0o600)
		var out, errOut bytes.Buffer
		if code := Run([]string{"--off"}, &out, &errOut); code != 0 {
			t.Fatalf("--off exit %d: %s", code, errOut.String())
		}
		b, _ := os.ReadFile(state.SettingsPath)
		if !strings.Contains(string(b), `"table":true`) || !strings.Contains(string(b), `"feature_stats":false`) {
			t.Errorf("settings after --off: %s", b)
		}
		if Enabled() {
			t.Fatal("still enabled after --off")
		}
		Record("tui.pin", Key)
		Flush()
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("stats folder made while off: %v", err)
		}
		Run([]string{"--on"}, &out, &errOut)
		if !Enabled() {
			t.Fatal("not enabled after --on")
		}
		Record("tui.pin", Key)
		Flush()
		if n := len(readLines(t, dir)); n != 1 {
			t.Errorf("got %d lines after --on, want 1", n)
		}
	})
	t.Run("on by default", func(t *testing.T) {
		setup(t)
		if !Enabled() {
			t.Error("off with no settings file and no env")
		}
		t.Setenv("CCS_NO_STATS", "0")
		if !Enabled() {
			t.Error("CCS_NO_STATS=0 turned it off")
		}
	})
}

func TestRegistryIDs(t *testing.T) {
	id := regexp.MustCompile(`^(tui|cli|mcp|watch)\.[a-z0-9_]+$`)
	seen := map[Feature]bool{}
	for _, i := range Registry {
		if !id.MatchString(string(i.ID)) {
			t.Errorf("bad id %q", i.ID)
		}
		if seen[i.ID] {
			t.Errorf("%q registered twice", i.ID)
		}
		seen[i.ID] = true
		if i.What == "" {
			t.Errorf("%q has no description", i.ID)
		}
	}
}

// The package never touches the network.
func TestNoNetworkImports(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "net" || strings.HasPrefix(p, "net/") || p == "os/exec" {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
}

func TestSizeCapAndRetention(t *testing.T) {
	dir := setup(t)
	os.MkdirAll(dir, 0o700)
	for i := 1; i <= Keep+2; i++ {
		os.WriteFile(filepath.Join(dir, time.Date(2020, time.Month(i), 1, 0, 0, 0, 0, time.Local).Format("2006-01")+".jsonl"), nil, 0o600)
	}
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	now = func() time.Time { return at }
	full := filepath.Join(dir, "2026-11.jsonl")
	os.WriteFile(full, make([]byte, MaxFile), 0o600)
	Record("tui.pin", Key)
	Flush()
	if fi, _ := os.Stat(full); fi.Size() != MaxFile {
		t.Errorf("a full month grew to %d", fi.Size())
	}
	if names := months(dir); len(names) != Keep || names[0] == "2020-01.jsonl" {
		t.Errorf("kept %v, want the newest %d", names, Keep)
	}
}

func TestReport(t *testing.T) {
	dir := setup(t)
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	for _, ago := range []int{1, 2, 20, 40} {
		now = func() time.Time { return at.AddDate(0, 0, -ago) }
		Record("tui.pin", Key)
		Flush()
	}
	now = func() time.Time { return at.AddDate(0, 0, -3) }
	Record("tui.pin", Mouse)
	Record("mcp.list_prs", "")
	Flush()
	os.WriteFile(filepath.Join(dir, "2026-11.jsonl"), append(must(os.ReadFile(filepath.Join(dir, "2026-11.jsonl"))), []byte("not json\n{\"t\":1,\"f\":\"tui.gone\"}\n")...), 0o600)
	r := Build(at)
	if len(r.Used) != 2 {
		t.Fatalf("used %+v", r.Used)
	}
	pin := r.Used[0]
	if pin.Feature != "tui.pin" || pin.Last7 != 3 || pin.Last30 != 4 || pin.Total != 5 || pin.Keys != 4 || pin.Mouse != 1 {
		t.Errorf("pin row %+v", pin)
	}
	if pin.First != at.AddDate(0, 0, -40).Format("2006-01-02") || pin.Last != at.AddDate(0, 0, -1).Format("2006-01-02") {
		t.Errorf("pin first/last %s %s", pin.First, pin.Last)
	}
	if len(r.Never) != len(Registry)-2 {
		t.Errorf("never used %d, want %d", len(r.Never), len(Registry)-2)
	}
	var out bytes.Buffer
	if Run(nil, &out, &out) != 0 || !strings.Contains(out.String(), "tui.pin") || !strings.Contains(out.String(), "Never used since") {
		t.Errorf("report:\n%s", out.String())
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

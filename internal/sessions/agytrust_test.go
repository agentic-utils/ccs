package sessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/state"
)

// agyRootsSetting writes agy_trusted_roots into the tests' settings.json,
// putting the file back afterwards.
func agyRootsSetting(t *testing.T, roots []string) {
	t.Helper()
	old, err := os.ReadFile(state.SettingsPath)
	t.Cleanup(func() {
		if err != nil {
			os.Remove(state.SettingsPath)
		} else {
			os.WriteFile(state.SettingsPath, old, 0o600)
		}
	})
	data, _ := json.Marshal(map[string]any{state.AgyTrustedRootsKey: roots})
	if err := os.WriteFile(state.SettingsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// launchedScript is the launcher an agy launch into cwd writes: the tab
// "fails" to open once it has it, so nothing waits.
func launchedScript(t *testing.T, name, cwd string) string {
	t.Helper()
	var script string
	OpenLaunchTab = func(s string) (string, error) {
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		script = string(b)
		return "", errors.New("no tab in tests")
	}
	LaunchSession(LaunchOpts{Name: name, Brief: "x", Cwd: cwd, Agy: true})
	if script == "" {
		t.Fatal("no launcher written")
	}
	return script
}

func TestAgyLaunchTrustsFoldersUnderTheRoots(t *testing.T) {
	sessionsFixture(t)
	stubLaunch(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in, out := filepath.Join(base, "code", "app"), filepath.Join(base, "other")
	for _, d := range []string{in, out} {
		os.MkdirAll(d, 0o700)
	}

	if got := launchedScript(t, "agy-off", in); strings.Contains(got, "--settings") {
		t.Errorf("no roots set: agy asks as before:\n%s", got)
	}
	agyRootsSetting(t, []string{filepath.Join(base, "code")})
	got := launchedScript(t, "agy-in", in)
	want := `'--settings' '{"trustedWorkspaces":["` + in + `"]}' --prompt-interactive "$(cat `
	if !strings.Contains(got, want) {
		t.Errorf("under a root: --settings trusts the folder, after --add-dir and before the brief and CCS_AGY_ARGS:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "${=CCS_AGY_ARGS}") {
		t.Errorf("CCS_AGY_ARGS stays last:\n%s", got)
	}
	if got := launchedScript(t, "agy-out", out); strings.Contains(got, "--settings") {
		t.Errorf("outside every root: nothing:\n%s", got)
	}
}

func TestAgyClaudeAndCodexLaunchesGetNoTrust(t *testing.T) {
	sessionsFixture(t)
	stubLaunch(t)
	dir := t.TempDir()
	agyRootsSetting(t, []string{dir})
	for _, o := range []LaunchOpts{{Name: "c", Brief: "x", Cwd: dir}, {Name: "x", Brief: "x", Cwd: dir, Codex: true}} {
		var script string
		OpenLaunchTab = func(s string) (string, error) {
			b, _ := os.ReadFile(s)
			script = string(b)
			return "", errors.New("no tab in tests")
		}
		LaunchSession(o)
		if strings.Contains(script, "trustedWorkspaces") {
			t.Errorf("%s: agy's flag on another agent:\n%s", o.Name, script)
		}
	}
}

func TestAgyTrustOnResume(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agyRootsSetting(t, []string{dir})
	if a := AgyTrust(dir, "conv"); len(a) != 2 || a[0] != "--settings" {
		t.Errorf("a resume under a root is trusted, got %q", a)
	}
	if a := AgyTrust(".", "conv"); a != nil {
		t.Errorf("a folder that isn't known (\".\") never is, got %q", a)
	}
}

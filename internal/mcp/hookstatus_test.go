package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// writeHooks puts exactly these of ccs's hooks (by subcommand) in the
// test's Claude Code settings, beside a hook of someone else's.
func writeHooks(t *testing.T, subs ...string) {
	t.Helper()
	hooks := map[string]any{"PreToolUse": []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/other-tool check"}}}}}
	for _, h := range ourHooks {
		for _, s := range subs {
			if h.Sub == s {
				hooks[h.Event] = []any{hookEntryFor(h, "/opt/homebrew/bin/ccs hook "+s)}
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"hooks": hooks})
	if err := os.WriteFile(claudeSettingsPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(claudeSettingsPath()) })
}

func TestHooksOutOfDate(t *testing.T) {
	writeHooks(t, "permission") // installed before Stop and SessionStart existed
	if got := strings.Join(HooksOutOfDate(), ","); got != "Stop,SessionStart" {
		t.Errorf("the two missing: %q", got)
	}
	writeHooks(t, "permission", "stop", "session-start")
	if got := HooksOutOfDate(); got != nil {
		t.Errorf("all installed: %v", got)
	}
	writeHooks(t) // none of ccs's: a choice, not out of date
	if got := HooksOutOfDate(); got != nil {
		t.Errorf("none installed: %v", got)
	}
	os.Remove(claudeSettingsPath())
	if got := HooksOutOfDate(); got != nil {
		t.Errorf("no settings file: %v", got)
	}
}

func TestHookStatusPrintsTheMissing(t *testing.T) {
	writeHooks(t, "permission")
	before, _ := os.ReadFile(claudeSettingsPath())
	var out, errb bytes.Buffer
	if code := RunHook([]string{"status"}, nil, &out, &errb); code != 1 {
		t.Errorf("missing hooks exit 1, got %d", code)
	}
	for _, want := range []string{"PermissionRequest  installed", "Stop               missing", "SessionStart       missing", "ccs hook install"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status says %q:\n%s", want, out.String())
		}
	}
	if after, _ := os.ReadFile(claudeSettingsPath()); !bytes.Equal(before, after) {
		t.Error("status only reads the settings")
	}

	writeHooks(t, "permission", "stop", "session-start")
	out.Reset()
	if code := RunHook([]string{"status"}, nil, &out, &errb); code != 0 || strings.Contains(out.String(), "missing") {
		t.Errorf("all installed: exit %d\n%s", code, out.String())
	}
}

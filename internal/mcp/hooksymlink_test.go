package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A settings file kept in a dotfiles repo and linked into place: install
// and uninstall edit the file the link points to and leave the link.
func TestHookInstallWritesThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "dotfiles", "claude")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, "settings.json")
	if err := os.WriteFile(target, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "claude", "settings.json")
	os.MkdirAll(filepath.Dir(link), 0o700)
	if err := os.Symlink("../dotfiles/claude/settings.json", link); err != nil {
		t.Fatal(err)
	}

	if _, err := editHookSettings(link, true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", fi.Mode(), err)
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("the target's mode changed to %v", st.Mode().Perm())
	}
	var got map[string]any
	raw, _ := os.ReadFile(target)
	if err := json.Unmarshal(raw, &got); err != nil || got["theme"] != "dark" || got["hooks"] == nil {
		t.Errorf("target holds the hooks and the old keys: %s", raw)
	}
	if b, err := os.ReadFile(link + backupSuffix); err != nil || string(b) != `{"theme":"dark"}` {
		t.Errorf("backup beside the link: %q %v", b, err)
	}
	if _, err := os.Stat(target + backupSuffix); err == nil {
		t.Error("nothing is left in the dotfiles folder")
	}
	if left, _ := filepath.Glob(filepath.Join(repo, "*.tmp-*")); len(left) > 0 {
		t.Errorf("temp files left: %v", left)
	}

	if _, err := editHookSettings(link, false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("uninstall replaced the link")
	}
	if raw, _ := os.ReadFile(target); strings.Contains(string(raw), " hook permission") {
		t.Errorf("uninstall left hooks: %s", raw)
	}
}

// A link whose target doesn't exist yet gets the target written, not a
// regular file in the link's place.
func TestHookInstallDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "repo", "settings.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := editHookSettings(link, true); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced")
	}
	if raw, err := os.ReadFile(target); err != nil || !strings.Contains(string(raw), " hook permission") {
		t.Errorf("target written: %s %v", raw, err)
	}
}

// None installed after `ccs hook install` had edited the file means the
// hooks were lost (the file replaced); after an uninstall it's a choice.
func TestHooksOutOfDateWhenLost(t *testing.T) {
	path := claudeSettingsPath()
	os.Remove(path + backupSuffix)
	os.Remove(path + removedSuffix)
	t.Cleanup(func() { os.Remove(path + backupSuffix); os.Remove(path + removedSuffix) })
	writeHooks(t)
	if got := HooksOutOfDate(); got != nil {
		t.Errorf("never installed: %v", got)
	}
	os.WriteFile(path+backupSuffix, []byte(`{}`), 0o600) // an earlier install's backup
	if got := strings.Join(HooksOutOfDate(), ","); got != "PermissionRequest,Stop,SessionStart" {
		t.Errorf("lost: %q", got)
	}

	if _, err := editHookSettings(path, true); err != nil {
		t.Fatal(err)
	}
	if got := HooksOutOfDate(); got != nil {
		t.Errorf("installed again: %v", got)
	}
	if _, err := editHookSettings(path, false); err != nil {
		t.Fatal(err)
	}
	if got := HooksOutOfDate(); got != nil {
		t.Errorf("uninstalled on purpose: %v", got)
	}
	if _, err := editHookSettings(path, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + removedSuffix); err == nil {
		t.Error("install clears the uninstall mark")
	}

	// Uninstalling when they're already gone records the choice too.
	writeHooks(t)
	if HooksOutOfDate() == nil {
		t.Fatal("lost again")
	}
	if _, err := editHookSettings(path, false); err != nil {
		t.Fatal(err)
	}
	if got := HooksOutOfDate(); got != nil {
		t.Errorf("uninstall with none there: %v", got)
	}
}

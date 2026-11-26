package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// agyRoots points SettingsPath at a temp settings.json holding roots
// (nothing written for nil).
func agyRoots(t *testing.T, roots []string) {
	t.Helper()
	old := SettingsPath
	SettingsPath = filepath.Join(t.TempDir(), "settings.json")
	t.Cleanup(func() { SettingsPath = old })
	if roots == nil {
		return
	}
	data, _ := json.Marshal(map[string]any{"table": true, AgyTrustedRootsKey: roots})
	if err := os.WriteFile(SettingsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// trusted is the trustedWorkspaces list AgyTrustArgs passes, nil for none.
func trusted(t *testing.T, args []string) []string {
	t.Helper()
	if args == nil {
		return nil
	}
	if len(args) != 2 || args[0] != "--settings" {
		t.Fatalf("want --settings <json>, got %q", args)
	}
	var s map[string][]string
	if err := json.Unmarshal([]byte(args[1]), &s); err != nil {
		t.Fatalf("not JSON: %q", args[1])
	}
	return s["trustedWorkspaces"]
}

// tree makes base/<p> folders and returns base with its symlinks resolved
// (macOS's temp folder is under the /var -> /private/var link).
func tree(t *testing.T, dirs ...string) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func TestAgyTrustOffByDefault(t *testing.T) {
	base := tree(t, "code/app")
	agyRoots(t, nil)
	if args, _ := AgyTrustArgs(filepath.Join(base, "code/app")); args != nil {
		t.Errorf("no settings file: nothing trusted, got %q", args)
	}
	agyRoots(t, []string{})
	if args, _ := AgyTrustArgs(filepath.Join(base, "code/app")); args != nil {
		t.Errorf("empty list: nothing trusted, got %q", args)
	}
}

func TestAgyTrustUnderRoot(t *testing.T) {
	base := tree(t, "code/app/sub", "code-other", "elsewhere")
	root := filepath.Join(base, "code")
	agyRoots(t, []string{filepath.Join(base, "missing"), root})
	for _, d := range []string{"code", "code/app", "code/app/sub"} {
		cwd := filepath.Join(base, d)
		args, r := AgyTrustArgs(cwd)
		if got := trusted(t, args); !slices.Equal(got, []string{cwd}) {
			t.Errorf("%s is under the root: trusted exactly, got %q", d, got)
		}
		if r != root {
			t.Errorf("%s: matched root %q, want %q", d, r, root)
		}
	}
	for _, d := range []string{"code-other", "elsewhere", "code/../elsewhere", "code/app/../../elsewhere", "missing-folder"} {
		if args, _ := AgyTrustArgs(filepath.Join(base, d)); args != nil {
			t.Errorf("%s isn't under the root: nothing, got %q", d, args)
		}
	}
}

func TestAgyTrustResolvesSymlinks(t *testing.T) {
	base := tree(t, "code/app", "outside/secret")
	agyRoots(t, []string{filepath.Join(base, "code")})
	// A link inside the root that leads out of it doesn't count.
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(base, "code/escape")); err != nil {
		t.Fatal(err)
	}
	if args, _ := AgyTrustArgs(filepath.Join(base, "code/escape/secret")); args != nil {
		t.Errorf("a link out of the root: nothing, got %q", args)
	}
	// A link outside the root that leads into it does, with both spellings
	// listed: agy sees the path the launcher cd'd to.
	link := filepath.Join(base, "outside/into")
	if err := os.Symlink(filepath.Join(base, "code/app"), link); err != nil {
		t.Fatal(err)
	}
	got := trusted(t, mustArgs(t, link))
	if !slices.Equal(got, []string{filepath.Join(base, "code/app"), link}) {
		t.Errorf("a link into the root: its real path and the path given, got %q", got)
	}
	// A root that is itself a link is resolved too.
	rootLink := filepath.Join(base, "rootlink")
	if err := os.Symlink(filepath.Join(base, "code"), rootLink); err != nil {
		t.Fatal(err)
	}
	agyRoots(t, []string{rootLink})
	if got := trusted(t, mustArgs(t, filepath.Join(base, "code/app"))); !slices.Equal(got, []string{filepath.Join(base, "code/app")}) {
		t.Errorf("a root given as a link: its folder's contents trusted, got %q", got)
	}
}

func TestAgyTrustHomeRoot(t *testing.T) {
	home := tree(t, "proj/app")
	t.Setenv("HOME", home)
	agyRoots(t, []string{"~/proj"})
	if got := trusted(t, mustArgs(t, filepath.Join(home, "proj/app"))); !slices.Equal(got, []string{filepath.Join(home, "proj/app")}) {
		t.Errorf("~/ is the home folder, got %q", got)
	}
}

func TestAgyTrustIgnoresRelativeRoots(t *testing.T) {
	base := tree(t, "code/app")
	t.Chdir(base)
	agyRoots(t, []string{"code", "."})
	if args, _ := AgyTrustArgs(filepath.Join(base, "code/app")); args != nil {
		t.Errorf("a relative root depends on where ccs runs: ignored, got %q", args)
	}
}

func mustArgs(t *testing.T, cwd string) []string {
	t.Helper()
	args, _ := AgyTrustArgs(cwd)
	if args == nil {
		t.Fatalf("%s: expected it trusted", cwd)
	}
	return args
}

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// AgyTrustedRootsKey is the settings.json key listing the folders under
// which ccs answers agy's folder-trust question for the sessions it starts.
// Empty (the default) leaves agy asking as it always has.
const AgyTrustedRootsKey = "agy_trusted_roots"

// AgyTrustedRoots is the agy_trusted_roots list as written: a JSON array of
// folders, a leading ~/ meaning the home folder. Nil when unset or unreadable.
func AgyTrustedRoots() []string {
	b, err := os.ReadFile(SettingsPath)
	if err != nil {
		return nil
	}
	var s map[string]json.RawMessage
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	var roots []string
	if json.Unmarshal(s[AgyTrustedRootsKey], &roots) != nil {
		return nil
	}
	return roots
}

// realPath is p made absolute with ~/ expanded, then every symlink and ..
// resolved, so it names the folder itself. False when it doesn't exist.
func realPath(p string) (string, bool) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		p = filepath.Join(home, p[1:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	return real, true
}

// within reports whether path is root or a folder under it. Both are
// already resolved by realPath.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// AgyTrustArgs are the agy flags that trust cwd for that session only:
// `--settings {"trustedWorkspaces":[...]}`, which agy reads over its
// settings.json without saving it, so nothing in ~/.gemini changes. agy asks
// "Trust this folder?" for the folder it runs in alone, matched exactly (a
// parent folder or a wildcard in its list doesn't count, and --add-dir
// folders aren't checked), and the flag's list replaces the file's for the
// session, so the list holds cwd: as given (agy sees the path the launcher
// cd'd to) and resolved, when they differ.
//
// Nil unless cwd, with symlinks and .. resolved, is one of the roots in
// agy_trusted_roots or under one (each resolved the same way, one that
// doesn't exist skipped), so a path can't escape a root through a link.
// root is the root that matched, for the activity log.
func AgyTrustArgs(cwd string) (args []string, root string) {
	real, ok := realPath(cwd)
	if !ok {
		return nil, ""
	}
	for _, r := range AgyTrustedRoots() {
		if r != "~" && !strings.HasPrefix(r, "~/") && !filepath.IsAbs(r) {
			continue // relative to wherever ccs runs: never a root
		}
		rr, ok := realPath(r)
		if !ok || !within(rr, real) {
			continue
		}
		dirs := []string{real}
		if abs, err := filepath.Abs(cwd); err == nil && abs != real {
			dirs = append(dirs, abs)
		}
		data, _ := json.Marshal(map[string][]string{"trustedWorkspaces": dirs})
		return []string{"--settings", string(data)}, r
	}
	return nil, ""
}

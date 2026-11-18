package stats

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every feature id written as a literal anywhere in ccs is registered: a
// misspelt one would be dropped silently and its feature look never used.
func TestRecordedLiteralsAreRegistered(t *testing.T) {
	lit := regexp.MustCompile(`"((?:tui|cli|mcp|watch)\.[a-z0-9_]+)"`)
	files, _ := filepath.Glob("../*/*.go")
	main, _ := filepath.Glob("../../*.go")
	files = append(files, main...)
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || strings.HasPrefix(f, "../stats/") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range lit.FindAllStringSubmatch(string(b), -1) {
			found++
			if !Known(Feature(m[1])) {
				t.Errorf("%s records %q, which isn't in the registry", f, m[1])
			}
		}
	}
	if found < 50 {
		t.Errorf("found only %d recorded ids: is the pattern still right?", found)
	}
}

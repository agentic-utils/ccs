package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestPrintHelp(t *testing.T) {
	// Just call it to ensure no panics - we can't easily test stdout
	// but this at least ensures the function doesn't crash
	printHelp()
}

func TestHelpTextListsEveryKey(t *testing.T) {
	var buf bytes.Buffer
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	printHelp()
	w.Close()
	os.Stdout = old
	buf.ReadFrom(r)
	for _, k := range []string{"Ctrl+S", "Ctrl+O", "Ctrl+L", "Ctrl+G", "Tab", "Ctrl+X", "Ctrl+R", "Ctrl+F"} {
		if !strings.Contains(buf.String(), k) {
			t.Errorf("--help should list %s", k)
		}
	}
}

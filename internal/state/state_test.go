package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsRotateAtOneMB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.log")
	if err := os.WriteFile(path, make([]byte, 1<<20+1), 0o644); err != nil {
		t.Fatal(err)
	}
	AppendLog(path, "after %d", 1)
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() != 1<<20+1 {
		t.Errorf("the full log should move to .1: %v", err)
	}
	if data, _ := os.ReadFile(path); !strings.HasSuffix(string(data), "after 1\n") || len(data) > 200 {
		t.Errorf("a fresh log should start: %q", data)
	}
}

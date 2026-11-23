//go:build darwin

package proc

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestParseProcArgs(t *testing.T) {
	buf := binary.LittleEndian.AppendUint32(nil, 3)
	buf = append(buf, "/usr/bin/codex\x00\x00\x00codex\x00resume\x00line one\nline two\x00HOME=/x\x00"...)
	if got, want := parseProcArgs(buf), `codex resume line one\012line two`; got != want {
		t.Errorf("parseProcArgs = %q, want %q", got, want)
	}
	if got := parseProcArgs([]byte{1, 0}); got != "" {
		t.Errorf("short buffer: %q", got)
	}
}

// The in-process listing has this test's own process as ps would print it.
func TestSysListingHasThisProcess(t *testing.T) {
	rows, err := sysListing()
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	for _, r := range rows {
		if r.pid != pid {
			continue
		}
		if r.ppid != strconv.Itoa(os.Getppid()) {
			t.Errorf("ppid %s, want %d", r.ppid, os.Getppid())
		}
		if want := psVis(strings.Join(os.Args, " ")); r.cmd != want {
			t.Errorf("cmd %q, want %q", r.cmd, want)
		}
		return
	}
	t.Fatalf("pid %s not listed among %d", pid, len(rows))
}

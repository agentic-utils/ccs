package profiling

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// serve starts this process's socket for one test.
func serve(t *testing.T) {
	t.Helper()
	stop, err := Serve(RoleTUI)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
}

func TestHeapProfileOverTheSocket(t *testing.T) {
	serve(t)
	s, err := Start(os.Getpid(), Heap, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("no heap profile at %s: %v", s.Path, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("profile mode %v, want 0600", fi.Mode().Perm())
	}
	if filepath.Dir(s.Path) != OutDir() {
		t.Errorf("written to %s, want beside the update log", s.Path)
	}
	if _, busy := Current(); busy {
		t.Error("still marked recording after a heap snapshot")
	}
}

func TestCPUProfileStopsByItselfAndRefusesASecond(t *testing.T) {
	serve(t)
	changes := make(chan struct{}, 8)
	SetOnChange(func() { changes <- struct{}{} })
	t.Cleanup(func() { SetOnChange(nil) })
	s, err := Start(os.Getpid(), CPU, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.Until.IsZero() || time.Until(s.Until) > 2*time.Second {
		t.Errorf("until %v, want about a second away", s.Until)
	}
	if r, ok := Current(); !ok || r.Kind != CPU {
		t.Errorf("Current = %+v, %v while recording", r, ok)
	}
	tg, err := Info(os.Getpid())
	if err != nil || tg.Running == nil || tg.Role != RoleTUI {
		t.Errorf("info while recording: %+v, %v", tg, err)
	}
	if _, err := Start(os.Getpid(), Trace, 1); err == nil || !strings.Contains(err.Error(), "already being recorded") {
		t.Errorf("a second profile: %v, want refused", err)
	}
	if _, err := StartManual(filepath.Join(OutDir(), "cpu-manual.pprof")); !errors.Is(err, ErrBusy) {
		t.Errorf("^T Q during ccs profile: %v, want ErrBusy", err)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s.Path); err != nil || fi.Size() == 0 {
		t.Fatalf("no cpu profile: %v", err)
	}
	if _, busy := Current(); busy {
		t.Error("still recording after it stopped")
	}
	if len(changes) < 2 {
		t.Errorf("%d change notices, want a start and a stop", len(changes))
	}
}

func TestOtherProcessesAreLockedOut(t *testing.T) {
	serve(t)
	// Another ccs holding the machine-wide lock: a fresh descriptor's flock.
	other, err := machineLock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Start(os.Getpid(), CPU, 1)
	other.Close()
	if err == nil || !strings.Contains(err.Error(), "another ccs process") {
		t.Errorf("start while another process records: %v", err)
	}
}

func TestStopEndsARunningProfile(t *testing.T) {
	stop, err := Serve(RoleWatch)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(os.Getpid(), Trace, MaxSeconds)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	s.Close()
	began := time.Now()
	stop()
	if time.Since(began) > 5*time.Second {
		t.Errorf("stop took %v", time.Since(began))
	}
	if _, busy := Current(); busy {
		t.Error("a profile outlived Serve's stop")
	}
	if _, err := os.Stat(SocketPath(os.Getpid())); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
	if fi, err := os.Stat(s.Path); err != nil || fi.Size() == 0 {
		t.Errorf("trace not written: %v", err)
	}
}

func TestCheckRequest(t *testing.T) {
	for _, c := range []struct {
		kind    string
		seconds int
		ok      bool
		want    int
	}{
		{"", 0, true, DefaultSeconds}, {CPU, 5, true, 5}, {Heap, 999, true, 0}, {Trace, MaxSeconds, true, MaxSeconds},
		{CPU, MaxSeconds + 1, false, 0}, {CPU, -1, false, 0}, {"goroutine", 1, false, 0}, {"../x", 1, false, 0},
	} {
		_, got, err := CheckRequest(c.kind, c.seconds)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("CheckRequest(%q, %d) = %d, %v", c.kind, c.seconds, got, err)
		}
	}
}

func TestUntrustedFolderIsRefused(t *testing.T) {
	serve(t)
	if err := os.Chmod(SockDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(SockDir(), 0o700)
	if _, err := Info(os.Getpid()); err == nil || !strings.Contains(err.Error(), "private folder") {
		t.Errorf("a world-writable folder: %v, want refused", err)
	}
}

func TestBadRequestsGetNothing(t *testing.T) {
	serve(t)
	c, err := net.Dial("unix", SocketPath(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintln(c, `{"op":"start","kind":"cpu","seconds":100000}`)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _ := c.Read(buf)
	if !strings.Contains(string(buf[:n]), "seconds must be") {
		t.Errorf("reply %q", buf[:n])
	}
	if _, busy := Current(); busy {
		t.Error("an out-of-range request started a profile")
	}
}

func TestPruneKeepsTheNewest(t *testing.T) {
	dir := OutDir()
	os.MkdirAll(dir, 0o700)
	clear := func() { // earlier tests' profiles are newer than these
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if isProfile(e.Name()) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	clear()
	t.Cleanup(clear)
	base := time.Now().Add(-time.Hour)
	for i := range Keep + 5 {
		p := filepath.Join(dir, "cpu-"+strconv.Itoa(i)+".pprof")
		os.WriteFile(p, []byte("x"), 0o600)
		at := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, at, at)
	}
	os.WriteFile(filepath.Join(dir, "update.log"), []byte("x"), 0o600)
	Prune()
	for i := range Keep + 5 {
		_, err := os.Stat(filepath.Join(dir, "cpu-"+strconv.Itoa(i)+".pprof"))
		if kept := err == nil; kept != (i >= 5) {
			t.Errorf("cpu-%d kept %v", i, kept)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "update.log")); err != nil {
		t.Error("pruned the update log")
	}
}

func TestPickAndCLI(t *testing.T) {
	serve(t)
	pidAlive = func(pid int) bool { return pid != 999999 }
	t.Cleanup(func() { pidAlive = func(pid int) bool { return true } })
	os.WriteFile(SocketPath(999999), nil, 0o600) // a gone process's
	if got, err := Pick(0, ""); err != nil || got.PID != os.Getpid() {
		t.Errorf("Pick = %+v, %v", got, err)
	}
	if _, err := os.Stat(SocketPath(999999)); !os.IsNotExist(err) {
		t.Error("a gone process's socket wasn't swept")
	}
	if _, err := Pick(0, RoleMCP); err == nil {
		t.Error("picked an mcp with none running")
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"--list"}, &out, &errb); code != 0 || !strings.Contains(out.String(), strconv.Itoa(os.Getpid())+"  tui") {
		t.Errorf("--list: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Run([]string{"--kind", "heap"}, &out, &errb); code != 0 || !strings.HasSuffix(strings.TrimSpace(out.String()), ".pprof") {
		t.Errorf("heap: %d %q %q", code, out.String(), errb.String())
	}
	if code := Run([]string{"--seconds", "0", "--kind", "cpu", "--pid", "1"}, &out, &errb); code == 0 {
		t.Error("profiled pid 1, which takes no profiles")
	}
	if code := Run([]string{"--seconds", "301"}, &out, &errb); code != 2 {
		t.Errorf("--seconds 301 exit %d, want 2", code)
	}
}

// Package profiling records profiles of a running ccs (the TUI, ccs watch,
// ccs mcp, ccs serve) for an agent investigating its power or speed, with
// nobody at the keyboard. Each long-running ccs listens on a socket of its
// own, <pid>.sock in a private folder beside the settings (Serve); ccs
// profile and the profile_ccs MCP tool ask it for a CPU profile, a heap
// profile or an execution trace (Start). A CPU profile or trace always
// stops by itself after at most MaxSeconds, and only one profile is
// recorded at a time on the machine. Only this user's processes can ask:
// the folder is 0700, the socket 0600, and both ends check the other's uid
// with the kernel (proc.SameUser), as serve-control.sock does. A request
// carries only a kind and a length; the file's name and place are the
// profiled process's own choice, beside the update log, the newest Keep
// kept.
package profiling

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// The kinds of profile.
const (
	CPU   = "cpu"
	Heap  = "heap"
	Trace = "trace"
)

const (
	// DefaultSeconds is a CPU profile's or trace's length when none is asked.
	DefaultSeconds = 30
	// MaxSeconds is the longest anyone can ask for: it always stops by then.
	MaxSeconds = 300
	// Keep is how many profiles are kept beside the update log; older ones
	// (^T Q's too) are removed after each one is written.
	Keep = 20
)

// SockDir holds each process's socket and the machine-wide lock. A var for
// tests, whose temp folders are too long for a socket path.
var SockDir = func() string { return filepath.Join(filepath.Dir(state.SettingsPath), "profile") }

// OutDir is where profiles are written: beside the update log, as ^T Q's.
var OutDir = func() string { return filepath.Dir(state.UpdateLogPath) }

// Running is a profile being recorded.
type Running struct {
	Kind  string    `json:"kind"`
	Path  string    `json:"path"`
	By    string    `json:"by"`             // "^T Q", or "ccs profile" for one asked for over the socket
	Until time.Time `json:"until,omitzero"` // when it stops by itself; zero for ^T Q's, which records until stopped
}

// ManualBy marks ^T Q's profile.
const ManualBy = "^T Q"

// ErrBusy is a refusal while a profile is being recorded.
var ErrBusy = errors.New("a profile is already being recorded")

var (
	mu       sync.Mutex
	cur      *Running
	curStop  chan struct{} // closed to stop a timed profile early (Serve's stop)
	curDone  chan struct{} // closed once it has been written
	onChange func()
)

// SetOnChange sets what's called (outside any lock) whenever a profile
// starts or stops in this process: the TUI redraws its header.
func SetOnChange(f func()) {
	mu.Lock()
	onChange = f
	mu.Unlock()
}

func notify() {
	mu.Lock()
	f := onChange
	mu.Unlock()
	if f != nil {
		f()
	}
}

// Current is the profile this process is recording, if any.
func Current() (Running, bool) {
	mu.Lock()
	defer mu.Unlock()
	if cur == nil {
		return Running{}, false
	}
	return *cur, true
}

func busyErr(r Running) error {
	if r.Until.IsZero() {
		return fmt.Errorf("%w: a %s profile (%s), until it's stopped", ErrBusy, r.Kind, r.By)
	}
	return fmt.Errorf("%w: a %s profile (%s) until %s", ErrBusy, r.Kind, r.By, r.Until.Format("15:04:05"))
}

// begin marks r as this process's profile, refusing while one is.
func begin(r Running, stop, done chan struct{}) error {
	mu.Lock()
	if cur != nil {
		err := busyErr(*cur)
		mu.Unlock()
		return err
	}
	cur, curStop, curDone = &r, stop, done
	mu.Unlock()
	notify()
	return nil
}

func end() {
	mu.Lock()
	cur, curStop, curDone = nil, nil, nil
	mu.Unlock()
	notify()
}

// StartManual starts ^T Q's CPU profile to path, until the returned stop is
// called. Refused while another profile is recorded in this process.
func StartManual(path string) (func(), error) {
	if err := begin(Running{Kind: CPU, Path: path, By: ManualBy}, nil, nil); err != nil {
		return nil, err
	}
	f, err := create(path)
	if err != nil {
		end()
		return nil, err
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		os.Remove(path)
		end()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			pprof.StopCPUProfile()
			f.Close()
			end()
			Prune()
		})
	}, nil
}

// create makes a new profile file only this user can read.
func create(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// FileName is a profile's name: kind, who, pid and time, so ^T Q's
// (cpu-<time>.pprof) and these sort and prune together.
func FileName(kind, role string, pid int, at time.Time) string {
	ext := ".pprof"
	if kind == Trace {
		ext = ".trace"
	}
	return fmt.Sprintf("%s-%s-%d-%s%s", kind, role, pid, at.Format("20060102-150405.000"), ext)
}

// CheckRequest validates a kind and a length, filling in the defaults.
func CheckRequest(kind string, seconds int) (string, int, error) {
	if kind == "" {
		kind = CPU
	}
	if kind != CPU && kind != Heap && kind != Trace {
		return "", 0, fmt.Errorf("unknown kind %q: cpu, heap or trace", kind)
	}
	if kind == Heap {
		return kind, 0, nil // a snapshot: no length
	}
	if seconds == 0 {
		seconds = DefaultSeconds
	}
	if seconds < 1 || seconds > MaxSeconds {
		return "", 0, fmt.Errorf("seconds must be 1 to %d", MaxSeconds)
	}
	return kind, seconds, nil
}

// machineLock takes the lock that keeps profiles one at a time across every
// ccs process, without waiting. It goes with the process if that crashes.
func machineLock() (*os.File, error) {
	dir := SockDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "profile.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w by another ccs process", ErrBusy)
	}
	return f, nil
}

// start records a profile of this process for an agent: a heap snapshot at
// once, or a CPU profile or trace that stops by itself after seconds (or
// when stopRemote is called). done is closed once the file is written, err
// set first.
func start(kind string, seconds int, role string) (Running, <-chan error, error) {
	kind, seconds, err := CheckRequest(kind, seconds)
	if err != nil {
		return Running{}, nil, err
	}
	if r, ok := Current(); ok { // say what's recording here before the machine lock does
		return Running{}, nil, busyErr(r)
	}
	lock, err := machineLock()
	if err != nil {
		return Running{}, nil, err
	}
	now := time.Now()
	r := Running{Kind: kind, Path: filepath.Join(OutDir(), FileName(kind, role, os.Getpid(), now)), By: "ccs profile"}
	if kind != Heap {
		r.Until = now.Add(time.Duration(seconds) * time.Second)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	if err := begin(r, stop, done); err != nil {
		lock.Close()
		return Running{}, nil, err
	}
	result := make(chan error, 1)
	finish := func(err error) {
		lock.Close()
		end()
		Prune()
		result <- err
		close(done)
	}
	os.MkdirAll(OutDir(), 0o700) // the logs' folder, which a ccs that never logged hasn't made
	f, err := create(r.Path)
	if err != nil {
		finish(err)
		return Running{}, nil, err
	}
	switch kind {
	case Heap:
		runtime.GC() // up to date, as go tool pprof's own heap endpoint does with gc=1
		err := pprof.Lookup("heap").WriteTo(f, 0)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		finish(err)
		return r, result, err
	case CPU:
		err = pprof.StartCPUProfile(f)
	case Trace:
		err = trace.Start(f)
	}
	if err != nil {
		f.Close()
		os.Remove(r.Path)
		finish(err)
		return Running{}, nil, err
	}
	go func() {
		t := time.NewTimer(time.Until(r.Until))
		defer t.Stop()
		select {
		case <-t.C:
		case <-stop:
		}
		if kind == CPU {
			pprof.StopCPUProfile()
		} else {
			trace.Stop()
		}
		finish(f.Close())
	}()
	return r, result, nil
}

// stopRemote stops a timed profile now, waiting for it to be written;
// nothing for ^T Q's, which its own stop ends.
func stopRemote() {
	mu.Lock()
	stop, done := curStop, curDone
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
}

// isProfile says a name in OutDir is one of ccs's profiles.
func isProfile(name string) bool {
	return (strings.HasPrefix(name, CPU+"-") || strings.HasPrefix(name, Heap+"-")) && strings.HasSuffix(name, ".pprof") ||
		strings.HasPrefix(name, Trace+"-") && strings.HasSuffix(name, ".trace")
}

// Prune keeps the newest Keep profiles in OutDir and removes the rest,
// leaving one being recorded alone.
func Prune() {
	dir := OutDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	busy, _ := Current()
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, e := range entries {
		if !e.Type().IsRegular() || !isProfile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	slices.SortFunc(files, func(a, b file) int { return b.mod.Compare(a.mod) })
	for i, f := range files {
		if i >= Keep && f.path != busy.Path {
			os.Remove(f.path)
		}
	}
}

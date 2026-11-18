package transcript

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// A big scan (GetConversations with most transcripts not parsed yet: every
// ccs mcp call, every CLI command, the list's first load) holds every
// transcript at once, about 620 MB with ~800 sessions. Several at once froze a 24 GB laptop, so every ccs process on
// the machine shares ScanSlots of them: each takes a flock on one of
// scan-N.lock beside the settings, waits up to ScanWait for one to free,
// and otherwise fails with ErrScanBusy rather than adding to the pile. The
// kernel drops a lock when its process exits, however it exits.
var (
	ScanSlots   = 2
	ScanWait    = 2 * time.Minute
	ScanLockDir = func() string { return filepath.Dir(state.MarksPath) }
	scanPoll    = 100 * time.Millisecond
)

// ErrScanBusy: every scan slot stayed taken for ScanWait.
var ErrScanBusy = errors.New("other ccs processes are reading every session")

// acquireScanSlot takes a free slot, waiting up to ScanWait; release frees it.
func acquireScanSlot() (release func(), err error) {
	dir := ScanLockDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}, nil // no folder for the locks: scan unlimited rather than not at all
	}
	deadline := time.Now().Add(ScanWait)
	for {
		for i := range ScanSlots {
			f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("scan-%d.lock", i)), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				return func() {}, nil
			}
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				return func() { f.Close() }, nil // closing the file drops the lock
			}
			f.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w (%d at once); try again in a moment", ErrScanBusy, ScanSlots)
		}
		time.Sleep(scanPoll)
	}
}

// held is this process's slot, kept from a big scan until ReleaseScanSlot:
// the parsed transcripts stay in memory after GetConversations returns
// (while a CLI command builds its answer, or an MCP call its reply), so the
// slot is held as long as they are.
var held struct {
	sync.Mutex
	release func()
}

// holdScanSlot takes a slot for a big scan of files, unless this process
// already holds one.
func holdScanSlot(files []string) error {
	held.Lock()
	defer held.Unlock()
	if held.release != nil || !bigScan(files) {
		return nil
	}
	release, err := acquireScanSlot()
	if err != nil {
		return err
	}
	held.release = release
	return nil
}

// ReleaseScanSlot gives this process's slot back: DropParseCache does it
// (ccs mcp after each call), and a long-running process that keeps its cache
// (the list, ccs serve) does it once its first load is done, so it never
// holds a slot for good. A CLI command keeps it until it exits.
func ReleaseScanSlot() {
	held.Lock()
	defer held.Unlock()
	if held.release != nil {
		held.release()
		held.release = nil
	}
}

// scanSlotMin is how many transcripts a scan must have to parse afresh
// before it takes a slot: a refresh of a warm cache, or a handful parsed
// earlier in the same command, doesn't hold everything at once.
var scanSlotMin = 50

// bigScan: more than scanSlotMin of files aren't parsed yet in this
// process, so this scan holds most of them at once.
func bigScan(files []string) bool {
	ParseCacheMu.Lock()
	defer ParseCacheMu.Unlock()
	n := 0
	for _, f := range files {
		if _, ok := ParseCache[f]; !ok {
			if n++; n > scanSlotMin {
				return true
			}
		}
	}
	return false
}

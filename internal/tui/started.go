package tui

import (
	"os"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// births caches each transcript's file birth (fileBirth) by path: startedAt
// asks for it on every sort, and a file's birth never changes.
var births sync.Map // path -> int64

// startedAt is when c started, in Unix nanoseconds, for sortList: its first
// message's timestamp, else its transcript's file birth (fileBirth), else
// its last message's (a transcript sets both or neither, so only a
// conversation with no file gets that far), else 0.
func startedAt(c transcript.Conversation) int64 {
	if t, err := time.Parse(time.RFC3339Nano, c.FirstTimestamp); err == nil {
		return t.UnixNano()
	}
	last := func() int64 {
		if t, err := time.Parse(time.RFC3339Nano, c.LastTimestamp); err == nil {
			return t.UnixNano()
		}
		return 0
	}
	if c.FilePath == "" {
		return last()
	}
	if v, ok := births.Load(c.FilePath); ok {
		return v.(int64)
	}
	info, err := os.Stat(c.FilePath)
	if err != nil {
		return last() // not cached: it may appear later
	}
	b := fileBirth(info).UnixNano()
	births.Store(c.FilePath, b)
	return b
}

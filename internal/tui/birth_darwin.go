package tui

import (
	"os"
	"syscall"
	"time"
)

// fileBirth is when the file was created (macOS records it), else its
// modification time.
func fileBirth(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Birthtimespec.Sec > 0 {
		return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec)
	}
	return info.ModTime()
}

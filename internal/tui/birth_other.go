//go:build !darwin

package tui

import (
	"os"
	"time"
)

// fileBirth is the file's modification time: os.Stat doesn't report a
// creation time here.
func fileBirth(info os.FileInfo) time.Time {
	return info.ModTime()
}

// Package state is where ccs keeps its own files (settings, session marks)
// and its logs (update, activity).
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Version is the running ccs release ("dev" in a local build), set by
// app.Main: --version, the update checks and every log line read it.
var Version = "dev"

// SettingsPath keeps the view ccs reopens in (^X T layout, ^X G grouping)
// across restarts.
var SettingsPath = func() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccs", "settings.json")
}()

// MarksPath keeps sessionMarks beside the settings.
var MarksPath = func() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccs", "sessions.json")
}()

// UpdateLogPath is where self-update steps are logged: each git/brew
// command with its full output, exit and duration, and each download with
// the address it connected to. Kept under 1MB (the previous file is .1).
var UpdateLogPath = func() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", "ccs", "update.log")
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "ccs", "update.log")
	}
	return filepath.Join(os.TempDir(), "ccs-update.log")
}()

var logMu sync.Mutex

func LogUpdate(format string, args ...any) { AppendLog(UpdateLogPath, format, args...) }

// ActivityLogPath is beside the update log: what ccs did on its own, such as
// auto-focus switching tabs (logActivity).
var ActivityLogPath = filepath.Join(filepath.Dir(UpdateLogPath), "activity.log")

func LogActivity(format string, args ...any) { AppendLog(ActivityLogPath, format, args...) }

// AppendLog adds a timestamped line to a log, capped at 1MB (the previous
// 1MB kept as .1).
func AppendLog(path, format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 1<<20 {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s ccs %s: %s\n", time.Now().Format(time.RFC3339), Version, fmt.Sprintf(format, args...))
}

func ErrOrOK(err error) any {
	if err == nil {
		return "ok"
	}
	return err
}

// Indent prefixes each line of command output for the log.
func Indent(out []byte) string {
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return ""
	}
	return "  | " + strings.ReplaceAll(text, "\n", "\n  | ")
}

// SessionName is what a launched session may be called: it goes into a
// shell command, a process's arguments and a folder name.
var SessionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LaunchDir holds each launched session's launcher and brief, in a folder of
// its own (<LaunchDir>/<name>), which a codex session may also write to.
// Not a temp dir: a launcher deleted before the tab runs it fails silently.
var LaunchDir = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccs", "launch")
}

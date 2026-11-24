//go:build darwin

package proc

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// sysListing is every process as `ps -axww -o pid=,ppid=,tty=,command=`
// lists it, read from the kernel in this process: kern.proc.all for pids,
// parents and terminals, then kern.procargs2 for each command line. With a
// thousand processes that is about 15ms, where ps takes a fifth of a second
// of CPU. Tests turn it off to use a fake ps.
var sysListing = func() ([]psRow, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	rows := make([]psRow, 0, len(procs))
	for i := range procs {
		p := &procs[i]
		pid := int(p.Proc.P_pid)
		if pid == 0 { // the kernel, which ps -ax leaves out
			continue
		}
		cmd := procArgs(pid)
		if p.Proc.P_stat == sZomb { // exited, not yet reaped
			cmd = "<defunct>"
		}
		if cmd == "" { // another user's, or gone meanwhile: ps shows its name
			cmd = unix.ByteSliceToString(p.Proc.P_comm[:])
		}
		rows = append(rows, psRow{
			pid:  strconv.Itoa(pid),
			ppid: strconv.Itoa(int(p.Eproc.Ppid)),
			tty:  ttyName(p.Eproc.Tdev),
			cmd:  cmd,
		})
	}
	return rows, nil
}

// sZomb is a process's state between its exit and its parent's wait.
const sZomb = 5

// procArgs is pid's command line, its arguments joined by spaces as ps
// prints them, or "" when it can't be read. kern.procargs2 is argc, the
// executable's path, NUL padding, then argc NUL-terminated arguments (then
// the environment, never read).
func procArgs(pid int) string {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) < 4 {
		return ""
	}
	return parseProcArgs(b)
}

// parseProcArgs reads a kern.procargs2 buffer; see procArgs.
func parseProcArgs(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	argc := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	end := strings.IndexByte(string(b), 0) // past the executable's path
	if end < 0 || argc <= 0 {
		return ""
	}
	b = b[end:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	args := make([]string, 0, argc)
	for len(args) < argc && len(b) > 0 {
		end := strings.IndexByte(string(b), 0)
		if end < 0 {
			args = append(args, string(b))
			break
		}
		args = append(args, string(b[:end]))
		b = b[end+1:]
	}
	return psVis(strings.Join(args, " "))
}

// psVis writes control characters as ps does, \ooo in octal (a newline in
// an argument is \012), so every process stays on one line of the listing;
// backslashes stay as they are.
func psVis(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			fmt.Fprintf(&b, "\\%03o", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ttys maps a terminal's device number to its name in /dev, as ps names it
// ("ttys003"), read again when a device isn't in it (a new tab).
var ttys struct {
	sync.Mutex
	names map[int32]string
}

// ttyName is dev's name, "??" for none, as ps prints it.
func ttyName(dev int32) string {
	if dev == -1 { // NODEV
		return "??"
	}
	ttys.Lock()
	defer ttys.Unlock()
	if name, ok := ttys.names[dev]; ok {
		return name
	}
	ttys.names = devNames()
	if name, ok := ttys.names[dev]; ok {
		return name
	}
	return "??"
}

// devNames is every terminal in /dev by device number.
func devNames() map[int32]string {
	names := map[int32]string{}
	entries, _ := os.ReadDir("/dev")
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "tty") && n != "console" {
			continue
		}
		var st unix.Stat_t
		if unix.Stat(filepath.Join("/dev", n), &st) == nil {
			names[st.Rdev] = n
		}
	}
	return names
}

// sysStartTimes is each pid's start time from kern.proc.pid, in this
// process: no ps started. To the second, as ps's lstart prints it. Pids it
// can't read (gone, or not allowed) are left out.
var sysStartTimes = func(pids []int) (map[int]time.Time, error) {
	out := make(map[int]time.Time, len(pids))
	for _, pid := range pids {
		k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil || k == nil || int(k.Proc.P_pid) != pid {
			continue
		}
		out[pid] = time.Unix(k.Proc.P_starttime.Sec, 0).UTC()
	}
	return out, nil
}

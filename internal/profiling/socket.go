package profiling

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
)

// The roles a ccs process serves profiles as.
const (
	RoleTUI   = "tui"
	RoleWatch = "watch"
	RoleMCP   = "mcp"
	RoleServe = "serve"
)

// request is all a client can say: no path, nothing else.
type request struct {
	Op      string `json:"op"` // info or start
	Kind    string `json:"kind,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}

// Target is a ccs process that takes profiles, as it describes itself.
type Target struct {
	PID     int       `json:"pid"`
	Role    string    `json:"role"`
	Version string    `json:"version"`
	Since   time.Time `json:"since"`
	Running *Running  `json:"running,omitempty"`
}

type reply struct {
	Target  *Target  `json:"target,omitempty"`
	Started *Running `json:"started,omitempty"`
	Done    bool     `json:"done,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// SocketPath is pid's socket.
func SocketPath(pid int) string { return filepath.Join(SockDir(), strconv.Itoa(pid)+".sock") }

// prepareDir makes the socket folder, 0700, and refuses one that isn't
// this user's private folder.
func prepareDir() error {
	dir := SockDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return proc.TrustedDir(dir)
}

// pidAlive is proc.PIDAlive; stubbed in tests.
var pidAlive = proc.PIDAlive

// sweepStale removes sockets of processes that have gone.
func sweepStale() {
	entries, _ := os.ReadDir(SockDir())
	for _, e := range entries {
		if pid, ok := sockPID(e.Name()); ok && pid != os.Getpid() && !pidAlive(pid) {
			os.Remove(filepath.Join(SockDir(), e.Name()))
		}
	}
}

func sockPID(name string) (int, bool) {
	n, ok := strings.CutSuffix(name, ".sock")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(n)
	return pid, err == nil && pid > 0
}

// Serve listens for profile requests to this process, as role, until the
// returned stop, which also stops a profile it's recording. A process that
// can't listen (another user's folder, say) just takes none.
func Serve(role string) (func(), error) {
	if err := prepareDir(); err != nil {
		return func() {}, err
	}
	sweepStale()
	path := SocketPath(os.Getpid())
	os.Remove(path) // a recycled pid's
	ln, err := net.Listen("unix", path)
	if err != nil {
		return func() {}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return func() {}, err
	}
	self := Target{PID: os.Getpid(), Role: role, Version: state.Version, Since: time.Now()}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				handle(c, self)
			}()
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			ln.Close()
			os.Remove(path)
			// A request accepted just before the close may start a profile
			// after the first stop, so stop again until every handler is done.
			idle := make(chan struct{})
			go func() { wg.Wait(); close(idle) }()
			for give := time.Now().Add(10 * time.Second); time.Now().Before(give); {
				stopRemote()
				select {
				case <-idle:
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		})
	}, nil
}

// handle answers one request on c, from this user only.
func handle(c net.Conn, self Target) {
	defer c.Close()
	if !proc.SameUser(c) {
		return
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, 4096)).ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return
	}
	var req request
	if json.Unmarshal(line, &req) != nil {
		write(c, reply{Error: "bad request"})
		return
	}
	switch req.Op {
	case "info":
		t := self
		if r, ok := Current(); ok {
			t.Running = &r
		}
		write(c, reply{Target: &t})
	case "start":
		r, done, err := start(req.Kind, req.Seconds, self.Role)
		if err != nil {
			write(c, reply{Error: err.Error()})
			state.LogActivity("profile: refused a %s profile of this ccs %s: %v", req.Kind, self.Role, err)
			return
		}
		state.LogActivity("profile: recording a %s profile of this ccs %s to %s", r.Kind, self.Role, r.Path)
		if write(c, reply{Started: &r}) != nil {
			c.Close() // the client's gone; the profile still stops by itself
		}
		err = <-done
		if err != nil {
			state.LogActivity("profile: %s failed: %v", r.Path, err)
		} else {
			state.LogActivity("profile: wrote %s", r.Path)
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		rep := reply{Done: true}
		if err != nil {
			rep.Error = err.Error()
		}
		write(c, rep)
	default:
		write(c, reply{Error: "unknown op"})
	}
}

func write(c net.Conn, r reply) error {
	b, _ := json.Marshal(r)
	_, err := c.Write(append(b, '\n'))
	return err
}

// dial connects to pid's socket once it's this user's private socket and
// the listener runs as this user.
func dial(pid int) (net.Conn, error) {
	path := SocketPath(pid)
	if _, err := os.Lstat(path); err != nil {
		return nil, fmt.Errorf("pid %d isn't a ccs that takes profiles (one from before ccs profile doesn't)", pid)
	}
	if err := proc.TrustedSocket(path); err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("pid %d isn't listening: %v", pid, err)
	}
	if !proc.SameUser(c) {
		c.Close()
		return nil, fmt.Errorf("pid %d's socket isn't this user's", pid)
	}
	return c, nil
}

// ask sends req to pid and returns the connection and the first reply.
func ask(pid int, req request) (net.Conn, *bufio.Reader, reply, error) {
	c, err := dial(pid)
	if err != nil {
		return nil, nil, reply{}, err
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		c.Close()
		return nil, nil, reply{}, err
	}
	br := bufio.NewReader(c)
	rep, err := readReply(br)
	if err != nil {
		c.Close()
		return nil, nil, reply{}, err
	}
	if rep.Error != "" {
		c.Close()
		return nil, nil, rep, errors.New(rep.Error)
	}
	return c, br, rep, nil
}

func readReply(br *bufio.Reader) (reply, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return reply{}, err
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return reply{}, err
	}
	return r, nil
}

// Info asks pid to describe itself.
func Info(pid int) (Target, error) {
	c, _, rep, err := ask(pid, request{Op: "info"})
	if err != nil {
		return Target{}, err
	}
	c.Close()
	if rep.Target == nil {
		return Target{}, errors.New("no answer")
	}
	return *rep.Target, nil
}

// Targets are the running ccs processes that take profiles, by pid.
func Targets() []Target {
	sweepStale()
	entries, _ := os.ReadDir(SockDir())
	var out []Target
	for _, e := range entries {
		pid, ok := sockPID(e.Name())
		if !ok {
			continue
		}
		if t, err := Info(pid); err == nil {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Target) int { return a.PID - b.PID })
	return out
}

// Pick is the process to profile: pid when given, else the one running
// ccs of role (tui by default). Several of a role need --pid.
func Pick(pid int, role string) (Target, error) {
	if pid > 0 {
		return Info(pid)
	}
	if role == "" {
		role = RoleTUI
	}
	var found []Target
	for _, t := range Targets() {
		if t.Role == role {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 0:
		return Target{}, fmt.Errorf("no running ccs %s takes profiles (one from before ccs profile doesn't)", role)
	case 1:
		return found[0], nil
	}
	var pids []string
	for _, t := range found {
		pids = append(pids, strconv.Itoa(t.PID))
	}
	return Target{}, fmt.Errorf("%d ccs %s processes run (pids %s): pass one's pid", len(found), role, strings.Join(pids, ", "))
}

// Started is a profile recording in another process.
type Started struct {
	Running
	conn net.Conn
	br   *bufio.Reader
}

// Start asks pid for a profile. Wait for it, or Close to leave it: it
// stops by itself either way.
func Start(pid int, kind string, seconds int) (*Started, error) {
	kind, seconds, err := CheckRequest(kind, seconds)
	if err != nil {
		return nil, err
	}
	c, br, rep, err := ask(pid, request{Op: "start", Kind: kind, Seconds: seconds})
	if err != nil {
		return nil, err
	}
	if rep.Started == nil {
		c.Close()
		return nil, errors.New("no answer")
	}
	return &Started{Running: *rep.Started, conn: c, br: br}, nil
}

// Wait waits for the profile to be written.
func (s *Started) Wait() error {
	defer s.conn.Close()
	wait := 15 * time.Second
	if !s.Until.IsZero() {
		wait += time.Until(s.Until)
	}
	s.conn.SetDeadline(time.Now().Add(wait))
	rep, err := readReply(s.br)
	if err != nil {
		return fmt.Errorf("no word that %s was written: %v", s.Path, err)
	}
	if rep.Error != "" {
		return errors.New(rep.Error)
	}
	return nil
}

// Close leaves the profile to finish on its own.
func (s *Started) Close() { s.conn.Close() }

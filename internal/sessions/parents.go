package sessions

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ParentRec is who launched (or last adopted) a named session: the session
// that ran launch_session or ccs launch for it, by name and session id. A
// session that starts others is an orchestrator, and the list draws it as a
// tree of what it started.
type ParentRec struct {
	Child      string    `json:"child"`              // the launched session's name
	ChildID    string    `json:"child_id,omitempty"` // its session id, once one registered
	Parent     string    `json:"parent"`             // the launching session's name
	ParentID   string    `json:"parent_id"`          // and its session id
	ParentKind string    `json:"parent_kind,omitempty"`
	At         time.Time `json:"at"`
}

// ParentsFile is launch-parents.json: one record per child name, the latest
// launch or adoption winning.
type ParentsFile struct {
	Parents map[string]ParentRec `json:"parents"`
}

// ParentsPath is launch-parents.json beside the session marks.
var ParentsPath = func() string { return filepath.Join(filepath.Dir(state.MarksPath), "launch-parents.json") }

// LoadParents reads the records without the lock (a reader sees the old or
// the new file, never half of one).
func LoadParents() ParentsFile {
	var f ParentsFile
	if data, err := os.ReadFile(ParentsPath()); err == nil {
		json.Unmarshal(data, &f)
	}
	if f.Parents == nil {
		f.Parents = map[string]ParentRec{}
	}
	return f
}

// WithParents changes the records under a flock, written through a unique
// temp file; f returns whether to save.
func WithParents(f func(p *ParentsFile) bool) error {
	path := ParentsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	p := LoadParents()
	if !f(&p) {
		return nil
	}
	data, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "launch-parents-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Caller is the session a ccs command or MCP call runs under.
type Caller struct {
	ID, Name, Kind string
}

// CallingSession is the session this process runs under: the Codex thread
// in CODEX_THREAD_ID, else the first ancestor that is a live Claude Code
// session's process (the MCP server is its session's child; ccs launch run
// from Bash a grandchild). Stubbed in tests.
var CallingSession = func() (Caller, bool) {
	if t := os.Getenv("CODEX_THREAD_ID"); transcript.CodexThreadID.MatchString(t) {
		name := CodexThreadName(t)
		if m, ok := transcript.ReadCodexMeta(transcript.CodexRollout(t)); ok {
			if n := transcript.CodexLaunchName(m); n != "" {
				name = n
			}
		}
		return Caller{ID: t, Name: name, Kind: transcript.KindCodex}, true
	}
	byPID := map[int]Caller{}
	for id, s := range ScanSessions(nil) {
		if !s.Codex {
			byPID[s.PID] = Caller{ID: id, Name: s.Stat.Name}
		}
	}
	tree, err := proc.PsTree()
	if err != nil {
		return Caller{}, false
	}
	ppid := map[int]int{}
	for _, line := range strings.Split(tree, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pp, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			ppid[pid] = pp
		}
	}
	pid := os.Getppid()
	for range 30 {
		if pid <= 1 {
			break
		}
		if c, ok := byPID[pid]; ok {
			return c, true
		}
		pid = ppid[pid]
	}
	return Caller{}, false
}

// recordLaunchParent notes that the calling session launched name (childID
// "" while it hasn't registered). A launch from outside any session (^X N,
// a shell) records nothing and leaves an earlier record as it was.
func recordLaunchParent(name string, childPID int) {
	me, ok := CallingSession()
	if !ok || me.Name == name {
		return
	}
	childID := ""
	if childPID > 0 {
		for id, s := range ScanSessions(nil) {
			if s.PID == childPID {
				childID = id
				break
			}
		}
	}
	if err := setParent(name, childID, me); err != nil {
		state.LogActivity("launch %s: couldn't record its parent %s: %v", name, me.Name, err)
	}
}

func setParent(name, childID string, p Caller) error {
	return WithParents(func(f *ParentsFile) bool {
		f.Parents[name] = ParentRec{Child: name, ChildID: childID, Parent: p.Name, ParentID: p.ID, ParentKind: p.Kind, At: time.Now().UTC()}
		return true
	})
}

// Adopt makes parent ("" for the calling session) the parent of the session
// named child, as if it had launched it; to "none" drops its record, so it
// goes back to the list's ordinary sections. The parent must be running
// (it's the one that will look after it); the child needn't be.
func Adopt(child, parent string) (string, error) {
	if !state.SessionName.MatchString(child) {
		return "", fmt.Errorf("name %q: use letters, digits, '.', '_' and '-' (at most 64)", child)
	}
	if parent == "none" {
		had := false
		err := WithParents(func(f *ParentsFile) bool {
			_, had = f.Parents[child]
			delete(f.Parents, child)
			return had
		})
		if err != nil {
			return "", err
		}
		if !had {
			return child + " had no parent", nil
		}
		return child + " has no parent now", nil
	}
	var p Caller
	live := ScanSessions(nil)
	if parent == "" {
		var ok bool
		if p, ok = CallingSession(); !ok {
			return "", errors.New("not run from a Claude Code or Codex session: name the parent with --to")
		}
	} else {
		var found []Caller
		for id, s := range live {
			if !s.Codex && s.Stat.Name == parent {
				found = append(found, Caller{ID: id, Name: parent})
			}
		}
		switch len(found) {
		case 0:
			return "", fmt.Errorf("no running session named %s", parent)
		case 1:
			p = found[0]
		default:
			return "", fmt.Errorf("%d running sessions are named %s", len(found), parent)
		}
	}
	if p.Name == child {
		return "", fmt.Errorf("%s can't be its own parent", child)
	}
	if ancestorOf(child, p.Name) {
		return "", fmt.Errorf("%s is under %s already: adopting it would make a loop", p.Name, child)
	}
	childID := ""
	for id, s := range live {
		if s.Stat.Name == child || (s.Codex && CodexThreadName(id) == child) {
			childID = id
		}
	}
	if err := setParent(child, childID, p); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s is under %s now", child, p.Name), nil
}

// ancestorOf: name is among the parents above who, by the records' names.
func ancestorOf(name, who string) bool {
	f := LoadParents()
	for range len(f.Parents) + 1 {
		r, ok := f.Parents[who]
		if !ok {
			return false
		}
		if r.Parent == name {
			return true
		}
		who = r.Parent
	}
	return false
}

// RunAdopt is `ccs adopt <name> [--to <parent>|none]`.
func RunAdopt(args []string, stdout, stderr io.Writer) int {
	usage := "usage: ccs adopt <name> [--to <parent> | --to none]\n  makes the calling session (or --to's) the parent of the session named <name>; --to none detaches it"
	var name, to string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--to" && i+1 < len(args):
			to = args[i+1]
			i++
		case strings.HasPrefix(a, "--to="):
			to = strings.TrimPrefix(a, "--to=")
		case strings.HasPrefix(a, "-") || name != "":
			fmt.Fprintln(stderr, usage)
			return 2
		default:
			name = a
		}
	}
	if name == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	msg, err := Adopt(name, to)
	if err != nil {
		fmt.Fprintln(stderr, "ccs adopt:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// ResolveParents is each session's recorded parent among convs, by session
// id: a record's child is the newest session with its name (a /clear makes
// a new id under the same name), else the one with its id; its parent
// likewise by name, else id. Records whose ends aren't both listed, and
// any that would make a loop, are left out.
func ResolveParents(convs []transcript.Conversation, f ParentsFile) map[string]string {
	if len(f.Parents) == 0 {
		return nil
	}
	newest := map[string]transcript.Conversation{}
	ids := map[string]bool{}
	for _, c := range convs {
		ids[c.SessionID] = true
		if c.Title == "" {
			continue
		}
		if n, ok := newest[c.Title]; !ok || c.LastTimestamp > n.LastTimestamp {
			newest[c.Title] = c
		}
	}
	find := func(name, id string) string {
		if c, ok := newest[name]; ok {
			return c.SessionID
		}
		if id != "" && ids[id] {
			return id
		}
		return ""
	}
	out := map[string]string{}
	for _, r := range f.Parents {
		child, parent := find(r.Child, r.ChildID), find(r.Parent, r.ParentID)
		if child == "" || parent == "" || child == parent {
			continue
		}
		out[child] = parent
	}
	for child := range out { // break any loop the records make
		seen := map[string]bool{child: true}
		for p := out[child]; p != ""; p = out[p] {
			if seen[p] {
				delete(out, child)
				break
			}
			seen[p] = true
		}
	}
	return out
}

// ParentNames is each session's recorded parent's name, by the child's
// session id, for list_sessions and session_triage.
func ParentNames(convs []transcript.Conversation) map[string]string {
	f := LoadParents()
	ids := ResolveParents(convs, f)
	title := map[string]string{}
	for _, c := range convs {
		title[c.SessionID] = cmp.Or(c.Title, c.SessionID)
	}
	out := map[string]string{}
	for child, parent := range ids {
		out[child] = title[parent]
	}
	return out
}

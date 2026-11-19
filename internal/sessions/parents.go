package sessions

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	// Source is what set it: "" a launch or adopt, "reorg" ccs reorg,
	// "start" the SessionStart hook, "brief" a handoff brief's first line
	// (RecordBriefParents).
	Source string `json:"source,omitempty"`
}

// ParentsFile is launch-parents.json: one record per child name, the latest
// launch or adoption winning.
type ParentsFile struct {
	Parents map[string]ParentRec `json:"parents"`
	// Detached is each child `ccs adopt <name> --to none` detached, by the
	// same key as Parents, so a brief's "Parent session:" line never puts it
	// back; any later record for it (a launch, an adoption) clears it.
	Detached map[string]time.Time `json:"detached,omitempty"`
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
	if f.Detached == nil {
		f.Detached = map[string]time.Time{}
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

// parentBackupKeep is how many reorg backups of launch-parents.json are kept.
const parentBackupKeep = 20

// ReorgBackup is launch-parents.json as it was just before one ccs reorg
// wrote its records, all stamped At: what --undo puts back.
type ReorgBackup struct {
	At      time.Time            `json:"at"`
	Parents map[string]ParentRec `json:"parents"`
}

// ParentBackups is the reorg backups not yet undone, newest first.
func ParentBackups() []string {
	found, _ := filepath.Glob(filepath.Join(filepath.Dir(ParentsPath()), "launch-parents-reorg-*.json.bak"))
	slices.Sort(found) // timestamped names sort by time
	slices.Reverse(found)
	return found
}

// BackupParents saves the records as they are now, before a reorg writes
// its own stamped at, keeping the newest parentBackupKeep. It returns the
// backup's path and the records it saved.
func BackupParents(at time.Time) (string, map[string]ParentRec, error) {
	path := filepath.Join(filepath.Dir(ParentsPath()), "launch-parents-reorg-"+at.UTC().Format("20060102T150405.000000000Z")+".json.bak")
	var saved map[string]ParentRec
	var werr error
	err := WithParents(func(f *ParentsFile) bool {
		saved = maps.Clone(f.Parents)
		data, err := json.MarshalIndent(ReorgBackup{At: at.UTC(), Parents: f.Parents}, "", " ")
		if err == nil {
			err = os.WriteFile(path, data, 0o600)
		}
		werr = err
		return false
	})
	if err = cmp.Or(err, werr); err != nil {
		return "", nil, err
	}
	if all := ParentBackups(); len(all) > parentBackupKeep {
		for _, old := range all[parentBackupKeep:] {
			os.Remove(old)
		}
	}
	return path, saved, nil
}

// ReadReorgBackup reads one backup.
func ReadReorgBackup(path string) (ReorgBackup, error) {
	var b ReorgBackup
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &b)
	}
	if err != nil {
		return b, fmt.Errorf("%s: %w", path, err)
	}
	return b, nil
}

// reorgWrote: r is a record the reorg of backup b wrote, still unchanged.
func reorgWrote(r ParentRec, b ReorgBackup) bool { return r.Source == "reorg" && r.At.Equal(b.At) }

// UndoCount is how many records undoing backup b would put back: those its
// reorg wrote that nothing has changed since.
func UndoCount(b ReorgBackup) int {
	n := 0
	for _, r := range LoadParents().Parents {
		if reorgWrote(r, b) {
			n++
		}
	}
	return n
}

// UndoReorg puts back what the reorg of the backup at path changed: each
// record it wrote that nothing has changed since goes back to its value in
// the backup, or is dropped if it had none. Anything written since (a
// launch, an adopt, another reorg) stays. The backup is then marked undone,
// so the next undo reaches the reorg before it. Nothing is backed up.
func UndoReorg(path string) (int, error) {
	b, err := ReadReorgBackup(path)
	if err != nil {
		return 0, err
	}
	n := 0
	err = WithParents(func(f *ParentsFile) bool {
		for key, r := range f.Parents {
			if !reorgWrote(r, b) {
				continue
			}
			if old, ok := b.Parents[key]; ok {
				f.Parents[key] = old
			} else {
				delete(f.Parents, key)
			}
			n++
		}
		return n > 0
	})
	if err != nil {
		return 0, err
	}
	return n, os.Rename(path, strings.TrimSuffix(path, ".bak")+".undone")
}

// RestoreParentRecs puts keys back as they were in saved (dropping those it
// didn't have): how a reorg takes back the records of an orchestrator whose
// launch failed.
func RestoreParentRecs(saved map[string]ParentRec, keys []string) error {
	return WithParents(func(f *ParentsFile) bool {
		for _, k := range keys {
			if old, ok := saved[k]; ok {
				f.Parents[k] = old
			} else {
				delete(f.Parents, k)
			}
		}
		return len(keys) > 0
	})
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
	return SetParents([]ParentRec{{Child: name, ChildID: childID, Parent: p.Name, ParentID: p.ID, ParentKind: p.Kind}})
}

// SetParents writes recs under one lock, each keyed by its child's name, or
// its id when it has no name, stamped now.
func SetParents(recs []ParentRec) error { return SetParentsAt(recs, time.Now()) }

// SetParentsAt is SetParents stamped at: a reorg stamps its records with its
// backup's time, so --undo finds exactly them.
func SetParentsAt(recs []ParentRec, at time.Time) error {
	at = at.UTC()
	return WithParents(func(f *ParentsFile) bool {
		for _, r := range recs {
			r.At = at
			key := cmp.Or(r.Child, r.ChildID)
			f.Parents[key] = r
			delete(f.Detached, key) // an explicit record ends a detach
		}
		return len(recs) > 0
	})
}

// briefPrefix starts a session-handoff brief, whose first line names the
// session that handed it off: "Parent session: `<name>`. Report back …".
const briefPrefix = "Parent session: `"

// BriefParent is the parent a handoff brief names, when text (a session's
// first user message) starts with exactly "Parent session: `<name>`.".
func BriefParent(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, briefPrefix)
	if !ok {
		return "", false
	}
	name, tail, ok := strings.Cut(rest, "`")
	if !ok || !strings.HasPrefix(tail, ".") || !state.SessionName.MatchString(name) {
		return "", false
	}
	return name, true
}

// firstUserText is a conversation's first user message, "" when it has none.
func firstUserText(c transcript.Conversation) string {
	for _, msg := range c.Messages {
		if msg.Role == "user" {
			return msg.Text
		}
	}
	return ""
}

// briefRecs is a record for each of convs that has no record and wasn't
// detached, whose first user message is a handoff brief naming a session
// among convs (the newest with that name) other than itself. A session
// started by session-handoff's open-tab.sh, which ccs didn't launch, gets
// its parent this way even without the SessionStart hook.
func briefRecs(convs []transcript.Conversation, f ParentsFile) []ParentRec {
	newest := map[string]transcript.Conversation{}
	for _, c := range convs {
		if c.Title == "" {
			continue
		}
		if n, ok := newest[c.Title]; !ok || c.LastTimestamp > n.LastTimestamp {
			newest[c.Title] = c
		}
	}
	var recs []ParentRec
	for _, c := range convs {
		child := ""
		if c.IsCustomTitle {
			child = c.Title // records are keyed by the name a launch gives
		}
		key := cmp.Or(child, c.SessionID)
		if _, ok := f.Parents[key]; ok {
			continue
		}
		if _, ok := f.Parents[c.SessionID]; ok {
			continue
		}
		if _, ok := f.Detached[key]; ok {
			continue
		}
		name, ok := BriefParent(firstUserText(c))
		if !ok || name == child {
			continue
		}
		p, ok := newest[name]
		if !ok || p.SessionID == c.SessionID {
			continue
		}
		recs = append(recs, ParentRec{Child: child, ChildID: c.SessionID, Parent: name, ParentID: p.SessionID, ParentKind: p.Kind, Source: "brief"})
	}
	return recs
}

// RecordBriefParents records briefRecs for convs, once each: worked out
// under the records' lock, so a launch or adoption written meanwhile wins.
// It returns the records it added.
func RecordBriefParents(convs []transcript.Conversation) ([]ParentRec, error) {
	var added []ParentRec
	err := WithParents(func(f *ParentsFile) bool {
		added = briefRecs(convs, *f)
		at := time.Now().UTC()
		for i := range added {
			added[i].At = at
			f.Parents[cmp.Or(added[i].Child, added[i].ChildID)] = added[i]
		}
		return len(added) > 0
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// DropParent removes key's record if it is still rec (a reorg's planning
// run whose plan failed).
func DropParent(key string, rec ParentRec) {
	WithParents(func(f *ParentsFile) bool {
		if r, ok := f.Parents[key]; ok && r.Source == rec.Source && r.Parent == rec.Parent {
			delete(f.Parents, key)
			return true
		}
		return false
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
			f.Detached[child] = time.Now().UTC() // a brief's first line mustn't put it back
			return true
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
// likewise by name, else id. Older sessions with that name follow the
// newest. Records whose ends aren't both listed, and any that would make a
// loop, are left out.
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
	// The older sessions with a record's name (before a /clear, or the name
	// used again) go where the newest does, unless a record of their own
	// says otherwise: records are kept one per name, so they'd have none.
	for _, c := range convs {
		if _, ok := out[c.SessionID]; ok || c.Title == "" {
			continue
		}
		n := newest[c.Title]
		if p := out[n.SessionID]; p != "" && p != c.SessionID && n.SessionID != c.SessionID {
			out[c.SessionID] = p
		}
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

// ParentNamesLoaded is ParentNames for a load of only some sessions (the
// running ones): a child whose parent isn't among convs still gets the name
// its record gives, so a running session under a closed orchestrator keeps
// its parent in session_triage and list_sessions.
func ParentNamesLoaded(convs []transcript.Conversation) map[string]string {
	out := ParentNames(convs)
	byName, ids := map[string]string{}, map[string]bool{}
	for _, c := range convs {
		ids[c.SessionID] = true
		if c.Title != "" {
			if _, ok := byName[c.Title]; !ok {
				byName[c.Title] = c.SessionID
			}
		}
	}
	for _, r := range LoadParents().Parents {
		child := byName[r.Child]
		if child == "" && ids[r.ChildID] {
			child = r.ChildID
		}
		if child != "" && out[child] == "" && r.Parent != "" {
			out[child] = r.Parent
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

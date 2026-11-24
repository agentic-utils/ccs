package sessions

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// A handover close: a graceful close whose notice asks the session to hand
// its leftovers to its parent first, after which what it owned (its PR
// claims, its child sessions) moves to the parent, and the parent is told.

// HandoverParent is the running Claude Code session a handover from child
// goes to: its recorded parent (launch, adoption, a brief), by session id
// when the record has one, else by name.
type HandoverParent struct {
	Name, ID string
	PID      int
	File     SessionFile
}

// MoveClaims moves the live PR claims of the session with id from to the
// running session called to, returning the PRs moved. The claims registry
// lives in the github package, which sets this (it imports this one); nil
// leaves claims as they are.
var MoveClaims func(from, to string) ([]string, error)

// parentOf finds child's running parent, or says why there is none to hand
// over to. Nothing is sent or stopped either way.
func parentOf(child string) (HandoverParent, error) {
	none := fmt.Errorf("%s has no running parent: use ^X K to close it, or adopt it first", child)
	rec, ok := LoadParents().Parents[child]
	if !ok || rec.Parent == "" {
		return HandoverParent{}, none
	}
	if rec.ParentKind == "codex" {
		return HandoverParent{}, fmt.Errorf("%s's parent %s is a Codex session, which can't be sent a handover: use ^X K to close it, or adopt it to a Claude Code session first", child, rec.Parent)
	}
	var pids []int
	for id, s := range ScanSessions(nil) {
		if s.Codex {
			continue
		}
		if rec.ParentID != "" && id == rec.ParentID || rec.ParentID == "" && s.Stat.Name == rec.Parent {
			pids = append(pids, s.PID)
		}
	}
	if len(pids) != 1 {
		if len(pids) > 1 {
			return HandoverParent{}, fmt.Errorf("%d running sessions are %s's parent %s: close it by hand", len(pids), child, rec.Parent)
		}
		return HandoverParent{}, fmt.Errorf("%s's parent %s isn't running: use ^X K to close it, or adopt it first", child, rec.Parent)
	}
	f, err := ReadSessionFile(pids[0])
	if err != nil || f.Socket == "" {
		return HandoverParent{}, fmt.Errorf("%s's parent %s has no message socket, so it can't be sent a handover: use ^X K to close it", child, rec.Parent)
	}
	if f.Name == child {
		return HandoverParent{}, fmt.Errorf("%s can't hand over to itself", child)
	}
	return HandoverParent{Name: f.Name, ID: f.SessionID, PID: pids[0], File: f}, nil
}

// HandoverParentName is the running parent a handover from child would go
// to, or why there is none.
func HandoverParentName(child string) (string, error) {
	p, err := parentOf(child)
	return p.Name, err
}

// handoverNoticeText is what the session is asked: the session-handoff
// skill's handback, sent to the parent, then a line in its own
// conversation. ref marks it in the transcript.
func handoverNoticeText(timeout time.Duration, ref string, p HandoverParent) string {
	return fmt.Sprintf("This session is about to be closed and its leftovers handed to its parent, %[2]s (notice %[1]s). Stop at a safe point now and start nothing new. "+
		"Then send %[2]s your handover with SendMessage (to: \"%[2]s\", its address uds:%[3]s): "+
		"1. what you finished, with PR URLs; "+
		"2. every piece of leftover work: open PRs and their state, review threads, follow-ups to schedule; "+
		"3. everything you created that should go once you're closed: worktrees and their branches, background processes you started with & or nohup (each with its pid and command), and temp files and directories. "+
		"%[2]s takes all of it over and cleans it up, so leave nothing only you know about; don't remove worktrees or stop processes that hold leftover work, list them instead. "+
		"Then say, in one line in this conversation, that you've handed over to %[2]s. "+
		"The session is stopped as soon as you go idle after this, or in %[4]s at the latest.", ref, p.Name, p.File.Socket, timeout)
}

// handoverSeen: the transcript at path holds, after offset, a SendMessage
// to the parent (by name or by its socket).
func handoverSeen(path string, offset int64, p HandoverParent) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	byName := []byte(`"to":"` + p.Name + `"`)
	bySock := []byte(p.File.Socket)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.Contains(line, []byte(`"name":"SendMessage"`)) && (bytes.Contains(line, byName) || len(bySock) > 0 && bytes.Contains(line, bySock)) {
			return true
		}
	}
	return false
}

// moveChildren re-parents child's children (by name, or by its session id)
// to p (of kind, a Caller.Kind), recorded as source, so closing it orphans
// nobody. Returns their names.
func moveChildren(child, childID string, p HandoverParent, kind, source string) []string {
	var moved []string
	err := WithParents(func(f *ParentsFile) bool {
		for k, r := range f.Parents {
			isChild := r.Parent == child || childID != "" && r.ParentID == childID
			if k == child || !isChild {
				continue
			}
			r.Parent, r.ParentID, r.ParentKind, r.Source, r.At = p.Name, p.ID, kind, source, time.Now().UTC()
			f.Parents[k] = r
			moved = append(moved, cmp.Or(r.Child, k))
		}
		return len(moved) > 0
	})
	if err != nil {
		state.LogActivity("close %s: couldn't move its children to %s: %v", child, p.Name, err)
		return nil
	}
	slices.Sort(moved)
	return moved
}

// handoverParentNote is the automatic notice the parent gets once the child
// is closed, so it learns of the handover even when the child didn't send it.
func handoverParentNote(child string, g graceful, seen bool, said string, claims, children []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s was closed after being asked to hand its leftovers over to you (notice %s): %s.\n", child, g.ref, g.how)
	if seen {
		b.WriteString("Its handover message to you: seen in its transcript.\n")
	} else {
		b.WriteString("Its handover message to you: not seen in its transcript.\n")
		if said != "" {
			b.WriteString("Its last message:\n" + said + "\n")
		}
	}
	list := func(xs []string) string {
		if len(xs) == 0 {
			return "none"
		}
		return strings.Join(xs, ", ")
	}
	fmt.Fprintf(&b, "Moved to you: PR claims: %s; child sessions: %s.", list(claims), list(children))
	return b.String()
}

// CloseHandover asks the one running Claude Code session called name to hand
// its leftovers to its running parent, closes it once it has gone idle after
// that (or timeout passes) as CloseGracefully does, moves its PR claims and
// child sessions to the parent and tells the parent. With no running parent
// nothing is sent or stopped. onSent is as for CloseGracefullyNotify.
func CloseHandover(name, id string, timeout time.Duration, onSent func(pid int)) (string, error) {
	if timeout <= 0 {
		timeout = GracefulTimeout
	}
	p, err := parentOf(name)
	if err != nil {
		return "", err
	}
	msg, g, err := closeGraceful(name, id, timeout, onSent, func(ref string) string { return handoverNoticeText(timeout, ref, p) })
	if err != nil {
		return "", err
	}
	seen := g.path != "" && handoverSeen(g.path, g.offset, p)
	var claims []string
	if MoveClaims != nil && g.sessionID != "" {
		if claims, err = MoveClaims(g.sessionID, p.Name); err != nil {
			state.LogActivity("close %s: couldn't move its PR claims to %s: %v", name, p.Name, err)
		}
	}
	children := moveChildren(name, g.sessionID, p, "", "handover")
	state.LogActivity("close %s: handed over to %s (handover message seen: %t); moved claims %v and children %v", name, p.Name, seen, claims, children)
	said := ""
	if !seen && g.path != "" {
		said = lastSaidBy(g.path)
	}
	tell := handoverParentNote(name, g, seen, said, claims, children)
	switch f, err := ReadSessionFile(p.PID); {
	case err != nil || f.SessionID != p.ID:
		state.LogActivity("close %s: %s isn't running as it was, so it wasn't told", name, p.Name)
		msg += fmt.Sprintf("\n%s isn't running as it was, so it wasn't told", p.Name)
	default:
		if err := SendCloseNotice(p.PID, f, tell); err != nil {
			state.LogActivity("close %s: couldn't tell %s: %v", name, p.Name, err)
			msg += fmt.Sprintf("\ncouldn't tell %s: %v", p.Name, err)
		}
	}
	seenText := "not seen"
	if seen {
		seenText = "seen"
	}
	return fmt.Sprintf("%s\nhanded over to %s (its handover message: %s); moved to %s: %d PR claim(s), %d child session(s)", msg, p.Name, seenText, p.Name, len(claims), len(children)), nil
}

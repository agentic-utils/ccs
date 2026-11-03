package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Nudges: after each pass, ccs watch tells the session that claimed a PR
// (ccs claim) what's wrong with it: unresolved review threads (when their
// count goes up), failing checks, or conflicts with its base. Only live
// owners are told, each once per PR, head and reason, over the session's own
// message socket, in an envelope that says it's an automatic notice from
// ccs watch, never typed by the user. A PR nobody owns gets nothing beyond
// the watcher's usual events.

// nudgeSender is the from-name nudges carry: not ccsSender, which says the
// user typed it.
const (
	nudgeSender = "ccs watch"
	nudgeNote   = "[Automatic status notice from ccs watch, the user's PR watcher. Not typed by the user and not from another Claude session: there is no one to reply to.]\n\n"
)

// nudgeSent is what one session was last told about one PR, at its head.
type nudgeSent struct {
	Head      string `json:"head"`
	Threads   int    `json:"threads,omitempty"` // the unresolved count it was told of
	Checks    bool   `json:"checks,omitempty"`
	Conflicts bool   `json:"conflicts,omitempty"`
}

// nudgeState is nudgesPath: what was sent, by nudgeKey.
type nudgeState struct {
	Sent map[string]*nudgeSent `json:"sent"`
}

func nudgesPath() string     { return filepath.Join(filepath.Dir(settingsPath), "nudges.json") }
func nudgesLockPath() string { return filepath.Join(filepath.Dir(settingsPath), "nudges.lock") }

// nudgeKey is one session's record for one PR.
func nudgeKey(pr, session string) string { return pr + " " + session }

// nudgeEvent is a line of prEventsPath about a nudge.
type nudgeEvent struct {
	At      string   `json:"at"`
	Event   string   `json:"event"` // nudged, nudge_failed
	PR      string   `json:"pr"`
	Head    string   `json:"head,omitempty"`
	Session string   `json:"session"` // the session's name
	Reasons []string `json:"reasons"` // threads, checks, conflicts
	Error   string   `json:"error,omitempty"`
}

// sendNudge delivers a nudge to a live session; stubbed in tests.
var sendNudge = func(c prClaim, text string) error {
	if c.Kind == "codex" {
		return codexQueue(c.Thread, codexNudgeNote+text)
	}
	f, err := readSessionFile(c.PID)
	if err != nil {
		return fmt.Errorf("session file: %w", err)
	}
	if f.SessionID != c.Session {
		return fmt.Errorf("pid %d is now another session", c.PID)
	}
	if f.Socket == "" {
		return fmt.Errorf("it has no message socket")
	}
	_, err = sendEnvelope(f.Socket, peerToken(c.PID, f.Socket), nudgeSender, nudgeNote, text, false)
	return err
}

// nudgeText is what a session is told about p, for the reasons given.
func nudgeText(p *watchedPR, reasons []string) string {
	var what []string
	for _, r := range reasons {
		switch r {
		case "threads":
			n := *p.Threads
			what = append(what, fmt.Sprintf("%d unresolved review thread%s", n, map[bool]string{true: "", false: "s"}[n == 1]))
		case "checks":
			what = append(what, "checks failing: "+strings.Join(p.Failing, ", "))
		case "conflicts":
			what = append(what, "merge conflicts with its base")
		}
	}
	return fmt.Sprintf("%s (%s) has %s.\nThis session claimed it with ccs claim. If it's yours, deal with it; if not, run: ccs claim %s --release", p.URL, p.Title, strings.Join(what, "; "), p.URL)
}

// nudgeReasons is what to tell a session about p that it hasn't been told
// at this head, given what it was told (nil: nothing yet).
func nudgeReasons(p *watchedPR, sent *nudgeSent) []string {
	if sent != nil && sent.Head != p.Head {
		sent = nil // a new head: everything wrong with it is news
	}
	if sent == nil {
		sent = &nudgeSent{}
	}
	var reasons []string
	if p.Threads != nil && *p.Threads > sent.Threads {
		reasons = append(reasons, "threads")
	}
	if p.Checks == "failing" && !sent.Checks {
		reasons = append(reasons, "checks")
	}
	if p.Conflicts && !sent.Conflicts {
		reasons = append(reasons, "conflicts")
	}
	return reasons
}

// nudgeTick nudges the live owners of the watcher's open PRs, records what
// was sent and appends the events. Records of PRs no longer open are dropped.
func nudgeTick(now time.Time) error {
	st, ok := loadPRWatchState()
	if !ok {
		return nil
	}
	claims := loadClaims()
	live := liveByPID()
	var events []any
	err := withNudgeState(func(s *nudgeState) bool {
		changed := false
		for key := range s.Sent {
			pr, _, _ := strings.Cut(key, " ")
			if p := st.PRs[pr]; p == nil || p.State != "open" {
				delete(s.Sent, key)
				changed = true
			}
		}
		urls := make([]string, 0, len(claims.Claims))
		for u := range claims.Claims {
			urls = append(urls, u)
		}
		slices.Sort(urls)
		for _, u := range urls {
			p := st.PRs[u]
			if p == nil || p.State != "open" || p.Draft {
				continue
			}
			for _, c := range claims.Claims[u] {
				if !claimLive(c, live) {
					continue
				}
				key := nudgeKey(u, c.Session)
				reasons := nudgeReasons(p, s.Sent[key])
				if len(reasons) == 0 {
					continue
				}
				name := ownerName(c, live)
				e := nudgeEvent{At: now.UTC().Format(time.RFC3339), Event: "nudged", PR: u, Head: p.Head, Session: name, Reasons: reasons}
				if err := sendNudge(c, nudgeText(p, reasons)); err != nil {
					e.Event, e.Error = "nudge_failed", err.Error()
					logActivity("nudge %s about %s: %v", name, u, err)
					events = append(events, e)
					continue // not recorded: tried again next pass
				}
				events = append(events, e)
				sent := s.Sent[key]
				if sent == nil || sent.Head != p.Head {
					sent = &nudgeSent{Head: p.Head}
					s.Sent[key] = sent
				}
				for _, r := range reasons {
					switch r {
					case "threads":
						sent.Threads = *p.Threads
					case "checks":
						sent.Checks = true
					case "conflicts":
						sent.Conflicts = true
					}
				}
				changed = true
			}
		}
		return changed
	})
	if werr := writeEventLines(events); err == nil {
		err = werr
	}
	return err
}

// withNudgeState runs f on the nudge record under an exclusive lock and
// saves it if f says it changed (a unique temp file, then a rename).
func withNudgeState(f func(s *nudgeState) bool) error {
	dir := filepath.Dir(nudgesPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(nudgesLockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	var s nudgeState
	if b, err := os.ReadFile(nudgesPath()); err == nil {
		json.Unmarshal(b, &s)
	}
	if s.Sent == nil {
		s.Sent = map[string]*nudgeSent{}
	}
	if !f(&s) {
		return nil
	}
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "nudges-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), nudgesPath()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

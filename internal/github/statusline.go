package github

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// Status line files: each Claude Code session's claimed open PRs, one file a
// session, so a status line script reads them (cat) instead of running ccs
// prs in every session every minute. The path and format are the status
// line script's own cache: ${TMPDIR:-/tmp}/claude-statusline-prs-<session
// id>, one line a PR, "<bucket> <url>", sorted by bucket: 0 ready to merge,
// 1 waiting (review, or for another PR to merge first), 2 checks running,
// 3 needs you, 4 draft. ccs watch writes every session's after each pass,
// removes those of sessions with none, and touches the sentinel (unix
// seconds), which tells the script ccs keeps them, so it skips its own
// refresh while the sentinel is fresh. ccs claim rewrites its session's.
const (
	statusLinePrefix   = "claude-statusline-prs-"
	statusLineSentinel = "claude-statusline-prs.ccs"
)

// StatusLineDir is where the files go: the TMPDIR the script reads. Tests
// point it at a temp folder.
var StatusLineDir = os.TempDir

// statusLineID: a session id safe as a file name (Claude Code's are uuids).
var statusLineID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// statusBucket is p's bucket in the status line files.
func statusBucket(p *WatchedPR) int {
	g, why := p.status()
	switch {
	case p.Draft:
		return 4
	case g == PRFix:
		return 3
	case g == PRReady:
		return 0
	case why == "checks running":
		return 2
	}
	return 1
}

// statusLines is each Claude Code session's file content by session id,
// from ccs watch's saved state and the claims, read only (no network).
func statusLines() map[string]string {
	st, ok := LoadPRWatchState()
	if !ok {
		return map[string]string{}
	}
	deps := LoadDepends()
	claims := LoadClaims()
	type line struct {
		b   int
		url string
	}
	bySession := map[string][]line{}
	for u, p := range st.PRs {
		if p.State != "open" {
			continue
		}
		c := *p
		if deps.Depends[u] != nil {
			c.BlockedBy, c.ClosedAfter = listBlocked(u, deps, st, false)
		}
		b := statusBucket(&c)
		for _, cl := range claims.Claims[u] {
			if cl.Kind == transcript.KindCodex || cl.Kind == transcript.KindAgy || !statusLineID.MatchString(cl.Session) {
				continue
			}
			if !slices.ContainsFunc(bySession[cl.Session], func(l line) bool { return l.url == u }) {
				bySession[cl.Session] = append(bySession[cl.Session], line{b, u})
			}
		}
	}
	out := make(map[string]string, len(bySession))
	for id, ls := range bySession {
		slices.SortFunc(ls, func(a, b line) int { return cmp.Or(a.b-b.b, strings.Compare(a.url, b.url)) })
		var sb strings.Builder
		for _, l := range ls {
			fmt.Fprintf(&sb, "%d %s\n", l.b, l.url)
		}
		out[id] = sb.String()
	}
	return out
}

// WriteStatusLines writes the status line files: with only set, that
// session's alone (ccs claim); else every session's, removing those of
// sessions with no claimed open PR, then the sentinel (ccs watch). A file
// is rewritten only when its content changes.
func WriteStatusLines(only string, now time.Time) error {
	dir := StatusLineDir()
	lines := statusLines()
	var errs []error
	put := func(id string) {
		path := filepath.Join(dir, statusLinePrefix+id)
		want, ok := lines[id]
		if !ok {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			return
		}
		if have, err := os.ReadFile(path); err == nil && string(have) == want {
			return
		}
		if err := writeAtomic(path, want); err != nil {
			errs = append(errs, err)
		}
	}
	if only != "" {
		if statusLineID.MatchString(only) {
			put(only)
		}
		return firstErr(errs)
	}
	for id := range lines {
		put(id)
	}
	old, _ := filepath.Glob(filepath.Join(dir, statusLinePrefix+"*"))
	for _, f := range old {
		id := strings.TrimPrefix(filepath.Base(f), statusLinePrefix)
		if _, ok := lines[id]; !ok && statusLineID.MatchString(id) { // never the script's .tmp files
			put(id)
		}
	}
	if err := writeAtomic(filepath.Join(dir, statusLineSentinel), strconv.FormatInt(now.Unix(), 10)+"\n"); err != nil {
		errs = append(errs, err)
	}
	return firstErr(errs)
}

func firstErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// writeAtomic writes path through a unique temp file beside it, so a reader
// never sees half a file.
func writeAtomic(path, content string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ccs-statusline-*")
	if err != nil {
		return err
	}
	_, werr := f.WriteString(content)
	cerr := f.Close()
	if err := cmp.Or(werr, cerr); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}

// logStatusLines writes them, logging a failure: a status line is never
// worth failing a pass or a claim over.
func logStatusLines(only string, now time.Time) {
	if err := WriteStatusLines(only, now); err != nil {
		state.LogActivity("status line files: %v", err)
	}
}

package github

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// Which local session opened or merged a PR. Everything on GitHub is the one
// user, whoever acted: a session, the browser, auto-merge or a cloud routine.
// The sessions' own transcripts tell them apart: a `gh pr create` whose output
// printed the PR's address when GitHub says it was created, a `gh pr merge`
// that ran when GitHub says it merged, or one with --auto before that. gh
// prints nothing for a merge outside a terminal, so a merge is matched by its
// command and the time, never by its output.

// ghAct is one `gh pr create` or `gh pr merge` a session ran.
type ghAct struct {
	Session string    // the session's id; a subagent's is its parent's
	Merge   bool      // gh pr merge, else gh pr create
	Auto    bool      // gh pr merge --auto: GitHub merges it later
	At      time.Time // when it was called
	Done    time.Time // when its result came back; zero while it runs
	Failed  bool      // the result was an error (refused, denied, failed)
	URLs    []string  // create: the addresses its output printed; merge: those its command names
	Nums    []int     // merge: the bare PR numbers its command names
	Bare    bool      // merge: names no PR ccs can read (the current branch, or a shell variable)
	Disable bool      // gh pr merge --disable-auto: turns auto-merge off, merges nothing
	Mixed   bool      // create: the command ran other gh commands too, so the address printed may be theirs
}

// PRActor is the local session behind something that happened to a PR.
type PRActor struct {
	Name    string `json:"name,omitempty"` // the session's name, "" when it has none
	Session string `json:"session"`        // its id
}

// localSlack is how far a transcript's clock and GitHub's may disagree.
const localSlack = 15 * time.Second

// localLookback is how old a transcript's last write may be and still be read.
const localLookback = 48 * time.Hour

// localActs is what the recently written transcripts say their sessions did.
type localActs struct {
	acts  []ghAct
	prs   map[string][]string // each session's PRs: Claude Code's pr-link records
	names map[string]string   // each session's name, when it was given one
}

// LocalActs reads them. Stubbed in tests.
var LocalActs = scanLocalActs

// localCache is how far each transcript has been read, so a pass takes in
// only what was appended.
var localCache = struct {
	sync.Mutex
	files map[string]*localFile
}{files: map[string]*localFile{}}

// localFile is one transcript, read up to off.
type localFile struct {
	off     int64
	session string
	name    string
	prs     []string
	acts    []ghAct
	pending map[string][]int // tool-use id to the acts awaiting its result
}

func scanLocalActs(now time.Time) *localActs {
	root := transcript.GetProjectsDir()
	paths, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	subs, _ := filepath.Glob(filepath.Join(root, "*", "*", "subagents", "agent-*.jsonl"))
	flows, _ := filepath.Glob(filepath.Join(root, "*", "*", "subagents", "workflows", "*", "agent-*.jsonl"))
	localCache.Lock()
	defer localCache.Unlock()
	out := &localActs{prs: map[string][]string{}, names: map[string]string{}}
	seen := map[string]bool{}
	for _, path := range append(append(paths, subs...), flows...) {
		if rel, err := filepath.Rel(root, path); err == nil && transcript.HiddenProjectDir(strings.SplitN(rel, string(filepath.Separator), 2)[0]) {
			continue // a copy moved aside by hand: its session's PRs come from the real one
		}
		info, err := os.Stat(path)
		if err != nil || now.Sub(info.ModTime()) > localLookback {
			continue
		}
		seen[path] = true
		f := localCache.files[path]
		if f == nil {
			f = &localFile{session: localSession(root, path)}
			localCache.files[path] = f
		}
		f.read(path)
		out.acts = append(out.acts, f.acts...)
		for _, u := range f.prs {
			if !slices.Contains(out.prs[f.session], u) {
				out.prs[f.session] = append(out.prs[f.session], u)
			}
		}
		if f.name != "" && !strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
			out.names[f.session] = f.name
		}
	}
	for path := range localCache.files {
		if !seen[path] {
			delete(localCache.files, path)
		}
	}
	return out
}

// localSession is the session a transcript belongs to: its file's name, or
// for a subagent's the folder its subagents folder is in.
func localSession(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSuffix(parts[1], ".jsonl")
}

// read takes in what path appended since the last read; a file that shrank
// (pruned) is read again from the start.
func (f *localFile) read(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || info.Size() == f.off {
		return
	} else if info.Size() < f.off {
		*f = localFile{session: f.session}
	}
	if _, err := file.Seek(f.off, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(file, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil { // an unfinished line waits for the next read
			return
		}
		f.off += int64(len(line))
		switch {
		case bytes.Contains(line, []byte(`"pr-link"`)) || bytes.Contains(line, []byte(`"custom-title"`)):
			f.record(line)
		case bytes.Contains(line, []byte("gh pr ")) || len(f.pending) > 0 && bytes.Contains(line, []byte(`"tool_result"`)):
			f.tools(line)
		}
	}
}

// record takes a pr-link or custom-title line.
func (f *localFile) record(line []byte) {
	var rec struct {
		Type        string `json:"type"`
		PRURL       string `json:"prUrl"`
		CustomTitle string `json:"customTitle"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	switch {
	case rec.Type == "pr-link" && transcript.PRURL.MatchString(rec.PRURL) && !slices.Contains(f.prs, rec.PRURL):
		f.prs = append(f.prs, rec.PRURL)
	case rec.Type == "custom-title" && rec.CustomTitle != "":
		f.name = rec.CustomTitle
	}
}

// tools notes a line's gh pr create and merge calls, and closes those its
// results answer.
func (f *localFile) tools(line []byte) {
	var rec struct {
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || len(rec.Message.Content) == 0 || rec.Message.Content[0] != '[' {
		return
	}
	var blocks []struct {
		Type      string                   `json:"type"`
		ID        string                   `json:"id"`
		Name      string                   `json:"name"`
		Input     struct{ Command string } `json:"input"`
		ToolUseID string                   `json:"tool_use_id"`
		IsError   bool                     `json:"is_error"`
		Content   json.RawMessage          `json:"content"`
	}
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return
	}
	at, _ := time.Parse(time.RFC3339, rec.Timestamp)
	for _, b := range blocks {
		switch {
		case b.Type == "tool_use" && b.Name == "Bash":
			for _, a := range ghActs(b.Input.Command) {
				a.Session, a.At = f.session, at
				if f.pending == nil {
					f.pending = map[string][]int{}
				}
				f.pending[b.ID] = append(f.pending[b.ID], len(f.acts))
				f.acts = append(f.acts, a)
			}
		case b.Type == "tool_result" && f.pending[b.ToolUseID] != nil:
			printed := transcript.GHOutputPRs(transcript.ResultText(b.Content))
			for _, i := range f.pending[b.ToolUseID] {
				a := &f.acts[i]
				a.Done, a.Failed = at, b.IsError
				// A create's PR is the one it printed; a merge that names none
				// (a shell variable) may still have printed the one it meant.
				if !a.Merge && !a.Mixed || a.Merge && a.Bare {
					a.URLs = printed
				}
			}
			delete(f.pending, b.ToolUseID)
		}
	}
}

var (
	ghMergeCmd  = regexp.MustCompile(`gh pr merge\b([^\n;&|]*)`)
	ghCreateCmd = regexp.MustCompile(`gh pr create\b`)
	// ghOtherCmd is a gh command that can print a PR's address and isn't a
	// create or a merge: with one in the same command, whose address a
	// create's output holds can't be told.
	ghOtherCmd = regexp.MustCompile(`gh (pr (view|list|status|checks|edit|comment|review|ready|close|reopen|diff|checkout)|api|search|issue|run|repo)\b`)
	// ghMergeValued are gh pr merge's flags that take a value.
	ghMergeValued = []string{"-R", "--repo", "-b", "--body", "-F", "--body-file", "-t", "--subject", "-A", "--author-email", "--match-head-commit"}
)

// ghActs is the gh pr create and merge calls in one shell command: at most
// one create (its PR comes from the output) and one act per merge.
func ghActs(cmd string) []ghAct {
	var acts []ghAct
	if ghCreateCmd.MatchString(cmd) {
		acts = append(acts, ghAct{Mixed: ghOtherCmd.MatchString(cmd) || len(ghCreateCmd.FindAllString(cmd, -1)) > 1})
	}
	for _, m := range ghMergeCmd.FindAllStringSubmatch(cmd, -1) {
		a := ghAct{Merge: true}
		repo, words := "", strings.Fields(m[1])
		var named []string
		for i := 0; i < len(words); i++ {
			w := strings.Trim(words[i], `"'`)
			flag, val, hasVal := strings.Cut(w, "=")
			switch {
			case w == "--auto":
				a.Auto = true
			case w == "--disable-auto":
				a.Disable = true
			case slices.Contains(ghMergeValued, flag):
				if !hasVal && i+1 < len(words) {
					i++
					val = strings.Trim(words[i], `"'`)
				}
				if flag == "-R" || flag == "--repo" {
					repo = strings.TrimPrefix(val, "https://github.com/")
				}
			case strings.HasPrefix(w, "-") || strings.ContainsAny(w, "<>"): // a flag, or a redirect
			default:
				named = append(named, w)
			}
		}
		for _, w := range named {
			n, err := strconv.Atoi(strings.TrimPrefix(w, "#"))
			switch {
			case transcript.PRURL.MatchString(w):
				a.URLs = append(a.URLs, w)
			case err == nil && n > 0 && repo != "":
				a.URLs = append(a.URLs, "https://github.com/"+repo+"/pull/"+strconv.Itoa(n))
			case err == nil && n > 0:
				a.Nums = append(a.Nums, n)
			}
		}
		a.Bare = len(a.URLs)+len(a.Nums) == 0
		acts = append(acts, a)
	}
	return acts
}

// actor is the session behind a.
func (l *localActs) actor(a *ghAct) *PRActor {
	return &PRActor{Name: l.names[a.Session], Session: a.Session}
}

// within: t fell between the call and its result, give or take the clocks.
// A call still running has no end, so nothing is within it yet.
func (a *ghAct) within(t time.Time) bool {
	return a.done() && !t.Before(a.At.Add(-localSlack)) && !t.After(a.Done.Add(localSlack))
}

// done: the call came back, and not as an error.
func (a *ghAct) done() bool { return !a.At.IsZero() && !a.Done.IsZero() && !a.Failed }

// opener is the session whose gh pr create printed p's address, at the time
// GitHub says p was created when that is known; nil when none did.
func (l *localActs) opener(p *WatchedPR) *PRActor {
	if l == nil {
		return nil
	}
	created, err := time.Parse(time.RFC3339, p.CreatedAt)
	for i := range l.acts {
		a := &l.acts[i]
		if a.Merge || !a.done() || !slices.Contains(a.URLs, p.URL) {
			continue
		}
		if err != nil || a.within(created) {
			return l.actor(a)
		}
	}
	return nil
}

// means: the merge a is of p. Its command names p's address, or p is one of
// the session's own PRs (Claude Code's pr-link records) and the command names
// its number or nothing (the current branch, a shell variable). A bare number
// alone could be any repo's PR.
func (l *localActs) means(a *ghAct, p *WatchedPR) bool {
	if slices.Contains(a.URLs, p.URL) {
		return true
	}
	return slices.Contains(l.prs[a.Session], p.URL) && (a.Bare || slices.Contains(a.Nums, p.Number))
}

// Merge attributions, as the merged event's merge field says them.
const (
	MergeLocal    = "local"      // a local session ran gh pr merge when it merged
	MergeAuto     = "auto_merge" // a local session ran gh pr merge --auto, and GitHub merged it later
	MergeNotLocal = "not_local"  // no local session's gh pr merge accounts for it: the browser, a cloud routine, the API
)

// merger is how p, merged at mergedAt, came to merge: the session whose gh pr
// merge ran then, else the last one to run gh pr merge --auto on it before,
// unless auto-merge was turned off again since. "" when it can't be told yet:
// no merge time, or a gh pr merge of it that hasn't come back.
func (l *localActs) merger(p *WatchedPR, mergedAt time.Time) (*PRActor, string) {
	if l == nil || mergedAt.IsZero() {
		return nil, ""
	}
	var direct, auto *ghAct
	var disabled time.Time // when auto-merge was last turned off
	running := false
	for i := range l.acts {
		a := &l.acts[i]
		if !a.Merge || a.Failed || !l.means(a, p) {
			continue
		}
		switch {
		case a.Done.IsZero():
			running = running || !a.At.After(mergedAt.Add(localSlack))
		case a.Disable:
			if a.At.After(disabled) && !a.At.After(mergedAt) {
				disabled = a.At
			}
		case a.Auto:
			if !a.At.After(mergedAt.Add(localSlack)) && (auto == nil || a.At.After(auto.At)) {
				auto = a
			}
		case a.within(mergedAt):
			if direct == nil || a.At.After(direct.At) {
				direct = a
			}
		}
	}
	switch {
	case direct != nil:
		return l.actor(direct), MergeLocal
	case auto != nil && auto.At.After(disabled):
		return l.actor(auto), MergeAuto
	case running:
		return nil, ""
	}
	return nil, MergeNotLocal
}

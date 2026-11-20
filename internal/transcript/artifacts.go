package transcript

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Artifact is a claude.ai page a session published, opened or read with
// Claude Code's Artifact tool. Only the tool's own result counts, never an
// address merely mentioned in text, as with PRs.
type Artifact struct {
	URL       string
	Title     string // the page's title, else what the call published or asked for
	Ts        string // the latest call that touched it
	Published bool   // this session published it, not only opened or read it
}

// ArtifactURL is a claude.ai artifact's address.
var ArtifactURL = regexp.MustCompile(`^https://claude\.ai/(?:code/)?artifact/[A-Za-z0-9_-]+$`)

// artifactInText is the first artifact address in a result's text, for a
// line whose toolUseResult ccs prune stripped.
var artifactInText = regexp.MustCompile(`https://claude\.ai/(?:code/)?artifact/[A-Za-z0-9_-]+`)

// artifactCall is an Artifact call awaiting its result.
type artifactCall struct {
	ID, Action, URL, Title string
}

// noteArtifactCall remembers an Artifact call whose result names a page:
// a publish of a page (not an asset upload), an open, a read or a delete.
func noteArtifactCall(conv *Conversation, call toolCall) {
	if call.Name != "Artifact" {
		return
	}
	var in struct {
		Action   string `json:"action"`
		URL      string `json:"url"`
		Title    string `json:"title"`
		FilePath string `json:"file_path"`
		Asset    bool   `json:"asset"`
		Path     string `json:"path"`
	}
	if json.Unmarshal(call.Input, &in) != nil {
		return
	}
	action := cmp.Or(in.Action, "publish")
	switch {
	case action == "publish" && in.Asset, action == "read" && in.Path != "", action == "delete" && in.Path != "":
		return // an asset or one published file, not the page
	case action != "publish" && action != "open" && action != "read" && action != "delete":
		return
	}
	title := in.Title
	if title == "" && in.FilePath != "" {
		title = filepath.Base(in.FilePath)
	}
	conv.artifactCalls = append(slices.Clone(conv.artifactCalls), artifactCall{call.ID, action, in.URL, title})
}

// noteArtifactResult applies the result of a call noteArtifactCall kept:
// the address and title from the line's toolUseResult, else from the call's
// input and the result's text.
func noteArtifactResult(conv *Conversation, id string, content json.RawMessage, isError bool, line []byte, ts string) {
	i := slices.IndexFunc(conv.artifactCalls, func(c artifactCall) bool { return c.ID == id })
	if i < 0 {
		return
	}
	call := conv.artifactCalls[i]
	conv.artifactCalls = slices.Delete(slices.Clone(conv.artifactCalls), i, i+1)
	if isError {
		return
	}
	var r struct {
		ToolUseResult *struct {
			URL   string `json:"url"`
			Title string `json:"title"`
			Read  *struct {
				URL   string `json:"url"`
				Title string `json:"title"`
				Code  int    `json:"code"`
			} `json:"read"`
		} `json:"toolUseResult"`
	}
	if bytes.Contains(line, []byte(`"toolUseResult"`)) {
		json.Unmarshal(line, &r)
	}
	url, title := "", ""
	if res := r.ToolUseResult; res != nil {
		url, title = res.URL, res.Title
		if res.Read != nil {
			if res.Read.Code != 0 && res.Read.Code != 200 {
				return
			}
			url, title = cmp.Or(url, res.Read.URL), cmp.Or(title, res.Read.Title)
		}
	}
	text := ResultText(content)
	if url == "" {
		switch call.Action {
		case "publish": // "Published <file> at <url> (Version N)"
			if strings.HasPrefix(text, "Published ") {
				url = artifactInText.FindString(text)
			}
		case "open": // "Opened the Artifact at <url> …"
			if strings.HasPrefix(text, "Opened ") {
				url = cmp.Or(artifactInText.FindString(text), call.URL)
			}
		default:
			url = call.URL
		}
	}
	url = strings.TrimSuffix(url, "/")
	if !ArtifactURL.MatchString(url) {
		return
	}
	j := slices.IndexFunc(conv.Artifacts, func(a Artifact) bool { return a.URL == url })
	if call.Action == "delete" {
		if j >= 0 {
			conv.Artifacts = slices.Delete(slices.Clone(conv.Artifacts), j, j+1)
		}
		return
	}
	a := Artifact{URL: url}
	if j >= 0 {
		a = conv.Artifacts[j]
	}
	a.Title = cmp.Or(title, a.Title, call.Title)
	a.Ts = ts
	a.Published = a.Published || call.Action == "publish"
	conv.Artifacts = slices.Clone(conv.Artifacts)
	if j >= 0 {
		conv.Artifacts[j] = a
	} else {
		conv.Artifacts = append(conv.Artifacts, a)
	}
}

// ArtifactsOf is conv's artifacts and then its subagents' not among them.
func ArtifactsOf(conv Conversation) []Artifact {
	sub := subagentArtifacts(conv)
	if len(sub) == 0 {
		return conv.Artifacts
	}
	out := slices.Clone(conv.Artifacts)
	for _, a := range sub {
		if !slices.ContainsFunc(out, func(b Artifact) bool { return b.URL == a.URL }) {
			out = append(out, a)
		}
	}
	return out
}

// LatestArtifact is the one touched last; ok is false when there are none.
func LatestArtifact(as []Artifact) (Artifact, bool) {
	if len(as) == 0 {
		return Artifact{}, false
	}
	return slices.MaxFunc(as, func(a, b Artifact) int { return strings.Compare(a.Ts, b.Ts) }), true
}

// subagentArtifactsTTL is how long subagentArtifacts trusts what it last read.
const subagentArtifactsTTL = 10 * time.Second

// SubArtifactCache is what subagentArtifacts last read, per subagents folder
// and per transcript; tests clear it.
var SubArtifactCache = struct {
	sync.Mutex
	Dirs  map[string]subArtifactsRead
	files map[string]*subArtifactFile
}{Dirs: map[string]subArtifactsRead{}, files: map[string]*subArtifactFile{}}

type subArtifactsRead struct {
	at        time.Time
	artifacts []Artifact
}

// subArtifactFile is how far one subagent transcript has been read: its
// complete lines up to off, and what they gave (calls awaiting results
// included) on a scratch Conversation.
type subArtifactFile struct {
	off  int64
	conv Conversation
}

// subagentArtifacts is the artifacts conv's subagents published, opened or
// read, reread at most every subagentArtifactsTTL and then only what each
// transcript appended (the preview asks on every frame).
func subagentArtifacts(conv Conversation) []Artifact {
	if conv.FilePath == "" {
		return nil
	}
	dir := filepath.Join(strings.TrimSuffix(conv.FilePath, ".jsonl"), "subagents")
	SubArtifactCache.Lock()
	defer SubArtifactCache.Unlock()
	if r, ok := SubArtifactCache.Dirs[dir]; ok && time.Since(r.at) < subagentArtifactsTTL {
		return r.artifacts
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	more, _ := filepath.Glob(filepath.Join(dir, "workflows", "*", "agent-*.jsonl"))
	var out []Artifact
	for _, path := range append(paths, more...) {
		f := SubArtifactCache.files[path]
		if f == nil {
			f = &subArtifactFile{}
			SubArtifactCache.files[path] = f
		}
		f.read(path)
		for _, a := range f.conv.Artifacts {
			if !slices.ContainsFunc(out, func(b Artifact) bool { return b.URL == a.URL }) {
				out = append(out, a)
			}
		}
	}
	SubArtifactCache.Dirs[dir] = subArtifactsRead{time.Now(), out}
	return out
}

// read takes in what path appended since the last read; a file that shrank
// is read again from the start.
func (f *subArtifactFile) read(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || info.Size() == f.off {
		return
	} else if info.Size() < f.off {
		*f = subArtifactFile{}
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
		if bytes.Contains(line, []byte(`"Artifact"`)) || len(f.conv.artifactCalls) > 0 && bytes.Contains(line, []byte(`"tool_result"`)) {
			f.line(line)
		}
	}
}

// line notes a line's Artifact calls and the results of those it kept.
func (f *subArtifactFile) line(line []byte) {
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
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
		toolCall
	}
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			noteArtifactCall(&f.conv, b.toolCall)
		case "tool_result":
			noteArtifactResult(&f.conv, b.ToolUseID, b.Content, b.IsError, line, rec.Timestamp)
		}
	}
}

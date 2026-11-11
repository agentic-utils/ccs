package github

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// Conditional requests: each REST GET ccs makes through ghJSON sends the
// ETag of the answer it saved last time (If-None-Match). GitHub answers 304
// Not Modified when nothing changed, and a 304 doesn't count against the
// rate limit, so re-reading an unchanged PR, its checks and its reviews
// costs nothing. Answers are saved one file a path in etagDir, shared by
// every ccs process and kept across restarts; files not rewritten in
// etagKeep are dropped (pruneETags), costing one full read if asked again.

const etagKeep = 3 * 24 * time.Hour

func etagDir() string { return filepath.Join(filepath.Dir(state.SettingsPath), "gh-etags") }

func etagFile(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(etagDir(), hex.EncodeToString(sum[:16])+".json")
}

// etagEntry is a saved answer: its ETag and body.
type etagEntry struct {
	Path string          `json:"path"`
	ETag string          `json:"etag"`
	Body json.RawMessage `json:"body"`
}

func loadETag(path string) (etagEntry, bool) {
	var e etagEntry
	b, err := os.ReadFile(etagFile(path))
	if err != nil || json.Unmarshal(b, &e) != nil || e.Path != path || e.ETag == "" || len(e.Body) == 0 {
		return etagEntry{}, false
	}
	return e, true
}

func saveETag(path, etag string, body []byte) {
	if !json.Valid(body) {
		return
	}
	if err := writeJSON(etagFile(path), etagEntry{Path: path, ETag: etag, Body: body}); err != nil {
		state.LogActivity("gh etags: couldn't save %s: %v", path, err)
	}
}

// prunedAt is when this process last pruned: ghGet prunes at most every
// etagPruneEvery, so ccs prs and ccs mcp keep the folder bounded without a
// ccs watch.
var (
	prunedMu sync.Mutex
	prunedAt time.Time
)

const etagPruneEvery = time.Hour

func prunePeriodically(now time.Time) {
	prunedMu.Lock()
	due := now.Sub(prunedAt) >= etagPruneEvery
	if due {
		prunedAt = now
	}
	prunedMu.Unlock()
	if due {
		pruneETags(now)
	}
}

// pruneETags drops saved answers not rewritten in etagKeep.
func pruneETags(now time.Time) {
	entries, err := os.ReadDir(etagDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > etagKeep {
			os.Remove(filepath.Join(etagDir(), e.Name()))
		}
	}
}

// splitHTTP reads gh api -i output: the status, the ETag and the body.
// Output with no status line (a stub, or a gh that ignored -i) is all body.
func splitHTTP(out []byte) (status int, etag string, body []byte) {
	if !bytes.HasPrefix(out, []byte("HTTP/")) {
		return 200, "", out
	}
	head, body, found := bytes.Cut(out, []byte("\r\n\r\n"))
	if !found {
		head, body, _ = bytes.Cut(out, []byte("\n\n"))
	}
	lines := strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n")
	if f := strings.Fields(lines[0]); len(f) >= 2 {
		status, _ = strconv.Atoi(f[1])
	}
	for _, l := range lines[1:] {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "etag") {
			etag = strings.TrimSpace(v)
		}
	}
	return status, etag, body
}

// ghGet is a REST GET of path through ghCall, made conditional on the saved
// answer's ETag; a 304 returns the saved body.
func ghGet(path string) ([]byte, error) {
	prunePeriodically(time.Now())
	saved, have := loadETag(path)
	args := []string{"api", path, "-i"}
	if have {
		args = append(args, "-H", "If-None-Match: "+saved.ETag)
	}
	out, err := ghCall(args...)
	if err != nil {
		return nil, err
	}
	status, etag, body := splitHTTP(out)
	switch {
	case status == 304 && have:
		now := time.Now() // still in use: keep it past pruneETags
		os.Chtimes(etagFile(path), now, now)
		return saved.Body, nil
	case status == 304:
		return nil, errors.New(path + ": 304 Not Modified with no saved answer")
	}
	if etag != "" {
		saveETag(path, etag, body)
	}
	return body, nil
}

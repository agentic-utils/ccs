package github

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// etagServer stubs gh api -i like GitHub: each path's body has an ETag, and
// a request whose If-None-Match matches it gets 304 with no body.
type etagServer struct {
	bodies   map[string]string
	full     int // 200 answers: what counts against the rate limit
	notMod   int // 304 answers: free
	crlf     bool
	lastArgs []string
}

func (s *etagServer) etag(path string) string {
	return fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(path+"\x00"+s.bodies[path])))
}

func (s *etagServer) run(args ...string) ([]byte, error) {
	s.lastArgs = args
	path := args[1]
	if !slices.Contains(args, "-i") { // like gh: no -i, no headers, so nothing could be cached
		s.full++
		return []byte(s.bodies[path]), nil
	}
	nl := "\n"
	if s.crlf {
		nl = "\r\n"
	}
	if i := slices.Index(args, "-H"); i >= 0 && strings.TrimPrefix(args[i+1], "If-None-Match: ") == s.etag(path) {
		s.notMod++
		// like gh: the headers on stdout, and exit 1 all the same
		return []byte("HTTP/2.0 304 Not Modified" + nl + "Etag: " + s.etag(path) + nl + nl), errors.New("exit status 1: gh: HTTP 304")
	}
	s.full++
	return []byte("HTTP/2.0 200 OK" + nl + "Content-Type: application/json" + nl + "ETag: " + s.etag(path) + nl + nl + s.bodies[path]), nil
}

func etagEnv(t *testing.T, bodies map[string]string) *etagServer {
	t.Helper()
	s := &etagServer{bodies: bodies}
	oldRun, oldSettings := RunGH, state.SettingsPath
	RunGH, state.SettingsPath = s.run, t.TempDir()+"/settings.json"
	t.Cleanup(func() { RunGH, state.SettingsPath = oldRun, oldSettings })
	return s
}

func TestGHJSONAsksConditionally(t *testing.T) {
	for _, crlf := range []bool{false, true} {
		s := etagEnv(t, map[string]string{"repos/o/r/pulls/1": `{"draft":true}`})
		s.crlf = crlf
		for i := range 3 {
			var pr struct{ Draft bool }
			if err := ghJSON(&pr, "repos/o/r/pulls/1"); err != nil || !pr.Draft {
				t.Fatalf("read %d: %v %+v", i, err, pr)
			}
		}
		if !slices.Contains(s.lastArgs, "-i") || !slices.Contains(s.lastArgs, "If-None-Match: "+s.etag("repos/o/r/pulls/1")) {
			t.Errorf("asks with -i and the saved ETag: %v", s.lastArgs)
		}
		if s.full != 1 || s.notMod != 2 {
			t.Errorf("crlf %v: one full read, then 304s from the saved answer: full %d, 304 %d", crlf, s.full, s.notMod)
		}
		// A change on GitHub's side: a new ETag, a full read, the new body.
		s.bodies["repos/o/r/pulls/1"] = `{"draft":false}` // same length, new ETag
		var pr struct{ Draft bool }
		if err := ghJSON(&pr, "repos/o/r/pulls/1"); err != nil || pr.Draft || s.full != 2 {
			t.Errorf("a changed answer is read in full: %v %+v full %d", err, pr, s.full)
		}
	}
}

func TestGHJSONSharesSavedAnswersAcrossPaths(t *testing.T) {
	s := etagEnv(t, map[string]string{"repos/o/r/pulls/1": `{"n":1}`, "repos/o/r/pulls/2": `{"n":2}`})
	var a, b struct{ N int }
	ghJSON(&a, "repos/o/r/pulls/1")
	ghJSON(&b, "repos/o/r/pulls/2")
	ghJSON(&a, "repos/o/r/pulls/1")
	ghJSON(&b, "repos/o/r/pulls/2")
	if a.N != 1 || b.N != 2 || s.full != 2 || s.notMod != 2 {
		t.Errorf("each path its own answer: %d %d full %d 304 %d", a.N, b.N, s.full, s.notMod)
	}
}

func TestGHJSON304WithoutASavedAnswer(t *testing.T) {
	s := etagEnv(t, map[string]string{"p": `{}`})
	RunGH = func(args ...string) ([]byte, error) {
		return []byte("HTTP/2.0 304 Not Modified\n\n"), errors.New("exit status 1: gh: HTTP 304")
	}
	var v any
	if err := ghJSON(&v, "p"); err == nil {
		t.Error("a 304 with nothing saved is an error, never an empty answer")
	}
	_ = s
}

func TestGHJSONWithoutHeaders(t *testing.T) {
	etagEnv(t, nil)
	RunGH = func(args ...string) ([]byte, error) { return []byte(`{"n":3}`), nil }
	var v struct{ N int }
	if err := ghJSON(&v, "p"); err != nil || v.N != 3 {
		t.Errorf("output with no status line is the body: %v %+v", err, v)
	}
	if _, ok := loadETag("p"); ok {
		t.Error("no ETag, nothing saved")
	}
}

func TestPruneETags(t *testing.T) {
	s := etagEnv(t, map[string]string{"old": `{}`, "used": `{"u":1}`})
	var v any
	ghJSON(&v, "old")
	ghJSON(&v, "used")
	long := time.Now().Add(-etagKeep - time.Hour)
	os.Chtimes(etagFile("old"), long, long)
	ghJSON(&v, "used") // a 304 touches it
	if s.notMod != 1 {
		t.Fatalf("304s: %d", s.notMod)
	}
	os.Chtimes(etagFile("used"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	pruneETags(time.Now())
	if _, ok := loadETag("old"); ok {
		t.Error("an answer unused for etagKeep is dropped")
	}
	if _, ok := loadETag("used"); !ok {
		t.Error("an answer used within etagKeep is kept")
	}
	// ghGet prunes by itself, at most every etagPruneEvery, ccs watch or not.
	ghJSON(&v, "old")
	os.Chtimes(etagFile("old"), long, long)
	prunedAt = time.Now()
	ghJSON(&v, "used")
	if _, ok := loadETag("old"); !ok {
		t.Error("pruned again within etagPruneEvery")
	}
	prunedAt = time.Time{}
	ghJSON(&v, "used")
	if _, ok := loadETag("old"); ok {
		t.Error("ghGet prunes once etagPruneEvery has passed")
	}
}

func TestGHGetKeepsThePathFirst(t *testing.T) {
	// ghResource reads the path from args[1]: search/ paths must stay in
	// the search bucket with -i and -H after them.
	s := etagEnv(t, map[string]string{"search/issues?q=x": `{}`})
	var v any
	ghJSON(&v, "search/issues?q=x")
	ghJSON(&v, "search/issues?q=x")
	if ghResource(s.lastArgs) != "search" || s.lastArgs[1] != "search/issues?q=x" {
		t.Errorf("args %v", s.lastArgs)
	}
}

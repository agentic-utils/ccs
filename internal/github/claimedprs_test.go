package github

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// A PR claimed for a session is among its PRs though its transcript never
// linked it (opened before a /clear or restart, then claimed again).
func TestPRsOfIncludesClaimedPRs(t *testing.T) {
	claimEnv(t) // the calling session is s-alpha
	conv := transcript.Conversation{SessionID: "s-alpha", FilePath: filepath.Join(t.TempDir(), "s-alpha.jsonl"), PRs: []string{claimC}}
	if got := PRsOf(conv); !slices.Equal(got, []string{claimC}) {
		t.Fatalf("nothing claimed: only the transcript's, got %v", got)
	}
	if _, err := Claim(claimA, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	ResetClaimedPRs()
	if got := PRsOf(conv); !slices.Equal(got, []string{claimC, claimA}) {
		t.Fatalf("claimed PR after the transcript's: got %v", got)
	}
	// One the transcript links too is listed once.
	if _, err := Claim(claimC, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	ResetClaimedPRs()
	if got := PRsOf(conv); !slices.Equal(got, []string{claimC, claimA}) {
		t.Fatalf("a PR both linked and claimed is listed once: got %v", got)
	}
	if !slices.Equal(conv.PRs, []string{claimC}) {
		t.Fatalf("PRsOf wrote into the conversation's list: %v", conv.PRs)
	}
}

// A released PR stops showing, unless the transcript links it too.
func TestPRsOfDropsReleasedClaims(t *testing.T) {
	claimEnv(t)
	conv := transcript.Conversation{SessionID: "s-alpha", FilePath: filepath.Join(t.TempDir(), "s-alpha.jsonl"), PRs: []string{claimB}}
	for _, pr := range []string{claimA, claimB} {
		if _, err := Claim(pr, false, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	ResetClaimedPRs()
	if got := PRsOf(conv); !slices.Equal(got, []string{claimB, claimA}) {
		t.Fatalf("both claimed: got %v", got)
	}
	for _, pr := range []string{claimA, claimB} {
		if _, err := Claim(pr, true, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	ResetClaimedPRs()
	if got := PRsOf(conv); !slices.Equal(got, []string{claimB}) {
		t.Fatalf("released: only the one the transcript links stays, got %v", got)
	}
}

// Claims match by kind and session id, as ccs prs --claude-session-id does:
// another session with the same name, or a Codex thread with the same id,
// isn't this session.
func TestPRsOfMatchesClaimsByIDNotName(t *testing.T) {
	claimEnv(t)
	WithClaims(func(c *ClaimsFile) bool {
		c.Claims[claimA] = []PRClaim{{Session: "s-other", Name: "alpha"}}                                 // same name, other session
		c.Claims[claimB] = []PRClaim{{Session: "s-alpha", Kind: transcript.KindCodex, Thread: "s-alpha"}} // a Codex thread
		c.Claims[claimC] = []PRClaim{{Session: "s-alpha", Name: "renamed"}}                               // this session, renamed since
		return true
	})
	ResetClaimedPRs()
	claude := transcript.Conversation{SessionID: "s-alpha", Title: "alpha", FilePath: filepath.Join(t.TempDir(), "a.jsonl")}
	if got := PRsOf(claude); !slices.Equal(got, []string{claimC}) {
		t.Fatalf("Claude session s-alpha: only its own claim by id, got %v", got)
	}
	codex := transcript.Conversation{SessionID: "s-alpha", Kind: transcript.KindCodex, FilePath: filepath.Join(t.TempDir(), "c.jsonl")}
	if got := PRsOf(codex); !slices.Equal(got, []string{claimB}) {
		t.Fatalf("Codex thread s-alpha: its claim by thread, got %v", got)
	}
	sub := transcript.Conversation{SessionID: "s-alpha", Kind: transcript.KindSubagent}
	if got := PRsOf(sub); len(got) != 0 {
		t.Fatalf("a subagent row claims nothing: got %v", got)
	}
}

// The list asks for every session on every pass: claims.json is stat'ed at
// most once a claimedTTL and read again only when it changed, and an
// unchanged answer is the same slice, so PRsOf's merged list stays cached.
func TestClaimedPRsReadsTheFileOnlyWhenItChanges(t *testing.T) {
	claimEnv(t)
	conv := transcript.Conversation{SessionID: "s-alpha", FilePath: filepath.Join(t.TempDir(), "s.jsonl"), PRs: []string{claimC}}
	if _, err := Claim(claimA, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	ResetClaimedPRs()
	first := PRsOf(conv)
	if again := PRsOf(conv); !sameArray(first, again) {
		t.Fatal("an unchanged claims file should hand back the cached merged list")
	}
	if _, err := Claim(claimB, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := PRsOf(conv); !slices.Equal(got, []string{claimC, claimA}) {
		t.Fatalf("within claimedTTL the file isn't looked at again: got %v", got)
	}
	claimedCache.Lock()
	claimedCache.checked = time.Now().Add(-2 * claimedTTL)
	claimedCache.Unlock()
	if got := PRsOf(conv); !slices.Equal(got, []string{claimC, claimA, claimB}) {
		t.Fatalf("past claimedTTL a changed file is read again: got %v", got)
	}
}

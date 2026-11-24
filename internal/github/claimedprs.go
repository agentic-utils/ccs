package github

import (
	"os"
	"slices"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// A session's PRs include the ones claimed for it (ccs claim, claim_pr,
// ccs watch's auto-claim, a handover or switch), so a PR it opened before a
// /clear or restart, which its transcript no longer links, still shows.
// They match as ccs prs --claude-session-id does: by kind and id
// (OwnerOf, OwnerKey), never by name. A released claim is gone from the
// file, so the PR goes with it unless the transcript links it too.

// claimedTTL is how long claimedPRs trusts what it last read before it
// stats claims.json again: PRsOf runs for every session on every pass of
// the list, so a stat per call would be too many.
const claimedTTL = 5 * time.Second

// claimedCache is claims.json as last read, each owner's PRs by OwnerKey.
// The slices are handed out again while the file is unchanged, so PRsOf's
// merged list stays cached (sameArray); callers never write into them.
var claimedCache = struct {
	sync.Mutex
	checked time.Time
	mtime   time.Time
	size    int64
	read    bool
	by      map[string][]string
}{}

// ResetClaimedPRs forgets what claimedPRs read, so the next call reads
// claims.json again. For tests.
func ResetClaimedPRs() {
	claimedCache.Lock()
	defer claimedCache.Unlock()
	claimedCache.checked, claimedCache.mtime, claimedCache.size = time.Time{}, time.Time{}, 0
	claimedCache.read, claimedCache.by = false, nil
}

// convOwnerKey is the OwnerKey claims for conv are filed under; "" for a
// subagent, which never claims.
func convOwnerKey(conv transcript.Conversation) string {
	if conv.SessionID == "" {
		return ""
	}
	switch conv.Kind {
	case "", "claude":
		return OwnerKey("claude", conv.SessionID)
	case transcript.KindCodex, transcript.KindAgy:
		return OwnerKey(conv.Kind, conv.SessionID)
	}
	return ""
}

// claimedPRs is the PRs claimed for conv's session, sorted by address.
func claimedPRs(conv transcript.Conversation) []string {
	key := convOwnerKey(conv)
	if key == "" {
		return nil
	}
	claimedCache.Lock()
	defer claimedCache.Unlock()
	now := time.Now()
	if !claimedCache.read || now.Sub(claimedCache.checked) >= claimedTTL {
		claimedCache.checked = now
		fi, err := os.Stat(ClaimsPath())
		switch {
		case err != nil:
			claimedCache.read, claimedCache.by = true, nil
			claimedCache.mtime, claimedCache.size = time.Time{}, 0
		case !claimedCache.read || !fi.ModTime().Equal(claimedCache.mtime) || fi.Size() != claimedCache.size:
			claimedCache.read, claimedCache.mtime, claimedCache.size = true, fi.ModTime(), fi.Size()
			claimedCache.by = claimedByOwner(LoadClaims())
		}
	}
	return claimedCache.by[key]
}

// claimedByOwner is each owner's claimed PRs by OwnerKey, sorted.
func claimedByOwner(c ClaimsFile) map[string][]string {
	by := map[string][]string{}
	for u, cs := range c.Claims {
		if !transcript.PRURL.MatchString(u) { // it goes into a GraphQL query
			continue
		}
		for _, cl := range cs {
			o := OwnerOf(cl, nil)
			k := OwnerKey(o.Kind, o.Session)
			if !slices.Contains(by[k], u) {
				by[k] = append(by[k], u)
			}
		}
	}
	for k := range by {
		slices.Sort(by[k])
		by[k] = slices.Clip(by[k])
	}
	return by
}

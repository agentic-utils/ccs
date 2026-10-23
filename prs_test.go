package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	prA = "https://github.com/o/r/pull/1"
	prB = "https://github.com/o/r/pull/2"
	prC = "https://github.com/o/r/pull/3"
	prD = "https://github.com/o/r/pull/4"
)

func TestPRLinksParsed(t *testing.T) {
	var c Conversation
	for _, l := range []string{
		`{"type":"pr-link","prUrl":"` + prA + `"}`,
		`{"type":"pr-link","prUrl":"` + prB + `"}`,
		`{"type":"pr-link","prUrl":"` + prA + `"}`,                         // linked again
		`{"type":"pr-link","prUrl":"https://github.com/o/r\")}{x/pull/5"}`, // not a PR address
		`{"type":"pr-link","prUrl":"https://gitlab.com/o/r/pull/6"}`,       // not github.com
	} {
		parseLine(&c, []byte(l))
	}
	if !slices.Equal(c.PRs, []string{prA, prB}) {
		t.Errorf("each github.com PR once, first touched first: %v", c.PRs)
	}
}

func TestPRsOrderedReadyFirstMergedLast(t *testing.T) {
	m := model{prs: map[string]prInfo{
		prA: {State: "MERGED", Title: "old"},
		prB: {State: "OPEN", MergeState: "BLOCKED", Review: "REVIEW_REQUIRED", Title: "waits"},
		prC: {State: "OPEN", MergeState: "CLEAN", Title: "go"},
		prD: {State: "OPEN", MergeState: "BLOCKED", Checks: "FAILURE", Title: "broken"},
	}, width: 100}
	conv := Conversation{PRs: []string{prA, prB, prC, prD}}
	if got := m.sessionPRs(conv); !slices.Equal(got, []string{prC, prD, prB, prA}) {
		t.Errorf("ready, needs you, waiting, merged: %v", got)
	}
	lines := m.prLines(conv, 100)
	for i, want := range []string{"r#3 go · ready to merge", "r#4 broken · checks failing", "r#2 waits · review required", "1 merged · ^T all"} {
		if !strings.Contains(strip2(lines[i]), want) {
			t.Errorf("line %d = %q, want %q", i, strip2(lines[i]), want)
		}
	}
	if got := strings.TrimSpace(m.prCell(conv, false)); got != "✓1 ✗1 1" {
		t.Errorf("PRS column counts open PRs only: %q", got)
	}
	if got := m.prCell(Conversation{PRs: []string{prA}}, false); strings.TrimSpace(got) != "" {
		t.Errorf("nothing open, nothing shown: %q", got)
	}
	m.prs[prB] = prInfo{State: "OPEN", Draft: true, MergeState: "CLEAN"}
	if _, why := m.prs[prB].status(); why != "draft" {
		t.Errorf("a draft is never ready to merge: %s", why)
	}
}

func TestPRFetchScope(t *testing.T) {
	var asked [][]string
	m := initialModel(buildItems([]Conversation{
		{SessionID: "new", LastTimestamp: time.Now().UTC().Format(time.RFC3339), PRs: []string{prA, prB}},
		{SessionID: "old", LastTimestamp: "2020-01-01T00:00:00Z", PRs: []string{prC}},
	}), "", nil)
	m.fetchPRs = func(urls []string) (map[string]prInfo, error) { asked = append(asked, urls); return nil, nil }
	sel := func(id string) {
		m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id })
	}
	// run sends what prCmd wants and applies the answers (none: the fake knows nothing).
	run := func() {
		var msgs []prDoneMsg
		var collect func(tea.Msg)
		collect = func(msg tea.Msg) {
			switch msg := msg.(type) {
			case tea.BatchMsg:
				for _, c := range msg {
					if c != nil {
						collect(c())
					}
				}
			case prDoneMsg:
				msgs = append(msgs, msg)
			}
		}
		if cmd := m.prCmd(); cmd != nil {
			collect(cmd())
		}
		for _, msg := range msgs {
			m.applyPRs(msg)
		}
	}
	sel("old")
	run()
	if len(asked) != 2 || !slices.Equal(asked[0], []string{prC}) || !slices.Equal(asked[1], []string{prA, prB, prC}) {
		t.Fatalf("the selected session's PRs go first on their own, then a round of recent and selected sessions: %v", asked)
	}
	asked = nil
	run()
	if len(asked) != 0 {
		t.Fatalf("nothing more until the next round: %v", asked)
	}
	m.prs[prA] = prInfo{State: "MERGED"}
	m.prs[prB] = prInfo{State: "OPEN"}
	m.prs[prC] = prInfo{State: "CLOSED"}
	m.prNext = time.Time{}
	run()
	if len(asked) != 1 || !slices.Equal(asked[0], []string{prB}) {
		t.Errorf("a round re-asks open PRs only, never merged or closed ones: %v", asked)
	}
	// While a round is out, a newly selected session isn't kept waiting.
	m.prs = map[string]prInfo{}
	m.prSelAsked = map[string]bool{}
	m.prNext, m.prRound = time.Time{}, prParallel
	asked = nil
	sel("new")
	run()
	if len(asked) != 1 || !slices.Equal(asked[0], []string{prA, prB}) {
		t.Errorf("the selected session is asked about at once: %v", asked)
	}
}

// A fake gh answering one PR and failing another the way GitHub does: the
// data for the rest, an error for that field, and a non-zero exit.
func TestGhPRBatchReadsPartialAnswers(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	os.WriteFile(gh, []byte(`#!/bin/sh
case "$*" in *'repository(owner:"o",name:"r"){pullRequest(number:1)'*) ;; *) exit 3;; esac
echo '{"data":{"p0":{"pullRequest":{"number":1,"title":"t","state":"OPEN","isDraft":false,"mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}}},"p1":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND"}]}'
exit 1
`), 0o755)
	got, err := ghPRBatch(gh, []string{prA, prB})
	if err != nil || got[prA] != (prInfo{Number: 1, Title: "t", State: "OPEN", MergeState: "CLEAN", Review: "APPROVED", Checks: "SUCCESS"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[prB].State != "GONE" {
		t.Errorf("a PR GitHub couldn't find says so instead of checking forever: %+v", got[prB])
	}
}

func TestPRColoursMatchGitHub(t *testing.T) {
	u := func(n int) string { return fmt.Sprint("https://github.com/o/r/pull/", n) }
	conv := Conversation{SessionID: "s", PRs: []string{u(1), u(2), u(3), u(4), u(5)}}
	m := initialModel(buildItems([]Conversation{conv}), "", nil)
	m.prs = map[string]prInfo{
		u(1): {State: "OPEN", MergeState: "CLEAN"},
		u(2): {State: "OPEN", Checks: "PENDING", MergeState: "BLOCKED"},
		u(3): {State: "OPEN", Draft: true},
		u(4): {State: "MERGED"},
		u(5): {State: "CLOSED"},
	}
	lines := m.prLines(conv, 200)
	for i, want := range []string{"\033[32m✓", "\033[" + amber + "m○", "\033[90m○"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d should start %q: %q", i, want, lines[i])
		}
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "\033["+prPurple+"m1 merged") || !strings.Contains(last, "\033[31m1 closed") {
		t.Errorf("merged should be purple and closed red: %q", last)
	}
}

package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/transcript"
)

const (
	prA = "https://github.com/o/r/pull/1"
	prB = "https://github.com/o/r/pull/2"
	prC = "https://github.com/o/r/pull/3"
	prD = "https://github.com/o/r/pull/4"
)

func TestPRLinksParsed(t *testing.T) {
	var c transcript.Conversation
	for _, l := range []string{
		`{"type":"pr-link","prUrl":"` + prA + `"}`,
		`{"type":"pr-link","prUrl":"` + prB + `"}`,
		`{"type":"pr-link","prUrl":"` + prA + `"}`,                         // linked again
		`{"type":"pr-link","prUrl":"https://github.com/o/r\")}{x/pull/5"}`, // not a PR address
		`{"type":"pr-link","prUrl":"https://gitlab.com/o/r/pull/6"}`,       // not github.com
	} {
		transcript.ParseLine(&c, []byte(l))
	}
	if !slices.Equal(c.PRs, []string{prA, prB}) {
		t.Errorf("each github.com PR once, first touched first: %v", c.PRs)
	}
}

// ghLines is a Bash call running cmd and its result printing out.
func ghLines(id, cmd, out string) []string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{"command":` + q(cmd) + `}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + q(out) + `}]}}`,
	}
}

func TestPRsFromGhOutput(t *testing.T) {
	var lines []string
	lines = append(lines, ghLines("t1", `for r in a b; do gh pr create --repo o/$r --fill; done`, "a | "+prA+" | ok\nb | "+prB)...)
	lines = append(lines, ghLines("t2", `gh pr merge $n -R o/$r --merge`, "✓ Merged pull request o/r#3 (title)")...)
	lines = append(lines, ghLines("t3", `gh search prs --owner o`, prD)...) // only mentions a PR
	var c transcript.Conversation
	for _, l := range lines {
		transcript.ParseLine(&c, []byte(l))
	}
	if !slices.Equal(c.PRs, []string{prA, prB, prC}) {
		t.Errorf("PRs gh pr create and merge printed, not ones other output mentions: %v", c.PRs)
	}
	if len(c.GhCalls) != 0 {
		t.Errorf("answered calls are dropped: %v", c.GhCalls)
	}
}

func TestSubagentPRs(t *testing.T) {
	dir := t.TempDir()
	conv := transcript.Conversation{FilePath: filepath.Join(dir, "s.jsonl"), PRs: []string{prA}}
	sub := filepath.Join(dir, "s", "subagents")
	os.MkdirAll(filepath.Join(sub, "workflows", "wf_1"), 0o755)
	write := func(path string, lines []string) {
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(strings.Join(lines, "\n") + "\n")
		f.Close()
	}
	a := filepath.Join(sub, "agent-a.jsonl")
	write(a, ghLines("t1", "gh pr create --fill", prA+"\n"+prB))
	write(filepath.Join(sub, "workflows", "wf_1", "agent-w.jsonl"), ghLines("t2", "gh pr create --fill", prC))
	if got := github.PrsOf(conv); !slices.Equal(got, []string{prA, prB, prC}) {
		t.Errorf("the session's own PRs, then its subagents' not among them: %v", got)
	}
	write(a, ghLines("t3", "gh pr merge 4 --merge", "✓ Merged pull request o/r#4"))
	github.SubPRCache.Lock()
	delete(github.SubPRCache.Dirs, sub) // past subagentPRsTTL
	github.SubPRCache.Unlock()
	if got := github.PrsOf(conv); !slices.Contains(got, prD) {
		t.Errorf("a subagent's later output is read too: %v", got)
	}
}

func TestPRsOrderedReadyFirstMergedLast(t *testing.T) {
	m := model{prs: map[string]github.PrInfo{
		prA: {State: "MERGED", Title: "old"},
		prB: {State: "OPEN", MergeState: "BLOCKED", Review: "REVIEW_REQUIRED", Title: "waits"},
		prC: {State: "OPEN", MergeState: "CLEAN", Title: "go"},
		prD: {State: "OPEN", MergeState: "BLOCKED", Checks: "FAILURE", Title: "broken"},
	}, width: 100}
	conv := transcript.Conversation{PRs: []string{prA, prB, prC, prD}}
	if got := m.sessionPRs(conv); !slices.Equal(got, []string{prC, prD, prB, prA}) {
		t.Errorf("ready, needs you, waiting, merged: %v", got)
	}
	lines := m.prLines(conv, 100)
	for i, want := range []string{"r#3 go · ready to merge", "r#4 broken · checks failing", "r#2 waits · review required", "1 merged · ^O all"} {
		if !strings.Contains(strip2(lines[i]), want) {
			t.Errorf("line %d = %q, want %q", i, strip2(lines[i]), want)
		}
	}
	if got := strings.TrimSpace(m.prCell(conv, false)); got != "✓1 ✗1 1 ●1" {
		t.Errorf("PRS column counts every group: %q", got)
	}
	m.prs["https://github.com/o/r/pull/5"] = github.PrInfo{State: "CLOSED"}
	m.prs["https://github.com/o/r/pull/6"] = github.PrInfo{State: "GONE"}
	if got := strings.TrimSpace(m.prCell(transcript.Conversation{PRs: []string{prA, "https://github.com/o/r/pull/5", "https://github.com/o/r/pull/6"}}, false)); got != "●1" {
		t.Errorf("merged counted, closed and not found left out: %q", got)
	}
	if got := m.prCell(transcript.Conversation{PRs: []string{"https://github.com/o/r/pull/6"}}, false); strings.TrimSpace(got) != "" {
		t.Errorf("nothing found, nothing shown: %q", got)
	}
	m.prs[prB] = github.PrInfo{State: "OPEN", Draft: true, MergeState: "CLEAN"}
	if _, why := m.prs[prB].Status(); why != "draft" {
		t.Errorf("a draft is never ready to merge: %s", why)
	}
}

func TestPRFetchScope(t *testing.T) {
	var asked [][]string
	m := initialModel(buildItems([]transcript.Conversation{
		{SessionID: "new", LastTimestamp: time.Now().UTC().Format(time.RFC3339), PRs: []string{prA, prB}},
		{SessionID: "old", LastTimestamp: "2020-01-01T00:00:00Z", PRs: []string{prC}},
	}), "", nil)
	m.fetchPRs = func(urls []string) (map[string]github.PrInfo, error) { asked = append(asked, urls); return nil, nil }
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
	m.prs[prA] = github.PrInfo{State: "MERGED"}
	m.prs[prB] = github.PrInfo{State: "OPEN"}
	m.prs[prC] = github.PrInfo{State: "CLOSED"}
	m.prNext = time.Time{}
	run()
	if len(asked) != 1 || !slices.Equal(asked[0], []string{prB}) {
		t.Errorf("a round re-asks open PRs only, never merged or closed ones: %v", asked)
	}
	// While a round is out, a newly selected session isn't kept waiting.
	m.prs = map[string]github.PrInfo{}
	m.prSelAsked = map[string]bool{}
	m.prNext, m.prRound = time.Time{}, prParallel
	asked = nil
	sel("new")
	run()
	if len(asked) != 1 || !slices.Equal(asked[0], []string{prA, prB}) {
		t.Errorf("the selected session is asked about at once: %v", asked)
	}
}

func TestSelectingASessionRefreshesItsOpenPRs(t *testing.T) {
	var asked [][]string
	now := time.Now().UTC().Format(time.RFC3339)
	m := initialModel(buildItems([]transcript.Conversation{
		{SessionID: "a", LastTimestamp: now, PRs: []string{prA, prB}},
		{SessionID: "b", LastTimestamp: now, PRs: []string{prC}},
	}), "", nil)
	m.prs[prA] = github.PrInfo{State: "OPEN"}
	m.prs[prB] = github.PrInfo{State: "MERGED"}
	m.prs[prC] = github.PrInfo{State: "OPEN"}
	m.fetchPRs = func(urls []string) (map[string]github.PrInfo, error) { asked = append(asked, urls); return nil, nil }
	m.prNext = time.Now().Add(time.Hour) // no round in the way
	run := func(id string) {
		t.Helper()
		asked = nil
		m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id })
		cmd := m.prCmd()
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range msg {
				if c != nil {
					m.applyPRs(c().(prDoneMsg))
				}
			}
		case prDoneMsg:
			m.applyPRs(msg)
		}
	}
	run("a")
	if len(asked) != 1 || !slices.Equal(asked[0], []string{prA}) {
		t.Fatalf("landing refreshes its open PRs, not merged ones: %v", asked)
	}
	run("a")
	if len(asked) != 0 {
		t.Errorf("staying on it asks once: %v", asked)
	}
	run("b")
	if len(asked) != 1 || !slices.Equal(asked[0], []string{prC}) {
		t.Errorf("landing on another refreshes that one: %v", asked)
	}
	run("a")
	if len(asked) != 1 || !slices.Equal(asked[0], []string{prA}) {
		t.Errorf("coming back refreshes again: %v", asked)
	}
}

// A fake gh answering one PR and failing another the way GitHub does: the
// data for the rest, an error for that field, and a non-zero exit.
func TestGhPRBatchReadsPartialAnswers(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	os.WriteFile(gh, []byte(`#!/bin/sh
case "$*" in *'repository(owner:"o",name:"r"){pullRequest(number:1)'*) ;; *) exit 3;; esac
echo '{"data":{"p0":{"pullRequest":{"number":1,"title":"t","state":"OPEN","isDraft":false,"mergeStateStatus":"BLOCKED","reviewDecision":"APPROVED","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE","contexts":{"nodes":[{"name":"test","conclusion":"FAILURE"},{"name":"lint","conclusion":"SUCCESS"},{"context":"ci/deploy","state":"ERROR"}]}}}}]}}},"p1":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND"}]}'
exit 1
`), 0o755)
	got, err := github.GhPRBatch(gh, []string{prA, prB})
	want := github.PrInfo{Number: 1, Title: "t", State: "OPEN", MergeState: "BLOCKED", Review: "APPROVED", Checks: "FAILURE", Failing: []string{"test", "ci/deploy"}}
	if err != nil || !reflect.DeepEqual(got[prA], want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, why := got[prA].Status(); why != "checks failing: test, ci/deploy" {
		t.Errorf("a PR with failing checks names them: %q", why)
	}
	if got[prB].State != "GONE" {
		t.Errorf("a PR GitHub couldn't find says so instead of checking forever: %+v", got[prB])
	}
}

func TestPRColoursMatchGitHub(t *testing.T) {
	u := func(n int) string { return fmt.Sprint("https://github.com/o/r/pull/", n) }
	conv := transcript.Conversation{SessionID: "s", PRs: []string{u(1), u(2), u(3), u(4), u(5)}}
	m := initialModel(buildItems([]transcript.Conversation{conv}), "", nil)
	m.prs = map[string]github.PrInfo{
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

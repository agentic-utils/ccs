package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	revA       = "https://github.com/o/infra/pull/1"
	revB       = "https://github.com/o/infra/pull/2"
	revC       = "https://github.com/o/bot/pull/3"
	revChannel = "C0REVIEW1"
	revMention = "<!subteam^S0TEAM1>"
)

// fakeSlack records what ccs posts.
type fakeSlack struct {
	sync.Mutex
	posts []map[string]any
	auth  []string
}

func reviewSetup(t *testing.T) *fakeSlack {
	t.Helper()
	dir := t.TempDir()
	fs := &fakeSlack{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.Lock()
		defer fs.Unlock()
		fs.auth = append(fs.auth, r.Header.Get("Authorization"))
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasPrefix(r.URL.Path, "/conversations.open"):
			io.WriteString(w, `{"ok":true,"channel":{"id":"D0DM"}}`)
		case strings.HasPrefix(r.URL.Path, "/chat.postMessage"):
			fs.posts = append(fs.posts, body)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": body["channel"], "ts": "1.2"})
		case strings.HasPrefix(r.URL.Path, "/chat.getPermalink"):
			io.WriteString(w, `{"ok":true,"permalink":"https://example.slack.com/archives/C0REVIEW1/p12"}`)
		default:
			io.WriteString(w, `{"ok":false,"error":"unknown_method"}`)
		}
	}))
	t.Cleanup(srv.Close)
	oldS, oldL, oldBase, oldTok, oldPlan, oldCfg, oldRecheck := settingsPath, updateLogPath, slackBase, slackToken, planReview, reviewCfg, reviewRecheck
	reviewRecheck = func(p watchedPR) (*watchedPR, error) { return &p, nil } // unchanged on GitHub
	reviewCfg = reviewConfig{Channel: revChannel, Mention: revMention, Hook: "true"}
	settingsPath, updateLogPath, slackBase = dir+"/settings.json", dir+"/update.log", srv.URL+"/"
	slackToken = func() (string, error) { return "xoxp-secret", nil }
	t.Cleanup(func() {
		settingsPath, updateLogPath, slackBase, slackToken, planReview, reviewCfg, reviewRecheck = oldS, oldL, oldBase, oldTok, oldPlan, oldCfg, oldRecheck
	})
	return fs
}

// saveWatch writes the watcher's state with these PRs.
func saveWatch(t *testing.T, prs ...*watchedPR) {
	t.Helper()
	s := prWatchState{Author: "me", Owner: "o", PRs: map[string]*watchedPR{}}
	for _, p := range prs {
		s.PRs[p.URL] = p
	}
	if err := savePRWatchState(s); err != nil {
		t.Fatal(err)
	}
}

func greenPR(url, head string) *watchedPR {
	zero := 0
	return &watchedPR{URL: url, Repo: "o/infra", Number: 1, Title: "PLAT-1/feat: thing", State: "open", Head: head, Checks: "success", Review: "required", Threads: &zero}
}

// platformPlan puts every PR of the batch in one #control-plane post.
func platformPlan(items []reviewItem) (reviewPlan, error) {
	var p reviewPlan
	text := revMention + " could i get a review please"
	var prs []string
	for _, it := range items {
		text += "\n" + it.URL + " · green"
		prs = append(prs, it.URL)
	}
	p.ChannelPost = &struct {
		Text string   `json:"text"`
		PRs  []string `json:"prs"`
	}{text, prs}
	return p, nil
}

func events(t *testing.T) []reviewEvent {
	t.Helper()
	b, _ := os.ReadFile(prEventsPath())
	var out []reviewEvent
	for _, l := range bytes.Split(b, []byte("\n")) {
		var e reviewEvent
		if json.Unmarshal(l, &e) == nil && strings.HasPrefix(e.Event, "review_") {
			out = append(out, e)
		}
	}
	return out
}

// London, a weekday morning.
var reviewT0 = time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC)

func TestReviewWaitsForTheQueueToSettle(t *testing.T) {
	slack := reviewSetup(t)
	planned := 0
	planReview = func(items []reviewItem) (reviewPlan, error) { planned++; return platformPlan(items) }
	saveWatch(t, greenPR(revA, "h1"))
	for _, d := range []time.Duration{0, 5 * time.Minute} {
		if err := reviewTick(reviewT0.Add(d), false, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	saveWatch(t, greenPR(revA, "h1"), greenPR(revB, "h2")) // joins at +6m: the wait restarts
	reviewTick(reviewT0.Add(6*time.Minute), false, io.Discard)
	reviewTick(reviewT0.Add(12*time.Minute), false, io.Discard)
	if planned != 0 || len(slack.posts) != 0 {
		t.Fatalf("nothing goes until no PR has joined for %v: planned %d, posts %d", reviewDebounce, planned, len(slack.posts))
	}
	if err := reviewTick(reviewT0.Add(16*time.Minute), false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if planned != 1 || len(slack.posts) != 1 {
		t.Fatalf("one batch, one post: planned %d, posts %d", planned, len(slack.posts))
	}
	post := slack.posts[0]
	text, _ := post["text"].(string)
	if post["channel"] != revChannel || !strings.Contains(text, revA) || !strings.Contains(text, revB) || !strings.HasSuffix(text, reviewFooter) {
		t.Errorf("both PRs in #control-plane, footer last: %v", post)
	}
	if post["unfurl_links"] != false {
		t.Errorf("no link previews: %v", post)
	}
	ev := events(t)
	if len(ev) != 1 || ev[0].Event != "review_requested" || ev[0].Permalink == "" || len(ev[0].PRs) != 2 {
		t.Errorf("one review_requested with the permalink: %+v", ev)
	}

	// The same heads are never asked about again; a new head is.
	reviewTick(reviewT0.Add(40*time.Minute), false, io.Discard)
	if planned != 1 {
		t.Errorf("asked again for the same head")
	}
	saveWatch(t, greenPR(revA, "h1b"), greenPR(revB, "h2"))
	reviewTick(reviewT0.Add(41*time.Minute), false, io.Discard)
	reviewTick(reviewT0.Add(52*time.Minute), false, io.Discard)
	if planned != 2 {
		t.Errorf("a new head is asked about: planned %d", planned)
	}
	for _, a := range slack.auth {
		if a != "Bearer xoxp-secret" {
			t.Errorf("token in the header: %q", a)
		}
	}
	if b, _ := os.ReadFile(prEventsPath()); bytes.Contains(b, []byte("xoxp-secret")) {
		t.Error("the token reached the events file")
	}
}

func TestReviewSkipsUnresolvedThreads(t *testing.T) {
	reviewSetup(t)
	planned := 0
	planReview = func(items []reviewItem) (reviewPlan, error) { planned++; return platformPlan(items) }
	p := greenPR(revA, "h1")
	one := 1
	p.Threads = &one // the watcher's own predicate says it still needs the author
	saveWatch(t, p)
	reviewTick(reviewT0, false, io.Discard)
	reviewTick(reviewT0.Add(20*time.Minute), false, io.Discard)
	if planned != 0 {
		t.Error("a PR with an unresolved thread is never sent for review")
	}
}

func TestReviewQueueDropsWhatNoLongerNeedsReview(t *testing.T) {
	reviewSetup(t)
	planned := 0
	planReview = func(items []reviewItem) (reviewPlan, error) { planned++; return platformPlan(items) }
	saveWatch(t, greenPR(revA, "h1"))
	reviewTick(reviewT0, false, io.Discard)
	p := greenPR(revA, "h1")
	p.Review = "approved" // approved meanwhile: the predicate says no
	saveWatch(t, p)
	reviewTick(reviewT0.Add(20*time.Minute), false, io.Discard)
	if planned != 0 || len(loadReviewState().Queue) != 0 {
		t.Errorf("an approved PR leaves the queue unasked: planned %d", planned)
	}
}

func TestReviewQuietHours(t *testing.T) {
	slack := reviewSetup(t)
	planReview = platformPlan
	night := time.Date(2026, 11, 3, 22, 30, 0, 0, reviewTZ)
	saveWatch(t, greenPR(revA, "h1"))
	reviewTick(night, false, io.Discard)
	reviewTick(night.Add(3*time.Hour), false, io.Discard)
	if len(slack.posts) != 0 {
		t.Fatal("nothing is posted between 22:00 and 07:00")
	}
	saveWatch(t, greenPR(revA, "h1"), greenPR(revB, "h2"))
	reviewTick(night.Add(5*time.Hour), false, io.Discard)                       // 03:30, B joins
	reviewTick(time.Date(2026, 11, 4, 7, 0, 0, 0, reviewTZ), false, io.Discard) // morning
	if len(slack.posts) != 1 || !strings.Contains(slack.posts[0]["text"].(string), revB) {
		t.Errorf("one morning batch with both: %v", slack.posts)
	}
}

func TestReviewDraftsWaitForConfirmation(t *testing.T) {
	slack := reviewSetup(t)
	planReview = func(items []reviewItem) (reviewPlan, error) {
		var p reviewPlan
		p.Drafts = append(p.Drafts, struct {
			To     string   `json:"to"`
			ToName string   `json:"to_name"`
			Text   string   `json:"text"`
			PRs    []string `json:"prs"`
		}{"U0REVIEWER", "Sam", "hey, could you review " + revC + "?", []string{revC}})
		p.Holds = append(p.Holds, struct {
			PR     string `json:"pr"`
			Reason string `json:"reason"`
		}{revA, "waits for infra#9"})
		return p, nil
	}
	c := greenPR(revC, "h3")
	c.Repo = "o/bot"
	saveWatch(t, greenPR(revA, "h1"), c)
	reviewTick(reviewT0, false, io.Discard)
	if err := reviewTick(reviewT0.Add(reviewDebounce), false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(slack.posts) != 0 {
		t.Fatalf("a draft is never sent on its own: %v", slack.posts)
	}
	ev := events(t)
	if len(ev) != 2 || ev[0].Event != "review_dm_pending" || ev[0].ID != "r1" || !strings.HasSuffix(ev[0].Text, reviewFooter) || ev[1].Event != "review_held" || ev[1].PR != revA {
		t.Errorf("a pending draft and a hold: %+v", ev)
	}

	// The held PR isn't planned again within the hour; after it, it is.
	planned := 0
	planReview = func(items []reviewItem) (reviewPlan, error) { planned++; return platformPlan(items) }
	reviewTick(reviewT0.Add(30*time.Minute), false, io.Discard)
	reviewTick(reviewT0.Add(45*time.Minute), false, io.Discard)
	if planned != 0 {
		t.Errorf("held PR planned again too soon")
	}
	reviewTick(reviewT0.Add(71*time.Minute), false, io.Discard)
	reviewTick(reviewT0.Add(82*time.Minute), false, io.Discard)
	if planned != 1 {
		t.Errorf("held PR planned again after %v: %d", reviewHeldRetry, planned)
	}

	var out, errOut bytes.Buffer
	if code := runReview([]string{"list"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "r1  to Sam (U0REVIEWER)") {
		t.Errorf("list: %d %q %q", code, out.String(), errOut.String())
	}
	n := len(slack.posts)
	out.Reset()
	if code := runReview([]string{"send", "r1"}, &out, &errOut); code != 0 {
		t.Fatalf("send: %q", errOut.String())
	}
	if len(slack.posts) != n+1 || slack.posts[n]["channel"] != "D0DM" || !strings.Contains(slack.posts[n]["text"].(string), revC) {
		t.Errorf("sent to the reviewer's DM: %v", slack.posts)
	}
	if code := runReview([]string{"send", "r1"}, &out, &errOut); code != 1 {
		t.Errorf("a sent draft is gone")
	}
}

func TestReviewDropDiscardsADraft(t *testing.T) {
	slack := reviewSetup(t)
	withReviewState(func(s *reviewState) bool {
		s.Drafts["r7"] = reviewDraft{ID: "r7", To: "C0OTHER1", Text: "x", PRs: []string{revA}}
		return true
	})
	var out, errOut bytes.Buffer
	if code := runReview([]string{"drop", "r7"}, &out, &errOut); code != 0 || len(loadReviewState().Drafts) != 0 || len(slack.posts) != 0 {
		t.Errorf("drop: %d drafts %v posts %v", code, loadReviewState().Drafts, slack.posts)
	}
}

func TestReviewPlanFailureKeepsTheQueue(t *testing.T) {
	slack := reviewSetup(t)
	planReview = func([]reviewItem) (reviewPlan, error) { return reviewPlan{}, errors.New("review hook: timed out") }
	saveWatch(t, greenPR(revA, "h1"))
	reviewTick(reviewT0, false, io.Discard)
	if err := reviewTick(reviewT0.Add(reviewDebounce), false, io.Discard); err == nil {
		t.Fatal("a failed plan is an error")
	}
	if len(slack.posts) != 0 || len(loadReviewState().Queue) != 1 {
		t.Errorf("nothing posted, still queued")
	}
	if ev := events(t); len(ev) != 1 || ev[0].Event != "review_plan_failed" {
		t.Errorf("review_plan_failed: %+v", ev)
	}
}

func TestCheckPlanRefusesBadPlans(t *testing.T) {
	defer func(c reviewConfig) { reviewCfg = c }(reviewCfg)
	reviewCfg = reviewConfig{Channel: revChannel, Mention: revMention}
	items := []reviewItem{{URL: revA}, {URL: revB}}
	post := func(text string, prs ...string) reviewPlan {
		var p reviewPlan
		p.ChannelPost = &struct {
			Text string   `json:"text"`
			PRs  []string `json:"prs"`
		}{text, prs}
		return p
	}
	good := post(revMention+" review please\n"+revA+"\n"+revB, revA, revB)
	if got, warn, err := checkPlan(good, items); err != nil || got.ChannelPost == nil || len(warn) != 0 {
		t.Fatalf("a good plan: %+v %v %v", got, warn, err)
	}
	for name, p := range map[string]reviewPlan{
		"no mention":       post("review please\n"+revA+"\n"+revB, revA, revB),
		"not in the batch": post(revMention+" "+revA+" "+revB+" "+revC, revA, revB, revC),
		"links another PR": post(revMention+" "+revA+" "+revB+" see https://github.com/o/x/pull/9", revA, revB),
		"too many lines":   post(revMention+"\nplease\nthanks\n"+revA+"\n"+revB, revA, revB),
		"too long":         post(revMention+" "+revA+" "+revB+strings.Repeat("x", reviewMaxText), revA, revB),
	} {
		if _, _, err := checkPlan(p, items); err == nil {
			t.Errorf("%s: a bad post is the only thing in the plan, so nothing is left: accepted", name)
		}
	}
	// One PR left out: the post goes, the other stays queued with a warning.
	if got, warn, err := checkPlan(post(revMention+" "+revA, revA), items); err != nil || got.ChannelPost == nil || len(warn) != 1 || !strings.Contains(warn[0], revB) {
		t.Errorf("left out: %+v %v %v", got, warn, err)
	}
	draft := func(to string, prs ...string) struct {
		To     string   `json:"to"`
		ToName string   `json:"to_name"`
		Text   string   `json:"text"`
		PRs    []string `json:"prs"`
	} {
		return struct {
			To     string   `json:"to"`
			ToName string   `json:"to_name"`
			Text   string   `json:"text"`
			PRs    []string `json:"prs"`
		}{to, "x", "please " + strings.Join(prs, " "), prs}
	}
	// Posted and drafted: both kept (a DM to a reviewer the channel doesn't reach).
	both := post(revMention+"\n"+revA+"\n"+revB, revA, revB)
	both.Drafts = append(both.Drafts, draft("U0123456", revA))
	if got, warn, err := checkPlan(both, items); err != nil || got.ChannelPost == nil || len(got.Drafts) != 1 || len(warn) != 0 {
		t.Errorf("posted and drafted: %+v %v %v", got, warn, err)
	}
	// A hold for a PR that's asked for is dropped; a bad draft is dropped alone.
	mixed := post(revMention+"\n"+revA+"\n"+revB, revA, revB)
	mixed.Holds = append(mixed.Holds, struct {
		PR     string `json:"pr"`
		Reason string `json:"reason"`
	}{revA, "x"})
	mixed.Drafts = append(mixed.Drafts, draft("#control-plane", revA))
	got, warn, err := checkPlan(mixed, items)
	if err != nil || got.ChannelPost == nil || len(got.Holds) != 0 || len(got.Drafts) != 0 || len(warn) != 2 {
		t.Errorf("mixed: %+v %v %v", got, warn, err)
	}
}

func TestParsePlanTakesTheJSONOut(t *testing.T) {
	p, err := parsePlan("here you go:\n```json\n{\"channel_post\": null, \"drafts\": [], \"holds\": [{\"pr\": \"" + revA + "\", \"reason\": \"r\"}]}\n```")
	if err != nil || len(p.Holds) != 1 || p.Holds[0].PR != revA {
		t.Errorf("%+v %v", p, err)
	}
	if _, err := parsePlan("no idea"); err == nil {
		t.Error("no JSON: accepted")
	}
}

func TestReviewDryRunPostsAndRecordsNothing(t *testing.T) {
	slack := reviewSetup(t)
	planReview = platformPlan
	saveWatch(t, greenPR(revA, "h1"))
	var out bytes.Buffer
	if err := reviewTick(reviewT0, true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), revA) || len(slack.posts) != 0 || len(loadReviewState().Queue) != 0 || len(events(t)) != 0 {
		t.Errorf("dry run: printed %q, posts %d", out.String(), len(slack.posts))
	}
}

func TestReviewConfigChecked(t *testing.T) {
	good := reviewConfig{Channel: "C0REVIEW1", Mention: "<!subteam^S0TEAM1>", Hook: "cat --some-flag"}
	if err := good.check(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []reviewConfig{
		{Channel: "#control", Mention: good.Mention, Hook: good.Hook},
		{Channel: good.Channel, Mention: "@team", Hook: good.Hook},
		{Channel: good.Channel, Mention: good.Mention, Hook: ""},
		{Channel: good.Channel, Mention: good.Mention, Hook: "no-such-routing-hook-xyz"},
	} {
		if c.check() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestQuietHours(t *testing.T) {
	for h, want := range map[int]bool{21: false, 22: true, 3: true, 6: true, 7: false, 12: false} {
		if got := quietHours(time.Date(2026, 7, 1, h, 30, 0, 0, reviewTZ)); got != want {
			t.Errorf("%02d:30 London: quiet %v, want %v", h, got, want)
		}
	}
}

func TestReviewHookContract(t *testing.T) {
	dir := t.TempDir()
	hook := dir + "/route.sh"
	// The hook sees the batch on stdin only, and its stdout is the plan.
	os.WriteFile(hook, []byte(`#!/bin/sh
cat > "$(dirname "$0")/stdin.json"
echo "some chatter before the plan"
echo '{"channel_post":{"text":"<!subteam^S0TEAM1> please review","prs":["https://github.com/o/r/pull/1"]},"drafts":[],"holds":[]}'
`), 0o700)
	old := reviewCfg
	t.Cleanup(func() { reviewCfg = old })
	reviewCfg = reviewConfig{Channel: "C0REVIEW1", Mention: "<!subteam^S0TEAM1>", Hook: hook + " --arg"}
	items := []reviewItem{{URL: "https://github.com/o/r/pull/1", Title: "t; rm -rf / $(x)", Repo: "o/r", Head: "h"}}
	plan, err := hookPlan(items)
	if err != nil || plan.ChannelPost == nil || plan.ChannelPost.PRs[0] != items[0].URL {
		t.Fatalf("plan from the hook's stdout: %+v %v", plan, err)
	}
	var in hookInput
	data, _ := os.ReadFile(dir + "/stdin.json")
	if json.Unmarshal(data, &in) != nil || in.Channel != "C0REVIEW1" || in.Mention != "<!subteam^S0TEAM1>" || len(in.PRs) != 1 || in.PRs[0].Title != items[0].Title {
		t.Errorf("stdin = %s", data)
	}

	os.WriteFile(hook, []byte("#!/bin/sh\necho 'no routing table' >&2\nexit 3\n"), 0o700)
	if _, err := hookPlan(items); err == nil || !strings.Contains(err.Error(), "no routing table") {
		t.Errorf("a failed hook's stderr is reported: %v", err)
	}
}

func TestRecheckDropsPRsApprovedSincePlanning(t *testing.T) {
	reviewSetup(t)
	w := prWatchState{PRs: map[string]*watchedPR{
		revA: {URL: revA, Head: "h1", Checks: "success", Review: "required"},
		revB: {URL: revB, Head: "h2", Checks: "success", Review: "required"},
	}}
	reviewRecheck = func(p watchedPR) (*watchedPR, error) {
		if p.URL == revB {
			p.Review = "approved"
		}
		return &p, nil
	}
	var p reviewPlan
	p.ChannelPost = &struct {
		Text string   `json:"text"`
		PRs  []string `json:"prs"`
	}{revMention + " review please\n" + revA + " infra\n" + revB + " infra", []string{revA, revB}}
	got, warn := recheckPlan(p, w)
	if got.ChannelPost == nil || !slices.Equal(got.ChannelPost.PRs, []string{revA}) || strings.Contains(got.ChannelPost.Text, revB) || !strings.Contains(got.ChannelPost.Text, revMention) {
		t.Errorf("the approved PR goes from the post, line and all: %+v", got.ChannelPost)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "approved") {
		t.Errorf("and says why: %v", warn)
	}
	reviewRecheck = func(p watchedPR) (*watchedPR, error) { p.Review = "approved"; return &p, nil }
	if got, _ := recheckPlan(p, w); got.ChannelPost != nil {
		t.Errorf("all approved: no post: %+v", got.ChannelPost)
	}
}

func TestReviewSaysItIsDeprecated(t *testing.T) {
	reviewSetup(t)
	var out, errb strings.Builder
	runReview([]string{"list"}, &out, &errb)
	if !strings.Contains(errb.String(), "deprecated") {
		t.Errorf("ccs review warns it goes next release: %q", errb.String())
	}
}

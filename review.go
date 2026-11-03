package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Review-request batching (`ccs watch --review`): PRs the watcher sees green
// and waiting for an approval (watchedPR.needsReview) are queued, and once
// none has joined the queue for reviewDebounce (outside quiet hours) the
// batch goes to a headless claude, which routes it by the user's routing
// guide (reviewConfig). ccs checks its plan, posts the one automatic kind of
// message (a review request in the configured channel) as the user, and
// keeps every other message as a draft for `ccs review send`. Nothing is
// asked twice for the same head commit.

const (
	reviewDebounce  = 10 * time.Minute // no new PR in the queue for this long: send the batch
	reviewMaxWait   = 30 * time.Minute // a PR never waits longer than this for the queue to settle
	reviewHeldRetry = time.Hour        // a held PR is planned again after this, in case its prerequisite merged
	reviewMaxText   = 3000
	reviewFooter    = "_✨ via ccs watch · claude sonnet_"
)

// reviewConfig is where review requests go, from ccs watch's flags: the one
// channel ccs posts to on its own, the mention that post must carry, and a
// file of routing instructions for the planner (who reviews what, which
// channels, who to DM). None is built in: they are the user's own.
type reviewConfig struct {
	Channel string // Slack channel id
	Mention string // Slack mrkdwn, e.g. <!subteam^S…> or <@U…>
	Guide   string // path of the routing guide
}

var reviewCfg reviewConfig

// check: a channel id, a mention in Slack's own syntax and a readable guide.
func (c reviewConfig) check() error {
	if !regexp.MustCompile(`^[CG][A-Z0-9]{6,}$`).MatchString(c.Channel) {
		return fmt.Errorf("--review-channel %q isn't a Slack channel id", c.Channel)
	}
	if !regexp.MustCompile(`^<(!subteam\^[A-Z0-9]+|@[UW][A-Z0-9]+)>$`).MatchString(c.Mention) {
		return fmt.Errorf("--review-mention %q isn't <!subteam^…> or <@U…>", c.Mention)
	}
	if _, err := os.Stat(c.Guide); err != nil {
		return fmt.Errorf("--review-guide: %w", err)
	}
	return nil
}

// reviewTZ and the quiet hours: nothing is posted from 22:00 to 07:00 London time.
var reviewTZ = func() *time.Location {
	if l, err := time.LoadLocation("Europe/London"); err == nil {
		return l
	}
	return time.UTC
}()

func quietHours(t time.Time) bool {
	h := t.In(reviewTZ).Hour()
	return h >= 22 || h < 7
}

// reviewState is reviewPath: what was asked for, what's queued, the drafts.
// Keys of Requested and Held are reviewKey (PR and head commit).
type reviewState struct {
	Queue       map[string]reviewQueued `json:"queue"`
	LastJoin    time.Time               `json:"last_join"` // when a PR last joined the queue
	Requested   map[string]reviewAsked  `json:"requested"`
	Held        map[string]reviewAsked  `json:"held"`
	Drafts      map[string]reviewDraft  `json:"drafts"`
	NextDraftID int                     `json:"next_draft_id"`
}

type reviewQueued struct {
	Head   string    `json:"head"`
	Title  string    `json:"title"`
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	Since  time.Time `json:"since"`
}

type reviewAsked struct {
	At     time.Time `json:"at"`
	How    string    `json:"how"` // channel permalink, "draft <id>", or the hold's reason
	Reason string    `json:"reason,omitempty"`
}

// reviewDraft is a message ccs won't send without `ccs review send <id>`.
type reviewDraft struct {
	ID      string    `json:"id"`
	To      string    `json:"to"`      // Slack user (U…/W…) or channel (C…/G…/D…) id
	ToName  string    `json:"to_name"` // who that is, for the list
	Text    string    `json:"text"`    // as it will be sent, footer included
	PRs     []string  `json:"prs"`
	Heads   []string  `json:"heads"`
	Created time.Time `json:"created"`
}

func reviewKey(pr, head string) string { return pr + "@" + head }

func reviewPath() string     { return filepath.Join(filepath.Dir(settingsPath), "review.json") }
func reviewLockPath() string { return filepath.Join(filepath.Dir(settingsPath), "review.lock") }

func loadReviewState() reviewState {
	var s reviewState
	if b, err := os.ReadFile(reviewPath()); err == nil {
		json.Unmarshal(b, &s)
	}
	if s.Queue == nil {
		s.Queue = map[string]reviewQueued{}
	}
	if s.Requested == nil {
		s.Requested = map[string]reviewAsked{}
	}
	if s.Held == nil {
		s.Held = map[string]reviewAsked{}
	}
	if s.Drafts == nil {
		s.Drafts = map[string]reviewDraft{}
	}
	return s
}

// withReviewState runs f on the state under the lock (`ccs review send` runs
// beside the watcher) and saves it if f says so.
func withReviewState(f func(s *reviewState) bool) error {
	dir := filepath.Dir(reviewPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(reviewLockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	s := loadReviewState()
	if !f(&s) {
		return nil
	}
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "review-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), reviewPath())
}

// reviewEvent is a line of prEventsPath about review requests.
type reviewEvent struct {
	At        string   `json:"at"`
	Event     string   `json:"event"` // review_requested, review_dm_pending, review_held, review_plan_failed
	PR        string   `json:"pr,omitempty"`
	PRs       []string `json:"prs,omitempty"`
	Channel   string   `json:"channel,omitempty"`
	Permalink string   `json:"permalink,omitempty"`
	ID        string   `json:"id,omitempty"`
	Recipient string   `json:"recipient,omitempty"`
	Text      string   `json:"text,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

// reviewQueueUpdate puts the watcher's needs-review PRs not yet asked about
// into the queue and drops those that no longer need it (approved, failing,
// a new head: a new head joins again if it's still green).
func reviewQueueUpdate(s *reviewState, w prWatchState, now time.Time) bool {
	changed := false
	for url, q := range s.Queue {
		p := w.PRs[url]
		if p == nil || !p.needsReview() || p.Head != q.Head {
			delete(s.Queue, url)
			changed = true
		}
	}
	for url, p := range w.PRs {
		if !p.needsReview() {
			continue
		}
		key := reviewKey(url, p.Head)
		if _, done := s.Requested[key]; done {
			continue
		}
		if h, held := s.Held[key]; held && now.Sub(h.At) < reviewHeldRetry {
			continue
		}
		if _, queued := s.Queue[url]; queued {
			continue
		}
		s.Queue[url] = reviewQueued{Head: p.Head, Title: p.Title, Repo: p.Repo, Number: p.Number, Since: now}
		s.LastJoin = now
		changed = true
	}
	return changed
}

// reviewDue: the queue has settled (or waited long enough) and it isn't quiet hours.
func reviewDue(s reviewState, now time.Time) bool {
	if len(s.Queue) == 0 || quietHours(now) {
		return false
	}
	if now.Sub(s.LastJoin) >= reviewDebounce {
		return true
	}
	for _, q := range s.Queue {
		if now.Sub(q.Since) >= reviewMaxWait {
			return true
		}
	}
	return false
}

// reviewItem is one PR of a batch, as the planner sees it.
type reviewItem struct {
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	Repo      string   `json:"repo"`
	Head      string   `json:"head"`
	Status    string   `json:"status"`
	Reviewers []string `json:"requested_reviewers,omitempty"` // GitHub logins and @org/team slugs
	Owners    []string `json:"owner_sessions,omitempty"`
}

// reviewPlan is what the planner returns.
type reviewPlan struct {
	ChannelPost *struct {
		Text string   `json:"text"`
		PRs  []string `json:"prs"`
	} `json:"channel_post"`
	Drafts []struct {
		To     string   `json:"to"`
		ToName string   `json:"to_name"`
		Text   string   `json:"text"`
		PRs    []string `json:"prs"`
	} `json:"drafts"`
	Holds []struct {
		PR     string `json:"pr"`
		Reason string `json:"reason"`
	} `json:"holds"`
}

// requestedReviewers is who GitHub says the PR waits on. Stubbed in tests.
var requestedReviewers = func(repo string, number int) ([]string, error) {
	var pr struct {
		Users []struct {
			Login string `json:"login"`
		} `json:"requested_reviewers"`
		Teams []struct {
			Slug string `json:"slug"`
		} `json:"requested_teams"`
	}
	if err := ghJSON(&pr, fmt.Sprintf("repos/%s/pulls/%d", repo, number)); err != nil {
		return nil, err
	}
	var out []string
	for _, u := range pr.Users {
		out = append(out, u.Login)
	}
	owner, _, _ := strings.Cut(repo, "/")
	for _, t := range pr.Teams {
		out = append(out, "@"+owner+"/"+t.Slug)
	}
	return out, nil
}

// reviewBatch is the queue as planner input, oldest first.
func reviewBatch(s reviewState) []reviewItem {
	claims := loadClaims()
	var items []reviewItem
	for url, q := range s.Queue {
		it := reviewItem{URL: url, Title: q.Title, Repo: q.Repo, Head: q.Head, Status: "checks green, waiting for review"}
		if r, err := requestedReviewers(q.Repo, q.Number); err == nil {
			it.Reviewers = r
		} else {
			logActivity("review: reviewers of %s: %v", url, err)
		}
		for _, c := range claims.Claims[url] {
			if !slices.Contains(it.Owners, c.Name) {
				it.Owners = append(it.Owners, c.Name)
			}
		}
		items = append(items, it)
	}
	slices.SortFunc(items, func(a, b reviewItem) int { return s.Queue[a.URL].Since.Compare(s.Queue[b.URL].Since) })
	return items
}

// reviewPrompt is the headless run's instructions: the channel, the
// mention, the user's routing guide, then the batch as JSON.
const reviewPrompt = `You route GitHub review requests for the user who wrote these PRs. You decide; ccs posts. Do not post, send or edit anything yourself.

Follow the routing guide at the end. ccs enforces these rules:
- PRs the guide routes to the review channel (%[1]s) go in ONE message: it must mention %[2]s verbatim (plus any person mention the guide gives), be in the first person as the user, and list each PR as its full URL with a short status. At most one short ask line plus one line per PR. No footer: ccs adds it.
- Anyone the review channel doesn't reach gets a draft instead: a DM to their Slack user id (U...), or a draft to another channel's id. Find ids with Slack user search if you need to. Drafts are only sent once the user confirms them.
- Never guess a channel. If you can't route a PR with certainty, hold it with the reason.
- Hold a PR whose prerequisite hasn't merged (a dependency the batch or its title states).
- Every PR in the batch must appear exactly once: in channel_post.prs, in one draft's prs, or in holds.

Reply with ONLY this JSON, no prose, no code fences:
{"channel_post": {"text": "...", "prs": ["<url>"]} or null,
 "drafts": [{"to": "<U... or C... id>", "to_name": "<who>", "text": "...", "prs": ["<url>"]}],
 "holds": [{"pr": "<url>", "reason": "..."}]}

The routing guide:
%[3]s

The batch:
`

// planReview runs the headless planner. Stubbed in tests.
var planReview = func(items []reviewItem) (reviewPlan, error) {
	guide, err := os.ReadFile(reviewCfg.Guide)
	if err != nil {
		return reviewPlan{}, fmt.Errorf("routing guide: %w", err)
	}
	batch, _ := json.MarshalIndent(items, "", " ")
	prompt := fmt.Sprintf(reviewPrompt, reviewCfg.Channel, reviewCfg.Mention, guide) + string(batch)
	// The prompt goes first: the tool lists are variadic and would swallow it.
	out, err := runBounded(5*time.Minute, nil, "claude", "-p", prompt, "--model", "sonnet", "--output-format", "json",
		"--allowedTools", "Skill Read Grep Glob mcp__claude_ai_Slack__slack_search_users mcp__claude_ai_Slack__slack_read_user_profile",
		"--disallowedTools", "Bash Write Edit mcp__claude_ai_Slack__slack_send_message mcp__claude_ai_Slack__slack_send_message_draft mcp__claude_ai_Slack__slack_schedule_message")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		} else if len(out) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return reviewPlan{}, fmt.Errorf("claude -p: %w", err)
	}
	res, err := claudeResult(out)
	if err != nil {
		return reviewPlan{}, err
	}
	if res.IsError {
		return reviewPlan{}, fmt.Errorf("claude -p: %s", res.Result)
	}
	return parsePlan(res.Result)
}

// claudeOutput is a claude -p --output-format json result.
type claudeOutput struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// claudeResult is that result: one object, or (newer Claude Code) an array
// of messages ending with it.
func claudeResult(out []byte) (claudeOutput, error) {
	var one claudeOutput
	if json.Unmarshal(out, &one) == nil {
		return one, nil
	}
	var all []claudeOutput
	if err := json.Unmarshal(out, &all); err != nil {
		return claudeOutput{}, fmt.Errorf("claude -p output: %w", err)
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Type == "result" {
			return all[i], nil
		}
	}
	return claudeOutput{}, errors.New("claude -p output has no result")
}

// parsePlan takes the JSON object out of the planner's reply.
func parsePlan(s string) (reviewPlan, error) {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return reviewPlan{}, errors.New("no JSON in the planner's reply")
	}
	var p reviewPlan
	if err := json.Unmarshal([]byte(s[i:j+1]), &p); err != nil {
		return reviewPlan{}, fmt.Errorf("planner's JSON: %w", err)
	}
	return p, nil
}

var (
	slackID     = regexp.MustCompile(`^[UWCGD][A-Z0-9]{6,}$`)
	githubPRURL = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+`)
)

// checkPlan: every PR of the batch exactly once, nothing else; the channel
// post is to the one channel, mentions the team, links only its own PRs and
// is short; drafts go to a Slack id.
func checkPlan(p reviewPlan, items []reviewItem) error {
	inBatch := map[string]bool{}
	for _, it := range items {
		inBatch[it.URL] = true
	}
	seen := map[string]bool{}
	place := func(pr string) error {
		if !inBatch[pr] {
			return fmt.Errorf("%s isn't in the batch", pr)
		}
		if seen[pr] {
			return fmt.Errorf("%s is placed twice", pr)
		}
		seen[pr] = true
		return nil
	}
	linksOnly := func(text string, prs []string) error {
		if len(text) == 0 || len(text) > reviewMaxText {
			return fmt.Errorf("message length %d", len(text))
		}
		for _, u := range githubPRURL.FindAllString(text, -1) {
			if !slices.Contains(prs, u) {
				return fmt.Errorf("message links %s, which it isn't about", u)
			}
		}
		for _, u := range prs {
			if !strings.Contains(text, u) {
				return fmt.Errorf("message doesn't link %s", u)
			}
		}
		return nil
	}
	if c := p.ChannelPost; c != nil {
		if len(c.PRs) == 0 {
			return errors.New("channel post with no PRs")
		}
		for _, u := range c.PRs {
			if err := place(u); err != nil {
				return err
			}
		}
		if reviewCfg.Mention == "" || !strings.Contains(c.Text, reviewCfg.Mention) {
			return errors.New("channel post doesn't carry the review mention")
		}
		n := 0
		for _, l := range strings.Split(c.Text, "\n") {
			if strings.TrimSpace(l) != "" {
				n++
			}
		}
		if n > len(c.PRs)+1 {
			return fmt.Errorf("channel post is %d lines for %d PRs", n, len(c.PRs))
		}
		if err := linksOnly(c.Text, c.PRs); err != nil {
			return err
		}
	}
	for _, d := range p.Drafts {
		if !slackID.MatchString(d.To) {
			return fmt.Errorf("draft to %q, not a Slack id", d.To)
		}
		if len(d.PRs) == 0 {
			return errors.New("draft with no PRs")
		}
		for _, u := range d.PRs {
			if err := place(u); err != nil {
				return err
			}
		}
		if err := linksOnly(d.Text, d.PRs); err != nil {
			return err
		}
	}
	for _, h := range p.Holds {
		if err := place(h.PR); err != nil {
			return err
		}
	}
	for u := range inBatch {
		if !seen[u] {
			return fmt.Errorf("%s is left out", u)
		}
	}
	return nil
}

// slackBase and slackHTTP are Slack's Web API; tests point them at a stub.
var (
	slackBase = "https://slack.com/api/"
	slackHTTP = &http.Client{Timeout: 20 * time.Second}
)

// slackToken is SLACK_USER_TOKEN, from the environment or ~/.claude.secret.
// It goes only into the Authorization header: never logged, never an argument.
var slackToken = func() (string, error) {
	if t := os.Getenv("SLACK_USER_TOKEN"); t != "" {
		return t, nil
	}
	home, _ := os.UserHomeDir()
	f, err := os.Open(filepath.Join(home, ".claude.secret"))
	if err != nil {
		return "", errors.New("SLACK_USER_TOKEN isn't set and ~/.claude.secret can't be read")
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		if v, ok := strings.CutPrefix(line, "SLACK_USER_TOKEN="); ok {
			return strings.Trim(v, `"'`), nil
		}
	}
	return "", errors.New("no SLACK_USER_TOKEN in ~/.claude.secret")
}

// slackCall is one Web API call; its error never includes the token.
func slackCall(method string, params map[string]any, out any) error {
	token, err := slackToken()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(params)
	req, err := http.NewRequest("POST", slackBase+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := slackHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
	}
	if !res.OK {
		return fmt.Errorf("slack %s: %s", method, res.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// slackPost posts text as the user (to a user id through their DM) and
// returns the message's permalink, "" if Slack wouldn't give one.
func slackPost(to, text string) (string, error) {
	channel := to
	if to[0] == 'U' || to[0] == 'W' {
		var open struct {
			Channel struct {
				ID string `json:"id"`
			} `json:"channel"`
		}
		if err := slackCall("conversations.open", map[string]any{"users": to}, &open); err != nil {
			return "", err
		}
		channel = open.Channel.ID
	}
	var posted struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := slackCall("chat.postMessage", map[string]any{"channel": channel, "text": text, "unfurl_links": false, "unfurl_media": false}, &posted); err != nil {
		return "", err
	}
	q := url.Values{"channel": {posted.Channel}, "message_ts": {posted.TS}}
	var link struct {
		Permalink string `json:"permalink"`
	}
	if err := slackGet("chat.getPermalink?"+q.Encode(), &link); err != nil {
		logActivity("review: permalink: %v", err)
	}
	return link.Permalink, nil
}

// slackGet is a GET Web API call (chat.getPermalink takes no JSON body).
func slackGet(path string, out any) error {
	token, err := slackToken()
	if err != nil {
		return err
	}
	req, err := http.NewRequest("GET", slackBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := slackHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// withFooter is text as sent: ccs's attribution line last.
func withFooter(text string) string { return strings.TrimRight(text, "\n ") + "\n" + reviewFooter }

// writeReviewEvents appends events beside the watcher's.
func writeReviewEvents(events []reviewEvent) {
	var all []any
	for _, e := range events {
		all = append(all, e)
	}
	if err := writeEventLines(all); err != nil {
		logActivity("review: writing events: %v", err)
	}
}

// reviewTick is the review step after each watch pass: queue, and when
// due, plan, check, post and record. dryRun prints the plan for whatever
// is waiting now (debounce and quiet hours ignored) and records nothing.
func reviewTick(now time.Time, dryRun bool, stdout io.Writer) error {
	w, ok := loadPRWatchState()
	if !ok {
		return nil
	}
	var s reviewState
	if dryRun {
		s = loadReviewState()
		s.Queue = map[string]reviewQueued{}
		reviewQueueUpdate(&s, w, now)
	} else {
		if err := withReviewState(func(st *reviewState) bool {
			changed := reviewQueueUpdate(st, w, now)
			s = *st
			return changed
		}); err != nil {
			return err
		}
		if !reviewDue(s, now) {
			return nil
		}
	}
	if len(s.Queue) == 0 {
		if dryRun {
			fmt.Fprintln(stdout, "nothing waiting for review")
		}
		return nil
	}
	items := reviewBatch(s)
	plan, err := planReview(items)
	if err == nil {
		err = checkPlan(plan, items)
		if err != nil && dryRun {
			out, _ := json.MarshalIndent(plan, "", "  ")
			fmt.Fprintln(stdout, string(out))
		}
	}
	at := now.UTC().Format(time.RFC3339)
	if err != nil {
		if !dryRun {
			withReviewState(func(st *reviewState) bool { st.LastJoin = now; return true }) // retry after the debounce
			writeReviewEvents([]reviewEvent{{At: at, Event: "review_plan_failed", PRs: urlsOf(items), Reason: err.Error()}})
		}
		return fmt.Errorf("review plan: %w", err)
	}
	if dryRun {
		out, _ := json.MarshalIndent(struct {
			Batch []reviewItem `json:"batch"`
			Plan  reviewPlan   `json:"plan"`
		}{items, plan}, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return nil
	}

	heads := map[string]string{}
	for _, it := range items {
		heads[it.URL] = it.Head
	}
	var events []reviewEvent
	var permalink string
	var postErr error
	if c := plan.ChannelPost; c != nil {
		permalink, postErr = slackPost(reviewCfg.Channel, withFooter(c.Text))
		if postErr == nil {
			events = append(events, reviewEvent{At: at, Event: "review_requested", Channel: reviewCfg.Channel, PRs: c.PRs, Permalink: permalink})
		}
	}
	err = withReviewState(func(st *reviewState) bool {
		if c := plan.ChannelPost; c != nil && postErr == nil {
			for _, u := range c.PRs {
				st.Requested[reviewKey(u, heads[u])] = reviewAsked{At: now, How: "posted " + permalink}
				delete(st.Queue, u)
			}
		}
		for _, d := range plan.Drafts {
			st.NextDraftID++
			id := fmt.Sprintf("r%d", st.NextDraftID)
			text := withFooter(d.Text)
			var hs []string
			for _, u := range d.PRs {
				hs = append(hs, heads[u])
				st.Requested[reviewKey(u, heads[u])] = reviewAsked{At: now, How: "draft " + id}
				delete(st.Queue, u)
			}
			st.Drafts[id] = reviewDraft{ID: id, To: d.To, ToName: d.ToName, Text: text, PRs: d.PRs, Heads: hs, Created: now}
			events = append(events, reviewEvent{At: at, Event: "review_dm_pending", ID: id, Recipient: d.To + " (" + d.ToName + ")", Text: text, PRs: d.PRs})
		}
		for _, h := range plan.Holds {
			st.Held[reviewKey(h.PR, heads[h.PR])] = reviewAsked{At: now, How: "held", Reason: h.Reason}
			delete(st.Queue, h.PR)
			events = append(events, reviewEvent{At: at, Event: "review_held", PR: h.PR, Reason: h.Reason})
		}
		if postErr != nil {
			st.LastJoin = now // the post failed: its PRs stay queued for the next try
		}
		return true
	})
	writeReviewEvents(events)
	if postErr != nil {
		return fmt.Errorf("posting to #control-plane: %w", postErr)
	}
	return err
}

func urlsOf(items []reviewItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.URL)
	}
	return out
}

// runReview is `ccs review list|send <id>|drop <id>`.
func runReview(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ccs review list | send <id> | drop <id>\n\nDrafted review requests from ccs watch --review that wait for you to\nconfirm them: list shows them, send posts one as you, drop discards one.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	switch {
	case rest[0] == "list" && len(rest) == 1:
		s := loadReviewState()
		if len(s.Drafts) == 0 {
			fmt.Fprintln(stdout, "no drafts waiting")
			return 0
		}
		ids := make([]string, 0, len(s.Drafts))
		for id := range s.Drafts {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b string) int { return s.Drafts[a].Created.Compare(s.Drafts[b].Created) })
		for _, id := range ids {
			d := s.Drafts[id]
			fmt.Fprintf(stdout, "%s  to %s (%s)  %s\n%s\n\n", id, d.ToName, d.To, d.Created.Local().Format("02 Jan 15:04"), indentDraft(d.Text))
		}
		return 0
	case (rest[0] == "send" || rest[0] == "drop") && len(rest) == 2:
		id := rest[1]
		var d reviewDraft
		found := false
		withReviewState(func(s *reviewState) bool { d, found = s.Drafts[id]; return false })
		if !found {
			fmt.Fprintf(stderr, "no draft %s (ccs review list shows them)\n", id)
			return 1
		}
		at := time.Now().UTC().Format(time.RFC3339)
		if rest[0] == "send" {
			link, err := slackPost(d.To, d.Text)
			if err != nil {
				fmt.Fprintln(stderr, "ccs review send:", err)
				return 1
			}
			writeReviewEvents([]reviewEvent{{At: at, Event: "review_requested", Channel: d.To, PRs: d.PRs, Permalink: link, ID: id}})
			fmt.Fprintf(stdout, "sent %s to %s %s\n", id, d.ToName, link)
		} else {
			fmt.Fprintf(stdout, "dropped %s\n", id)
		}
		withReviewState(func(s *reviewState) bool { delete(s.Drafts, id); return true })
		return 0
	}
	fs.Usage()
	return 2
}

func indentDraft(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

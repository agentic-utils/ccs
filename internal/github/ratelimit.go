package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// GitHub's allowance is the user's, shared by every process that runs gh as
// them: ccs watch, each session's ccs prs and ccs mcp, and whatever else they
// run. Once a call is refused for the rate limit, nothing in ccs asks that
// resource again until it resets: the refusal is recorded in ghLimitPath, so
// every ccs process backs off, not only the one that was refused.

// RateLimitedError is a gh call refused (or not made) because the resource's
// allowance is spent until Until.
type RateLimitedError struct {
	Resource string
	Until    time.Time
	Err      error // GitHub's refusal; nil when the call wasn't made
}

func (e *RateLimitedError) Error() string {
	s := fmt.Sprintf("GitHub %s rate limit: not asking until %s", e.Resource, e.Until.UTC().Format("15:04:05 UTC"))
	if e.Err != nil {
		s += " (" + e.Err.Error() + ")"
	}
	return s
}

func (e *RateLimitedError) Unwrap() error { return e.Err }

// IsRateLimited says err is a rate-limit refusal or backoff.
func IsRateLimited(err error) bool {
	var rl *RateLimitedError
	return errors.As(err, &rl)
}

// ghLimitPath records, per resource, when ccs may ask GitHub again.
func ghLimitPath() string { return filepath.Join(filepath.Dir(state.SettingsPath), "gh-limit.json") }

const (
	// limitFallback is the backoff when GitHub's reset time can't be read,
	// or a secondary limit (which has none) was hit.
	limitFallback = time.Minute
	// limitMax caps a backoff: the primary limit resets within the hour.
	limitMax = time.Hour + time.Minute
)

// ghResource is the rate-limit bucket a gh command spends: search, graphql
// or core (the rest of the REST API).
func ghResource(args []string) string {
	if len(args) > 0 && args[0] == "search" {
		return "search"
	}
	if len(args) > 1 && args[0] == "api" {
		switch {
		case args[1] == "graphql":
			return "graphql"
		case strings.HasPrefix(args[1], "search/"):
			return "search"
		}
	}
	return "core"
}

func loadGHLimits() map[string]time.Time {
	var l map[string]time.Time
	b, err := os.ReadFile(ghLimitPath())
	if err != nil || json.Unmarshal(b, &l) != nil {
		return map[string]time.Time{}
	}
	return l
}

// limitedUntil is when resource may be asked again, if that's after now.
func limitedUntil(resource string, now time.Time) (time.Time, bool) {
	until, ok := loadGHLimits()[resource]
	// past, or beyond any real reset (a clock jump, a bad file): ignored, so
	// it can never hold for longer than limitMax
	if !ok || !until.After(now) || until.After(now.Add(limitMax)) {
		return time.Time{}, false
	}
	return until, true
}

// rateLimitRefusal says gh's error is GitHub refusing for a rate limit,
// primary ("API rate limit exceeded") or secondary.
func rateLimitRefusal(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate limit exceeded") || strings.Contains(s, "secondary rate limit")
}

// limitReset asks GitHub when resource resets. The rate_limit endpoint costs
// nothing against the allowance. A spent primary limit says its reset; a
// secondary limit (allowance left) or no answer backs off limitFallback.
func limitReset(resource string, now time.Time) time.Time {
	out, err := RunGH("api", "rate_limit")
	var r struct {
		Resources map[string]struct {
			Remaining int   `json:"remaining"`
			Reset     int64 `json:"reset"`
		} `json:"resources"`
	}
	if err != nil || json.Unmarshal(out, &r) != nil {
		return now.Add(limitFallback)
	}
	res, ok := r.Resources[resource]
	reset := time.Unix(res.Reset, 0)
	if !ok || res.Remaining > 0 || !reset.After(now) {
		return now.Add(limitFallback)
	}
	if reset.After(now.Add(limitMax)) {
		return now.Add(limitMax)
	}
	return reset
}

// noteRateLimit records that resource is limited until until, keeping a
// later time another process recorded. Written whole through a temp file;
// two processes racing can lose one entry, which costs one more refusal.
func noteRateLimit(resource string, until time.Time) {
	l := loadGHLimits()
	if l[resource].After(until) {
		return
	}
	l[resource] = until
	for k, t := range l {
		if !t.After(time.Now()) {
			delete(l, k)
		}
	}
	if err := writeJSON(ghLimitPath(), l); err != nil {
		state.LogActivity("gh rate limit: couldn't record the backoff: %v", err)
	}
}

// ghCall is RunGH that honours the shared backoff: while the resource is
// limited it fails at once without running gh, and a rate-limit refusal
// starts a backoff until GitHub's reset time.
func ghCall(args ...string) ([]byte, error) {
	resource := ghResource(args)
	if until, ok := limitedUntil(resource, time.Now()); ok {
		return nil, &RateLimitedError{Resource: resource, Until: until}
	}
	out, err := RunGH(args...)
	if err != nil && rateLimitRefusal(err) {
		until := limitReset(resource, time.Now())
		noteRateLimit(resource, until)
		state.LogActivity("gh rate limit: %s spent, not asking until %s", resource, until.UTC().Format("15:04:05 UTC"))
		return out, &RateLimitedError{Resource: resource, Until: until, Err: err}
	}
	return out, err
}

// writeJSON writes v to path whole, through a unique temp file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

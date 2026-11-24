package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/profiling"
)

// profileCall runs profile_ccs. It answers once the profile has started,
// never waiting out its length: a tool call gets about 10s.
func profileCall(raw json.RawMessage) (string, error) {
	var a struct {
		PID     int    `json:"pid"`
		Role    string `json:"role"`
		Kind    string `json:"kind"`
		Seconds int    `json:"seconds"`
		List    bool   `json:"list"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	if a.List {
		ts := profiling.Targets()
		if len(ts) == 0 {
			return "no running ccs takes profiles", nil
		}
		var lines []string
		for _, t := range ts {
			lines = append(lines, profiling.TargetLine(t))
		}
		return strings.Join(lines, "\n"), nil
	}
	kind, secs, err := profiling.CheckRequest(a.Kind, a.Seconds)
	if err != nil {
		return "", err
	}
	t, err := profiling.Pick(a.PID, a.Role)
	if err != nil {
		return "", err
	}
	s, err := profiling.Start(t.PID, kind, secs)
	if err != nil {
		return "", err
	}
	s.Close() // it stops by itself
	return profiling.Describe(t, s.Running, time.Now()), nil
}

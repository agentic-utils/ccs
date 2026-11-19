package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// TeamLead is the session id of the lead of team (a teammate's
// Conversation.Team): Claude Code writes it to the team's config.json under
// the Claude folder's teams/, as leadSessionId. "" when there's no such
// team or config. A team's lead never changes, so an answer is kept; a
// miss is asked again, since the config may not be written yet.
func TeamLead(team string) string {
	if team == "" || strings.ContainsAny(team, `/\`) || team == "." || team == ".." {
		return ""
	}
	// Keyed by the config's path: a team is the same team only in the same
	// teams folder.
	path := filepath.Join(TeamsDir(), team, "config.json")
	teamLeads.Lock()
	defer teamLeads.Unlock()
	if id, ok := teamLeads.m[path]; ok {
		return id
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		LeadSessionID string `json:"leadSessionId"`
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.LeadSessionID == "" {
		return ""
	}
	teamLeads.m[path] = cfg.LeadSessionID
	return cfg.LeadSessionID
}

// TeamsDir is Claude Code's teams folder, beside its projects folder.
var TeamsDir = func() string { return filepath.Join(filepath.Dir(GetProjectsDir()), "teams") }

var teamLeads = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// TeamSessionPrefix is the start of the lead's session id that a team named
// for its lead session ("session-f3356504") carries, for when its config is
// gone; "" for any other name.
func TeamSessionPrefix(team string) string {
	p, ok := strings.CutPrefix(team, "session-")
	if !ok || len(p) < 8 {
		return ""
	}
	for _, r := range p {
		if !strings.ContainsRune("0123456789abcdef-", r) {
			return ""
		}
	}
	return p
}

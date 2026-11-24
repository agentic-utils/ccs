package mcp

// A permission prompt's options, as Claude Code's own permission menu has
// them: Yes; "Yes, and don't ask again" for each permission update Claude
// Code suggested in the hook's input (permission_suggestions), when it is
// one ccs offers; and No, with an optional note telling Claude what to do
// instead. The TUI's menu and ccs serve's page both draw these, and
// promptDecision and the hook check a choice against them, so the three
// never disagree. The hook hands Claude Code back the suggestion it was
// given itself, by its index in its own input (updatedPermissions), never
// one sent to it.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// The choices a permission prompt takes. A "don't ask again" choice is
// alwaysPrefix and the suggestion's 1-based index in permission_suggestions.
const (
	choiceAllow  = "allow"
	choiceDeny   = "deny"
	alwaysPrefix = "always:"
)

// HeldOption is one row of a permission prompt's menu: what to send
// (Choice: allow, always:<n> or deny) and how Claude Code words it.
type HeldOption struct {
	Choice string `json:"choice"`
	Label  string `json:"label"`
}

// permSuggestion is the part of a permission update ccs reads to decide
// whether to offer it and how to word it.
type permSuggestion struct {
	Type  string `json:"type"`
	Rules []struct {
		ToolName    string `json:"toolName"`
		RuleContent string `json:"ruleContent"`
	} `json:"rules"`
	Behavior    string   `json:"behavior"`
	Destination string   `json:"destination"`
	Mode        string   `json:"mode"`
	Directories []string `json:"directories"`
}

// offeredModes are the permission modes a suggestion may switch to. Never
// bypassPermissions or dontAsk: neither is a "yes to this", and a phone tap
// must never turn every prompt off.
var offeredModes = map[string]bool{"acceptEdits": true, "default": true, "plan": true, "auto": true}

// destinations are where Claude Code can save an update, as a menu says it.
var destinations = map[string]string{
	"session":         "this session",
	"localSettings":   "this project",
	"projectSettings": "this project, for everyone",
	"userSettings":    "every project",
}

// suggestionLabel is how a suggestion reads as a menu row, or "" when ccs
// doesn't offer it: allow rules, a switch to an ordinary mode, or more
// working folders, each saved to a destination Claude Code names.
func suggestionLabel(raw json.RawMessage) string {
	var s permSuggestion
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	where, ok := destinations[s.Destination]
	if !ok {
		return ""
	}
	clean := func(t string) string { return strings.TrimSpace(transcript.CleanText(t)) }
	switch s.Type {
	case "addRules":
		if s.Behavior != "allow" || len(s.Rules) == 0 {
			return ""
		}
		var rules []string
		for _, r := range s.Rules {
			name := clean(r.ToolName)
			if name == "" {
				return ""
			}
			if c := clean(r.RuleContent); c != "" {
				name += "(" + c + ")"
			}
			rules = append(rules, name)
		}
		return fmt.Sprintf("Yes, and don't ask again for %s in %s", strings.Join(rules, ", "), where)
	case "setMode":
		if !offeredModes[s.Mode] {
			return ""
		}
		if s.Mode == "acceptEdits" {
			return "Yes, and allow all edits in " + where
		}
		return fmt.Sprintf("Yes, and switch to %s mode in %s", s.Mode, where)
	case "addDirectories":
		var dirs []string
		for _, d := range s.Directories {
			if d = clean(d); d != "" {
				dirs = append(dirs, d)
			}
		}
		if len(dirs) == 0 {
			return ""
		}
		return fmt.Sprintf("Yes, and allow access to %s in %s", strings.Join(dirs, ", "), where)
	}
	return ""
}

// denyLabel is the No row: picking it lets you say what to do instead.
const denyLabel = "No, and tell Claude what to do differently"

// permOptions are a permission prompt's menu rows, in Claude Code's order.
func permOptions(req hookRequest) []HeldOption {
	out := []HeldOption{{choiceAllow, "Yes"}}
	for i, raw := range req.PermissionSuggestions {
		if l := suggestionLabel(raw); l != "" {
			out = append(out, HeldOption{alwaysPrefix + strconv.Itoa(i+1), l})
		}
	}
	return append(out, HeldOption{choiceDeny, denyLabel})
}

// suggestionFor is the suggestion an always:<n> choice names in req's own
// input, and its label; ok is false for anything ccs doesn't offer.
func suggestionFor(req hookRequest, n int) (json.RawMessage, string, bool) {
	if req.ToolName == "AskUserQuestion" || n < 1 || n > len(req.PermissionSuggestions) {
		return nil, "", false
	}
	raw := req.PermissionSuggestions[n-1]
	l := suggestionLabel(raw)
	return raw, l, l != ""
}

// alwaysIndex reads an always:<n> choice; 0 when it isn't one.
func alwaysIndex(choice string) int {
	s, ok := strings.CutPrefix(choice, alwaysPrefix)
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// denyMessage is what a deny tells Claude: who denied it and, with a note,
// what they want instead (cleaned; "" leaves it out).
func denyMessage(who, note string) string {
	msg := "Denied from ccs by " + who
	if note = OtherText(note); note != "" {
		msg += ". The user says what to do instead: " + note
	}
	return msg
}

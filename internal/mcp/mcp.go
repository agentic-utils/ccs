// Package mcp is `ccs mcp`: an MCP server on stdin/stdout (newline-delimited
// JSON-RPC 2.0) that lets Claude find and read earlier sessions, list PRs and
// start or close sessions. Claude Code starts it on demand:
// `claude mcp add --scope user ccs -- ccs mcp`.
// ponytail: hand-rolled JSON-RPC (initialize, ping, tools/list, tools/call);
// switch to the official Go SDK if we ever need resources, prompts or HTTP.
package mcp

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

const mcpProtocol = "2025-06-18"

// mcpSource loads the conversations to search; each call rescans, and
// parseCache makes unchanged files free.
type mcpSource func() ([]transcript.Conversation, error)

// AfterCall, set by Main and never in tests, runs after each tools/call:
// transcript.DropParseCache, so an idle server holds a few MB rather than
// every parsed transcript (~620 MB each, one server per Claude session).
var AfterCall func()

func RunMCP(in io.Reader, out io.Writer, load mcpSource) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue // a notification (initialized, cancelled…): nothing to answer
		}
		result, rpcErr := mcpHandle(req.Method, req.Params, load)
		if AfterCall != nil && req.Method == "tools/call" {
			AfterCall()
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func mcpHandle(method string, params json.RawMessage, load mcpSource) (any, map[string]any) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		v := mcpProtocol
		if p.ProtocolVersion != "" && p.ProtocolVersion <= mcpProtocol {
			v = p.ProtocolVersion // the client's, if we're at least as new
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ccs", "version": state.Version},
			"instructions": "Search and read the user's earlier Claude Code sessions (from ~/.claude/projects). " +
				"Use search_sessions to find a past conversation by what was said in it, list_sessions for recent ones, " +
				"and read_session to read one. Sessions are the user's private history; quote only what's relevant. " +
				"launch_session starts a named session in a new iTerm tab or tmux window and close_session stops one and closes its tab; restart_session stops one and resumes it in a new tab (your own once your turn ends). A session you launch is your child, and adopt_session moves one under you (or another session). " +
				"list_prs lists the user's open PRs with the session that owns each (or with merged, the recent merges and who merged them); claim_pr records a PR's owner and depends the order PRs must merge in.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, map[string]any{"code": -32602, "message": "invalid params"}
		}
		stats.Record(stats.Feature("mcp."+p.Name), "") // dropped unless a registered tool
		text, err := Call(p.Name, p.Arguments, load)
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
	}
	return nil, map[string]any{"code": -32601, "message": "method not found: " + method}
}

var mcpTools = []map[string]any{
	{
		"name":        "search_sessions",
		"description": "Find earlier Claude Code sessions whose messages, title, project path or session id contain every word of the query (case-insensitive). Sessions whose name contains the query words come first, then those with the most messages containing all the words, then the most recent, each with a matching snippet and how to resume it.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":            map[string]any{"type": "string", "description": "words to look for, all must appear"},
				"project":          map[string]any{"type": "string", "description": "only sessions whose working directory contains this"},
				"max_results":      map[string]any{"type": "integer", "description": "default 10, at most 50"},
				"include_headless": map[string]any{"type": "boolean", "description": "also one-shot runs nobody talked to (claude -p with a single prompt, codex exec); default false"},
			},
			"required": []string{"query"},
		},
	},
	{
		"name":        "list_sessions",
		"description": "List the most recently active Claude Code sessions, newest first. Each says its kind (claude or codex), session id, project folder, last message time and the command that resumes it, whether or not it is running, and its parent: the session that launched it through launch_session or ccs launch, or adopted it with adopt_session. A session that is running also says what it is doing (as ccs sessions): busy, idle or waiting on the user and since when, the question or permission prompt it waits on, whether its last message asks something, its last API error, its PRs, and the background tasks and subagents it runs. live_only keeps just the running ones.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project":          map[string]any{"type": "string", "description": "only sessions whose working directory contains this"},
				"limit":            map[string]any{"type": "integer", "description": "default 20, at most 100"},
				"live_only":        map[string]any{"type": "boolean", "description": "only the sessions running now; default false"},
				"parent":           map[string]any{"type": "string", "description": "only the sessions whose parent (the session that launched or adopted them) has this name"},
				"include_headless": map[string]any{"type": "boolean", "description": "also one-shot runs nobody talked to (claude -p with a single prompt, codex exec); default false"},
			},
		},
	},
	{
		"name":        "session_triage",
		"description": "Each running session in exactly one bucket, with a one-line reason (as ccs sessions --triage --json): waiting_on_user (a menu, permission prompt or question waits on the user, its last message asks something or asks them to run a command, or its open PR is ready to merge and needs their go), working (mid-turn), waiting_on_external (an open PR it owns with checks running, review required or a recorded depends wait; a background task or Monitor running; a subagent running; a ScheduleWakeup or CronCreate wake-up pending), done (no open PRs, nothing pending, and its last message says it is finished), stalled (idle past stall_after_minutes with nothing pending, an open PR it should fix named in the reason) or idle (the same, not yet that long). Each row also has its parent (the session that launched or adopted it), its idle time and since when, its open PRs with their status (from ccs watch's saved state and the claims), and its next scheduled wake-up when known. Listed waiting_on_user first, then stalled, done, idle, waiting_on_external, working; the longest idle first within each.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"stall_after_minutes": map[string]any{"type": "integer", "description": "how long an idle session with nothing pending waits before it is stalled; default 15"},
				"project":             map[string]any{"type": "string", "description": "only sessions whose working directory contains this"},
				"parent":              map[string]any{"type": "string", "description": "only the sessions whose parent (the session that launched or adopted them) has this name"},
			},
		},
	},
	{
		"name":        "read_session",
		"description": "Read a session's messages (user and Claude text; tool calls are not included, except AskUserQuestion: each question with its numbered options, and the user's answer with any notes they added, are messages of their own where they were asked). Long messages are cut. Page with offset/limit; with query, only messages containing it (plus their neighbours) are returned.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "string", "description": "full id, or a unique prefix"},
				"query":      map[string]any{"type": "string", "description": "optional: only messages containing this, with one message of context either side"},
				"offset":     map[string]any{"type": "integer", "description": "index of the first message to return (default 0; negative counts from the end)"},
				"limit":      map[string]any{"type": "integer", "description": "default 40, at most 200"},
			},
			"required": []string{"session_id"},
		},
	},
	{
		"name":        "launch_session",
		"description": "Start a new named Claude Code (or codex) session in a new iTerm tab or tmux window, with a brief as its first prompt or resuming an earlier session, and wait until it runs. Returns its pid and tty, or says it is still starting (its tab is up but no session has registered, as while a folder-trust question waits there): that is not a failure, and the name is refused if launched again meanwhile. A new session's brief starts with the user's launch-preamble.md, when they keep one beside ccs's settings.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":     map[string]any{"type": "string", "description": "the session's name: letters, digits, '.', '_' and '-'"},
				"brief":    map[string]any{"type": "string", "description": "its first prompt"},
				"resume":   map[string]any{"type": "string", "description": "a session id to resume instead (or as well)"},
				"cwd":      map[string]any{"type": "string", "description": "absolute path of the folder to start in"},
				"codex":    map[string]any{"type": "boolean", "description": "start codex instead of claude"},
				"preamble": map[string]any{"type": "boolean", "description": "false to give the brief as it is, without the user's launch preamble; default true"},
			},
			"required": []string{"name", "cwd"},
		},
	},
	{
		"name":        "close_session",
		"description": "Stop the running session with this name, confirm its process exited, and close its iTerm tab or tmux pane. That stops it mid-whatever-it-was-doing; with graceful, a Claude Code session is first asked to wrap up (stop at a safe point, remove its worktrees, say what's left) and is stopped once it has gone idle after that, or when the timeout passes. With graceful the call returns as soon as the notice is sent, with the pid; the close goes on in the background, and list_sessions shows the session gone once it's done (read_session on it gives what it said was left). If the notice can't be sent nothing is stopped and the call fails.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":            map[string]any{"type": "string", "description": "the session's name"},
				"session_id":      map[string]any{"type": "string", "description": "optional: refuse unless the session with this name is still this session id (nothing is stopped otherwise)"},
				"graceful":        map[string]any{"type": "boolean", "description": "ask it to wrap up first and wait for it to go idle; default false. Not for Codex sessions"},
				"timeout_seconds": map[string]any{"type": "integer", "description": "with graceful: how long to wait before stopping it anyway; default 120, at most 900"},
			},
			"required": []string{"name"},
		},
	},
	{
		"name":        "adopt_session",
		"description": "Make a session the child of another, as if that one had launched it: the list then draws it in that orchestrator's tree, and list_sessions and session_triage give it as its parent. With no parent, the calling session adopts it; parent \"none\" detaches it. launch_session already does this for the sessions it starts (the latest launch or adoption wins), so this is for handing a session to another orchestrator or taking one over.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":   map[string]any{"type": "string", "description": "the child session's name"},
				"parent": map[string]any{"type": "string", "description": "the running session to put it under; default the calling session; \"none\" to detach it"},
			},
			"required": []string{"name"},
		},
	},
	{
		"name":        "restart_session",
		"description": "Restart the running Claude Code session with this name: stop it and resume the same conversation in a new iTerm tab or tmux window, with the same name, folder and the user's claude flags, to pick up a new ccs, Claude Code or plugin version or a flag it lacked. Returns as soon as the new tab is open. Refused while the session is mid-turn (working, or waiting on a menu or prompt) unless force, which stops it where it is. Called on your own session, it returns at once and the restart happens once your turn ends (or after timeout_seconds), so end the turn straight after. The resumed session's first message says it was restarted; its background tasks, Monitors and subagents don't survive.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":            map[string]any{"type": "string", "description": "the session's name"},
				"session_id":      map[string]any{"type": "string", "description": "optional: refuse unless the session with this name is still this session id (nothing is stopped otherwise)"},
				"force":           map[string]any{"type": "boolean", "description": "restart it even mid-turn; default false"},
				"timeout_seconds": map[string]any{"type": "integer", "description": "restarting your own session: how long to wait for your turn to end before stopping it anyway; default 120, at most 900"},
			},
			"required": []string{"name"},
		},
	},
	{
		"name":        "list_prs",
		"description": "The user's open PRs (as ccs prs --json): each with the live sessions that own it (ccs claim, or ccs watch for the session that opened it), the session that opened it or that none here did (opened_by, origin), the PRs it must merge after that haven't merged (blocked_by, from ccs depends), its group (ready, needs_you, waiting) and status, and flags for PRs nobody owns and PRs two sessions claim. With merged, the PRs ccs watch saw merge in the last week instead (as ccs prs --merged --json), newest first, each with how it merged (merge: local, auto_merge or not_local; empty when ccs watch couldn't tell) and the session that merged it or turned on auto-merge (merged_by: name and session id).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"merged": map[string]any{"type": "boolean", "description": "list the PRs merged in the last week instead of the open ones; default false"},
			},
		},
	},
	{
		"name":        "claim_pr",
		"description": "Record that a running session owns a PR (as ccs claim), so ccs watch tells it of the PR's failing checks, conflicts and new review threads and list_prs names it as the owner. Without session, the claim is yours; with session, it's for the running session of that name (an orchestrator claiming for a session it runs). Another session's claim on the PR stays beside it, and two owners show as contested in list_prs. Claims end when the PR merges or closes, or the session stops. With release, the session's claim is dropped instead, and ccs watch won't claim it back for it.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pr":      map[string]any{"type": "string", "description": "the PR's address, https://github.com/owner/repo/pull/N"},
				"session": map[string]any{"type": "string", "description": "optional: the name of the running session that owns it; default your own session"},
				"release": map[string]any{"type": "boolean", "description": "drop the claim instead; default false"},
			},
			"required": []string{"pr"},
		},
	},
	{
		"name":        "depends",
		"description": "The recorded merge order (as ccs depends): with pr and after, record that pr must not merge before each PR in after has (added to what it already waits for; a PR waiting for itself or a circle is refused); with pr and drop, forget what pr waits for; with neither, list every record as JSON (pr, after, waiting: those of after not known to have merged, by: the session that recorded it). ccs watch holds a waiting PR's ready event, list_prs shows it in blocked_by, and blocked_automerge and merged_early events say when auto-merge is on for it or it merged first. A wait on a PR closed without merging is dropped by ccs watch (a dependency_dropped event); until then the list's closed field and list_prs' waits_on_closed name it. ccs refuses no merge itself. A PR is its address, owner/repo#N, or repo#N when ccs knows one repo of that name.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pr":    map[string]any{"type": "string", "description": "the PR that must wait"},
				"after": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "the PRs that must merge first"},
				"drop":  map[string]any{"type": "boolean", "description": "forget what pr waits for instead; default false"},
			},
		},
	},
	{
		"name":        "orchestrator_report",
		"description": "Each orchestrator's scorecard (as ccs orchestrator-report --json): the session ccs orchestrator set up and any session that launched another, each over the sessions under it. Per orchestrator: response (minutes from an event that needs it, a session stalled or done, a PR ready, merged early or blocked from auto-merge, or a failing PR nobody owns, to its first message or tool call naming it; missed past 4 hours), stalled minutes and the worst session, done-to-close minutes, the user's messages to it that nudge about idle sessions or PRs (each a failure) and that correct it, with the matching lines, its hands-on tool calls (work it should hand off) and ccs commands run through Bash that an MCP tool covers, PRs merged and ready-to-merge minutes, its sessions running now and how many are stalled, done or idle, span breaches (over 6 direct sessions, deeper than 2), and the previous saved day's numbers. Saves the scorecards as today's unless save is false.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"since_hours":  map[string]any{"type": "integer", "description": "the period: this many hours up to now; default 24"},
				"orchestrator": map[string]any{"type": "string", "description": "only this orchestrator (a session name); default every one"},
				"save":         map[string]any{"type": "boolean", "description": "save the scorecards as today's; default true"},
			},
		},
	},
	{
		"name":        "child_notices",
		"description": "Turn child-turn notices on or off for an orchestrator (as ccs hook child-notices on|off). With ccs's Stop hook installed (ccs hook install), each time a session you launched or adopted finishes a turn you get an automatic notice from \"ccs hook\": its name, its bucket now (waiting_on_user, waiting_on_external, done or idle), its last message cut to 300 characters and its open PRs, and a child_turn line goes to pr-events.jsonl. Turns started only by an automatic ccs notice, turns that ended with a SendMessage to you, and a repeat in the same bucket within 30 seconds aren't told. On by default.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"enabled":      map[string]any{"type": "boolean", "description": "true to be told, false to stop"},
				"orchestrator": map[string]any{"type": "string", "description": "the orchestrator's session name; default your own session"},
			},
			"required": []string{"enabled"},
		},
	},
}

// childNoticesCall runs child_notices.
func childNoticesCall(raw json.RawMessage) (string, error) {
	var a struct {
		Enabled      *bool  `json:"enabled"`
		Orchestrator string `json:"orchestrator"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Enabled == nil {
		return "", errors.New("enabled (true or false) is required")
	}
	name := strings.TrimSpace(a.Orchestrator)
	if name == "" {
		c, ok := sessions.CallingSession()
		if !ok || c.Name == "" {
			return "", errors.New("couldn't tell which session is calling; pass orchestrator")
		}
		name = c.Name
	}
	if err := SetChildNotices(name, *a.Enabled, time.Now()); err != nil {
		return "", err
	}
	return fmt.Sprintf("child-turn notices %s for %s", map[bool]string{true: "on", false: "off"}[*a.Enabled], name), nil
}

// reportCall runs orchestrator_report.
func reportCall(raw json.RawMessage) (string, error) {
	var a struct {
		Since        int    `json:"since_hours"`
		Orchestrator string `json:"orchestrator"`
		Save         *bool  `json:"save"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	now := time.Now()
	cards, err := BuildReport(ReportOpts{From: now.Add(-time.Duration(clampInt(a.Since, 24, 24*14)) * time.Hour), To: now, Orchestrator: strings.TrimSpace(a.Orchestrator), Now: now})
	if err != nil {
		return "", err
	}
	return reportJSON(withPrevious(cards, a.Save == nil || *a.Save)), nil
}

// prCall runs list_prs, claim_pr and depends, which need no transcripts.
func prCall(name string, raw json.RawMessage) (string, error) {
	var a struct {
		Merged  bool     `json:"merged"`
		PR      string   `json:"pr"`
		Session string   `json:"session"`
		Release bool     `json:"release"`
		After   []string `json:"after"`
		Drop    bool     `json:"drop"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	if name == "claim_pr" {
		if a.PR == "" {
			return "", errors.New("pr is required")
		}
		return github.ClaimAs(strings.TrimSpace(a.PR), strings.TrimSpace(a.Session), a.Release, time.Now())
	}
	var args []string
	run := github.RunPRs
	switch name {
	case "list_prs":
		args = []string{"--json"}
		if a.Merged {
			args = append(args, "--merged")
		}
	case "depends":
		run = github.RunDepends
		switch {
		case a.PR == "" && len(a.After) == 0 && !a.Drop:
			args = []string{"--json"}
		case a.PR == "":
			return "", errors.New("pr is required with after or drop")
		case a.Drop && len(a.After) > 0:
			return "", errors.New("give after or drop, not both")
		case !a.Drop && len(a.After) == 0:
			return "", errors.New("give the PRs it waits for in after, or drop to forget them")
		default:
			args = []string{a.PR}
			if a.Drop {
				args = append(args, "--drop")
			}
			for _, u := range a.After {
				args = append(args, "--after", u)
			}
		}
	}
	var out, errs strings.Builder
	if run(args, &out, &errs) != 0 {
		return "", errors.New(cmp.Or(strings.TrimSpace(errs.String()), name+" failed"))
	}
	return out.String(), nil
}

// mcpSessionCall runs launch_session, close_session and restart_session,
// which need no transcripts.
func mcpSessionCall(name string, raw json.RawMessage) (string, error) {
	var a struct {
		Name, Brief, Resume, Cwd, Parent string
		Codex                            bool
		Preamble                         *bool
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	if name == "close_session" {
		return closeCall(a.Name, raw)
	}
	if name == "restart_session" {
		return restartCall(a.Name, raw)
	}
	if name == "adopt_session" {
		return sessions.Adopt(a.Name, a.Parent)
	}
	if !filepath.IsAbs(a.Cwd) {
		return "", fmt.Errorf("cwd must be an absolute path")
	}
	l, err := sessions.LaunchSession(sessions.LaunchOpts{Name: a.Name, Brief: a.Brief, Resume: a.Resume, Cwd: a.Cwd, Codex: a.Codex, NoPreamble: a.Preamble != nil && !*a.Preamble, RecordParent: true})
	if err != nil {
		return "", err
	}
	return l.Text(a.Name), nil
}

// closeCall is close_session: at once, or with graceful after asking the
// session to wrap up.
func closeCall(name string, raw json.RawMessage) (string, error) {
	var a struct {
		Graceful bool   `json:"graceful"`
		Timeout  int    `json:"timeout_seconds"`
		ID       string `json:"session_id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	if !a.Graceful {
		return sessions.CloseByNameID(name, a.ID)
	}
	// The wrap-up takes minutes and a tool call has far less, so this answers
	// once the notice is sent, as restart_session answers once its tab is up;
	// the wait and the close go on here in the background (this server lives
	// as long as the session that called it), the outcome to the activity log.
	timeout := time.Duration(clampInt(a.Timeout, 120, 900)) * time.Second
	type result struct {
		text string
		err  error
	}
	sent := make(chan int, 1)
	done := make(chan result, 1)
	go func() {
		text, err := closeGracefully(name, a.ID, timeout, func(pid int) { sent <- pid })
		done <- result{text, err}
	}()
	select {
	case r := <-done:
		return r.text, r.err
	case pid := <-sent:
		go func() {
			r := <-done
			if r.err != nil {
				state.LogActivity("close_session %s: %v", name, r.err)
			} else {
				state.LogActivity("close_session %s: %s", name, r.text)
			}
		}()
		return fmt.Sprintf("asked %s (pid %d) to wrap up: it will be stopped and its tab closed once it goes idle, or after %s. list_sessions shows it gone once it is, and read_session on it gives what it said was left", name, pid, timeout), nil
	}
}

// closeGracefully is sessions.CloseGracefullyNotifyID, stubbed in tests.
var closeGracefully = sessions.CloseGracefullyNotifyID

// restartSession is sessions.RestartSession, stubbed in tests.
var restartSession = sessions.RestartSession

// restartCall is restart_session. It returns once the new tab is open; the
// wait for the resumed session to register goes on here in the background
// (this server lives as long as the session that called it) and its outcome
// goes to the activity log.
func restartCall(name string, raw json.RawMessage) (string, error) {
	var a struct {
		Force   bool   `json:"force"`
		Timeout int    `json:"timeout_seconds"`
		ID      string `json:"session_id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	type result struct {
		text string
		err  error
	}
	tab := make(chan string, 1)
	done := make(chan result, 1)
	go func() {
		text, err := restartSession(sessions.RestartOpts{
			Name: name, ID: a.ID, Force: a.Force,
			Timeout: time.Duration(clampInt(a.Timeout, 120, 900)) * time.Second,
			OnTab:   func(tty string) { tab <- tty },
		})
		done <- result{text, err}
	}()
	select {
	case r := <-done:
		return r.text, r.err
	case tty := <-tab:
		go func() {
			if r := <-done; r.err != nil {
				state.LogActivity("restart_session %s: %v", name, r.err)
			}
		}()
		return fmt.Sprintf("stopped %s and opened its new tab on %s, where it is resuming the same conversation: list_sessions shows it once it's up", name, tty), nil
	}
}

func Call(name string, raw json.RawMessage, load mcpSource) (string, error) {
	if name == "launch_session" || name == "close_session" || name == "restart_session" || name == "adopt_session" {
		return mcpSessionCall(name, raw)
	}
	if name == "list_prs" || name == "claim_pr" || name == "depends" {
		return prCall(name, raw)
	}
	if name == "orchestrator_report" {
		return reportCall(raw)
	}
	if name == "child_notices" {
		return childNoticesCall(raw)
	}
	var a struct {
		Query      string `json:"query"`
		Project    string `json:"project"`
		MaxResults int    `json:"max_results"`
		Limit      int    `json:"limit"`
		Offset     int    `json:"offset"`
		SessionID  string `json:"session_id"`
		Headless   bool   `json:"include_headless"`
		LiveOnly   bool   `json:"live_only"`
		StallAfter int    `json:"stall_after_minutes"`
		Parent     string `json:"parent"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
	}
	if name == "session_triage" || name == "list_sessions" && a.LiveOnly {
		load = liveOr(load)
	}
	convs, err := load()
	if err != nil {
		return "", fmt.Errorf("couldn't read sessions: %v", err)
	}
	if a.Project != "" {
		p := strings.ToLower(a.Project)
		kept := convs[:0:0]
		for _, c := range convs {
			if strings.Contains(strings.ToLower(c.Cwd), p) {
				kept = append(kept, c)
			}
		}
		convs = kept
	}
	if name == "session_triage" {
		stall := DefaultStallAfter
		if a.StallAfter > 0 {
			stall = time.Duration(a.StallAfter) * time.Minute
		}
		rows := TriageRows(convs, liveSessions(), time.Now(), stall)
		if a.Project != "" {
			p := strings.ToLower(a.Project)
			kept := rows[:0]
			for _, r := range rows {
				if strings.Contains(strings.ToLower(r.Cwd), p) {
					kept = append(kept, r)
				}
			}
			rows = kept
		}
		if a.Parent != "" {
			kept := rows[:0]
			for _, r := range rows {
				if r.Parent == a.Parent {
					kept = append(kept, r)
				}
			}
			rows = kept
		}
		return triageJSON(rows, stall), nil
	}
	if name != "read_session" && !a.Headless { // read_session reads any session by id
		kept := convs[:0:0]
		for _, c := range convs {
			if !c.Headless() {
				kept = append(kept, c)
			}
		}
		convs = kept
	}
	switch name {
	case "search_sessions":
		convs = slices.DeleteFunc(slices.Clone(convs), func(c transcript.Conversation) bool { return transcript.HideFromSearch(c, a.Query) })
		return mcpSearch(convs, a.Query, clampInt(a.MaxResults, 10, 50))
	case "list_sessions":
		live := liveSessions()
		if a.LiveOnly {
			kept := convs[:0:0]
			for _, c := range convs {
				if _, ok := live[c.SessionID]; ok {
					kept = append(kept, c)
				}
			}
			convs = kept
		}
		parents := sessions.ParentNamesLoaded(convs)
		if a.Parent != "" {
			kept := convs[:0:0]
			for _, c := range convs {
				if parents[c.SessionID] == a.Parent {
					kept = append(kept, c)
				}
			}
			convs = kept
		}
		return mcpList(convs, live, parents, clampInt(a.Limit, 20, 100)), nil
	case "read_session":
		return mcpRead(convs, a.SessionID, a.Query, a.Offset, clampInt(a.Limit, 40, 200))
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func clampInt(v, def, hi int) int {
	if v <= 0 {
		return def
	}
	return min(v, hi)
}

// Header is one session's summary line for search and list results.
func Header(c transcript.Conversation) string {
	kind, resume := "claude", "claude --resume "
	if c.Kind == transcript.KindCodex {
		kind, resume = transcript.KindCodex, "codex resume "
	}
	return fmt.Sprintf("%s · %s\n  kind %s · session %s · project %s · last active %s (%s) · %d messages\n  resume: cd %s && %s%s",
		transcript.GetTopic(c), transcript.ShortModel(c), kind, c.SessionID, c.Cwd, transcript.FormatTimestamp(c.LastTimestamp), transcript.FormatAgo(c.LastTimestamp, time.Now()),
		len(c.Messages), c.Cwd, resume, c.SessionID)
}

func mcpSearch(convs []transcript.Conversation, query string, n int) (string, error) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return "", fmt.Errorf("query is empty")
	}
	type hit struct {
		c       transcript.Conversation
		rank    int // matchRank: named after the query, then the words as a phrase, then the rest
		hits    int
		snippet string
	}
	var found []hit
	for _, c := range convs {
		lower := c.SearchLower
		if lower == "" {
			lower = strings.ToLower(transcript.SearchTextOf(c))
		}
		if !transcript.QueryMatches(lower, transcript.GetTopic(c), terms) {
			continue
		}
		h := hit{c: c, rank: transcript.MatchRank(lower, transcript.GetTopic(c), terms)}
		for _, m := range c.Messages {
			if transcript.ContainsAll(strings.ToLower(m.Text), terms) {
				h.hits++
				if h.snippet == "" {
					h.snippet = snippetAround(transcript.CleanText(m.Text), terms[0], 240)
				}
			}
		}
		found = append(found, h)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].rank != found[j].rank {
			return found[i].rank < found[j].rank
		}
		if found[i].hits != found[j].hits {
			return found[i].hits > found[j].hits
		}
		return found[i].c.LastTimestamp > found[j].c.LastTimestamp
	})
	if len(found) == 0 {
		return fmt.Sprintf("No sessions contain all of: %s", query), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d sessions match %q", len(found), query)
	if len(found) > n {
		fmt.Fprintf(&b, " (showing %d)", n)
	}
	b.WriteString("\n")
	for i, h := range found[:min(n, len(found))] {
		fmt.Fprintf(&b, "\n%d. %s\n  %d matching messages", i+1, Header(h.c), h.hits)
		if h.snippet != "" {
			fmt.Fprintf(&b, ", e.g.: %s", h.snippet)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func mcpList(convs []transcript.Conversation, live map[string]sessions.LiveSession, parents map[string]string, n int) string {
	now := time.Now()
	sort.SliceStable(convs, func(i, j int) bool { return convs[i].LastTimestamp > convs[j].LastTimestamp })
	var b strings.Builder
	fmt.Fprintf(&b, "%d sessions", len(convs))
	if len(convs) > n {
		fmt.Fprintf(&b, " (newest %d)", n)
	}
	b.WriteString("\n")
	for i, c := range convs[:min(n, len(convs))] {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, Header(c))
		if p := parents[c.SessionID]; p != "" {
			fmt.Fprintf(&b, "  parent: %s\n", p)
		}
		if l, ok := live[c.SessionID]; ok {
			b.WriteString(sessionRow(c, l, true, now).liveLine(now) + "\n")
		}
	}
	return b.String()
}

const mcpMessageRunes = 4000

func mcpRead(convs []transcript.Conversation, id, query string, offset, limit int) (string, error) {
	if id == "" {
		return "", fmt.Errorf("session_id is required")
	}
	var match []transcript.Conversation
	for _, c := range convs {
		if c.SessionID == id {
			match = []transcript.Conversation{c}
			break
		}
		if strings.HasPrefix(c.SessionID, id) {
			match = append(match, c)
		}
	}
	switch {
	case len(match) == 0:
		return "", fmt.Errorf("no session %q (it may be older than the search window; ccs mcp --all searches everything)", id)
	case len(match) > 1:
		return "", fmt.Errorf("%q matches %d sessions; give more of the id", id, len(match))
	}
	c := match[0]
	entries := readEntries(c)
	idx := make([]int, 0, len(entries))
	if q := strings.ToLower(strings.TrimSpace(query)); q != "" {
		keep := map[int]bool{}
		for i, e := range entries {
			if strings.Contains(strings.ToLower(e.text), q) {
				keep[i-1], keep[i], keep[i+1] = true, true, true
			}
		}
		for i := range entries {
			if keep[i] {
				idx = append(idx, i)
			}
		}
	} else {
		for i := range entries {
			idx = append(idx, i)
		}
	}
	if offset < 0 {
		offset = max(len(idx)+offset, 0)
	}
	offset = min(offset, len(idx))
	page := idx[offset:min(offset+limit, len(idx))]

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", Header(c))
	fmt.Fprintf(&b, "messages %d-%d of %d", offset+1, offset+len(page), len(idx))
	if query != "" {
		fmt.Fprintf(&b, " matching %q (with context)", query)
	}
	if offset+len(page) < len(idx) {
		fmt.Fprintf(&b, "; more with offset=%d", offset+len(page))
	}
	b.WriteString("\n")
	for _, i := range page {
		e := entries[i]
		if e.msg < 0 {
			fmt.Fprintf(&b, "\n[%d] %s %s:\n%s\n", i, transcript.FormatTimestamp(e.ts), e.who, cutText(e.text))
			continue
		}
		m := c.Messages[e.msg]
		who, text := "Claude", transcript.CleanText(m.Text)
		if m.Role == "sent" {
			who = "Claude, to " + transcript.ShortAddr(m.To)
		}
		if ev, ok := transcript.ChannelParts(text); ok && m.Role == "channel" {
			who, text = "Note", ev.Line()
			if ev.Link != "" {
				text += "\n" + ev.Link
			}
			if ev.Detail != "" {
				text += "\n" + ev.Detail
			}
		}
		if m.Role == "user" {
			who = "User"
			if from, body, ok := transcript.PeerParts(text); ok {
				who, text = "From "+from, body
			} else if tag, summary, ok := transcript.HarnessNote(text); ok {
				who, text = "Note", tag+" · "+summary
			}
		}
		fmt.Fprintf(&b, "\n[%d] %s %s:\n%s\n", i, transcript.FormatTimestamp(m.Ts), who, cutText(text))
	}
	return b.String(), nil
}

func cutText(text string) string {
	if r := []rune(text); len(r) > mcpMessageRunes {
		return string(r[:mcpMessageRunes]) + fmt.Sprintf("… (%d more characters)", len(r)-mcpMessageRunes)
	}
	return text
}

// readEntry is one item read_session lists: a message (msg its index), or
// part of an AskUserQuestion (msg -1), which the transcript keeps out of
// the messages.
type readEntry struct {
	msg           int
	who, text, ts string
}

// readEntries is c's messages with each AskUserQuestion put where it was
// asked: one entry per question with its options, then one per answer with
// the choice and the user's notes (or the result as Claude Code wrote it,
// when the transcript doesn't hold the choices). The query filter reads
// e.text, which for a message is its raw text.
func readEntries(c transcript.Conversation) []readEntry {
	// Questions go where they were asked and answers where their result
	// arrived, since a message can come in while the menu waits.
	type placed struct {
		at int // before message at
		e  readEntry
	}
	var extra []placed
	for _, a := range c.Asks {
		for _, q := range a.Questions {
			extra = append(extra, placed{a.At, readEntry{msg: -1, who: "Claude asks (AskUserQuestion)", text: q.Text(), ts: a.Ts}})
		}
		answer := func(who, text string) {
			extra = append(extra, placed{a.AnswerAt, readEntry{msg: -1, who: who, text: text, ts: a.AnswerTs}})
		}
		switch {
		case !a.Answered:
		case len(a.Answers) > 0:
			for _, ans := range a.Answers {
				answer("User answered", ans.Text())
			}
		case a.Failed:
			answer("User declined", cmp.Or(a.Result, "(no answer)"))
		default:
			answer("User answered", cmp.Or(a.Result, "(no answer)"))
		}
	}
	sort.SliceStable(extra, func(i, j int) bool { return extra[i].at < extra[j].at })
	out := make([]readEntry, 0, len(c.Messages)+len(extra))
	for i := 0; i <= len(c.Messages); i++ {
		for len(extra) > 0 && extra[0].at <= i {
			out = append(out, extra[0].e)
			extra = extra[1:]
		}
		if i < len(c.Messages) {
			out = append(out, readEntry{msg: i, text: c.Messages[i].Text})
		}
	}
	return out
}

// snippetAround returns about width characters of s centred on the first
// case-insensitive match of term, on one line.
func snippetAround(s, term string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	at := strings.Index(strings.ToLower(s), term)
	if at < 0 {
		return transcript.Truncate(s, width)
	}
	pos := len([]rune(s[:at]))
	start := max(pos-width/2, 0)
	end := min(start+width, len(r))
	out := string(r[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(r) {
		out += "…"
	}
	return out
}

// Main is `ccs mcp`: serve until stdin closes. Logs go to stderr, which
// Claude Code keeps out of the protocol stream.
func Main(cutoff time.Time, maxSize int64, excludeDirs []string) {
	load := func() ([]transcript.Conversation, error) {
		return transcript.GetConversations(cutoff, maxSize, excludeDirs)
	}
	UseLiveLoads()
	AfterCall = transcript.DropParseCache
	err := RunMCP(os.Stdin, os.Stdout, load)
	stats.Flush()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccs mcp:", err)
		os.Exit(1)
	}
}

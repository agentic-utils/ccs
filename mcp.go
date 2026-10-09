package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// `ccs mcp`: an MCP server on stdin/stdout (newline-delimited JSON-RPC 2.0)
// that lets Claude find and read earlier sessions. Claude Code starts it on
// demand: `claude mcp add --scope user ccs -- ccs mcp`.
// ponytail: hand-rolled JSON-RPC (initialize, ping, tools/list, tools/call);
// switch to the official Go SDK if we ever need resources, prompts or HTTP.

const mcpProtocol = "2025-06-18"

// mcpSource loads the conversations to search; each call rescans, and
// parseCache makes unchanged files free.
type mcpSource func() ([]Conversation, error)

func runMCP(in io.Reader, out io.Writer, load mcpSource) error {
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
			"serverInfo":      map[string]any{"name": "ccs", "version": version},
			"instructions": "Search and read the user's earlier Claude Code sessions (from ~/.claude/projects). " +
				"Use search_sessions to find a past conversation by what was said in it, list_sessions for recent ones, " +
				"and read_session to read one. Sessions are the user's private history; quote only what's relevant.",
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
		text, err := mcpCall(p.Name, p.Arguments, load)
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
				"query":       map[string]any{"type": "string", "description": "words to look for, all must appear"},
				"project":     map[string]any{"type": "string", "description": "only sessions whose working directory contains this"},
				"max_results": map[string]any{"type": "integer", "description": "default 10, at most 50"},
			},
			"required": []string{"query"},
		},
	},
	{
		"name":        "list_sessions",
		"description": "List the most recently active Claude Code sessions, newest first.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project": map[string]any{"type": "string", "description": "only sessions whose working directory contains this"},
				"limit":   map[string]any{"type": "integer", "description": "default 20, at most 100"},
			},
		},
	},
	{
		"name":        "read_session",
		"description": "Read a session's messages (user and Claude text; tool calls are not included). Long messages are cut. Page with offset/limit; with query, only messages containing it (plus their neighbours) are returned.",
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
}

func mcpCall(name string, raw json.RawMessage, load mcpSource) (string, error) {
	var a struct {
		Query      string `json:"query"`
		Project    string `json:"project"`
		MaxResults int    `json:"max_results"`
		Limit      int    `json:"limit"`
		Offset     int    `json:"offset"`
		SessionID  string `json:"session_id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %v", err)
		}
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
	switch name {
	case "search_sessions":
		return mcpSearch(convs, a.Query, clampInt(a.MaxResults, 10, 50))
	case "list_sessions":
		return mcpList(convs, clampInt(a.Limit, 20, 100)), nil
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

// mcpHeader is one session's summary line for search and list results.
func mcpHeader(c Conversation) string {
	return fmt.Sprintf("%s · %s\n  session %s · project %s · last active %s (%s) · %d messages\n  resume: cd %s && claude --resume %s",
		getTopic(c), shortModel(c), c.SessionID, c.Cwd, formatTimestamp(c.LastTimestamp), formatAgo(c.LastTimestamp, time.Now()),
		len(c.Messages), c.Cwd, c.SessionID)
}

func mcpSearch(convs []Conversation, query string, n int) (string, error) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return "", fmt.Errorf("query is empty")
	}
	type hit struct {
		c       Conversation
		named   bool // the query is in its name: what someone searching for a session usually means
		hits    int
		snippet string
	}
	var found []hit
	for _, c := range convs {
		lower := c.searchLower
		if lower == "" {
			lower = strings.ToLower(searchTextOf(c))
		}
		if !queryMatches(lower, getTopic(c), terms) {
			continue
		}
		h := hit{c: c, named: containsAll(nameWords(getTopic(c)), terms)}
		for _, m := range c.Messages {
			if containsAll(strings.ToLower(m.Text), terms) {
				h.hits++
				if h.snippet == "" {
					h.snippet = snippetAround(cleanText(m.Text), terms[0], 240)
				}
			}
		}
		found = append(found, h)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].named != found[j].named {
			return found[i].named
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
		fmt.Fprintf(&b, "\n%d. %s\n  %d matching messages", i+1, mcpHeader(h.c), h.hits)
		if h.snippet != "" {
			fmt.Fprintf(&b, ", e.g.: %s", h.snippet)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func mcpList(convs []Conversation, n int) string {
	sort.SliceStable(convs, func(i, j int) bool { return convs[i].LastTimestamp > convs[j].LastTimestamp })
	var b strings.Builder
	fmt.Fprintf(&b, "%d sessions", len(convs))
	if len(convs) > n {
		fmt.Fprintf(&b, " (newest %d)", n)
	}
	b.WriteString("\n")
	for i, c := range convs[:min(n, len(convs))] {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, mcpHeader(c))
	}
	return b.String()
}

const mcpMessageRunes = 4000

func mcpRead(convs []Conversation, id, query string, offset, limit int) (string, error) {
	if id == "" {
		return "", fmt.Errorf("session_id is required")
	}
	var match []Conversation
	for _, c := range convs {
		if c.SessionID == id {
			match = []Conversation{c}
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
	idx := make([]int, 0, len(c.Messages))
	if q := strings.ToLower(strings.TrimSpace(query)); q != "" {
		keep := map[int]bool{}
		for i, m := range c.Messages {
			if strings.Contains(strings.ToLower(m.Text), q) {
				keep[i-1], keep[i], keep[i+1] = true, true, true
			}
		}
		for i := range c.Messages {
			if keep[i] {
				idx = append(idx, i)
			}
		}
	} else {
		for i := range c.Messages {
			idx = append(idx, i)
		}
	}
	if offset < 0 {
		offset = max(len(idx)+offset, 0)
	}
	offset = min(offset, len(idx))
	page := idx[offset:min(offset+limit, len(idx))]

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", mcpHeader(c))
	fmt.Fprintf(&b, "messages %d-%d of %d", offset+1, offset+len(page), len(idx))
	if query != "" {
		fmt.Fprintf(&b, " matching %q (with context)", query)
	}
	if offset+len(page) < len(idx) {
		fmt.Fprintf(&b, "; more with offset=%d", offset+len(page))
	}
	b.WriteString("\n")
	for _, i := range page {
		m := c.Messages[i]
		who, text := "Claude", cleanText(m.Text)
		if m.Role == "user" {
			who = "User"
			if from, body, ok := peerParts(text); ok {
				who, text = "From "+from, body
			} else if tag, summary, ok := harnessNote(text); ok {
				who, text = "Note", tag+" · "+summary
			}
		}
		if r := []rune(text); len(r) > mcpMessageRunes {
			text = string(r[:mcpMessageRunes]) + fmt.Sprintf("… (%d more characters)", len(r)-mcpMessageRunes)
		}
		fmt.Fprintf(&b, "\n[%d] %s %s:\n%s\n", i, formatTimestamp(m.Ts), who, text)
	}
	return b.String(), nil
}

// snippetAround returns about width characters of s centred on the first
// case-insensitive match of term, on one line.
func snippetAround(s, term string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	at := strings.Index(strings.ToLower(s), term)
	if at < 0 {
		return truncate(s, width)
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

// mcpMain is `ccs mcp`: serve until stdin closes. Logs go to stderr, which
// Claude Code keeps out of the protocol stream.
func mcpMain(cutoff time.Time, maxSize int64, excludeDirs []string) {
	load := func() ([]Conversation, error) { return getConversations(cutoff, maxSize, excludeDirs) }
	if err := runMCP(os.Stdin, os.Stdout, load); err != nil {
		fmt.Fprintln(os.Stderr, "ccs mcp:", err)
		os.Exit(1)
	}
}

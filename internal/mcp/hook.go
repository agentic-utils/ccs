package mcp

// `ccs hook permission` is a Claude Code PermissionRequest hook: Claude Code
// runs it for each permission prompt and AskUserQuestion menu before it
// shows them, and a decision it prints (allow, with an AskUserQuestion's
// answers in updatedInput, or deny) settles the prompt without it ever
// appearing. It hands the prompt to a running `ccs serve --allow-write` over
// a local socket and prints whatever the page answers; with no ccs serve
// listening, or when ccs serve lets it go, it prints nothing and Claude Code
// shows the prompt in the terminal as usual. The answer goes back on the
// hook's own connection, so it can only ever settle the prompt that asked.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
)

// hookSockPath is where ccs serve listens for hooks: beside the settings,
// in ccs's own folder.
var hookSockPath = func() string { return filepath.Join(filepath.Dir(state.SettingsPath), "serve-hook.sock") }

// hookTimeout is the timeout `ccs hook install` gives the hook in Claude
// Code's settings (seconds): above maxHookWait, so ccs serve always lets the
// hook go first.
const hookTimeout = 1860

// maxHookWait is the longest ccs serve holds a prompt (--hook-wait).
const maxHookWait = 30 * time.Minute

// hookRequest is the part of Claude Code's hook input ccs uses.
type hookRequest struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	Event     string          `json:"hook_event_name"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// hookHello is the hook's first line: its prompt, and a nonce ccs serve
// must echo on every line it sends back.
type hookHello struct {
	Nonce   string      `json:"nonce"`
	Request hookRequest `json:"request"`
}

// hookReply is a line from ccs serve: first an acknowledgement (Held),
// then its answer. Behavior is "allow", "deny" or "" (let the prompt show
// in its tab); Answers are an AskUserQuestion's chosen labels by question.
// It never carries tool input: the hook builds what it hands Claude Code
// from its own input, so ccs serve (or anything posing as it) can't change
// a command.
type hookReply struct {
	Nonce    string            `json:"nonce"`
	Held     bool              `json:"held,omitempty"`
	Behavior string            `json:"behavior,omitempty"`
	Answers  map[string]string `json:"answers,omitempty"`
	Message  string            `json:"message,omitempty"`
}

// hookAckWait is how long the hook waits for ccs serve to acknowledge its
// prompt before letting it show in its tab.
var hookAckWait = 2 * time.Second

// RunHook is `ccs hook`.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: ccs hook permission | ccs hook install [--print] | ccs hook uninstall")
		return 2
	}
	switch args[0] {
	case "permission":
		permissionHook(stdin, stdout)
		return 0 // never fail the prompt: no output means Claude Code asks as usual
	case "install", "uninstall":
		path := claudeSettingsPath()
		if slices.Contains(args[1:], "--print") {
			b, _ := json.MarshalIndent(hookEntry(hookCommand()), "", "  ")
			fmt.Fprintf(stdout, "Add this under hooks.PermissionRequest in %s:\n%s\n", path, b)
			return 0
		}
		msg, err := editHookSettings(path, args[0] == "install")
		if err != nil {
			fmt.Fprintln(stderr, "ccs hook:", err)
			return 1
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	fmt.Fprintln(stderr, "ccs hook: unknown command", args[0])
	return 2
}

// permissionHook hands the prompt on stdin to ccs serve and prints the
// decision it makes of the answer. Anything unexpected (no ccs serve, a
// socket or folder that isn't this user's own, no acknowledgement in
// hookAckWait, a reply for another prompt, an answer that doesn't fit the
// question) prints nothing, so Claude Code asks in the tab as usual.
func permissionHook(stdin io.Reader, stdout io.Writer) {
	raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return
	}
	var req hookRequest
	if json.Unmarshal(raw, &req) != nil || req.Event != "PermissionRequest" || req.SessionID == "" || req.ToolName == "" {
		return
	}
	path := hookSockPath()
	if trustedSocket(path) != nil {
		return
	}
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return // no ccs serve answering prompts: the terminal asks
	}
	defer conn.Close()
	// The kernel says who is listening, whatever the socket file looked
	// like a moment ago: anything but this user isn't ccs serve.
	if uid, err := peerUID(conn); err != nil || uid != uint32(os.Getuid()) {
		return
	}
	nonce := newPromptID() + newPromptID()
	line, _ := json.Marshal(hookHello{Nonce: nonce, Request: req})
	conn.SetDeadline(time.Now().Add(hookAckWait))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var ack hookReply
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &ack) != nil || !ack.Held || ack.Nonce != nonce {
		return
	}
	conn.SetDeadline(time.Now().Add(maxHookWait + time.Minute))
	var reply hookReply
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &reply) != nil || reply.Nonce != nonce {
		return
	}
	decision := hookDecision(req, reply)
	if decision == nil {
		return
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": decision}})
	fmt.Fprintln(stdout, string(out))
}

// trustedSocket refuses a socket that isn't this user's own: its folder must
// be a real directory (not a symlink) owned by this user and writable by no
// one else, and the socket a real socket owned by this user that only they
// can use. Other macOS users are what this keeps out; a process running as
// this user could change Claude Code's settings or run claude itself, so
// it's beyond what any check here could stop (docs/remote.md).
func trustedSocket(path string) error {
	uid := uint32(os.Getuid())
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 || dir.Mode().Perm()&0o022 != 0 || ownerOf(dir) != uid {
		return fmt.Errorf("%s isn't a private folder of this user's", filepath.Dir(path))
	}
	sock, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if sock.Mode()&os.ModeSocket == 0 || sock.Mode().Perm()&0o077 != 0 || ownerOf(sock) != uid {
		return fmt.Errorf("%s isn't this user's private socket", path)
	}
	return nil
}

// hookDecision is what the hook prints for ccs serve's reply, built from the
// hook's own input: deny; allow with no input change for a permission
// prompt; allow for an AskUserQuestion only with an answer to each of its
// questions, each answer one of that question's options (several, joined
// by ", ", for a multi-select), put into the hook's own copy of the input.
// Anything else is nil: the prompt shows in its tab.
func hookDecision(req hookRequest, r hookReply) map[string]any {
	switch r.Behavior {
	case "deny":
		msg := strings.Map(func(c rune) rune {
			if c < 0x20 || c == 0x7f {
				return ' '
			}
			return c
		}, r.Message)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return map[string]any{"behavior": "deny", "message": msg}
	case "allow":
		if req.ToolName != "AskUserQuestion" {
			if len(r.Answers) > 0 {
				return nil
			}
			return map[string]any{"behavior": "allow"}
		}
		var input map[string]any
		var in askInput
		if json.Unmarshal(req.ToolInput, &input) != nil || json.Unmarshal(req.ToolInput, &in) != nil || len(in.Questions) == 0 || len(r.Answers) != len(in.Questions) {
			return nil
		}
		answers := map[string]any{}
		for _, q := range in.Questions {
			got, ok := r.Answers[q.Question]
			if !ok || got == "" {
				return nil
			}
			labels := map[string]bool{}
			for _, o := range q.Options {
				labels[o.Label] = true
			}
			picks := []string{got}
			if q.MultiSelect && !labels[got] {
				picks = strings.Split(got, ", ")
			}
			for _, p := range picks {
				if !labels[p] {
					return nil
				}
			}
			answers[q.Question] = got
		}
		input["answers"] = answers
		return map[string]any{"behavior": "allow", "updatedInput": input}
	}
	return nil
}

// claudeSettingsPath is Claude Code's user settings file.
var claudeSettingsPath = func() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "settings.json")
}

// hookCommand is the command the settings run: this ccs by its full path
// (Claude Code runs hooks without the user's shell functions).
func hookCommand() string {
	bin, err := exec.LookPath("ccs")
	if err != nil {
		if bin, err = os.Executable(); err != nil {
			bin = "ccs"
		}
	}
	return shellWord(bin) + " hook permission"
}

func shellWord(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func hookEntry(command string) map[string]any {
	return map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": command, "timeout": hookTimeout}}}
}

// isOurCommand: a hook command that is ccs's own `<path>/ccs hook permission`.
// Install and uninstall both go by it: exactly what install writes
// (hookCommand, whatever this binary is called), or any ccs's.
func isOurCommand(c string) bool {
	if c == hookCommand() {
		return true
	}
	bin, ok := strings.CutSuffix(c, " hook permission")
	bin = strings.Trim(bin, "'")
	return ok && (bin == "ccs" || strings.HasSuffix(bin, "/ccs"))
}

// isOurHook: a PermissionRequest group with ccs's hook in it.
func isOurHook(entry any) bool {
	m, _ := entry.(map[string]any)
	hs, _ := m["hooks"].([]any)
	for _, h := range hs {
		if hm, ok := h.(map[string]any); ok {
			if c, _ := hm["command"].(string); isOurCommand(c) {
				return true
			}
		}
	}
	return false
}

// withoutOurs drops ccs's hook from each group, keeping every other hook,
// and a group only when nothing is left in it.
func withoutOurs(entries []any) []any {
	var out []any
	for _, e := range entries {
		m, ok := e.(map[string]any)
		hs, _ := m["hooks"].([]any)
		if !ok || !isOurHook(e) {
			out = append(out, e)
			continue
		}
		var kept []any
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok {
				if c, _ := hm["command"].(string); isOurCommand(c) {
					continue
				}
			}
			kept = append(kept, h)
		}
		if len(kept) > 0 {
			m["hooks"] = kept
			out = append(out, m)
		}
	}
	return out
}

// editHookSettings adds (or removes) ccs's PermissionRequest hook in
// Claude Code's settings, keeping everything else as it was and a copy of
// the old file beside it.
func editHookSettings(path string, install bool) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	settings := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return "", fmt.Errorf("%s isn't valid JSON, so it was left alone: %v", path, err)
		}
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	entries, _ := hooks["PermissionRequest"].([]any)
	had := slices.ContainsFunc(entries, isOurHook)
	kept := withoutOurs(entries)
	switch {
	case install && had:
		return "ccs's PermissionRequest hook is already in " + path, nil
	case !install && !had:
		return "ccs's PermissionRequest hook isn't in " + path, nil
	}
	if install {
		kept = append(kept, hookEntry(hookCommand()))
	}
	if len(kept) == 0 {
		delete(hooks, "PermissionRequest")
	} else {
		hooks["PermissionRequest"] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	if len(raw) > 0 {
		if err := os.WriteFile(path+".before-ccs-hook", raw, 0o600); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := fmt.Sprintf("%s.tmp-%s", path, newPromptID())
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if install {
		return "added ccs's PermissionRequest hook to " + path + " (the old file is beside it, .before-ccs-hook). Sessions started from now on use it; restart running ones to pick it up.", nil
	}
	return "removed ccs's PermissionRequest hook from " + path, nil
}

func newPromptID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

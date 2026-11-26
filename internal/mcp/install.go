package mcp

// ccs mcp install: add the ccs MCP server to Claude Code, Codex or agy,
// through each agent's own `mcp add`, never by writing its config. It runs
// only when asked, says what it runs first, and leaves a server already
// named ccs as it is. The orchestrator's set-up uses the same checks
// (MissingMCP, ApplyMCP) for Claude Code and Codex.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/stats"
)

// AgentRun runs an agent CLI's mcp commands, returning stdout and stderr.
type AgentRun func(bin string, args ...string) ([]byte, error)

// RealAgentRun runs it detached, so it can't prompt, from the home folder,
// not wherever ccs was started: a repo's own ccs server is no use elsewhere.
func RealAgentRun(bin string, args ...string) ([]byte, error) {
	dir, _ := os.UserHomeDir()
	return proc.RunCommandIn(dir, 30*time.Second, nil, true, bin, args...)
}

// MCPFix is one agent's missing ccs MCP server: the command that adds it.
type MCPFix struct {
	Bin  string
	Args []string
}

func (f MCPFix) String() string { return f.Bin + " " + strings.Join(f.Args, " ") }

// CCSCommand is how an MCP config should start ccs: by name when it's on
// PATH (a Homebrew upgrade keeps that working), else this binary.
func CCSCommand() string {
	if _, err := exec.LookPath("ccs"); err == nil {
		return "ccs"
	}
	if self, err := os.Executable(); err == nil {
		return self
	}
	return "ccs"
}

// addArgs is the command that adds ccs to bin's MCP servers.
func addArgs(bin string) []string {
	switch bin {
	case "claude":
		return []string{"mcp", "add", "--scope", "user", "ccs", "--", CCSCommand(), "mcp"}
	case "codex":
		return []string{"mcp", "add", "ccs", "--", CCSCommand(), "mcp"}
	}
	return []string{"mcp", "add", "ccs", CCSCommand(), "mcp"} // agy: flags before the name, none needed
}

// MissingMCP says which of agents (claude, codex, agy) are installed and lack
// the ccs MCP server. One that can't say is left alone, with a note: never
// install over an unknown.
func MissingMCP(run AgentRun, lookBin func(string) (string, error), agents ...string) (fixes []MCPFix, notes []string) {
	for _, bin := range agents {
		if _, err := lookBin(bin); err != nil {
			continue
		}
		has, note := hasCCS(run, bin)
		if note != "" {
			notes = append(notes, note)
		}
		if has == hasNo {
			fixes = append(fixes, MCPFix{bin, addArgs(bin)})
		}
	}
	return fixes, notes
}

type hasState int

const (
	hasUnknown hasState = iota
	hasNo
	hasYes
)

// hasCCS asks bin whether it has a server named ccs.
func hasCCS(run AgentRun, bin string) (hasState, string) {
	switch bin {
	case "claude":
		out, err := run("claude", "mcp", "get", "ccs")
		switch {
		case err == nil:
			if !strings.Contains(string(out), "Connected") {
				return hasYes, "Claude Code has a ccs MCP server that isn't connected, left as it is (claude mcp get ccs says why)"
			}
			return hasYes, ""
		case strings.Contains(string(out), "No MCP server"):
			return hasNo, ""
		}
		return hasUnknown, fmt.Sprintf("couldn't tell whether Claude Code has the ccs MCP server: %v", err)
	case "codex":
		out, err := run("codex", "mcp", "list", "--json")
		var servers []struct {
			Name string `json:"name"`
		}
		if err != nil || json.Unmarshal(out, &servers) != nil {
			return hasUnknown, "couldn't tell whether Codex has the ccs MCP server"
		}
		for _, s := range servers {
			if s.Name == "ccs" {
				return hasYes, ""
			}
		}
		return hasNo, ""
	case "agy":
		// A table: NAME TYPE STATUS COMMAND/URL, one server a row.
		out, err := run("agy", "mcp", "list")
		lines := strings.Split(string(out), "\n")
		if err != nil || len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "NAME") {
			if err == nil && strings.TrimSpace(string(out)) != "" {
				return hasUnknown, "couldn't tell whether agy has the ccs MCP server: agy mcp list printed something unexpected"
			}
			if err == nil { // no servers at all, nothing printed
				return hasNo, ""
			}
			return hasUnknown, fmt.Sprintf("couldn't tell whether agy has the ccs MCP server: %v", err)
		}
		for _, l := range lines[1:] {
			if f := strings.Fields(l); len(f) > 0 && f[0] == "ccs" {
				return hasYes, ""
			}
		}
		return hasNo, ""
	}
	return hasUnknown, "ccs doesn't know how to add an MCP server to " + bin
}

// ApplyMCP adds what's missing and checks it took: Claude Code must report it
// connected, Codex and agy (which don't test servers) must list it.
func ApplyMCP(run AgentRun, fixes []MCPFix) (done []string, errs []string) {
	for _, f := range fixes {
		// Already there means another ccs got there since the plan: check it as any other.
		if out, err := run(f.Bin, f.Args...); err != nil && !strings.Contains(string(out), "already exists") {
			errs = append(errs, fmt.Sprintf("%s failed: %v: %s", f, err, strings.TrimSpace(string(out))))
			continue
		}
		if f.Bin == "agy" {
			if has, note := hasCCS(run, "agy"); has != hasYes {
				errs = append(errs, "added the ccs MCP server to agy, but agy mcp list doesn't show it "+note)
			} else {
				done = append(done, "ccs MCP server added to agy")
			}
			continue
		}
		args := []string{"mcp", "get", "ccs"}
		out, err := run(f.Bin, args...)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("added it to %s, but %s %s failed: %v", f.Bin, f.Bin, strings.Join(args, " "), err))
		case f.Bin == "claude" && !strings.Contains(string(out), "Connected"):
			errs = append(errs, "added the ccs MCP server to claude, but it doesn't report as connected: "+strings.TrimSpace(string(out)))
		case f.Bin == "claude":
			done = append(done, "ccs MCP server added to claude and connected")
		default:
			done = append(done, "ccs MCP server added to "+f.Bin)
		}
	}
	return done, errs
}

// installRun and installLookBin are what ccs mcp install runs; stubbed in tests.
var (
	installRun     AgentRun = RealAgentRun
	installLookBin          = sessions.LookBin
)

// RunInstall is `ccs mcp install --claude|--codex|--agy [--print]`: add the
// ccs MCP server to each agent named, through its own mcp add. Exit code.
func RunInstall(args []string, stdout, stderr io.Writer) int {
	stats.Record("cli.mcp_install", "")
	fs := flag.NewFlagSet("ccs mcp install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	claude := fs.Bool("claude", false, "add it to Claude Code (claude mcp add --scope user)")
	codex := fs.Bool("codex", false, "add it to Codex (codex mcp add)")
	agy := fs.Bool("agy", false, "add it to agy, Antigravity CLI (agy mcp add)")
	print := fs.Bool("print", false, "only print the commands that would run")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs mcp install --claude|--codex|--agy [--print]\n\nAdds the ccs MCP server to each agent named, through its own `mcp add`; a server already named ccs is left as it is. Sessions already running load it once restarted.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var agents []string
	for _, a := range []struct {
		on   bool
		name string
	}{{*claude, "claude"}, {*codex, "codex"}, {*agy, "agy"}} {
		if a.on {
			agents = append(agents, a.name)
		}
	}
	if len(agents) == 0 || fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	code := 0
	var fixes []MCPFix
	for _, a := range agents {
		if _, err := installLookBin(a); err != nil {
			fmt.Fprintf(stderr, "%s isn't on PATH: nothing changed for it\n", a)
			code = 1
			continue
		}
		has, note := hasCCS(installRun, a)
		if note != "" {
			fmt.Fprintln(stderr, note)
		}
		switch has {
		case hasYes:
			fmt.Fprintf(stdout, "%s already has an MCP server named ccs: left as it is\n", agentName(a))
		case hasNo:
			fixes = append(fixes, MCPFix{a, addArgs(a)})
		default:
			code = 1
		}
	}
	for _, f := range fixes {
		if *print {
			fmt.Fprintln(stdout, f)
			continue
		}
		fmt.Fprintln(stdout, "running: "+f.String())
	}
	if *print || len(fixes) == 0 {
		return code
	}
	done, errs := ApplyMCP(installRun, fixes)
	for _, d := range done {
		fmt.Fprintln(stdout, d+"; sessions already running load it once restarted")
	}
	for _, e := range errs {
		fmt.Fprintln(stderr, e)
		code = 1
	}
	return code
}

// agentName is how messages name an agent's CLI.
func agentName(bin string) string {
	switch bin {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	}
	return bin
}

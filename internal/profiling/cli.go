package profiling

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

const usage = `Usage: ccs profile [--pid N | --role tui|watch|mcp|serve] [--kind cpu|heap|trace] [--seconds 30] [--no-wait]
       ccs profile --list [--json]

Record a profile of a running ccs without touching it: by default the one
running ccs TUI, a %ds CPU profile. A CPU profile or trace stops by itself
(at most %ds); a heap profile is a snapshot. Prints the file it wrote, beside
ccs's update log (the newest %d kept). One profile at a time; only your own
processes can ask.
`

// Run is ccs profile.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stderr, usage, DefaultSeconds, MaxSeconds, Keep) }
	pid := fs.Int("pid", 0, "the ccs process to profile")
	role := fs.String("role", RoleTUI, "the kind of ccs process, when there is one of it")
	kind := fs.String("kind", CPU, "cpu, heap or trace")
	seconds := fs.Int("seconds", DefaultSeconds, "how long a cpu profile or trace records")
	noWait := fs.Bool("no-wait", false, "print the file's name at once and leave it recording")
	list := fs.Bool("list", false, "list the ccs processes that take profiles")
	asJSON := fs.Bool("json", false, "with --list, as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	if *list {
		ts := Targets()
		if *asJSON {
			if ts == nil {
				ts = []Target{}
			}
			b, _ := json.MarshalIndent(ts, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		if len(ts) == 0 {
			fmt.Fprintln(stderr, "no running ccs takes profiles")
			return 0
		}
		for _, t := range ts {
			fmt.Fprintln(stdout, TargetLine(t))
		}
		return 0
	}
	k, secs, err := CheckRequest(*kind, *seconds)
	if err != nil {
		fmt.Fprintln(stderr, "ccs profile:", err)
		return 2
	}
	t, err := Pick(*pid, *role)
	if err != nil {
		fmt.Fprintln(stderr, "ccs profile:", err)
		return 1
	}
	s, err := Start(t.PID, k, secs)
	if err != nil {
		fmt.Fprintln(stderr, "ccs profile:", err)
		return 1
	}
	if *noWait {
		s.Close()
		fmt.Fprintln(stdout, s.Path)
		if !s.Until.IsZero() {
			fmt.Fprintf(stderr, "recording a %s profile of ccs %s (pid %d) until %s; it stops by itself\n", k, t.Role, t.PID, s.Until.Format("15:04:05"))
		}
		return 0
	}
	if !s.Until.IsZero() {
		fmt.Fprintf(stderr, "recording a %s profile of ccs %s (pid %d) for %ds…\n", k, t.Role, t.PID, secs)
	}
	if err := s.Wait(); err != nil {
		fmt.Fprintln(stderr, "ccs profile:", err)
		return 1
	}
	fmt.Fprintln(stdout, s.Path)
	fmt.Fprintln(stderr, Hint(s.Running))
	return 0
}

// Hint is how to read a profile.
func Hint(r Running) string {
	if r.Kind == Trace {
		return "read it with: go tool trace " + r.Path
	}
	return "read it with: go tool pprof -top " + r.Path
}

// TargetLine describes a target on one line.
func TargetLine(t Target) string {
	line := fmt.Sprintf("%d  %s  v%s  up since %s", t.PID, t.Role, t.Version, t.Since.Format("Jan 2 15:04"))
	if r := t.Running; r != nil {
		line += fmt.Sprintf("  recording %s (%s)", r.Kind, r.By)
		if !r.Until.IsZero() {
			line += " until " + r.Until.Format("15:04:05")
		}
	}
	return line
}

// Describe is a started profile in a sentence, for the MCP tool, which
// answers at once rather than waiting.
func Describe(t Target, r Running, now time.Time) string {
	if r.Until.IsZero() {
		return fmt.Sprintf("wrote a %s profile of ccs %s (pid %d) to %s; %s", r.Kind, t.Role, t.PID, r.Path, Hint(r))
	}
	return fmt.Sprintf("recording a %s profile of ccs %s (pid %d) to %s for %s, until %s; it stops by itself, and the file is complete after that; then %s",
		r.Kind, t.Role, t.PID, r.Path, r.Until.Sub(now).Round(time.Second), r.Until.Format("15:04:05"), Hint(r))
}

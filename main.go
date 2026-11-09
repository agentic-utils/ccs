// Command ccs searches and resumes Claude Code and Codex sessions.
package main

import "github.com/agentic-utils/ccs/internal/tui"

// version is set at build time (-X main.version).
var version = "dev"

func main() {
	tui.Main(version)
}

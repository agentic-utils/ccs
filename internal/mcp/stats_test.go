package mcp

import (
	"testing"

	"github.com/agentic-utils/ccs/internal/stats"
)

// Every MCP tool is counted under mcp.<name>; a new tool needs a registry entry.
func TestToolsAreRegisteredStats(t *testing.T) {
	for _, tool := range mcpTools {
		name, _ := tool["name"].(string)
		if !stats.Known(stats.Feature("mcp." + name)) {
			t.Errorf("MCP tool %q has no mcp.%s in the stats registry", name, name)
		}
	}
}

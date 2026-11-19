package tui

import (
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/github"
)

// A PR title comes from GitHub and is drawn as is in the preview's PR lines
// and the ^O popup: control codes in it never reach the terminal.
func TestPRTitleIsCleaned(t *testing.T) {
	m := model{prs: map[string]github.PRInfo{
		prA: {State: "OPEN", MergeState: "CLEAN", Title: "fix\u009b2J it\x1b]0;pwned\x07 \u009d8;;http://evil\u009cnow"},
	}, width: 100}
	for name, line := range map[string]string{"PR line": m.prLine(prA), "^O row": m.prLinkText(prA)} {
		if strings.Contains(line, "\u009b") || strings.Contains(line, "\u009d") || strings.Contains(line, "2J") ||
			strings.Contains(line, "pwned") || strings.Contains(line, "evil") {
			t.Errorf("%s keeps the title's control codes: %q", name, line)
		}
		if !strings.Contains(line, "fix it now") {
			t.Errorf("%s lost the title's text: %q", name, line)
		}
	}
}

package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

func TestNudgeEnvelopeIsNotTheUser(t *testing.T) {
	claimEnv(t)
	sessDir := t.TempDir() // never the real ~/.claude/sessions
	old := sessions.GetSessionsDir
	sessions.GetSessionsDir = func() string { return sessDir }
	t.Cleanup(func() { sessions.GetSessionsDir = old })
	sock, got := fakeSocket(t)
	pid := os.Getpid()
	os.WriteFile(filepath.Join(sessDir, fmt.Sprintf("%d.json", pid)), []byte(fmt.Sprintf(`{"sessionId":"s-me","name":"me","messagingSocketPath":%q}`, sock)), 0o600)
	sum := sha256.Sum256([]byte(sock))
	os.WriteFile(filepath.Join(sessDir, fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:]))), []byte(`{"peerToken":"tok"}`), 0o600)

	if err := github.SendNudge(github.PRClaim{Session: "s-other", PID: pid}, "x"); err == nil {
		t.Error("a pid now running another session is refused")
	}
	if err := github.SendNudge(github.PRClaim{Session: "s-me", PID: pid}, "checks failing"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(<-got), "\n")
	var msg struct {
		Message struct{ Content string }
		From    string `json:"from"`
	}
	if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &msg) != nil {
		t.Fatalf("auth then message: %q", lines)
	}
	c := msg.Message.Content
	if !strings.HasPrefix(c, `<cross-session-message from-name="ccs watch">`+"\n"+github.NudgeNote+"checks failing") || strings.Contains(c, ccsSender) || strings.Contains(c, transcript.CcsNote) || msg.From != "" {
		t.Errorf("an automatic notice from ccs watch, never the user, with no return address: %q from=%q", c, msg.From)
	}
	if !strings.Contains(github.NudgeNote, "Not typed by the user") {
		t.Error("the note says the user didn't type it")
	}
}

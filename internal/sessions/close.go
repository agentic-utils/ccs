package sessions

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// A graceful close: the session is asked to wrap up before it is stopped,
// since a plain close stops it mid-whatever-it-was-doing.

// closeSender is the from-name the notice carries: not CcsSender, which says
// the user typed it.
const (
	closeSender = "ccs close"
	CloseNote   = "[Automatic notice from ccs close, run by the user or by the session that coordinates theirs. Not typed by the user and not a conversation: there is no one to reply to.]\n\n"
)

// GracefulTimeout is how long a graceful close waits for the session to go
// idle after the notice before stopping it anyway.
const GracefulTimeout = 2 * time.Minute

// GracefulPoll is how often a graceful close looks at the session.
var GracefulPoll = time.Second

// closeNoticeText is what the session is asked. ref marks it in the
// transcript, so its arrival can be seen.
func closeNoticeText(timeout time.Duration, ref string) string {
	return fmt.Sprintf("This session is about to be closed (notice %s). Stop at a safe point now and start nothing new: "+
		"commit or stash work worth keeping, stop the background tasks you started, and remove the git worktrees you created that nothing still needs. "+
		"Then say, in one short message in this conversation, what you finished and what is left undone. "+
		"The session is stopped as soon as you go idle after this, or in %s at the latest.", ref, timeout)
}

// SendCloseNotice sends text to the Claude Code session on pid over its
// socket, in ccs close's own envelope, with no typing fallback. Stubbed in
// tests.
var SendCloseNotice = func(pid int, f SessionFile, text string) error {
	if f.Socket == "" {
		return errors.New("it has no message socket")
	}
	_, err := SendEnvelope(f.Socket, PeerToken(pid, f.Socket), closeSender, CloseNote, text, false)
	return err
}

// transcriptOf is the path of a Claude Code session's transcript, "" if none.
func transcriptOf(id string) string {
	return transcript.TranscriptPath(id)
}

// appendedHas: the file at path holds want somewhere after offset.
func appendedHas(path string, offset int64, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() < offset {
		offset = 0 // rewritten (a prune): look at all of it
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	data, err := io.ReadAll(f)
	return err == nil && bytes.Contains(data, []byte(want))
}

// lastSaidBy is the session's last message of its own in the transcript at
// path, cut to a few lines' worth, "" if none.
func lastSaidBy(path string) string {
	c, err := transcript.ParseConversationFile(path, time.Time{}, 0)
	if err != nil || c == nil {
		return ""
	}
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if m := c.Messages[i]; m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
			text := transcript.CleanText(strings.TrimSpace(m.Text))
			if r := []rune(text); len(r) > 1500 {
				text = string(r[:1500]) + "…"
			}
			return text
		}
	}
	return ""
}

// CloseGracefully asks the one running Claude Code session called name to
// wrap up, waits until it has gone idle after the notice reached it (or
// timeout passes), then closes it as CloseByName does. Nothing is stopped
// when the notice can't be sent: the caller asked for a graceful close, and
// can still close it plainly.
func CloseGracefully(name string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = GracefulTimeout
	}
	pids, err := Named(name)
	if err != nil {
		return "", err
	}
	switch len(pids) {
	case 0:
		return "", fmt.Errorf("no running session named %s", name)
	case 1:
	default:
		return "", fmt.Errorf("%d running sessions are named %s (pids %v): close one by hand", len(pids), name, pids)
	}
	pid := pids[0]
	plain := fmt.Sprintf("nothing was stopped; ccs close %s stops it at once", name)
	f, err := ReadSessionFile(pid)
	if err != nil || f.SessionID == "" {
		// Codex has no session file and no socket: nothing can ask it to wrap up.
		return "", fmt.Errorf("%s (pid %d) isn't a Claude Code session ccs can send a notice to (a Codex session, perhaps), so it can't be closed gracefully: %s", name, pid, plain)
	}
	if f.Name != name {
		return "", fmt.Errorf("pid %d is now another session (%s): %s", pid, f.Name, plain)
	}
	tty := proc.TTYOf(pid)
	if tty == "" {
		return "", fmt.Errorf("%s (pid %d) has no terminal: an app or claude --bg started it, close it there", name, pid)
	}
	path := transcriptOf(f.SessionID)
	var offset int64
	if info, err := os.Stat(path); err == nil {
		offset = info.Size()
	}
	var b [4]byte
	rand.Read(b[:])
	ref := hex.EncodeToString(b[:])
	sent := time.Now()
	if err := SendCloseNotice(pid, f, closeNoticeText(timeout, ref)); err != nil {
		return "", fmt.Errorf("couldn't ask %s (pid %d) to wrap up: %v: %s", name, pid, err, plain)
	}
	state.LogActivity("close %s: asked it to wrap up (notice %s), waiting up to %s", name, ref, timeout)

	// Idle after the notice: its status changed since the notice was sent (or
	// it was seen busy since, for a session file with no status time) and the
	// notice is in its transcript, so the idle it's in now isn't the one it
	// was in before it read it.
	deadline := sent.Add(timeout)
	arrived, sawBusy, how := false, false, ""
	for how == "" {
		Sleep(GracefulPoll)
		switch cur, err := ReadSessionFile(pid); {
		case !proc.PIDAlive(pid):
			how = "it exited by itself"
		case errors.Is(err, fs.ErrNotExist) || err == nil && cur.SessionID != f.SessionID:
			how = "its session ended"
		case err != nil:
			// Caught mid-rewrite (written in place, so briefly empty): look again.
		default:
			if path == "" {
				path = transcriptOf(f.SessionID)
			}
			arrived = arrived || path != "" && appendedHas(path, offset, ref)
			sawBusy = sawBusy || cur.Status == "busy"
			if arrived && cur.Status == "idle" && (sawBusy || cur.StatusUpdatedAt > sent.UnixMilli()) {
				how = fmt.Sprintf("it went idle %s after being asked to wrap up", time.Since(sent).Round(time.Second))
			}
		}
		if how == "" && time.Now().After(deadline) {
			switch {
			case !arrived:
				how = fmt.Sprintf("the notice never showed in its transcript within %s (it may be holding it for approval), so it was stopped without wrapping up", timeout)
			default:
				how = fmt.Sprintf("it hadn't gone idle %s after being asked to wrap up, so it was stopped as it was", timeout)
			}
		}
	}
	said := ""
	if strings.HasPrefix(how, "it went idle") {
		if text := lastSaidBy(path); text != "" {
			said = "\nIts last message:\n" + text
		}
	}
	state.LogActivity("close %s: %s", name, how)
	if !proc.PIDAlive(pid) {
		CloseTab(tty)
		return fmt.Sprintf("%s (pid %d): %s%s", name, pid, how, said), nil
	}
	msg, err := CloseSession(name, pid)
	if err != nil {
		return "", fmt.Errorf("%s: %s; then %v", name, how, err)
	}
	return msg + ": " + how + said, nil
}

// RunClose is `ccs close`.
func RunClose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	graceful := fs.Bool("graceful", false, "ask the session to wrap up first (finish or stop safely, remove its worktrees, say what's left), and stop it once it goes idle")
	timeout := fs.Duration("timeout", GracefulTimeout, "with --graceful: how long to wait before stopping it anyway")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: ccs close [--graceful [--timeout 2m]] <name>\n\nStop the named session and close its tab.")
		fs.PrintDefaults()
	}
	// The name may come first, as it always did.
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		name = ""
	}
	if name == "" {
		fs.Usage()
		return 2
	}
	var msg string
	var err error
	if *graceful {
		msg, err = CloseGracefully(name, *timeout)
	} else {
		msg, err = CloseByName(name)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccs close:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

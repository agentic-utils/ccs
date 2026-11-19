package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
)

// finishedRun is boxModel with a finished ! command for "live", run in dir
// by pid os.Getpid() started at "Thu Nov 19 12:00:00 2026", and its result
// sends caught instead of delivered.
func finishedRun(t *testing.T, dir, out string, code int) (model, *[]string) {
	t.Helper()
	m := boxModel(t, "idle", dir)
	m.chatFocus = false // ^S works whatever has the keys
	m.run = &shellRun{id: "live", dir: dir, line: "git status", gen: 1, cancel: func() {},
		res: proc.ShellResult{Out: []byte(out), Code: code, Took: 300 * time.Millisecond},
		pid: os.Getpid(), procFrom: "Thu Nov 19 12:00:00 2026"}
	stubLiveStart(t, "Thu Nov 19 12:00:00 2026", true)
	var mu sync.Mutex
	sent := &[]string{}
	old := deliverRunResult
	deliverRunResult = func(pid int, id, name, kind, text string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		*sent = append(*sent, id+"|"+text)
		return "✓ Sent the result to " + name, nil
	}
	t.Cleanup(func() { deliverRunResult = old })
	return m, sent
}

func stubLiveStart(t *testing.T, start string, ok bool) {
	t.Helper()
	old := liveStart
	liveStart = func(pid int, id string) (string, bool) { return start, ok }
	t.Cleanup(func() { liveStart = old })
}

func ctrlS(m model) (model, tea.Cmd) { return key(m, tea.KeyMsg{Type: tea.KeyCtrlS}) }

func runeKey(m model, r rune) (model, tea.Cmd) {
	return key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
}

func TestRunSendWithOutput(t *testing.T) {
	m, sent := finishedRun(t, home(t)+"/proj", "On branch main\nnothing to commit\n", 0)
	if v := strip2(m.chatView()); !strings.Contains(v, "^S send to") {
		t.Fatalf("the finished box offers ^S:\n%s", v)
	}
	m, _ = ctrlS(m)
	if v := strip2(m.chatView()); !strings.Contains(v, "send to") || !strings.Contains(v, "o with output") || !strings.Contains(v, "e exit code only") || !strings.Contains(v, "esc cancel") {
		t.Fatalf("the choice line:\n%s", v)
	}
	m, cmd := runeKey(m, 'o')
	m = update(m, work(t, cmd))
	want := "live|I ran `git status` in ~/proj: exit 0:\n```\nOn branch main\nnothing to commit\n```"
	if len(*sent) != 1 || (*sent)[0] != want {
		t.Errorf("sent %q\nwant %q", *sent, want)
	}
	if m.run.asking || !strings.Contains(m.sendNote["live"], "Sent the result") {
		t.Errorf("asking %v note %q", m.run.asking, m.sendNote["live"])
	}
}

// home points HOME at a temp folder and returns it, so tildePath is known.
func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	os.MkdirAll(h+"/proj", 0o700)
	return h
}

func TestRunSendExitCodeOnly(t *testing.T) {
	m, sent := finishedRun(t, home(t)+"/proj", "boom\n", 3)
	m, _ = ctrlS(m)
	m, cmd := runeKey(m, 'e')
	update(m, work(t, cmd))
	if len(*sent) != 1 || (*sent)[0] != "live|I ran `git status` in ~/proj: exit 3" {
		t.Errorf("sent %q", *sent)
	}
}

func TestRunSendOutcomes(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{{proc.ErrTimedOut, "timed out after " + formatTook(runTimeout)}, {context.Canceled, "stopped"}} {
		r := &shellRun{line: "x", dir: "/tmp", err: c.err}
		if got := r.runMessage(false); got != "I ran `x` in /tmp: "+c.want {
			t.Errorf("%v: %q", c.err, got)
		}
	}
}

func TestRunSendFenceOutlastsBackticks(t *testing.T) {
	r := &shellRun{line: "cat README.md", dir: "/tmp", res: proc.ShellResult{Out: []byte("a ``` b\n```` c\n")}}
	got := r.runMessage(true)
	if !strings.Contains(got, ":\n`````\na ``` b\n```` c\n`````") {
		t.Errorf("the fence must be longer than any backtick run:\n%s", got)
	}
}

func TestRunSendCapsAtALine(t *testing.T) {
	line := strings.Repeat("x", 99) + "\n" // 100 bytes a line
	out := strings.Repeat(line, 3000)      // 300KB, ~293KB
	r := &shellRun{line: "seq", dir: "/tmp", res: proc.ShellResult{Out: []byte(out)}}
	got := r.runMessage(true)
	if !strings.Contains(got, "(output cut to 16KB of 293KB):") {
		t.Errorf("says it was cut and from what:\n%s", got[:120])
	}
	body := got[strings.Index(got, "```\n")+4 : strings.LastIndex(got, "\n```")]
	if len(body) > runSendCap || !strings.HasSuffix(body, strings.Repeat("x", 99)) || strings.Count(body, "\n")+1 != 163 {
		t.Errorf("cut at a line boundary within the cap: %d bytes, %d lines", len(body), strings.Count(body, "\n")+1)
	}
	if !strings.HasSuffix(got, "\n… 2837 more lines cut") {
		t.Errorf("ends with the lines cut: %q", got[len(got)-40:])
	}
}

func TestRunSendEscCancels(t *testing.T) {
	m, sent := finishedRun(t, t.TempDir(), "hi\n", 0)
	m, _ = ctrlS(m)
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || m.run == nil || m.run.asking || len(*sent) != 0 {
		t.Errorf("esc cancels the choice only: run %v sent %q", m.run, *sent)
	}
	if _, cmd = runeKey(m, 'o'); cmd != nil || len(*sent) != 0 {
		t.Error("o after esc is an ordinary key: it sends nothing")
	}
	// While asking, other keys are ignored rather than typed or acted on.
	m, _ = ctrlS(m)
	m, _ = runeKey(m, 'x')
	if !m.run.asking || m.chatInput.Value() != "" || m.textInput.Value() != "" || len(*sent) != 0 {
		t.Errorf("a stray key: asking %v box %q search %q", m.run.asking, m.chatInput.Value(), m.textInput.Value())
	}
}

// The result goes to the session the command ran for, by its id, pid and
// start time, never to the one selected now.
func TestRunSendGoesToTheSessionItRanFor(t *testing.T) {
	m, sent := finishedRun(t, t.TempDir(), "hi\n", 0)
	m, _ = ctrlS(m)
	for i := range m.filtered { // the selection moves away while the choice is open
		if m.filtered[i].conv.SessionID != "live" {
			m.cursor = i
			break
		}
	}
	if m.runShown() {
		t.Fatal("the box belongs to live, so it isn't shown on another session")
	}
	m, cmd := runeKey(m, 'o')
	if cmd != nil || m.run.asking || len(*sent) != 0 {
		t.Errorf("with its box gone the choice closes and nothing is sent: %q", *sent)
	}
	// Back on live, the send still names and reaches live.
	m.selectSession("live")
	m.run.asking = true
	m, cmd = runeKey(m, 'o')
	update(m, work(t, cmd))
	if len(*sent) != 1 || !strings.HasPrefix((*sent)[0], "live|") {
		t.Errorf("sent %q", *sent)
	}
}

func TestRunSendRefusedWhenTheSessionRestartedOrWent(t *testing.T) {
	for _, c := range []struct {
		name  string
		start string
		ok    bool
	}{{"restarted", "Thu Nov 19 13:00:00 2026", true}, {"gone", "", false}} {
		t.Run(c.name, func(t *testing.T) {
			m, sent := finishedRun(t, t.TempDir(), "hi\n", 0)
			stubLiveStart(t, c.start, c.ok)
			m, _ = ctrlS(m)
			m, cmd := runeKey(m, 'o')
			if cmd != nil || len(*sent) != 0 || !strings.Contains(m.sendNote["live"], "isn't the same running session") {
				t.Errorf("sent %q note %q", *sent, m.sendNote["live"])
			}
		})
	}
	t.Run("new pid", func(t *testing.T) {
		m, sent := finishedRun(t, t.TempDir(), "hi\n", 0)
		m.run.pid = 0 // no pid was recorded: never guess one now
		m, _ = ctrlS(m)
		m, cmd := runeKey(m, 'o')
		if cmd != nil || len(*sent) != 0 {
			t.Errorf("sent %q", *sent)
		}
	})
}

func TestCtrlSOnlyOnAFinishedBoxWithAnEmptyMessage(t *testing.T) {
	m, _ := finishedRun(t, t.TempDir(), "hi\n", 0)
	m.chatFocus = true
	m.chatInput.SetValue("half a message")
	m, _ = ctrlS(m)
	if m.run.asking || m.chatInput.Value() != "half a message" {
		t.Errorf("^S with text in the box changes nothing: asking %v box %q", m.run.asking, m.chatInput.Value())
	}
	m.chatInput.SetValue("")
	m.run.running = true
	m, _ = ctrlS(m)
	if m.run.asking {
		t.Error("^S while it runs changes nothing")
	}
}

// The real delivery never types: no socket means not sent.
func TestRunResultIsNeverTyped(t *testing.T) {
	m := boxModel(t, "idle", t.TempDir()) // its session file names an unreachable socket
	log := fakeTerminal(t)
	m.livePIDs = map[string]int{"live": os.Getpid()}
	note, err := deliverRunResult(os.Getpid(), "live", "live", "", "I ran `ls` in /tmp: exit 0")
	if !errors.Is(err, sessions.ErrUnsent) || !strings.Contains(note, "never typed") {
		t.Errorf("note %q err %v", note, err)
	}
	if got, _ := os.ReadFile(log); len(got) != 0 {
		t.Errorf("typed into the terminal:\n%s", got)
	}
	_ = m
}

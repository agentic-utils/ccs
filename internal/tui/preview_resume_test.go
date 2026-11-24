package tui

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// growing is a conversation that gains messages and tool calls as a busy
// session's does: replies, typed messages, harness notes that stack, sent
// messages, tool calls, turn ends, new days and a queue that comes and goes.
func growing(r *rand.Rand, c *transcript.Conversation, t *time.Time, steps int) {
	for range steps {
		*t = t.Add(time.Duration(r.IntN(240)) * time.Minute)
		ts := t.Format(time.RFC3339)
		switch r.IntN(8) {
		case 0:
			c.Messages = append(c.Messages, transcript.Message{Role: "user", Text: fmt.Sprintf("please **do** %d", r.IntN(99)), Ts: ts})
		case 1:
			c.Messages = append(c.Messages, transcript.Message{Role: "user", Text: "<task-notification>done " + ts + "</task-notification>", Ts: ts})
		case 2:
			c.Messages = append(c.Messages, transcript.Message{Role: "sent", To: "uds:/tmp/x.sock", Note: "hi", Text: "a message", Ts: ts})
			if r.IntN(2) == 0 {
				if c.PeerNotes == nil {
					c.PeerNotes = map[int]string{}
				}
				c.PeerNotes[len(c.Messages)-1] = "worker ✓ arrived"
			}
		case 3, 4:
			c.Tools = append(c.Tools, transcript.ToolUse{At: len(c.Messages), Name: "Bash", Arg: fmt.Sprintf("ls %d", r.IntN(9)), Ts: ts})
		case 5:
			c.Tools = append(c.Tools, transcript.ToolUse{At: len(c.Messages), Ts: ts, Took: time.Duration(r.IntN(600)) * time.Second})
		default:
			c.Messages = append(c.Messages, transcript.Message{Role: "assistant", Text: fmt.Sprintf("Reply %d with `code` and a [link](https://example.com/%d).\n\n- one\n- two", r.IntN(99), r.IntN(9)), Ts: ts})
		}
		c.Queue = nil
		if r.IntN(3) == 0 {
			c.Queue = []transcript.QueuedMessage{{Text: "queued " + ts}}
		}
	}
}

func TestPreviewResumesAsItWouldBuild(t *testing.T) {
	for seed := range uint64(30) {
		r := rand.New(rand.NewPCG(seed, 1))
		c := transcript.Conversation{SessionID: "s"}
		at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
		cache := &previewCache{}
		for step := range 40 {
			growing(r, &c, &at, 1+r.IntN(4))
			conv := c
			conv.Messages, conv.Tools = slices.Clone(c.Messages), slices.Clone(c.Tools)
			got := slices.Clone(cachedPreview(cache, conv, "", 80))
			want, stamps := buildPreview(conv, "", 80)
			if !slices.Equal(got, want) || !slices.Equal(cache.stamps, stamps) {
				t.Fatalf("seed %d step %d: resumed preview differs from a full build:\n%q\n%q", seed, step, got, want)
			}
		}
	}
}

func TestPreviewRebuildsWhenItCantResume(t *testing.T) {
	c := transcript.Conversation{SessionID: "s", Messages: []transcript.Message{{Role: "assistant", Text: "one"}, {Role: "assistant", Text: "two"}}}
	cache := &previewCache{}
	cachedPreview(cache, c, "", 80)
	for name, change := range map[string]func(*transcript.Conversation){
		"last message changed": func(c *transcript.Conversation) {
			c.Messages[1].Text = "TWO"
			c.Messages = append(c.Messages, transcript.Message{Role: "assistant", Text: "three"})
		},
		"tool among old ones": func(c *transcript.Conversation) {
			c.Tools = append(c.Tools, transcript.ToolUse{At: 1, Name: "Bash", Arg: "ls"})
		},
		"old peer label changed": func(c *transcript.Conversation) {
			c.PeerNotes = map[int]string{0: "x"}
			c.Messages = append(c.Messages, transcript.Message{Role: "assistant", Text: "three"})
		},
		"another session": func(c *transcript.Conversation) { c.SessionID = "t" },
	} {
		conv := c
		conv.Messages = slices.Clone(c.Messages)
		change(&conv)
		if cache.resume.canResume(conv, "", 80) {
			t.Errorf("%s: must build afresh", name)
		}
	}
	if cache.resume.canResume(c, "", 100) || cache.resume.canResume(c, "q", 80) {
		t.Error("another width or a search builds afresh")
	}
}

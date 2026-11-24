package tui

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// frameModel is a list of three live sessions, drawn through a frame cache,
// with the frame clock stopped at at.
func frameModel(t *testing.T, at *time.Time) model {
	t.Helper()
	oldFrame, oldHeld := frameNow, heldNow
	frameNow = func() time.Time { return *at }
	heldNow = frameNow // the pane's clock too
	t.Cleanup(func() { frameNow, heldNow = oldFrame, oldHeld })
	var convs []transcript.Conversation
	for i, id := range []string{"a", "b", "c"} {
		convs = append(convs, transcript.Conversation{
			SessionID: id, Title: "session " + id, LastTimestamp: fmt.Sprintf("2026-11-24T1%d:00:00Z", i),
			Messages: []transcript.Message{{Role: "user", Text: "hi " + id}, {Role: "assistant", Text: "hello from " + id + "?"}},
		})
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 140, 36
	m.frame = &frameCache{}
	res, _ := m.Update(m.liveFor(map[string]string{"a": "idle", "b": "idle", "c": "idle"}, nil))
	return res.(model)
}

// liveFor is a live tick's answer: these sessions live with these statuses
// (none: not live), and these transcripts grown.
func (m model) liveFor(stats map[string]string, updated []transcript.Conversation) liveMsg {
	msg := liveMsg{live: map[string]bool{}, pids: map[string]int{}, stats: map[string]sessions.SessionStat{},
		scan: map[string]sessions.LiveSession{}, updated: updated, gen: m.gen, tickGen: m.liveGen - 1}
	for id, st := range stats {
		msg.live[id], msg.pids[id] = true, os.Getpid()
		msg.stats[id] = sessions.SessionStat{Status: st, Since: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
		msg.scan[id] = sessions.LiveSession{PID: os.Getpid(), Stat: msg.stats[id]}
	}
	return msg
}

// Whatever the live tick, held poll and ccs serve's status bring, the frame
// View hands out is the one a fresh draw would make: a change is never left
// off the screen.
func TestStillMessagesDrawAsAFreshView(t *testing.T) {
	statuses := []string{"idle", "busy", "waiting", ""}
	for seed := range uint64(50) {
		r := rand.New(rand.NewPCG(seed, 2))
		at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
		m := frameModel(t, &at)
		grown := 0
		for step := range 60 {
			var msg tea.Msg
			switch r.IntN(8) {
			case 0, 1: // a live tick: statuses change or stay
				stats := map[string]string{}
				for _, id := range []string{"a", "b", "c"} {
					if st := statuses[r.IntN(len(statuses))]; st != "" || r.IntN(3) > 0 {
						stats[id] = cmpOr(st, "idle")
					}
				}
				if r.IntN(2) == 0 { // as last time
					stats = map[string]string{}
					for id := range m.live {
						stats[id] = cmpOr(m.chatStatus[id].Status, "idle")
					}
				}
				var updated []transcript.Conversation
				if r.IntN(4) == 0 {
					c := m.items[r.IntN(len(m.items))].conv
					grown++
					c.Messages = append(c.Messages, transcript.Message{Role: "assistant", Text: fmt.Sprintf("more %d", grown)})
					c.Size += 10
					updated = append(updated, c)
				}
				msg = m.liveFor(stats, updated)
			case 2: // a held poll: a prompt comes, stays or goes
				var prompts []mcp.HeldPrompt
				if r.IntN(2) == 0 {
					prompts = []mcp.HeldPrompt{{ID: "p1", Session: "b", Tool: "Bash", Summary: "ls", At: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}}
				}
				msg = heldMsg{prompts: prompts}
			case 3:
				msg = serveStatusMsg{st: mcp.ServiceStatus{Installed: r.IntN(2) == 0, Running: r.IntN(2) == 0}}
			case 4:
				msg = liveTickMsg{gen: m.liveGen + 1} // a chain a wake replaced: changes nothing
			case 5:
				msg = tea.KeyMsg{Type: []tea.KeyType{tea.KeyDown, tea.KeyUp}[r.IntN(2)]}
			case 6:
				at = at.Add(time.Duration(r.IntN(90)) * time.Second)
				msg = heldRetryMsg{}
			default:
				msg = chatTickResult{id: m.selectedSession()}
			}
			res, _ := m.Update(msg)
			m = res.(model)
			if got, want := m.View(), m.drawView(); got != want {
				t.Fatalf("seed %d step %d: after %T the frame isn't what a fresh draw gives:\n%s", seed, step, msg, lineDiff(got, want))
			}
		}
	}
}

// A live tick, held poll or fast tick that changes nothing draws nothing
// new; any key, and a tick that changes something, does.
func TestUnchangedMessagesDontRedraw(t *testing.T) {
	at := time.Date(2026, 11, 24, 12, 0, 30, 0, time.UTC)
	m := frameModel(t, &at)
	res, _ := m.Update(heldMsg{}) // the first answer: nothing held
	m = res.(model)
	m.View()
	same := m.liveFor(map[string]string{"a": "idle", "b": "idle", "c": "idle"}, nil)
	draws := m.frame.draws
	gen := m.viewGen
	for _, msg := range []tea.Msg{same, liveTickMsg{gen: m.liveGen + 1}, heldMsg{}, heldRetryMsg{}, serveStatusMsg{}, chatTickResult{id: m.selectedSession()}, same} {
		res, _ := m.Update(msg)
		m = res.(model)
		m.View()
	}
	if m.frame.draws != draws || m.viewGen != gen {
		t.Fatalf("unchanged messages drew %d frames (gen %d -> %d)", m.frame.draws-draws, gen, m.viewGen)
	}

	for name, msg := range map[string]tea.Msg{
		"a status":       m.liveFor(map[string]string{"a": "busy", "b": "idle", "c": "idle"}, nil),
		"a held prompt":  heldMsg{prompts: []mcp.HeldPrompt{{ID: "p", Session: "a", Tool: "Bash"}}},
		"a key":          tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")},
		"ccs serve":      serveStatusMsg{st: mcp.ServiceStatus{Installed: true}},
		"the window":     tea.WindowSizeMsg{Width: 120, Height: 30},
		"a session gone": m.liveFor(map[string]string{"a": "busy", "b": "idle"}, nil),
	} {
		before := m.frame.draws
		res, _ := m.Update(msg)
		m = res.(model)
		m.View()
		if m.frame.draws != before+1 {
			t.Errorf("%s didn't redraw", name)
		}
	}

	// The minute moves "5m ago" on: the next message draws again.
	draws = m.frame.draws
	at = at.Add(time.Minute)
	res, _ = m.Update(heldRetryMsg{})
	m = res.(model)
	m.View()
	if m.frame.draws != draws+1 {
		t.Error("a new minute didn't redraw")
	}
}

// While the selected session works, its spinner and elapsed time move on
// every fast tick.
func TestBusySelectionRedrawsEachTick(t *testing.T) {
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	m := frameModel(t, &at)
	res, _ := m.Update(m.liveFor(map[string]string{"a": "busy", "b": "busy", "c": "busy"}, nil))
	m = res.(model)
	m.View()
	draws := m.frame.draws
	for range 3 {
		at = at.Add(chatFastRefresh)
		res, _ := m.Update(chatTickResult{id: m.selectedSession()})
		m = res.(model)
		m.View()
	}
	if m.frame.draws != draws+3 {
		t.Fatalf("a working session's frame drew %d times in 3 ticks", m.frame.draws-draws)
	}
}

// A still message doesn't sort, but one in a new minute does: a snooze ends
// or a cooldown passes with nothing else happening.
func TestStillMessagesSortOnceAMinute(t *testing.T) {
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	m := frameModel(t, &at)
	m.sortMinute = time.Now().Unix() / 60
	m.marks = &sessionMarks{Settled: map[string]string{m.filtered[0].conv.SessionID: m.filtered[0].conv.LastTimestamp}}
	first := m.filtered[0].conv.SessionID
	res, _ := m.Update(heldRetryMsg{})
	if m = res.(model); m.filtered[0].conv.SessionID != first {
		t.Fatal("a still message sorted the list")
	}
	m.sortMinute--
	res, _ = m.Update(heldRetryMsg{})
	if m = res.(model); m.filtered[0].conv.SessionID == first {
		t.Fatal("a still message in a new minute didn't sort")
	}
}

// lineDiff is the lines a and b differ in.
func lineDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := range max(len(al), len(bl)) {
		x, y := "", ""
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, fmt.Sprintf("line %d:\n  cached %q\n  fresh  %q", i, x, y))
		}
	}
	return strings.Join(out, "\n")
}

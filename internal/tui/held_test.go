package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/sessions"
)

type heldCall struct {
	id, choice string
	labels     map[int][]string
	other      map[int]bool
	notes      map[int]string
}

// stubHeld makes ccs serve hold these prompts; answers are recorded.
func stubHeld(t *testing.T, prompts ...mcp.HeldPrompt) *[]heldCall {
	t.Helper()
	var calls []heldCall
	oldList, oldAnswer, oldSock := heldPrompts, answerHeld, controlSock
	heldPrompts = func() ([]mcp.HeldPrompt, error) { return prompts, nil }
	answerHeld = func(id, choice string, labels map[int][]string, other map[int]bool, notes map[int]string) (string, error) {
		calls = append(calls, heldCall{id, choice, labels, other, notes})
		return "done " + choice, nil
	}
	controlSock = func() bool { return true }
	t.Cleanup(func() { heldPrompts, answerHeld, controlSock = oldList, oldAnswer, oldSock })
	return &calls
}

// press sends one key and runs whatever command comes back.
func press(m model, k string) model {
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	return run(res.(model), cmd)
}

func TestHeldShownInBottomPane(t *testing.T) {
	stubHeld(t, mcp.HeldPrompt{ID: "p4", Session: "live", Tool: "Bash", Summary: "make deploy"})
	m := waitModel(t)
	m.width, m.height = 100, 40
	m.lastKeyAt = time.Now() // just typed: auto-select waits
	m = run(m, m.heldCmd())
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "permission prompt")})
	if v := strip2(m.View()); !strings.Contains(v, "also in its tab") || !strings.Contains(v, "Bash · permission") || !strings.Contains(v, "esc list") {
		t.Errorf("bottom pane:\n%s", v)
	}
}

// No control socket: nothing is asked and nothing is marked held.
func TestHeldNeedsServe(t *testing.T) {
	stubHeld(t, mcp.HeldPrompt{ID: "p5", Session: "live", Tool: "Bash"})
	controlSock = func() bool { return false }
	m := waitModel(t)
	if cmd := m.heldCmd(); cmd != nil || m.heldPrompt != nil {
		t.Error("no ccs serve, no asking")
	}
}

// While a permission prompt is held, a digit is swallowed: it never reaches
// the answer that types into the session's terminal.
func TestHeldPermissionSwallowsDigits(t *testing.T) {
	m, calls, typedp := paneModel(t, mcp.HeldPrompt{ID: "p3", Session: "live", Tool: "Bash", Summary: "ls"})
	conv := m.items[0].conv
	for _, it := range m.items {
		if it.conv.SessionID == "live" {
			conv = it.conv
		}
	}
	for _, k := range []string{"1", "5", "9"} {
		if _, done := m.heldKey(k, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}, conv); !done {
			t.Errorf("digit %s must be the held prompt's, never the terminal answer's", k)
		}
	}
	m = press(m, "1")
	if *typedp || len(*calls) != 0 || m.chatInput.Value() != "" {
		t.Errorf("a digit on a held permission prompt: typed %v, calls %v, box %q", *typedp, *calls, m.chatInput.Value())
	}
}

// An answer sent whose reply never came is reported as unknown, never failed.
func TestHeldOutcomeUnknown(t *testing.T) {
	m, _, _ := paneModel(t, mcp.HeldPrompt{ID: "p4", Session: "live", Tool: "Bash", Summary: "ls"})
	m.applyHeldDone(heldDoneMsg{name: "worker", err: mcp.ErrOutcomeUnknown})
	if !strings.Contains(m.heldErr, "reply never came") || strings.Contains(m.heldErr, "Couldn't") {
		t.Errorf("heldErr %q", m.heldErr)
	}
}

// paneModel is waitModel with "live" selected, waiting, its prompt p held by
// ccs, and no terminal typing possible.
func paneModel(t *testing.T, p mcp.HeldPrompt) (model, *[]heldCall, *bool) {
	t.Helper()
	calls := stubHeld(t, p)
	typed := false
	oldFind, oldSend := sessions.FindTerminal, sessions.SendKeys
	sessions.FindTerminal = func(int) sessions.Terminal { return sessions.Terminal{Kind: "tmux pane", Target: "%1"} }
	sessions.SendKeys = func(sessions.Terminal, string, bool) error { typed = true; return nil }
	t.Cleanup(func() { sessions.FindTerminal, sessions.SendKeys = oldFind, oldSend })
	m := waitModel(t)
	m.width, m.height = 100, 40
	m.lastKeyAt = time.Now() // just typed: auto-select waits, so the test drives the keys
	m = run(m, m.heldCmd())
	m.filtered = m.items
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "permission prompt")})
	return m, calls, &typed
}

func keyType(m model, k tea.KeyType) model {
	res, cmd := m.Update(tea.KeyMsg{Type: k})
	return run(res.(model), cmd)
}

// The bottom pane answers the selected session's held prompt with no Enter
// first: A and D ask y/n, a letter it doesn't use goes to
// the search, Esc leaves it for the list; nothing is typed into a terminal.
func TestHeldPaneAnswersAPermission(t *testing.T) {
	m, calls, typed := paneModel(t, mcp.HeldPrompt{ID: "p6", Session: "live", Tool: "Bash", Summary: "make deploy"})
	if !m.heldKeysActive() || m.chatFocus {
		t.Fatalf("the held prompt has the keys at once: active %v chat %v", m.heldKeysActive(), m.chatFocus)
	}
	if v := strip2(m.View()); !strings.Contains(v, "open its tab") || !strings.Contains(v, "keys: menu") {
		t.Errorf("the pane says what its keys do:\n%s", v)
	}
	m = press(m, "a")
	if len(*calls) != 0 || m.heldConfirmed == nil || !strings.Contains(strip2(m.View()), "Allow Bash") {
		t.Fatalf("a asks y/n first: calls %+v", *calls)
	}
	if m = press(m, "n"); len(*calls) != 0 || m.heldConfirmed != nil {
		t.Fatal("n sends nothing")
	}
	m = press(press(m, "d"), "y")
	if len(*calls) != 1 || (*calls)[0].choice != "deny" {
		t.Fatalf("d then y denies: %+v", *calls)
	}
	if m = keyType(m, tea.KeyEsc); m.heldKeysActive() {
		t.Error("Esc leaves it for the list")
	}
	m = press(m, "a")
	if m.textInput.Value() != "a" || m.heldConfirmed != nil {
		t.Errorf("after Esc, letters go to the search: %q", m.textInput.Value())
	}
	m = selectLive(keyType(m, tea.KeyBackspace)) // the search empty again, back on it
	m = keyType(m, tea.KeyEnter)
	if !m.heldKeysActive() {
		t.Fatal("Enter gives it the keys again")
	}
	m = press(m, "x")
	if m.textInput.Value() != "x" || len(*calls) != 1 {
		t.Errorf("a letter it doesn't use goes to the search: %q %+v", m.textInput.Value(), *calls)
	}
	if *typed {
		t.Error("nothing is ever typed into a terminal")
	}
}

// A session whose prompt ccs holds is selected once you've stopped typing
// in ccs, its held prompt focused behind the typing guard, whether or not
// ^X A is on; never while typing, with text in the search, or with a popup
// open.
func TestHeldAutoSelect(t *testing.T) {
	offset := time.Duration(0)
	old := heldNow
	heldNow = func() time.Time { return time.Now().Add(offset) }
	t.Cleanup(func() { heldNow = old })
	fresh := func() model {
		m, _, _ := paneModel(t, mcp.HeldPrompt{ID: "p8", Session: "live", Tool: "Bash", Summary: "ls"})
		m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID != "live" })
		m.selectionMoved()
		m.autoFocus, m.lastKeyAt = true, time.Now().Add(-time.Hour)
		return m
	}
	selected := func(m model) bool { return m.filtered[m.cursor].conv.SessionID == "live" }

	m := fresh()
	m.autoFocus = false
	if m.autoSelect(); !selected(m) {
		t.Error("^X A off: a held prompt is still selected")
	}
	m = fresh()
	m.lastKeyAt = time.Now()
	if m.autoSelect(); selected(m) {
		t.Error("typing in ccs just now: the selection stays")
	}
	m = fresh()
	m.textInput.SetValue("dep")
	if m.autoSelect(); selected(m) {
		t.Error("text in the search: the selection stays")
	}
	m = fresh()
	m.helpOpen = true
	if m.autoSelect(); selected(m) {
		t.Error("a popup open: the selection stays")
	}

	m = fresh()
	if m.autoSelect(); !selected(m) || !m.heldFocus {
		t.Fatalf("idle with ^X A on: selected %v, pane focused %v", selected(m), m.heldFocus)
	}
	// The guard: keys still being typed when it came up are dropped.
	_, calls, _ := paneModel(t, mcp.HeldPrompt{ID: "p8", Session: "live", Tool: "Bash", Summary: "ls"})
	*calls = nil
	offset = 300 * time.Millisecond
	m = press(m, "a")
	if m.heldConfirmed != nil || !strings.Contains(m.heldNote, "paused") {
		t.Fatalf("a key in the first second is dropped: confirm %v note %q", m.heldConfirmed, m.heldNote)
	}
	offset = 3 * time.Second
	if m = press(m, "a"); m.heldConfirmed == nil {
		t.Error("after the pause, keys answer as usual")
	}
	// Once per wait: a second look doesn't move it again.
	m.selectionMoved()
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID != "live" })
	if m.autoSelect(); selected(m) {
		t.Error("a wait already dealt with isn't selected again")
	}
}

// This ccs holds every prompt, whatever the focus (questions arrive while
// you're elsewhere), for the hook's longest wait, with no key-press
// release; blur and focus reports change nothing about the hold.
func TestTUIHoldsWhateverTheFocus(t *testing.T) {
	cfg := tuiHoldConfig()
	if cfg.MayHold != nil || cfg.KeyRelease != nil || !cfg.Yields || cfg.Wait() != 30*time.Minute {
		t.Errorf("config: mayhold %v keyrelease %v yields %v wait %s", cfg.MayHold != nil, cfg.KeyRelease != nil, cfg.Yields, cfg.Wait())
	}
	released := false
	old := newTUIHolder
	newTUIHolder = func() *mcp.Holder { released = true; return old() }
	t.Cleanup(func() { newTUIHolder = old })
	m := waitModel(t)
	res, _ := m.Update(tea.BlurMsg{})
	res, _ = res.(model).Update(tea.FocusMsg{})
	if released || res.(model).blurred {
		t.Error("focus reports don't touch the hold")
	}
}

// Held prompts are selected whether or not ^X A is on, the oldest first,
// one at a time: never off a held prompt still waiting, the next once it's
// settled.
func TestHeldAutoSelectOldestOneAtATime(t *testing.T) {
	m := waitModel(t)
	m.width, m.height = 100, 40
	m.filtered = m.items
	m.autoFocus, m.lastKeyAt = false, time.Now().Add(-time.Hour)
	t0 := time.Now()
	m.heldPrompt = map[string]mcp.HeldPrompt{
		"menu": {ID: "pm", Session: "menu", Tool: "Bash", At: t0.Add(2 * time.Second)},
		"asks": {ID: "pa", Session: "asks", Tool: "Bash", At: t0.Add(time.Second)},
	}
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	on := func() string { return m.filtered[m.cursor].conv.SessionID }
	m.autoSelect()
	if on() != "asks" || !m.heldFocus {
		t.Fatalf("the oldest held first, ^X A off: on %s, pane %v", on(), m.heldFocus)
	}
	m.heldFocus = false // Esc out of the pane, still on it
	m.autoSelect()
	if on() != "asks" {
		t.Errorf("stays on a held prompt still waiting: on %s", on())
	}
	delete(m.heldPrompt, "asks") // answered
	m.autoSelect()
	if on() != "asks" {
		t.Errorf("never two auto-selects back to back: on %s", on())
	}
	m.autoSelectAt = time.Now().Add(-time.Minute) // a while later
	m.autoSelect()
	if on() != "menu" {
		t.Errorf("the next once it's settled: on %s", on())
	}
	// Unsent text anywhere: no move.
	m2 := waitModel(t)
	m2.filtered = m2.items
	m2.lastKeyAt = time.Now().Add(-time.Hour)
	m2.heldPrompt = map[string]mcp.HeldPrompt{"menu": {ID: "pm", Session: "menu", Tool: "Bash", At: t0}}
	m2.cursor = slices.IndexFunc(m2.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m2.chatInput.SetValue("half a message")
	if m2.autoSelect(); m2.filtered[m2.cursor].conv.SessionID != "live" {
		t.Error("a draft keeps the selection")
	}
}

// ^X A never switches to a held prompt's tab: it shows no menu there.
func TestAutoFocusSkipsHeldPrompts(t *testing.T) {
	m := waitModel(t)
	m.autoFocus = true
	m.livePIDs = map[string]int{"menu": 4242}
	m.chatStatus = map[string]sessions.SessionStat{"menu": waiting(time.Now(), "permission prompt")}
	m.heldPrompt = map[string]mcp.HeldPrompt{"menu": {ID: "pm", Session: "menu", Tool: "Bash"}}
	if cmd := m.autoFocusCmd(); cmd != nil {
		t.Error("a held prompt's tab is never switched to")
	}
}

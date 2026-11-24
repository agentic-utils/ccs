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

// While a permission prompt is held every digit is its own: one past its
// rows, or 0, is swallowed with a note, and none reaches the terminal.
func TestHeldPermissionSwallowsDigits(t *testing.T) {
	m, calls, typedp := permPane(t, bashPrompt())
	for _, k := range []string{"0", "4", "9"} {
		m = press(m, k)
	}
	if *typedp || len(*calls) != 0 || m.chatInput.Value() != "" || !strings.Contains(m.heldNote, "1-3") {
		t.Errorf("a digit past the rows: typed %v, calls %v, box %q, note %q", *typedp, *calls, m.chatInput.Value(), m.heldNote)
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

// bashPrompt is a held Bash prompt with Claude Code's three rows.
func bashPrompt() mcp.HeldPrompt {
	return mcp.HeldPrompt{ID: "p6", Session: "live", Tool: "Bash", Summary: "make deploy", Options: []mcp.HeldOption{
		{Choice: "allow", Label: "Yes"},
		{Choice: "always:1", Label: "Yes, and don't ask again for Bash(make deploy) in this project"},
		{Choice: "deny", Label: "No, and tell Claude what to do differently"},
	}}
}

// permPane is a held permission prompt's session selected, its menu shown
// and past the first-appearance pause. One arrow key there and back first:
// the first key after the model is made wakes it, and its command comes
// back in a batch that run doesn't unpack.
func permPane(t *testing.T, h mcp.HeldPrompt) (model, *[]heldCall, *bool) {
	t.Helper()
	tick := menuClock(t)
	m, calls, typed := paneModel(t, h)
	m.showMenu(m.heldShown())
	tick(time.Second)
	m = keyType(keyType(m, tea.KeyDown), tea.KeyUp)
	return m, calls, typed
}

// The bottom pane draws Claude Code's permission menu and answers it with
// no Enter first and no y/n: ↑/↓ (j/k) move, Enter sends the row, a digit
// sends its row, A yes, D no; a letter it doesn't use goes to the search,
// Esc leaves it for the list; nothing is typed into a terminal.
func TestHeldPaneAnswersAPermission(t *testing.T) {
	m, calls, typed := permPane(t, bashPrompt())
	if !m.heldKeysActive() || m.chatFocus {
		t.Fatalf("the held prompt has the keys at once: active %v chat %v", m.heldKeysActive(), m.chatFocus)
	}
	v := strip2(m.View())
	for _, want := range []string{"Bash · permission", "make deploy", "❯ 1. Yes", "2. Yes, and don't ask again for Bash(make deploy)", "3. No, and tell Claude what to do differently", "enter send", "a yes", "keys: menu"} {
		if !strings.Contains(v, want) {
			t.Errorf("the pane lacks %q:\n%s", want, v)
		}
	}
	m = press(press(m, "j"), "j")
	m = press(m, "k")
	if m.textInput.Value() != "" || !strings.Contains(strip2(m.View()), "❯ 2. Yes, and") {
		t.Fatalf("j and k move the menu: search %q\n%s", m.textInput.Value(), strip2(m.View()))
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].choice != "always:1" {
		t.Fatalf("Enter sends the highlighted row at once: %+v", *calls)
	}
	m, calls, _ = permPane(t, bashPrompt())
	if m = press(m, "1"); len(*calls) != 1 || (*calls)[0].choice != "allow" {
		t.Fatalf("a digit sends its row at once: %+v", *calls)
	}
	m, calls, _ = permPane(t, bashPrompt())
	if m = press(m, "A"); len(*calls) != 1 || (*calls)[0].choice != "allow" {
		t.Fatalf("A allows at once: %+v", *calls)
	}
	m, calls, _ = permPane(t, bashPrompt())
	if m = press(m, "d"); len(*calls) != 1 || (*calls)[0].choice != "deny" || len((*calls)[0].notes) != 0 {
		t.Fatalf("D denies at once, with no note: %+v", *calls)
	}
	m, calls, typed = permPane(t, bashPrompt())
	if m = keyType(m, tea.KeyEsc); m.heldKeysActive() {
		t.Error("Esc leaves it for the list")
	}
	m = press(m, "a")
	if m.textInput.Value() != "a" || len(*calls) != 0 {
		t.Errorf("after Esc, letters go to the search: %q %+v", m.textInput.Value(), *calls)
	}
	m = selectLive(keyType(m, tea.KeyBackspace)) // the search empty again, back on it
	m = keyType(m, tea.KeyEnter)
	if !m.heldKeysActive() {
		t.Fatal("Enter gives it the keys again")
	}
	m = press(m, "x")
	if m.textInput.Value() != "x" || len(*calls) != 0 {
		t.Errorf("a letter it doesn't use goes to the search: %q %+v", m.textInput.Value(), *calls)
	}
	if *typed {
		t.Error("nothing is ever typed into a terminal")
	}
}

// No, and tell Claude: its row opens a box; Enter sends a deny with what
// was typed as its note, an empty box a plain no; Esc closes the box and
// keeps the draft.
func TestHeldPermissionDenyNote(t *testing.T) {
	m, calls, _ := permPane(t, bashPrompt())
	m = press(m, "3")
	if len(*calls) != 0 || !strings.Contains(strip2(m.View()), "send no") {
		t.Fatalf("No opens its box first: %+v\n%s", *calls, strip2(m.View()))
	}
	for _, r := range "use make test" {
		m = press(m, string(r))
	}
	m = keyType(m, tea.KeyEsc)
	if !m.heldKeysActive() || len(*calls) != 0 || m.textInput.Value() != "" {
		t.Fatalf("Esc closes the box only: active %v calls %+v", m.heldKeysActive(), *calls)
	}
	m = keyType(m, tea.KeyEnter) // the cursor is still on No: the box again, with the draft
	if v := strip2(m.View()); !strings.Contains(v, "use make test") {
		t.Fatalf("the draft is kept:\n%s", v)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].choice != "deny" || (*calls)[0].notes[0] != "use make test" {
		t.Fatalf("Enter sends no with the note: %+v", *calls)
	}
	m, calls, _ = permPane(t, bashPrompt())
	m = keyType(press(m, "3"), tea.KeyEnter)
	if len(*calls) != 1 || (*calls)[0].choice != "deny" || len((*calls)[0].notes) != 0 {
		t.Fatalf("an empty box sends a plain no: %+v", *calls)
	}
}

// For menuFresh after a prompt first shows, Enter, digits, A and D do
// nothing (arrows still move), so a key meant for something else can't
// approve it; after it they answer.
func TestHeldPermissionFreshGuard(t *testing.T) {
	tick := menuClock(t)
	m, calls, _ := paneModel(t, bashPrompt())
	m.showMenu(m.heldShown())
	m = keyType(m, tea.KeyDown)
	for _, k := range []string{"1", "a", "A", "d", "D"} {
		m = press(m, k)
	}
	m = keyType(m, tea.KeyEnter)
	if len(*calls) != 0 || !strings.Contains(strip2(m.View()), "❯ 2.") {
		t.Fatalf("fresh: nothing sent, arrows move: %+v", *calls)
	}
	tick(menuFresh)
	if m = press(m, "a"); len(*calls) != 1 || (*calls)[0].choice != "allow" {
		t.Fatalf("after the pause: %+v", *calls)
	}
}

// An older ccs serve lists no options: the menu is Yes and No.
func TestHeldPermissionWithoutOptions(t *testing.T) {
	m, calls, _ := permPane(t, mcp.HeldPrompt{ID: "p7", Session: "live", Tool: "Bash", Summary: "ls"})
	if v := strip2(m.View()); !strings.Contains(v, "1. Yes") || !strings.Contains(v, "2. No, and tell Claude") {
		t.Errorf("fallback rows:\n%s", v)
	}
	if m = press(m, "1"); len(*calls) != 1 || (*calls)[0].choice != "allow" {
		t.Errorf("1 allows: %+v", *calls)
	}
}

// A session whose prompt ccs holds is selected once you've stopped typing
// in ccs, its held prompt focused behind the typing guard, whether or not
// ^T A is on; never while typing, with text in the search, or with a popup
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
		t.Error("^T A off: a held prompt is still selected")
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
		t.Fatalf("idle with ^T A on: selected %v, pane focused %v", selected(m), m.heldFocus)
	}
	// The guard: keys still being typed when it came up are dropped.
	_, calls, _ := paneModel(t, mcp.HeldPrompt{ID: "p8", Session: "live", Tool: "Bash", Summary: "ls"})
	*calls = nil
	offset = 300 * time.Millisecond
	m = press(m, "a")
	if len(*calls) != 0 || !strings.Contains(m.heldNote, "paused") {
		t.Fatalf("a key in the first second is dropped: calls %+v note %q", *calls, m.heldNote)
	}
	offset = 3 * time.Second
	if m = press(m, "a"); len(*calls) != 1 || (*calls)[0].choice != "allow" {
		t.Errorf("after the pause, keys answer as usual: %+v", *calls)
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

// Held prompts are selected whether or not ^T A is on, the oldest first,
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
		t.Fatalf("the oldest held first, ^T A off: on %s, pane %v", on(), m.heldFocus)
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

// ^T A never switches to a held prompt's tab: it shows no menu there.
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

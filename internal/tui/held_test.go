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
}

// stubHeld makes ccs serve hold these prompts; answers are recorded.
func stubHeld(t *testing.T, prompts ...mcp.HeldPrompt) *[]heldCall {
	t.Helper()
	var calls []heldCall
	oldList, oldAnswer, oldSock := heldPrompts, answerHeld, controlSock
	heldPrompts = func() ([]mcp.HeldPrompt, error) { return prompts, nil }
	answerHeld = func(id, choice string, labels map[int][]string) (string, error) {
		calls = append(calls, heldCall{id, choice, labels})
		return "done " + choice, nil
	}
	controlSock = func() bool { return true }
	t.Cleanup(func() { heldPrompts, answerHeld, controlSock = oldList, oldAnswer, oldSock })
	return &calls
}

// heldModel is waitModel with live waiting, ccs serve holding p for it, and
// the overlay open on it.
func heldModel(t *testing.T, p mcp.HeldPrompt) (model, *[]heldCall) {
	t.Helper()
	calls := stubHeld(t, p)
	m := waitModel(t)
	m.width, m.height = 100, 40
	m.filtered = m.items
	m = run(m, m.heldCmd())
	m = openByHand(m, map[string]sessions.SessionStat{"live": waiting(time.Now(), "permission prompt")})
	return m, calls
}

// press sends one key and runs whatever command comes back.
func press(m model, k string) model {
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	return run(res.(model), cmd)
}

func TestHeldQuestionAnsweredByNumber(t *testing.T) {
	m, calls := heldModel(t, mcp.HeldPrompt{ID: "p1", Session: "live", Tool: "AskUserQuestion",
		Questions: []mcp.HeldQuestion{{Question: "Which colour?", Options: []string{"Red", "Blue"}}}})
	pop := strip2(m.waitPopup())
	for _, want := range []string{"held by ccs", "Which colour?", "1. Red", "2. Blue", "1-2 answer", "T back to its tab"} {
		if !strings.Contains(pop, want) {
			t.Errorf("overlay lacks %q:\n%s", want, pop)
		}
	}
	m = press(m, "2")
	if len(*calls) != 0 || !strings.Contains(m.waitPopup(), "Answer") {
		t.Fatalf("a digit asks y/n first: calls %+v", *calls)
	}
	if m = press(m, "n"); len(*calls) != 0 {
		t.Fatal("n sends nothing")
	}
	m = press(press(m, "2"), "y")
	if len(*calls) != 1 || (*calls)[0].id != "p1" || (*calls)[0].choice != "answer" || (*calls)[0].labels[0][0] != "Blue" {
		t.Fatalf("calls %+v", *calls)
	}
	if !strings.Contains(m.waitNote, "done answer") || m.waitInput.Value() != "" {
		t.Errorf("outcome %q, box %q", m.waitNote, m.waitInput.Value())
	}
	if m = press(m, "9"); len(*calls) != 1 {
		t.Error("a digit past the options sends nothing")
	}
}

func TestHeldPermissionAsksYesNo(t *testing.T) {
	m, calls := heldModel(t, mcp.HeldPrompt{ID: "p2", Session: "live", Tool: "Bash", Summary: "rm -rf build"})
	if pop := strip2(m.waitPopup()); !strings.Contains(pop, "wants to use Bash rm -rf build") || !strings.Contains(pop, "A allow") || !strings.Contains(pop, "D deny") {
		t.Errorf("overlay:\n%s", pop)
	}
	m = press(m, "A")
	if len(*calls) != 0 || !strings.Contains(strip2(m.waitPopup()), "Allow Bash for") {
		t.Fatalf("A must ask y/n first: %+v", *calls)
	}
	m = press(m, "n")
	if len(*calls) != 0 || m.heldConfirmed != nil {
		t.Errorf("n sends nothing: %+v", *calls)
	}
	m = press(m, "D")
	m = press(m, "y")
	if len(*calls) != 1 || (*calls)[0].choice != "deny" {
		t.Errorf("D then y denies: %+v", *calls)
	}
	m = press(m, "A")
	m = press(m, "y")
	if len(*calls) != 2 || (*calls)[1].choice != "allow" {
		t.Errorf("A then y allows: %+v", *calls)
	}
	m = press(m, "T")
	if len(*calls) != 3 || (*calls)[2].choice != "mac" {
		t.Errorf("T sends it back to its tab: %+v", *calls)
	}
}

// A menu with a pick-any question, or several questions, is answered by
// typing one answer per question; Enter parses it and asks y/n.
func TestHeldTypedAnswers(t *testing.T) {
	h := mcp.HeldPrompt{ID: "p3", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{
		{Question: "Colour?", Options: []string{"Red", "Blue"}},
		{Question: "Checks?", Multi: true, Options: []string{"lint", "tests", "vet"}},
		{Question: "Ship?", Options: []string{"Yes", "No"}},
	}}
	m, calls := heldModel(t, h)
	if pop := strip2(m.waitPopup()); !strings.Contains(pop, "one per question (1+3 for pick-any)") || strings.Contains(pop, "ccs serve page") {
		t.Errorf("overlay:\n%s", pop)
	}
	// In ^X W the answers are the box's text; Enter parses, never sends.
	for _, k := range []string{"2", ",", " ", "1", "+", "3", ",", "1"} {
		m = press(m, k)
	}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = run(res.(model), cmd)
	if m.heldConfirmed == nil || !strings.Contains(m.heldConfirmed.what, "Colour? → Blue · Checks? → lint, vet · Ship? → Yes") || len(*calls) != 0 {
		t.Fatalf("summary %+v, calls %+v", m.heldConfirmed, *calls)
	}
	m = press(m, "y")
	if len(*calls) != 1 || (*calls)[0].choice != "answer" || strings.Join((*calls)[0].labels[1], "|") != "lint|vet" || (*calls)[0].labels[0][0] != "Blue" || (*calls)[0].labels[2][0] != "Yes" {
		t.Errorf("sent %+v", *calls)
	}
}

func TestParseHeldAnswers(t *testing.T) {
	h := mcp.HeldPrompt{Questions: []mcp.HeldQuestion{
		{Question: "A?", Options: []string{"a1", "a2"}},
		{Question: "B?", Multi: true, Options: []string{"b1", "b2", "b3"}},
	}}
	if labels, summary, err := parseHeldAnswers(h, " 2 , 1+3 "); err != nil || labels[0][0] != "a2" || strings.Join(labels[1], ",") != "b1,b3" || summary != "A? → a2 · B? → b1, b3" {
		t.Errorf("good: %v %q %v", labels, summary, err)
	}
	for in, want := range map[string]string{
		"1":     "1 answers for 2 questions",
		"1,1,1": "3 answers for 2 questions",
		"3,1":   "question 1 has options 1-2",
		"1,4":   "question 2 has options 1-3",
		"1+2,1": "question 1 takes one answer",
		"1,x":   "question 2 has options",
		"1,":    "question 2 has options",
	} {
		if _, _, err := parseHeldAnswers(h, in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}

func TestHeldShownInBottomPane(t *testing.T) {
	stubHeld(t, mcp.HeldPrompt{ID: "p4", Session: "live", Tool: "Bash", Summary: "make deploy"})
	m := waitModel(t)
	m.width, m.height = 100, 40
	m = run(m, m.heldCmd())
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m.applyStats(map[string]sessions.SessionStat{"live": waiting(time.Now(), "permission prompt")})
	if v := strip2(m.View()); !strings.Contains(v, "held by ccs") || !strings.Contains(v, "answer it here") {
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
	m, calls := heldModel(t, mcp.HeldPrompt{ID: "p3", Session: "live", Tool: "Bash", Summary: "ls"})
	typed := false
	oldFind, oldSend := sessions.FindTerminal, sessions.SendKeys
	sessions.FindTerminal = func(int) sessions.Terminal { return sessions.Terminal{Kind: "tmux pane", Target: "%1"} }
	sessions.SendKeys = func(sessions.Terminal, string, bool) error { typed = true; return nil }
	t.Cleanup(func() { sessions.FindTerminal, sessions.SendKeys = oldFind, oldSend })
	conv := m.items[0].conv
	for _, it := range m.items {
		if it.conv.SessionID == "live" {
			conv = it.conv
		}
	}
	for _, k := range []string{"1", "5", "9"} {
		if _, done := m.heldKey(k, conv); !done {
			t.Errorf("digit %s must be the held prompt's, never the terminal answer's", k)
		}
	}
	m = press(m, "1")
	if typed || len(*calls) != 0 || m.waitInput.Value() != "" {
		t.Errorf("a digit on a held permission prompt: typed %v, calls %v, box %q", typed, *calls, m.waitInput.Value())
	}
}

// An answer sent whose reply never came is reported as unknown, never failed.
func TestHeldOutcomeUnknown(t *testing.T) {
	m, _ := heldModel(t, mcp.HeldPrompt{ID: "p4", Session: "live", Tool: "Bash", Summary: "ls"})
	m.applyHeldDone(heldDoneMsg{name: "worker", err: mcp.ErrOutcomeUnknown})
	if !strings.Contains(m.waitErr, "reply never came") || strings.Contains(m.waitErr, "Couldn't") {
		t.Errorf("waitErr %q", m.waitErr)
	}
}

// paneModel is waitModel with "live" selected, waiting, its prompt p held by
// ccs serve, the overlay closed, and no terminal typing possible.
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

// The bottom pane answers the selected session's held prompt: Enter focuses
// it, A and D ask y/n, T sends it back, Esc leaves; nothing is typed into a
// terminal.
func TestHeldPaneAnswersAPermission(t *testing.T) {
	m, calls, typed := paneModel(t, mcp.HeldPrompt{ID: "p6", Session: "live", Tool: "Bash", Summary: "make deploy"})
	m = keyType(m, tea.KeyEnter)
	if !m.heldFocus || m.chatFocus {
		t.Fatalf("Enter focuses the held prompt's pane: held %v chat %v", m.heldFocus, m.chatFocus)
	}
	if v := strip2(m.View()); !strings.Contains(v, "back to its tab and open it") {
		t.Errorf("the pane says what Enter does now:\n%s", v)
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
	m = press(m, "x")
	if m.textInput.Value() != "" || len(*calls) != 1 {
		t.Error("other keys are the pane's: none reach the search")
	}
	m = press(m, "t")
	if len(*calls) != 2 || (*calls)[1].choice != "mac" {
		t.Errorf("t sends it back: %+v", *calls)
	}
	if m = keyType(m, tea.KeyEsc); m.heldFocus {
		t.Error("Esc leaves the pane for the search")
	}
	if *typed {
		t.Error("nothing is ever typed into a terminal")
	}
}

func TestHeldPaneAnswersAQuestion(t *testing.T) {
	m, calls, typed := paneModel(t, mcp.HeldPrompt{ID: "p7", Session: "live", Tool: "AskUserQuestion",
		Questions: []mcp.HeldQuestion{{Question: "Which colour?", Options: []string{"Red", "Blue"}}}})
	m = keyType(m, tea.KeyEnter)
	m = press(m, "2")
	if len(*calls) != 0 || !strings.Contains(strip2(m.View()), "2. Blue? y/n") {
		t.Fatalf("a digit asks y/n first: %+v", *calls)
	}
	m = press(m, "y")
	if len(*calls) != 1 || (*calls)[0].choice != "answer" || (*calls)[0].labels[0][0] != "Blue" || *typed {
		t.Errorf("answered by id: %+v typed %v", *calls, *typed)
	}
	m.applyHeldDone(heldDoneMsg{name: "live", err: mcp.ErrOutcomeUnknown})
	if v := strip2(m.View()); !strings.Contains(v, "reply never came") {
		t.Errorf("a lost reply is reported as unknown in the pane:\n%s", v)
	}
}

// With ^X A on, a session that starts waiting is selected once you've
// stopped typing in ccs, its held prompt focused behind the typing guard;
// never with ^X A off, while typing, with text in the search, or with a
// popup open.
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
	if m.autoSelect(); selected(m) {
		t.Error("^X A off: the selection stays")
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
	m.waitOpen = true
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

// The bottom pane takes a held menu's typed answers too: one per question,
// Enter parses (a wrong one says why and sends nothing), y sends.
func TestHeldPaneTypedAnswers(t *testing.T) {
	m, calls, typed := paneModel(t, mcp.HeldPrompt{ID: "p8", Session: "live", Tool: "AskUserQuestion", Questions: []mcp.HeldQuestion{
		{Question: "Colour?", Options: []string{"Red", "Blue"}},
		{Question: "Ship?", Options: []string{"Yes", "No"}},
	}})
	m = keyType(m, tea.KeyEnter)
	for _, k := range []string{"3", ",", "1"} {
		m = press(m, k)
	}
	m = keyType(m, tea.KeyEnter)
	if m.heldConfirmed != nil || len(*calls) != 0 || !strings.Contains(strip2(m.View()), "question 1 has options 1-2") {
		t.Fatalf("a wrong answer says why and sends nothing: %+v", *calls)
	}
	m = keyType(m, tea.KeyBackspace)
	m = keyType(m, tea.KeyBackspace)
	m = keyType(m, tea.KeyBackspace)
	for _, k := range []string{"2", ",", "1"} {
		m = press(m, k)
	}
	m = keyType(m, tea.KeyEnter)
	if m.heldConfirmed == nil || !strings.Contains(m.heldConfirmed.what, "Colour? → Blue · Ship? → Yes") {
		t.Fatalf("summary: %+v", m.heldConfirmed)
	}
	m = press(m, "y")
	if len(*calls) != 1 || (*calls)[0].labels[0][0] != "Blue" || (*calls)[0].labels[1][0] != "Yes" || *typed {
		t.Errorf("sent %+v typed %v", *calls, *typed)
	}
}

// Focus starts unknown: until the terminal reports it, this ccs never
// claims the hold and says so in the header; losing focus lets what it
// holds go to the tabs.
func TestHoldFocus(t *testing.T) {
	t.Cleanup(func() { tuiFocused.Store(false); tuiFocusKnown.Store(false) })
	tuiFocused.Store(false)
	tuiFocusKnown.Store(false)
	made := 0
	old := newTUIHolder
	newTUIHolder = func() *mcp.Holder { made++; return old() }
	t.Cleanup(func() { newTUIHolder = old })
	claimHoldReal()
	if made != 0 || ownHold() != nil {
		t.Fatal("focus unknown: no hold is claimed")
	}
	m := waitModel(t)
	m.width, m.height = 160, 40
	if v := strip2(m.View()); !strings.Contains(v, "not holding: focus unknown") {
		t.Errorf("the header says why it isn't holding:\n%s", v)
	}
	holdFocus(false)
	if !tuiFocusKnown.Load() || tuiFocused.Load() {
		t.Error("a blur report: known, unfocused")
	}
	holdFocus(true)
	if !tuiFocused.Load() {
		t.Error("focused again")
	}
}

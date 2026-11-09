package app

import (
	"bytes"
	"encoding/json"
	tea "github.com/charmbracelet/bubbletea"
	"slices"
	"strings"
	"testing"
)

// parsedSession is a Claude transcript of lines run through parseLine.
func parsedSession(id string, lines ...string) Conversation {
	c := Conversation{SessionID: id}
	for _, l := range lines {
		parseLine(&c, []byte(l))
	}
	finishParse(&c)
	return c
}

func userLine(entry, prompt, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user", "entrypoint": entry, "promptId": prompt, "timestamp": "2026-11-03T10:00:00Z", "cwd": "/w",
		"message": map[string]any{"role": "user", "content": text},
	})
	return string(b)
}

func TestHeadlessRule(t *testing.T) {
	title := `{"type":"custom-title","customTitle":"kept"}`
	for _, c := range []struct {
		name  string
		conv  Conversation
		gone  bool
		about string
	}{
		{"one prompt", parsedSession("a", userLine("sdk-cli", "p1", "grant one")), true, "claude -p given one prompt"},
		{"tool results share it", parsedSession("b", userLine("sdk-cli", "p1", "x"), userLine("sdk-cli", "p1", "y")), true, "lines of the same prompt"},
		{"second prompt", parsedSession("c", userLine("sdk-cli", "p1", "x"), userLine("sdk-cli", "p2", "and then")), false, "someone talked to it"},
		{"titled", parsedSession("d", userLine("sdk-cli", "p1", "x"), title), false, "a name the user gave"},
		{"interactive", parsedSession("e", userLine("cli", "p1", "x")), false, "typed in a terminal"},
		{"T3 Code", parsedSession("f", userLine("sdk-ts", "p1", "x")), false, "an app you type in"},
		{"codex exec", Conversation{SessionID: "g", Kind: kindCodex, codexExec: true}, true, "codex exec"},
		{"codex tui", Conversation{SessionID: "h", Kind: kindCodex}, false, "an interactive codex"},
	} {
		if got := c.conv.headless(); got != c.gone {
			t.Errorf("%s (%s): headless %v, want %v", c.name, c.about, got, c.gone)
		}
	}
}

func headlessModel(convs ...Conversation) model {
	for i := range convs {
		convs[i].Messages = append(convs[i].Messages, Message{Role: "user", Text: "word " + convs[i].SessionID, Ts: "2026-11-03T10:00:00Z"})
		convs[i].LastTimestamp = "2026-11-03T10:00:00Z"
	}
	return initialModel(buildItems(convs), "", nil)
}

func ids(m model) []string {
	var out []string
	for _, it := range m.filtered {
		out = append(out, it.conv.SessionID)
	}
	slices.Sort(out)
	return out
}

func TestHeadlessHiddenFromList(t *testing.T) {
	run := parsedSession("run", userLine("sdk-cli", "p1", "grant"))
	kept := parsedSession("kept", userLine("cli", "p1", "hello"))
	m := headlessModel(run, kept)
	m.width, m.height = 160, 30
	if got := ids(m); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("a headless run is hidden: %v", got)
	}
	if v := strip2(m.View()); strings.Contains(v, "headless") {
		t.Errorf("nothing on screen mentions headless runs: %q", v)
	}
	m.textInput.SetValue("word")
	m.updateFilter()
	if got := ids(m); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("search doesn't find it either: %v", got)
	}

	m = headlessModel(run, kept)
	m.marks = &sessionMarks{Pinned: []string{"run"}}
	m.updateFilter()
	if got := ids(m); !slices.Contains(got, "run") {
		t.Errorf("a pinned run shows: %v", got)
	}

	m = headlessModel(run, kept)
	m.selectSession("run")
	if got := ids(m); !slices.Contains(got, "run") || m.filtered[m.cursor].conv.SessionID != "run" {
		t.Errorf("opening it from a link shows and selects it: %v", got)
	}

	showHeadless = true
	defer func() { showHeadless = false }()
	m = headlessModel(run, kept)
	if got := ids(m); !slices.Equal(got, []string{"kept", "run"}) {
		t.Errorf("--all shows everything: %v", got)
	}
}

func TestHeadlessChildHiddenUnderItsParent(t *testing.T) {
	parent := parsedSession("parent", userLine("cli", "p1", "start it"))
	child := parsedSession("child", userLine("sdk-cli", "p1", "do the thing"))
	orphan := parsedSession("orphan", userLine("sdk-cli", "p1", "do another"))
	m := headlessModel(parent, child, orphan)
	m.parents = map[string]string{"child": "parent"}
	m.noParent = map[string]int{"orphan": len(m.items)}
	m.updateFilter()
	if got := ids(m); !slices.Equal(got, []string{"parent"}) {
		t.Errorf("a headless run goes, nested under a shown session or not: %v", got)
	}
}

func TestMCPSkipsHeadless(t *testing.T) {
	convs := []Conversation{
		parsedSession("aaaa-run", userLine("sdk-cli", "p1", "backtest grant")),
		parsedSession("bbbb-chat", userLine("cli", "p1", "backtest by hand")),
	}
	call := func(req string) string {
		var out bytes.Buffer
		if err := runMCP(strings.NewReader(req+"\n"), &out, func() ([]Conversation, error) { return slices.Clone(convs), nil }); err != nil {
			t.Fatal(err)
		}
		var r map[string]any
		json.Unmarshal(out.Bytes(), &r)
		return toolText(t, r)
	}
	list := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}`)
	if strings.Contains(list, "aaaa-run") || !strings.Contains(list, "bbbb-chat") {
		t.Errorf("list_sessions leaves headless runs out: %s", list)
	}
	search := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_sessions","arguments":{"query":"backtest","include_headless":true}}}`)
	if !strings.Contains(search, "aaaa-run") {
		t.Errorf("include_headless brings them back: %s", search)
	}
	read := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_session","arguments":{"session_id":"aaaa"}}}`)
	if !strings.Contains(read, "backtest grant") {
		t.Errorf("read_session reads any session: %s", read)
	}
}

func TestHeadlessToggleKey(t *testing.T) {
	defer func() { showHeadless = false }()
	run := parsedSession("run", userLine("sdk-cli", "p1", "grant word"))
	kept := parsedSession("kept", userLine("cli", "p1", "hello word"))
	m := headlessModel(run, kept)
	m.width, m.height = 170, 30
	m.textInput.SetValue("word") // narrowing from the shown list would miss the hidden run
	m.updateFilter()
	m, _ = chord(m, "h")
	if got := ids(m); !showHeadless || !slices.Equal(got, []string{"kept", "run"}) {
		t.Errorf("^X H shows headless runs, search kept: %v", got)
	}
	if head := strip2(strings.Split(m.View(), "\n")[0]); strings.Contains(head, "headless") {
		t.Errorf("the header doesn't mention headless runs: %q", head)
	}
	m, _ = chord(m, "h")
	if got := ids(m); showHeadless || !slices.Equal(got, []string{"kept"}) {
		t.Errorf("^X H again hides them: %v", got)
	}
	if head := strip2(strings.Split(m.View(), "\n")[0]); strings.Contains(head, "headless") {
		t.Errorf("the header doesn't mention headless runs: %q", head)
	}
	m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlX}) // ^X pressed: its hints leave H out too
	if head := strip2(strings.Split(m.View(), "\n")[0]); strings.Contains(head, "headless") {
		t.Errorf("the ^X hints don't list it: %q", head)
	}
}

func TestHeadlessToggleSurvivesRestart(t *testing.T) {
	defer func() { showHeadless = false }()
	m := headlessModel(parsedSession("kept", userLine("cli", "p1", "hello")))
	showHeadless = true
	s := m.uiState()
	showHeadless = false
	m.restore(s)
	if !showHeadless {
		t.Error("an update restart keeps headless runs shown")
	}
	showHeadless = true
	m.restore(uiState{}) // from a ccs before ^X H: --all's choice stands
	if !showHeadless {
		t.Error("state without the field leaves the flag's choice")
	}
}

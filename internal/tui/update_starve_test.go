package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/state"
)

// autoModel is a ccs offered v0.25.0 with automatic updates on and you
// away, installing to /bin/ccs.
func autoModel(t *testing.T, installs *int) model {
	t.Helper()
	m := updateModel(t, installs)
	m.autoUpdate, m.blurred, m.updateOpen = true, true, false
	return m
}

// heldQuestion holds a one-question prompt for session s.
func heldQuestion(m *model, id string) {
	if m.heldPrompt == nil {
		m.heldPrompt = map[string]mcp.HeldPrompt{}
	}
	m.heldPrompt["s"] = mcp.HeldPrompt{ID: id, Session: "s", Tool: "AskUserQuestion", At: time.Now(),
		Questions: []mcp.HeldQuestion{{Question: "Which colour?", Options: []string{"Red", "Blue"}}}}
}

// Questions held all the time (a new one always waiting) no longer stop an
// automatic update: once you're away it installs and restarts, logging
// that the held questions are handed back.
func TestHeldQuestionsDontStarveUpdates(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	held, installs := 3, 0
	stubHeldCount(t, &held)
	m := autoModel(t, &installs)
	heldQuestion(&m, "p1")
	m.menuFor(m.heldPrompt["s"]) // shown, nothing picked

	if !m.awayForUpdate() {
		t.Fatal("held questions alone don't stop an update")
	}
	res, cmd := m.Update(autoUpdateTickMsg{})
	if m = res.(model); !m.updating || !m.autoInstalling {
		t.Fatal("away with questions held: install")
	}
	if h := strip2(m.View()); strings.Contains(h, "update waiting") {
		t.Errorf("nothing really waits, so no note:\n%s", h)
	}
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); installs != 1 || !m.quitting || m.restart != "/bin/ccs" {
		t.Fatalf("installed and restarting (installs %d, restart %q)", installs, m.restart)
	}
	if log, _ := os.ReadFile(state.ActivityLogPath); strings.Count(string(log), "update: restarting with 3 held question(s) handed back; they come back after the restart") != 1 {
		t.Errorf("the hand-back is logged once:\n%s", log)
	}
}

// A question half answered (an answer picked, a note typed, a box open,
// past its first question) holds the update, with the reason in the header
// and the log; finishing or abandoning it lets the update go.
func TestHalfAnsweredQuestionHoldsTheUpdate(t *testing.T) {
	for _, c := range []struct {
		name string
		mark func(*heldMenu)
	}{
		{"an answer picked", func(mn *heldMenu) { mn.picks[0] = []int{1} }},
		{"a note typed", func(mn *heldMenu) { mn.notes[0] = "warmer" }},
		{"your own text", func(mn *heldMenu) { mn.other[0] = "green" }},
		{"an Other draft", func(mn *heldMenu) { mn.drafts[0] = "gre" }},
		{"a box open", func(mn *heldMenu) { mn.editing = boxNote }},
		{"past the first question", func(mn *heldMenu) { mn.step = 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func(v string) { state.Version = v }(state.Version)
			state.Version = "0.24.1"
			os.Remove(state.ActivityLogPath)
			held, installs := 1, 0
			stubHeldCount(t, &held)
			m := autoModel(t, &installs)
			heldQuestion(&m, "p1")
			mn := m.menuFor(m.heldPrompt["s"])
			c.mark(mn)

			if m.awayForUpdate() {
				t.Fatal("a half-answered question stops the update")
			}
			res, _ := m.Update(autoUpdateTickMsg{})
			res, _ = res.(model).Update(autoUpdateTickMsg{})
			if m = res.(model); m.updating {
				t.Fatal("no install while it's half answered")
			}
			if h := strip2(m.View()); !strings.Contains(h, "update waiting: a question half answered") {
				t.Errorf("the header names why:\n%s", h)
			}
			if log, _ := os.ReadFile(state.ActivityLogPath); strings.Count(string(log), "update waits: a question half answered") != 1 {
				t.Errorf("the wait is logged once:\n%s", log)
			}

			m.menus["p1"] = newHeldMenu("p1") // abandoned: back to nothing picked
			if res, _ = m.Update(autoUpdateTickMsg{}); !res.(model).updating {
				t.Fatal("abandoned: the update goes ahead")
			}
		})
	}
}

// A menu whose question is no longer held (answered, or gone) doesn't
// count, whatever was picked in it.
func TestAnsweredQuestionsMenuDoesntHoldTheUpdate(t *testing.T) {
	installs := 0
	m := autoModel(t, &installs)
	mn := newHeldMenu("gone")
	mn.picks[0] = []int{0}
	m.menus = map[string]*heldMenu{"gone": mn}
	if why := m.updateBlocker(); why != "" {
		t.Errorf("a menu for a prompt no longer held stops nothing, got %q", why)
	}
}

// Started answering while the automatic install ran: the restart waits for
// that answer, saying so, then goes once it's sent or abandoned.
func TestHalfAnsweredDuringInstallWaitsToRestart(t *testing.T) {
	defer func(v string) { state.Version = v }(state.Version)
	state.Version = "0.24.1"
	held, installs := 1, 0
	stubHeldCount(t, &held)
	m := autoModel(t, &installs)
	heldQuestion(&m, "p1")
	res, cmd := m.Update(autoUpdateTickMsg{})
	if m = res.(model); !m.updating {
		t.Fatal("away, held but untouched: installing")
	}
	m.menuFor(m.heldPrompt["s"]).picks[0] = []int{1} // you start answering meanwhile
	res, _ = m.Update(firstOfBatch(cmd))
	if m = res.(model); m.quitting || m.restartReady != "/bin/ccs" {
		t.Fatalf("installed, restart held for the answer: quitting %v ready %q", m.quitting, m.restartReady)
	}
	if h := strip2(m.View()); !strings.Contains(h, "update waiting: a question half answered") {
		t.Errorf("the header says why:\n%s", h)
	}
	delete(m.heldPrompt, "s") // answered: no longer held
	res, _ = m.Update(autoUpdateTickMsg{})
	if m = res.(model); !m.quitting || m.restart != "/bin/ccs" || installs != 1 {
		t.Errorf("answered: restart into the ready install, without installing again (installs %d)", installs)
	}
}

// The way out (quit, an error, or the restart into an update) hands held
// prompts back with retry, never a final release.
func TestRestartHandsHeldPromptsBack(t *testing.T) {
	old := handBack
	var got []string
	handBack = func(h *mcp.Holder, why string) { got = append(got, why) }
	t.Cleanup(func() { handBack = old })
	tuiHold.Lock()
	tuiHold.h = mcp.NewHolder(tuiHoldConfig())
	tuiHold.Unlock()

	releaseHold("ccs quit")
	if len(got) != 1 || got[0] != "ccs quit" {
		t.Errorf("handed back once with its reason, got %v", got)
	}
	if ownHold() != nil {
		t.Error("the hold is given up")
	}
	releaseHold("again")
	if len(got) != 1 {
		t.Error("nothing more to hand back")
	}
}

package sessions

import (
	"os"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// briefConv is a named session whose first user message is first.
func briefConv(id, name, first string) transcript.Conversation {
	return transcript.Conversation{
		SessionID: id, Title: name, IsCustomTitle: name != "",
		LastTimestamp: time.Now().UTC().Format(time.RFC3339),
		Messages:      []transcript.Message{{Role: "user", Text: first}, {Role: "assistant", Text: "ok"}},
	}
}

const handoff = "Parent session: `istiod-upgrade`. Report back with SendMessage to `istiod-upgrade` when done, or if blocked.\n\n**Goal:** …"

func freshParents(t *testing.T) {
	t.Helper()
	os.Remove(ParentsPath())
	t.Cleanup(func() { os.Remove(ParentsPath()) })
}

func TestBriefParent(t *testing.T) {
	for text, want := range map[string]string{
		handoff:                                 "istiod-upgrade",
		"Parent session: `a.b_c-1`.":            "a.b_c-1",
		"Parent session: `x`":                   "", // no full stop after it
		"hi\nParent session: `istiod-upgrade`.": "", // not at the start
		" Parent session: `istiod-upgrade`.":    "", // exactly at the start
		"Parent session: `bad name`.":           "", // not a session name
		"Parent session: istiod-upgrade.":       "",
	} {
		got, ok := BriefParent(text)
		if got != want || ok != (want != "") {
			t.Errorf("BriefParent(%q) = %q %v, want %q", text, got, ok, want)
		}
	}
}

func TestRecordBriefParents(t *testing.T) {
	freshParents(t)
	parent := briefConv("p-id", "istiod-upgrade", "upgrade istiod")
	child := briefConv("c-id", "argocd-charts", handoff)
	midText := briefConv("m-id", "mid", "please read this:\n"+handoff)
	unknown := briefConv("u-id", "orphan", "Parent session: `nobody-by-that-name`. Report back …")
	added, err := RecordBriefParents([]transcript.Conversation{parent, child, midText, unknown})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Fatalf("only the brief's child, under a known session: %+v", added)
	}
	r := LoadParents().Parents["argocd-charts"]
	if r.Parent != "istiod-upgrade" || r.ParentID != "p-id" || r.ChildID != "c-id" || r.Source != "brief" || r.At.IsZero() {
		t.Errorf("recorded by name and id, as a brief: %+v", r)
	}
	if got := ResolveParents([]transcript.Conversation{parent, child}, LoadParents())["c-id"]; got != "p-id" {
		t.Errorf("nested under its parent: %q", got)
	}
	if again, _ := RecordBriefParents([]transcript.Conversation{parent, child}); len(again) != 0 {
		t.Errorf("recorded once: %+v", again)
	}
}

// A record from the SessionStart hook, a launch or an adoption always wins.
func TestBriefNeverOverridesARecord(t *testing.T) {
	freshParents(t)
	parent := briefConv("p-id", "istiod-upgrade", "x")
	other := briefConv("o-id", "orchestrator", "x")
	child := briefConv("c-id", "argocd-charts", handoff)
	SetParents([]ParentRec{{Child: "argocd-charts", ChildID: "c-id", Parent: "orchestrator", ParentID: "o-id", Source: "start"}})
	if added, _ := RecordBriefParents([]transcript.Conversation{parent, other, child}); len(added) != 0 {
		t.Errorf("the hook's record stands: %+v", added)
	}
	if r := LoadParents().Parents["argocd-charts"]; r.Parent != "orchestrator" || r.Source != "start" {
		t.Errorf("untouched: %+v", r)
	}
}

// `ccs adopt <name> --to none` sticks: the brief doesn't put it back, and a
// later explicit record clears the detach.
func TestBriefRespectsADetach(t *testing.T) {
	freshParents(t)
	parent := briefConv("p-id", "istiod-upgrade", "x")
	child := briefConv("c-id", "argocd-charts", handoff)
	if _, err := Adopt("argocd-charts", "none"); err != nil {
		t.Fatal(err)
	}
	if added, _ := RecordBriefParents([]transcript.Conversation{parent, child}); len(added) != 0 {
		t.Errorf("detached stays detached: %+v", added)
	}
	SetParents([]ParentRec{{Child: "argocd-charts", Parent: "istiod-upgrade", ParentID: "p-id"}})
	if _, ok := LoadParents().Detached["argocd-charts"]; ok {
		t.Error("an adoption afterwards ends the detach")
	}
}

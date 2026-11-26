package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

func writeAgyTail(t *testing.T, mod time.Time, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

// An agy session's status clock: a running turn's runs from its prompt, not
// its latest step (agy writes each step's record as the step ends); a
// finished turn's from when its last record was written, the turn's end; a
// question asked with ask_question is waiting until its result arrives.
func TestAgyStatClock(t *testing.T) {
	const id = "0e5a1b2c-0000-4000-8000-0000000000c1"
	prompt := `{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"2026-03-02T10:00:00Z","content":"go"}`
	step := `{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-03-02T10:00:05Z","tool_calls":[{"name":"run_command","args":{"CommandLine":"\"x\""}}]}`
	mod := time.Date(2026, 3, 2, 10, 1, 0, 0, time.UTC)

	st := AgyStat(id, writeAgyTail(t, mod, prompt, step))
	if st.Status != "busy" || !st.Since.Equal(time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("running: got %s since %v, want busy since the prompt", st.Status, st.Since)
	}

	final := `{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-03-02T10:00:30Z","content":"done"}`
	st = AgyStat(id, writeAgyTail(t, mod, prompt, step, final))
	if st.Status != "idle" || !st.Since.Equal(mod) {
		t.Errorf("finished: got %s since %v, want idle since %v", st.Status, st.Since, mod)
	}

	ask := `{"step_index":2,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-03-02T10:00:30Z","tool_calls":[{"name":"ask_question","args":{"questions":"[]"}}]}`
	st = AgyStat(id, writeAgyTail(t, mod, prompt, step, ask))
	if st.Status != "waiting" || st.WaitingFor != "input needed" {
		t.Errorf("asking: got %+v, want waiting", st)
	}
}

func TestAgyPromptAt(t *testing.T) {
	got := agyPromptAt(transcript.AgySummary{LastUserInput: "2026-11-26 13:49:03.566256+00:00"})
	if want := time.Date(2026, 11, 26, 13, 49, 3, 566256000, time.UTC); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if !agyPromptAt(transcript.AgySummary{LastUserInput: ""}).IsZero() {
		t.Error("empty: want zero")
	}
}

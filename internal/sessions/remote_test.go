package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// remoteControlSetting sets ^T V for the test and puts the settings back.
func remoteControlSetting(t *testing.T, on bool) {
	t.Helper()
	old, err := os.ReadFile(state.SettingsPath)
	t.Cleanup(func() {
		if err != nil {
			os.Remove(state.SettingsPath)
		} else {
			os.WriteFile(state.SettingsPath, old, 0o600)
		}
	})
	if err := state.SetRemoteControl(on); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteControlSettingKeepsOtherKeys(t *testing.T) {
	os.WriteFile(state.SettingsPath, []byte(`{"table":true,"feature_stats":false}`), 0o600)
	t.Cleanup(func() { os.Remove(state.SettingsPath) })
	if state.RemoteControl() {
		t.Error("off unless chosen")
	}
	if err := state.SetRemoteControl(true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(state.SettingsPath)
	if !state.RemoteControl() || !strings.Contains(string(b), `"table":true`) || !strings.Contains(string(b), `"feature_stats":false`) {
		t.Errorf("on, with every other key kept: %s", b)
	}
	os.WriteFile(state.SettingsPath, []byte(`{"remote_control":"yes"}`), 0o600)
	if state.RemoteControl() {
		t.Error("only a JSON true turns it on")
	}
}

func TestRemoteControlArgs(t *testing.T) {
	remoteControlSetting(t, false)
	if a := state.RemoteControlArgs("worker", false); a != nil {
		t.Errorf("setting off and not asked for: nothing, got %v", a)
	}
	if a := state.RemoteControlArgs("worker", true); !slices.Equal(a, []string{"--remote-control", "worker"}) {
		t.Errorf("asked for: named after the session, got %v", a)
	}
	if a := state.RemoteControlArgs("-x", true); a != nil {
		t.Errorf("a name that isn't a session name (read as an option) gives nothing, got %v", a)
	}
	remoteControlSetting(t, true)
	if a := state.RemoteControlArgs("worker", false); !slices.Equal(a, []string{"--remote-control", "worker"}) {
		t.Errorf("setting on: named, got %v", a)
	}
}

func TestLaunchScriptNamesRemoteControl(t *testing.T) {
	remoteControlSetting(t, false)
	dir := "/c/launch/n"
	got := launchScript(LaunchOpts{Name: "n", Brief: "do it", Cwd: "/w"}, "/bin/claude", dir)
	if strings.Contains(got, "--remote-control") {
		t.Errorf("off: no Remote Control flag:\n%s", got)
	}
	got = launchScript(LaunchOpts{Name: "n", Resume: "abc", Cwd: "/w", RemoteControl: true}, "/bin/claude", dir)
	if !strings.Contains(got, `--name 'n' '--remote-control' 'n' ${=CCS_CLAUDE_ARGS}`) {
		t.Errorf("asked for: --remote-control with its name, before CCS_CLAUDE_ARGS:\n%s", got)
	}
	remoteControlSetting(t, true)
	got = launchScript(LaunchOpts{Name: "n", Brief: "do it", Cwd: "/w"}, "/bin/claude", dir)
	if !strings.Contains(got, `--name 'n' '--remote-control' 'n' ${=CCS_CLAUDE_ARGS}`) {
		t.Errorf("^T V on: every Claude launch gets it:\n%s", got)
	}
	for _, o := range []LaunchOpts{{Name: "n", Brief: "x", Cwd: "/w", Codex: true}, {Name: "n", Brief: "x", Cwd: "/w", Agy: true}} {
		if got := launchScript(o, "/bin/x", dir); strings.Contains(got, "remote-control") {
			t.Errorf("Claude Code only:\n%s", got)
		}
	}
}

func TestResumeRemoteControlArgs(t *testing.T) {
	remoteControlSetting(t, true)
	named := transcript.Conversation{SessionID: "s", Title: "fix-login", IsCustomTitle: true}
	if a := ResumeRemoteControlArgs(named, nil); !slices.Equal(a, []string{"--remote-control", "fix-login"}) {
		t.Errorf("a named session resumes under its name, got %v", a)
	}
	if a := ResumeRemoteControlArgs(named, []string{"--fork-session"}); a != nil {
		t.Errorf("never a fork: it's a new conversation, got %v", a)
	}
	if a := ResumeRemoteControlArgs(transcript.Conversation{SessionID: "s", Title: "An AI title"}, nil); a != nil {
		t.Errorf("no name of its own: Claude Code's title stays, got %v", a)
	}
	remoteControlSetting(t, false)
	if a := ResumeRemoteControlArgs(named, nil); a != nil {
		t.Errorf("off: nothing, got %v", a)
	}
}

func TestRestartKeepsRemoteControlOnUnderItsName(t *testing.T) {
	remoteControlSetting(t, false)
	r := newRestartFixture(t)
	body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"name":"worker","status":"idle","statusUpdatedAt":%d,"messagingSocketPath":"/tmp/none.sock","bridgeSessionId":"session_01abc"}`,
		r.pid, r.id, time.Now().Add(-time.Hour).UnixMilli())
	os.WriteFile(filepath.Join(GetSessionsDir(), fmt.Sprintf("%d.json", r.pid)), []byte(body), 0o600)
	if _, err := RestartSession(RestartOpts{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	if len(r.launches) != 1 || !r.launches[0].RemoteControl {
		t.Errorf("a session with Remote Control on comes back with it on, named, whatever ^T V says: %+v", r.launches)
	}
}

func TestRestartLeavesRemoteControlOffWhenItWasOff(t *testing.T) {
	remoteControlSetting(t, false)
	r := newRestartFixture(t) // its session file has no bridgeSessionId
	if _, err := RestartSession(RestartOpts{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	if len(r.launches) != 1 || r.launches[0].RemoteControl {
		t.Errorf("off before, off after: %+v", r.launches)
	}
}

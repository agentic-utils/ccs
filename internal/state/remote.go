package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// RemoteControlKey is the settings.json key for ^X V: Claude Code sessions
// ccs starts or resumes get Remote Control on, under their ccs name.
const RemoteControlKey = "remote_control"

// RemoteControl reports whether ^X V is on. Off unless chosen.
func RemoteControl() bool {
	b, err := os.ReadFile(SettingsPath)
	if err != nil {
		return false
	}
	var s map[string]json.RawMessage
	if json.Unmarshal(b, &s) != nil {
		return false
	}
	var on bool
	return json.Unmarshal(s[RemoteControlKey], &on) == nil && on
}

// SetRemoteControl writes RemoteControlKey into settings.json, keeping every
// other key as it is.
func SetRemoteControl(on bool) error {
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(SettingsPath); err == nil {
		json.Unmarshal(b, &raw)
	}
	raw[RemoteControlKey], _ = json.Marshal(on)
	data, _ := json.Marshal(raw)
	dir := filepath.Dir(SettingsPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "settings-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o600)
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), SettingsPath)
}

// RemoteControlArgs are the claude flags that turn Remote Control on under
// name: `--remote-control <name>`. The name has to be given here: Claude
// Code ignores the session's --name or /rename title for Remote Control when
// it has suppressed the conversation's history (one started under another
// organisation, say after a cswap switch), and falls back to
// <hostname>-<random words> or a title made from a message. An explicit
// Remote Control name is the one thing it always uses. Nil when neither on
// (ask for it) nor RemoteControl() is set, or name isn't a session name.
func RemoteControlArgs(name string, on bool) []string {
	if !SessionName.MatchString(name) || !(on || RemoteControl()) {
		return nil
	}
	return []string{"--remote-control", name}
}

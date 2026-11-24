package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProfileCCSRefusesBadArgumentsAndListsNone(t *testing.T) {
	if _, err := Call("profile_ccs", json.RawMessage(`{"kind":"goroutine"}`), nil); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("kind goroutine: %v", err)
	}
	if _, err := Call("profile_ccs", json.RawMessage(`{"seconds":301}`), nil); err == nil || !strings.Contains(err.Error(), "seconds must be") {
		t.Errorf("seconds 301: %v", err)
	}
	if got, err := Call("profile_ccs", json.RawMessage(`{"list":true}`), nil); err != nil || got != "no running ccs takes profiles" {
		t.Errorf("list with none: %q, %v", got, err)
	}
	if _, err := Call("profile_ccs", json.RawMessage(`{}`), nil); err == nil || !strings.Contains(err.Error(), "no running ccs tui") {
		t.Errorf("no TUI running: %v", err)
	}
}

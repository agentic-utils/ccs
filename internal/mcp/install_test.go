package mcp

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// fakeAgents stubs the agent CLIs: lists says what each "mcp get/list"
// prints before an add and after; ran records every command.
type fakeAgents struct {
	onPath map[string]bool
	has    map[string]bool
	weird  map[string]bool // its list prints something unparseable
	ran    []string
}

func (f *fakeAgents) run(bin string, args ...string) ([]byte, error) {
	cmd := bin + " " + strings.Join(args, " ")
	f.ran = append(f.ran, cmd)
	switch {
	case len(args) > 1 && args[1] == "add":
		f.has[bin] = true
		return nil, nil
	case f.weird[bin]:
		return []byte("something else entirely"), nil
	case bin == "claude":
		if f.has[bin] {
			return []byte("ccs:\n  Status: ✓ Connected"), nil
		}
		return []byte(`No MCP server named "ccs".`), errors.New("exit status 1")
	case bin == "codex":
		if f.has[bin] {
			return []byte(`[{"name":"ccs"}]`), nil
		}
		return []byte(`[{"name":"other"}]`), nil
	case bin == "agy":
		out := "NAME   TYPE   STATUS   COMMAND/URL\nlinear http   enabled  https://x\n"
		if f.has[bin] {
			out += "ccs    stdio  enabled  ccs mcp\n"
		}
		return []byte(out), nil
	}
	return nil, errors.New("unknown")
}

func installTest(t *testing.T, f *fakeAgents) {
	t.Helper()
	oldRun, oldLook := installRun, installLookBin
	t.Cleanup(func() { installRun, installLookBin = oldRun, oldLook })
	installRun = f.run
	installLookBin = func(bin string) (string, error) {
		if f.onPath[bin] {
			return "/bin/" + bin, nil
		}
		return "", errors.New("not found")
	}
}

func adds(ran []string) []string {
	var out []string
	for _, r := range ran {
		if strings.Contains(r, " mcp add ") {
			out = append(out, r)
		}
	}
	return out
}

func TestInstallAddsOnlyWhatsMissing(t *testing.T) {
	f := &fakeAgents{onPath: map[string]bool{"claude": true, "codex": true, "agy": true}, has: map[string]bool{"codex": true}}
	installTest(t, f)
	var out, errOut bytes.Buffer
	if code := RunInstall([]string{"--claude", "--codex", "--agy"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	got := adds(f.ran)
	if len(got) != 2 || !strings.HasPrefix(got[0], "claude mcp add --scope user ccs -- ") || !strings.HasPrefix(got[1], "agy mcp add ccs ") || !strings.HasSuffix(got[1], " mcp") {
		t.Fatalf("want claude and agy added through their own mcp add, got %q", got)
	}
	for _, want := range []string{"Codex already has an MCP server named ccs", "added to claude and connected", "added to agy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output should say %q: %s", want, out.String())
		}
	}
}

func TestInstallPrintChangesNothing(t *testing.T) {
	f := &fakeAgents{onPath: map[string]bool{"agy": true}, has: map[string]bool{}}
	installTest(t, f)
	var out, errOut bytes.Buffer
	if code := RunInstall([]string{"--agy", "--print"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if len(adds(f.ran)) != 0 || !strings.HasPrefix(out.String(), "agy mcp add ccs ") {
		t.Fatalf("--print only prints: ran %q, printed %q", f.ran, out.String())
	}
}

func TestInstallNeverOverAnUnknown(t *testing.T) {
	f := &fakeAgents{onPath: map[string]bool{"agy": true}, has: map[string]bool{}, weird: map[string]bool{"agy": true}}
	installTest(t, f)
	var out, errOut bytes.Buffer
	if code := RunInstall([]string{"--agy"}, &out, &errOut); code != 1 {
		t.Fatalf("an answer it can't read should fail, got %d", code)
	}
	if len(adds(f.ran)) != 0 || !strings.Contains(errOut.String(), "couldn't tell") {
		t.Fatalf("nothing is added over an unknown: ran %q, said %q", f.ran, errOut.String())
	}
}

func TestInstallNeedsAnAgentOnPath(t *testing.T) {
	f := &fakeAgents{onPath: map[string]bool{}, has: map[string]bool{}}
	installTest(t, f)
	var out, errOut bytes.Buffer
	if code := RunInstall([]string{"--codex"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "codex isn't on PATH") {
		t.Fatalf("got %d %q", code, errOut.String())
	}
	if code := RunInstall(nil, &out, &errOut); code != 2 {
		t.Fatalf("no agent named is a usage error, got %d", code)
	}
	if len(f.ran) != 0 {
		t.Errorf("ran %q", f.ran)
	}
}

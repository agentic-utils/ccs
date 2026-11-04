package main

import (
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func writePNG(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
}

func TestPastedImagePaths(t *testing.T) {
	dir := t.TempDir()
	spaced := filepath.Join(dir, "Screenshot 2026 at 17.20.png")
	writePNG(t, spaced)
	home, _ := os.UserHomeDir()
	for in, want := range map[string][]string{
		`/tmp/x/Screenshot\ 2026\ at\ 17.20.png`: {"/tmp/x/Screenshot 2026 at 17.20.png"},
		`'/tmp/a b.PNG'`:                         {"/tmp/a b.PNG"},
		`"/tmp/a b.jpg" /tmp/c.webp`:             {"/tmp/a b.jpg", "/tmp/c.webp"},
		"/tmp/a.png\n/tmp/b.gif\n":               {"/tmp/a.png", "/tmp/b.gif"},
		"file:///tmp/a%20b.png":                  {"/tmp/a b.png"},
		"~/shot.jpeg":                            {filepath.Join(home, "shot.jpeg")},
		spaced:                                   {spaced}, // bare spaces, only when it exists
		"/tmp/gone with spaces.png":              nil,
		"/tmp/notes.txt":                         nil,
		"see /tmp/a.png":                         nil,
		"relative.png":                           nil,
		"fix the bug":                            nil,
		"/tmp/a.png and more":                    nil,
	} {
		if got := pastedImagePaths(in); !slices.Equal(got, want) {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestWithImagesAndStrip(t *testing.T) {
	images := []pastedImage{{1, "/p/a.png"}, {2, "/p/b.png"}, {3, "/p/deleted.png"}}
	text := "[Image #1] and [Image #2] and [Image #1] again, [Image #9] is typed"
	wire := withImages(text, images)
	if strings.Contains(wire, "\n") {
		t.Errorf("must stay on one line for typing: %q", wire)
	}
	if !strings.Contains(wire, "[Image #1] /p/a.png, [Image #2] /p/b.png]") || strings.Contains(wire, "deleted") {
		t.Errorf("wire: %q", wire)
	}
	if got := stripImageNote(wire); got != text {
		t.Errorf("strip: %q", got)
	}
	if got := withImages("no images", images); got != "no images" {
		t.Errorf("got %q", got)
	}
	// through the socket's wrapper, as peerParts sees it
	from, body, ok := peerParts("<cross-session-message from-name=\"" + ccsSender + "\">\n" + ccsNote + wire + "\n</cross-session-message>")
	if !ok || from != ccsSender || body != text {
		t.Errorf("peerParts: %q %q %v", from, body, ok)
	}
}

func TestCopyPastedImage(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Screenshot 1.png")
	writePNG(t, src)
	dst, err := copyPastedImage(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil { // macOS deleting the temp file
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil || info.Mode().Perm() != 0o600 || filepath.Dir(dst) != pastedDir() || strings.Contains(dst, " 1") {
		t.Errorf("copy %q: %v %v", dst, info, err)
	}
	fake := filepath.Join(t.TempDir(), "fake.png")
	os.WriteFile(fake, []byte("not an image at all"), 0o600)
	if _, err := copyPastedImage(fake); err == nil {
		t.Error("named .png but isn't one")
	}
}

func TestSweepPastedDir(t *testing.T) {
	dir := t.TempDir()
	old, fresh := filepath.Join(dir, "old.png"), filepath.Join(dir, "new.png")
	os.WriteFile(old, nil, 0o600)
	os.WriteFile(fresh, nil, 0o600)
	when := time.Now().Add(-pastedImageAge - time.Hour)
	os.Chtimes(old, when, when)
	sweepPastedDir(dir)
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Error("an old copy should go")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a new copy stays")
	}
}

func paste(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true}
}

// From the search, a pasted screenshot lands in the selected live session's
// box as a placeholder, sends with its path behind it, and the copy outlives
// the temp file; sending clears it for the next.
func TestPasteLandsInLiveSessionAndSends(t *testing.T) {
	m := chatModel(t)
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })
	m.syncChatDraft()
	id := "live"
	src := filepath.Join(t.TempDir(), "TemporaryItems", "Screen Shot.png")
	os.MkdirAll(filepath.Dir(src), 0o700)
	writePNG(t, src)
	r, _ := m.Update(paste(strings.ReplaceAll(src, " ", `\ `)))
	m = r.(model)
	os.Remove(src)
	if m.textInput.Value() != "" {
		t.Errorf("the path went into the search: %q", m.textInput.Value())
	}
	if !m.chatFocus || m.chatInput.Value() != "[Image #1] " {
		t.Fatalf("box %q focus %v", m.chatInput.Value(), m.chatFocus)
	}
	r, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = r.(model)
	if m.chatInput.Value() != "[Image #1] x" {
		t.Errorf("typing continues after it: %q", m.chatInput.Value())
	}
	wire := withImages(m.chatInput.Value(), m.pasted[id].images)
	path := m.pasted[id].images[0].path
	if !strings.Contains(wire, path) {
		t.Fatalf("wire %q", wire)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the copy should be there: %v", err)
	}
	r, _ = m.Update(sendDoneMsg{id: id, text: "[Image #1] x", note: "✓ Sent via its message socket"})
	m = r.(model)
	if len(m.pasted[id].images) != 0 {
		t.Error("sent images should be forgotten")
	}
	m.chatInput.SetValue("")
	writePNG(t, src)
	r, _ = m.Update(paste(src))
	m = r.(model)
	if m.chatInput.Value() != "[Image #2] " {
		t.Errorf("numbers keep rising: %q", m.chatInput.Value())
	}
	// Nothing delivered: the text comes back, so its image must stay.
	r, _ = m.Update(sendDoneMsg{id: id, text: "[Image #2]", err: errUnsent, note: "✗"})
	m = r.(model)
	if len(m.pasted[id].images) != 1 {
		t.Error("an unsent image should stay")
	}
}

func TestPasteNotImageOrNoSession(t *testing.T) {
	m := chatModel(t)
	m.cursor = 1
	m.syncChatDraft()
	r, _ := m.Update(paste("/tmp/some/notes.txt"))
	m = r.(model)
	if strings.Contains(m.chatInput.Value(), "Image") || m.textInput.Value() != "/tmp/some/notes.txt" {
		t.Errorf("plain text stays plain: box %q search %q", m.chatInput.Value(), m.textInput.Value())
	}
	m.textInput.SetValue("")
	m.updateFilter()
	m.cursor = slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == "live" })

	gone := "/var/folders/x/TemporaryItems/NSIRD/Screenshot.png"
	r, _ = m.Update(paste(gone))
	m = r.(model)
	if m.textInput.Value() != "" || !strings.Contains(m.errorMsg, "gone") {
		t.Errorf("gone temp file: search %q error %q", m.textInput.Value(), m.errorMsg)
	}

	// A session that isn't open: told so, and the search stays clean.
	m.live = nil
	src := filepath.Join(t.TempDir(), "a.png")
	writePNG(t, src)
	r, _ = m.Update(paste(src))
	m = r.(model)
	if m.textInput.Value() != "" || !strings.Contains(m.errorMsg, "open in claude") {
		t.Errorf("search %q error %q", m.textInput.Value(), m.errorMsg)
	}
}

func TestCtrlVClipboard(t *testing.T) {
	old := clipboardImage
	defer func() { clipboardImage = old }()
	m := chatModel(t)
	m.cursor = 1
	m.syncChatDraft()
	path := filepath.Join(pastedDir(), "clip.png")
	os.MkdirAll(pastedDir(), 0o700)
	writePNG(t, path)
	clipboardImage = func() (string, error) { return path, nil }
	r, _ := m.Update(clipImageCmd())
	m = r.(model)
	if m.chatInput.Value() != "[Image #1] " || !m.chatFocus {
		t.Errorf("box %q", m.chatInput.Value())
	}
	// With no image there, ^V is the usual text paste.
	clipboardImage = func() (string, error) { return "", nil }
	if _, then := m.Update(clipImageCmd()); then == nil {
		t.Error("should fall back to a text paste")
	}
}

// A real send carries the path over the socket; the session's transcript line
// reads back as the placeholder, and the pending message clears.
func TestImageNoteReachesSocketAndClearsPending(t *testing.T) {
	wire := withImages("[Image #1] look", []pastedImage{{1, "/p/a.png"}})
	line, _ := json.Marshal("<cross-session-message from-name=\"" + ccsSender + "\">\n" + ccsNote + wire + "\n</cross-session-message>")
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"user","isMeta":true,"cwd":"/p","message":{"content":` + string(line) + `},"timestamp":"2026-09-29T10:01:00Z"}` + "\n"
	os.WriteFile(path, []byte(body), 0o600)
	c, err := parseConversationFile(path, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	plain := strip2(strings.Join(buildPreviewLines(*c, "", 80), "\n"))
	if !strings.Contains(plain, "[Image #1] look") || strings.Contains(plain, "/p/a.png") {
		t.Errorf("preview:\n%s", plain)
	}
	typed := Conversation{Messages: []Message{{Role: "user", Text: wire}}}
	plain = strip2(strings.Join(buildPreviewLines(typed, "", 80), "\n"))
	if strings.Contains(plain, "/p/a.png") {
		t.Errorf("a typed one hides it too:\n%s", plain)
	}
}

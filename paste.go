package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// A pasted image shows as [Image #N] in the message box and in the transcript.
// A session's message socket takes text only, so the session gets the path of
// a copy, in a note after the text, and opens it with its own tools. The copy
// is taken at the paste: macOS deletes its screenshot temp files within moments.

const (
	maxPastedImage = 30 << 20
	pastedImageAge = 30 * 24 * time.Hour
	imageNoteStart = "[Pasted images, read each file to see it: "
)

var (
	imagePlaceholder = regexp.MustCompile(`\[Image #(\d+)\]`)
	imageNote        = regexp.MustCompile(`(?s)\s*` + regexp.QuoteMeta(imageNoteStart) + `.*\]\s*$`)
	imageExt         = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp)$`)
	sweepPasted      sync.Once
)

type pastedImage struct {
	n    int
	path string
}

// sessionImages is what a session's message boxes hold; next keeps numbers
// rising through the session, as Claude Code's do.
type sessionImages struct {
	next   int
	images []pastedImage
}

// clipImageMsg is ^V's look at the clipboard; path is "" with no image there.
type clipImageMsg struct {
	path string
	err  error
}

func pastedDir() string { return filepath.Join(filepath.Dir(settingsPath), "pasted") }

// stripImageNote is a sent message as it was typed, without the paths behind
// its placeholders.
func stripImageNote(text string) string {
	if !strings.Contains(text, imageNoteStart) {
		return text
	}
	return imageNote.ReplaceAllString(text, "")
}

// withImages adds the note naming the files behind text's placeholders. It
// stays on one line: a message typed into a terminal can't hold a newline.
func withImages(text string, images []pastedImage) string {
	var named []string
	for _, n := range placeholderNumbers(text) {
		if i := slices.IndexFunc(images, func(p pastedImage) bool { return p.n == n }); i >= 0 {
			named = append(named, fmt.Sprintf("[Image #%d] %s", n, images[i].path))
		}
	}
	if len(named) == 0 {
		return text
	}
	return text + " " + imageNoteStart + strings.Join(named, ", ") + "]"
}

func placeholderNumbers(text string) []int {
	var out []int
	for _, m := range imagePlaceholder.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// pastedImagePaths reads pasted text as image files: one or more paths as a
// terminal pastes or drops them (shell-escaped or quoted, or file:// addresses),
// or one path with bare spaces. Nil unless all of it is image paths.
func pastedImagePaths(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) < 5 || len(s) > 8192 || !strings.ContainsAny(s[:1], `/~'"f`) {
		return nil
	}
	var out []string
	for _, tok := range shellWords(s) {
		p := imagePath(tok)
		if p == "" {
			out = nil
			break
		}
		out = append(out, p)
	}
	if out != nil {
		return out
	}
	if p := imagePath(s); p != "" && !strings.Contains(s, "\n") {
		if _, err := os.Stat(p); err == nil {
			return []string{p}
		}
	}
	return nil
}

// imagePath is tok as an absolute path if it names an image by its extension.
func imagePath(tok string) string {
	if rest, ok := strings.CutPrefix(tok, "file://"); ok {
		var err error
		if tok, err = url.PathUnescape(rest); err != nil {
			return ""
		}
	}
	if rest, ok := strings.CutPrefix(tok, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		tok = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(tok) || !imageExt.MatchString(tok) {
		return ""
	}
	return tok
}

// shellWords splits s as a shell would: on bare whitespace, with backslashes
// and quotes kept out of the words.
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	in, quote := false, rune(0)
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' {
				esc = true
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			esc, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words
}

// newPastedFile is a fresh private path in pastedDir, clearing out old copies
// the first time.
func newPastedFile(ext string) (string, error) {
	dir := pastedDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sweepPasted.Do(func() { go sweepPastedDir(dir) })
	var b [8]byte
	rand.Read(b[:])
	return filepath.Join(dir, hex.EncodeToString(b[:])+ext), nil
}

func sweepPastedDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > pastedImageAge {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// copyPastedImage copies src to pastedDir, checking by its bytes it's an image.
func copyPastedImage(src string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a file")
	}
	if info.Size() > maxPastedImage {
		return "", fmt.Errorf("over %dMB", maxPastedImage>>20)
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[http.DetectContentType(head[:n])]
	if ext == "" {
		return "", errors.New("not an image")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	dst, err := newPastedFile(ext)
	if err != nil {
		return "", err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, io.LimitReader(f, maxPastedImage))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return "", err
	}
	return dst, nil
}

// clipboardImage writes the clipboard's image to a new file; "" with none there.
var clipboardImage = func() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", nil
	}
	dst, err := newPastedFile(".png")
	if err != nil {
		return "", err
	}
	_, err = runBounded(5*time.Second, nil, "osascript",
		"-e", "on run argv",
		"-e", "set d to the clipboard as «class PNGf»",
		"-e", "set f to open for access POSIX file (item 1 of argv) with write permission",
		"-e", "write d to f",
		"-e", "close access f",
		"-e", "end run", dst)
	if err != nil { // osascript fails the same way for "no image there"
		os.Remove(dst)
		return "", nil
	}
	return dst, nil
}

func clipImageCmd() tea.Msg {
	path, err := clipboardImage()
	return clipImageMsg{path: path, err: err}
}

// imageBox is the message box a pasted image goes to: the overlay's, or the
// selected live session's whichever box has focus. why says why there's none.
func (m *model) imageBox() (in *textinput.Model, id, why string) {
	switch {
	case m.waitOpen:
		return &m.waitInput, m.waitID, ""
	case len(m.filtered) == 0 || !m.selectedLive():
		return nil, "", "Only a session open in claude (●) can receive an image"
	case m.boxHidden():
		return nil, "", "It's waiting on you: answer its question first (" + keyEnter + " opens its tab)"
	}
	return &m.chatInput, m.filtered[m.cursor].conv.SessionID, ""
}

// takesImages: no prompt or popup has the keyboard, so a paste can be an image.
func (m model) takesImages() bool {
	return !m.chordX && !m.prompting() && !m.showUsage && !slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open })
}

// attachImage puts an image already in pastedDir into its box as a placeholder.
func (m *model) attachImage(path string) {
	in, id, why := m.imageBox()
	if in == nil {
		os.Remove(path)
		m.errorMsg = why
		return
	}
	if m.pasted == nil {
		m.pasted = make(map[string]*sessionImages)
	}
	box := m.pasted[id]
	if box == nil {
		box = &sessionImages{next: 1}
		m.pasted[id] = box
	}
	n := box.next
	box.next++
	box.images = append(box.images, pastedImage{n: n, path: path})

	v, pos := []rune(in.Value()), in.Position()
	text := fmt.Sprintf("[Image #%d] ", n)
	if pos > 0 && v[pos-1] != ' ' {
		text = " " + text
	}
	in.SetValue(string(v[:pos]) + text + string(v[pos:]))
	in.SetCursor(pos + len([]rune(text)))
	if !m.waitOpen {
		m.chatFocus = true
	}
}

// pasteImages handles text pasted as image paths; false leaves it to be text.
func (m *model) pasteImages(text string) bool {
	paths := pastedImagePaths(text)
	if paths == nil {
		return false
	}
	if in, _, why := m.imageBox(); in == nil {
		m.errorMsg = why
		return true
	}
	var copies []string
	for _, p := range paths {
		dst, err := copyPastedImage(p)
		switch {
		case err == nil:
			copies = append(copies, dst)
		case errors.Is(err, os.ErrNotExist) && strings.Contains(p, "/TemporaryItems/"):
			m.errorMsg = "That screenshot has already gone from disk: copy it again and press ^V"
			return true
		case errors.Is(err, os.ErrNotExist) || err.Error() == "not an image" || err.Error() == "not a file":
			for _, c := range copies {
				os.Remove(c)
			}
			return false // only named like an image: it's text
		default:
			m.errorMsg = "Couldn't take that image: " + err.Error()
		}
	}
	for _, c := range copies {
		m.attachImage(c)
	}
	return true
}

// dropSentImages forgets the images text sent, keeping any in other drafts.
func (m *model) dropSentImages(id, text string) {
	box := m.pasted[id]
	if box == nil {
		return
	}
	sent := placeholderNumbers(text)
	box.images = slices.DeleteFunc(box.images, func(p pastedImage) bool { return slices.Contains(sent, p.n) })
}

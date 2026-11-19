package transcript

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// hasControl reports a rune CleanText must never let through: a C0 control
// other than newline and tab, DEL, a C1 control, or invalid UTF-8.
func hasControl(s string) bool {
	return !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r < 0xa0)
	})
}

func TestCleanTextC1(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text is unchanged", "hello, world", "hello, world"},
		{"emoji and accents stay", "café ☕ 🙂 naïve — 日本", "café ☕ 🙂 naïve — 日本"},
		{"newline and tab stay", "a\tb\nc", "a\tb\nc"},
		{"7-bit CSI colour", "a\x1b[31mred\x1b[0mb", "aredb"},
		{"carriage return dropped", "50%\r100%", "50%100%"},
		{"C1 CSI as UTF-8, whole sequence", "a\u009b2Jb", "ab"},
		{"C1 CSI colour as UTF-8", "a\u009b31mred\u009b0mb", "aredb"},
		{"C1 OSC hyperlink as UTF-8", "a\u009d8;;http://x\u009cb\u009d8;;\u009cc", "abc"},
		{"C1 OSC ended by BEL", "a\u009d0;title\u0007b", "ab"},
		{"lone C1 NEL", "a\u0085b", "ab"},
		{"raw C1 CSI byte, whole sequence", "a\x9b2Jb", "ab"},
		{"raw C1 CSI colour", "a\x9b31mred\x9b0mb", "aredb"},
		{"raw C1 OSC byte, whole sequence", "a\x9d0;x\x07b", "ab"},
		{"raw C1 OSC ended by raw ST", "a\x9d8;;http://x\x9cb", "ab"},
		{"box drawing (continuation bytes 0x80-0x9f) stays", "┌─┐ │ └─┘ ▸ ◆ ✻", "┌─┐ │ └─┘ ▸ ◆ ✻"},
		{"invalid UTF-8", "a\xff\xfeb", "a�b"},
		{"cut multibyte rune", "ab\xc3", "ab�"},
		{"DEL dropped", "a\x7fb", "ab"},
		{"mixed", "ok ✓\u009b1m bold\u009b0m \x1b]8;;http://y\x1b\\link\x1b]8;;\x1b\\ 🙂", "ok ✓ bold link 🙂"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CleanText(c.in)
			if got != c.want {
				t.Errorf("CleanText(%q) = %q, want %q", c.in, got, c.want)
			}
			if hasControl(got) {
				t.Errorf("controls left in %q", got)
			}
		})
	}
}

// The topic (a title, else the first prompt) is drawn in the list, the
// preview header and prompts: it's cleaned like any transcript text.
func TestGetTopicIsCleaned(t *testing.T) {
	for _, c := range []Conversation{
		{Title: "deploy\u009b2J\x1b[31m now\x1b[0m"},
		{Messages: []Message{{Role: "user", Text: "deploy\x9b2J\x1b]0;x\x07 now"}}},
	} {
		if got := GetTopic(c); got != "deploy now" {
			t.Errorf("GetTopic = %q, want %q", got, "deploy now")
		}
	}
	if got := GetTopic(Conversation{Title: "café 🙂 fix"}); got != "café 🙂 fix" {
		t.Errorf("a clean title changed: %q", got)
	}
}

// A clean string comes back as the same string, without copying: the fast
// path is what every transcript render takes.
func TestCleanTextFastPathReturnsTheSameString(t *testing.T) {
	s := strings.Repeat("plain transcript text, café 🙂\n", 10)
	if got := CleanText(s); got != s {
		t.Fatalf("changed %q", got)
	}
	allocs := testing.AllocsPerRun(100, func() { _ = CleanText(s) })
	if allocs != 0 {
		t.Errorf("the fast path allocates %v times", allocs)
	}
}

func benchText(withEscapes bool) string {
	var b strings.Builder
	line := "Here's what changed: the watcher now reads every page of checks, café 🙂 and reviews.\n"
	for b.Len() < 10<<10 {
		if withEscapes && b.Len()%2000 < len(line) {
			b.WriteString("\x1b[32mok\x1b[0m \u009b1mbold\u009b0m\r\n")
		}
		b.WriteString(line)
	}
	return b.String()
}

func BenchmarkCleanTextClean10KB(b *testing.B) {
	s := benchText(false)
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		_ = CleanText(s)
	}
}

func BenchmarkCleanTextEscapes10KB(b *testing.B) {
	s := benchText(true)
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		_ = CleanText(s)
	}
}

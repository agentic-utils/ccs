package transcript

import (
	"os"
	"testing"
)

// BenchmarkParseTranscript parses a real transcript whole, as a full scan
// does: set CCS_BENCH_TRANSCRIPT to a .jsonl to run it (skipped otherwise,
// since real transcripts aren't in the repo).
func BenchmarkParseTranscript(b *testing.B) {
	path := os.Getenv("CCS_BENCH_TRANSCRIPT")
	if path == "" {
		b.Skip("set CCS_BENCH_TRANSCRIPT to a transcript")
	}
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(info.Size())
	for b.Loop() {
		if _, err := ParseConversationUncached(path, info); err != nil {
			b.Fatal(err)
		}
	}
}

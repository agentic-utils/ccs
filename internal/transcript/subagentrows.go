package transcript

import (
	"os"
	"strings"
	"sync/atomic"
)

// KindSubagent is a Claude Code subagent (Agent tool, async or a teammate)
// listed as a row of its own under the session running it: its transcript
// is <session>/subagents/agent-<id>.jsonl, which GetConversations never
// reads. Rows are read-only: a subagent has no session file or socket.
const KindSubagent = "subagent"

// SubagentConv is s, one of parent's subagents, as a Conversation: SessionID
// s.ID, Kind KindSubagent, ParentID the parent's session, titled by its
// meta.json. With prev (the last read of it) only the lines appended since
// are parsed (ParseAppended); without, a copy cached at the file's size and
// time is reused, else it's read whole once.
func SubagentConv(prev *Conversation, parent Conversation, s Subagent) (*Conversation, error) {
	if prev != nil && prev.FilePath == s.Path {
		return ParseAppended(prev)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		return nil, err
	}
	ParseCacheMu.Lock()
	cached, ok := ParseCache[s.Path]
	ParseCacheMu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		if cached.conv == nil {
			return nil, nil
		}
		c := *cached.conv
		return &c, nil
	}
	return parseSubagentWhole(s.Path, parent.SessionID, s.Name, parent.Cwd)
}

// SubagentReads counts whole reads of subagent transcripts; tests use it to
// see a row isn't read again until its file changes.
var SubagentReads atomic.Int64

// parseSubagentWhole reads a subagent's transcript whole and caches it in
// ParseCache by its size and time, like any transcript.
func parseSubagentWhole(path, parentID, name, cwd string) (*Conversation, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	SubagentReads.Add(1)
	conv, err := ParseConversationUncached(path, info)
	if err != nil {
		return nil, err
	}
	if conv != nil {
		conv.Kind, conv.ParentID, conv.Spawned = KindSubagent, parentID, true
		if name != "" {
			conv.Title = name // meta.json's, cleaned as it was read
		}
		if conv.Cwd == "unknown" && cwd != "" {
			conv.Cwd = cwd
		}
		conv.SearchText = SearchTextOf(*conv) // with its kind and title
		conv.SearchLower = strings.ToLower(conv.SearchText)
	}
	ParseCacheMu.Lock()
	ParseCache[path] = parsedFile{info.Size(), info.ModTime(), conv}
	ParseCacheMu.Unlock()
	return conv, nil
}

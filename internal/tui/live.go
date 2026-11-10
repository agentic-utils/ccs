package tui

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/transcript"
)

type liveTickMsg struct{}

type focusDoneMsg struct {
	pid   int
	found bool
	gone  bool   // the session wasn't running any more, so nothing was opened
	name  string // with gone
}

// resumeDoneMsg reports how Enter/Ctrl+F opened a conversation: in a new
// tab/window (ccs stays up), or not at all (exec in place after quitting).
type resumeDoneMsg struct {
	conv   transcript.Conversation
	fork   bool
	opened bool
	err    error
}

type liveMsg struct {
	live       map[string]bool
	pids       map[string]int
	updated    []transcript.Conversation       // live conversations whose transcript grew
	unknown    []string                        // live sessions not in the list yet
	stats      map[string]sessions.SessionStat // each live session's status, from its session file
	unverified map[string]bool                 // live sessions whose pid couldn't be checked against the file's procStart
	scan       map[string]sessions.LiveSession // this tick's sessions, the next tick's prev
	hosts      map[string]string               // live sessions run by an app, not in a terminal: the app's name
	bg         map[string]sessions.BgSession   // background sessions (claude --bg); nil when not asked this tick
	gen        int
}

func liveTick() tea.Cmd {
	return tea.Tick(sessions.LiveInterval, func(time.Time) tea.Msg { return liveTickMsg{} })
}

// liveCmd refreshes liveness and the content of live sessions off the UI
// goroutine. Sessions that were live last tick are checked too, so a
// session's final lines land promptly after claude exits.
func (m model) liveCmd(askAgents bool) tea.Cmd {
	byID := make(map[string]transcript.Conversation, len(m.live))
	for _, item := range m.items {
		byID[item.conv.SessionID] = item.conv
	}
	wasLive, prev, gen, record := maps.Clone(m.live), m.lastScan, m.gen, m.reopenChecked
	return func() tea.Msg {
		scanned := sessions.ScanSessions(prev)
		live := make(map[string]bool, len(scanned))
		msg := liveMsg{live: live, pids: make(map[string]int, len(scanned)), gen: gen, stats: make(map[string]sessions.SessionStat, len(scanned)), unverified: make(map[string]bool), scan: scanned}
		if askAgents {
			msg.bg, _ = sessions.ListAgents() // nil on failure (no claude, or too old): the last answer stands
		}
		msg.hosts = map[string]string{}
		for id, s := range scanned {
			live[id], msg.pids[id] = true, s.PID
			msg.stats[id] = s.Stat
			if h := sessions.HostOf(s.PID, s.Start); h != "" {
				msg.hosts[id] = h
			}
			if s.Unverified {
				msg.unverified[id] = true
			}
		}
		sessions.NoteCodexExecs(scanned)
		if record { // only once the last boot's record has been read
			sessions.RecordOpen(live)
		}
		check := make(map[string]bool, len(live)+len(wasLive))
		for id := range live {
			check[id] = true
		}
		for id := range wasLive {
			check[id] = true
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for id := range check {
			conv, ok := byID[id]
			if !ok {
				if live[id] {
					msg.unknown = append(msg.unknown, id)
				}
				continue
			}
			// A read still stuck from an earlier tick (e.g. a hung mount)
			// isn't started again; the rest carry on.
			if _, busy := sessions.LiveReads.LoadOrStore(conv.FilePath, true); busy {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer sessions.LiveReads.Delete(conv.FilePath)
				defer transcript.RecoverWorker()
				if c, err := transcript.ParseAppended(&conv); err == nil && c != nil && c.Size != conv.Size {
					mu.Lock()
					msg.updated = append(msg.updated, *c)
					mu.Unlock()
				}
			}()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(sessions.LiveInterval): // deliver what's ready; a straggler's cached result is picked up next tick
		}
		mu.Lock()
		defer mu.Unlock()
		msg.updated = slices.Clone(msg.updated)
		return msg
	}
}

// applyLive swaps updated conversations into the list, re-sorted by last
// activity, keeping the cursor on the same conversation. Unlike a full
// refresh it only re-matches and re-counts the changed rows, so a streaming
// session doesn't rebuild every cache on the UI goroutine each tick.
func (m *model) applyLive(updated []transcript.Conversation) {
	byID := make(map[string]listItem, len(updated))
	for _, item := range buildItems(updated) {
		byID[item.conv.SessionID] = item
	}
	items := make([]listItem, len(m.items))
	newMessages := make(map[string]bool, len(updated))
	for i, item := range m.items {
		// A full scan may have read this file after the live tick did.
		if u, ok := byID[item.conv.SessionID]; ok && !item.conv.ReadAt.After(u.conv.ReadAt) {
			newMessages[item.conv.SessionID] = len(u.conv.Messages) != len(item.conv.Messages)
			u.rank = item.rank
			item = u
		} else {
			delete(byID, item.conv.SessionID)
		}
		items[i] = item
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].conv.LastTimestamp > items[j].conv.LastTimestamp
	})

	var selectedID string
	oldLines := 0
	if len(m.filtered) > 0 {
		selectedID = m.filtered[m.cursor].conv.SessionID
		oldLines = len(m.previewLines())
	}
	shown := make(map[string]bool, len(m.filtered))
	for _, item := range m.filtered {
		shown[item.conv.SessionID] = true
	}
	query := strings.ToLower(m.textInput.Value())
	filtered := make([]listItem, 0, len(m.filtered)+len(updated))
	for _, item := range items {
		match := shown[item.conv.SessionID]
		if _, changed := byID[item.conv.SessionID]; changed {
			match = query == "" || strings.Contains(item.searchLower, query)
			// Most appends are tool calls: HITS only moves with new messages.
			if m.hits != nil && newMessages[item.conv.SessionID] {
				delete(m.hits.byID, item.conv.SessionID)
			}
		}
		if match {
			filtered = append(filtered, item)
		}
	}
	m.items, m.filtered = items, filtered
	m.cursor = min(m.cursor, max(0, len(filtered)-1))
	found := false
	for i, item := range filtered {
		if item.conv.SessionID == selectedID {
			m.cursor, found = i, true
			break
		}
	}
	// The selected preview can shrink (or be a different conversation), so a
	// stored scroll offset past its end would make Ctrl+K seem stuck.
	if _, changed := byID[selectedID]; changed && found && m.previewScroll > 0 {
		// Scrolled back reading: new lines arrive at the bottom, so move the
		// offset by as much, keeping the same lines on screen.
		m.previewScroll += len(m.previewLines()) - oldLines
	}
	if _, changed := byID[selectedID]; changed || !found {
		m.previewScroll = max(0, min(m.previewScroll, m.maxPreviewScroll()))
	}
}

// isLive re-reads the session files so a prune/rename decision never trusts a
// liveness snapshot up to a refresh interval old. A session already marked live
// (e.g. one ccs just opened, before claude writes its session file) stays live
// until the next refresh replaces m.live.
func (m *model) isLive(id string) bool {
	fresh := sessions.ReadLiveSessions()
	if m.live[id] {
		fresh[id] = true
	}
	m.live = fresh
	return fresh[id]
}

// opening reports (and says why) when id mustn't be opened now: one at a
// time, so a double Enter can't open two tabs, and not while a tab ccs just
// opened is still starting (resuming again would start a second copy). A
// fork is a new session, so only the first applies.
func (m *model) opening(id string, fork bool) bool {
	if m.resuming {
		m.errorMsg = "Still opening the last one..."
		return true
	}
	if t, ok := m.opened[id]; ok && !fork && time.Since(t) < sessions.OpenedGrace {
		m.errorMsg = "Already opening in another tab"
		return true
	}
	return false
}

// focusCmd brings a live session's terminal to the front (Enter on an empty
// message box). Unlike resumeCmd it never starts claude: a session that just
// exited, or whose file can't be read right now, is reported instead of
// resumed a second time.
func (m *model) focusCmd(conv transcript.Conversation) tea.Cmd {
	if m.inT3(conv.SessionID) {
		return m.openT3Cmd(conv)
	}
	if m.hostedElsewhere(conv.SessionID) || m.opening(conv.SessionID, false) {
		return nil
	}
	if b, ok := m.bg[conv.SessionID]; ok {
		return m.attachCmd(conv, b)
	}
	m.resuming = true
	m.markSeen(conv.SessionID) // you're going to look at it
	name := m.sessionName(conv)
	return func() tea.Msg {
		pid, ok := sessions.LiveSessionPIDs()[conv.SessionID]
		if !ok {
			return focusDoneMsg{gone: true, name: name}
		}
		return focusDoneMsg{pid: pid, found: sessions.FocusSession(pid, true)}
	}
}

// resumeCmd opens conv off the UI goroutine. Liveness is checked afresh, so
// a session resumed elsewhere since the last live tick is focused, not
// resumed a second time. Forks always open a new session.
func (m *model) resumeCmd(conv transcript.Conversation, fork bool) tea.Cmd {
	if !fork && m.inT3(conv.SessionID) {
		return m.openT3Cmd(conv)
	}
	if !fork && m.hostedElsewhere(conv.SessionID) || m.opening(conv.SessionID, fork) {
		return nil
	}
	if b, ok := m.bg[conv.SessionID]; ok && !fork && m.live[conv.SessionID] {
		return m.attachCmd(conv, b)
	}
	m.resuming = true
	if !fork {
		m.markSeen(conv.SessionID) // you're going to look at it
	}
	flags := slices.Clone(m.claudeFlags)
	if fork {
		flags = append(flags, "--fork-session")
	}
	return func() tea.Msg {
		if !fork {
			if pid, ok := sessions.LiveSessionPIDs()[conv.SessionID]; ok {
				return focusDoneMsg{pid: pid, found: sessions.FocusSession(pid, true)}
			}
		}
		opened, err := sessions.OpenResumeTab(conv, flags)
		return resumeDoneMsg{conv: conv, fork: fork, opened: opened, err: err}
	}
}

type reopenMsg struct{ ids []string }

// reopenCheckCmd reads the record left by the last boot: its sessions that
// aren't open again already are offered for reopening. Nothing on the same
// boot, or with no record. Runs before the first recordOpen overwrites it.
func reopenCheckCmd() tea.Msg {
	data, err := os.ReadFile(sessions.OpenRecordPath)
	var rec sessions.OpenRecord
	if err != nil || json.Unmarshal(data, &rec) != nil {
		return reopenMsg{}
	}
	if boot := sessions.BootID(); boot == "" || rec.Boot == boot {
		return reopenMsg{}
	}
	live := sessions.LiveSessionPIDs()
	var ids []string
	for _, id := range rec.Sessions {
		if _, ok := live[id]; !ok {
			ids = append(ids, id)
		}
	}
	return reopenMsg{ids: ids}
}

type reopenDoneMsg struct {
	opened []string
	err    error // a tab may or may not have opened: stopped there
	noTabs bool  // neither tmux nor iTerm: nothing opened
}

// reopenCmd resumes each session in its own tab, skipping any open again by
// now. It stops at the first failure rather than resuming in place.
func (m *model) reopenCmd(convs []transcript.Conversation) tea.Cmd {
	m.resuming = true
	flags := slices.Clone(m.claudeFlags)
	return func() tea.Msg {
		var done reopenDoneMsg
		live := sessions.LiveSessionPIDs()
		for _, conv := range convs {
			if _, ok := live[conv.SessionID]; ok {
				continue
			}
			opened, err := sessions.OpenResumeTab(conv, flags)
			if err != nil {
				done.err = err
				break
			}
			if !opened {
				done.noTabs = true
				break
			}
			done.opened = append(done.opened, conv.SessionID)
		}
		return done
	}
}

func (m model) reopenPopup() string {
	var b strings.Builder
	n := len(m.reopen)
	what := "the session"
	if n > 1 {
		what = fmt.Sprintf("the %d sessions", n)
	}
	fmt.Fprintf(&b, "\033[1mReopen %s open before the restart?\033[0m\n\n", what)
	nameW := 0
	for _, c := range m.reopen[:min(n, sessions.MaxReopenRows)] {
		nameW = max(nameW, ansi.StringWidth(transcript.Truncate(m.sessionName(c), 40)))
	}
	for _, c := range m.reopen[:min(n, sessions.MaxReopenRows)] {
		b.WriteString(padRight(transcript.Truncate(m.sessionName(c), 40), nameW) + "  \033[90m" + filepath.Base(c.Cwd) + "\033[0m\n")
	}
	if n > sessions.MaxReopenRows {
		fmt.Fprintf(&b, "\033[90m+%d more\033[0m\n", n-sessions.MaxReopenRows)
	}
	b.WriteString("\n\033[90m" + hints(keyEnter, "reopen all in new tabs", keyEsc, "not now") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(b.String())
}

// hostedApp is the app session id runs in when ccs can't open it: no
// terminal, and not a background session (those open with claude attach).
func (m model) hostedApp(id string) string {
	if _, bg := m.bg[id]; bg {
		return ""
	}
	return m.hosts[id]
}

// inT3 reports session id live in T3 Code, which Enter opens there.
func (m model) inT3(id string) bool { return m.live[id] && m.hostedApp(id) == sessions.T3App }

// openT3Cmd opens a session's thread in T3 Code, off the UI goroutine;
// without a link it says to open it there, as for other apps.
func (m *model) openT3Cmd(conv transcript.Conversation) tea.Cmd {
	m.resuming = true
	m.markSeen(conv.SessionID) // you're going to look at it
	name := m.sessionName(conv)
	return func() tea.Msg {
		pid, ok := sessions.LiveSessionPIDs()[conv.SessionID]
		if !ok {
			return focusDoneMsg{gone: true, name: name}
		}
		link, err := sessions.T3Link(pid)
		if err == nil {
			err = sessions.OpenApp(link)
		}
		return t3DoneMsg{err}
	}
}

// t3DoneMsg reports openT3Cmd: nil once T3 Code was asked to show the thread.
type t3DoneMsg struct{ err error }

// hostedElsewhere: session id is live in an app ccs can't switch to; says so.
func (m *model) hostedElsewhere(id string) bool {
	h := m.hostedApp(id)
	if h == "" || !m.live[id] {
		return false
	}
	m.note("In " + h + ": open it there, or message it from its box here")
	return true
}

type attachDoneMsg struct {
	id     string
	opened bool
	err    error
}

// attachCmd opens a background session with claude attach in a new tmux
// window or iTerm tab. Never in place: ccs would be gone.
func (m *model) attachCmd(conv transcript.Conversation, b sessions.BgSession) tea.Cmd {
	m.resuming = true
	m.markSeen(conv.SessionID)
	cwd := conv.Cwd
	if cwd == "" || cwd == "unknown" {
		cwd = "."
	}
	return func() tea.Msg {
		claudePath, err := exec.LookPath("claude")
		if err != nil {
			return attachDoneMsg{id: b.ID}
		}
		args := []string{claudePath, "attach", b.ID}
		if handled, err := sessions.ResumeInTmuxWindow(cwd, args); handled {
			return attachDoneMsg{b.ID, true, err}
		}
		opened, err := sessions.ResumeInITermTab(cwd, args)
		return attachDoneMsg{b.ID, opened, err}
	}
}

type stopDoneMsg struct {
	name string
	err  error
}

// stopCmd stops a background session (^X K, after y): claude stop.
// claude attach with its id restarts it.
func stopCmd(name string, b sessions.BgSession) tea.Cmd {
	return func() tea.Msg {
		_, err := proc.RunBounded(10*time.Second, nil, "claude", "stop", b.ID)
		return stopDoneMsg{name, err}
	}
}

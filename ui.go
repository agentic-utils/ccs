package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// listItem holds display and search data for a conversation
type listItem struct {
	conv        Conversation
	searchText  string // All searchable content
	searchLower string // searchText lowercased once, for case-insensitive filtering
}

// selectedStyle highlights the cursor row. The rest of the UI is rendered with
// raw ANSI escapes in View/formatListItem/renderPreview.
var selectedStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("62")).
	Foreground(lipgloss.Color("230")).
	Bold(true)

// model is the bubbletea application state
type model struct {
	items           []listItem
	filtered        []listItem
	textInput       textinput.Model
	cursor          int
	previewScroll   int // lines scrolled back from the newest message; 0 shows the latest at the bottom
	width           int
	height          int
	listHeight      int // Calculated visible list height
	selected        *Conversation
	fork            bool // exec the selected conversation with --fork-session
	quitting        bool
	claudeFlags     []string
	confirmDelete   bool                 // Are we in delete confirmation mode?
	deleteIndex     int                  // Index of item to delete
	confirmPrune    bool                 // Are we in prune confirmation mode?
	pruneIndex      int                  // Index of item to prune
	pruneSaved      int64                // Bytes the pending prune would reclaim (measured on Ctrl+X)
	renaming        bool                 // Are we typing a new name?
	resuming        bool                 // an Enter/Ctrl+F open is in flight
	opened          map[string]time.Time // sessions ccs opened in a tab, until their session file appears
	renameIndex     int                  // Index of item being renamed
	renameInput     textinput.Model
	errorMsg        string          // Show deletion/prune errors
	preview         *previewCache   // memoised preview lines for the selected conversation
	hits            *hitCounter     // memoised per-query hit counts, keyed by SessionID
	lastFilterQuery string          // lowercased query the current m.filtered was built from
	live            map[string]bool // SessionIDs attached to a running claude process
	livePIDs        map[string]int  // live SessionID -> claude pid (for its session file and socket)

	// Message box for the selected live session (see chat*).
	chatFocus      bool // typing goes to the message box, not the search
	chatInput      textinput.Model
	chatTickOn     bool                   // the fast refresh for the selected live session is running
	chatStatus     map[string]sessionStat // busy/idle per live session, from its session file
	pending        map[string]string      // text sent but not yet seen in the transcript, by SessionID
	sendNote       map[string]string      // outcome of the last send, by SessionID
	sentAt         map[string]time.Time   // when a socket send finished, by SessionID, until the session queues it
	held           map[string]bool        // a receipt said the session is holding the message for approval
	sending        bool
	reload         func() ([]listItem, error) // re-scans conversations; nil disables auto-refresh
	gen            int                        // bumped by delete/prune/rename so an older in-flight refresh can't undo them
	refreshStarted time.Time                  // when the in-flight scan began; zero when none
	lastRefresh    time.Time                  // when the list last matched disk (startup or a completed scan)
	lastKick       time.Time                  // last full scan started early because an unknown session went live
	kicked         map[string]bool            // unknown live sessions an early scan was already started for
	refreshFailed  bool                       // the last scan errored; the list is from lastRefresh

	// Self-update. checkLatest nil disables the check (tests, dev builds);
	// upgrade installs tag and returns the binary to restart; nil means ccs
	// can't update this install itself (e.g. Nix), so only notify.
	checkLatest    func() (string, error)
	upgrade        *upgrader
	progress       *updateProgress // step + start time, written by the upgrade goroutine
	updateTo       string          // newer release tag found, "" if none
	changelog      []string        // what changed from this version up to updateTo, for the popup
	fetchChangelog func(to string) ([]string, error)
	// Ctrl+L changelog: every release in the feed, scrollable.
	notesOpen         bool
	notes             []string // nil until fetched
	notesErr          string
	notesScroll       int
	fetchNotes        func() ([]string, error)
	notesFetching     bool      // a fetch is in flight (started at launch or by Ctrl+L)
	updateShownAt     time.Time // popup ignores keys for a moment so in-flight typing can't answer it
	updateOpen        bool
	updateErr         string // last install failure, shown in the popup with a retry
	updateCheckFailed bool
	updateHeld        bool // popup waited behind another prompt; restart its key grace when it shows
	updating          bool
	dismissed         string // tag the user said "later" to
	restart           string // after an upgrade: binary to exec once the TUI exits

	// Usage screen (Tab).
	showUsage        bool
	usage            usageData
	usageLoading     bool
	usageExclude     []string // project dirs to skip, as for the session list
	allowance        []allowanceLimit
	allowanceErr     error
	allowanceAt      time.Time // last allowance fetch that returned, success or not
	allowanceLoading bool
	allowanceEnabled bool   // fetch the allowance for the header (off in tests)
	account          string // Claude account email, from Claude Code's config
	allowanceStale   bool   // the account changed while a fetch was in flight: fetch again

	// Account switcher (Ctrl+O), backed by the cswap CLI.
	acctOpen bool
	helpOpen bool // Ctrl+G shortcut list
	// Ctrl+T: the links in the visible part of the preview, to open by key.
	linksOpen   bool
	links       []linkItem
	linksCursor int
	accts       []cswapAccount
	acctCursor  int
	acctBusy    bool      // a cswap command is running
	acctMsg     string    // outcome of the last list/switch/add, shown in the popup
	acctPending bool      // opened by a click: the list still needs loading
	lastMouse   time.Time // when the last mouse report arrived, to spot split ones
	linkPending string    // a clicked link, opened by Update (handleMouse can't return a command)
}

// mouseFragment spots the rest of a split mouse report that mouseLeak can't
// tell from typing on its own: "[" after an escape (read as alt+[), or bits of
// "[<64;10;5M" arriving right after a mouse event. Nobody types those that fast.
func (m model) mouseFragment(k tea.KeyMsg) bool {
	s := string(k.Runes)
	if k.Alt && s == "[" {
		return true
	}
	return time.Since(m.lastMouse) < 150*time.Millisecond && strings.Trim(s, "[<;0123456789Mm") == ""
}

// mouseLeak matches a mouse report that arrived split and was read as typed
// text (e.g. "[<65;40;12M" or "<64;10;5"), so it never reaches the search.
var mouseLeak = regexp.MustCompile(`^\x1b?\[?<\d+(;\d+){1,2}[Mm]?$|\[<\d+;\d+;\d+[Mm]`)

// listLayout returns the screen rows View puts the session list and the
// preview on: list rows start at listTop, listHeight of them, and the
// preview starts at previewTop. Must match View.
func (m model) listLayout() (listTop, listHeight, previewTop int) {
	sections := 1
	if m.errorMsg != "" {
		sections++
	}
	listHeight = max(m.height*30/100, 3)
	listTop = sections + 4 // title, sections, blank, column header, rule
	return listTop, listHeight, listTop + listHeight + 1
}

// handleMouse sends the wheel to whatever is under the pointer: the
// conversation preview scrolls, the list moves its selection. A click on a
// list row selects it.
func (m model) handleMouse(msg tea.MouseMsg) model {
	if m.linksOpen {
		return m // keyboard only while it's open
	}
	if m.notesOpen { // the wheel scrolls the changelog, nothing else reacts
		if msg.Button == tea.MouseButtonWheelUp {
			m.scrollNotes(-3)
		} else if msg.Button == tea.MouseButtonWheelDown {
			m.scrollNotes(3)
		}
		return m
	}
	if m.showUsage || m.prompting() || m.updateOpen || m.acctOpen || m.helpOpen || msg.Action != tea.MouseActionPress {
		return m
	}
	if msg.Button == tea.MouseButtonLeft && msg.Y == 1 && cswapPath() != "" {
		if start, end, ok := m.accountSpan(); ok && msg.X >= start && msg.X < end {
			m.acctOpen, m.acctMsg, m.acctCursor = true, "", 0
			m.acctBusy = true
			m.acctPending = true // Update starts the list command (handleMouse can't return one)
			return m
		}
	}
	listTop, listHeight, previewTop := m.listLayout()
	onPreview := msg.Y >= previewTop
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		up := msg.Button == tea.MouseButtonWheelUp
		switch {
		case onPreview && up:
			m.previewScroll = min(m.previewScroll+3, m.maxPreviewScroll()) // back towards older messages
		case onPreview:
			m.previewScroll = max(0, m.previewScroll-3) // forward towards the newest
		case up && m.cursor > 0:
			m.cursor--
			m.previewScroll = 0
			m.selectionMoved()
		case !up && m.cursor < len(m.filtered)-1:
			m.cursor++
			m.previewScroll = 0
			m.selectionMoved()
		}
	case tea.MouseButtonLeft:
		if msg.Y == 1 { // the search row
			m.chatFocus = false
			return m
		}
		if m.chatRows() > 0 && msg.Y >= m.height-3 { // the message box
			m.chatFocus = true
			return m
		}
		if onPreview && len(m.filtered) > 0 {
			// ccs has the mouse, so the terminal never sees a click on a link: open it here.
			lines := strings.Split(m.renderPreview(m.filtered[m.cursor], m.previewRenderHeight()), "\n")
			if row := msg.Y - previewTop; row >= 0 && row < len(lines) {
				m.linkPending = linkAt(lines[row], msg.X)
			}
			return m
		}
		if msg.Y < listTop || msg.Y >= listTop+listHeight {
			return m
		}
		start := 0 // same scroll offset View uses
		if m.cursor >= listHeight {
			start = m.cursor - listHeight + 1
		}
		if i := start + msg.Y - listTop; i < len(m.filtered) && i != m.cursor {
			m.cursor = i
			m.previewScroll = 0
			m.selectionMoved()
		}
	}
	return m
}

// prompting reports an open rename, delete or prune prompt.
func (m model) prompting() bool {
	return m.renaming || m.confirmDelete || m.confirmPrune
}

func initialModel(items []listItem, filterQuery string, claudeFlags []string) model {
	ti := textinput.New()
	ti.Placeholder = "type to search..."
	ti.Prompt = "> "
	ti.Focus()
	// A blinking cursor redraws twice a second forever, which terminals show
	// as constant tab activity; a steady one lets an idle ccs draw nothing.
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.SetValue(filterQuery)
	ti.Width = 40

	chat := textinput.New()
	chat.Prompt = "› "
	chat.CharLimit = 4000
	chat.Cursor.SetMode(cursor.CursorStatic)
	chat.Focus() // only fed keys while chatFocus; drawn without a cursor otherwise

	m := model{
		chatInput:   chat,
		items:       items,
		textInput:   ti,
		claudeFlags: claudeFlags,
		preview:     &previewCache{},
		opened:      make(map[string]time.Time),
		kicked:      make(map[string]bool),
		hits:        &hitCounter{byID: make(map[string]int)},
	}
	m.updateFilter()
	return m
}

func (m *model) updateFilter() {
	queryLower := strings.ToLower(m.textInput.Value())
	if queryLower == "" {
		// Make a copy to avoid sharing backing array with m.items
		m.filtered = make([]listItem, len(m.items))
		copy(m.filtered, m.items)
	} else {
		// Incremental narrowing: if the new query contains the previous one, every
		// item matching the new query already matched the old one, so filter the
		// previous (smaller) result set instead of rescanning every conversation.
		source := m.items
		if m.lastFilterQuery != "" && strings.HasPrefix(queryLower, m.lastFilterQuery) {
			source = m.filtered
		}
		terms := strings.Fields(queryLower)
		next := make([]listItem, 0, len(source))
		for _, item := range source {
			if queryMatches(item.searchLower, item.conv.Title, terms) {
				next = append(next, item)
			}
		}
		// Best matches first (named after the query, then the words as a
		// phrase), newest first within each.
		rank := make(map[string]int, len(next))
		for _, it := range next {
			rank[it.conv.SessionID] = matchRank(it.searchLower, it.conv.Title, terms)
		}
		sort.SliceStable(next, func(i, j int) bool {
			ri, rj := rank[next[i].conv.SessionID], rank[next[j].conv.SessionID]
			if ri != rj {
				return ri < rj
			}
			return next[i].conv.LastTimestamp > next[j].conv.LastTimestamp
		})
		m.filtered = next
	}
	m.lastFilterQuery = queryLower
	// Keep cursor in bounds
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.previewScroll = 0
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.reload != nil {
		cmds = append(cmds, refreshTick(), liveTick(), waitReceipt)
	}
	if m.checkLatest != nil {
		cmds = append(cmds, m.checkUpdateCmd())
	}
	if m.allowanceEnabled {
		cmds = append(cmds, m.allowanceCmd())
	}
	if m.showUsage { // restored onto the usage screen after an update
		cmds = append(cmds, m.usageCmd())
	}
	if m.fetchNotes != nil { // in the background, so Ctrl+L opens instantly
		cmds = append(cmds, m.notesCmd())
	}
	return tea.Batch(cmds...)
}

// Update runs update, then keeps the message box in step with the selection:
// focus only stays in it while the selected session is live, and the fast
// refresh runs only while a live session is selected.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	res, cmd := m.update(msg)
	n := res.(model)
	if n.chatFocus && !n.selectedLive() {
		n.chatFocus = false
	}
	if tick := n.ensureChatTick(); tick != nil {
		return n, tea.Batch(cmd, tick)
	}
	return n, cmd
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Calculate visible list height
		m.listHeight = m.height * 30 / 100
		if m.listHeight < 3 {
			m.listHeight = 3
		}
		// Clear so a shrink doesn't leave wider stale rows behind.
		return m, tea.ClearScreen

	case refreshTickMsg:
		if !m.refreshStarted.IsZero() { // an early scan is running; keep the schedule
			return m, refreshTick()
		}
		return m, m.startRefresh(false)

	case resumeDoneMsg:
		m.resuming = false
		switch {
		case msg.err != nil:
			m.errorMsg = fmt.Sprintf("Opening a new tab timed out, it may still have opened - check before retrying (%v)", msg.err)
		case msg.opened && !msg.fork:
			// Live before claude writes its session file. Copied, not mutated:
			// an in-flight live tick may be reading the old map.
			live := maps.Clone(m.live)
			if live == nil {
				live = make(map[string]bool)
			}
			live[msg.conv.SessionID] = true
			m.live = live
			m.opened[msg.conv.SessionID] = time.Now()
		case !msg.opened && m.updating:
			m.errorMsg = "Update in progress - resume again once it finishes"
		case !msg.opened:
			conv := msg.conv
			m.selected, m.fork, m.quitting = &conv, msg.fork, true
			return m, tea.Quit
		}
		return m, nil

	case focusDoneMsg:
		m.resuming = false
		if !msg.found {
			m.errorMsg = "Session is open in claude but its terminal wasn't found - ^F forks it"
		}
		return m, nil

	case liveTickMsg:
		return m, m.liveCmd()

	case liveMsg:
		m.live = msg.live
		if msg.pids != nil {
			m.livePIDs = msg.pids
		}
		// Keep sessions ccs just opened live until claude's own file shows up.
		for id, t := range m.opened {
			if msg.live[id] || time.Since(t) >= openedGrace {
				delete(m.opened, id)
				continue
			}
			if !m.live[id] {
				m.live = maps.Clone(m.live)
				if m.live == nil {
					m.live = make(map[string]bool)
				}
				m.live[id] = true
			}
		}
		if len(msg.updated) > 0 && msg.gen == m.gen && !m.prompting() {
			m.applyLive(msg.updated)
		}
		cmds := []tea.Cmd{liveTick()}
		// A session that went live but isn't listed (a brand-new conversation):
		// scan now rather than at the next minute, at most every 15s.
		if len(msg.unknown) > 0 && m.refreshStarted.IsZero() && time.Since(m.lastKick) > 15*time.Second {
			// Only for sessions an early scan hasn't already looked for: one
			// the scan can't list (no messages yet, over the size limit) would
			// otherwise trigger a scan every 15s forever.
			fresh := false
			for _, id := range msg.unknown {
				if !m.kicked[id] {
					m.kicked[id], fresh = true, true
				}
			}
			if fresh {
				m.lastKick = time.Now()
				cmds = append(cmds, m.startRefresh(true))
			}
		}
		return m, tea.Batch(cmds...)

	case refreshMsg:
		m.refreshStarted = time.Time{}
		m.refreshFailed = msg.err != nil
		m.live = msg.live // liveness is independent of the list, never stale-dropped
		// Delete/prune/rename prompts hold an index into m.filtered, so don't
		// reshuffle it under them; and a scan that started before a delete,
		// prune or rename would undo it. The next tick catches up.
		if msg.gen == m.gen && !m.prompting() && msg.err == nil {
			m.applyRefresh(refreshMsg{items: keepNewer(m.items, msg.items), live: msg.live})
			m.lastRefresh = time.Now()
		}
		// The allowance (shown in the search row) refreshes with the list;
		// the usage charts only while their screen is open.
		var usageCmds []tea.Cmd
		if m.allowanceEnabled || m.showUsage {
			usageCmds = append(usageCmds, m.allowanceCmd())
		}
		if m.showUsage {
			usageCmds = append(usageCmds, m.usageCmd())
		}
		if msg.early {
			return m, tea.Batch(usageCmds...) // the one-minute chain is still ticking on its own
		}
		return m, tea.Batch(append(usageCmds, refreshTick())...)

	case usageMsg:
		m.usageLoading = false
		m.usage = msg.data
		return m, nil

	case acctListMsg:
		m.acctBusy = false
		if msg.err != nil {
			m.acctMsg = "✗ cswap list: " + msg.err.Error()
			return m, nil
		}
		m.accts = msg.accts
		if m.acctCursor >= len(m.accts) || m.acctCursor < 0 {
			m.acctCursor = 0
		}
		return m, nil

	case acctActionMsg:
		m.acctBusy = false
		if msg.err != nil {
			m.acctMsg = "✗ " + msg.what + ": " + msg.err.Error()
			return m, nil
		}
		m.acctMsg = "✓ " + msg.done
		// The live login changed: re-read the account and allowance now.
		cmds := []tea.Cmd{m.acctListCmd()}
		if m.allowanceLoading {
			m.allowanceStale = true
		} else {
			m.allowanceAt = time.Time{}
			cmds = append(cmds, m.allowanceCmd())
		}
		return m, tea.Batch(cmds...)

	case allowanceMsg:
		m.allowanceLoading = false
		m.allowanceAt = time.Now()
		if m.allowanceStale { // fetched for the previous account: go again
			m.allowanceStale = false
			m.allowanceAt = time.Time{}
			return m, m.allowanceCmd()
		}
		m.allowanceErr = msg.err
		if msg.err == nil {
			m.allowance = msg.limits
		}
		if msg.account != "" {
			m.account = msg.account
		}
		return m, nil

	case updateCheckTickMsg:
		return m, m.checkUpdateCmd()

	case latestMsg:
		m.updateCheckFailed = msg.err != nil
		var fetch tea.Cmd
		if newerVersion(msg.tag, version) && msg.tag != m.dismissed && !m.updating {
			if msg.tag != m.updateTo {
				m.updateOpen = true
				m.updateShownAt = time.Now()
				m.changelog = nil
				if f := m.fetchChangelog; f != nil {
					tag := msg.tag
					fetch = func() tea.Msg {
						lines, err := f(tag)
						if err != nil {
							logUpdate("changelog %s: %v", tag, err)
						}
						return changelogMsg{tag, lines}
					}
				}
			}
			m.updateTo = msg.tag
			if m.upgrade != nil {
				m.upgrade.Prepare(msg.tag) // download now so Enter only has to install
			}
		}
		next := updateCheckInterval
		if msg.err != nil {
			next = updateCheckBackoff
		}
		return m, tea.Batch(fetch, tea.Tick(next, func(time.Time) tea.Msg { return updateCheckTickMsg{} }))

	case notesMsg:
		m.notesFetching = false
		if msg.err != nil {
			m.notesErr = msg.err.Error()
			logUpdate("changelog: %v", msg.err)
		} else {
			m.notes = msg.lines
		}
		return m, nil

	case changelogMsg:
		if msg.tag == m.updateTo {
			m.changelog = msg.lines
		}
		return m, nil

	case updatingTickMsg:
		if m.updating {
			return m, updatingTick() // redraw so the step and seconds stay live
		}
		return m, nil

	case upgradeDoneMsg:
		m.updating = false
		if m.progress != nil {
			logUpdate("install %s: %v in %s", m.updateTo, errOrOK(msg.err), time.Since(m.progress.started).Round(time.Millisecond))
		}
		if msg.err != nil {
			// Keep the offer open with the reason, so the failure can't vanish
			// on the next keypress and Enter retries.
			m.updateErr = msg.err.Error()
			m.updateOpen = true
			m.updateShownAt = time.Now()
			return m, nil
		}
		m.restart = msg.path
		m.quitting = true
		return m, tea.Quit

	case tea.MouseMsg:
		m.lastMouse = time.Now()
		m = m.handleMouse(msg)
		if m.acctPending {
			m.acctPending = false
			return m, m.acctListCmd()
		}
		if u := m.linkPending; u != "" {
			m.linkPending = ""
			return m, openLinkCmd(u)
		}
		return m, nil

	case linkErrMsg:
		m.errorMsg = "Couldn't open the link: " + msg.err.Error()
		return m, nil

	case chatTickMsg:
		return m, m.chatTickCmd()

	case chatTickResult:
		m.chatTickOn = false // rescheduled by Update while the selection stays live
		if msg.stat.status != "" {
			if m.chatStatus == nil {
				m.chatStatus = make(map[string]sessionStat)
			}
			m.chatStatus[msg.id] = msg.stat
		}
		if msg.conv != nil && msg.gen == m.gen && !m.prompting() {
			m.applyLive([]Conversation{*msg.conv})
		}
		m.checkDelivered(msg.id)
		return m, nil

	case receiptMsg:
		m.applyReceipt(msg)
		return m, waitReceipt

	case sendDoneMsg:
		m.sending = false
		if m.sendNote == nil {
			m.sendNote = make(map[string]string)
		}
		m.sendNote[msg.id] = msg.note
		if msg.err != nil {
			delete(m.pending, msg.id)
		} else if strings.Contains(msg.note, "socket") {
			if m.sentAt == nil {
				m.sentAt = make(map[string]time.Time)
			}
			m.sentAt[msg.id] = time.Now()
		}
		m.checkDelivered(msg.id)
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes && (mouseLeak.MatchString(string(msg.Runes)) || m.mouseFragment(msg)) {
			return m, nil // a fragmented mouse report, not typing
		}
		if m.notesOpen { // owns the keyboard until closed
			switch msg.String() {
			case "ctrl+l", "esc":
				m.notesOpen = false
			case "enter":
				if m.notesErr != "" && !m.notesFetching && m.fetchNotes != nil { // retry
					m.notesErr = ""
					return m, m.notesCmd()
				}
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "up", "ctrl+p", "ctrl+k":
				m.scrollNotes(-1)
			case "down", "ctrl+n", "ctrl+j":
				m.scrollNotes(1)
			case "pgup":
				m.scrollNotes(-m.notesRows())
			case "pgdown", " ":
				m.scrollNotes(m.notesRows())
			}
			return m, nil
		}
		if m.linksOpen { // owns the keyboard until closed
			switch k := msg.String(); k {
			case "ctrl+t", "esc":
				m.linksOpen = false
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "up", "ctrl+p", "ctrl+k":
				m.linksCursor = max(m.linksCursor-1, 0)
			case "down", "ctrl+n", "ctrl+j":
				m.linksCursor = min(m.linksCursor+1, len(m.links)-1)
			case "enter":
				m.linksOpen = false
				return m, openLinkCmd(m.links[m.linksCursor].url)
			default:
				if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
					if i := int(k[0] - '1'); i < len(m.links) {
						m.linksOpen = false
						return m, openLinkCmd(m.links[i].url)
					}
				}
			}
			return m, nil
		}
		if msg.String() == "ctrl+t" && !m.prompting() && !m.acctOpen && !m.showUsage {
			m.helpOpen = false
			if m.links = m.visibleLinks(); len(m.links) == 0 {
				m.errorMsg = "No links in view"
				return m, nil
			}
			m.linksOpen, m.linksCursor = true, 0
			return m, nil
		}
		if msg.String() == "ctrl+l" && !m.prompting() && !m.acctOpen {
			m.helpOpen, m.notesOpen, m.notesScroll = false, true, 0
			if m.notes == nil && !m.notesFetching && m.fetchNotes != nil {
				m.notesErr = ""
				return m, m.notesCmd()
			}
			return m, nil
		}
		if m.helpOpen { // owns the keyboard until closed
			switch msg.String() {
			case "ctrl+g", "esc":
				m.helpOpen = false
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			}
			return m, nil
		}
		if m.acctOpen {
			return m.acctKey(msg)
		}
		if msg.String() == "ctrl+g" && !m.prompting() && !m.showUsage {
			m.helpOpen = true
			return m, nil
		}
		if msg.String() == "ctrl+o" && !m.prompting() {
			return m.openAccounts()
		}
		// The popup waits behind rename/delete/prune prompts, so it never takes
		// their keys; once they close, it gets a fresh grace period.
		if m.updateOpen && m.prompting() {
			m.updateHeld = true
		} else if m.updateOpen {
			if m.updateHeld {
				m.updateHeld = false
				m.updateShownAt = time.Now()
			}
			if time.Since(m.updateShownAt) < updateKeyGrace {
				return m, nil
			}
			switch msg.String() {
			case "enter":
				m.updateOpen = false
				if m.upgrade == nil {
					return m, nil
				}
				m.updating = true
				m.updateErr = ""
				m.progress = &updateProgress{step: "starting", started: time.Now()}
				upgrade, tag, progress := m.upgrade, m.updateTo, m.progress
				return m, tea.Batch(func() tea.Msg {
					path, err := upgrade.Install(tag, progress.set)
					return upgradeDoneMsg{path, err}
				}, updatingTick())
			case "esc":
				m.updateOpen = false
				m.dismissed = m.updateTo
				m.updateTo = ""
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			}
			return m, nil
		}
		if m.renaming {
			switch msg.String() {
			case "enter":
				if m.renameIndex < len(m.filtered) && m.isLive(m.filtered[m.renameIndex].conv.SessionID) {
					m.renaming = false
					m.errorMsg = "Session is open in claude - use /rename there"
					return m, nil
				}
				m.renameConversation(strings.TrimSpace(m.renameInput.Value()))
				return m, nil
			case "esc", "ctrl+c":
				m.renaming = false
				return m, nil
			}
			var cmd tea.Cmd
			m.renameInput, cmd = m.renameInput.Update(msg)
			return m, cmd
		}

		// Handle delete confirmation mode
		if m.confirmDelete {
			switch msg.String() {
			case "y", "Y":
				m.deleteConversation()
				return m, nil
			case "n", "N", "esc":
				m.confirmDelete = false
				return m, nil
			}
			return m, nil // Ignore all other keys
		}

		// Handle prune confirmation mode
		if m.confirmPrune {
			switch msg.String() {
			case "y", "Y":
				if m.pruneIndex < len(m.filtered) && m.isLive(m.filtered[m.pruneIndex].conv.SessionID) {
					m.confirmPrune = false
					m.errorMsg = "Session is open in claude - quit it before pruning"
					return m, nil
				}
				m.pruneConversation()
				return m, nil
			case "n", "N", "esc":
				m.confirmPrune = false
				return m, nil
			}
			return m, nil // Ignore all other keys
		}

		// Clear error message on any keypress in normal mode
		if m.errorMsg != "" {
			m.errorMsg = ""
		}

		if msg.String() == "tab" {
			m.showUsage = !m.showUsage
			if m.showUsage {
				return m, tea.Batch(m.usageCmd(), m.allowanceCmd())
			}
			return m, nil
		}
		if m.showUsage && msg.String() != "ctrl+c" {
			return m, nil // the usage screen has no other keys; typing mustn't edit a hidden search
		}

		if m.chatFocus {
			switch msg.String() {
			case "esc":
				m.chatFocus = false // back to the search
				return m, nil
			case "enter":
				if strings.TrimSpace(m.chatInput.Value()) == "" && len(m.filtered) > 0 {
					// Enter on an empty box opens the session itself.
					m.chatFocus = false
					return m, m.resumeCmd(m.filtered[m.cursor].conv, false)
				}
				return m, m.sendCmd()
			case "up", "ctrl+p", "down", "ctrl+n", "pgup", "pgdown", "ctrl+j", "ctrl+k", "ctrl+c", "ctrl+f", "ctrl+d", "ctrl+r", "ctrl+x", "ctrl+]", "ctrl+\\":
				// fall through to the list's handling below
			default:
				var cmd tea.Cmd
				m.chatInput, cmd = m.chatInput.Update(msg)
				return m, cmd
			}
		}

		switch msg.String() {
		case "ctrl+s":
			// Keyboard way into the message box of the selected live session
			// (clicking it is the mouse way).
			if m.selectedLive() && !m.showUsage {
				m.chatFocus = true
			} else if len(m.filtered) > 0 {
				m.errorMsg = "Only a session open in claude (●) can receive messages"
			}
			return m, nil

		case "esc":
			// Esc only ever clears the search; Ctrl+C quits.
			if m.textInput.Value() != "" {
				m.textInput.SetValue("")
				m.updateFilter()
			}
			return m, nil

		case "ctrl+c":
			if m.updating {
				m.errorMsg = "Update in progress - quitting now could leave it half-installed"
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit

		case "enter":
			if len(m.filtered) == 0 {
				m.quitting = true
				return m, tea.Quit
			}
			if m.selectedLive() { // first Enter: the message box; Enter again on it empty opens the session
				m.chatFocus = true
				return m, nil
			}
			return m, m.resumeCmd(m.filtered[m.cursor].conv, false)

		case "ctrl+f":
			// Fork: resume into a new session id, leaving the original as is.
			if len(m.filtered) == 0 {
				return m, nil
			}
			return m, m.resumeCmd(m.filtered[m.cursor].conv, true)

		case "ctrl+d":
			if len(m.filtered) > 0 {
				m.confirmDelete = true
				m.deleteIndex = m.cursor
			}
			return m, nil

		case "ctrl+r":
			if len(m.filtered) > 0 {
				conv := m.filtered[m.cursor].conv
				// A running claude re-appends its own title and would undo ours.
				if m.isLive(conv.SessionID) {
					m.errorMsg = "Session is open in claude - use /rename there"
					return m, nil
				}
				m.renaming = true
				m.renameIndex = m.cursor
				m.renameInput = textinput.New()
				m.renameInput.Prompt = "Rename: "
				m.renameInput.Width = 50
				m.renameInput.SetValue(conv.Title)
				m.renameInput.Focus()
				m.renameInput.Cursor.SetMode(cursor.CursorStatic)
				return m, nil
			}
			return m, nil

		case "ctrl+x":
			if len(m.filtered) > 0 {
				// Prune rewrites the file; a running claude's appends during
				// the rewrite would be lost.
				if m.isLive(m.filtered[m.cursor].conv.SessionID) {
					m.errorMsg = "Session is open in claude - quit it before pruning"
					return m, nil
				}
				// Measure the projected saving so the prompt can show it.
				// ponytail: reads the file once now (and again on confirm) - a
				// multi-GB file briefly blocks, acceptable for a manual action.
				st, err := pruneFile(m.filtered[m.cursor].conv.FilePath, false, pruneOpts{dropSnapshots: true, stripToolResults: true})
				if err != nil {
					m.errorMsg = fmt.Sprintf("Prune preview failed: %v", err)
					return m, nil
				}
				m.confirmPrune = true
				m.pruneIndex = m.cursor
				m.pruneSaved = st.bytesIn - st.bytesOut
			}
			return m, nil

		case "up", "ctrl+p":
			if m.cursor > 0 {
				m.cursor--
				m.previewScroll = 0
				m.selectionMoved()
			}
			return m, nil

		case "down", "ctrl+n":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m.previewScroll = 0
				m.selectionMoved()
			}
			return m, nil

		case "ctrl+]", "ctrl+\\": // next / previous search hit
			m.jumpHit(msg.String() == "ctrl+]")
			return m, nil

		case "pgup", "ctrl+k":
			m.previewScroll = min(m.previewScroll+10, m.maxPreviewScroll()) // back
			return m, nil

		case "pgdown", "ctrl+j":
			m.previewScroll = max(0, m.previewScroll-10) // forward
			return m, nil

		case "ctrl+u":
			m.textInput.SetValue("")
			m.updateFilter()
			return m, nil
		}
	}

	// Update text input
	var cmd tea.Cmd
	prevValue := m.textInput.Value()
	m.textInput, cmd = m.textInput.Update(msg)
	if m.textInput.Value() != prevValue {
		m.updateFilter()
	}
	return m, cmd
}

// View draws the current screen, then any popup on top. Popups are overlaid
// here, not by each screen, so every screen (and any added later) shows them.
func (m model) View() string {
	screen := m.viewScreen()
	if m.updateOpen && !m.prompting() {
		screen = overlayCentre(screen, m.updatePopup(), m.width, m.height)
	}
	if m.acctOpen { // owns the keyboard, so it's drawn on top
		screen = overlayCentre(screen, m.acctPopup(), m.width, m.height)
	}
	if m.helpOpen {
		screen = overlayCentre(screen, m.helpPopup(), m.width, m.height)
	}
	if m.notesOpen {
		screen = overlayCentre(screen, m.notesPopup(), m.width, m.height)
	}
	if m.linksOpen {
		screen = overlayCentre(screen, m.linksPopup(), m.width, m.height)
	}
	return screen
}

// Compact key labels, shared by every hint so they stay consistent.
const (
	keyEnter = "enter" // words, not ⏎ ⇥: those draw wider than a cell in some fonts
	keyTab   = "tab"
	keyEsc   = "esc"
)

// shortcuts is the keys that do something right now on the list, for the
// Ctrl+G popup (the usage screen's header already lists all of its keys): whether a conversation is selected and
// live, and whether the message box has focus.
func (m model) shortcuts() [][2]string {
	var out [][2]string
	add := func(on bool, k, what string) {
		if on {
			out = append(out, [2]string{k, what})
		}
	}
	hasCswap := cswapPath() != ""
	sel := len(m.filtered) > 0
	live := sel && m.selectedLive()
	switch {
	case m.chatFocus:
		add(true, keyEnter, "send; on an empty box, open the session")
		add(true, keyEsc, "back to the search")
	case live:
		add(true, keyEnter, "message it (enter again to open it)")
		add(true, "^S", "message it")
	default:
		add(sel, keyEnter, "resume")
	}
	add(sel, "^F", "fork")
	add(sel && !live, "^R", "rename")
	add(sel, "^D", "delete")
	add(sel, "^X", "prune")
	add(sel, "^J/K", "scroll the conversation")
	add(sel && m.textInput.Value() != "" && len(m.hitLines()) > 0, "^] ^\\", "next / previous search hit")
	add(len(m.filtered) > 1, "↑↓ ^P/N", "move through the list")
	add(true, keyTab, "usage")
	add(!m.chatFocus && m.textInput.Value() != "", keyEsc+" ^U", "clear the search")
	add(hasCswap, "^O", "switch account")
	add(sel && len(m.visibleLinks()) > 0, "^T", "open a link in view")
	add(true, "^L", "changelog")
	add(true, "^C", "quit")
	return out
}

// headerLine fits the title row into width so it never wraps (listLayout
// counts it as one row): hint pairs go from the end, keeping the last keep
// (help, quit), then the title is cut, then the kept hints go too.
func headerLine(width int, title string, pairs []string, keep int) string {
	fits := func(t, h string) bool { return 2+ansi.StringWidth(t)+1+ansi.StringWidth(h) <= width }
	drop := len(pairs)/2 - keep // pairs that may go
	for !fits(title, hints(pairs...)) && drop > 0 {
		i := 2 * (drop - 1)
		pairs = append(pairs[:i:i], pairs[i+2:]...)
		drop--
	}
	help := hints(pairs...)
	if !fits(title, help) {
		title = ansi.Truncate(title, max(width-2-1-ansi.StringWidth(help), 3), "…") + "\033[0m"
	}
	if !fits(title, help) {
		help = ""
		title = ansi.Truncate(title, max(width-3, 1), "…") + "\033[0m"
	}
	pad := max(width-2-ansi.StringWidth(title)-ansi.StringWidth(help), 1)
	if help == "" {
		pad = 0
	}
	return "  " + title + strings.Repeat(" ", pad) + "\033[90m" + help + "\033[0m"
}

// hints renders key/label pairs as "enter resume  ^S msg", keys in bold so
// "enter update" reads as key then action.
func hints(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, "\033[1m"+pairs[i]+"\033[22m "+pairs[i+1])
	}
	return strings.Join(parts, "  ")
}

// helpPopup lists the shortcuts that apply now; Ctrl+G or Esc closes it.
func (m model) helpPopup() string {
	var b strings.Builder
	b.WriteString("\033[1mshortcuts\033[0m\n\n")
	for _, s := range m.shortcuts() {
		fmt.Fprintf(&b, "\033[33m%-8s\033[0m %s\n", s[0], s[1])
	}
	b.WriteString("\n\033[90m" + hints("^G/"+keyEsc, "close") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Render(b.String())
}

// overlayCentre draws box over the middle of screen, keeping what's visible
// either side of it. Widths are measured without ANSI codes.
func overlayCentre(screen, box string, width, height int) string {
	lines := strings.Split(screen, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	boxLines := strings.Split(box, "\n")
	boxW := 0
	for _, l := range boxLines {
		boxW = max(boxW, ansi.StringWidth(l))
	}
	top := max((height-len(boxLines))/2, 0)
	left := max((width-boxW)/2, 0)
	for i, bl := range boxLines {
		row := top + i
		if row >= len(lines) {
			break
		}
		base := lines[row]
		if w := ansi.StringWidth(base); w < left+boxW {
			base += strings.Repeat(" ", left+boxW-w)
		}
		pad := strings.Repeat(" ", boxW-ansi.StringWidth(bl))
		lines[row] = ansi.Truncate(base, left, "") + "\033[0m" + bl + pad + ansi.TruncateLeft(base, left+boxW, "")
	}
	return strings.Join(lines, "\n")
}

// viewScreen draws the current screen: the session list or the usage screen.
func (m model) viewScreen() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	var b strings.Builder

	// The list spans the full terminal width; TOPIC flexes to fill it.
	tableWidth := m.width

	// Title line with help right-aligned
	note := m.refreshNote()
	if workerPanicked.Load() {
		note += " · internal error, logged to " + workerPanicLog
	}
	if m.updateCheckFailed && m.updateTo == "" {
		note += " · update check failed"
	}
	status := ""
	if m.refreshStalled() {
		status = fmt.Sprintf(" · refresh stalled %dm", int(time.Since(m.refreshStarted).Minutes()))
	}
	if m.updating {
		status = fmt.Sprintf(" · updating to %s: %s...", m.updateTo, m.progress)
	}
	title := fmt.Sprintf("\033[1;36mccs\033[0m \033[90m· claude code search · %s%s\033[0m\033[33m%s\033[0m", version, note, status)
	pairs, keep := []string{keyEnter, "resume", "^S", "msg", keyTab, "usage", "^G", "help", "^C", "quit"}, 2
	if m.showUsage { // only Tab, account, help and quit do anything there
		pairs, keep = []string{keyTab, "back", "^O", "account", "^L", "changelog", "^C", "quit"}, 1 // every key it has, so no ^G help
	} else if m.chatFocus {
		pairs, keep = []string{keyEnter, "send", keyEsc, "search", "^J/K", "scroll", "^G", "help"}, 1
	}
	b.WriteString(headerLine(tableWidth, title, pairs, keep) + "\n")

	// Search line or delete confirmation
	var sections []string
	var inputSection string
	if m.renaming {
		sections = append(sections, "  "+m.renameInput.View()+"  \033[90m"+hints(keyEnter, "save", keyEsc, "cancel")+"\033[0m")
	} else if m.confirmPrune {
		conv := m.filtered[m.pruneIndex].conv
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")). // Amber
			Render(fmt.Sprintf("Prune \"%s\"? %s -> %s, saves %s (keeps dialogue).",
				truncate(getTopic(conv), 32), formatBytes(conv.Size), formatBytes(conv.Size-m.pruneSaved), formatBytes(m.pruneSaved))) +
			"  \033[90m" + hints("y", "prune", "n/"+keyEsc, "cancel") + "\033[0m"
		sections = append(sections, "  "+inputSection)
	} else if m.confirmDelete {
		conv := m.filtered[m.deleteIndex].conv
		topic := getTopic(conv)
		liveWarning := ""
		if m.live[conv.SessionID] {
			liveWarning = " It is open in claude."
		}
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // Red
			Render(fmt.Sprintf("Delete conversation \"%s\"?%s", truncate(topic, 50), liveWarning)) +
			"  \033[90m" + hints("y", "delete", "n/"+keyEsc, "cancel") + "\033[0m"
		sections = append(sections, "  "+inputSection)
	} else if m.showUsage {
		sections = append(sections, "  \033[1;36mUsage\033[0m \033[90m· last 12h · "+keyTab+" back\033[0m")
	} else {
		prefix, usage, count := m.searchRowParts()
		inputSection = prefix + usage + "\033[90m" + count + "\033[0m"
		sections = append(sections, inputSection)
	}

	// Show error message if set
	if m.errorMsg != "" {
		errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
		sections = append(sections, "  "+errorStyle.Render(m.errorMsg))
	}

	b.WriteString(strings.Join(sections, "\n"))
	b.WriteString("\n\n")

	if m.showUsage {
		b.WriteString(m.usageView(m.height - 2 - len(sections)))
		return b.String()
	}

	// Calculate heights
	listHeight := m.height * 30 / 100
	if listHeight < 3 {
		listHeight = 3
	}
	previewHeight := m.previewRenderHeight() + m.chatRows()

	// Column headers
	b.WriteString(fmt.Sprintf("  \033[90m%-*s  %-*s  %-*s  %-*s  %*s  %*s  %*s  %*s\033[0m\n",
		colWhen, "WHEN", colProject, "PROJECT", m.topicColWidth(), strings.Repeat(" ", colMarks)+"TOPIC", colModel, "MODEL", colSize, "SIZE", colCtx, "CTX", colMsgs, "MSGS", colHits, "HITS"))
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	visibleItems := listHeight
	start := 0
	if m.cursor >= visibleItems {
		start = m.cursor - visibleItems + 1
	}

	for i := start; i < min(start+visibleItems, len(m.filtered)); i++ {
		item := m.filtered[i]
		isSelected := i == m.cursor
		line := m.formatListItem(item, isSelected)

		if isSelected {
			// Pad to full width for selection highlight
			line = padRight("> "+line, m.width)
			b.WriteString(selectedStyle.Render(line))
		} else {
			b.WriteString("  " + line)
		}
		b.WriteString("\n")
	}

	// Fill remaining list space
	for i := len(m.filtered) - start; i < visibleItems; i++ {
		b.WriteString("\n")
	}

	// Preview section
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	if len(m.filtered) > 0 {
		rows := m.chatRows()
		preview := m.renderPreview(m.filtered[m.cursor], previewHeight-rows)
		if rows > 0 {
			// Pin the message box to the bottom whatever the preview's length.
			lines := strings.Split(preview, "\n")
			for len(lines) < previewHeight-rows {
				lines = append(lines, "")
			}
			preview = strings.Join(lines, "\n") + "\n" + m.chatView()
		}
		b.WriteString(preview)
	}

	return b.String()
}

// Fixed list column widths. TOPIC is the flex column - it absorbs the rest of
// the terminal width (see topicColWidth).
const (
	colWhen    = 8 // longest is "12mo ago"
	colProject = 22
	colModel   = 12
	colCtx     = 5
	colMsgs    = 5
	colHits    = 4
	colSize    = 6
	colGap     = 2 // spaces between columns
	listIndent = 2 // leading "  " / "> " on each row
	numGaps    = 7
	colMarks   = 4 // status icons (● ⚙ !) packed right, then a space
)

// topicColWidth flexes the TOPIC column to fill the terminal width.
func (m model) topicColWidth() int {
	used := listIndent + colWhen + colProject + colModel + colCtx + colMsgs + colHits + colSize + numGaps*colGap
	if w := m.width - used; w > 10 {
		return w
	}
	return 10
}

func (m model) formatListItem(item listItem, selected bool) string {
	project := item.conv.Cwd
	if idx := strings.LastIndex(project, "/"); idx >= 0 {
		project = project[idx+1:]
	}
	project = truncate(project, colProject)

	// Status icons, packed right against the topic so titles stay aligned:
	// ● open in a running claude, ⚙ started by a script or another session.
	// ponytail: the glyphs are ambiguous-width, so a marked row may sit one
	// cell off on CJK-width terminals - cosmetic only.
	var icons, colouredIcons string
	n := 0
	for _, s := range []struct {
		on           bool
		glyph, color string
	}{{m.live[item.conv.SessionID], "●", "32"}, {item.conv.Spawned, "⚙", "90"}, {item.conv.LastError != "", "!", "1;31"}} {
		if !s.on {
			continue
		}
		n++
		icons += s.glyph
		colouredIcons += "\033[" + s.color + "m" + s.glyph + "\033[0m"
	}
	pad := strings.Repeat(" ", colMarks-1-n)
	marks, colouredMarks := pad+icons+" ", pad+colouredIcons+" "
	tw := m.topicColWidth()
	// A trailing ✍ marks a name you set (not the ai-title Claude gives almost
	// every session); the title is cut short so it always stays visible.
	topic := truncate(getTopic(item.conv), tw-colMarks)
	if item.conv.IsCustomTitle {
		topic = truncate(getTopic(item.conv), tw-colMarks-2) + " ✍"
	}

	// Message count
	msgs := len(item.conv.Messages)

	// Number of messages containing the query (memoised per query).
	hits := m.hitCount(item)

	size := formatBytes(item.conv.Size)

	when := formatAgo(item.conv.LastTimestamp, time.Now())

	ctx := formatTokens(item.conv.ContextTokens)
	model := truncate(shortModel(item.conv), colModel)

	// Format: when | project | topic | model | size | ctx | msgs | hits (aligned columns)
	if selected {
		return fmt.Sprintf("%-*s  %-*s  %s%-*s  %-*s  %*s  %*s  %*d  %*d",
			colWhen, when, colProject, project, marks, tw-colMarks, topic, colModel, model, colSize, size, colCtx, ctx, colMsgs, msgs, colHits, hits)
	}
	// Pad before colouring so the escape codes don't eat into the column width.
	topic = colouredMarks + padRight(topic, tw-colMarks)
	// CTX is coloured by how full the context is (see ctxColour).
	ctxCode, flash := ctxColour(item.conv.ContextTokens, contextWindow(item.conv.ActiveModel, item.conv.PeakContext))
	if flash && time.Now().Unix()%2 == 1 {
		ctxCode = "2;31" // flashing: dim every other second
	}
	return fmt.Sprintf("\033[90m%-*s\033[0m  \033[1;33m%-*s\033[0m  %s  \033[37m%-*s\033[0m  \033[35m%*s\033[0m  \033["+ctxCode+"m%*s\033[0m  %*d  \033[36m%*d\033[0m",
		colWhen, when, colProject, project, topic, colModel, model, colSize, size, colCtx, ctx, colMsgs, msgs, colHits, hits)
}

// renameConversation sets a custom title the same way /rename does: by
// appending a custom-title line to the transcript.
func (m *model) renameConversation(name string) {
	m.renaming = false
	if name == "" || m.renameIndex >= len(m.filtered) {
		return
	}
	conv := m.filtered[m.renameIndex].conv
	line, _ := json.Marshal(map[string]string{"type": "custom-title", "customTitle": name, "sessionId": conv.SessionID})
	if err := appendLine(conv.FilePath, line); err != nil {
		m.errorMsg = fmt.Sprintf("Rename failed: %v", err)
		return
	}
	m.gen++
	// The file grew, so the next live tick or scan reparses it; drop the
	// cached copy rather than let anything build on it.
	parseCacheMu.Lock()
	delete(parseCache, conv.FilePath)
	parseCacheMu.Unlock()
	c := conv
	c.Title, c.IsCustomTitle = name, true
	c.searchText = "" // rebuilt by buildItems, exactly as a full parse would
	renamed := buildItems([]Conversation{c})[0]
	for _, items := range [][]listItem{m.items, m.filtered} {
		for i := range items {
			if items[i].conv.SessionID == conv.SessionID {
				items[i] = renamed
			}
		}
	}
	m.errorMsg = ""
}

// appendLine appends one JSONL line, first adding a newline if the file lacks
// a trailing one so the new line can't fuse with the last record.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// deleteConversation removes the selected conversation from disk and UI
func (m *model) deleteConversation() {
	if m.deleteIndex >= len(m.filtered) {
		return
	}

	conv := m.filtered[m.deleteIndex].conv

	// Delete the file (ignore if already deleted)
	if err := os.Remove(conv.FilePath); err != nil && !os.IsNotExist(err) {
		m.errorMsg = fmt.Sprintf("Delete failed: %v", err)
		m.confirmDelete = false
		return
	}
	m.gen++
	parseCacheMu.Lock()
	delete(parseCache, conv.FilePath)
	parseCacheMu.Unlock()

	// Remove from filtered slice
	m.filtered = append(m.filtered[:m.deleteIndex], m.filtered[m.deleteIndex+1:]...)

	// Remove from items slice (find by SessionID)
	for i, item := range m.items {
		if item.conv.SessionID == conv.SessionID {
			m.items = append(m.items[:i], m.items[i+1:]...)
			break
		}
	}

	// Adjust cursor
	if len(m.filtered) == 0 {
		m.cursor = 0
	} else if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	// Otherwise cursor stays at same position (shows next item)

	// Exit confirmation mode
	m.confirmDelete = false
	m.errorMsg = ""
}

// pruneConversation prunes the selected conversation file in place and refreshes
// its displayed size. The conversation stays in the list (only shrunk).
// ponytail: synchronous - a multi-GB file briefly blocks the UI, same as delete.
func (m *model) pruneConversation() {
	m.confirmPrune = false
	if m.pruneIndex >= len(m.filtered) {
		return
	}
	conv := m.filtered[m.pruneIndex].conv

	_, err := pruneFile(conv.FilePath, true, pruneOpts{dropSnapshots: true, stripToolResults: true})
	if err != nil {
		m.errorMsg = fmt.Sprintf("Prune failed: %v", err)
		return
	}
	m.gen++

	// Its parse state (read position) no longer matches the file. Rather than
	// re-read it here on the UI goroutine, mark it so the next read is a full
	// one (off the UI goroutine), and so the next scan's copy wins.
	parseCacheMu.Lock()
	delete(parseCache, conv.FilePath)
	parseCacheMu.Unlock()
	newSize := conv.Size
	if info, e := os.Stat(conv.FilePath); e == nil {
		newSize = info.Size()
	}
	for _, items := range [][]listItem{m.items, m.filtered} {
		for i := range items {
			if items[i].conv.SessionID == conv.SessionID {
				items[i].conv.Size = newSize
				items[i].conv.tailApplied = true
				items[i].conv.readAt = time.Time{}
			}
		}
	}
	m.errorMsg = ""
}

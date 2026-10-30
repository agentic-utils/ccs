package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	rank        int    // matchRank for the current search, 0 without one
	depth       int    // nested under the session that started it (sortList)
	familyCwd   string // its top-level session's cwd, which ^X G groups the family by
	familyState int    // liveGroup of its family's most urgent session: the state section it's listed in
	hidden      bool   // in a collapsed folder, under the one session it shows (^X C)
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
	table           bool              // ^X T: the full-width table instead of the sidebar
	parents         map[string]string // spawned SessionID -> the session whose claude -p started it
	noParent        map[string]int    // spawned SessionID -> len(items) when no parent was found
	grouped         bool              // ^X X G: sessions grouped by folder
	chordX          bool              // ^X pressed: the next key picks a ccs action
	selectedID      string            // the selected session, to see when it changes
	selectedAt      time.Time         // when it was selected: its preview waits previewDebounce
	marks           *sessionMarks     // when each session was last looked at (done)
	blurred         bool              // the terminal says ccs isn't focused: nothing is being seen
	snoozeOpen      bool              // the ^X S picker
	snoozeCursor    int
	autoFocus       bool                 // ^X A: switch to a session that starts waiting on you (autoFocusCmd)
	focusBusy       bool                 // an auto-focus attempt is in flight
	focusHeld       string               // the session auto-focus switched to; none other until it's answered
	focusHandled    map[string]time.Time // SessionID -> the wait (statusUpdatedAt) auto-focus has dealt with
	focusNoted      map[string]bool      // auto-focus steps logged, by session, step and wait
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
	pruneSaved      int64                // Bytes the pending prune would reclaim (measured on Ctrl+X X)
	renaming        bool                 // Are we typing a new name?
	resuming        bool                 // an Enter/Ctrl+F open is in flight
	opened          map[string]time.Time // sessions ccs opened in a tab, until their session file appears
	renameIndex     int                  // Index of item being renamed
	renameInput     textinput.Model
	errorMsg        string            // Show deletion/prune errors
	preview         *previewCache     // memoised preview lines for the selected conversation
	hits            *hitCounter       // memoised per-query hit counts, keyed by SessionID
	lastFilterQuery string            // lowercased query the current m.filtered was built from
	live            map[string]bool   // SessionIDs attached to a running claude process
	livePIDs        map[string]int    // live SessionID -> claude pid (for its session file and socket)
	hosts           map[string]string // live sessions run by an app with no terminal (hostOf): ccs can't switch to them

	// Message box for the selected live session (see chat*).
	chatFocus  bool // typing goes to the message box, not the search
	chatInput  textinput.Model
	chatFor    string                 // the session chatInput's draft belongs to
	chatDrafts map[string]string      // the other sessions' drafts, so a draft never follows the selection
	chatTickOn bool                   // the fast refresh for the selected live session is running
	chatStatus map[string]sessionStat // every live session's last known status (set only by setStat; applyStats drops sessions that are gone)
	pending    map[string]string      // text sent but not yet seen in the transcript, by SessionID
	sendNote   map[string]string      // outcome of the last send, by SessionID
	sentAt     map[string]time.Time   // when a socket send finished, by SessionID, until the session queues it
	held       map[string]bool        // a receipt said the session is holding the message for approval
	sending    bool

	// Input-prompt overlay (see wait*): live sessions waiting on the user.
	waitOpen       bool              // the overlay is open (Ctrl+X W); it owns the keyboard
	waitID         string            // the session it shows
	waitInput      textinput.Model   // the shown session's draft
	waitDrafts     map[string]string // the other sessions' drafts, so a draft never follows you to another session
	waitErr        string            // the overlay's own errors, kept apart from the main screen's
	waitScroll     int               // like previewScroll, for the overlay's transcript
	waitPreview    *previewCache     // the overlay's transcript, wrapped to its width
	waitDue        map[string]bool   // sessions with a wait (or a snooze that ran out) not yet seen: the banner shows them
	snoozed        map[string]snoozeRec
	snooze         time.Duration              // CCS_SNOOZE
	unverified     map[string]bool            // live sessions whose pid couldn't be checked against their file
	lastScan       map[string]liveSession     // the last live tick's sessions, so the next can keep one whose file is briefly unreadable
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

	// Account switcher (Ctrl+X U), backed by the cswap CLI.
	acctOpen bool
	helpOpen bool // ? shortcut list

	reopen        []Conversation // sessions open before a restart, offered for reopening
	reopenChecked bool           // the last boot's record has been read, so it may be overwritten
	// Each session's PRs (prs.go): what GitHub said, by address.
	prs        map[string]prInfo
	prNext     time.Time       // when the next round is due
	prQueue    [][]string      // the round's requests not sent yet
	prRound    int             // the round's requests in flight
	prSelBusy  bool            // the selected session's own request is in flight
	prSelAsked map[string]bool // asked for the selected session since the last round
	prErr      string
	fetchPRs   func([]string) (map[string]prInfo, error) // nil: never look PRs up
	// Ctrl+O: the links in the visible part of the preview, to open by key.
	linksOpen   bool
	links       []linkItem
	linksInput  textinput.Model // filters the links (linksShown)
	linksCursor int             // into linksShown
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
	if !m.table { // the sidebar and the preview beside it, under title, sections, blank
		listTop = sections + 2
		return listTop, max(m.height-listTop, 1), listTop
	}
	listHeight = max(m.height*30/100, 3)
	listTop = sections + 4 // title, sections, blank, column header, rule
	return listTop, listHeight, listTop + listHeight + 1
}

// sidebarWidth is the session list's width beside the preview, 0 in the table.
func (m model) sidebarWidth() int {
	if m.table {
		return 0
	}
	return min(max(m.width/4, 30), 48)
}

// paneWidth is the preview's width: beside the sidebar and its │, or the whole terminal.
func (m model) paneWidth() int {
	if sw := m.sidebarWidth(); sw > 0 {
		return max(m.width-sw-1, 20)
	}
	return m.width
}

// handleMouse sends the wheel to whatever is under the pointer: the
// conversation preview scrolls, the list moves its selection. A click on a
// list row selects it.
func (m model) handleMouse(msg tea.MouseMsg) model {
	if m.linksOpen || m.snoozeOpen || len(m.reopen) > 0 {
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
	if m.waitOpen && !m.popupAboveWait() { // the wheel scrolls its transcript, nothing else reacts
		if msg.Button == tea.MouseButtonWheelUp {
			m.waitScroll = min(m.waitScroll+3, m.maxWaitScroll())
		} else if msg.Button == tea.MouseButtonWheelDown {
			m.waitScroll = max(0, m.waitScroll-3)
		}
		return m
	}
	if m.waitOpen || m.popupAboveWait() || m.showUsage || m.prompting() || msg.Action != tea.MouseActionPress {
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
	paneX := 0 // where the preview's columns start
	if sw := m.sidebarWidth(); sw > 0 {
		paneX = sw + 1
	}
	onPreview := msg.Y >= previewTop && msg.X >= paneX
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		up := msg.Button == tea.MouseButtonWheelUp
		switch {
		case onPreview && up:
			m.previewScroll = min(m.previewScroll+3, m.maxPreviewScroll()) // back towards older messages
		case onPreview:
			m.previewScroll = max(0, m.previewScroll-3) // forward towards the newest
		case m.step(map[bool]int{true: -1, false: 1}[up]):
			m.previewScroll = 0
			m.selectionMoved()
		}
	case tea.MouseButtonLeft:
		if msg.Y == 1 { // the search row
			m.chatFocus = false
			return m
		}
		if m.chatRows() > 0 && !m.boxHidden() && msg.Y >= m.height-3 && msg.X >= paneX { // the message box
			m.chatFocus = true
			return m
		}
		if onPreview && len(m.filtered) > 0 {
			// ccs has the mouse, so the terminal never sees a click on a link: open it here.
			lines := strings.Split(m.renderPreview(m.filtered[m.cursor], m.previewRenderHeight()), "\n")
			if row := msg.Y - previewTop; row >= 0 && row < len(lines) {
				m.linkPending = linkAt(lines[row], msg.X-paneX)
			}
			return m
		}
		if msg.Y < listTop || msg.Y >= listTop+listHeight {
			return m
		}
		rows := m.listRows()
		r := m.listStart(rows, listHeight) + msg.Y - listTop
		if r >= len(rows) || rows[r].item < 0 { // a group's header
			return m
		}
		if i := rows[r].item; i != m.cursor {
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

	wait := chat
	links := textinput.New()
	links.Prompt = "> "
	links.Placeholder = "type to filter..."
	links.Cursor.SetMode(cursor.CursorStatic)
	links.Focus()
	snooze, ok := snoozeLength(os.Getenv("CCS_SNOOZE"))
	m := model{
		chatInput:   chat,
		waitInput:   wait,
		linksInput:  links,
		waitPreview: &previewCache{},
		snooze:      snooze,
		items:       items,
		textInput:   ti,
		claudeFlags: claudeFlags,
		preview:     &previewCache{},
		opened:      make(map[string]time.Time),
		kicked:      make(map[string]bool),
		hits:        &hitCounter{byID: make(map[string]int)},
		prs:         make(map[string]prInfo),
		prSelAsked:  make(map[string]bool),
	}
	if !ok {
		m.errorMsg = fmt.Sprintf("CCS_SNOOZE=%q isn't a duration like 15m; snoozing for %s", os.Getenv("CCS_SNOOZE"), shortDuration(snooze))
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
		for i := range next {
			next[i].rank = matchRank(next[i].searchLower, next[i].conv.Title, terms)
		}
		m.filtered = next
	}
	m.sortList()
	m.lastFilterQuery = queryLower
	// Keep cursor in bounds
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.previewScroll = 0
}

// liveGroup is where a session sits in the list: waiting on a menu or
// prompt (amber ?), then idle with a question in its last message (green ?),
// then idle, then busy, then not open in claude.
func (m model) liveGroup(c Conversation) int {
	switch {
	case m.putAway(c) != awake: // the bottom sections: settled, snoozed
		return 5 + m.putAway(c)
	case !m.live[c.SessionID]:
		return 5
	case m.waitSignal(c) == waitPrompt:
		return 0
	case m.waitSignal(c) == waitAsked: // idle, its last message asks something
		return 1
	case m.chatStatus[c.SessionID].status == "busy":
		return 4
	case m.unseen(c): // finished since you last looked
		return 2
	}
	return 3
}

// sortList orders the list by liveGroup, then best search match (named after
// the query, then the words as a phrase), then newest, keeping the cursor on
// the same session. Statuses change on ticks, so Update runs it every time.
func (m *model) sortList() {
	if len(m.filtered) == 0 {
		return
	}
	less := func(i, j int) bool {
		a, b := m.filtered[i], m.filtered[j]
		if ga, gb := m.liveGroup(a.conv), m.liveGroup(b.conv); ga != gb {
			return ga < gb
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.conv.LastTimestamp > b.conv.LastTimestamp
	}
	id := m.filtered[min(m.cursor, len(m.filtered)-1)].conv.SessionID
	// ponytail: sorts on every Update (the families and groups move with
	// statuses); a few hundred rows, cache it if it shows in profiles.
	sort.SliceStable(m.filtered, less)
	m.nestFamilies()
	// State sections (waiting, asked, idle, busy, not open), a family in its
	// most urgent session's; with ^X G, folders within each, a group where its
	// first session lands, so the one holding a question comes first.
	// Families stay together under their top session's folder.
	first := map[string]int{}
	if m.grouped {
		for i, it := range m.filtered {
			if k := groupOf(it); first[k] == 0 {
				first[k] = i + 1
			}
		}
	}
	sort.SliceStable(m.filtered, func(i, j int) bool {
		a, b := m.filtered[i], m.filtered[j]
		if a.familyState != b.familyState {
			return a.familyState < b.familyState
		}
		return first[groupOf(a)] < first[groupOf(b)]
	})
	m.hideCollapsed()
	if i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id }); i >= 0 {
		m.cursor = i
	}
	for m.cursor > 0 && m.filtered[m.cursor].hidden { // its folder was collapsed: to the one it shows
		m.cursor--
	}
}

// step moves the cursor by one shown session (dir -1 up, 1 down), past those
// a collapsed folder hides; false at either end.
func (m *model) step(dir int) bool {
	for i := m.cursor + dir; i >= 0 && i < len(m.filtered); i += dir {
		if !m.filtered[i].hidden {
			m.cursor = i
			return true
		}
	}
	return false
}

// nestFamilies puts each shown spawned session right under the shown session
// that started it (depth 1, its own children depth 2…). A family sits where
// its first member was in the sorted list, so a child waiting on a question
// still brings its family up.
func (m *model) nestFamilies() {
	n := len(m.filtered)
	at := make(map[string]int, n)
	for i, it := range m.filtered {
		at[it.conv.SessionID] = i
	}
	parent := make([]int, n)
	kids := map[int][]int{}
	for i, it := range m.filtered {
		parent[i] = -1
		if it.conv.Spawned {
			if p, ok := at[m.spawnParent(it.conv)]; ok && p != i {
				parent[i] = p
				kids[p] = append(kids[p], i)
			}
		}
	}
	out := make([]listItem, 0, n)
	seen := make([]bool, n)
	var walk func(i, depth int, cwd string)
	walk = func(i, depth int, cwd string) {
		if seen[i] {
			return
		}
		seen[i] = true
		it := m.filtered[i]
		it.depth, it.familyCwd = depth, cwd
		out = append(out, it)
		for _, k := range kids[i] {
			walk(k, depth+1, cwd)
		}
	}
	for i := range m.filtered {
		r := i
		for d := 0; parent[r] >= 0 && d < n; d++ { // up to its top session (d guards a cycle)
			r = parent[r]
		}
		walk(r, 0, m.filtered[r].conv.Cwd)
		walk(i, 0, m.filtered[i].conv.Cwd) // only reached by a cycle
	}
	for start := 0; start < len(out); { // families are contiguous, each from its depth-0 session
		end := start + 1
		for end < len(out) && out[end].depth > 0 {
			end++
		}
		state := m.liveGroup(out[start].conv)
		for _, it := range out[start+1 : end] {
			state = min(state, m.liveGroup(it.conv))
		}
		if pin := m.familyPin(out[start:end]); pin >= 0 { // above every state, in pin order
			state = pin - len(m.marks.Pinned)
		}
		for i := start; i < end; i++ {
			out[i].familyState = state
		}
		start = end
	}
	m.filtered = out
}

// familyPin is the first pin place of a family's sessions, -1 if none is pinned.
func (m model) familyPin(family []listItem) int {
	pin := -1
	for _, it := range family {
		if i := m.pinIndex(it.conv.SessionID); i >= 0 && (pin < 0 || i < pin) {
			pin = i
		}
	}
	return pin
}

// groupOf is the ^X G group a session is listed under: its family's folder,
// within its state section. Pinned families keep their own order, ungrouped.
func groupOf(it listItem) string {
	if it.familyState < 0 {
		return fmt.Sprint(it.familyState)
	}
	return fmt.Sprint(it.familyState, " ", groupKey(it.familyCwd))
}

// sectionOf is the state section a familyState is listed in: pinned ones
// (negative, in pin order) share one.
func sectionOf(state int) int { return max(state, -1) }

// doneColour marks a session that finished since you last looked.
const doneColour = "1;36"

// pinnedSection heads the pinned sessions, above every state.
var pinnedSection = struct{ name, colour string }{"pinned", "1;37"}

// stateSections name liveGroup's values, the list's sections, and colour them.
var stateSections = [...]struct{ name, colour string }{
	{"waiting on you", "1;" + amber}, {"asked you something", "32"}, {"done", doneColour}, {"idle", "32"}, {"busy", amber}, {"not open in claude", "90"}, {"settled", "90"}, {"snoozed", "90"},
}

// spawnParent is the session whose claude -p started c, "" if none is known.
// Found once and kept; a miss is retried when the list grows.
func (m *model) spawnParent(c Conversation) string {
	if p := m.parents[c.SessionID]; p != "" {
		return p
	}
	if n, ok := m.noParent[c.SessionID]; ok && n == len(m.items) {
		return ""
	}
	if m.parents == nil {
		m.parents, m.noParent = map[string]string{}, map[string]int{}
	}
	p := findSpawnParent(c, m.items)
	if p == "" {
		m.noParent[c.SessionID] = len(m.items)
	} else {
		m.parents[c.SessionID] = p
	}
	return p
}

// findSpawnParent finds the session that ran a claude -p holding c's first
// prompt within ten minutes before c began (two for a short prompt); Claude
// Code records no parent.
// ponytail: matched on the prompt's start, so two sessions sending the same
// prompt at the same moment can't be told apart.
func findSpawnParent(c Conversation, items []listItem) string {
	first := ""
	for _, msg := range c.Messages {
		if msg.Role == "user" {
			first = spawnText(msg.Text)
			break
		}
	}
	if first == "" {
		return ""
	}
	window := 10 * time.Minute
	if len(first) < 20 { // a short prompt ("reply ok") is common: only trust it close in time
		window = 2 * time.Minute
	}
	first = first[:min(len(first), 60)]
	start, err := time.Parse(time.RFC3339, c.FirstTimestamp)
	if err != nil {
		return ""
	}
	best, bestGap := "", window
	for _, it := range items {
		if it.conv.SessionID == c.SessionID {
			continue
		}
		for _, s := range it.conv.spawns {
			t, err := time.Parse(time.RFC3339, s.ts)
			if gap := start.Sub(t); err == nil && gap > -time.Minute && gap < bestGap && strings.Contains(s.text, first) {
				best, bestGap = it.conv.SessionID, max(gap, -gap)
			}
		}
	}
	return best
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.reload != nil {
		cmds = append(cmds, refreshTick(), liveTick(), waitReceipt, reopenCheckCmd)
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
// it holds the selected session's draft, focus only stays in it while the
// selected session is live, and the fast refresh runs only while a live
// session is selected.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	res, cmd := m.update(msg)
	n := res.(model)
	n.sortList()
	if settle := n.noteSelection(); settle != nil {
		cmd = tea.Batch(cmd, settle)
	}
	if n.noteSeen() {
		n.sortList() // out of done
	}
	n.syncChatDraft()
	if n.chatFocus && (!n.selectedLive() || n.boxHidden()) {
		n.chatFocus = false
	}
	if pr := n.prCmd(); pr != nil {
		cmd = tea.Batch(cmd, pr)
	}
	if tick := n.ensureChatTick(); tick != nil {
		return n, tea.Batch(cmd, tick)
	}
	return n, cmd
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// The last column stays empty: a row reaching it wraps if the terminal
		// draws any character in it one cell wider than we measure (emoji
		// widths vary by terminal), which scrolls the screen and leaves stale rows.
		m.width = max(msg.Width-1, 1)
		m.height = msg.Height
		// Calculate visible list height
		m.listHeight = m.height * 30 / 100
		if m.listHeight < 3 {
			m.listHeight = 3
		}
		// The inputs scroll their text during Update, so their width must be set here, not only on View's copy.
		inner, _ := m.waitLayout()
		m.chatInput.Width, m.waitInput.Width = chatInputWidth(m.paneWidth()), chatInputWidth(inner)
		if m.waitOpen && m.height < waitMinHeight {
			m.closeWait()
			m.errorMsg = waitTooShort
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

	case reopenMsg:
		m.reopenChecked = true
		for _, id := range msg.ids {
			for _, item := range m.items {
				if item.conv.SessionID == id {
					m.reopen = append(m.reopen, item.conv)
					break
				}
			}
		}
		return m, nil

	case reopenDoneMsg:
		m.resuming = false
		live := maps.Clone(m.live)
		if live == nil {
			live = make(map[string]bool)
		}
		for _, id := range msg.opened {
			live[id] = true
			m.opened[id] = time.Now()
		}
		m.live = live
		switch {
		case msg.err != nil:
			m.errorMsg = fmt.Sprintf("Reopened %d, then opening a tab timed out: it may still have opened (%v)", len(msg.opened), msg.err)
		case msg.noTabs:
			m.errorMsg = fmt.Sprintf("Reopened %d: opening the rest needs iTerm or tmux", len(msg.opened))
		default:
			m.errorMsg = fmt.Sprintf("Reopened %d sessions", len(msg.opened))
		}
		return m, nil

	case answerDoneMsg:
		switch {
		case msg.err != nil:
			m.waitErr = "Answering " + msg.name + " failed (" + msg.err.Error() + "): answer it in its tab"
		case msg.where == "":
			m.waitErr = "Couldn't find " + msg.name + "'s terminal: answer it in its tab"
		}
		return m, nil

	case focusDoneMsg:
		m.resuming = false
		if msg.gone {
			m.errorMsg = "Couldn't find " + msg.name + "'s terminal right now: it may have exited"
		} else if !msg.found {
			m.errorMsg = "Session is open in claude but its terminal wasn't found - ^F forks it"
		}
		return m, nil

	case liveTickMsg:
		return m, m.liveCmd()

	case liveMsg:
		m.live = msg.live
		if msg.pids != nil {
			m.livePIDs, m.unverified, m.lastScan, m.hosts = msg.pids, msg.unverified, msg.scan, msg.hosts
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
		m.applyStats(msg.stats)
		cmds := []tea.Cmd{liveTick(), m.autoFocusCmd()}
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

	case prDoneMsg:
		m.applyPRs(msg)
		return m, nil

	case tea.FocusMsg: // terminals that report focus; without it ccs counts as focused
		m.blurred = false
		return m, nil

	case tea.BlurMsg:
		m.blurred = true
		return m, nil

	case autoFocusMsg:
		m.applyAutoFocus(msg)
		return m, nil

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
		m.chatTickOn = false // rescheduled by Update while its session stays live
		m.setStat(msg.id, msg.stat)
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
			if errors.Is(msg.err, errUnsent) {
				m.sendNote[msg.id] += " · " + m.giveBack(msg.id, msg.text, msg.overlay)
			}
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
		// ccs's own actions are ^X then a letter (^X ^W works as ^X W), Claude
		// Code's pattern, so every other Ctrl letter edits text as in its
		// prompt. k is the key, or "^x <letter>" for a chord.
		k := msg.String()
		chord := m.chordX
		if chord {
			m.chordX = false
			k = "^x " + strings.TrimPrefix(k, "ctrl+")
		} else if k == "ctrl+x" && !m.prompting() && !slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open }) {
			m.chordX = true
			return m, nil
		}
		// The overlay owns the keyboard until closed. (A pop-up is only the
		// banner, which takes no keys.)
		if m.waitOpen && !m.popupAboveWait() {
			if chord {
				if k == "^x w" {
					m.closeWait()
				}
				return m, nil
			}
			return m.waitKey(msg)
		}
		if m.snoozeOpen { // owns the keyboard until closed
			return m.snoozeKey(k)
		}
		if k == "^x s" && len(m.filtered) > 0 && m.marks != nil && !m.prompting() && !m.acctOpen && !m.showUsage && !m.waitOpen {
			m.helpOpen = false
			if c := m.filtered[m.cursor].conv; m.putAway(c) != awake {
				m.wake(c.SessionID)
			} else {
				m.snoozeOpen, m.snoozeCursor = true, 0
			}
			return m, nil
		}
		if len(m.reopen) > 0 { // owns the keyboard until answered
			switch msg.String() {
			case "enter":
				if m.resuming {
					m.errorMsg = "Still opening the last one..."
					return m, nil
				}
				convs := m.reopen
				m.reopen = nil
				return m, m.reopenCmd(convs)
			case "esc":
				m.reopen = nil
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			}
			return m, nil
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
			case "ctrl+o", "esc":
				m.linksOpen = false
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "up", "ctrl+p":
				m.linksCursor = max(m.linksCursor-1, 0)
			case "down", "ctrl+n":
				m.linksCursor = min(m.linksCursor+1, len(m.linksShown())-1)
			case "enter":
				if shown := m.linksShown(); len(shown) > 0 { // stays open for the next
					return m, openLinkCmd(shown[min(m.linksCursor, len(shown)-1)].url)
				}
			default:
				before := m.linksInput.Value()
				var cmd tea.Cmd
				m.linksInput, cmd = editKey(m.linksInput, msg)
				if m.linksInput.Value() != before {
					m.linksCursor = 0
				}
				return m, cmd
			}
			return m, nil
		}
		if k == "^x t" && !m.prompting() && !m.acctOpen && !m.showUsage && !m.waitOpen {
			m.helpOpen, m.table = false, !m.table
			saveSettings(m.settings())
			m.chatInput.Width = chatInputWidth(m.paneWidth())
			return m, nil
		}
		if k == "^x a" && !m.prompting() && !m.acctOpen && !m.showUsage && !m.waitOpen {
			m.helpOpen, m.autoFocus = false, !m.autoFocus
			m.focusHeld = ""
			saveSettings(m.settings())
			return m, nil
		}
		if k == "^x c" && m.grouped && !m.prompting() && !m.acctOpen && !m.showUsage && !m.waitOpen {
			m.helpOpen = false
			m.toggleCollapse()
			return m, nil
		}
		if k == "^x p" && !m.prompting() && !m.acctOpen && !m.showUsage && !m.waitOpen {
			m.helpOpen = false
			m.togglePin()
			return m, nil
		}
		if (k == "alt+up" || k == "alt+down") && !m.prompting() && !m.acctOpen && !m.showUsage && !m.waitOpen && !m.chatFocus {
			m.movePin(map[string]int{"alt+up": -1, "alt+down": 1}[k])
			return m, nil
		}
		if k == "^x g" && !m.prompting() && !m.acctOpen && !m.showUsage && !m.waitOpen {
			m.helpOpen, m.grouped = false, !m.grouped
			saveSettings(m.settings())
			m.sortList()
			return m, nil
		}
		if msg.String() == "ctrl+o" && !m.prompting() && !m.acctOpen && !m.showUsage && !m.waitOpen {
			m.helpOpen = false
			if m.links = m.linksWithPRs(); len(m.links) == 0 {
				m.errorMsg = "No links in view"
				return m, nil
			}
			m.linksOpen, m.linksCursor = true, 0
			m.linksInput.SetValue("")
			return m, nil
		}
		if msg.String() == "ctrl+l" && !m.prompting() && !m.acctOpen && !m.waitOpen {
			m.helpOpen, m.notesOpen, m.notesScroll = false, true, 0
			if m.notes == nil && !m.notesFetching && m.fetchNotes != nil {
				m.notesErr = ""
				return m, m.notesCmd()
			}
			return m, nil
		}
		if m.helpOpen { // owns the keyboard until closed
			switch msg.String() {
			case "?", "esc":
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
		// ? opens help when it can't be meant as text: an empty search, the
		// message box not focused.
		if msg.String() == "?" && m.textInput.Value() == "" && !m.chatFocus && !m.prompting() && !m.showUsage && !m.waitOpen {
			m.helpOpen = true
			return m, nil
		}
		if k == "^x u" && !m.prompting() && !m.waitOpen {
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
			m.renameInput, cmd = editKey(m.renameInput, msg)
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
		if m.showUsage && k != "ctrl+c" && k != "^x w" {
			return m, nil // the usage screen has no other keys; typing mustn't edit a hidden search
		}

		if m.chatFocus && !chord {
			switch msg.String() {
			case "esc":
				m.chatFocus = false // back to the search
				return m, nil
			case "enter":
				if strings.TrimSpace(m.chatInput.Value()) == "" && len(m.filtered) > 0 {
					// Enter on an empty box opens the session itself.
					m.chatFocus = false
					return m, m.focusCmd(m.filtered[m.cursor].conv)
				}
				return m, m.sendCmd()
			case "up", "ctrl+p", "down", "ctrl+n", "pgup", "pgdown", "ctrl+c", "ctrl+f", "ctrl+d", "ctrl+r", "ctrl+]", "ctrl+\\":
				// fall through to the list's handling below
			default:
				var cmd tea.Cmd
				m.chatInput, cmd = editKey(m.chatInput, msg)
				return m, cmd
			}
		}

		switch k {
		case "ctrl+s":
			// Keyboard way into the message box of the selected live session
			// (clicking it is the mouse way).
			if m.boxHidden() && !m.showUsage {
				m.errorMsg = "It's waiting on you: answer its question first (" + keyEnter + " opens its tab)"
			} else if m.selectedLive() && !m.showUsage {
				m.chatFocus = true
			} else if len(m.filtered) > 0 {
				m.errorMsg = "Only a session open in claude (●) can receive messages"
			}
			return m, nil

		case "esc":
			// Esc clears the search and goes back to the top of the list;
			// Ctrl+C quits.
			if m.textInput.Value() != "" {
				m.textInput.SetValue("")
				m.updateFilter()
			}
			if m.cursor != 0 {
				m.cursor, m.previewScroll = 0, 0
				m.selectionMoved()
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
			// A live session's tab is focused, not resumed again (resumeCmd checks).
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
			return m, m.refreshNow()

		case "^x e":
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

		case "^x x":
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

		case "up", "ctrl+p", "down", "ctrl+n":
			if m.step(map[bool]int{true: -1, false: 1}[k == "up" || k == "ctrl+p"]) {
				m.previewScroll = 0
				m.selectionMoved()
			}
			return m, nil

		case "ctrl+]", "ctrl+\\": // next / previous search hit
			m.jumpHit(msg.String() == "ctrl+]")
			return m, nil

		case "pgup":
			m.previewScroll = min(m.previewScroll+10, m.maxPreviewScroll()) // back
			return m, nil

		case "pgdown":
			m.previewScroll = max(0, m.previewScroll-10) // forward
			return m, nil

		case "^x w":
			m.openWait()
			return m, nil
		}
		if chord { // a ^X chord no action took never edits a text box
			return m, nil
		}
	}

	// Anything else (e.g. a Ctrl+V paste's result) is for the input that has focus.
	var cmd tea.Cmd
	if _, isKey := msg.(tea.KeyMsg); !isKey {
		switch {
		case m.waitOpen:
			m.waitInput, cmd = editKey(m.waitInput, msg)
			return m, cmd
		case m.renaming:
			m.renameInput, cmd = editKey(m.renameInput, msg)
			return m, cmd
		case m.chatFocus:
			m.chatInput, cmd = editKey(m.chatInput, msg)
			return m, cmd
		}
	}
	prevValue := m.textInput.Value()
	m.textInput, cmd = editKey(m.textInput, msg)
	if m.textInput.Value() != prevValue {
		m.updateFilter()
	}
	return m, cmd
}

// View draws the current screen, then any popup on top. Popups are overlaid
// here, not by each screen, so every screen (and any added later) shows them.
func (m model) View() string {
	screen := m.viewScreen()
	if m.waitOpen {
		screen = overlayCentre(screen, m.waitPopup(), m.width, m.height)
	}
	for _, p := range m.overPopups() {
		if p.open {
			screen = overlayCentre(screen, p.draw(m), m.width, m.height)
		}
	}
	return clampFrame(screen, m.width, m.height)
}

// clampFrame keeps the frame inside the terminal, whatever drew it: a row
// that wraps or one too many scrolls the screen, which takes the title row
// off the top and leaves stale rows behind.
func clampFrame(s string, w, h int) string {
	if w <= 0 || h <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	lines = lines[:min(len(lines), h)]
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			lines[i] = ansi.Truncate(l, w, "") + "\033[0m"
		}
	}
	return strings.Join(lines, "\n")
}

type popup struct {
	open bool
	draw func(model) string // a method expression, so listing popups copies no model
}

// overPopups is every popup that can sit over the overlay, bottom first:
// View draws them in this order, and popupAboveWait checks them.
func (m model) overPopups() []popup {
	return []popup{
		{m.updateOpen && !m.prompting(), model.updatePopup},
		{m.acctOpen, model.acctPopup},
		{m.helpOpen, model.helpPopup},
		{m.notesOpen, model.notesPopup},
		{m.linksOpen, model.linksPopup},
		{m.snoozeOpen, model.snoozePopup},
		{len(m.reopen) > 0, model.reopenPopup},
	}
}

// Compact key labels, shared by every hint so they stay consistent.
const (
	keyEnter = "enter" // words, not ⏎ ⇥: those draw wider than a cell in some fonts
	keyTab   = "tab"
	keyEsc   = "esc"
)

// shortcuts is the keys that do something right now on the list, for the
// ? popup (the usage screen's header and the overlay's hint row list
// their own): whether a conversation is selected and live, and whether the
// message box has focus.
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
		add(true, keyEnter, "open its tab")
		add(!m.boxHidden(), "^S", "message it")
	default:
		add(sel, keyEnter, "resume")
	}
	add(sel, "^F", "fork")
	add(sel && !live, "^X E", "edit its name")
	add(true, "^R", "refresh now, and check for a ccs update")
	add(sel, "^D", "delete")
	add(sel, "^X X", "prune")
	add(sel, "pgup/dn", "scroll the conversation")
	add(sel && m.textInput.Value() != "" && len(m.hitLines()) > 0, "^] ^\\", "next / previous search hit")
	add(len(m.filtered) > 1, "↑↓ ^P/N", "move through the list")
	add(true, keyTab, "usage")
	add(!m.chatFocus && m.textInput.Value() != "", keyEsc, "clear the search")
	add(len(m.waitQueue(true)) > 0, "^X W", "sessions waiting on you")
	add(hasCswap, "^X U", "switch account")
	add(sel && len(m.linksWithPRs()) > 0, "^O", "open a link in view or one of its PRs")
	add(!m.table, "^X T", "the full table, every column")
	add(m.table, "^X T", "back to the sidebar")
	add(!m.autoFocus, "^X A", "switch to a session when it starts waiting on you")
	add(m.autoFocus, "^X A", "stop switching to waiting sessions")
	pinned := sel && m.pinIndex(m.filtered[m.cursor].conv.SessionID) >= 0
	add(sel && m.marks != nil && !pinned, "^X P", "pin it to the top")
	add(pinned, "^X P", "unpin it")
	add(pinned && len(m.marks.Pinned) > 1, "Alt+↑↓", "move it among the pinned")
	if sel && m.grouped && m.marks != nil {
		if m.collapsed(m.filtered[m.cursor].familyCwd) {
			add(true, "^X C", "expand its folder")
		} else {
			add(true, "^X C", "collapse its folder to one session")
		}
	}
	if sel && m.marks != nil {
		if m.putAway(m.filtered[m.cursor].conv) != awake {
			add(true, "^X S", "bring it back")
		} else {
			add(true, "^X S", "snooze or settle it")
		}
	}
	add(!m.grouped, "^X G", "group by folder")
	add(m.grouped, "^X G", "ungroup")
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

// helpPopup lists the shortcuts that apply now; ? or Esc closes it.
func (m model) helpPopup() string {
	var b strings.Builder
	b.WriteString("\033[1mshortcuts\033[0m\n\n")
	for _, s := range m.shortcuts() {
		fmt.Fprintf(&b, "\033[33m%-8s\033[0m %s\n", s[0], s[1])
	}
	b.WriteString("\n\033[90m" + hints("?/"+keyEsc, "close") + "\033[0m")
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
	// Each state with its key in front, in bold, as the hints read.
	key := func(k string) string { return "\033[1m" + k + "\033[22m " }
	if n := m.waitingCount(); n > 0 && !m.showUsage {
		status += " · "
		if !m.popupAboveWait() && !m.prompting() { // otherwise Ctrl+X W does nothing
			status += key("^X W")
		}
		status += fmt.Sprintf("%d waiting", n)
	}
	// Auto-focus on or off, so you know which you're in; after the waiting
	// count, which matters more when a narrow screen cuts the title.
	if !m.showUsage {
		state := "off"
		if m.autoFocus {
			state = "on"
		}
		status += " · " + key("^X A") + "auto-focus " + state
	}
	title := fmt.Sprintf("\033[1;36mccs\033[0m \033[90m· claude code search · %s%s\033[0m\033[33m%s\033[0m", version, note, status)
	group := "group" // the modes toggled while working; ^X T and the rest are in ? help
	if m.grouped {
		group = "ungroup"
	}
	pairs, keep := []string{"^X G", group, "?", "help", "^C", "quit"}, 2 // ^X A is beside auto-focus in the title
	if m.showUsage {                                                     // only Tab, account, help and quit do anything there
		pairs, keep = []string{keyTab, "back", "^X U", "account", "^L", "changelog"}, 1 // every key it has, so no ? help
		if len(m.waitQueue(true)) > 0 {
			pairs = append(pairs, "^X W", "waiting")
		}
		pairs = append(pairs, "^C", "quit")
	} else if m.chordX { // ^X pressed: what the next key does
		pairs, keep = []string{"A", "auto-focus", "E", "edit name", "G", "group", "T", "table", "U", "account", "W", "waiting", "X", "prune", keyEsc, "cancel"}, 1
	} else if m.chatFocus {
		pairs, keep = []string{keyEnter, "send", keyEsc, "search", "pgup/dn", "scroll"}, 1 // ? types into the box here
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
		msg := strings.Join(strings.Fields(m.errorMsg), " ") // one row: listLayout counts it as one
		sections = append(sections, "  "+errorStyle.Render(ansi.Truncate(msg, max(m.width-3, 1), "…")))
	}

	b.WriteString(strings.Join(sections, "\n") + "\n")
	if sw := m.sidebarWidth(); sw > 0 && !m.showUsage { // a rule over the list and preview, joining the divider
		b.WriteString("\033[90m" + strings.Repeat("─", sw) + "┬" + strings.Repeat("─", max(m.width-sw-1, 0)) + "\033[0m")
	}
	b.WriteString("\n")

	if m.showUsage {
		b.WriteString(m.usageView(m.height - 2 - len(sections)))
		return b.String()
	}
	if !m.table {
		b.WriteString(m.sidebarView())
		return b.String()
	}

	// Calculate heights
	listHeight := m.height * 30 / 100
	if listHeight < 3 {
		listHeight = 3
	}
	previewHeight := m.previewRenderHeight() + m.chatRows()

	// Column headers
	hitsHead := ""
	if m.searching() {
		hitsHead = fmt.Sprintf("  %*s", colHits, "HITS")
	}
	b.WriteString(fmt.Sprintf("  \033[90m%-*s  %-*s  %-*s  %-*s  %*s  %*s  %*s%s\033[0m\n",
		colWhen, "WHEN", colProject, "PROJECT", m.topicColWidth(), strings.Repeat(" ", colMarks)+"TOPIC", colModel, "MODEL", colSize, "SIZE", colCtx, "CTX", colMsgs, "MSGS", hitsHead+fmt.Sprintf("  %*s", colPRs, "PRS")))
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	rows := m.listRows()
	start := m.listStart(rows, listHeight)
	top, size, bar := scrollThumb(len(rows), start, listHeight)
	for r := range listHeight {
		line := ""
		if i := start + r; i < len(rows) && rows[i].item < 0 {
			line = rows[i].header
		} else if i < len(rows) {
			isSelected := rows[i].item == m.cursor
			line = "  " + m.formatListItem(m.filtered[rows[i].item], isSelected)
			if isSelected {
				// Pad to full width for selection highlight
				line = selectedStyle.Render(padRight(line, m.width-1)) // the highlight marks it, no >
			}
		}
		if bar {
			line = withBar(line, m.width, barCell(r, top, size))
		}
		b.WriteString(line + "\n")
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
	colPRs     = 11 // "✓1 ✗1 2 ●3"
	colHits    = 4
	colSize    = 6
	colGap     = 2 // spaces between columns
	listIndent = 2 // leading "  " on each row
	numGaps    = 7
	colMarks   = 3 // status icons (● !) packed right, then a space
)

// searching: a search is typed, so HITS has something to count.
func (m model) searching() bool { return strings.TrimSpace(m.textInput.Value()) != "" }

// topicColWidth flexes the TOPIC column to fill the terminal width.
func (m model) topicColWidth() int {
	used := listIndent + colWhen + colProject + colModel + colCtx + colMsgs + colPRs + colSize + numGaps*colGap + 1 // +1: the scrollbar
	if m.searching() {                                                                                              // HITS only means something with a search typed
		used += colHits + colGap
	}
	if w := m.width - used; w > 10 {
		return w
	}
	return 10
}

func (m model) formatListItem(item listItem, selected bool) string {
	project := truncate(projectName(item.conv.Cwd), colProject)

	marks, colouredMarks := m.listMarks(item)
	mw := colMarks // the marks' width, and with ^X G an indent under the folder's header
	if m.grouped {
		marks, colouredMarks, mw = "  "+marks, "  "+colouredMarks, colMarks+2
	}
	tw := m.topicColWidth()
	// A trailing ✎ marks a name you set (not the ai-title Claude gives almost
	// every session); the title is cut short so it always stays visible. Not
	// ✍: some terminals draw that emoji two cells wide, so the row wraps.
	nest := nestPrefix(item.depth)
	topic := nest + listTopic(item.conv, tw-mw-ansi.StringWidth(nest))
	if tasks := m.taskCell(item.conv, false); tasks != "" { // running background tasks, after the title
		topic = nest + listTopic(item.conv, tw-mw-ansi.StringWidth(nest)-ansi.StringWidth(tasks)-1) + " " + tasks
	}

	// Message count
	msgs := len(item.conv.Messages)

	// Number of messages containing the query (memoised per query).
	hits := ""
	if m.searching() {
		hits = fmt.Sprintf("  %*d", colHits, m.hitCount(item))
	}

	size := formatBytes(item.conv.Size)

	when := formatAgo(item.conv.LastTimestamp, time.Now())

	ctx := formatTokens(item.conv.ContextTokens)
	model := truncate(shortModel(item.conv), colModel)

	// Format: when | project | topic | model | size | ctx | msgs | hits | prs (aligned columns)
	if selected {
		return fmt.Sprintf("%-*s  %-*s  %s%-*s  %-*s  %*s  %*s  %*d%s",
			colWhen, when, colProject, project, marks, tw-mw, topic, colModel, model, colSize, size, colCtx, ctx, colMsgs, msgs, hits+"  "+m.prCell(item.conv, false))
	}
	// Pad before colouring so the escape codes don't eat into the column width.
	topic = colouredMarks + padRight(topic, tw-mw)
	// CTX is coloured by how full the context is (see ctxColour).
	ctxCode, flash := ctxColour(item.conv.ContextTokens, contextWindow(item.conv.ActiveModel, item.conv.PeakContext))
	if flash && time.Now().Unix()%2 == 1 {
		ctxCode = "2;31" // flashing: dim every other second
	}
	return fmt.Sprintf("\033[90m%-*s\033[0m  \033[1;33m%-*s\033[0m  %s  \033[37m%-*s\033[0m  \033[35m%*s\033[0m  \033["+ctxCode+"m%*s\033[0m  %*d\033[36m%s\033[0m  %s",
		colWhen, when, colProject, project, topic, colModel, model, colSize, size, colCtx, ctx, colMsgs, msgs, hits, m.prCell(item.conv, true))
}

// listTopic is the session's title cut to w, a trailing ✎ when you named it.
// nestPrefix marks a session started by the one above it: "└ ", indented
// two more per level below the first.
func nestPrefix(depth int) string {
	if depth == 0 {
		return ""
	}
	return strings.Repeat("  ", depth-1) + "└ "
}

func listTopic(conv Conversation, w int) string {
	if conv.IsCustomTitle {
		return truncate(getTopic(conv), w-2) + " ✎"
	}
	return truncate(getTopic(conv), w)
}

// listMarks is a row's colMarks-wide status column, plain and coloured.
func (m model) listMarks(item listItem) (string, string) {
	conv := item.conv
	// Status icons, packed right against the topic so titles stay aligned:
	// ● open in a running claude (? instead when it waits on you: bright for
	// a prompt, green when its last message asks something), ! its last
	// action errored. A session another one started sits └ under it instead
	// of a mark: entrypoint sdk-cli alone can't tell a script's claude -p from
	// a session typed in through an app.
	// ponytail: the glyphs are ambiguous-width, so a marked row may sit one
	// cell off on CJK-width terminals - cosmetic only.
	var icons, colouredIcons string
	n := 0
	// Green when idle, amber while busy, an amber ? when a menu or prompt
	// waits on you; a green ? when idle and its last message asks something.
	liveGlyph, liveColour := "●", "32"
	if m.hosts[conv.SessionID] != "" { // open in an app ccs can't switch to
		liveGlyph = "○"
	}
	switch m.waitSignal(conv) {
	case waitPrompt:
		liveGlyph, liveColour = "?", "1;"+amber
	case waitAsked:
		liveGlyph = "?"
	default:
		if m.chatStatus[conv.SessionID].status == "busy" {
			liveColour = amber
		} else if m.unseen(conv) {
			liveColour = doneColour
		}
	}
	for _, s := range []struct {
		on           bool
		glyph, color string
	}{{m.live[conv.SessionID], liveGlyph, liveColour}, {conv.LastError != "", "!", "1;31"}} {
		if !s.on {
			continue
		}
		n++
		icons += s.glyph
		colouredIcons += "\033[" + s.color + "m" + s.glyph + "\033[0m"
	}
	pad := strings.Repeat(" ", colMarks-1-n)
	return pad + icons + " ", pad + colouredIcons + " "
}

// sidebarView is the session list as a sidebar with the preview (and message
// box) beside it, filling the rows under the search. listLayout must match.
func (m model) sidebarView() string {
	_, h, _ := m.listLayout()
	sw, pw := m.sidebarWidth(), m.paneWidth()
	rows := m.listRows()
	start := m.listStart(rows, h)
	var pane []string
	if len(m.filtered) > 0 {
		rows := m.chatRows()
		pane = strings.Split(m.renderPreview(m.filtered[m.cursor], h-rows), "\n")
		if rows > 0 { // pin the message box to the bottom whatever the preview's length
			for len(pane) < h-rows {
				pane = append(pane, "")
			}
			pane = append(pane[:h-rows], strings.Split(m.chatView(), "\n")...)
		}
	}
	top, size, bar := scrollThumb(len(rows), start, h)
	lines := make([]string, h)
	for r := range lines {
		left := strings.Repeat(" ", sw)
		if i := start + r; i < len(rows) && rows[i].item < 0 {
			left = padRight(rows[i].header, sw)
		} else if i < len(rows) {
			left = m.sidebarRow(m.filtered[rows[i].item], rows[i].item == m.cursor, sw)[rows[i].part]
		}
		lines[r] = left + "\033[90m│\033[0m" // the divider doubles as the list's scrollbar
		if bar {
			lines[r] = left + barCell(r, top, size)
		}
		if r < len(pane) {
			lines[r] += ansi.Truncate(pane[r], pw, "…")
		}
	}
	return strings.Join(lines, "\n")
}

// rowsPerItem is how many screen rows a session takes in the list.
func (m model) rowsPerItem() int {
	if m.table {
		return 1
	}
	return 2
}

// listRow is one screen row of the list: row part of session item, or, when
// item is -1, a group's header.
type listRow struct {
	item, part int
	header     string
}

// listRows lays the list out in screen rows: a header before each state
// section, and with ^X G before each folder within it (sortList keeps a
// section's and a group's sessions together).
func (m model) listRows() []listRow {
	counts := map[string]int{}
	sections := map[int]int{}
	for _, it := range m.filtered {
		counts[groupOf(it)]++
		sections[sectionOf(it.familyState)]++
	}
	var rows []listRow
	for i, it := range m.filtered {
		if s := sectionOf(it.familyState); i == 0 || s != sectionOf(m.filtered[i-1].familyState) {
			sec := pinnedSection
			if s >= 0 {
				sec = stateSections[min(s, len(stateSections)-1)]
			}
			rows = append(rows, listRow{item: -1, header: fmt.Sprintf("  \033[%sm%s\033[0m \033[90m%d\033[0m", sec.colour, sec.name, sections[s])})
		}
		if k := groupOf(it); m.grouped && it.familyState >= 0 && (i == 0 || k != groupOf(m.filtered[i-1])) {
			fold, more := "", ""
			if m.collapsed(it.familyCwd) {
				fold, more = "▸ ", fmt.Sprintf(" · %d more", counts[k]-1)
			}
			rows = append(rows, listRow{item: -1, header: fmt.Sprintf("    \033[1;37m%s%s\033[0m \033[90m%d%s\033[0m", fold, projectName(groupKey(it.familyCwd)), counts[k], more)})
		}
		if it.hidden {
			continue
		}
		for p := range m.rowsPerItem() {
			rows = append(rows, listRow{item: i, part: p})
		}
	}
	return rows
}

// listStart is the first of h rows in view: the top, until the selection's
// last row would pass the bottom, then that row at the bottom.
func (m model) listStart(rows []listRow, h int) int {
	last := 0
	for r, row := range rows {
		if row.item == m.cursor {
			last = r
		}
	}
	start := max(0, last-h+1)
	for start < len(rows) && rows[start].part > 0 { // never open on half a session
		start++
	}
	return start
}

// groupKey is the folder a session is grouped under: its working directory,
// with a worktree under .claude/worktrees counted as its repo.
func groupKey(cwd string) string {
	if i := strings.Index(cwd, "/.claude/worktrees/"); i >= 0 {
		return cwd[:i]
	}
	return cwd
}

// sidebarRow is one session in the sidebar, w wide, on two rows: its marks
// and title (and HITS while searching), then a grey line of when it was
// last active, its context and project, with its PR counts on the right.
func (m model) sidebarRow(item listItem, selected bool, w int) [2]string {
	conv := item.conv
	marks, colouredMarks := m.listMarks(item)
	// The title row's right: running tasks, then HITS while searching, so the
	// HITS stay lined up at the edge; the PR counts have the grey row below.
	hits, colouredHits := "", ""
	if m.searching() {
		hits = fmt.Sprint(m.hitCount(item))
		colouredHits = "\033[36m" + hits + "\033[0m"
	}
	if tasks := m.taskCell(conv, false); tasks != "" {
		hits, colouredHits = strings.TrimSpace(tasks+" "+hits), strings.TrimSpace(m.taskCell(conv, true)+" "+colouredHits)
	}
	// One space before the marks, so their glyphs sit under the search's >;
	// with ^X G two more, so sessions sit in from their folder's header.
	lead := " "
	if m.grouped {
		lead = "   "
	}
	tw := w - len(lead) - colMarks - ansi.StringWidth(hits) - 1
	nest := nestPrefix(item.depth)
	topic := padRight(nest+listTopic(conv, tw-ansi.StringWidth(nest)), tw)
	prs := strings.TrimLeft(m.prCell(conv, !selected), " ")
	stats := strings.TrimSuffix(formatAgo(conv.LastTimestamp, time.Now()), " ago")
	if conv.ContextTokens > 0 {
		stats += " · " + formatTokens(conv.ContextTokens) + " ctx"
	}
	if h := m.hosts[conv.SessionID]; h != "" && m.live[conv.SessionID] {
		stats += " · in " + h
	}
	stats += " · " + projectName(conv.Cwd) // last: it's the one cut short
	sw := w - len(lead) - colMarks - ansi.StringWidth(prs) - 1
	stats = padRight(truncate(stats, sw), sw)
	indent := strings.Repeat(" ", len(lead)+colMarks)
	if selected { // the highlight marks it, no >
		return [2]string{
			selectedStyle.Render(padRight(lead+marks+topic+" "+hits, w)),
			selectedStyle.Render(padRight(indent+stats+" "+prs, w)),
		}
	}
	return [2]string{
		lead + colouredMarks + topic + " " + colouredHits,
		indent + "\033[90m" + stats + "\033[0m " + prs,
	}
}

// projectName is the last part of a session's working directory.
func projectName(cwd string) string {
	if i := strings.LastIndex(cwd, "/"); i >= 0 {
		return cwd[i+1:]
	}
	return cwd
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

// settingsPath keeps the view ccs reopens in (^X T layout, ^X G grouping)
// across restarts.
var settingsPath = func() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "ccs", "settings.json")
}()

type settings struct {
	Table     bool `json:"table,omitempty"`
	Grouped   bool `json:"grouped,omitempty"`
	AutoFocus bool `json:"auto_focus,omitempty"`
}

func (m model) settings() settings {
	return settings{Table: m.table, Grouped: m.grouped, AutoFocus: m.autoFocus}
}

// loadSettings reads them; a missing or unreadable file means the defaults.
func loadSettings() (s settings) {
	if b, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

// saveSettings writes them atomically. A failure only means the next start
// opens in the default view, so it isn't reported.
func saveSettings(s settings) {
	data, _ := json.Marshal(s)
	if os.MkdirAll(filepath.Dir(settingsPath), 0o700) != nil {
		return
	}
	tmp := settingsPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, settingsPath)
	}
}

// refreshNow (^R) does now what the minute's ticks do: rescans the sessions,
// re-asks GitHub about PRs and the allowance, and checks for a ccs update.
// A scan already running isn't started twice; the one-minute chain carries on.
func (m *model) refreshNow() tea.Cmd {
	var cmds []tea.Cmd
	if m.refreshStarted.IsZero() {
		cmds = append(cmds, m.startRefresh(true))
	}
	m.prNext = time.Time{} // the next Update starts a PR round
	if m.allowanceEnabled {
		m.allowanceAt = time.Time{}
		cmds = append(cmds, m.allowanceCmd())
	}
	if m.checkLatest != nil {
		cmds = append(cmds, m.checkUpdateCmd())
	}
	return tea.Batch(cmds...)
}

// killed is what ^K, ^U or ^W last deleted in any text box, for ^Y to put
// back, as in Claude Code's prompt (the text box has the rest of its keys).
var killed string

// editKey passes a message to a text box, adding Claude Code's ^Y.
func editKey(in textinput.Model, msg tea.Msg) (textinput.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return in.Update(msg)
	}
	before, pos := []rune(in.Value()), in.Position()
	switch k.String() {
	case "ctrl+y":
		if killed != "" {
			in.SetValue(string(before[:pos]) + killed + string(before[pos:]))
			in.SetCursor(pos + len([]rune(killed)))
		}
		return in, nil
	case "ctrl+k":
		if pos < len(before) {
			killed = string(before[pos:])
		}
	case "ctrl+u":
		if pos > 0 {
			killed = string(before[:pos])
		}
	}
	in, cmd := in.Update(msg)
	if s := k.String(); s == "ctrl+w" || s == "alt+backspace" {
		if p := in.Position(); p < pos {
			killed = string(before[p:pos])
		}
	}
	return in, cmd
}

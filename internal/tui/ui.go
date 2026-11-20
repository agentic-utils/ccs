package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

	"github.com/agentic-utils/ccs/internal/github"
	"github.com/agentic-utils/ccs/internal/mcp"
	"github.com/agentic-utils/ccs/internal/selfupdate"
	"github.com/agentic-utils/ccs/internal/sessions"
	"github.com/agentic-utils/ccs/internal/state"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// listItem holds display and search data for a conversation
type listItem struct {
	conv        transcript.Conversation
	searchText  string // All searchable content
	searchLower string // searchText lowercased once, for case-insensitive filtering
	rank        int    // matchRank for the current search, 0 without one
	depth       int    // nested under the session that started it (sortList)
	familyCwd   string // its top-level session's cwd, which ^X G groups the family by
	state       int    // its liveGroup as of the last sortList
	familyState int    // liveGroup of its family's most urgent session: the state section it's listed in
	sectioned   bool   // familyState is set: listed under a section heading (nestFamilies)
	kids        int    // sessions launched under it (an orchestrator's tree), any depth
	guides      string // in an orchestrator's tree: the │ and spaces its ancestors draw before its own connector
	lastKid     bool   // the last of its parent's children: └, not ├
	cooling     bool   // nested, not running, but active within closedCooldown: still shown, its grey line says "closed"
	closedKids  int    // its launched sessions that have closed (none of their tree running), behind a "N closed" line
	hideClosed  bool   // closed, behind its parent's "▸ N closed" line until ^X C opens that fold
	linesAfter  []closedLine
}

// closedLine is a parent's "▸ N closed" line in an orchestrator's tree,
// drawn after the row it's attached to (the last of the parent's branch
// before its closed sessions): the guides its children draw, how many have
// closed, whether they're shown, and the parent's depth.
type closedLine struct {
	guides string
	n      int
	owner  int
	open   bool   // the closed sessions are shown, below the line
	id     string // the parent's session id: the fold ^X C or a click opens or closes
	name   string // the parent, as foldName names it
}

// selectedStyle highlights the cursor row. The rest of the UI is rendered with
// raw ANSI escapes in View/formatListItem/renderPreview.
var selectedStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("62")).
	Foreground(lipgloss.Color("230")).
	Bold(true)

// model is the bubbletea application state
type model struct {
	items            []listItem
	filtered         []listItem
	textInput        textinput.Model
	cursor           int
	previewScroll    int // lines scrolled back from the newest message; 0 shows the latest at the bottom
	width            int
	table            bool                 // ^X T: the full-width table instead of the sidebar
	parents          map[string]string    // spawned SessionID -> the session whose claude -p started it
	noParent         map[string]int       // spawned SessionID -> len(items) when no parent was found
	grouped          bool                 // ^X X G: sessions grouped by folder
	chordX           bool                 // ^X pressed: the next key picks a ccs action
	byID             map[string]int       // m.items by session id, for shownAncestor; rebuilt when an entry is stale
	openClosed       map[string]bool      // ^X C: parents (by session id) whose closed sessions are shown (saved)
	headerCollapsed  bool                 // ^X F or a click on the summary line: the details under it are hidden
	selectedID       string               // the selected session, to see when it changes
	selectedAt       time.Time            // when it was selected: its preview waits previewDebounce
	marks            *sessionMarks        // when each session was last looked at (done)
	blurred          bool                 // the terminal says ccs isn't focused: nothing is being seen
	hooksMissing     []string             // ccs's hooks missing from Claude Code's settings, when some are there (hooks.go)
	menus            map[string]*heldMenu // each held question menu's progress, by prompt id (menu.go)
	menuShown        string               // the held prompt whose menu is on screen: menuFresh starts when it changes
	menuLeft         string               // the held prompt left with Esc: its keys go to the list until it's selected again (or →, Enter)
	viewing          string               // the done session selected and seen: marked seen when the selection leaves it
	snoozeOpen       bool                 // the ^X S picker
	snoozeCursor     int
	autoFocus        bool                 // ^X A: switch to a session that starts waiting on you (autoFocusCmd)
	focusBusy        bool                 // an auto-focus attempt is in flight
	focusHeld        string               // the session auto-focus switched to; none other until it's answered
	focusHandled     map[string]time.Time // SessionID -> the wait (statusUpdatedAt) auto-focus has dealt with
	focusNoted       map[string]bool      // auto-focus steps logged, by session, step and wait
	height           int
	listHeight       int // Calculated visible list height
	selected         *transcript.Conversation
	fork             bool // exec the selected conversation with --fork-session
	quitting         bool
	claudeFlags      []string
	confirmDelete    bool                 // Are we in delete confirmation mode?
	deleteIndex      int                  // Index of item to delete
	confirmPrune     bool                 // Are we in prune confirmation mode?
	pruneIndex       int                  // Index of item to prune
	pruneSaved       int64                // Bytes the pending prune would reclaim (measured on Ctrl+X X)
	renaming         bool                 // Are we typing a new name?
	resuming         bool                 // an Enter/Ctrl+F open is in flight
	opened           map[string]time.Time // sessions ccs opened in a tab, until their session file appears
	renameIndex      int                  // Index of item being renamed
	renameInput      textinput.Model
	launchStep       int // ^X N: 1 typing the new session's name, 2 its brief
	launchInput      textinput.Model
	launchName       string                        // the name typed at step 1
	launchCwd        string                        // where it starts: the selected session's folder
	errorMsg         string                        // Show deletion/prune errors
	noteMsg          string                        // errorMsg set by note(): information, not an error, so not red
	preview          *previewCache                 // memoised preview lines for the selected conversation
	peerNotes        *peerLabelCache               // memoised peerLabels for the selected conversation
	hits             *hitCounter                   // memoised per-query hit counts, keyed by SessionID
	lastFilterQuery  string                        // lowercased query the current m.filtered was built from
	live             map[string]bool               // SessionIDs attached to a running claude process
	livePIDs         map[string]int                // live SessionID -> claude pid (for its session file and socket)
	hosts            map[string]string             // live sessions run by an app with no terminal (hostOf): ccs can't switch to them
	bg               map[string]sessions.BgSession // background sessions (claude --bg), opened with claude attach
	agentsAt         time.Time                     // when claude agents was last asked
	confirmStop      string                        // ^X K asked whether to stop this session: y stops it
	confirmServeStop bool                          // ^X B asked whether to stop ccs serve's agent: y stops it
	heldPrompt       map[string]mcp.HeldPrompt     // prompts held by ccs serve or this ccs, by session (held.go)
	heldAt           time.Time
	heldBusy         bool
	waitSeen         map[string]time.Time    // when ccs saw each session's current wait begin (setStat): its held copy may still be on its way (heldPending)
	paneAt           time.Time               // when Update last finished: the bottom pane's clock (paneNow)
	heldAnswered     map[string]answeredWait // the wait each held prompt ccs answered was part of: hidden while the status still reports it
	heldGone         map[string]time.Time    // when each session's held prompt left the list (answered, sent back): its status may still say waiting
	heldConfirmed    *heldConfirm            // a digit, A or D, waiting for y/n
	heldFocus        bool                    // the bottom pane's held prompt has the keys
	heldAutoAt       time.Time               // when an auto-select focused it (zero: by hand, or the guard has passed)
	heldNote         string                  // the pane's last held-prompt outcome
	heldNoteFor      string                  // the session heldNote is about: shown only while it's selected
	heldNoteShown    bool                    // heldNote is in the main view's note line too
	heldErr          string                  // the pane's last held-prompt error
	selectHandled    map[string]string       // SessionID -> the wait (or held prompt) auto-select has dealt with
	autoSelectAt     time.Time               // the last auto-select: the next waits autoFocusIdle after it, as after a key
	chatAfter        string                  // answered from ccs: this session's message box takes the keys once its menu has gone
	chatAfterID      string                  // the held prompt that answer was for
	serveQROpen      bool                    // ^X B's popup while the agent runs: its address and QR code
	serveQR          string                  // the QR code, drawn
	serveQRWidth     int                     // its width in cells
	serveQRURL       string                  // the address it encodes
	serveQRErr       string                  // why there's no address, if not
	serve            mcp.ServiceStatus       // ccs serve's launchd agent, as last read (serveCheckCmd)
	setup            *serveSetup             // ^X B's setup popup (serve_setup.go), nil when closed
	servePreview     previewState            // the preview child started from it, as shown
	serveCheckedAt   time.Time
	serveChecking    bool
	confirmRestart   string         // ^X R asked whether to restart this session: y restarts it
	confirmHandover  string         // ^X W asked whether to hand this session over to its parent and close it: y does
	handoverTo       string         // with confirmHandover: the parent it goes to
	reorgPlan        *reorgProposal // ^X M's plan, shown in its popup until enter applies it or esc drops it
	reorgShownAt     time.Time      // when it came on screen: enter waits updateKeyGrace
	reorgReady       *reorgProposal // a plan that came back while the user was typing: it opens once they pause
	reorgScroll      int            // the popup's first line in view
	reorgBusy        string         // what ^X M is doing: "planning", "reorganising", or ""
	restartForce     bool           // with confirmRestart: it was mid-turn when asked, so y forces it

	// transcript.Message box for the selected live session (see chat*).
	chatFocus  bool // typing goes to the message box, not the search
	chatInput  textinput.Model
	chatFor    string                          // the session chatInput's draft belongs to
	chatDrafts map[string]string               // the other sessions' drafts, so a draft never follows the selection
	pasted     map[string]*sessionImages       // images behind the [Image #N] in each session's boxes
	chatTickOn bool                            // the fast refresh for the selected live session is running
	chatStatus map[string]sessions.SessionStat // every live session's last known status (set only by setStat; applyStats drops sessions that are gone)
	pending    map[string]string               // text sent but not yet seen in the transcript, by SessionID
	sendNote   map[string]string               // outcome of the last send, by SessionID
	sentAt     map[string]time.Time            // when a socket send finished, by SessionID, until the session queues it
	held       map[string]bool                 // a receipt said the session is holding the message for approval
	sending    bool
	run        *shellRun // the last ! command run from the message box (run.go)
	runGen     int       // bumped per run, so a stale result or tick is dropped

	unverified     map[string]bool                 // live sessions whose pid couldn't be checked against their file
	lastScan       map[string]sessions.LiveSession // the last live tick's sessions, so the next can keep one whose file is briefly unreadable
	reload         func() ([]listItem, error)      // re-scans conversations; nil disables auto-refresh
	gen            int                             // bumped by delete/prune/rename so an older in-flight refresh can't undo them
	refreshStarted time.Time                       // when the in-flight scan began; zero when none
	lastRefresh    time.Time                       // when the list last matched disk (startup or a completed scan)
	lastKick       time.Time                       // last full scan started early because an unknown session went live
	kicked         map[string]bool                 // unknown live sessions an early scan was already started for
	refreshFailed  bool                            // the last scan errored; the list is from lastRefresh

	// Self-update. checkLatest nil disables the check (tests, dev builds);
	// upgrade installs tag and returns the binary to restart; nil means ccs
	// can't update this install itself (e.g. Nix), so only notify.
	checkLatest    func() (string, error)
	upgrade        *selfupdate.Upgrader
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
	autoUpdate        bool   // ^X I: install a new version once you're away (the default)
	profileStop       func() // ^X Q: a CPU profile is being recorded to profilePath; this stops it
	profilePath       string
	autoFailed        string    // the version an automatic install failed for: offered, not retried
	autoInstalling    bool      // this install started on its own: the new ccs says so
	lastKeyAt         time.Time // the last key press, for "away"
	updatedFrom       string    // started after updating itself from this version: the popup says so
	updatedOpen       bool
	updateErr         string // last install failure, shown in the popup with a retry
	updateCheckFailed bool
	updateHeld        bool   // popup waited behind another prompt; restart its key grace when it shows
	updateWaitLogged  bool   // "update waits: <why>" logged for this wait
	restartReady      string // an installed update whose restart waits for held prompts to settle
	updating          bool
	dismissed         string // tag the user said "later" to
	restart           string // after an upgrade: binary to exec once the TUI exits

	// Usage screen (^X Y).
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

	reopen        []transcript.Conversation // sessions open before a restart, offered for reopening
	reopenChecked bool                      // the last boot's record has been read, so it may be overwritten

	orchOffer *orchestratorPlan // the set-up popup is open, with what it will change
	orchBusy  bool              // opening or setting it up: never twice at once
	orchAsked bool              // the open offer came from ^X O
	orchSince time.Time         // when the offer last came on screen: keys wait for its grace
	orchID    string            // the orchestrator session, listed in its own section
	// Orchestrator trees (tree.go): launch-parents.json as last read, each
	// listed session's resolved parent, and the folded trees' sessions,
	// kept out of m.filtered until the next updateFilter or sortList.
	launchRecs   sessions.ParentsFile
	launchMod    time.Time
	launchLoaded bool
	launchRes    map[string]string
	launchResN   int
	foldedAway   []string
	// hideSidebar (^X L): the sidebar layout shows only the preview, full width (saved).
	hideSidebar bool
	// Each session's PRs (prs.go): what GitHub said, by address.
	prs        map[string]github.PRInfo
	prNext     time.Time       // when the next round is due
	prQueue    [][]string      // the round's requests not sent yet
	prRound    int             // the round's requests in flight
	prSelBusy  bool            // the selected session's own request is in flight
	prSelAsked map[string]bool // asked for the selected session since the last round
	prSelected string          // the session whose PRs were last refreshed for being selected

	revealed map[string]bool // headless sessions shown anyway: opened by selectSession
	prErr    string
	fetchPRs func([]string) (map[string]github.PRInfo, error) // nil: never look PRs up
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

// sidebarWidth is the session list's width beside the preview, 0 in the table
// or while ^X L hides it. A search filters the list, so it shows while one is
// typed and hides again once the search is cleared, ^X L staying as it was.
func (m model) sidebarWidth() int {
	if m.table || m.hideSidebar && m.textInput.Value() == "" {
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
	if m.linksOpen || m.snoozeOpen || len(m.reopen) > 0 || m.reorgPlan != nil || m.offerShown() {
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
	if m.popupOpen() || m.showUsage || m.prompting() || msg.Action != tea.MouseActionPress {
		return m
	}
	if msg.Button == tea.MouseButtonLeft && msg.Y == 1 && cswapPath() != "" {
		if start, end, ok := m.accountSpan(); ok && msg.X >= start && msg.X < end {
			stats.Record("tui.account", stats.Mouse)
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
		if _, _, held := m.selectedHeld(); held && m.chatRows() > 0 && msg.Y >= m.height-m.chatRows() && msg.X >= paneX { // the held prompt's pane
			m.heldFocus, m.heldAutoAt = true, time.Time{}
			return m
		}
		if m.chatRows() > 0 && !m.boxHidden() && msg.Y >= m.height-3 && msg.X >= paneX { // the message box
			if !m.chatFocus {
				stats.Record("tui.box", stats.Mouse)
			}
			m.chatFocus = true
			return m
		}
		if onPreview && len(m.filtered) > 0 {
			// ccs has the mouse, so the terminal never sees a click on a link: open it here.
			lines := strings.Split(m.renderPreview(m.filtered[m.cursor], m.previewRenderHeight()), "\n")
			if row := msg.Y - previewTop; row >= 0 && row < len(lines) {
				m.linkPending = linkAt(lines[row], msg.X-paneX)
				// A click anywhere on the summary line (its first) shows or hides
				// the details; the detail rows below it hold links and stay put.
				if m.linkPending != "" {
					stats.Record("tui.link_open", stats.Mouse)
				} else if row == 0 {
					stats.Record("tui.details", stats.Mouse)
					m.toggleHeader()
				}
			}
			return m
		}
		if msg.Y < listTop || msg.Y >= listTop+listHeight {
			return m
		}
		rows := m.listRows()
		r := m.listStart(rows, listHeight) + msg.Y - listTop
		if r < len(rows) && rows[r].item < 0 && rows[r].fold != "" { // a "N closed" line: the mouse's ^X C
			stats.Record("tui.tree_view", stats.Mouse)
			m.toggleFold(rows[r].fold, rows[r].foldName)
			return m
		}
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

// stoppable reports whether ^X K can stop the session: a background one, or
// a live one in a terminal tab (an app's has no tab to close).
func (m model) stoppable(id string) bool {
	if !m.live[id] {
		return false
	}
	if _, bg := m.bg[id]; bg {
		return true
	}
	_, live := m.livePIDs[id]
	return live && m.hostedApp(id) == ""
}

// prompting reports an open rename, delete, prune or stop prompt.
func (m model) prompting() bool {
	return m.renaming || m.confirmDelete || m.confirmPrune || m.confirmStop != "" || m.confirmServeStop || m.confirmRestart != "" || m.confirmHandover != "" || m.launchStep > 0
}

// restartable reports whether ^X R can restart the session: a live Claude
// Code session in a terminal tab (not a background one, an app's or Codex).
func (m model) restartable(id string) bool {
	if !m.live[id] {
		return false
	}
	if _, bg := m.bg[id]; bg {
		return false
	}
	i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id })
	if i < 0 || m.filtered[i].conv.Kind == transcript.KindCodex {
		return false
	}
	_, live := m.livePIDs[id]
	return live && m.hostedApp(id) == ""
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

	links := textinput.New()
	links.Prompt = "> "
	links.Placeholder = "type to filter..."
	links.Cursor.SetMode(cursor.CursorStatic)
	links.Focus()
	m := model{
		chatInput:   chat,
		linksInput:  links,
		items:       items,
		textInput:   ti,
		claudeFlags: claudeFlags,
		preview:     &previewCache{},
		peerNotes:   &peerLabelCache{},
		opened:      make(map[string]time.Time),
		kicked:      make(map[string]bool),
		hits:        &hitCounter{byID: make(map[string]int)},
		prs:         make(map[string]github.PRInfo),
		prSelAsked:  make(map[string]bool),
	}
	m.updateFilter()
	return m
}

func (m *model) updateFilter() {
	m.foldedAway = nil // rebuilt from m.items, folded sessions included
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
			if transcript.QueryMatches(item.searchLower, item.conv.Title, terms) {
				next = append(next, item)
			}
		}
		for i := range next {
			next[i].rank = transcript.MatchRank(next[i].searchLower, next[i].conv.Title, terms)
		}
		m.filtered = next
	}
	m.hideHeadless()
	m.hideReorgRuns()
	m.sortList()
	m.lastFilterQuery = queryLower
	// Keep cursor in bounds
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.previewScroll = 0
}

// showHeadless (--all) lists headless runs (transcript.Conversation.headless) too.
var showHeadless bool

// hideHeadless drops headless runs from m.filtered unless --all, or the run
// is pinned or opened through selectSession. Nesting under a shown parent
// doesn't keep one: a script's claude -p loop would hang dozens under it.
// Running doesn't count either: a backtest runs dozens at once, and a run
// anyone talks to gets a second prompt and stops being headless.
func (m *model) hideHeadless() {
	if showHeadless {
		return
	}
	shown := func(c transcript.Conversation) bool {
		return !c.Headless() || m.pinIndex(c.SessionID) >= 0 || m.revealed[c.SessionID]
	}
	kept := m.filtered[:0]
	for _, it := range m.filtered {
		if shown(it.conv) {
			kept = append(kept, it)
		}
	}
	m.filtered = kept
}

// hideReorgRuns keeps reorg planning runs out of a search
// (transcript.HideFromSearch) unless the search names one or it was selected
// by name. A run stays listed with no search.
func (m *model) hideReorgRuns() {
	if !m.searching() {
		return
	}
	q := m.textInput.Value()
	m.filtered = slices.DeleteFunc(m.filtered, func(it listItem) bool {
		return transcript.HideFromSearch(it.conv, q) && !m.revealed[it.conv.SessionID]
	})
}

// liveGroup is where a session sits in the list: waiting on a menu or
// prompt, then idle with a question in its last message, then idle (done
// ones too, marked only by colour, so opening one never moves it), then
// busy, then closed (not running in claude or codex).
func (m model) liveGroup(c transcript.Conversation) int {
	switch {
	case m.putAway(c) != awake: // the bottom sections: settled, snoozed
		return 4 + m.putAway(c)
	case !m.live[c.SessionID]:
		return 4
	case m.waitSignal(c) == waitPrompt:
		return 0
	case m.waitSignal(c) == waitAsked: // idle, its last message asks something
		return 1
	case m.chatStatus[c.SessionID].Status == "busy":
		return 3
	}
	return 2
}

// sortList orders the list by liveGroup, then best search match (named after
// the query, then the words as a phrase), then newest, keeping the cursor on
// the same session. Statuses change on ticks, so Update runs it after every
// message but those that only move the selection (movesOnly).
func (m *model) sortList() {
	if len(m.filtered) == 0 {
		return
	}
	id := m.filtered[min(m.cursor, len(m.filtered)-1)].conv.SessionID
	if len(m.foldedAway) > 0 { // back in, as they are now, to be nested and counted again
		away := map[string]bool{}
		for _, a := range m.foldedAway {
			away[a] = true
		}
		for _, it := range m.items {
			if away[it.conv.SessionID] {
				m.filtered = append(m.filtered, it)
			}
		}
		m.foldedAway = nil
	}
	for i := range m.filtered { // once each: the comparisons below run n log n times
		m.filtered[i].state = m.liveGroup(m.filtered[i].conv)
	}
	sortItems(m.filtered, func(a, b *listItem) bool {
		if a.state != b.state {
			return a.state < b.state
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.conv.LastTimestamp > b.conv.LastTimestamp
	})
	m.nestFamilies()
	// State sections (waiting, asked, idle, busy, not open), a family in its
	// most urgent session's; with ^X G, folders within each, a group where its
	// first session lands, so the one holding a question comes first.
	// Families stay together under their top session's folder.
	first := map[group]int{}
	if m.grouped {
		for i := range m.filtered {
			if k := groupOf(&m.filtered[i]); first[k] == 0 {
				first[k] = i + 1
			}
		}
	}
	sortItems(m.filtered, func(a, b *listItem) bool {
		if a.familyState != b.familyState {
			return a.familyState < b.familyState
		}
		return first[groupOf(a)] < first[groupOf(b)]
	})
	if i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id }); i >= 0 {
		m.cursor = i
	}
	m.cursor = max(0, min(m.cursor, len(m.filtered)-1))
}

// sortItems stable-sorts items by less through an index, so a comparison
// never copies a listItem (each holds a whole transcript.Conversation).
func sortItems(items []listItem, less func(a, b *listItem) bool) {
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return less(&items[idx[i]], &items[idx[j]]) })
	permute(items, idx)
}

// permute reorders items so items[i] becomes the old items[idx[i]], in place,
// cycle by cycle, rather than through a second whole list; idx is used up.
func permute(items []listItem, idx []int) {
	for i := range idx {
		if idx[i] < 0 || idx[i] == i {
			continue
		}
		tmp := items[i]
		j := i
		for idx[j] != i {
			k := idx[j]
			items[j] = items[k]
			idx[j] = -1
			j = k
		}
		items[j] = tmp
		idx[j] = -1
	}
}

// step moves the cursor by one session (dir -1 up, 1 down); false at either end.
func (m *model) step(dir int) bool {
	if i := m.cursor + dir; i >= 0 && i < len(m.filtered) {
		m.cursor = i
		return true
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
	launched := m.launchParents()
	orch := make([]bool, n) // started others through ccs launch: an orchestrator
	for i, it := range m.filtered {
		parent[i] = -1
		if p, ok := at[launched[it.conv.SessionID]]; ok && p != i {
			parent[i] = p
			kids[p] = append(kids[p], i)
			orch[p] = true
			continue
		}
		if _, bg := m.bg[it.conv.SessionID]; it.conv.Spawned || bg {
			if p, ok := at[m.shownAncestor(m.spawnParent(it.conv), at, launched)]; ok && p != i {
				parent[i] = p
				kids[p] = append(kids[p], i)
			}
			continue
		}
		if p := codexLauncher(m.filtered, i); p >= 0 {
			parent[i] = p
			kids[p] = append(kids[p], i)
			orch[p] = true
		}
	}
	// A nested session, launched or spawned, is closed when nothing in its
	// tree runs; it waits behind its parent's "N closed" line. A
	// search shows everything where it is.
	closed := make([]bool, n)
	if !m.searching() {
		alive := make([]int8, n) // 0 unknown, 1 alive, 2 not
		var isAlive func(i, d int) bool
		isAlive = func(i, d int) bool {
			if alive[i] == 0 {
				alive[i] = 2
				if c := m.filtered[i].conv; m.live[c.SessionID] || codexSubagentBusy(c) || justClosed(c) {
					alive[i] = 1
				}
				for _, k := range kids[i] {
					if d < n && isAlive(k, d+1) {
						alive[i] = 1
					}
				}
			}
			return alive[i] == 1
		}
		for i := range m.filtered {
			// Launched or spawned (a codex subagent under the session that
			// ran it, say): either kind waits behind the line when closed.
			if parent[i] >= 0 {
				closed[i] = !isAlive(i, 0)
			}
		}
	}
	for i := range m.filtered { // a nested session that just closed: shown a while, marked
		c := m.filtered[i].conv
		m.filtered[i].cooling = parent[i] >= 0 && !m.live[c.SessionID] && !codexSubagentBusy(c) && justClosed(c)
	}
	order := make([]int, 0, n) // the new list, as indexes into m.filtered
	seen := make([]bool, n)
	var walk func(i, depth int, cwd, guides string, last bool)
	walk = func(i, depth int, cwd, guides string, last bool) {
		if seen[i] {
			return
		}
		seen[i] = true
		it := &m.filtered[i]
		it.depth, it.familyCwd, it.guides, it.lastKid = depth, cwd, guides, last
		it.closedKids, it.hideClosed, it.linesAfter = 0, false, nil
		order = append(order, i)
		below := guides // the guides its children draw: │ under it while siblings follow it
		if depth > 0 {
			below += map[bool]string{true: "  ", false: "│ "}[last]
		}
		var todo []int
		for _, k := range kids[i] {
			if !seen[k] {
				todo = append(todo, k)
			}
		}
		var open, shut []int
		for _, k := range todo {
			if closed[k] {
				shut = append(shut, k)
			} else {
				open = append(open, k)
			}
		}
		// Shown, closed sessions follow the live ones at the same level.
		// Hidden, a "N closed" line ends the branch after the last row so
		// far, and they wait behind it until ^X C shows them.
		// A "N closed" line names the parent's closed sessions and the key
		// that shows or hides them (^X C). Hidden, it ends the branch and they
		// wait behind it. Shown, it heads them at their live siblings' level,
		// so they read as siblings, not as children of the line.
		showing := m.showingClosed(it.conv.SessionID)
		for j, k := range open {
			walk(k, depth+1, cwd, below, j == len(open)-1 && len(shut) == 0)
		}
		it.closedKids = len(shut)
		if len(shut) == 0 {
			return
		}
		at := order[len(order)-1]
		m.filtered[at].linesAfter = append(m.filtered[at].linesAfter, closedLine{below, len(shut), depth, showing, it.conv.SessionID, foldName(it.conv)})
		from := len(order)
		for j, k := range shut {
			walk(k, depth+1, cwd, below, j == len(shut)-1)
		}
		if showing {
			return
		}
		for _, k := range order[from:] {
			m.filtered[k].hideClosed = true
		}
	}
	for i := range m.filtered {
		r := i
		for d := 0; parent[r] >= 0 && d < n; d++ { // up to its top session (d guards a cycle)
			r = parent[r]
		}
		walk(r, 0, m.filtered[r].conv.Cwd, "", true)
		walk(i, 0, m.filtered[i].conv.Cwd, "", true) // only reached by a cycle
	}
	isOrch := make([]bool, len(order)) // orch, in the new order
	for at, i := range order {
		isOrch[at] = orch[i]
	}
	permute(m.filtered, order)
	out := m.filtered
	for start := 0; start < len(out); { // families are contiguous, each from its depth-0 session
		end := start + 1
		for end < len(out) && out[end].depth > 0 {
			end++
		}
		launches := slices.Contains(isOrch[start:end], true)
		if launches { // each session's tree below it: how many, and what needs a look
			for i := end - 1; i >= start; i-- {
				out[i].kids = 0
				for j := i + 1; j < end && out[j].depth > out[i].depth; j++ {
					out[i].kids++
				}
			}
		}
		state := out[start].state
		for i := start + 1; i < end; i++ {
			state = min(state, out[i].state)
		}
		if pin := m.familyPin(out[start:end]); pin >= 0 { // above every state, in pin order
			state = pin - len(m.marks.Pinned)
		}
		if launches || m.orchID != "" && slices.ContainsFunc(out[start:end], func(it listItem) bool { return it.conv.SessionID == m.orchID }) {
			state = orchFamily // above the pins too
		}
		for i := start; i < end; i++ {
			out[i].familyState, out[i].sectioned = state, true
		}
		start = end
	}
	m.foldTrees()
}

// foldTrees takes the sessions under each folded orchestrator out of
// m.filtered into m.foldedAway (sortList puts them back before the next
// sort), and counts them on its row. A search folds nothing, so every match
// shows.
func (m *model) foldTrees() {
	if m.searching() {
		return
	}
	kept := m.filtered[:0]
	under := -1 // depth of the folded row whose tree is being skipped
	for i := range m.filtered {
		it := m.filtered[i]
		if under >= 0 && it.depth > under || it.hideClosed {
			m.foldedAway = append(m.foldedAway, it.conv.SessionID)
			// An ancestor's closed line attached to it moves to the last row shown.
			if k := len(kept) - 1; k >= 0 {
				for _, l := range it.linesAfter {
					if l.owner < it.depth && (under < 0 || l.owner < under) {
						kept[k].linesAfter = append(kept[k].linesAfter, l)
					}
				}
			}
			continue
		}
		kept = append(kept, it)
	}
	m.filtered = kept
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
func groupOf(it *listItem) group {
	if it.familyState < 0 {
		return group{state: it.familyState}
	}
	return group{it.familyState, groupKey(it.familyCwd)}
}

// group identifies a folder within a state section; comparable, so building
// one allocates nothing (sortList compares them n log n times).
type group struct {
	state  int
	folder string
}

// orchFamily is the orchestrator's familyState and orchestratorSec its
// section, above the pinned ones.
const (
	orchFamily      = -1 << 30
	orchestratorSec = -2
)

// sectionOf is the state section a familyState is listed in: pinned ones
// (negative, in pin order) share one, and the orchestrator has its own.
func sectionOf(state int) int {
	if state == orchFamily {
		return orchestratorSec
	}
	return max(state, -1)
}

// doneColour marks a session that finished since you last looked;
// waitColour one waiting on a menu or prompt, askedColour one whose last
// message asks something.
const (
	doneColour  = "1;36"
	waitColour  = "38;5;201"
	askedColour = "38;5;219"
)

// pinnedSection heads the pinned sessions, above every state.
var pinnedSection = struct{ name, colour string }{"pinned", "1;37"}

// orchestratorSection heads the orchestrator, above the pinned ones.
var orchestratorSection = struct{ name, colour string }{orchestratorName, "1;37"}

// stateSections name liveGroup's values, the list's sections, and colour them.
var stateSections = [...]struct{ name, colour string }{
	{"waiting on you", "1;" + waitColour}, {"asked you something", askedColour}, {"idle", "32"}, {"busy", amber}, {"closed", "90"}, {"settled", "90"}, {"snoozed", "90"},
}

// spawnParent is the session whose claude -p started c, "" if none is known.
// Found once and kept; a miss is retried when the list grows.
func (m *model) spawnParent(c transcript.Conversation) string {
	if c.ParentID != "" { // a Codex subagent names its parent thread
		return c.ParentID
	}
	if c.Team != "" { // an agent-team teammate: its team's lead
		if p := teamLeadOf(c, m.items); p != "" {
			return p
		}
	}
	if c.Kind == transcript.KindCodex {
		if p := sessions.ExecParentOf(c.SessionID); p != "" { // a codex exec seen running under that session
			return p
		}
	}
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

// closedCooldown is how long a nested session stays in view after its last
// message once it isn't running, before it goes behind its parent's
// "▸ N closed" line: time to look back at what just finished.
const closedCooldown = 30 * time.Minute

// justClosed: c's last message is within closedCooldown. Counted from the
// last message, which every transcript has, so it holds across a restart
// and for sessions that closed while ccs wasn't running.
func justClosed(c transcript.Conversation) bool {
	t, err := time.Parse(time.RFC3339, c.LastTimestamp)
	return err == nil && time.Since(t) < closedCooldown
}

// codexSubagentBusy: c is a Codex subagent written to within
// transcript.SubagentIdle. It has no process of its own (it runs inside its
// exec), so liveness can't say it's running.
func codexSubagentBusy(c transcript.Conversation) bool {
	if c.Kind != transcript.KindCodex || c.ParentID == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, c.LastTimestamp)
	return err == nil && time.Since(t) < transcript.SubagentIdle
}

// codexLauncher is, for a named Codex session with no recorded parent, the
// listed session whose transcript started it (`Launches`: open-tab.sh,
// ccs launch or claude --name naming it, or launch_session), the newest one
// that began before it; -1 if none. Codex has no SessionStart hook to
// record the parent when it starts, so this is how it gets one. Claude
// sessions get theirs from that hook, so a `ccs adopt --to none` stays put.
func codexLauncher(items []listItem, i int) int {
	c := items[i].conv
	if c.Kind != transcript.KindCodex || c.Title == "" {
		return -1
	}
	best := -1
	for j, it := range items {
		if j == i || !slices.Contains(it.conv.Launches, c.Title) || it.conv.FirstTimestamp > c.FirstTimestamp {
			continue
		}
		if best < 0 || it.conv.FirstTimestamp > items[best].conv.FirstTimestamp {
			best = j
		}
	}
	return best
}

// itemAt is id's index in m.items, through m.byID, rebuilt when an entry
// is stale or missing (the list changed).
func (m *model) itemAt(id string) (int, bool) {
	if i, ok := m.byID[id]; ok && i < len(m.items) && m.items[i].conv.SessionID == id {
		return i, true
	}
	m.byID = make(map[string]int, len(m.items))
	for i, it := range m.items {
		m.byID[it.conv.SessionID] = i
	}
	i, ok := m.byID[id]
	return i, ok
}

// shownAncestor is id, or when id isn't shown (a headless codex exec whose
// subagents are listed, say), the nearest session above it that is: its
// launch parent or the session that spawned it, a few levels up at most.
// So a spawned session nests under what's on screen instead of going flat.
func (m *model) shownAncestor(id string, at map[string]int, launched map[string]string) string {
	if _, ok := at[id]; ok || id == "" {
		return id
	}
	for range 5 {
		i, ok := m.itemAt(id)
		if !ok {
			return ""
		}
		next := launched[id]
		if next == "" {
			next = m.spawnParent(m.items[i].conv)
		}
		if next == "" || next == id {
			return ""
		}
		if _, ok := at[next]; ok {
			return next
		}
		id = next
	}
	return ""
}

// teamLeadOf is the lead session of teammate c's team: the team's config
// names it (leadSessionId), else, the config gone, the listed session whose
// id starts as the team's name says ("session-f3356504"). Never c itself.
func teamLeadOf(c transcript.Conversation, items []listItem) string {
	if p := transcript.TeamLead(c.Team); p != "" && p != c.SessionID {
		return p
	}
	prefix := transcript.TeamSessionPrefix(c.Team)
	if prefix == "" {
		return ""
	}
	for _, it := range items {
		if id := it.conv.SessionID; id != c.SessionID && strings.HasPrefix(id, prefix) {
			return id
		}
	}
	return ""
}

// findSpawnParent finds the session that ran a claude -p holding c's first
// prompt within ten minutes before c began (two for a short prompt); Claude
// Code records no parent.
// ponytail: matched on the prompt's start, so two sessions sending the same
// prompt at the same moment can't be told apart.
func findSpawnParent(c transcript.Conversation, items []listItem) string {
	first := ""
	for _, msg := range c.Messages {
		if msg.Role == "user" {
			first = transcript.SpawnText(msg.Text)
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
		for _, s := range it.conv.Spawns {
			t, err := time.Parse(time.RFC3339, s.Ts)
			if gap := start.Sub(t); err == nil && gap > -time.Minute && gap < bestGap && strings.Contains(s.Text, first) {
				best, bestGap = it.conv.SessionID, max(gap, -gap)
			}
		}
	}
	if best == "" && c.Kind == transcript.KindCodex && c.CodexExec {
		best = codexStarter(start, c.SessionID, items)
	}
	return best
}

// codexStartWindow is how long before a codex exec run began the command
// that started it may be: a few seconds in practice.
const codexStartWindow = time.Minute

// codexStarter finds the session that started a codex exec run when its
// prompt isn't on the command line (codex-delegate's codex-run.sh -f
// prompt.md): the one whose Bash call, its own or a subagent's, started
// codex closest before the run began, within codexStartWindow. Only sessions
// active then are read, so a run doesn't open every session's subagents.
// ponytail: two sessions starting codex in the same moment can't be told
// apart; the closer call wins.
func codexStarter(start time.Time, self string, items []listItem) string {
	best, bestGap := "", codexStartWindow
	for _, it := range items {
		c := it.conv
		if c.SessionID == self || c.Kind == transcript.KindCodex {
			continue
		}
		first, err1 := time.Parse(time.RFC3339, c.FirstTimestamp)
		last, err2 := time.Parse(time.RFC3339, c.LastTimestamp)
		if err1 != nil || err2 != nil || first.After(start) || last.Before(start.Add(-codexStartWindow)) {
			continue
		}
		for _, s := range append(slices.Clone(c.Spawns), transcript.SubagentSpawns(c)...) {
			t, err := time.Parse(time.RFC3339, s.Ts)
			if gap := start.Sub(t); err == nil && transcript.StartsCodex(s.Text) && gap > -5*time.Second && gap < bestGap {
				best, bestGap = c.SessionID, max(gap, -gap)
			}
		}
	}
	return best
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.reload != nil {
		cmds = append(cmds, transcript.RefreshTick(), liveTick(), waitReceipt, reopenCheckCmd, orchestratorProbe)
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
	if m.updatedFrom != "" && m.fetchNotes != nil { // what changed since the version it updated from
		from := m.updatedFrom
		cmds = append(cmds, func() tea.Msg {
			lines, err := selfupdate.ReleaseChangelog(from, "v"+state.Version)
			if err != nil {
				state.LogUpdate("changelog %s..%s: %v", from, state.Version, err)
			}
			return updatedChangelogMsg{lines}
		})
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
	n.openReadyReorg()
	if !movesOnly(msg) {
		n.sortList()
	}
	if settle := n.noteSelection(); settle != nil {
		cmd = tea.Batch(cmd, settle)
	}
	n.noteSeen() // done sorts with idle, so this never moves a session
	n.syncChatDraft()
	if n.chatFocus && (!n.selectedLive() || n.boxHidden()) {
		n.chatFocus = false
	}
	if _, _, held := n.selectedHeld(); n.heldFocus && !held {
		n.heldFocus, n.heldAutoAt = false, time.Time{}
	}
	n.keepChatAfterAnswer()
	n.autoSelect()
	if n.showUsage || n.helpOpen || n.popupOpen() { // another screen took over: the pane loses the keys
		n.heldFocus = false
	}
	// A search being typed takes the keys from a held menu for good: clearing
	// it doesn't give them back; selecting the session again or → does.
	if n.textInput.Value() != "" {
		if h, _, ok := n.selectedHeld(); ok {
			n.heldFocus, n.menuLeft = false, h.ID
		}
	}
	n.showMenu(n.heldShown())
	n.noteReview()
	n.paneAt = heldNow() // the pane's clock for this frame (paneNow)
	if pr := n.prCmd(); pr != nil {
		cmd = tea.Batch(cmd, pr)
	}
	if tick := n.ensureChatTick(); tick != nil {
		return n, tea.Batch(cmd, tick)
	}
	return n, cmd
}

// movesOnly reports a message that can only move the selection or scroll:
// the mouse and the arrow and page keys. Nothing it does changes the list's
// order (pinning and the like sort themselves), so Update skips the sort, which
// a wheel's burst of events would otherwise run dozens of times a second.
func movesOnly(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case tea.MouseMsg, previewSettledMsg:
		return true
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "down", "pgup", "pgdown", "ctrl+p", "ctrl+n":
			return true
		}
	}
	return false
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
		m.chatInput.Width = chatInputWidth(m.paneWidth())
		// Clear so a shrink doesn't leave wider stale rows behind.
		return m, tea.ClearScreen

	case transcript.RefreshTickMsg:
		if !m.refreshStarted.IsZero() { // an early scan is running; keep the schedule
			return m, transcript.RefreshTick()
		}
		return m, m.startRefresh(false)

	case attachDoneMsg:
		m.resuming = false
		switch {
		case msg.err != nil:
			m.errorMsg = fmt.Sprintf("Opening a new tab timed out, it may still have opened - check before retrying (%v)", msg.err)
		case !msg.opened:
			m.errorMsg = "A background session opens in a new tmux window or iTerm tab: run claude attach " + msg.id
		}
		return m, nil

	case launchDoneMsg:
		if msg.err != nil {
			m.errorMsg = "Couldn't start " + msg.name + ": " + msg.err.Error()
		} else if msg.l.Starting {
			m.note(fmt.Sprintf("%s is still starting on %s: its tab may be asking something", msg.name, msg.l.TTY))
		} else {
			m.note(fmt.Sprintf("Started %s (pid %d, %s)", msg.name, msg.l.PID, msg.l.TTY))
		}
		return m, nil

	case setupDoneMsg:
		return m, m.applySetupDone(msg)
	case previewMsg:
		m.applyPreview(msg)
		return m, nil
	case serveStatusMsg, serveDoneMsg:
		return m, m.applyServe(msg)

	case closeDoneMsg:
		if msg.err != nil {
			m.errorMsg = msg.err.Error()
		} else {
			m.note(msg.text)
		}
		return m, nil

	case stopDoneMsg:
		if msg.err != nil {
			m.errorMsg = "Couldn't stop " + msg.name + ": " + msg.err.Error()
		} else {
			m.note("Stopped " + msg.name + " (Enter resumes it)")
		}
		return m, nil

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
			m.applyParentResume(msg)
		case !msg.opened && m.updating:
			m.errorMsg = "Update in progress - resume again once it finishes"
		case !msg.opened:
			conv := msg.conv
			m.selected, m.fork, m.quitting = &conv, msg.fork, true
			return m, tea.Quit
		}
		return m, nil

	case reorgPlannedMsg:
		m.reorgBusy = ""
		if msg.err != nil {
			m.errorMsg = "reorg: " + msg.err.Error()
		} else if msg.p.moves() == 0 {
			m.note(fmt.Sprintf("reorg: nothing to move (%d suggestions left out)", len(msg.p.dropped)))
		} else {
			m.reorgReady = &msg.p // opens once typing pauses (openReadyReorg)
			m.note("reorg: plan ready; it opens when you stop typing")
		}
		return m, nil

	case reorgDoneMsg:
		m.reorgBusy = ""
		m.launchLoaded = false // the new parents
		m.sortList()
		if msg.err != nil {
			m.errorMsg = "reorg: " + msg.err.Error()
		} else {
			m.note("reorg: " + msg.res.String())
		}
		return m, nil

	case orchestratorOfferMsg:
		if !msg.opened && (m.orchBusy || m.orchOffer != nil || loadOrchestrator().Offered) {
			return m, nil // answered, or ^X O got there, while the check ran
		}
		m.orchBusy = false
		m.orchOffer, m.orchAsked, m.orchSince = &msg.plan, msg.opened, time.Now()
		return m, nil

	case orchestratorDoneMsg:
		m.orchBusy = false
		m.syncOrchestrator()
		if msg.err != nil {
			m.errorMsg = msg.err.Error()
			if msg.text != "" { // what the set-up did before it failed
				m.errorMsg = msg.text + "; " + m.errorMsg
			}
			return m, nil
		}
		m.note(msg.text)
		if slices.ContainsFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == msg.id }) {
			m.selectSession(msg.id)
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

	case t3DoneMsg:
		m.resuming = false
		if msg.err != nil {
			state.LogActivity("couldn't open in T3 Code: %v", msg.err)
			m.note("In T3 Code: open it there, or message it from its box here")
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
		ask := time.Since(m.agentsAt) >= sessions.AgentsInterval
		if ask {
			m.agentsAt = time.Now()
		}
		return m, tea.Batch(m.liveCmd(ask), m.serveCheckCmd(), m.heldCmd())

	case heldMsg:
		m.applyHeld(msg)
		return m, m.heldRetry()

	case heldRetryMsg:
		return m, m.heldCmd()

	case heldDoneMsg:
		m.applyHeldDone(msg)
		switch { // the pane goes once it's settled: say how it went where it stays
		case m.heldErr != "":
			m.errorMsg = m.heldErr
		case m.heldNote != "":
			// Only where it belongs: the session the answer was for, while
			// it's selected; moving the selection clears it (selectionMoved).
			if len(m.filtered) > 0 && m.filtered[m.cursor].conv.SessionID == msg.session {
				m.note(m.heldNote)
				m.heldNoteShown = true
			}
		}
		return m, m.heldCmd()

	case liveMsg:
		m.live = msg.live
		if msg.pids != nil {
			m.livePIDs, m.unverified, m.lastScan, m.hosts = msg.pids, msg.unverified, msg.scan, msg.hosts
			// A held prompt answered in its own tab (Claude Code shows it
			// there too) is dropped from ccs at once.
			if h := ownHold(); h != nil {
				h.Reconcile(msg.scan)
			}
			if msg.bg != nil {
				m.bg = msg.bg
			}
		}
		m.syncOrchestrator()
		m.reloadParents() // a launch or adoption from any session
		// Keep sessions ccs just opened live until claude's own file shows up.
		for id, t := range m.opened {
			if msg.live[id] || time.Since(t) >= sessions.OpenedGrace {
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
		cmds := []tea.Cmd{liveTick(), m.autoFocusCmd(), m.heldCmd()}
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
		return m, checkHooksCmd

	case hooksMsg:
		m.hooksMissing = msg.missing
		return m, nil

	case tea.BlurMsg:
		m.blurred = true
		return m, nil

	case autoFocusMsg:
		m.applyAutoFocus(msg)
		return m, nil

	case refreshMsg:
		m.refreshStarted = time.Time{}
		// No projects folder yet on a first run is an empty list, not a failure.
		m.refreshFailed = msg.err != nil && !(errors.Is(msg.err, fs.ErrNotExist) && len(m.items) == 0)
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
		// Claude Code's settings are read once a minute, with the scheduled scan.
		return m, tea.Batch(append(usageCmds, checkHooksCmd, transcript.RefreshTick())...)

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
		if selfupdate.NewerVersion(msg.tag, state.Version) && m.autoUpdate && m.upgrade != nil && msg.tag != m.autoFailed && !m.updating {
			// No offer: prepared now, installed once you're away (autoUpdateTick).
			first := msg.tag != m.updateTo
			m.updateTo = msg.tag
			m.upgrade.Prepare(msg.tag)
			var away tea.Cmd
			if first {
				away = autoUpdateTick()
			}
			return m, tea.Batch(away, tea.Tick(updateCheckInterval, func(time.Time) tea.Msg { return updateCheckTickMsg{} }))
		}
		if selfupdate.NewerVersion(msg.tag, state.Version) && msg.tag != m.dismissed && !m.updating {
			if msg.tag != m.updateTo {
				m.updateOpen = true
				m.updateShownAt = time.Now()
				m.changelog = nil
				if f := m.fetchChangelog; f != nil {
					tag := msg.tag
					fetch = func() tea.Msg {
						lines, err := f(tag)
						if err != nil {
							state.LogUpdate("changelog %s: %v", tag, err)
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
			state.LogUpdate("changelog: %v", msg.err)
		} else {
			m.notes = msg.lines
		}
		return m, nil

	case changelogMsg:
		if msg.tag == m.updateTo {
			m.changelog = msg.lines
		}
		return m, nil

	case autoUpdateTickMsg:
		if m.restartReady != "" {
			if m.updateBlocker() != "" {
				return m, autoUpdateTick()
			}
			m.restart, m.quitting = m.restartReady, true
			state.LogUpdate("restarting into %s now that nothing stops it", m.updateTo)
			logHandBack()
			return m, tea.Quit
		}
		switch {
		case m.updating || m.updateTo == "" || !m.autoUpdate || m.upgrade == nil || m.updateTo == m.autoFailed:
			return m, nil // nothing to install on its own now
		case m.updateBlocker() != "":
			if !m.updateWaitLogged {
				state.LogActivity("update waits: %s", m.updateBlocker())
				m.updateWaitLogged = true
			}
			return m, autoUpdateTick()
		case !m.awayForUpdate():
			m.updateWaitLogged = false
			return m, autoUpdateTick()
		}
		state.LogUpdate("auto-update: installing %s while you're away", m.updateTo)
		stats.Record("tui.update_auto", stats.Auto)
		m.autoInstalling = true
		return m, m.startInstall()

	case updatedChangelogMsg:
		m.changelog = msg.lines
		return m, nil

	case updatingTickMsg:
		if m.updating {
			return m, updatingTick() // redraw so the step and seconds stay live
		}
		return m, nil

	case upgradeDoneMsg:
		m.updating = false
		if m.progress != nil {
			state.LogUpdate("install %s: %v in %s", m.updateTo, state.ErrOrOK(msg.err), time.Since(m.progress.started).Round(time.Millisecond))
		}
		if msg.err != nil {
			// Keep the offer open with the reason, so the failure can't vanish
			// on the next keypress and Enter retries.
			m.updateErr = msg.err.Error()
			if m.autoInstalling { // offered from now on, not tried again on its own
				m.autoFailed, m.autoInstalling = m.updateTo, false
			}
			m.updateOpen = true
			m.updateShownAt = time.Now()
			return m, nil
		}
		// An automatic update installed while you started answering a
		// question (or ran a command) waits for that; held questions alone
		// don't stop it, since the restart hands them back to their hooks.
		if why := m.updateBlocker(); why != "" && m.autoInstalling {
			m.restartReady = msg.path
			state.LogActivity("update ready: waiting for %s", why)
			return m, autoUpdateTick()
		}
		m.restart = msg.path
		m.quitting = true
		logHandBack()
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
		if msg.conv != nil && msg.gen == m.gen && !m.prompting() {
			m.applyLive([]transcript.Conversation{*msg.conv})
		}
		m.setStat(msg.id, msg.stat) // after the transcript, which can say a busy turn ended
		m.checkDelivered(msg.id)
		return m, m.heldCmd() // quickly while a wait's held copy is due (heldPending)

	case sessions.ReceiptMsg:
		m.applyReceipt(msg)
		return m, waitReceipt

	case clipImageMsg:
		switch {
		case msg.err != nil:
			m.errorMsg = "Couldn't read the clipboard: " + msg.err.Error()
		case msg.path != "":
			m.attachImage(msg.path)
		default:
			return m, textinput.Paste // no image there: the usual text paste
		}
		return m, nil

	case runDoneMsg, runTickMsg:
		return m, m.applyRun(msg)

	case sendDoneMsg:
		m.sending = false
		if !errors.Is(msg.err, sessions.ErrUnsent) {
			m.dropSentImages(msg.id, msg.text)
		}
		if m.sendNote == nil {
			m.sendNote = make(map[string]string)
		}
		m.sendNote[msg.id] = msg.note
		if msg.err != nil {
			delete(m.pending, msg.id)
			if errors.Is(msg.err, sessions.ErrUnsent) && !msg.keepDraft {
				m.sendNote[msg.id] += " · " + m.giveBack(msg.id, msg.text)
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
		prevKey := m.lastKeyAt
		m.lastKeyAt = time.Now()
		if h, _, ok := m.selectedHeld(); ok && m.menuLeft == h.ID && msg.String() == "right" && m.textInput.Value() == "" {
			m.menuLeft = "" // give the held prompt the keys again
			return m, nil
		}
		if m.heldKeysActive() {
			if cmd, done := m.heldPaneKey(msg, prevKey); done {
				return m, cmd
			}
		}
		if msg.Type == tea.KeyRunes && (mouseLeak.MatchString(string(msg.Runes)) || m.mouseFragment(msg)) {
			return m, nil // a fragmented mouse report, not typing
		}
		if m.takesImages() {
			if msg.Type == tea.KeyRunes && (msg.Paste || len(msg.Runes) > 1) && m.pasteImages(string(msg.Runes)) {
				return m, nil
			}
			if msg.Type == tea.KeyCtrlV {
				stats.Record("tui.paste_image", stats.Key)
				return m, clipImageCmd
			}
		}
		// ccs's own actions are ^X then a letter (^X ^A works as ^X A), Claude
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
		if m.snoozeOpen { // owns the keyboard until closed
			return m.snoozeKey(k)
		}
		if m.setup != nil { // owns the keyboard until closed
			return m.serveSetupKey(msg)
		}
		if m.serveQROpen { // owns the keyboard until closed
			return m.serveQRKey(k)
		}
		if k == "^x s" && len(m.filtered) > 0 && m.marks != nil && !m.prompting() && !m.acctOpen && !m.showUsage {
			m.helpOpen = false
			if c := m.filtered[m.cursor].conv; m.putAway(c) != awake {
				stats.Record("tui.wake", stats.Key)
				m.wake(c.SessionID)
			} else {
				stats.Record("tui.snooze", stats.Key)
				m.snoozeOpen, m.snoozeCursor = true, 0
			}
			return m, nil
		}
		if m.reorgPlan != nil { // owns the keyboard until answered
			switch msg.String() {
			case "enter":
				if time.Since(m.reorgShownAt) < updateKeyGrace {
					return m, nil // a key already on its way when the plan appeared
				}
				if !m.reorgAtEnd() {
					m.scrollReorg(m.reorgRoom())
					return m, nil
				}
				stats.Record("tui.reorg_apply", stats.Key)
				p := *m.reorgPlan
				m.reorgPlan, m.reorgBusy = nil, "reorganising"
				return m, reorgApplyCmd(p)
			case "up", "ctrl+p":
				m.scrollReorg(-1)
			case "down", "ctrl+n":
				m.scrollReorg(1)
			case "pgup":
				m.scrollReorg(-m.reorgRoom())
			case "pgdown":
				m.scrollReorg(m.reorgRoom())
			case "esc":
				m.reorgPlan = nil
				m.note("reorg cancelled: nothing moved")
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
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
				stats.Record("tui.reopen", stats.Key)
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
		if m.orchOffer != nil && !m.offerShown() {
			m.orchSince = time.Time{} // it comes on screen when this is done, with a fresh grace
		} else if m.orchOffer != nil { // owns the keyboard until answered
			if m.orchSince.IsZero() {
				m.orchSince = time.Now()
			}
			if msg.String() != "ctrl+c" && time.Since(m.orchSince) < updateKeyGrace {
				return m, nil
			}
			switch msg.String() {
			case "enter":
				stats.Record("tui.orchestrator_setup", stats.Key)
				p := *m.orchOffer
				m.orchOffer, m.orchBusy = nil, true
				markOrchestratorAnswered()
				m.note("Setting up the orchestrator: " + trustNote)
				return m, orchestratorSetupCmd(p)
			case "esc":
				m.orchOffer = nil
				markOrchestratorAnswered()
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
					stats.Record("tui.link_open", stats.Key)
					u := shown[min(m.linksCursor, len(shown)-1)].url
					if id, ok := strings.CutPrefix(u, sessionLink); ok { // a session: select it, done here
						m.linksOpen = false
						m.selectSession(id)
						return m, nil
					}
					return m, openLinkCmd(u)
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
		if k == "^x t" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.table", stats.Key)
			m.helpOpen, m.table = false, !m.table
			saveSettings(m.settings())
			m.chatInput.Width = chatInputWidth(m.paneWidth())
			return m, nil
		}
		if k == "^x l" && !m.table && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.hide_list", stats.Key)
			m.helpOpen, m.hideSidebar = false, !m.hideSidebar
			saveSettings(m.settings())
			m.chatInput.Width = chatInputWidth(m.paneWidth())
			return m, nil
		}
		if k == "^x f" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.details", stats.Key)
			m.helpOpen = false
			m.toggleHeader()
			return m, nil
		}
		if k == "^x y" && !m.prompting() && !m.acctOpen { // your usage, and back: the only key for the usage screen
			return m, m.toggleUsage()
		}
		if k == "^x b" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.serve", stats.Key)
			m.helpOpen = false
			return m, m.serveKey()
		}
		if k == "^x a" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.auto_focus", stats.Key)
			m.helpOpen, m.autoFocus = false, !m.autoFocus
			m.focusHeld = ""
			saveSettings(m.settings())
			return m, nil
		}
		if k == "^x h" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.headless", stats.Key)
			m.helpOpen, showHeadless = false, !showHeadless
			sel := ""
			if len(m.filtered) > 0 {
				sel = m.filtered[m.cursor].conv.SessionID
			}
			m.lastFilterQuery = "" // narrowing from the shown list would miss the runs it hid
			m.updateFilter()
			if i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == sel }); i >= 0 {
				m.cursor = i
			}
			return m, nil
		}
		if k == "^x k" && len(m.filtered) > 0 && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.kill", stats.Key)
			m.helpOpen = false
			if conv := m.filtered[m.cursor].conv; m.stoppable(conv.SessionID) {
				m.confirmStop = conv.SessionID
			} else {
				m.errorMsg = "^X K stops a live session in a terminal tab, or a background one; this one isn't"
			}
			return m, nil
		}
		if k == "^x w" && len(m.filtered) > 0 && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.handover", stats.Key)
			m.helpOpen = false
			conv := m.filtered[m.cursor].conv
			if !m.restartable(conv.SessionID) {
				m.errorMsg = "^X W hands a live Claude Code session in a terminal tab over to its parent; this one isn't"
				return m, nil
			}
			parent, err := handoverParent(m.sessionName(conv))
			if err != nil {
				m.errorMsg = err.Error()
				return m, nil
			}
			m.confirmHandover, m.handoverTo = conv.SessionID, parent
			return m, nil
		}
		if k == "^x r" && len(m.filtered) > 0 && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.restart", stats.Key)
			m.helpOpen = false
			if id := m.filtered[m.cursor].conv.SessionID; m.restartable(id) {
				m.confirmRestart = id
				st := m.chatStatus[id].Status
				m.restartForce = st == "busy" || st == "waiting"
			} else {
				m.errorMsg = "^X R restarts a live Claude Code session in a terminal tab; this one isn't"
			}
			return m, nil
		}
		if k == "^x o" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.orchestrator", stats.Key)
			m.helpOpen = false
			switch {
			case m.orchBusy || m.resuming:
				m.errorMsg = "Still opening the orchestrator..."
			case !sessions.CanOpenTabs():
				m.errorMsg = "^X O opens a tab in iTerm or tmux, and this is neither: run ccs orchestrator instead"
			default:
				m.orchBusy, m.errorMsg = true, ""
				return m, orchestratorKeyCmd
			}
			return m, nil
		}
		if k == "^x n" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.new_session", stats.Key)
			m.helpOpen = false
			if !sessions.CanOpenTabs() {
				m.errorMsg = "^X N opens the new session in an iTerm tab or tmux window, and this is neither"
				return m, nil
			}
			m.launchCwd, _ = os.Getwd()
			if len(m.filtered) > 0 {
				if cwd := m.filtered[m.cursor].conv.Cwd; cwd != "" && cwd != "unknown" {
					m.launchCwd = cwd
				}
			}
			m.launchStep, m.errorMsg = 1, ""
			m.launchInput = sessions.LaunchPrompt("New session name: ", "")
			return m, nil
		}
		if k == "^x q" && !m.prompting() && !m.acctOpen {
			stats.Record("tui.profile", stats.Key)
			m.helpOpen = false
			if m.profileStop != nil {
				m.profileStop()
				m.profileStop = nil
				m.note("CPU profile saved: " + m.profilePath + " (go tool pprof -top it)")
				return m, nil
			}
			path := filepath.Join(filepath.Dir(state.UpdateLogPath), "cpu-"+time.Now().Format("20060102-150405")+".pprof")
			stop, err := startProfile(path)
			if err != nil {
				m.errorMsg = "Couldn't record a profile: " + err.Error()
				return m, nil
			}
			m.profileStop, m.profilePath = stop, path
			m.note("Recording a CPU profile: do what's slow, then ^X Q again")
			return m, nil
		}
		if k == "^x i" && !m.prompting() && !m.acctOpen {
			stats.Record("tui.auto_update", stats.Key)
			m.helpOpen, m.autoUpdate = false, !m.autoUpdate
			saveSettings(m.settings())
			if !m.autoUpdate {
				m.note("Automatic updates off: new versions are offered instead (^X I turns them back on)")
				return m, nil
			}
			m.note("Automatic updates on: a new version installs once you're away")
			if m.updateOpen && m.updateErr == "" { // the offer on screen becomes an automatic install
				m.updateOpen = false
				return m, autoUpdateTick()
			}
			return m, nil
		}
		if k == "^x p" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.pin", stats.Key)
			m.helpOpen = false
			m.togglePin()
			return m, nil
		}
		if k == "^x m" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.reorg", stats.Key)
			m.helpOpen = false
			if m.reorgBusy != "" {
				m.note("a reorg is already " + m.reorgBusy)
				return m, nil
			}
			m.reorgBusy = "planning"
			m.note("Asking Sonnet to group every session by topic; the plan shows before anything moves")
			return m, m.reorgPlanCmd()
		}
		if k == "^x c" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.tree_view", stats.Key)
			m.helpOpen = false
			m.toggleClosed()
			return m, nil
		}
		if (k == "alt+up" || k == "alt+down") && !m.prompting() && !m.acctOpen && !m.showUsage && !m.chatFocus {
			stats.Record("tui.pin_move", stats.Key)
			m.movePin(map[string]int{"alt+up": -1, "alt+down": 1}[k])
			return m, nil
		}
		if k == "^x g" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.group", stats.Key)
			m.helpOpen, m.grouped = false, !m.grouped
			saveSettings(m.settings())
			m.sortList()
			return m, nil
		}
		if msg.String() == "ctrl+o" && !m.prompting() && !m.acctOpen && !m.showUsage {
			stats.Record("tui.links", stats.Key)
			m.helpOpen = false
			if m.links = m.linksWithPRs(); len(m.links) == 0 {
				m.errorMsg = "No links in view"
				return m, nil
			}
			m.linksOpen, m.linksCursor = true, 0
			m.linksInput.SetValue("")
			return m, nil
		}
		if msg.String() == "ctrl+l" && !m.prompting() && !m.acctOpen {
			stats.Record("tui.changelog", stats.Key)
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
		// ? opens help when it can't be meant as text: the box being typed
		// into (the message box when focused, else the search) is empty, as
		// on Claude Code's prompt. After text it's a ?.
		box := m.textInput.Value()
		if m.chatFocus {
			box = m.chatInput.Value()
		}
		if msg.String() == "?" && box == "" && !m.prompting() && !m.showUsage {
			stats.Record("tui.help", stats.Key)
			m.helpOpen = true
			return m, nil
		}
		if k == "^x u" && !m.prompting() {
			stats.Record("tui.account", stats.Key)
			return m.openAccounts()
		}
		// The popup waits behind rename/delete/prune prompts, so it never takes
		// their keys; once they close, it gets a fresh grace period.
		if m.updatedOpen { // "ccs updated itself": any key closes it; Enter and Esc do only that
			m.updatedOpen = false
			if k := msg.String(); k == "enter" || k == "esc" {
				return m, nil
			}
		}
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
				if m.upgrade == nil {
					m.updateOpen = false
					return m, nil
				}
				m.updateOpen = false
				if m.restartReady != "" { // installed already: just restart into it
					m.restart, m.quitting = m.restartReady, true
					return m, tea.Quit
				}
				stats.Record("tui.update_install", stats.Key)
				return m, m.startInstall()
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
		if m.launchStep > 0 {
			switch msg.String() {
			case "enter":
				v := strings.TrimSpace(m.launchInput.Value())
				if m.launchStep == 1 {
					if !state.SessionName.MatchString(v) {
						m.errorMsg = "A session name is letters, digits, '.', '_' and '-'"
						return m, nil
					}
					m.launchName, m.launchStep = v, 2
					m.launchInput = sessions.LaunchPrompt("Brief: ", "")
					m.errorMsg = ""
					return m, nil
				}
				if v == "" {
					m.errorMsg = "Type its first prompt"
					return m, nil
				}
				m.launchStep, m.errorMsg = 0, ""
				m.note("Starting " + m.launchName + "…")
				return m, launchCmd(sessions.LaunchOpts{Name: m.launchName, Brief: v, Cwd: m.launchCwd})
			case "esc", "ctrl+c":
				m.launchStep = 0
				return m, nil
			}
			var cmd tea.Cmd
			m.launchInput, cmd = editKey(m.launchInput, msg)
			return m, cmd
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

		if m.confirmServeStop {
			switch msg.String() {
			case "y", "Y":
				m.confirmServeStop = false
				return m, stopServeCmd()
			case "n", "N", "esc":
				m.confirmServeStop = false
			}
			return m, nil // Ignore all other keys
		}

		// Handle stop confirmation mode
		if id := m.confirmStop; id != "" {
			switch msg.String() {
			case "y", "Y":
				m.confirmStop = ""
				i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id })
				if i < 0 || !m.stoppable(id) { // it ended, or went, while asking
					m.errorMsg = "It isn't running any more"
					return m, nil
				}
				conv := m.filtered[i].conv
				if b, bg := m.bg[id]; bg {
					return m, stopCmd(m.sessionName(conv), b)
				}
				return m, closeCmd(m.sessionName(conv), m.livePIDs[id], id)
			case "n", "N", "esc":
				m.confirmStop = ""
				return m, nil
			}
			return m, nil // Ignore all other keys
		}

		// Handle hand-over confirmation mode
		if id := m.confirmHandover; id != "" {
			switch msg.String() {
			case "y", "Y":
				m.confirmHandover = ""
				i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id })
				if i < 0 || !m.restartable(id) { // it ended, or went, while asking
					m.errorMsg = "It isn't running any more"
					return m, nil
				}
				name := m.sessionName(m.filtered[i].conv)
				m.note(fmt.Sprintf("Asking %s to hand over to %s; it closes once it's done", name, m.handoverTo))
				return m, handoverCmd(name, id)
			case "n", "N", "esc":
				m.confirmHandover = ""
				return m, nil
			}
			return m, nil // Ignore all other keys
		}

		// Handle restart confirmation mode
		if id := m.confirmRestart; id != "" {
			switch msg.String() {
			case "y", "Y":
				m.confirmRestart = ""
				i := slices.IndexFunc(m.filtered, func(it listItem) bool { return it.conv.SessionID == id })
				if i < 0 || !m.restartable(id) {
					m.errorMsg = "It isn't running any more"
					return m, nil
				}
				name := m.sessionName(m.filtered[i].conv)
				m.note("Restarting " + name + "...")
				return m, restartCmd(name, m.restartForce)
			case "n", "N", "esc":
				m.confirmRestart = ""
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
			// Tab is only ever for the sessions that need you, then those
			// with a PR ready to merge, then any other open PR: the next one
			// after the selection, wrapping. On the usage screen it goes back
			// to the list first. With none it says so and does nothing
			// else; ^X Y is the usage screen.
			m.showUsage = false
			if id, tier, ok := m.nextWaiting(); ok {
				if tier >= attentionReady {
					stats.Record("tui.next_pr", stats.Key)
				} else {
					stats.Record("tui.next_waiting", stats.Key)
				}
				m.selectSession(id)
				return m, nil
			}
			m.note("nothing needs you")
			return m, nil
		}
		if m.showUsage && k == "esc" {
			return m, m.toggleUsage() // back to the list
		}
		if m.showUsage && k != "ctrl+c" {
			return m, nil // the usage screen has no other keys; typing mustn't edit a hidden search
		}

		// A finished ! command's box: ^S offers to send its result to the
		// session it ran for, and the choice owns the keys until answered.
		// Only with the message box empty, so a message being written is
		// never touched.
		if m.run != nil && m.run.asking {
			if !m.runShown() || m.run.running {
				m.run.asking = false
			} else {
				return m, m.runKey(k)
			}
		}
		if k == "ctrl+s" && m.runSendable() {
			stats.Record("tui.run_send_ask", stats.Key)
			m.run.asking = true
			return m, nil
		}

		if m.chatFocus && !chord {
			switch msg.String() {
			case "esc":
				if m.stopRun() { // a ! command's box: stop it, or close it
					return m, nil
				}
				m.chatFocus = false // back to the search
				return m, nil
			case "ctrl+c":
				if m.run != nil && m.run.running && m.stopRun() {
					return m, nil // stops the command, not ccs
				}
			case "pgup", "pgdown":
				if m.runShown() {
					m.scrollRun(map[string]int{"pgup": -10, "pgdown": 10}[msg.String()])
					return m, nil
				}
			case "enter":
				if strings.TrimSpace(m.chatInput.Value()) == "" && len(m.filtered) > 0 {
					// Enter on an empty box opens the session itself.
					stats.Record("tui.open", stats.Key)
					m.chatFocus = false
					return m, m.focusCmd(m.filtered[m.cursor].conv)
				}
				if m.selectedLive() {
					if cmd, done := m.commandLine(); done { // ! and / lines: only ever from a key pressed here
						return m, cmd
					}
				}
				stats.Record("tui.send", stats.Key)
				return m, m.sendCmd()
			case "up", "ctrl+p", "down", "ctrl+n", "ctrl+f", "ctrl+r", "ctrl+]", "ctrl+\\":
				// fall through to the list's handling below
			default:
				var cmd tea.Cmd
				m.chatInput, cmd = editKey(m.chatInput, msg)
				return m, cmd
			}
		}

		switch k {
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
				if m.orchBusy { // quitting would kill the set-up halfway
					return m, nil
				}
				m.quitting = true
				return m, tea.Quit
			}
			if m.orchBusy && m.filtered[m.cursor].conv.SessionID == m.orchID { // it's already being opened
				m.errorMsg = "Still opening the orchestrator..."
				return m, nil
			}
			// Two steps on a session with a message box: this Enter puts typing
			// in the box, and Enter on the empty box goes to its tab. One waiting
			// on a menu or prompt has no box, so Enter goes straight there.
			if m.selectedLive() && !m.boxHidden() {
				stats.Record("tui.box", stats.Key)
				m.chatFocus = true
				return m, nil
			}
			if _, _, held := m.selectedHeld(); held { // answer it here (it's in its tab too)
				stats.Record("tui.held_pane", stats.Key)
				m.heldFocus, m.heldAutoAt = true, time.Time{}
				return m, nil
			}
			// A live session's tab is focused, not resumed again (resumeCmd checks).
			stats.Record("tui.open", stats.Key)
			return m, m.resumeCmd(m.filtered[m.cursor].conv, false)

		case "ctrl+f":
			// Fork: resume into a new session id, leaving the original as is.
			if len(m.filtered) == 0 {
				return m, nil
			}
			stats.Record("tui.fork", stats.Key)
			return m, m.resumeCmd(m.filtered[m.cursor].conv, true)

		case "^x d":
			if len(m.filtered) > 0 && m.filtered[m.cursor].conv.Kind == transcript.KindCodex {
				m.note("A Codex session: ccs leaves Codex's files alone (codex delete removes one)")
				return m, nil
			}
			if len(m.filtered) > 0 {
				stats.Record("tui.delete", stats.Key)
				m.confirmDelete = true
				m.deleteIndex = m.cursor
			}
			return m, nil

		case "ctrl+r":
			stats.Record("tui.refresh", stats.Key)
			return m, m.refreshNow()

		case "^x e":
			if len(m.filtered) > 0 {
				conv := m.filtered[m.cursor].conv
				if conv.Kind == transcript.KindCodex {
					m.note("A Codex session: rename it in Codex")
					return m, nil
				}
				// A running claude re-appends its own title and would undo ours.
				if m.isLive(conv.SessionID) {
					m.errorMsg = "Session is open in claude - use /rename there"
					return m, nil
				}
				stats.Record("tui.rename", stats.Key)
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
			if len(m.filtered) > 0 && m.filtered[m.cursor].conv.Kind == transcript.KindCodex {
				m.note("A Codex session: ccs only prunes Claude's transcripts")
				return m, nil
			}
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
				st, err := transcript.PruneFile(m.filtered[m.cursor].conv.FilePath, false, transcript.PruneOpts{DropSnapshots: true, StripToolResults: true})
				if err != nil {
					m.errorMsg = fmt.Sprintf("Prune preview failed: %v", err)
					return m, nil
				}
				stats.Record("tui.prune", stats.Key)
				m.confirmPrune = true
				m.pruneIndex = m.cursor
				m.pruneSaved = st.BytesIn - st.BytesOut
			}
			return m, nil

		case "up", "ctrl+p", "down", "ctrl+n":
			if m.step(map[bool]int{true: -1, false: 1}[k == "up" || k == "ctrl+p"]) {
				m.previewScroll = 0
				m.selectionMoved()
			}
			return m, nil

		case "ctrl+]", "ctrl+\\": // next / previous search hit
			stats.Record("tui.hit_jump", stats.Key)
			m.jumpHit(msg.String() == "ctrl+]")
			return m, nil

		case "pgup":
			m.previewScroll = min(m.previewScroll+10, m.maxPreviewScroll()) // back
			return m, nil

		case "pgdown":
			m.previewScroll = max(0, m.previewScroll-10) // forward
			return m, nil

		}
		if chord { // a ^X chord no action took never edits a text box
			return m, nil
		}
	}

	// Anything else (e.g. a Ctrl+V paste's result) is for the input that has
	// the keyboard: never a box hidden under a popup or form.
	var cmd tea.Cmd
	if _, isKey := msg.(tea.KeyMsg); !isKey {
		if in := m.keyboardBox(); in != nil {
			*in, cmd = editKey(*in, msg)
			return m, cmd
		}
		if m.keyboardOwned() {
			return m, nil // a popup with no text box has the keyboard
		}
		switch {
		case m.renaming:
			m.renameInput, cmd = editKey(m.renameInput, msg)
			return m, cmd
		case m.launchStep > 0:
			m.launchInput, cmd = editKey(m.launchInput, msg)
			return m, cmd
		case m.chatFocus:
			m.chatInput, cmd = editKey(m.chatInput, msg)
			return m, cmd
		}
	}
	prevValue := m.textInput.Value()
	m.textInput, cmd = editKey(m.textInput, msg)
	if m.textInput.Value() != prevValue {
		if prevValue == "" {
			stats.Record("tui.search", stats.Key) // a search begun, never its words
		}
		m.updateFilter()
	}
	return m, cmd
}

// View draws the current screen, then any popup on top. Popups are overlaid
// here, not by each screen, so every screen (and any added later) shows them.
func (m model) View() string {
	screen := m.viewScreen()
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

// overPopups is every popup, bottom first: View draws them in this order,
// and popupOpen checks them.
// popupOpen: a popup is open; it takes the keys and the wheel.
func (m model) popupOpen() bool {
	return slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open })
}

// keyboardBox is the text box inside whatever popup, form or menu has the
// keyboard, for messages that aren't keys (a Ctrl+V paste's result): the
// setup form's focused field, the links filter, or the held menu's Other or
// note box. Nil when none of those has it.
func (m *model) keyboardBox() *textinput.Model {
	switch {
	case m.setup != nil:
		if m.setup.step == setupForm && m.setup.focus >= 0 && m.setup.focus < len(m.setup.fields) {
			return &m.setup.fields[m.setup.focus]
		}
		return nil
	case m.linksOpen:
		return &m.linksInput
	}
	if h, _, ok := m.selectedHeld(); ok && m.heldKeysActive() {
		if mn := m.menus[h.ID]; mn != nil && mn.editing != boxNone {
			return &mn.box
		}
	}
	return nil
}

// keyboardOwned: a popup or form with no text box of its own has the
// keyboard, so nothing typed or pasted may reach a box beneath it.
func (m model) keyboardOwned() bool {
	return slices.ContainsFunc(m.overPopups(), func(p popup) bool { return p.open })
}

func (m model) overPopups() []popup {
	return []popup{
		{m.updateOpen && !m.prompting(), model.updatePopup},
		{m.updatedOpen, model.updatedPopup},
		{m.acctOpen, model.acctPopup},
		{m.helpOpen, model.helpPopup},
		{m.notesOpen, model.notesPopup},
		{m.linksOpen, model.linksPopup},
		{m.snoozeOpen, model.snoozePopup},
		{m.serveQROpen, model.serveQRPopup},
		{m.setup != nil, model.serveSetupPopup},
		{len(m.reopen) > 0, model.reopenPopup},
		{m.reorgPlan != nil, model.reorgPopup},
		{m.offerShown(), model.orchestratorPopup},
	}
}

// keyRule is how ccs decides between a single key and a ^X chord, at the
// top of the ? popup and --help: a single key does something now and leaves
// nothing behind; a chord changes something that stays. AGENTS.md says why.
var keyRule = []string{
	"single keys act now and leave nothing behind; ^X then a letter changes",
	"something that stays (a mark, a setting, a file, whether it runs), and",
	"the ones that can't be undone ask first",
}

// Compact key labels, shared by every hint so they stay consistent.
const (
	keyEnter = "enter" // words, not ⏎ ⇥: those draw wider than a cell in some fonts
	keyTab   = "tab"
	keyEsc   = "esc"
)

// shortcuts is the keys that do something right now on the list, for the
// ? popup (the usage screen's header lists
// their own): whether a conversation is selected and live, and whether the
// message box has focus. An entry with no key is a group's heading; a
// heading with nothing under it is left out.
func (m model) shortcuts() [][2]string {
	var out [][2]string
	add := func(on bool, k, what string) {
		if on {
			out = append(out, [2]string{k, what})
		}
	}
	group := func(title string) {
		if n := len(out); n > 0 && out[n-1][0] == "" {
			out = out[:n-1]
		}
		out = append(out, [2]string{"", title})
	}
	hasCswap := cswapPath() != ""
	sel := len(m.filtered) > 0
	live := sel && m.selectedLive()
	pinned := sel && m.pinIndex(m.filtered[m.cursor].conv.SessionID) >= 0

	group("moving around")
	add(len(m.filtered) > 1, "↑↓ ^P/N", "move through the list")
	add(sel, "pgup/dn", "scroll the conversation (the mouse wheel too)")
	add(sel && m.textInput.Value() != "" && len(m.hitLines()) > 0, "^] ^\\", "jump to the next / previous search hit")
	add(!m.chatFocus && m.textInput.Value() != "", keyEsc, "clear the search and go back to the top")

	group("this session")
	switch {
	case m.chatFocus:
		add(true, keyEnter, "send the message; on an empty box, go to its tab")
		add(true, keyEsc, "leave the message box for the search (a command's output: stop it, or close it)")
		add(true, "^V", "paste a screenshot from the clipboard into the message")
		add(true, "!cmd", "run cmd here, in ccs, in the session's folder; its output shows above the box (PgUp/PgDn scroll)")
		add(m.runSendable(), "^S", "send the finished command's result to the session it ran for: o with its output, e the exit code only")
		add(true, "/cmd", "type a slash command into the session's tab, so Claude Code runs it (only while it's idle)")
		add(true, "!! //", "send a line starting ! or / as an ordinary message")
	case m.heldKeysActive():
		add(true, "↑↓ j k", "a question: move over its options and Other… (no Enter first: the menu has the keys while the search is empty)")
		add(true, "1-9", "a question: highlight that option (a pick-any one: tick it)")
		add(true, keyEnter, "a question: pick the highlighted option and send it (Other… opens a box for your own answer)")
		add(true, "space", "a pick-any question: tick or untick; enter confirms")
		add(true, "n", "a note on this question's answer, sent with it (empty removes it)")
		add(true, "←", "back a question, keeping your answers (several end on a review: enter submits)")
		add(true, "A D", "a permission prompt: allow or deny (asks y/n)")
		add(true, keyEsc, "back to the list, keeping your answers (→ or tab returns to the menu)")
		add(true, "^P ^N", "move the list, menu or not; any other letter types in the search")
	case live && m.boxHidden() && m.heldPrompt[m.filtered[m.cursor].conv.SessionID].ID != "":
		add(true, "→ "+keyEnter, "answer the prompt ccs holds for it, here: its question's menu, or A allow / D deny (y/n); it's in its tab too, and the first answer wins")
	case live && m.boxHidden():
		add(true, keyEnter, "go to its tab to answer it")
	case live:
		add(true, keyEnter, "type in its message box; enter again on the empty box goes to its tab")
		add(true, "^V", "paste a screenshot from the clipboard into a message to it")
	default:
		add(sel, keyEnter, "resume it in claude, in a new tab in tmux or iTerm")
	}
	add(sel, "^F", "fork it: a new session with the same history, in a new tab")
	add(sel && len(m.linksWithPRs()) > 0, "^O", "open a link in view, one of its PRs or artifacts")

	group("^X then a letter: this session")
	add(sel && m.marks != nil && !pinned, "^X P", "pin it to the top of the list")
	add(pinned, "^X P", "unpin it")
	add(pinned && len(m.marks.Pinned) > 1, "Alt+↑↓", "move it among the pinned sessions")
	if sel && m.marks != nil {
		if m.putAway(m.filtered[m.cursor].conv) != awake {
			add(true, "^X S", "bring it back from settled or snoozed")
		} else {
			add(true, "^X S", "put it away: settle until its next message, or snooze it")
		}
	}
	add(sel && !live, "^X E", "edit its name, as /rename does")
	add(sel && m.headerCollapsed, "^X F", "show its details under the summary line (or click the line)")
	add(sel && !m.headerCollapsed, "^X F", "hide its details, leaving the summary line (or click the line)")
	add(sel, "^X X", "shrink its file, keeping the dialogue; asks first, loses /rewind")
	if sel {
		id := m.filtered[m.cursor].conv.SessionID
		if _, bg := m.bg[id]; bg && m.stoppable(id) {
			add(true, "^X K", "stop this background session; asks first")
		} else if m.stoppable(id) {
			add(true, "^X K", "stop this session and close its tab; asks first")
		}
		add(m.restartable(id), "^X R", "restart it: stop, then resume this conversation in a new tab with the same flags; asks first")
		add(m.restartable(id), "^X W", "hand its leftovers to its parent, then close it; its PRs and children move to the parent; asks first")
	}
	add(sel, "^X D", "delete its transcript file; asks first, can't be undone")

	group("^X then a letter: view and tools")
	add(sessions.CanOpenTabs(), "^X N", "start a new session in an iTerm tab or tmux window, in this one's folder")
	add(sessions.CanOpenTabs(), "^X O", "open the orchestrator, setting one up if there is none")
	add(!m.grouped, "^X G", "group the list by folder")
	add(m.grouped, "^X G", "stop grouping by folder")
	add(m.reorgBusy == "", "^X M", "move sessions into topics: Sonnet plans, you accept or drop it")
	if id, name, ok := m.foldOwner(m.cursor); sel && ok {
		add(!m.showingClosed(id), "^X C", "show the closed sessions under "+name)
		add(m.showingClosed(id), "^X C", "hide the closed sessions under "+name)
	}
	add(!m.table, "^X T", "the full table, every column")
	add(m.table, "^X T", "back to the sidebar")
	add(!m.table && !m.hideSidebar, "^X L", "hide the list, the preview taking the whole width (↑↓ still move)")
	add(!m.table && m.hideSidebar, "^X L", "show the list beside the preview again")
	add(!showHeadless, "^X H", "show headless runs (claude -p, codex exec)")
	add(showHeadless, "^X H", "hide headless runs")
	add(!m.autoFocus, "^X A", "switch to a session's tab when it starts waiting on you")
	add(m.autoFocus, "^X A", "stop switching to sessions that wait on you")
	add(!m.serve.Installed, "^X B", "set up ccs serve: try it on this Mac now, or set it up for your phone")
	add(m.serve.Installed && !m.serve.Running, "^X B", "start ccs serve, the sessions in a browser (its launchd agent)")
	add(m.serve.Running, "^X B", "ccs serve's address and QR code; s stops it (asks first), e edits its setup")
	add(!m.autoUpdate, "^X I", "install ccs updates on their own once you're away")
	add(m.autoUpdate, "^X I", "stop installing ccs updates on their own")
	add(m.profileStop == nil, "^X Q", "record a CPU profile, for when ccs feels slow")
	add(m.profileStop != nil, "^X Q", "stop recording and save the CPU profile")
	add(hasCswap, "^X U", "switch Claude account (cswap)")
	add(!m.showUsage, "^X Y", "your usage: 12h of token use, and your allowance")
	add(m.showUsage, "^X Y/"+keyEsc, "back to the list")

	group("ccs")
	add(true, "^R", "refresh sessions, PRs and allowance now; check for an update")
	add(true, keyTab, "needs you (question, asked, unread), then ready PRs, open PRs")
	add(true, "^L", "what changed in each ccs release")
	add(true, "?", "this help, from an empty search or message box (after text it's a ?)")
	add(true, "^C", "quit")
	return out
}

// toggleUsage opens the usage screen (^X Y), or goes back from it (^X Y or
// Esc; Tab goes back too, to the next session that needs you).
func (m *model) toggleUsage() tea.Cmd {
	m.showUsage = !m.showUsage
	if m.showUsage {
		stats.Record("tui.usage_screen", stats.Key)
		return tea.Batch(m.usageCmd(), m.allowanceCmd())
	}
	return nil
}

// headerLine fits the title row into width so it never wraps (listLayout
// counts it as one row): hint pairs go from the end, keeping the last keep
// (help, quit), then the title is cut, then the kept hints go too.
func headerLine(width int, title, toggles string, pairs []string, keep int) string {
	right := func(h string) string {
		if toggles == "" {
			return h
		}
		return toggles + "   " + h
	}
	fits := func(t, h string) bool { return 2+ansi.StringWidth(t)+1+ansi.StringWidth(h) <= width }
	drop := len(pairs)/2 - keep // pairs that may go
	for !fits(title, right(hints(pairs...))) && drop > 0 {
		i := 2 * (drop - 1)
		pairs = append(pairs[:i:i], pairs[i+2:]...)
		drop--
	}
	help := hints(pairs...)
	if fits(title, right(help)) { // the toggles go before the title is cut
		help = right(help)
	}
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

// toggleHint is a mode's key, name and state for the header: grey, with
// only "on" in green, so the modes that are on stand out.
// chordPairs is the hint row while ^X waits for its second key: each letter
// and what it does, never a mode's on/off (that shows when no chord is
// pending). Every "^x <letter>" Update handles is here, in order, except ^X H,
// which is left unadvertised (headless runs stay out of the header).
var chordPairs = []string{"A", "auto-focus", "B", "browser (ccs serve)", "C", "closed", "D", "delete", "E", "edit name", "F", "details", "G", "group", "I", "auto-update", "K", "stop", "L", "list", "M", "move into topics", "N", "new", "O", "orchestrator", "P", "pin", "Q", "profile", "R", "restart", "S", "snooze", "T", "table", "U", "account", "W", "hand over", "X", "prune", "Y", "usage", keyEsc, "cancel"}

func toggleHint(key, label string, on bool) string {
	state := "off"
	if on {
		state = "\033[32mon\033[90m"
	}
	return "\033[1m" + key + "\033[22m " + label + " " + state
}

// hints renders key/label pairs as "enter resume  ^F fork", keys in bold so
// "enter update" reads as key then action.
func hints(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, "\033[1m"+pairs[i]+"\033[22m "+pairs[i+1])
	}
	return strings.Join(parts, "  ")
}

// helpPopup lists the shortcuts that apply now; ? or Esc closes it.
// hitGlyph marks the count of messages matching the search (HITS).
const hitGlyph = "⌕"

// legendLines is the ? help's key to every count's glyph, two lines so the
// popup stays short: the title row's tasks, subagents and hits, then the PR
// counts (prCell), in the order a row shows them. Every count the list draws
// has a glyph, so none is a bare number.
func legendLines() string {
	item := func(glyph, colour, what string) string {
		return "\033[" + colour + "m" + glyph + "\033[0m " + what
	}
	pr := func(group int, what string) string {
		glyph, colour := prMark(group)
		return item(glyph, colour, what)
	}
	return "\033[90mcounts:\033[0m  " + strings.Join([]string{
		item("⧗", transcript.TaskColour, "commands and monitors running"),
		item("◆", transcript.SubagentColour, "subagents running"),
		item(hitGlyph, "36", "search hits"),
	}, "  ") + "\n\033[90mPRs:\033[0m     " + strings.Join([]string{
		pr(github.PRReady, "ready"),
		pr(github.PRFix, "needs you"),
		pr(github.PRWaiting, "waiting"),
		pr(prDraft, "draft"),
		pr(github.PRMerged, "merged"),
	}, "  ")
}

func (m model) helpPopup() string {
	var b strings.Builder
	b.WriteString("\033[1mshortcuts\033[0m\n")
	for _, l := range keyRule {
		b.WriteString("\033[90m" + l + "\033[0m\n")
	}
	b.WriteString("\n")
	for i, s := range m.shortcuts() {
		if s[0] == "" { // a group's heading
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("\033[90m" + s[1] + "\033[0m\n")
			continue
		}
		fmt.Fprintf(&b, "\033[33m%-8s\033[0m %s\n", s[0], s[1])
	}
	if !m.showUsage {
		b.WriteString("\n" + legendLines() + "\n")
	}
	if len(m.hooksMissing) > 0 {
		fmt.Fprintf(&b, "\n\033[33mhooks out of date:\033[0m %s %s missing from Claude Code's settings; run \033[1mccs hook install\033[0m (ccs hook status lists them)\n", strings.Join(m.hooksMissing, ", "), map[bool]string{true: "is", false: "are"}[len(m.hooksMissing) == 1])
	}
	b.WriteString("\n\033[90m" + hints("?/"+keyEsc, "close") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(0, 3).
		Render(b.String())
}

// overlayCentre draws box over the middle of screen, keeping what's visible
// either side of it. Widths are measured without ANSI codes.
func overlayCentre(screen, box string, width, height int) string {
	if width <= 0 || height <= 0 {
		// Before the first WindowSizeMsg there is no screen to draw over.
		return screen
	}
	lines := strings.Split(screen, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	boxLines := strings.Split(box, "\n")
	boxW := 0
	for _, l := range boxLines {
		boxW = max(boxW, ansi.StringWidth(l))
	}
	// Never over the title row, whose notes (the waiting count, an update) stay
	// readable while a popup is open; a box taller than the rest is cut at the bottom.
	top := max((height-len(boxLines))/2, min(1, height-1))
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
	if transcript.WorkerPanicked.Load() {
		note += " · internal error, logged to " + transcript.WorkerPanicLog
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
	} else if why := m.updateBlocker(); why != "" && m.updateTo != "" && (m.restartReady != "" || (m.autoUpdate && m.upgrade != nil && m.updateTo != m.autoFailed)) {
		status = " · update waiting: " + why
	}
	// Each state with its key in front, in bold, as the hints read.
	key := func(k string) string { return "\033[1m" + k + "\033[22m " }
	if n := m.waitingCount(); n > 0 && !m.showUsage { // they sort first in the list, held ones selected for you
		status += fmt.Sprintf(" · %d waiting", n)
	}
	if m.reorgBusy != "" {
		status += " · reorg " + m.reorgBusy + "…"
	}
	if m.profileStop != nil { // a reminder: it records until stopped
		status += " · " + key("^X Q") + "\033[31mrecording profile\033[33m"
	}
	// The modes ^X toggles, on the right beside the key hints. Not while a
	// chord is pending: then the row lists only what the next key does.
	var toggles []string
	if !m.showUsage && !m.chordX {
		toggles = append(toggles, toggleHint("^X A", "auto-focus", m.autoFocus))
		if m.serve.Installed { // set up for remote use: say whether it's reachable
			toggles = append(toggles, toggleHint("^X B", "browser", m.serve.Running))
		}
		if !m.table && m.hideSidebar { // the way back; a shown list speaks for itself
			toggles = append(toggles, toggleHint("^X L", "list", !m.hideSidebar))
		}
		if m.checkLatest != nil { // a release build: one that updates
			toggles = append(toggles, toggleHint("^X I", "auto-update", m.autoUpdate))
		}
		if n := m.hooksNote(); n != "" {
			toggles = append(toggles, n)
		}
	}
	title := fmt.Sprintf("\033[1;36mccs\033[0m \033[90m· coding cli sessions · %s%s\033[0m\033[33m%s\033[0m", state.Version, note, status)
	pairs, keep := []string{"^X Y", "usage", "?", "help", "^C", "quit"}, 2 // the rest are in ? help
	if h := m.tabHint(); h != "" {                                         // Tab is only for the sessions that need you, then PRs
		pairs = append([]string{keyTab, h}, pairs...)
	}
	if m.showUsage { // only these keys do anything there
		pairs, keep = []string{keyEsc, "back", "^X U", "account", "^L", "changelog"}, 1 // every key it has, so no ? help
		pairs = append(pairs, "^C", "quit")
	} else if m.chordX { // ^X pressed: what the next key does
		pairs, keep = chordPairs, 1
	} else if m.heldKeysActive() {
		pairs, keep = []string{keyEsc, "back to the list", "keys:", "menu", "?", "help"}, 2 // Esc's hint goes first when it's narrow
	} else if m.chatFocus {
		send := "send"
		if strings.TrimSpace(m.chatInput.Value()) == "" {
			send = "its tab"
		}
		pairs, keep = []string{keyEnter, send, keyEsc, "search", "pgup/dn", "scroll", "?", "help"}, 1 // ? opens help on an empty box, as on the search
	}
	b.WriteString(headerLine(tableWidth, title, strings.Join(toggles, "  "), pairs, keep) + "\n")

	// Search line or delete confirmation
	var sections []string
	var inputSection string
	if m.launchStep > 0 {
		sections = append(sections, "  "+m.launchInput.View()+"  \033[90min "+sessions.TildePath(m.launchCwd)+"  "+hints(keyEnter, map[bool]string{true: "next", false: "start it"}[m.launchStep == 1], keyEsc, "cancel")+"\033[0m")
	} else if m.renaming {
		sections = append(sections, "  "+m.renameInput.View()+"  \033[90m"+hints(keyEnter, "save", keyEsc, "cancel")+"\033[0m")
	} else if m.confirmPrune {
		conv := m.filtered[m.pruneIndex].conv
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")). // Amber
			Render(fmt.Sprintf("Prune \"%s\"? %s -> %s, saves %s (keeps dialogue).",
				transcript.Truncate(transcript.GetTopic(conv), 32), transcript.FormatBytes(conv.Size), transcript.FormatBytes(conv.Size-m.pruneSaved), transcript.FormatBytes(m.pruneSaved))) +
			"  \033[90m" + hints("y", "prune", "n/"+keyEsc, "cancel") + "\033[0m"
		sections = append(sections, "  "+inputSection)
	} else if m.confirmServeStop {
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // Red
			Render("Stop ccs serve? Remote access ends until ^X B starts it again.") +
			"  \033[90m" + hints("y", "stop", "n/"+keyEsc, "cancel") + "\033[0m"
		sections = append(sections, "  "+inputSection)
	} else if i := slices.IndexFunc(m.filtered, func(it listItem) bool { return m.confirmStop != "" && it.conv.SessionID == m.confirmStop }); i >= 0 {
		conv := m.filtered[i].conv
		what := "Stop %q and close its tab?"
		if _, bg := m.bg[conv.SessionID]; bg {
			what = "Stop background session %q?"
		}
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // Red
			Render(fmt.Sprintf(what, transcript.Truncate(m.sessionName(conv), 50))) +
			"  \033[90m" + hints("y", "stop", "n/"+keyEsc, "cancel") + "\033[0m"
		sections = append(sections, "  "+inputSection)
	} else if i := slices.IndexFunc(m.filtered, func(it listItem) bool { return m.confirmHandover != "" && it.conv.SessionID == m.confirmHandover }); i >= 0 {
		name := transcript.Truncate(m.sessionName(m.filtered[i].conv), 40)
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")). // Amber
			Render(fmt.Sprintf("Hand %q over to %s and close it? It sends %s its leftovers first.", name, transcript.Truncate(m.handoverTo, 40), transcript.Truncate(m.handoverTo, 40))) +
			"  \033[90m" + hints("y", "hand over", "n/"+keyEsc, "cancel") + "\033[0m"
		sections = append(sections, "  "+inputSection)
	} else if i := slices.IndexFunc(m.filtered, func(it listItem) bool { return m.confirmRestart != "" && it.conv.SessionID == m.confirmRestart }); i >= 0 {
		name := transcript.Truncate(m.sessionName(m.filtered[i].conv), 50)
		what, colour := fmt.Sprintf("Restart %q? It stops, then resumes this conversation in a new tab.", name), "214" // Amber
		if m.restartForce {
			what, colour = fmt.Sprintf("%q is mid-turn: restart it anyway, stopping it where it is?", name), "196" // Red
		}
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colour)).
			Render(what) +
			"  \033[90m" + hints("y", "restart", "n/"+keyEsc, "cancel") + "\033[0m"
		sections = append(sections, "  "+inputSection)
	} else if m.confirmDelete {
		conv := m.filtered[m.deleteIndex].conv
		topic := transcript.GetTopic(conv)
		liveWarning := ""
		if m.live[conv.SessionID] {
			liveWarning = " It is open in claude."
		}
		inputSection = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")). // Red
			Render(fmt.Sprintf("Delete conversation \"%s\"?%s", transcript.Truncate(topic, 50), liveWarning)) +
			"  \033[90m" + hints("y", "delete", "n/"+keyEsc, "cancel") + "\033[0m"
		sections = append(sections, "  "+inputSection)
	} else if m.showUsage {
		sections = append(sections, "  \033[1;36mUsage\033[0m \033[90m· last 12h · "+keyEsc+" back\033[0m")
	} else {
		prefix, usage, count := m.searchRowParts()
		inputSection = prefix + usage + "\033[90m" + count + "\033[0m"
		sections = append(sections, inputSection)
	}

	// Show error message if set
	if m.errorMsg != "" {
		errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
		if m.errorMsg == m.noteMsg {
			errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
		}
		msg := strings.Join(strings.Fields(m.errorMsg), " ") // one row: listLayout counts it as one
		sections = append(sections, "  "+errorStyle.Render(ansi.Truncate(msg, max(m.width-3, 1), "…")))
	}

	b.WriteString(strings.Join(sections, "\n") + "\n")
	if sw := m.sidebarWidth(); sw > 0 && !m.showUsage { // a rule over the list and preview, joining the divider
		b.WriteString("\033[90m" + strings.Repeat("─", sw) + "┬" + strings.Repeat("─", max(m.width-sw-1, 0)) + "\033[0m")
	} else if !m.table && !m.showUsage { // ^X L hid the list: the rule over the preview alone
		b.WriteString("\033[90m" + strings.Repeat("─", m.width) + "\033[0m")
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
	colPRs     = 14 // "✓1 ✗1 ◷1 ○1 ●3"
	colHits    = 5  // "⌕123"
	colSize    = 6
	colGap     = 2 // spaces between columns
	listIndent = 2 // leading "  " on each row
	numGaps    = 7
	colMarks   = 4 // status icons (● !) packed right, the agent's glyph (✻ ⬡), then a space
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
	project := transcript.Truncate(projectName(item.conv.Cwd), colProject)

	marks, colouredMarks := m.listMarks(item)
	mw := colMarks // the marks' width, and with ^X G an indent under the folder's header
	if m.grouped {
		marks, colouredMarks, mw = "  "+marks, "  "+colouredMarks, colMarks+2
	}
	tw := m.topicColWidth()
	nest := m.treePrefix(item)
	title := listTopic(item.conv, tw-mw-ansi.StringWidth(nest))
	after := m.taskCell(item.conv, false)
	if after != "" { // running background tasks, after the title
		title = listTopic(item.conv, tw-mw-ansi.StringWidth(nest)-ansi.StringWidth(after)-1)
		after = " " + after
	}
	topic := nest + title + after

	// transcript.Message count
	msgs := len(item.conv.Messages)

	// Number of messages containing the query (memoised per query).
	hits := ""
	if m.searching() {
		hits = fmt.Sprintf("  %*s", colHits, hitGlyph+fmt.Sprint(m.hitCount(item)))
	}

	size := transcript.FormatBytes(item.conv.Size)

	when := transcript.FormatAgo(item.conv.LastTimestamp, time.Now())

	ctx := transcript.FormatTokens(item.conv.ContextTokens)
	model := transcript.Truncate(transcript.ShortModel(item.conv), colModel)

	// Format: when | project | topic | model | size | ctx | msgs | hits | prs (aligned columns)
	if selected {
		return fmt.Sprintf("%-*s  %-*s  %s%-*s  %-*s  %*s  %*s  %*d%s",
			colWhen, when, colProject, project, marks, tw-mw, topic, colModel, model, colSize, size, colCtx, ctx, colMsgs, msgs, hits+"  "+m.prCell(item.conv, false))
	}
	// Pad before colouring so the escape codes don't eat into the column width.
	topic = colouredMarks + padRight(nest+paintTitle(title, m.titleColour(item))+after, tw-mw)
	// CTX is coloured by how full the context is (see ctxColour).
	ctxCode, flash := transcript.CtxColour(item.conv.ContextTokens, item.conv.WindowSize())
	if flash && time.Now().Unix()%2 == 1 {
		ctxCode = "2;31" // flashing: dim every other second
	}
	return fmt.Sprintf("\033[90m%-*s\033[0m  \033[1;33m%-*s\033[0m  %s  \033[37m%-*s\033[0m  \033[35m%*s\033[0m  \033["+ctxCode+"m%*s\033[0m  %*d\033[36m%s\033[0m  %s",
		colWhen, when, colProject, project, topic, colModel, model, colSize, size, colCtx, ctx, colMsgs, msgs, hits, m.prCell(item.conv, true))
}

// nestPrefix marks a session started by the one above it: "└ ", indented
// two more per level below the first.
func nestPrefix(depth int) string {
	if depth == 0 {
		return ""
	}
	return strings.Repeat("  ", depth-1) + "└ "
}

// listTopic is the session's title cut to w.
func listTopic(conv transcript.Conversation, w int) string {
	return transcript.Truncate(transcript.GetTopic(conv), w)
}

// listMarks is a row's colMarks-wide status column, plain and coloured.
func (m model) listMarks(item listItem) (string, string) {
	conv := item.conv
	// Status icons, packed right against the topic so titles stay aligned:
	// ● open in a running claude (○ in an app ccs can't open; coloured by its
	// state, magenta when it waits on you), ! its last
	// action errored. A session another one started sits └ under it instead
	// of a mark: entrypoint sdk-cli alone can't tell a script's claude -p from
	// a session typed in through an app.
	// ponytail: the glyphs are ambiguous-width, so a marked row may sit one
	// cell off on CJK-width terminals - cosmetic only.
	var icons, colouredIcons string
	n := 0
	// Green when idle, amber while busy, cyan when done since you looked,
	// magenta when a menu or prompt waits on you, light magenta when idle and
	// its last message asks something.
	liveGlyph, liveColour := "●", "32"
	if h := m.hostedApp(conv.SessionID); h != "" && h != sessions.T3App { // open in an app ccs can't switch to
		liveGlyph = "○"
	}
	switch m.waitSignal(conv) { // the circle stays (● or ○ says whether ccs can open it); its colour says why
	case waitPrompt:
		liveColour = waitColour
	case waitAsked:
		liveColour = askedColour
	default:
		if m.chatStatus[conv.SessionID].Status == "busy" {
			liveColour = amber
		} else if m.unseen(conv) && conv.SessionID != m.viewing { // being read now: no longer unread
			liveColour = doneColour
		}
	}
	// The title's colour says the state (titleColour), so the circle shows
	// only what the colour can't: a session in an app ccs can't open (○).
	dot := m.live[conv.SessionID] && liveGlyph == "○"
	for _, s := range []struct {
		on           bool
		glyph, color string
	}{{dot, liveGlyph, liveColour}, {conv.LastError != "", "!", "1;31"}} {
		if !s.on {
			continue
		}
		n++
		icons += s.glyph
		colouredIcons += "\033[" + s.color + "m" + s.glyph + "\033[0m"
	}
	glyph, colour := transcript.AgentGlyph(conv) // every row: which agent it is
	pad := strings.Repeat(" ", colMarks-2-n)
	return pad + icons + glyph + " ", pad + colouredIcons + "\033[" + colour + "m" + glyph + "\033[0m "
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
	if sw == 0 { // ^X L hid the list: the preview alone
		for len(pane) < h { // the full height, as with the list
			pane = append(pane, "")
		}
		pane = pane[:h]
		for r := range pane {
			pane[r] = ansi.Truncate(pane[r], pw, "…")
		}
		return strings.Join(pane, "\n")
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
	fold       string // a "N closed" line: its parent's session id, which a click opens or closes
	foldName   string
}

// listRows lays the list out in screen rows: a header before each state
// section (after a blank row, except the first; none over a lone session
// named as the section, namesSection), and with ^X G before each folder within it (sortList keeps a
// section's and a group's sessions together).
func (m model) listRows() []listRow {
	counts := map[group]int{}
	sections := map[int]int{}
	for i := range m.filtered {
		counts[groupOf(&m.filtered[i])]++
		if s := sectionOf(m.filtered[i].familyState); s != orchestratorSec || m.filtered[i].depth == 0 {
			sections[s]++ // the orchestrator section counts its trees
		}
	}
	rows := make([]listRow, 0, len(m.filtered)*m.rowsPerItem()+2*len(sections)+len(counts))
	for i := range m.filtered {
		it := &m.filtered[i]
		if s := sectionOf(it.familyState); i == 0 || s != sectionOf(m.filtered[i-1].familyState) {
			sec := pinnedSection
			switch {
			case s == orchestratorSec:
				sec = orchestratorSection
				if sections[s] > 1 {
					sec.name += "s"
				}
			case s >= 0:
				sec = stateSections[min(s, len(stateSections)-1)]
			}
			if i > 0 { // a blank row parts it from the section above
				rows = append(rows, listRow{item: -1})
			}
			if !namesSection(sec.name, sections[s], it) {
				rows = append(rows, listRow{item: -1, header: fmt.Sprintf("  \033[%sm%s\033[0m \033[90m%d\033[0m", sec.colour, sec.name, sections[s])})
			}
		}
		if k := groupOf(it); m.grouped && it.familyState >= 0 && (i == 0 || k != groupOf(&m.filtered[i-1])) {
			rows = append(rows, listRow{item: -1, header: fmt.Sprintf("    \033[1;37m%s\033[0m \033[90m%d\033[0m", projectName(groupKey(it.familyCwd)), counts[k])})
		}
		for p := range m.rowsPerItem() {
			rows = append(rows, listRow{item: i, part: p})
		}
		for _, l := range it.linesAfter {
			rows = append(rows, listRow{item: -1, header: m.closedLineText(l), fold: l.id, foldName: l.name})
		}
	}
	return rows
}

// namesSection: a section of one session titled as the section itself (the
// orchestrator, named orchestrator) needs no header; the row says it already.
func namesSection(name string, n int, it *listItem) bool {
	return n == 1 && transcript.GetTopic(it.conv) == name
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
		hits = hitGlyph + fmt.Sprint(m.hitCount(item))
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
	// In an orchestrator's tree the connector goes before the marks, so the
	// agent's glyph nests with the name: the marks' padding, then │ ├ └, then
	// any ○ or !, the glyph and the title. The grey row carries the │ down.
	conn, greyConn := treeLines(item)
	if conn != "" {
		pad := strings.Repeat(" ", colMarks-2)
		marks = pad + conn + strings.TrimLeft(marks, " ")
		colouredMarks = pad + "\033[90m" + conn + "\033[0m" + strings.TrimLeft(colouredMarks, " ")
		greyConn = pad + greyConn
	}
	mw := ansi.StringWidth(marks)
	tw := w - len(lead) - mw - ansi.StringWidth(hits) - 1
	nest := m.treePrefix(item)
	title := listTopic(conv, tw-ansi.StringWidth(nest))
	topic := padRight(nest+title, tw)
	colouredTopic := padRight(nest+paintTitle(title, m.titleColour(item)), tw)
	prs := strings.TrimLeft(m.prCell(conv, !selected), " ")
	stats := strings.TrimSuffix(transcript.FormatAgo(conv.LastTimestamp, time.Now()), " ago")
	if item.cooling { // it goes behind the closed line once closedCooldown has passed
		stats = "closed " + stats
	}
	if conv.Kind != "" {
		stats = conv.Kind + " · " + stats
	}
	if conv.ContextTokens > 0 {
		stats += " · " + transcript.FormatTokens(conv.ContextTokens) + " ctx"
	}
	if b, ok := m.bg[conv.SessionID]; ok && m.live[conv.SessionID] {
		stats += " · bg"
		if b.State != "" {
			stats += " " + b.State
		}
	} else if h := m.hostedApp(conv.SessionID); h != "" && m.live[conv.SessionID] {
		stats += " · in " + h
	}
	if !m.headedBy(item) {
		stats += " · " + projectName(conv.Cwd) // last: it's the one cut short
	}
	// The grey row starts under the title: after the tree's lines, the
	// marks' width and any nesting prefix.
	gap := strings.Repeat(" ", mw-ansi.StringWidth(greyConn)+ansi.StringWidth(nest))
	indent, colouredIndent := lead+greyConn+gap, lead+"\033[90m"+greyConn+"\033[0m"+gap
	if inTree(item) && item.kids > 0 { // a │ under its glyph joins it to its children's connectors
		before := strings.Repeat(" ", mw-2-ansi.StringWidth(greyConn))
		after := strings.Repeat(" ", 1+ansi.StringWidth(nest))
		indent = lead + greyConn + before + "│" + after
		colouredIndent = lead + "\033[90m" + greyConn + "\033[0m" + before + "\033[90m│\033[0m" + after
	}
	sw := w - ansi.StringWidth(indent) - ansi.StringWidth(prs) - 1
	stats = padRight(transcript.Truncate(stats, sw), sw)
	if selected { // the highlight marks it, no >
		return [2]string{
			selectedStyle.Render(padRight(lead+marks+topic+" "+hits, w)),
			selectedStyle.Render(padRight(indent+stats+" "+prs, w)),
		}
	}
	return [2]string{
		lead + colouredMarks + colouredTopic + " " + colouredHits,
		colouredIndent + "\033[90m" + stats + "\033[0m " + prs,
	}
}

// headedBy: the header above item already names its folder, so its grey row
// needn't: with ^X G, a session in its folder's group (a worktree's own name
// still differs, and stays), and the orchestrator under its own section, whose
// folder is named after it.
func (m model) headedBy(item listItem) bool {
	name := projectName(item.conv.Cwd)
	if sectionOf(item.familyState) == orchestratorSec {
		return name == orchestratorSection.name
	}
	return m.grouped && item.familyState >= 0 && name == projectName(groupKey(item.familyCwd))
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
	transcript.ParseCacheMu.Lock()
	delete(transcript.ParseCache, conv.FilePath)
	transcript.ParseCacheMu.Unlock()
	c := conv
	c.Title, c.IsCustomTitle = name, true
	c.SearchText = "" // rebuilt by buildItems, exactly as a full parse would
	renamed := buildItems([]transcript.Conversation{c})[0]
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
	transcript.ParseCacheMu.Lock()
	delete(transcript.ParseCache, conv.FilePath)
	transcript.ParseCacheMu.Unlock()

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

	_, err := transcript.PruneFile(conv.FilePath, true, transcript.PruneOpts{DropSnapshots: true, StripToolResults: true})
	if err != nil {
		m.errorMsg = fmt.Sprintf("Prune failed: %v", err)
		return
	}
	m.gen++

	// Its parse state (read position) no longer matches the file. Rather than
	// re-read it here on the UI goroutine, mark it so the next read is a full
	// one (off the UI goroutine), and so the next scan's copy wins.
	transcript.ParseCacheMu.Lock()
	delete(transcript.ParseCache, conv.FilePath)
	transcript.ParseCacheMu.Unlock()
	newSize := conv.Size
	if info, e := os.Stat(conv.FilePath); e == nil {
		newSize = info.Size()
	}
	for _, items := range [][]listItem{m.items, m.filtered} {
		for i := range items {
			if items[i].conv.SessionID == conv.SessionID {
				items[i].conv.Size = newSize
				items[i].conv.TailApplied = true
				items[i].conv.ReadAt = time.Time{}
			}
		}
	}
	m.errorMsg = ""
}

type settings struct {
	Table     bool `json:"table,omitempty"`
	Grouped   bool `json:"grouped,omitempty"`
	AutoFocus bool `json:"auto_focus,omitempty"`
	// HideSidebar (^X L): the sidebar layout shows the preview alone.
	HideSidebar bool `json:"hide_sidebar,omitempty"`
	// OpenClosed (^X C): the parents, by session id, whose closed sessions
	// are shown; every other fold is closed. An older ccs's tree_view, one
	// view for every tree, isn't read.
	OpenClosed []string `json:"open_closed,omitempty"`
	// HeaderOpen (^X F): the preview's full details, one line unless chosen.
	HeaderOpen bool `json:"header_open,omitempty"`
	// AutoUpdate (^X I) installs new versions once you're away instead of
	// offering them. Off unless chosen; always written once known.
	AutoUpdate *bool `json:"auto_update,omitempty"`
	// ManualUpdate is the old setting, read only to carry a choice forward.
	ManualUpdate bool `json:"manual_update,omitempty"`
	found        bool // the file existed
}

// autoUpdateOn is the remembered ^X I choice. An install from before updates
// were opt-in keeps what it had (on unless manual_update), so upgrading
// changes nobody's behaviour; a new install starts off.
func (s settings) autoUpdateOn() bool {
	if s.AutoUpdate != nil {
		return *s.AutoUpdate
	}
	return s.found && !s.ManualUpdate
}

func (m model) settings() settings {
	auto := m.autoUpdate
	return settings{Table: m.table, Grouped: m.grouped, AutoFocus: m.autoFocus, HideSidebar: m.hideSidebar, OpenClosed: m.openFolds(), HeaderOpen: !m.headerCollapsed, AutoUpdate: &auto}
}

// loadSettings reads them; a missing or unreadable file means the defaults.
func loadSettings() (s settings) {
	if b, err := os.ReadFile(state.SettingsPath); err == nil {
		json.Unmarshal(b, &s)
		s.found = true // hide_orchestrator, from before ^X C went, is ignored
	}
	return s
}

// foreignSettings are the keys of settings.json something other than the
// list writes (feature_stats, from ccs stats --off): a save carries them
// over. Every other key is the list's, so one it no longer has (hide_*) goes.
var foreignSettings = []string{"feature_stats"}

// mergeSettings is s as JSON, with the file's foreign keys kept.
func mergeSettings(s settings) []byte {
	own, _ := json.Marshal(s)
	old := map[string]json.RawMessage{}
	if b, err := os.ReadFile(state.SettingsPath); err != nil || json.Unmarshal(b, &old) != nil {
		return own
	}
	out := map[string]json.RawMessage{}
	json.Unmarshal(own, &out)
	for _, k := range foreignSettings {
		if v, ok := old[k]; ok {
			out[k] = v
		}
	}
	data, _ := json.Marshal(out)
	return data
}

// saveSettings writes them atomically. A failure only means the next start
// opens in the default view, so it isn't reported.
func saveSettings(s settings) {
	data := mergeSettings(s)
	if os.MkdirAll(filepath.Dir(state.SettingsPath), 0o700) != nil {
		return
	}
	tmp := state.SettingsPath + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, state.SettingsPath)
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

// note shows msg in the error row, plainly: something to know, not a failure.
func (m *model) note(msg string) {
	m.errorMsg, m.noteMsg = msg, msg
}

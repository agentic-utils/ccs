package main

import (
	"encoding/json"
	"fmt"
	"html"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	teammateMsg  = regexp.MustCompile(`(?s)<teammate-message([^>]*)>\n(.*?)\n?</teammate-message>`)
	teammateAttr = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

type teammateBlock struct{ from, note, body string }

// teammateBlocks splits a message from teammate agents (optionally after
// "Another Claude session sent a message:") into one block per sender. A
// JSON report's result becomes the body; a status with nothing to read
// (idle, shutdown…) has an empty body and a short note instead.
func teammateBlocks(text string) []teammateBlock {
	if !strings.Contains(text, "<teammate-message") {
		return nil
	}
	var out []teammateBlock
	for _, m := range teammateMsg.FindAllStringSubmatch(text, -1) {
		b := teammateBlock{from: "a teammate", body: strings.TrimSpace(m[2])}
		for _, a := range teammateAttr.FindAllStringSubmatch(m[1], -1) {
			switch a[1] {
			case "teammate_id":
				b.from = html.UnescapeString(a[2])
			case "summary":
				b.note = html.UnescapeString(a[2])
			}
		}
		var status map[string]any
		if strings.HasPrefix(b.body, "{") && json.Unmarshal([]byte(b.body), &status) == nil {
			str := func(k string) string { v, _ := status[k].(string); return v }
			kind := strings.ReplaceAll(str("type"), "_", " ")
			b.body = str("result")
			if b.body == "" {
				b.body = str("message")
			}
			note := kind
			for _, k := range []string{"summary", "subject", "reason", "idleReason", "failureReason"} {
				if v := str(k); v != "" && v != b.body {
					note += " · " + v
					break
				}
			}
			b.note = note
		}
		out = append(out, b)
	}
	return out
}

// previewCache memoises buildPreviewLines for the selected conversation so the
// preview isn't rebuilt (scanning every message) on every frame. It lives behind
// a pointer so it survives the value-receiver copies of model that View makes.
type previewCache struct {
	key   string
	lines []string
}

// hitCounter memoises HITS (messages containing the query) per conversation for
// the current query, so formatListItem doesn't rescan every visible row's
// messages on every frame. Pointer-held so it survives model value copies.
type hitCounter struct {
	query string
	byID  map[string]int
}

// countHits is the number of a conversation's messages containing query.
func countHits(conv Conversation, query string) int {
	terms := strings.Fields(strings.ToLower(query))
	n := 0
	for _, msg := range conv.Messages {
		if len(terms) > 0 && containsAll(strings.ToLower(msg.Text), terms) {
			n++
		}
	}
	return n
}

// hitCount returns the memoised hit count for item under the current query.
func (m model) hitCount(item listItem) int {
	query := m.textInput.Value()
	if query == "" {
		return 0
	}
	if m.hits == nil { // model built without initialModel (e.g. tests)
		return countHits(item.conv, query)
	}
	if m.hits.query != query {
		m.hits.query = query
		m.hits.byID = make(map[string]int)
	}
	id := item.conv.SessionID
	if h, ok := m.hits.byID[id]; ok {
		return h
	}
	h := countHits(item.conv, query)
	m.hits.byID[id] = h
	return h
}

// previewLines returns the preview lines for the selected conversation,
// rebuilding only when the selection or query changes. Keyed by SessionID (not
// cursor index) so it stays correct when the filtered list shifts.
func (m model) previewLines() []string {
	if len(m.filtered) == 0 {
		return nil
	}
	conv := m.filtered[m.cursor].conv
	query := m.textInput.Value()
	if m.preview == nil { // model built without initialModel (e.g. tests)
		return buildPreviewLines(conv, query, m.width)
	}
	// Preview lines depend only on the messages (so tool-only appends reuse
	// them) and on the width they're wrapped to.
	key := fmt.Sprintf("%s\x00%d\x00%d\x00%s", conv.SessionID, len(conv.Messages), m.width, query)
	if m.preview.key != key {
		m.preview.key = key
		m.preview.lines = buildPreviewLines(conv, query, m.width)
	}
	return m.preview.lines
}

// buildPreviewLines builds the scrollable message lines of a conversation
// preview (everything below the fixed header). Shared by renderPreview and
// maxPreviewScroll so the render and the scroll-clamp can never disagree on how
// far the preview can scroll.
func buildPreviewLines(conv Conversation, query string, width int) []string {
	var msgLines []string

	// Find messages containing the query
	terms := strings.Fields(strings.ToLower(query))
	matchSet := make(map[int]bool)
	if query != "" {
		for i, msg := range conv.Messages {
			if len(terms) > 0 && containsAll(strings.ToLower(msg.Text), terms) {
				matchSet[i] = true
			}
		}
	}

	// Build set of indices to show. Without a search, the whole conversation
	// (scrolling reads it end to end); with one, the matches in context.
	showSet := make(map[int]bool)
	if query == "" {
		for i := range conv.Messages {
			showSet[i] = true
		}
	}

	// Always show first 2 and last 2 messages
	for i := 0; i < 2 && i < len(conv.Messages); i++ {
		showSet[i] = true
	}
	for i := len(conv.Messages) - 2; i < len(conv.Messages); i++ {
		if i >= 0 {
			showSet[i] = true
		}
	}

	// Add matches with context
	for idx := range matchSet {
		if idx > 0 {
			showSet[idx-1] = true
		}
		showSet[idx] = true
		if idx < len(conv.Messages)-1 {
			showSet[idx+1] = true
		}
	}

	// Display messages with gaps. Consecutive messages from one speaker share
	// a header (lastRole/lastDay); each message starts with its time.
	lastShown := -1
	lastRole, lastDay := "", ""
	for i := 0; i < len(conv.Messages); i++ {
		if !showSet[i] {
			continue
		}
		if lastShown >= 0 && i > lastShown+1 {
			lastRole = ""
		}

		if lastShown >= 0 && i > lastShown+1 {
			skipped := i - lastShown - 1
			msgLines = append(msgLines, fmt.Sprintf("\033[90m  ... %d messages ...\033[0m", skipped))
			msgLines = append(msgLines, "")
		} else if lastShown == -1 && i > 0 {
			msgLines = append(msgLines, fmt.Sprintf("\033[90m  ... %d earlier messages\033[0m", i))
			msgLines = append(msgLines, "")
		}

		msg := conv.Messages[i]
		msg.Text = cleanText(msg.Text)
		ts := formatTimestamp(msg.Ts)
		day, clock, _ := strings.Cut(ts, " ")
		newDay := day != "" && day != lastDay
		if newDay { // the date gets its own row, once per day
			msgLines = append(msgLines, dateRow(day), "")
			lastDay = day
		}
		if blocks := teammateBlocks(msg.Text); blocks != nil && msg.Role == "user" {
			// Messages from teammate agents: each under its own "From" header,
			// JSON status reports unpacked; bare status pings are one dim line.
			marker := " "
			if matchSet[i] {
				marker = "▶"
			}
			for _, b := range blocks {
				if b.body == "" {
					line := truncate("▸ "+b.from+" · "+b.note, max(width-2-gutterExtra, 20))
					msgLines = append(msgLines, "\033[90m"+marker+" "+fmt.Sprintf("%-5s", clock)+" "+highlight(line, query)+"\033[0m", "")
					continue
				}
				head := fmt.Sprintf("\033[36m%s From %s\033[0m", marker, b.from)
				if b.note != "" {
					head += " \033[90m· " + highlight(b.note, query) + "\033[0m"
				}
				msgLines = append(msgLines, head)
				msgLines = append(msgLines, timeGutter(renderBody(b.body, query, max(width-gutterExtra, 0)), clock)...)
				msgLines = append(msgLines, "")
			}
			lastShown, lastRole = i, ""
			continue
		}
		if from, body, ok := peerParts(msg.Text); ok && msg.Role == "user" {
			// A message sent from another session or ccs, not typed here.
			marker := " "
			if matchSet[i] {
				marker = "▶"
			}
			msgLines = append(msgLines, fmt.Sprintf("\033[36m%s From %s\033[0m", marker, from))
			msgLines = append(msgLines, timeGutter(renderBody(body, query, max(width-gutterExtra, 0)), clock)...)
			msgLines = append(msgLines, "")
			lastShown, lastRole = i, ""
			continue
		}
		if tag, summary, ok := harnessNote(msg.Text); ok && msg.Role == "user" {
			// Injected by the harness (task notifications, reminders, command
			// output): one dim line, so the real conversation stays readable.
			marker := " "
			if matchSet[i] {
				marker = "▶"
			}
			line := "▸ " + tag
			if summary != "" {
				line += " · " + summary
			}
			if width > 0 {
				line = truncate(line, width-2-gutterExtra) // truncate squeezes spaces, so add the gutter after
			}
			if lastRole == "harness" && lastShown == i-1 {
				msgLines = msgLines[:len(msgLines)-1] // consecutive notes stack without blank lines
			}
			msgLines = append(msgLines, "\033[90m"+marker+" "+fmt.Sprintf("%-5s", clock)+" "+highlight(line, query)+"\033[0m", "")
			lastShown, lastRole = i, "harness"
			continue
		}
		if msg.Role != lastRole || newDay || matchSet[i] {
			name, colour, marker := "Claude", "34", " "
			if msg.Role == "user" {
				name, colour = "User", "32"
			}
			if matchSet[i] {
				colour, marker = "1;"+colour, "▶"
			}
			msgLines = append(msgLines, fmt.Sprintf("\033[%sm%s %s\033[0m", colour, marker, name))
		}
		lastRole = msg.Role
		msgLines = append(msgLines, timeGutter(renderBody(msg.Text, query, max(width-gutterExtra, 0)), clock)...)
		msgLines = append(msgLines, "")

		lastShown = i
	}

	if lastShown < len(conv.Messages)-1 {
		remaining := len(conv.Messages) - lastShown - 1
		msgLines = append(msgLines, fmt.Sprintf("\033[90m  ... %d more messages\033[0m", remaining))
	}

	return msgLines
}

// carryStyles makes each wrapped line self-contained: styles and a link still
// open at a line's end are closed there and reopened on the next line, so an
// underline or link never runs on into the rest of the screen.
func carryStyles(lines []string) []string {
	var sgr []string // SGR codes since the last reset
	link := ""       // open OSC 8 target
	for i, l := range lines {
		prefix := strings.Join(sgr, "")
		if link != "" {
			prefix += "\033]8;;" + link + "\033\\"
		}
		for _, seq := range ansiSeq.FindAllString(l, -1) {
			switch {
			case strings.HasPrefix(seq, "\x1b]8;"):
				link = strings.TrimSuffix(seq[strings.Index(seq[4:], ";")+5:], "\x1b\\")
			case seq == "\x1b[0m" || seq == "\x1b[m":
				sgr = sgr[:0]
			default:
				sgr = append(sgr, seq)
			}
		}
		suffix := ""
		if link != "" {
			suffix = "\033]8;;\033\\"
		}
		if len(sgr) > 0 {
			suffix += "\033[0m"
		}
		lines[i] = prefix + l + suffix
	}
	return lines
}

// hitLinesIn returns the preview line indexes of search matches: the lines
// buildPreviewLines marks with ▶ (headers and one-line notes, never body text,
// which is always indented).
func hitLinesIn(lines []string) []int {
	var hits []int
	for i, l := range lines {
		if strings.HasPrefix(ansiSeq.ReplaceAllString(l, ""), "▶") {
			hits = append(hits, i)
		}
	}
	return hits
}

func (m model) hitLines() []int { return hitLinesIn(m.previewLines()) }

// jumpHit scrolls the preview so the next (or previous) search hit is at the
// top, wrapping round at either end.
func (m *model) jumpHit(next bool) {
	if len(m.filtered) == 0 || m.textInput.Value() == "" {
		return
	}
	lines := m.previewLines()
	hits := hitLinesIn(lines)
	if len(hits) == 0 {
		return
	}
	rows := m.previewMessageRows(m.filtered[m.cursor].conv)
	back := min(m.previewScroll, max(0, len(lines)-rows))
	top := max(0, len(lines)-rows-back)
	scrollTo := func(line int) int { return min(max(len(lines)-rows-line, 0), m.maxPreviewScroll()) }
	// Hits in jump order from the top line, wrapping; take the first that
	// actually moves the view (near the end several hits share one position).
	var order []int
	if next {
		i := sort.SearchInts(hits, top+1)
		order = append(append(order, hits[i:]...), hits[:i]...)
	} else {
		i := sort.SearchInts(hits, top)
		for j := i - 1; j >= 0; j-- {
			order = append(order, hits[j])
		}
		for j := len(hits) - 1; j >= i; j-- {
			order = append(order, hits[j])
		}
	}
	for _, h := range order {
		if s := scrollTo(h); s != back {
			m.previewScroll = s
			return
		}
	}
}

// previewRenderHeight is the height View gives renderPreview.
func (m model) previewRenderHeight() int {
	_, listHeight, _ := m.listLayout()
	return m.height - listHeight - 6 - m.chatRows() // 6 for title + search + blank + header + borders
}

type linkErrMsg struct{ err error }

func openLinkCmd(u string) tea.Cmd {
	return func() tea.Msg {
		if err := openURL(u); err != nil {
			return linkErrMsg{err}
		}
		return nil
	}
}

// linkItem is one link in the Ctrl+T popup: where it goes and the text shown for it.
type linkItem struct{ url, text string }

// visibleLinks lists the links in the preview as drawn now, top to bottom,
// each once, with the text it's shown as. A link hyperlink() split word by
// word, or wrapped across lines, is joined back into one.
func (m model) visibleLinks() []linkItem {
	if len(m.filtered) == 0 {
		return nil
	}
	var out []linkItem
	seen := map[string]bool{}
	cur, gap, last := "", "", "" // open link, plain text since last closed, last closed link
	var text strings.Builder
	finish := func() {
		if last != "" && !seen[last] {
			seen[last] = true
			out = append(out, linkItem{last, strings.TrimSpace(text.String())})
		}
		last, gap = "", ""
		text.Reset()
	}
	for _, line := range strings.Split(m.renderPreview(m.filtered[m.cursor], m.previewRenderHeight()), "\n") {
		if cur == "" {
			gap += " "
		}
		pos := 0
		plain := func(t string) {
			if cur != "" {
				text.WriteString(t)
			} else if last != "" {
				gap += t
			}
		}
		for _, loc := range ansiSeq.FindAllStringIndex(line, -1) {
			plain(line[pos:loc[0]])
			if seq := line[loc[0]:loc[1]]; strings.HasPrefix(seq, "\x1b]8;") {
				url := strings.TrimSuffix(seq[strings.Index(seq[4:], ";")+5:], "\x1b\\")
				switch {
				case url == "" && cur != "":
					last, cur, gap = cur, "", ""
				case url != "" && url == last && strings.TrimSpace(gap) == "":
					text.WriteString(" ") // the next word of the same link
					cur, last = url, ""
				case url != "":
					finish()
					cur = url
				}
			}
			pos = loc[1]
		}
		plain(line[pos:])
	}
	if cur != "" {
		last = cur
	}
	finish()
	return out
}

// linksPopup lists visibleLinks, numbered, with the cursor on one.
func (m model) linksPopup() string {
	width := min(max(m.width-12, 30), 90)
	var b strings.Builder
	b.WriteString("\033[1mlinks in view\033[0m\n\n")
	for i, l := range m.links {
		num := "  "
		if i < 9 {
			num = fmt.Sprintf("%d ", i+1)
		}
		addr := strings.TrimPrefix(strings.TrimPrefix(l.url, "https://"), "http://")
		row := num + l.text
		if l.text != addr {
			row += " \033[90m" + addr + "\033[39m"
		}
		row = ansi.Truncate(row, width-2, "…")
		if i == m.linksCursor {
			row = "\033[1m› " + row + "\033[0m"
		} else {
			row = "  " + row
		}
		b.WriteString(row + "\n")
	}
	b.WriteString("\n\033[90m" + hints("↑↓", "move", keyEnter, "open", "1-9", "open", "^T/"+keyEsc, "close") + "\033[0m")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("39")).
		Padding(1, 3).
		Width(width + 6).
		Render(b.String())
}

// openURL opens a clicked link in the browser. Only web addresses: the text
// comes from transcripts.
var openURL = func(u string) error {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("not a web address: %s", truncate(u, 40))
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.Command(opener, u).Start()
}

// linkAt returns the OSC 8 link under screen column x of a rendered line, or "".
func linkAt(line string, x int) string {
	col, url := 0, ""
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			if loc := ansiSeq.FindStringIndex(line[i:]); loc != nil && loc[0] == 0 {
				seq := line[i : i+loc[1]]
				if strings.HasPrefix(seq, "\x1b]8;") {
					url = strings.TrimSuffix(seq[strings.Index(seq[4:], ";")+5:], "\x1b\\")
				}
				i += loc[1]
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		w := ansi.StringWidth(string(r))
		if x >= col && x < col+w {
			return url
		}
		col += w
		i += size
	}
	return ""
}

// cleanText drops terminal control codes that transcripts can carry (coloured
// command output, carriage returns): printed raw they'd move the cursor or
// leave stray "[" on screen.
func cleanText(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\t' || r == 0x7f }) {
		return s
	}
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// dateRow separates days in the preview; renderPreview pins the current one.
const dateRowPrefix = "\033[90m  ── "

func dateRow(day string) string { return dateRowPrefix + day + " ──\033[0m" }

// gutterExtra is how much further right message text sits to leave room for
// the time before its first line.
const gutterExtra = 6

// timeGutter puts clock ("21:45") before a message's first line and shifts the
// rest to line up after it.
func timeGutter(lines []string, clock string) []string {
	pad := strings.Repeat(" ", gutterExtra)
	for i, l := range lines {
		if clock != "" && strings.HasPrefix(l, "  ") {
			lines[i] = "  \033[90m" + fmt.Sprintf("%-5s", clock) + "\033[0m " + l[2:]
			clock = ""
		} else if strings.TrimSpace(l) == "" {
			lines[i] = ""
		} else {
			lines[i] = pad + l
		}
	}
	return lines
}

// maxPreviewScroll is the furthest the preview of the current selection can
// scroll - one line short of the rendered message-line count.
// It stops when the last line reaches the bottom of the preview, not the top,
// so scrolling never runs into empty space.
func (m model) maxPreviewScroll() int {
	if len(m.filtered) == 0 {
		return 0
	}
	return max(0, len(m.previewLines())-m.previewMessageRows(m.filtered[m.cursor].conv))
}

// previewMessageRows is how many message lines fit below the preview's fixed
// header, matching what View gives renderPreview.
func (m model) previewMessageRows(conv Conversation) int {
	_, _, previewTop := m.listLayout()
	return max(m.height-previewTop-m.chatRows()-len(previewHeader(conv, m.textInput.Value())), 1)
}

// previewHeader is the preview's fixed header (always visible above the
// scrolling messages).
func previewHeader(conv Conversation, query string) []string {
	header := []string{"\033[1;33mProject:\033[0m " + highlight(conv.Cwd, query)}
	if conv.Title != "" {
		header = append(header, "\033[1;33mName:\033[0m    "+highlight(conv.Title, query))
	}
	header = append(header, "\033[1;33mSession:\033[0m "+highlight(conv.SessionID, query))
	header = append(header, sessionStats(conv)...)
	return append(header, "")
}

func (m model) renderPreview(item listItem, height int) string {
	query := m.textInput.Value()
	conv := item.conv

	header := previewHeader(conv, query) // fixed, always visible

	msgLines := m.previewLines() // memoised; item is always the selected conversation

	// Apply scroll to messages only (header stays fixed). Clamp locally for this
	// render; the persisted m.previewScroll is bounded in Update via
	// maxPreviewScroll (this method has a value receiver, so a write here would
	// be discarded).
	msgHeight := height - len(header)
	if msgHeight < 1 {
		msgHeight = 1
	}
	// previewScroll counts back from the newest message, so 0 shows the end.
	back := min(m.previewScroll, max(0, len(msgLines)-msgHeight))
	start := max(0, len(msgLines)-msgHeight-back)
	visibleMsgLines := msgLines[start:min(start+msgHeight, len(msgLines))]

	// The header's last (blank) line pins the date of the top visible message,
	// unless that message's own date row is already at the top.
	if len(visibleMsgLines) > 0 && !strings.HasPrefix(visibleMsgLines[0], dateRowPrefix) {
		for i := start; i >= 0; i-- {
			if strings.HasPrefix(msgLines[i], dateRowPrefix) {
				header[len(header)-1] = msgLines[i]
				break
			}
		}
	}

	// While searching, where the view is among this conversation's hits.
	if hits := hitLinesIn(msgLines); query != "" && len(hits) > 0 {
		i := sort.SearchInts(hits, start) + 1 // first hit at or below the top
		header[len(header)-1] += fmt.Sprintf("  \033[90mhit %d/%d · ^] next ^\\ prev\033[0m", min(i, len(hits)), len(hits))
	}

	// Combine header + scrolled messages
	allLines := append(header, visibleMsgLines...)
	return strings.Join(allLines, "\n")
}

// sessionStats is the preview header's model, token and error lines.
func sessionStats(conv Conversation) []string {
	var lines []string
	if conv.ActiveModel != "" || conv.ContextTokens > 0 {
		window := contextWindow(conv.ActiveModel, conv.PeakContext)
		code, _ := ctxColour(conv.ContextTokens, window)
		model := conv.ActiveModel
		if model == "" {
			model = "unknown"
		}
		if conv.Model != "" && conv.Model != conv.ActiveModel { // switched with /model since the last reply
			model += " \033[90m(switched; last reply " + conv.Model + ")\033[0m"
		}
		lines = append(lines, fmt.Sprintf("\033[1;33mModel:\033[0m   %s · context \033[%sm%s\033[0m of %s (%d%%)",
			model, code, formatTokens(conv.ContextTokens), formatTokens(window), conv.ContextTokens*100/window))
	}
	if u := conv.Usage; u != (tokenUsage{}) {
		eff := u.effective()
		tok := func(n int64) string { // formatTokens leaves 0 blank, for the list
			if n <= 0 {
				return "0"
			}
			return formatTokens(int(n))
		}
		lines = append(lines, fmt.Sprintf("\033[1;33mTokens:\033[0m  in %s · out %s · cache read %s · cache write %s · effective %s (~$%.2f at API prices)",
			tok(u.Input), tok(u.Output), tok(u.CacheRead), tok(u.Cache5m+u.Cache1h), tok(int64(eff)), eff*pricePerMTok/1e6))
	}
	if conv.LastError != "" {
		lines = append(lines, fmt.Sprintf("\033[1;33mError:\033[0m   \033[1;31m%s\033[0m %s", conv.LastError, formatAgo(conv.LastErrorTs, time.Now())))
	}
	return lines
}

var (
	harnessTag  = regexp.MustCompile(`^<([a-z][a-z0-9-]*)[ >]`)
	harnessSumm = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	anyTag      = regexp.MustCompile(`</?[a-z][a-z0-9-]*[^>]*>`)
)

// harnessNote recognises a user-role message that the harness injected
// (starts with a tag like <task-notification> or <system-reminder>) and
// returns its tag and a one-line summary: its <summary> if it has one, else
// its text with the tags removed.
func harnessNote(text string) (tag, summary string, ok bool) {
	m := harnessTag.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	if sm := harnessSumm.FindStringSubmatch(text); sm != nil {
		summary = sm[1]
	} else {
		summary = anyTag.ReplaceAllString(text, " ")
	}
	return m[1], strings.Join(strings.Fields(summary), " "), true
}

// maxCodeLines is the longest fenced code block (a diff, a log) shown inline;
// longer ones collapse to a one-line summary unless the search matches inside.
const maxCodeLines = 8

func containsFold(s, query string) bool {
	terms := strings.Fields(strings.ToLower(query))
	return len(terms) > 0 && containsAll(strings.ToLower(s), terms)
}

// maxMessageRunes caps one message in the preview; beyond it the rest is
// summarised, so a pasted log can't swamp the conversation.
const maxMessageRunes = 20000

var (
	mdBold   = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	mdCode   = regexp.MustCompile("`([^`\n]+)`")
	mdBullet = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+`)
	mdHead   = regexp.MustCompile(`^#{1,6}\s+`)
	ansiSeq  = regexp.MustCompile(`\x1b\[[0-9;]*m|\x1b\]8;[^\x1b]*\x1b\\`)
	// a markdown link, or a bare address (trailing punctuation left out)
	anyLink = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)|https?://[^\s<>()\[\]"'\x60]*[^\s<>()\[\]"'\x60.,;:!?]`)
)

// renderBody lays out one message for the preview: common markdown rendered
// (bold, inline code, headings, bullets, fenced code), the search query
// highlighted, and every line wrapped to width with a hanging indent so
// nothing runs off the screen. width 0 means don't wrap.
func renderBody(text, query string, width int) []string {
	if r := []rune(text); len(r) > maxMessageRunes {
		text = string(r[:maxMessageRunes]) + fmt.Sprintf("\n… (%d more characters)", len(r)-maxMessageRunes)
	}
	const indent = "  "
	var out []string
	inCode := false
	lines := strings.Split(text, "\n")
	for li := 0; li < len(lines); li++ {
		line := lines[li]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			if inCode { // opening fence: collapse a long block (diffs, logs) to one line
				lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				end := li + 1
				for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "```") {
					end++
				}
				body := lines[li+1 : end]
				if len(body) > maxCodeLines && !containsFold(strings.Join(body, "\n"), query) {
					if lang == "" {
						lang = "code"
					}
					out = append(out, indent+fmt.Sprintf("\033[90m▸ %s · %d lines\033[0m", lang, len(body)))
					li, inCode = end, false // skip to the closing fence
				}
			}
			continue // the fence itself carries no content
		}
		if !inCode && isTableRow(trimmed) && li+1 < len(lines) && mdTableSep.MatchString(strings.TrimSpace(lines[li+1])) {
			end := li + 2
			for end < len(lines) && isTableRow(strings.TrimSpace(lines[end])) {
				end++
			}
			out = append(out, renderTable(lines[li:end], query, width-len(indent), indent)...)
			li = end - 1
			continue
		}
		var styled, hang string
		switch {
		case inCode:
			styled = "\033[36m" + highlightStyled(line, query) + "\033[39m"
			hang = "  "
		case mdHead.MatchString(trimmed):
			styled = "\033[1;4m" + highlightStyled(inlineMarkdown(mdHead.ReplaceAllString(trimmed, "")), query) + "\033[0m"
		default:
			if m := mdBullet.FindStringSubmatch(line); m != nil {
				hang = strings.Repeat(" ", len(m[0])) // continuation lines line up with the text
			}
			styled = highlightStyled(inlineMarkdown(line), query)
		}
		if width <= len(indent)+len(hang)+10 {
			out = append(out, indent+styled)
			continue
		}
		wrapped := carryStyles(strings.Split(ansi.Wrap(styled, width-len(indent)-len(hang)-1, ""), "\n"))
		for j, w := range wrapped {
			if j == 0 {
				out = append(out, indent+w)
			} else {
				out = append(out, indent+hang+strings.TrimLeft(w, " "))
			}
		}
	}
	return out
}

var mdTableSep = regexp.MustCompile(`^\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?$`)

func isTableRow(s string) bool { return strings.HasPrefix(s, "|") && strings.Count(s, "|") >= 2 }

func tableCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimSuffix(strings.TrimPrefix(row, "|"), "|")
	cells := strings.Split(row, "|")
	for i, c := range cells {
		cells[i] = strings.TrimSpace(c)
	}
	return cells
}

// renderTable draws a markdown table (header, separator, rows) as aligned
// columns. When it's wider than width, the widest columns are cut with "…".
func renderTable(rows []string, query string, width int, indent string) []string {
	var cells [][]string
	for i, r := range rows {
		if i != 1 { // skip the |---| separator
			cells = append(cells, tableCells(r))
		}
	}
	cols := 0
	for _, r := range cells {
		cols = max(cols, len(r))
	}
	widths := make([]int, cols)
	for ri, r := range cells {
		for c, v := range r {
			v = inlineMarkdown(v)
			cells[ri][c] = v
			widths[c] = max(widths[c], ansi.StringWidth(v))
		}
	}
	// ponytail: shrinks the widest column one cell at a time; fine for chat-sized tables.
	if avail := width - 3*(cols-1); width > 0 {
		for sum(widths) > avail {
			w := slices.Index(widths, slices.Max(widths))
			if widths[w] <= 3 {
				break
			}
			widths[w]--
		}
	}
	var out []string
	for ri, r := range cells {
		parts := make([]string, cols)
		for c := range cols {
			v := ""
			if c < len(r) {
				v = ansi.Truncate(r[c], widths[c], "…")
			}
			v = highlightStyled(v, query) + strings.Repeat(" ", widths[c]-ansi.StringWidth(v))
			if ri == 0 {
				v = "\033[1m" + v + "\033[22m"
			}
			parts[c] = v
		}
		out = append(out, indent+strings.Join(parts, " \033[90m│\033[39m "))
		if ri == 0 {
			seps := make([]string, cols)
			for c := range cols {
				seps[c] = strings.Repeat("─", widths[c])
			}
			out = append(out, indent+"\033[90m"+strings.Join(seps, "─┼─")+"\033[39m")
		}
	}
	return out
}

func sum(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

// inlineMarkdown renders [links](url), **bold** and `code` spans.
func inlineMarkdown(s string) string {
	// [text](url) -> underlined text, then the address in grey without
	// https://; bare addresses shortened the same way. All clickable.
	s = anyLink.ReplaceAllStringFunc(s, func(link string) string {
		m := anyLink.FindStringSubmatch(link)
		text, target := m[1], m[2]
		if target == "" { // a bare address
			text, target = link, link
		}
		url := strings.TrimPrefix(strings.TrimPrefix(target, "https://"), "http://")
		if text == target || text == url {
			return "\033[4m" + hyperlink(target, url) + "\033[24m"
		}
		return "\033[4m" + hyperlink(target, text) + "\033[24m \033[90m(" + hyperlink(target, url) + ")\033[39m"
	})
	s = mdCode.ReplaceAllString(s, "\033[36m$1\033[39m")
	return mdBold.ReplaceAllString(s, "\033[1m$1\033[22m")
}

// hyperlink makes text a terminal link to url (OSC 8: iTerm2, kitty, WezTerm,
// Ghostty…; others just show the text). Each word is linked on its own so a
// link wrapped across lines never leaves one open.
func hyperlink(url, text string) string {
	words := strings.Split(text, " ")
	for i, w := range words {
		if w != "" {
			words[i] = "\033]8;;" + url + "\033\\" + w + "\033]8;;\033\\"
		}
	}
	return strings.Join(words, " ")
}

// highlightStyled highlights query in text that already carries ANSI
// styling, matching only within the plain runs between escape codes.
func highlightStyled(s, query string) string {
	if query == "" {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range ansiSeq.FindAllStringIndex(s, -1) {
		b.WriteString(highlight(s[last:loc[0]], query))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(highlight(s[last:], query))
	return b.String()
}

func highlight(text, query string) string {
	if query == "" {
		return text
	}
	tr := []rune(text)
	lr := []rune(strings.ToLower(text))
	// Each word of the query is highlighted wherever it appears, longest first.
	terms := strings.Fields(strings.ToLower(query))
	sort.Slice(terms, func(a, b int) bool { return len([]rune(terms[a])) > len([]rune(terms[b])) })

	// Match on runes so multibyte text (CJK, emoji) is never sliced mid-rune.
	// ponytail: a handful of runes change length when lowercased (İ, Kelvin K),
	// which breaks the lr/tr index alignment - bail to plain text rather than
	// emit corrupted bytes. Highlighting those is not worth the complexity.
	if len(lr) != len(tr) || len(terms) == 0 {
		return text
	}

	var result strings.Builder
	for i := 0; i < len(tr); {
		n := 0
		for _, t := range terms {
			if q := []rune(t); i+len(q) <= len(tr) && string(lr[i:i+len(q)]) == t &&
				(i == 0 || !(unicode.IsLetter(lr[i-1]) || unicode.IsDigit(lr[i-1]))) { // word starts only, as the search matches
				n = len(q)
				break
			}
		}
		if n > 0 {
			// Yellow background, black text for highlight
			result.WriteString("\033[43;30m")
			result.WriteString(string(tr[i : i+n]))
			result.WriteString("\033[49;39m")
			i += n
		} else {
			result.WriteRune(tr[i])
			i++
		}
	}
	return result.String()
}

func padRight(s string, length int) string {
	r := []rune(s)
	if len(r) >= length {
		return string(r[:length])
	}
	// ponytail: pads by rune count, not display width; CJK/emoji rows can still
	// look a cell narrow. Swap in go-runewidth if column alignment matters.
	return s + strings.Repeat(" ", length-len(r))
}

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"os/exec"
	"path/filepath"
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
	key    string
	lines  []string
	stamps []previewStamp
}

// previewStamp says the preview lines from line on happened at when ("2026-09-29 21:45").
// renderPreview shows the top visible line's in the header.
type previewStamp struct {
	line int
	when string
}

// stampAt is when preview line happened, "" if nothing before it says.
func (c *previewCache) stampAt(line int) string {
	if c == nil {
		return ""
	}
	i := sort.Search(len(c.stamps), func(i int) bool { return c.stamps[i].line > line })
	if i == 0 {
		return ""
	}
	return c.stamps[i-1].when
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
	if len(m.filtered) == 0 || !m.previewSettled() {
		return nil // still scrolling through the list: the header alone
	}
	conv := m.filtered[m.cursor].conv
	conv.peerNotes = m.peerLabels(conv)                                         // the other ends, which other sessions' transcripts show
	return cachedPreview(m.preview, conv, m.textInput.Value(), m.paneWidth()-1) // the last column is the scrollbar
}

// previewDebounce is how long the selection must rest before its
// conversation is built: scrolling through the list draws only each
// preview's header, so a big conversation passed on the way costs nothing.
var previewDebounce = 150 * time.Millisecond

// previewSettledMsg redraws once the selection has rested.
type previewSettledMsg struct{}

func (m model) previewSettled() bool { return time.Since(m.selectedAt) >= previewDebounce }

// noteSelection starts the rest period when the selected session changes.
func (m *model) noteSelection() tea.Cmd {
	id := ""
	if len(m.filtered) > 0 {
		id = m.filtered[min(m.cursor, len(m.filtered)-1)].conv.SessionID
	}
	if id == m.selectedID {
		return nil
	}
	first := m.selectedID == ""
	m.selectedID = id
	if first || previewDebounce == 0 {
		return nil // the first selection isn't scrolling past anything
	}
	m.selectedAt = time.Now()
	return tea.Tick(previewDebounce, func(time.Time) tea.Msg { return previewSettledMsg{} })
}

// cachedPreview is buildPreviewLines memoised in c; a nil c (a model built
// without initialModel, e.g. tests) isn't memoised.
func cachedPreview(c *previewCache, conv Conversation, query string, width int) []string {
	if c == nil {
		return buildPreviewLines(conv, query, width)
	}
	// Preview lines depend only on the messages and tool calls, and on the
	// width they're wrapped to.
	key := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%s\x00%v", conv.SessionID, len(conv.Messages), len(conv.tools), width, query, conv.peerNotes) // %v sorts map keys
	if c.key != key {
		c.key = key
		c.lines, c.stamps = buildPreview(conv, query, width)
	}
	return c.lines
}

// scrollWindow is the [start, end) of n lines that rows rows show, scrolled
// back lines from the newest (clamped), so 0 shows the end.
func scrollWindow(n, rows, back int) (start, end int) {
	back = min(back, max(0, n-rows))
	start = max(0, n-rows-back)
	return start, min(start+rows, n)
}

// buildPreviewLines builds the scrollable message lines of a conversation
// preview (everything below the fixed header). Shared by renderPreview and
// maxPreviewScroll so the render and the scroll-clamp can never disagree on how
// far the preview can scroll.
func buildPreviewLines(conv Conversation, query string, width int) []string {
	lines, _ := buildPreview(conv, query, width)
	return lines
}

// buildPreview is buildPreviewLines with when each message's lines happened.
func buildPreview(conv Conversation, query string, width int) ([]string, []previewStamp) {
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

	// stamp notes when the lines about to be added happened, for the header,
	// and adds a row naming each new day after the first; true at a new day.
	var stamps []previewStamp
	lastDay := ""
	stamp := func(ts string) bool {
		when := formatTimestamp(ts)
		if when == "" {
			return false
		}
		stamps = append(stamps, previewStamp{len(msgLines), when})
		day, _, _ := strings.Cut(when, " ")
		if day == "" || day == lastDay {
			return false
		}
		if lastDay != "" {
			msgLines = append(msgLines, "\033[90m  "+humanWhen(day, "Mon 2 Jan")+"\033[0m", "")
		}
		lastDay = day
		return true
	}

	// Display messages with gaps. Tool calls and turn ends sit between them,
	// except in a search, which is the matches and their context.
	lastShown := -1
	lastRole := ""
	nextTool := 0
	drawTools := func(upTo int) {
		for query == "" && nextTool < len(conv.tools) && conv.tools[nextTool].at <= upTo {
			t := conv.tools[nextTool]
			stamp(t.ts)
			if t.took > 0 {
				msgLines = append(msgLines, turnEnd(conv, t), "")
			} else {
				msgLines = append(msgLines, renderTool(t, width)...)
				msgLines = append(msgLines, "")
			}
			lastRole = ""
			nextTool++
		}
	}
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

		drawTools(i)
		msg := conv.Messages[i]
		msg.Text = cleanText(msg.Text)
		newDay := stamp(msg.Ts)
		marker := " " // ▶ marks a search match; hitLinesIn looks for it
		if matchSet[i] {
			marker = "▶"
		}
		if blocks := teammateBlocks(msg.Text); blocks != nil && msg.Role == "user" {
			// Messages from teammate agents: each under its own "From" header,
			// JSON status reports unpacked; bare status pings are one dim line.
			for _, b := range blocks {
				if b.body == "" {
					line := truncate("▸ "+b.from+" · "+b.note, max(width-2, 20))
					msgLines = append(msgLines, "\033[90m"+marker+" "+highlight(line, query)+"\033[0m", "")
					continue
				}
				head := fmt.Sprintf("\033[36m%s From %s\033[0m", marker, b.from)
				if b.note != "" {
					head += " \033[90m· " + highlight(b.note, query) + "\033[0m"
				}
				msgLines = append(msgLines, head)
				msgLines = append(msgLines, renderBody(b.body, query, width)...)
				msgLines = append(msgLines, "")
			}
			lastShown, lastRole = i, ""
			continue
		}
		if msg.Role == "sent" {
			// A message this session sent another (SendMessage): to whom, and
			// whether it shows in their transcript (peerLabels).
			to := conv.peerNotes[i]
			if to == "" {
				to = shortAddr(msg.To)
			}
			head := fmt.Sprintf("\033[36m%s To %s\033[0m", marker, to)
			if msg.Note != "" {
				head += " \033[90m· " + highlight(cleanText(msg.Note), query) + "\033[0m"
			}
			msgLines = append(msgLines, head)
			msgLines = append(msgLines, renderBody(msg.Text, query, width)...)
			msgLines = append(msgLines, "")
			lastShown, lastRole = i, ""
			continue
		}
		if from, body, ok := peerParts(msg.Text); ok && msg.Role == "user" {
			// A message sent from another session or ccs, not typed here.
			if n := conv.peerNotes[i]; n != "" {
				from = n // the sending session as listed, found by its copy
			}
			msgLines = append(msgLines, fmt.Sprintf("\033[36m%s From %s\033[0m", marker, from))
			msgLines = append(msgLines, renderBody(body, query, width)...)
			msgLines = append(msgLines, "")
			lastShown, lastRole = i, ""
			continue
		}
		// note draws one dim line; consecutive notes stack without blank lines.
		note := func(line string) {
			if lastRole == "harness" && lastShown == i-1 {
				msgLines = msgLines[:len(msgLines)-1]
				if !newDay && len(stamps) > 0 && msg.Ts != "" {
					stamps[len(stamps)-1].line--
				}
			}
			msgLines = append(msgLines, "\033[90m"+marker+" "+line+"\033[0m", "")
			lastShown, lastRole = i, "harness"
		}
		if ev, ok := channelParts(msg.Text); ok && msg.Role == "channel" {
			// Pushed in by a channel plugin (PR events, Slack): one dim line
			// as Claude Code draws it, its body shown only if a search hit it.
			line := ev.line()
			if width > 0 {
				line = truncate(line, width-2)
			}
			line = highlight(line, query)
			if ev.link != "" {
				line = hyperlink(ev.link, line)
			}
			note(line)
			if ev.detail != "" && containsFold(ev.detail, query) {
				msgLines = append(msgLines[:len(msgLines)-1], renderBody(ev.detail, query, width)...)
				msgLines = append(msgLines, "")
				lastRole = ""
			}
			continue
		}
		if tag, summary, ok := harnessNote(msg.Text); ok && msg.Role == "user" {
			// Injected by the harness (task notifications, reminders, command
			// output): one dim line, so the real conversation stays readable.
			line := "▸ " + tag
			if summary != "" {
				line += " · " + summary
			}
			if width > 0 {
				line = truncate(line, width-2) // truncate squeezes spaces, so add the indent after
			}
			note(highlight(line, query))
			continue
		}
		mark := "●"
		if msg.Role == "user" {
			mark = "❯"
			msg.Text = stripImageNote(msg.Text)
		}
		if matchSet[i] {
			mark = "\033[1m▶\033[22m"
		}
		body := markFirst(renderBody(msg.Text, query, width), mark)
		if msg.Role == "user" {
			body = userRows(body, width)
		}
		lastRole = msg.Role
		msgLines = append(msgLines, body...)
		msgLines = append(msgLines, "")

		lastShown = i
	}
	drawTools(len(conv.Messages))

	if lastShown < len(conv.Messages)-1 {
		remaining := len(conv.Messages) - lastShown - 1
		msgLines = append(msgLines, fmt.Sprintf("\033[90m  ... %d more messages\033[0m", remaining))
	}

	return msgLines, stamps
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
		if strings.HasPrefix(skipLeadingANSI(l), "▶") {
			hits = append(hits, i)
		}
	}
	return hits
}

// skipLeadingANSI is l after the escape codes it starts with. Every frame
// checks every preview line, so it looks only at the start, never the whole
// line.
func skipLeadingANSI(l string) string {
	for strings.HasPrefix(l, "\x1b") {
		loc := ansiSeq.FindStringIndex(l)
		if loc == nil || loc[0] != 0 {
			break
		}
		l = l[loc[1]:]
	}
	return l
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
	top, _ := scrollWindow(len(lines), rows, m.previewScroll)
	back := max(0, len(lines)-rows-top) // previewScroll as drawn (clamped)
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
	_, _, previewTop := m.listLayout()
	return m.height - previewTop - m.chatRows()
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

// linkItem is one link in the Ctrl+O popup: where it goes and the text shown for it.
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

// linksWithPRs is what ^O lists: the selected session's PRs, then the other
// links in view.
func (m model) linksWithPRs() []linkItem {
	if len(m.filtered) == 0 {
		return nil
	}
	conv := m.filtered[m.cursor].conv
	out := append(m.sessionLinks(conv), m.prLinks(conv)...)
	for _, l := range m.visibleLinks() {
		if !slices.ContainsFunc(out, func(p linkItem) bool { return p.url == l.url }) {
			out = append(out, l)
		}
	}
	return out
}

// linkText is how l reads in the ^O popup; a PR's is drawn from its status
// now, so a lookup that lands while the popup is open shows.
func (m model) linkText(l linkItem) string {
	if l.text == "" {
		return m.prLinkText(l.url)
	}
	return l.text
}

// linksShown is the ^O popup's links matching what's typed in it, by the
// search's word rule over the text as shown and the address.
func (m model) linksShown() []linkItem {
	terms := strings.Fields(strings.ToLower(m.linksInput.Value()))
	if len(terms) == 0 {
		return m.links
	}
	var out []linkItem
	for _, l := range m.links {
		if queryMatches(strings.ToLower(ansi.Strip(m.linkText(l))+" "+l.url), "", terms) {
			out = append(out, l)
		}
	}
	return out
}

// linksPopup is the ^O popup: a filter box over linksShown, the cursor on
// one; a list taller than the screen scrolls with the cursor, with a scrollbar.
func (m model) linksPopup() string {
	width := min(max(m.width-12, 30), 90)
	rows := max(m.height-12, 3) // border, padding, title, filter and hints
	shown := m.linksShown()
	cur := min(m.linksCursor, max(len(shown)-1, 0))
	start := 0
	if cur >= rows {
		start = cur - rows + 1
	}
	end := min(start+rows, len(shown))
	var b strings.Builder
	b.WriteString("\033[1mlinks in view\033[0m")
	if len(shown) > 0 {
		fmt.Fprintf(&b, " \033[90m%d/%d\033[0m", cur+1, len(shown))
	}
	in := m.linksInput
	in.Width = width - 4
	b.WriteString("\n\n" + in.View() + "\n\n")
	top, size, bar := scrollThumb(len(shown), start, end-start)
	rowW := width
	if bar {
		rowW-- // the scrollbar's column
	}
	for i := start; i < end; i++ {
		l := shown[i]
		text := m.linkText(l)
		addr := strings.TrimPrefix(strings.TrimPrefix(l.url, "https://"), "http://")
		row := text
		if ansi.Strip(text) != addr && !prURL.MatchString(l.url) && !strings.HasPrefix(l.url, sessionLink) {
			row += " \033[90m" + addr + "\033[39m"
		}
		row = ansi.Truncate(row, rowW-2, "…")
		if i == cur {
			row = "\033[1m› " + row + "\033[0m"
		} else {
			row = "  " + row
		}
		if bar {
			row = withBar(row, width, barCell(i-start, top, size))
		}
		b.WriteString(row + "\n")
	}
	if len(shown) == 0 {
		b.WriteString("\033[90mno links match\033[0m\n")
	}
	b.WriteString("\n\033[90m" + hints("↑↓", "move", keyEnter, "open", "^O/"+keyEsc, "close") + "\033[0m")
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

// humanWhen is a stamp's date ("2026-09-29 21:45", or the date alone) laid out
// as format, with the year when it isn't this one.
func humanWhen(when, format string) string {
	layout := "2006-01-02 15:04"
	if len(when) == len("2006-01-02") {
		layout = "2006-01-02"
	}
	t, err := time.ParseInLocation(layout, when, time.Local)
	if err != nil {
		return when
	}
	if t.Year() != time.Now().Year() {
		format = strings.Replace(format, "2 Jan", "2 Jan 2006", 1)
	}
	return t.Format(format)
}

// formatTook is a length of time as Claude Code's terminal says it: 42s, 1m 4s, 1h 5m.
func formatTook(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", max(int(d.Seconds()), 1))
}

// turnEnd is the dim line after a finished turn: how long it took and when it
// ended, as Claude Code's terminal says it worked for.
func turnEnd(conv Conversation, t toolUse) string {
	glyph, _ := agentGlyph(conv)
	took := formatTook(t.took)
	if _, clock, ok := strings.Cut(formatTimestamp(t.ts), " "); ok {
		took += " · " + clock
	}
	return "\033[90m  " + glyph + " " + took + "\033[0m"
}

// whenWidth is the room the header's date and time take, with their margin.
const whenWidth = len("Wed 28 Oct 21:51") + 2

// withWhen puts when at the right of a width-wide header line, cutting the line
// if it must; a pane too narrow for both keeps the line.
func withWhen(line, when string, width int) string {
	room := width - ansi.StringWidth(when) - 2
	if room < 30 {
		return line
	}
	line = ansi.Truncate(line, room, "…") + "\033[0m"
	return line + strings.Repeat(" ", width-ansi.StringWidth(line)-ansi.StringWidth(when)) + "\033[90m" + when + "\033[0m"
}

// markFirst puts mark in the two-column indent of a message's first line, as
// Claude Code's terminal does, without the blank lines around it.
func markFirst(lines []string, mark string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
		}
	}
	if len(lines) > 0 {
		lines[0] = mark + " " + strings.TrimPrefix(lines[0], "  ")
	}
	return lines
}

// userBg is the row behind what the user typed, dark grey as in Claude Code's
// terminal. It sets the text colour too, so it reads on a light theme as well.
const userBg = "\033[48;5;237;38;5;252m"

// userRows paints the user's lines full width. Every code that ends a style
// inside them ends the background too, so it's set again after each.
func userRows(lines []string, width int) []string {
	for i, l := range lines {
		l = strings.ReplaceAll(l, "\033[0m", "\033[0m"+userBg)
		l = strings.ReplaceAll(l, "\033[49;39m", "\033[49;39m"+userBg)
		l = strings.ReplaceAll(l, "\033[39m", "\033[39m"+userBg)
		pad := strings.Repeat(" ", max(width-ansi.StringWidth(l), 0))
		lines[i] = userBg + l + pad + "\033[0m"
	}
	return lines
}

// renderTool draws a tool call as Claude Code's terminal does: "● Bash(cmd)",
// a command's lines kept as they are and indented under it.
func renderTool(t toolUse, width int) []string {
	text := t.name
	if arg := cleanText(t.arg); arg != "" {
		text += "(" + strings.ReplaceAll(arg, "\t", "    ") + ")"
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if width <= 12 {
			out = append(out, "  "+line)
			continue
		}
		for j, w := range strings.Split(ansi.Wrap(line, width-3, ""), "\n") {
			if j > 0 {
				w = strings.TrimLeft(w, " ")
			}
			out = append(out, "  "+w)
		}
	}
	if rest, ok := strings.CutPrefix(out[0], "  "+t.name); ok {
		out[0] = "● \033[1m" + t.name + "\033[22m" + rest
	} else { // a name too long for the line, wrapped
		out[0] = "● " + strings.TrimPrefix(out[0], "  ")
	}
	return out
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
	return max(m.height-previewTop-m.chatRows()-len(m.previewHeader(conv, m.textInput.Value())), 1)
}

// previewHeader is the preview's fixed header (always visible above the
// scrolling messages).
func (m model) previewHeader(conv Conversation, query string) []string {
	if m.headerCollapsed {
		return []string{m.headerSummary(conv, query), ""}
	}
	header := []string{"\033[1;33mProject:\033[0m " + highlight(conv.Cwd, query) + " \033[90m▾ ^X D\033[0m"}
	if conv.Title != "" {
		header = append(header, "\033[1;33mName:\033[0m    "+highlight(conv.Title, query))
	}
	header = append(header, "\033[1;33mSession:\033[0m "+highlight(conv.SessionID, query))
	header = append(header, sessionStats(conv)...)
	if !m.table { // the list's columns aren't beside it
		header = append(header, fmt.Sprintf("\033[1;33mSize:\033[0m    %s · %d messages · %s", formatBytes(conv.Size), len(conv.Messages), formatAgo(conv.LastTimestamp, time.Now())))
	}
	header = append(header, m.prLines(conv, m.paneWidth())...)
	header = append(header, m.taskLines(conv, m.paneWidth())...)
	header = append(header, m.subagentLines(conv, m.paneWidth())...)
	return append(header, "")
}

// headerSummary is the header as one line (^X D): the session's name and where
// it is, then what the full header would make you look for: an API error, its
// model and context, its PRs, running tasks and subagents, and its size.
func (m model) headerSummary(conv Conversation, query string) string {
	parts := []string{highlight(truncate(filepath.Base(conv.Cwd), 24), query)}
	if conv.Kind == kindCodex {
		glyph, colour := agentGlyph(conv)
		parts = append(parts, "\033["+colour+"m"+glyph+"\033[0m codex")
	}
	if conv.LastError != "" {
		parts = append(parts, "\033[1;31m! "+conv.LastError+"\033[0m")
	}
	if s := shortModel(conv); s != "" {
		parts = append(parts, s)
	}
	if conv.ContextTokens > 0 {
		window := conv.window()
		code, _ := ctxColour(conv.ContextTokens, window)
		parts = append(parts, fmt.Sprintf("\033[%sm%d%%\033[0m ctx", code, conv.ContextTokens*100/window))
	}
	if cell := strings.TrimSpace(m.prCell(conv, true)); ansi.Strip(cell) != "" {
		parts = append(parts, "PRs "+cell)
	}
	if n := len(m.runningTasks(conv)); n > 0 {
		parts = append(parts, fmt.Sprintf("\033[%sm⧗%d\033[0m", taskColour, n))
	}
	if n := m.runningSubagents(conv); n > 0 {
		parts = append(parts, fmt.Sprintf("\033[%sm◆%d\033[0m", subagentColour, n))
	}
	parts = append(parts, fmt.Sprintf("%s · %d msgs", formatBytes(conv.Size), len(conv.Messages)))
	// The topic gets what's left beside the facts and the time (whenWidth), so
	// a long one or a narrow pane can't push the counts off the line.
	const sep, hint, lead = " \033[90m·\033[0m ", " \033[90m^X D\033[0m", "\033[90m▸\033[0m "
	rest := strings.Join(parts, sep)
	topic := truncate(getTopic(conv), min(max(m.paneWidth()-1-whenWidth-ansi.StringWidth(lead+sep+rest+hint), 12), 40))
	return ansi.Truncate(lead+highlight(topic, query)+sep+rest+hint, max(m.paneWidth()-1, 20), "…") + "\033[0m"
}

// toggleHeader switches the preview header between its full form and one line.
func (m *model) toggleHeader() {
	m.headerCollapsed = !m.headerCollapsed
	saveSettings(m.settings())
	m.previewScroll = max(0, min(m.previewScroll, m.maxPreviewScroll())) // more rows can mean less to scroll
}

func (m model) renderPreview(item listItem, height int) string {
	query := m.textInput.Value()
	conv := item.conv

	header := m.previewHeader(conv, query) // fixed, always visible

	msgLines := m.previewLines() // memoised; item is always the selected conversation

	// Apply scroll to messages only (header stays fixed). Clamp locally for this
	// render; the persisted m.previewScroll is bounded in Update via
	// maxPreviewScroll (this method has a value receiver, so a write here would
	// be discarded).
	msgHeight := height - len(header)
	if msgHeight < 1 {
		msgHeight = 1
	}
	start, end := scrollWindow(len(msgLines), msgHeight, m.previewScroll)
	visibleMsgLines := msgLines[start:end]

	// The header's first line says when the top visible message happened.
	if when := m.preview.stampAt(start); when != "" && len(visibleMsgLines) > 0 {
		header[0] = withWhen(header[0], humanWhen(when, "Mon 2 Jan 15:04"), m.paneWidth()-1)
	}

	// While searching, where the view is among this conversation's hits.
	var hits []int
	if query != "" { // every frame: skip the scan when there can be no hits
		hits = hitLinesIn(msgLines)
	}
	if len(hits) > 0 {
		i := sort.SearchInts(hits, start) + 1 // first hit at or below the top
		header[len(header)-1] += fmt.Sprintf("  \033[90mhit %d/%d · ^] next ^\\ prev\033[0m", min(i, len(hits)), len(hits))
	}

	if top, size, ok := scrollThumb(len(msgLines), start, msgHeight); ok {
		visibleMsgLines = slices.Clone(visibleMsgLines) // the cache's lines stay bare
		for i := range visibleMsgLines {
			visibleMsgLines[i] = withBar(visibleMsgLines[i], m.paneWidth(), barCell(i, top, size))
		}
	}

	// Combine header + scrolled messages
	allLines := append(header, visibleMsgLines...)
	return strings.Join(allLines, "\n")
}

// sessionStats is the preview header's model, token and error lines.
func sessionStats(conv Conversation) []string {
	var lines []string
	if conv.Kind == kindCodex {
		glyph, colour := agentGlyph(conv)
		lines = append(lines, "\033[1;33mAgent:\033[0m   \033["+colour+"m"+glyph+"\033[0m codex")
	}
	if conv.ActiveModel != "" || conv.ContextTokens > 0 {
		window := conv.window()
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
	tok := func(n int64) string { // formatTokens leaves 0 blank, for the list
		if n <= 0 {
			return "0"
		}
		return formatTokens(int(n))
	}
	if u := conv.Usage; u != (tokenUsage{}) && conv.Kind == kindCodex {
		out := tok(u.Output)
		if conv.Reasoning > 0 {
			out += " (reasoning " + tok(conv.Reasoning) + ")"
		}
		cost := fmt.Sprintf(" · ~$%.2f at API prices", conv.Cost)
		switch {
		case conv.Unpriced && conv.Cost > 0:
			cost += " (some usage on a model with no price)"
		case conv.Unpriced:
			cost = " · no price for " + conv.ActiveModel
		}
		lines = append(lines, fmt.Sprintf("\033[1;33mTokens:\033[0m  in %s · out %s · cache read %s · cache write %s%s",
			tok(u.Input), out, tok(u.CacheRead), tok(u.Cache5m), cost))
	} else if u != (tokenUsage{}) {
		eff := u.effective()
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

// channelEvent is one event a channel plugin pushed into a session:
// <channel source="plugin:name:server" event="…" …>, then a line saying what
// happened, usually its address, then any body (a comment, a review).
type channelEvent struct {
	source, summary, link, detail string
}

var (
	channelMessage = regexp.MustCompile(`(?s)^<channel\s([^>]*)>\n?(.*?)\n?</channel>\s*$`)
	channelAttr    = regexp.MustCompile(`\b(source|event)="([^"]*)"`)
	bareURL        = regexp.MustCompile(`^https?://\S+$`)
)

func isChannelEvent(text string) bool {
	return strings.HasPrefix(text, "<channel ") && channelMessage.MatchString(text)
}

func channelParts(text string) (channelEvent, bool) {
	m := channelMessage.FindStringSubmatch(text)
	if m == nil {
		return channelEvent{}, false
	}
	var ev channelEvent
	event := ""
	for _, a := range channelAttr.FindAllStringSubmatch(m[1], -1) {
		if a[1] == "source" {
			ev.source = a[2][strings.LastIndex(a[2], ":")+1:] // plugin:agentic-utils-github:pr-events
		} else {
			event = a[2]
		}
	}
	lines := strings.Split(strings.TrimSpace(m[2]), "\n")
	ev.summary = strings.TrimPrefix(lines[0], ev.source+": ")
	lines = lines[1:]
	if len(lines) > 0 && bareURL.MatchString(strings.TrimSpace(lines[0])) {
		ev.link = strings.TrimSpace(lines[0])
		lines = lines[1:]
	}
	ev.detail = strings.TrimSpace(strings.Join(lines, "\n"))
	if ev.summary == "" {
		ev.summary = event
	}
	return ev, true
}

// line is the event as Claude Code's terminal shows it.
func (ev channelEvent) line() string {
	return "← " + ev.source + ": " + ev.summary
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
	anyLink = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)|https?://[^\s<>()\[\]"'\x60\x1b]*[^\s<>()\[\]"'\x60\x1b.,;:!?*]`)
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

// inlineMarkdown renders [links](url), **bold** and `code` spans. Links go
// last: bold or code markers matched across a link would put their codes
// inside its OSC 8 sequence, which terminals read differently (iTerm can show
// the address as text, so the row wraps and the screen scrolls). Until then the
// codes' own [ is hidden (styleOpen), or a link would start at it and take the
// text since then as its own.
func inlineMarkdown(s string) string {
	const styleOpen = "\033\x02"
	s = mdCode.ReplaceAllString(s, styleOpen+"36m$1"+styleOpen+"39m")
	s = mdBold.ReplaceAllString(s, styleOpen+"1m$1"+styleOpen+"22m")
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
	return strings.ReplaceAll(s, styleOpen, "\033[")
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
	if w := ansi.StringWidth(s); w < length { // screen cells, so emoji and CJK count double
		return s + strings.Repeat(" ", length-w)
	}
	return ansi.Truncate(s, max(length, 0), "")
}

// scrollThumb places a scrollbar's thumb on a track of h rows, for h of total
// lines in view from start: its size is the share in view, so a long
// conversation gets a short thumb. ok is false when everything fits.
func scrollThumb(total, start, h int) (top, size int, ok bool) {
	if h <= 0 || total <= h {
		return 0, 0, false
	}
	size = max(h*h/total, 1)
	return min(start*(h-size)/(total-h), h-size), size, true
}

// barCell is row r of a scrollbar whose thumb is size rows from top.
func barCell(r, top, size int) string {
	if r >= top && r < top+size {
		return "\033[37m┃\033[0m"
	}
	return "\033[90m│\033[0m"
}

// withBar puts cell in the last of w columns of line.
func withBar(line string, w int, cell string) string {
	line = ansi.Truncate(line, w-1, "")
	return line + "\033[0m" + strings.Repeat(" ", max(w-1-ansi.StringWidth(line), 0)) + cell
}

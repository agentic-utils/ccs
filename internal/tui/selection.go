package tui

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/proc"
	"github.com/agentic-utils/ccs/internal/stats"
	"github.com/agentic-utils/ccs/internal/transcript"
)

// ccs holds the mouse, so the terminal's own selection only works with ⌥
// held. Dragging in the preview selects its text here instead, and letting go
// copies it: previewSel is that drag.
type previewSel struct {
	pressed  bool     // the left button went down on the preview and hasn't come up
	px, py   int      // where it went down, on the screen
	dragging bool     // it moved a cell or more: a drag, not a click
	shown    bool     // a selection to draw: being dragged, or let go and not cleared yet
	a, h     selPoint // where the drag started and where it is now
}

// selPoint is a place in the preview's message lines: the line (an index
// into previewLines, so it holds while the preview scrolls) and the column.
type selPoint struct{ line, col int }

func (p selPoint) before(q selPoint) bool {
	return p.line < q.line || p.line == q.line && p.col < q.col
}

// selSpan is the selection's first and last cell, in order.
func (s previewSel) selSpan() (from, to selPoint) {
	if s.h.before(s.a) {
		return s.h, s.a
	}
	return s.a, s.h
}

// selCols is the [start, end) columns of line the selection covers, ok false
// when it covers none of it.
func (s previewSel) selCols(line int) (start, end int, ok bool) {
	if !s.shown {
		return 0, 0, false
	}
	from, to := s.selSpan()
	if line < from.line || line > to.line {
		return 0, 0, false
	}
	start, end = 0, 1<<30
	if line == from.line {
		start = from.col
	}
	if line == to.line {
		end = to.col + 1
	}
	return start, end, true
}

// msgArea is where the preview's message lines are on the screen: the first
// row under the fixed header, how many rows, and the first column.
func (m model) msgArea() (top, rows, x int) {
	_, _, previewTop := m.listLayout()
	header := 0
	if len(m.filtered) > 0 {
		header = len(m.previewHeader(m.filtered[m.cursor].conv, m.searchQuery()))
	}
	if sw := m.sidebarWidth(); sw > 0 {
		x = sw + 1
	}
	rows = max(m.previewRenderHeight()-header, 1)
	return previewTop + header, rows, x
}

// pointAt is the message line and column under screen cell (x, y), and
// whether there is one.
func (m model) pointAt(x, y int) (selPoint, bool) {
	top, rows, left := m.msgArea()
	lines := m.previewLines()
	if y < top || y >= top+rows || len(lines) == 0 {
		return selPoint{}, false
	}
	start, _ := scrollWindow(len(lines), rows, m.previewScroll)
	line := min(start+y-top, len(lines)-1)
	col := min(max(x-left, 0), max(m.paneWidth()-2, 0)) // never the scrollbar's column
	return selPoint{line, col}, true
}

// selectDrag follows the pointer while the button is held. The first move of
// a cell or more starts a selection where the button went down; dragging past
// the top or bottom of the messages scrolls the preview a line a move.
func (m model) selectDrag(msg tea.MouseMsg) model {
	if !m.sel.dragging {
		if msg.X == m.sel.px && msg.Y == m.sel.py {
			return m
		}
		m.sel.dragging = true // no longer a click, wherever it started
		a, ok := m.pointAt(m.sel.px, m.sel.py)
		if !ok { // went down on the header: nothing to select
			return m
		}
		m.sel.a, m.sel.h, m.sel.shown = a, a, true
	}
	if !m.sel.shown {
		return m
	}
	top, rows, _ := m.msgArea()
	y := msg.Y
	switch {
	case y < top:
		m.previewScroll = min(m.previewScroll+1, m.maxPreviewScroll())
		y = top
	case y >= top+rows:
		m.previewScroll = max(m.previewScroll-1, 0)
		y = top + rows - 1
	}
	if p, ok := m.pointAt(msg.X, y); ok {
		m.sel.h = p
	}
	return m
}

// selectRelease ends the press: a drag copies what it selected and stays
// drawn until the next click, key or scroll; a click does what a click on
// the preview always did.
func (m model) selectRelease() model {
	if !m.sel.dragging {
		x, y := m.sel.px, m.sel.py
		m.sel = previewSel{}
		m.clickPreview(x, y)
		return m
	}
	m.sel.pressed, m.sel.dragging = false, false
	if !m.sel.shown {
		return m
	}
	from, to := m.sel.selSpan()
	if text := selectionText(m.previewLines(), from, to); text != "" {
		stats.Record("tui.copy_selection", stats.Mouse)
		m.copyPending = text
	}
	return m
}

// clickPreview is a click on the preview at screen cell (x, y): open the link
// under it, or show or hide the details from the summary line.
func (m *model) clickPreview(x, y int) {
	if len(m.filtered) == 0 {
		return
	}
	_, _, previewTop := m.listLayout()
	_, _, paneX := m.msgArea()
	// ccs has the mouse, so the terminal never sees a click on a link: open it here.
	lines := strings.Split(m.renderPreview(m.filtered[m.cursor], m.previewRenderHeight()), "\n")
	row := y - previewTop
	if row < 0 || row >= len(lines) {
		return
	}
	m.linkPending = linkAt(lines[row], x-paneX)
	// A click anywhere on the summary line (its first) shows or hides the
	// details; the detail rows below it hold links and stay put.
	if m.linkPending != "" {
		stats.Record("tui.link_open", stats.Mouse)
	} else if row == 0 {
		stats.Record("tui.details", stats.Mouse)
		m.toggleHeader()
	}
}

// selectionText is the text from one cell to another of the preview's
// message lines as shown, without colours: a line wrapped to fit is joined
// back onto the one above, a newline in the text stays one, and the two
// columns every message line starts with (● or ❯, or the indent) are left
// out, so a code block keeps its own indentation.
func selectionText(lines []string, from, to selPoint) string {
	var b strings.Builder
	for l := from.line; l <= to.line && l < len(lines); l++ {
		raw, mark := splitMark(lines[l])
		plain := ansi.Strip(raw)
		start, end := 0, ansi.StringWidth(plain)
		if l == from.line {
			start = from.col
		}
		if l == to.line {
			end = min(end, to.col+1)
		}
		if mark == 0 {
			start = max(start, 2)
		}
		piece := ""
		if end > start {
			piece = ansi.Cut(plain, start, end)
		}
		if mark != 0 {
			piece = strings.TrimLeft(piece, " ") // the indent it was wrapped under
		}
		piece = strings.TrimRight(piece, " ") // a user row's padding
		if l > from.line {
			switch {
			case mark == 0:
				b.WriteByte('\n')
			case mark == softSpace && piece != "":
				b.WriteByte(' ')
			}
		}
		b.WriteString(piece)
	}
	return strings.Trim(b.String(), "\n")
}

// highlightLine draws columns [start, end) of a preview line in reverse video.
func highlightLine(line string, start, end int) string {
	w := ansi.StringWidth(line)
	end = min(end, w)
	if start >= end {
		return line
	}
	return ansi.Cut(line, 0, start) + "\033[0m\033[7m" + ansi.Strip(ansi.Cut(line, start, end)) + "\033[0m" + ansi.Cut(line, end, w)
}

// copiedMsg is the clipboard write's outcome.
type copiedMsg struct {
	n   int
	err error
}

func copyCmd(text string) tea.Cmd {
	return func() tea.Msg {
		return copiedMsg{utf8.RuneCountInString(text), writeClipboard(text)}
	}
}

func (m *model) applyCopied(msg copiedMsg) {
	if msg.err != nil {
		m.errorMsg = "Couldn't copy: " + msg.err.Error()
		return
	}
	m.note(fmt.Sprintf("copied %d chars", msg.n))
}

// copyReply is ^T C: the selected session's last reply to the clipboard, the
// keyboard's way to copy without the mouse.
func (m *model) copyReply() tea.Cmd {
	if len(m.filtered) == 0 {
		return nil
	}
	msgs := m.filtered[m.cursor].conv.Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			if text := strings.TrimSpace(transcript.CleanText(msgs[i].Text)); text != "" {
				return copyCmd(text)
			}
		}
	}
	m.note("no reply to copy")
	return nil
}

// clipboardTimeout is how long pbcopy may take.
const clipboardTimeout = 3 * time.Second

// pbcopy puts text on the macOS clipboard; errNoPbcopy where there is none.
var pbcopy = func(text string) error {
	path, err := exec.LookPath("pbcopy")
	if err != nil {
		return errNoPbcopy
	}
	return proc.RunInput(clipboardTimeout, text, path)
}

var errNoPbcopy = errors.New("no pbcopy")

// osc52 asks the terminal to put text on its clipboard (OSC 52), which reaches
// the machine you sit at over ssh; through tmux, wrapped for passthrough.
var osc52 = func(text string) {
	seq := "\033]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	if os.Getenv("TMUX") != "" {
		seq = "\033Ptmux;" + strings.ReplaceAll(seq, "\033", "\033\033") + "\033\\"
	}
	os.Stdout.WriteString(seq)
}

// writeClipboard copies text: pbcopy, and OSC 52 as well over ssh (pbcopy
// there is the remote machine's) or when there's no pbcopy. Nothing is ever
// pasted or typed.
func writeClipboard(text string) error {
	err := pbcopy(text)
	noPb := errors.Is(err, errNoPbcopy)
	if os.Getenv("SSH_TTY") != "" || noPb {
		osc52(text)
		if noPb {
			err = nil
		}
	}
	return err
}

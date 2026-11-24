package tui

// A held permission prompt's menu, in the bottom pane, as Claude Code's own
// permission menu works: numbered rows (1 Yes, then a "Yes, and don't ask
// again" for each rule Claude Code suggested, then No, and tell Claude what
// to do differently), ↑/↓ (j/k) to move, Enter or a row's digit sends it at
// once, A sends Yes and D sends No. No opens a box for what Claude should do
// instead (Enter sends it, empty sends a plain No; Esc closes it). Esc
// leaves the menu for the list. Enter, digits, A and D do nothing for
// menuFresh after the prompt first shows, so a key meant for something
// else can't approve it.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/agentic-utils/ccs/internal/mcp"
)

// permRows are a permission prompt's menu rows: what ccs serve (or this
// ccs's own hold) listed, else Yes and No, as an older one offers.
func permRows(h mcp.HeldPrompt) []mcp.HeldOption {
	if len(h.Options) > 0 {
		return h.Options
	}
	return []mcp.HeldOption{{Choice: "allow", Label: "Yes"}, {Choice: "deny", Label: "No, and tell Claude what to do differently"}}
}

// permKey handles a key on a held permission prompt's menu. done is false
// for a key the menu doesn't use (it goes to the search) and for Esc with
// no box open, which leaves the menu.
func (m *model) permKey(h mcp.HeldPrompt, name, k string, msg tea.KeyMsg) (tea.Cmd, bool) {
	mn := m.menuFor(h)
	rows := permRows(h)
	mn.cursor = min(max(mn.cursor, 0), len(rows)-1)
	digit := len(k) == 1 && k >= "1" && k <= "9"
	send := k == "enter" || digit || k == "a" || k == "A" || k == "d" || k == "D"
	if heldNow().Sub(mn.shownAt) < menuFresh && (k == "enter" || (mn.editing == boxNone && send)) {
		return nil, true // unread: a stray key never answers it
	}
	if mn.editing != boxNone {
		switch k {
		case "esc":
			mn.notes[0] = mn.box.Value() // kept for when it's opened again
			mn.editing = boxNone
			mn.box.Blur()
		case "enter":
			text := strings.TrimSpace(mn.box.Value())
			mn.editing = boxNone
			mn.box.Blur()
			return m.permSend(h, name, "deny", text), true
		default:
			mn.box, _ = editKey(mn.box, msg)
		}
		return nil, true
	}
	pick := func(i int) tea.Cmd {
		mn.cursor = i
		if rows[i].Choice == "deny" {
			m.menuOpen(mn, boxNote, mn.notes[0], "what Claude should do instead (enter with none: just no)")
			return nil
		}
		return m.permSend(h, name, rows[i].Choice, "")
	}
	switch {
	case k == "up" || k == "k":
		mn.cursor = (mn.cursor - 1 + len(rows)) % len(rows)
	case k == "down" || k == "j":
		mn.cursor = (mn.cursor + 1) % len(rows)
	case k == "esc":
		return nil, false // back to the list
	case digit:
		n := int(k[0] - '1')
		if n >= len(rows) {
			m.setHeldNote(fmt.Sprintf("options here are 1-%d", min(len(rows), 9)))
			return nil, true
		}
		return pick(n), true
	case k == "enter":
		return pick(mn.cursor), true
	case k == "a" || k == "A":
		return m.permSend(h, name, "allow", ""), true
	case k == "d" || k == "D":
		return m.permSend(h, name, "deny", ""), true
	case len(k) == 1 && k >= "0" && k <= "9":
		// 0: while a prompt is held every digit is the held prompt's.
		m.setHeldNote(fmt.Sprintf("options here are 1-%d", min(len(rows), 9)))
	default:
		return nil, false
	}
	return nil, true
}

// permSend answers a permission prompt: allow, always:<n> or deny, a deny's
// note being what Claude is told to do instead.
func (m *model) permSend(h mcp.HeldPrompt, name, choice, note string) tea.Cmd {
	var notes map[int]string
	if choice == "deny" && note != "" {
		notes = map[int]string{0: note}
	}
	return m.answerHeldCmd(h, name, choice, nil, nil, notes)
}

// permLines draws a permission prompt in Claude Code's style: one header
// line, what it wants to run wrapped, a blank line, then the numbered rows
// with "❯" on the highlighted one; No's box when it's open; one hint row
// while the menu has the keys (active).
func (m model) permLines(h mcp.HeldPrompt, width int, active bool) []string {
	mn := mn(m, h)
	width = max(width, 20)
	rows := permRows(h)
	cur := min(max(mn.cursor, 0), len(rows)-1)
	lines := []string{paneHeader("\033[1;"+amber+"m"+h.Tool+"\033[0m \033[90m· permission\033[0m", width)}
	lines = append(lines, wrapIndent("", "\033[1m", h.Summary, width)...)
	lines = append(lines, "", "Do you want to proceed?")
	for i, r := range rows {
		lead := "  "
		if i == cur && mn.editing == boxNone {
			lead = "\033[1;" + amber + "m❯\033[0m "
		}
		lines = append(lines, ansi.Truncate(fmt.Sprintf("%s%d. %s", lead, i+1, r.Label), width, "…"))
	}
	if mn.editing != boxNone {
		lines = append(append(lines, ""), menuBoxLines(mn, width)...)
		return append(lines, paneHintsFit(width, keyEnter, "send no", keyEsc, "cancel"))
	}
	if !active {
		return lines
	}
	send := "send"
	if rows[cur].Choice == "deny" {
		send = "no, with a note"
	}
	return append(lines, "", paneHintsFit(width, "↑↓", "move", keyEnter, send, "a", "yes", "d", "no", keyEsc, "list"))
}

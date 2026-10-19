package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// richText is a reply with everything that makes a rendered line hard to fit:
// a wide table, bold, nested bullets, links and very long lines.
const richText = "Yes, and there's more context than I'd found before. The most useful piece is the roster, which changes the picture.\n\n" +
	"**Delphi and Norse are two separate teams with different people.** From the [Notion page](https://www.notion.so/3d129959c003818c97cbdf8c101b84a2) roster:\n\n" +
	"| Team | Members |\n|---|---|\n" +
	"| Delphi (data scientists and ML) | Alvin, Chloe, Ilya, Saad, Vassilis, Wojciech, Xiomara, Yusuf, Zainab, and a few more names to make it wide |\n" +
	"| Norse Code (software engineers) | Dawid, Georg, Harry, Haseeb, James (team lead), Kacper, Lukas, Maciej, Nikolai, Oskar |\n\n" +
	"- **Saad is listed under Delphi.** That backs up what you said about Data moving across.\n" +
	"- **Delphi support is mostly done by Norse engineers.** Apart from Vassilis, nobody in Delphi does engineering work, so the Norse engineers cover it.\n" +
	"  - **No merger was decided.** Espen's last suggestion (2 Oct) was for Norse to keep both; the norse-delphi-coop channel is unrelated: it's from 2024.\n\n" +
	"**Why the rota has shifted lately:**\n" +
	"- **Squads started on 28 Sep.** Espen's [18 Sep post in #product](https://two-inc.slack.com/archives/C0123456/p1726650000000000) set up that squad. https://two-inc.slack.com/archives/C0123456/p1726650000000000?thread_ts=1726650000.000000&cid=C0123456\n\n" +
	"I can add the Notion roster to the ticket and change the plan to \"pools as they really are\" with a link to Notion for team membership, and `inline code that is quite long too` here?"

// richModel is waitModel with two waiting sessions whose transcripts are rich.
func richModel(t *testing.T, w, h int) model {
	t.Helper()
	m := waitModel(t)
	var convs []Conversation
	for _, item := range m.items {
		c := item.conv
		if c.SessionID == "live" || c.SessionID == "menu" {
			for j := 0; j < 6; j++ {
				c.Messages = append(c.Messages,
					Message{Role: "user", Text: fmt.Sprintf("question %d about the roster and the rota, with a long line that goes on and on past any sensible width", j), Ts: fmt.Sprintf("2026-10-19T09:%02d:00Z", 30+j)},
					Message{Role: "assistant", Text: richText, Ts: fmt.Sprintf("2026-10-19T09:%02d:30Z", 30+j)})
			}
		}
		convs = append(convs, c)
	}
	for i := 0; i < 60; i++ { // enough sessions to fill the list
		convs = append(convs, Conversation{SessionID: fmt.Sprintf("filler-%d", i), Title: fmt.Sprintf("filler session %d", i), LastTimestamp: "2026-09-20T09:00:00Z", Messages: []Message{{Role: "user", Text: "hi"}}})
	}
	m.items = buildItems(convs)
	m.updateFilter()
	m.width, m.height = w, h
	return m
}

// checkFrame asserts a frame never needs more rows or columns than the
// terminal has, and that no escape sequence was cut into visible text.
func checkFrame(t *testing.T, label string, view string, w, h int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > h {
		t.Errorf("%s: %d lines, want <= %d", label, len(lines), h)
	}
	for i, l := range lines {
		if sw := ansi.StringWidth(l); sw > w {
			t.Errorf("%s: line %d is %d wide, want <= %d: %q", label, i, sw, w, ansi.Strip(l))
		}
		if strings.ContainsAny(ansi.Strip(l), "\x1b\r") {
			t.Errorf("%s: line %d has a cut escape: %q", label, i, l)
		}
	}
}

func TestOverlayFrameFitsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{120, 45}, {100, 40}, {150, 50}} {
		w, h := size[0], size[1]
		m := richModel(t, w, h)
		now := time.Now()
		stats := map[string]sessionStat{"live": waiting(now.Add(-55*time.Minute), "input needed"), "menu": waiting(now.Add(-5*time.Minute), "permission prompt")}
		m.applyStats(stats)
		checkFrame(t, fmt.Sprintf("banner %dx%d", w, h), m.View(), w, h)
		m = selectLive(m)
		checkFrame(t, fmt.Sprintf("banner+preview %dx%d", w, h), m.View(), w, h)
		m = openByHand(m, stats)
		if !m.waitOpen {
			t.Fatalf("%dx%d: overlay didn't open", w, h)
		}
		checkFrame(t, fmt.Sprintf("overlay %dx%d", w, h), m.View(), w, h)
		m, _ = key(m, tea.KeyMsg{Type: tea.KeyCtrlN})
		checkFrame(t, fmt.Sprintf("overlay next %dx%d", w, h), m.View(), w, h)
	}
}

// narrowSymbols are the symbols from the dingbat and misc-symbol blocks that
// ccs draws and that terminals agree are one cell wide (checked in tmux).
// Others there, like ✍, are emoji that some terminals draw two cells wide
// while ansi.StringWidth counts one, so a full-width row soft-wraps and the
// renderer repaints rows in the wrong place.
var narrowSymbols = map[rune]bool{'⚙': true, '⚠': true, '✓': true, '✗': true, '✎': true}

func TestScreenUsesOnlySymbolsEveryTerminalDrawsNarrow(t *testing.T) {
	m := richModel(t, 150, 50)
	for i := range m.items {
		m.items[i].conv.IsCustomTitle = true
	}
	m.items[3].conv.Spawned = true
	m.updateFilter()
	now := time.Now()
	stats := map[string]sessionStat{"live": waiting(now, "input needed"), "menu": waiting(now, "permission prompt")}
	m.applyStats(stats)
	m = selectLive(m)
	views := []string{m.View()}
	views = append(views, openByHand(m, stats).View())
	for _, v := range views {
		for _, r := range ansi.Strip(v) {
			if r >= 0x2600 && r <= 0x27BF && !narrowSymbols[r] {
				t.Errorf("screen draws %q (U+%04X), which some terminals draw two cells wide", r, r)
			}
		}
	}
}

package mcp

// What the ccs serve pages show, shaped for a phone: sessions grouped by
// what they need, a session's messages as bubbles, the "waiting on you"
// count, when a page refreshes itself (no scripts: a meta refresh), and the
// web app manifest and icons that let it be added to a home screen.

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentic-utils/ccs/internal/transcript"
)

// msgPage is how many messages a session page shows.
const msgPage = 40

// msgFoldLines is how long a message gets before it's folded.
const msgFoldLines = 12

// bucketView is one triage bucket's sessions.
type bucketView struct {
	Key, Title string
	Rows       []TriageRow
}

var bucketTitles = map[string]string{
	BucketWaitingOnUser:     "Waiting on you",
	BucketWorking:           "Working",
	BucketWaitingOnExternal: "Waiting on others",
	BucketStalled:           "Stalled",
	BucketIdle:              "Idle",
	BucketDone:              "Done",
}

// buckets groups rows by bucket, in the order the buckets first appear
// (TriageRows sorts the most urgent first).
func buckets(rows []TriageRow) []bucketView {
	var out []bucketView
	at := map[string]int{}
	for _, r := range rows {
		i, ok := at[r.Bucket]
		if !ok {
			title := bucketTitles[r.Bucket]
			if title == "" {
				title = strings.ReplaceAll(r.Bucket, "_", " ")
			}
			at[r.Bucket] = len(out)
			out = append(out, bucketView{Key: r.Bucket, Title: title})
			i = len(out) - 1
		}
		out[i].Rows = append(out[i].Rows, r)
	}
	return out
}

func waitingOnYou(rows []TriageRow) int {
	n := 0
	for _, r := range rows {
		if r.Bucket == BucketWaitingOnUser {
			n++
		}
	}
	return n
}

// msgView is one message as a bubble.
type msgView struct {
	Kind     string // you, claude, peer, sent, note
	Who      string
	Time     string // 15:04
	Day      string // set on the first message of a day
	Segments []segment
	Lines    int
	Long     bool
	Summary  string // the first lines, for a folded message
}

// segment is prose or a fenced code block.
type segment struct {
	Code bool
	Text string
}

// segments splits text on ``` fences; a code block drops its language line.
func segments(text string) []segment {
	parts := strings.Split(text, "```")
	var out []segment
	for i, p := range parts {
		code := i%2 == 1 && i < len(parts)-1 // an unclosed fence stays prose
		if code {
			if nl := strings.IndexByte(p, '\n'); nl >= 0 && !strings.ContainsAny(p[:nl], " \t") {
				p = p[nl+1:]
			}
			p = strings.TrimRight(p, "\n")
		} else {
			p = strings.Trim(p, "\n")
		}
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, segment{Code: code, Text: p})
	}
	return out
}

// messageViews turns conv's messages from start to end into bubbles.
func messageViews(conv transcript.Conversation, start, end int) []msgView {
	agent := "Claude"
	if conv.Kind == transcript.KindCodex {
		agent = "Codex"
	}
	var out []msgView
	lastDay := ""
	for _, m := range conv.Messages[start:end] {
		text := transcript.CleanText(m.Text)
		v := msgView{}
		switch m.Role {
		case "assistant":
			v.Kind, v.Who = "claude", agent
		case "sent":
			v.Kind, v.Who = "sent", "To another session"
			if m.Note != "" {
				v.Who += " · " + transcript.CleanText(m.Note)
			}
		default:
			if tag, summary, ok := transcript.HarnessNote(text); ok {
				v.Kind, v.Who = "note", tag
				text = summary
			} else if from, body, ok := transcript.PeerParts(text); ok {
				v.Kind, text = "peer", body
				v.Who = "From " + from
				if from == remoteSender {
					v.Kind, v.Who = "you", "You, via ccs"
				}
			} else {
				v.Kind, v.Who = "you", "You"
				text = transcript.StripImageNote(text)
			}
		}
		if ts, err := time.Parse(time.RFC3339, m.Ts); err == nil {
			ts = ts.Local()
			v.Time = ts.Format("15:04")
			if day := ts.Format("Mon 2 Jan"); day != lastDay {
				v.Day, lastDay = day, day
			}
		}
		v.Segments = segments(text)
		v.Lines = strings.Count(text, "\n") + 1
		if v.Kind != "note" && v.Lines > msgFoldLines {
			v.Long = true
			lines := strings.SplitN(strings.TrimLeft(text, "\n"), "\n", 4)
			v.Summary = strings.Join(lines[:min(3, len(lines))], "\n")
		}
		if len(v.Segments) == 0 && v.Kind != "note" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// pageOf is the window of n messages an offset asks for: negative counts
// from the end (-40 is the latest page).
func pageOf(n, offset int) (start, end int) {
	if offset < 0 {
		start = max(0, n+offset)
	} else {
		start = min(offset, n)
	}
	return start, min(start+msgPage, n)
}

// refreshes says whether a page reloads itself: the sessions page always,
// a session's page while it waits on the user or has a prompt held (so its
// composer, hidden then, is never wiped mid-message).
func refreshes(name string, body any) bool {
	switch name {
	case "triage":
		return true
	case "session":
		v, _ := body.(sessionView)
		return v.Waiting()
	}
	return false
}

// Waiting: a prompt is held for the page, or the session waits on a menu.
func (v sessionView) Waiting() bool { return len(v.Prompts) > 0 || v.Status == "waiting" }

// The installable bits: a manifest and icons, served through the same
// checks as every page (Access, or the preview's path).

const iconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><rect width="100" height="100" rx="22" fill="#2563eb"/><path d="M30 33 L50 50 L30 67" fill="none" stroke="#fff" stroke-width="9" stroke-linecap="round" stroke-linejoin="round"/><rect x="55" y="60" width="18" height="8" rx="4" fill="#fff"/></svg>`

func (s *server) manifest(w http.ResponseWriter, r *http.Request) {
	base := s.base()
	icons := []map[string]string{{"src": base + "/icon.svg", "sizes": "any", "type": "image/svg+xml"}}
	for _, n := range []int{192, 512} {
		icons = append(icons, map[string]string{"src": base + "/icon/" + strconv.Itoa(n), "sizes": strconv.Itoa(n) + "x" + strconv.Itoa(n), "type": "image/png"})
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	json.NewEncoder(w).Encode(map[string]any{
		"name": "ccs", "short_name": "ccs", "description": "Your Claude Code and Codex sessions",
		"start_url": base + "/", "scope": base + "/", "display": "standalone",
		"background_color": "#0f1115", "theme_color": "#0f1115", "icons": icons,
	})
}

func (s *server) iconSVG(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Write([]byte(iconSVG))
}

var iconPNGs sync.Map // size -> []byte

// iconPNG draws the icon (the SVG's shapes) at 180, 192 or 512 pixels
// (/icon/<size>):
// home screens want a PNG.
func (s *server) iconPNG(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("size"))
	if err != nil || (n != 180 && n != 192 && n != 512) {
		http.NotFound(w, r)
		return
	}
	b, ok := iconPNGs.Load(n)
	if !ok {
		b = drawIcon(n)
		iconPNGs.Store(n, b)
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(b.([]byte))
}

func drawIcon(n int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	bg := color.RGBA{0x25, 0x63, 0xeb, 0xff}
	fg := color.RGBA{0xff, 0xff, 0xff, 0xff}
	u := float64(n) / 100
	seg := func(px, py, ax, ay, bx, by float64) float64 { // distance from p to segment ab
		dx, dy := bx-ax, by-ay
		t := math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/(dx*dx+dy*dy)))
		return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px, py := (float64(x)+.5)/u, (float64(y)+.5)/u
			// rounded square, r 22
			cx, cy := math.Max(22, math.Min(78, px)), math.Max(22, math.Min(78, py))
			if math.Hypot(px-cx, py-cy) > 22 {
				continue
			}
			c := bg
			if math.Min(seg(px, py, 30, 33, 50, 50), seg(px, py, 50, 50, 30, 67)) <= 4.5 ||
				seg(px, py, 59, 64, 69, 64) <= 4 {
				c = fg
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

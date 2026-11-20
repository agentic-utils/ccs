package sessions

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestSlotOrder(t *testing.T) {
	rank := map[string]int{"a": 0, "b": 1, "c": 2}
	cases := []struct {
		name string
		tabs [][]string
		want []int
	}{
		{"already in order", [][]string{{"a"}, {"b"}, {"c"}}, nil},
		{"reversed", [][]string{{"c"}, {"b"}, {"a"}}, []int{2, 1, 0}},
		// ccs's own tab (x) and a shell (y) keep their places; the sessions
		// move among the places sessions hold.
		{"others stay put", [][]string{{"x"}, {"c"}, {"y"}, {"a"}}, []int{0, 3, 2, 1}},
		{"no listed tab", [][]string{{"x"}, {"y"}}, nil},
		// a tab with two panes goes where its earliest listed session would
		{"split tab", [][]string{{"c"}, {"x", "b"}, {"a"}}, []int{2, 1, 0}},
	}
	for _, c := range cases {
		if got := slotOrder(c.tabs, rank); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

const panes = `$0 @0 1 /dev/ttys001
$0 @3 0 /dev/ttys004
$0 @3 0 /dev/ttys005
$0 @1 0 /dev/ttys002
$1 @2 1 /dev/ttys003
`

func TestTmuxSessions(t *testing.T) {
	got := tmuxSessions(panes)
	if len(got) != 2 || len(got[0]) != 3 || len(got[1]) != 1 {
		t.Fatalf("got %+v", got)
	}
	if w := got[0][1]; w.id != "@3" || w.active || !slices.Equal(w.ttys, []string{"/dev/ttys004", "/dev/ttys005"}) {
		t.Errorf("split window: %+v", w)
	}
	if !got[0][0].active {
		t.Errorf("@0 is current: %+v", got[0][0])
	}
}

func TestTmuxSwaps(t *testing.T) {
	wins := []tmuxWindow{{id: "@0"}, {id: "@1"}, {id: "@2"}}
	got := tmuxSwaps(wins, []int{2, 0, 1})
	want := [][2]string{{"@2", "@0"}, {"@0", "@1"}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestOrderTmux(t *testing.T) {
	defer func(f func(...string) (string, error)) { TmuxRun = f }(TmuxRun)
	var ran []string
	TmuxRun = func(args ...string) (string, error) {
		if args[0] == "list-panes" {
			return panes, nil
		}
		ran = append(ran, strings.Join(args, " "))
		return "", nil
	}
	t.Setenv("TMUX", "/tmp/tmux-1/default,1,0")
	// list order: ttys002's window, then the split one; ccs is on ttys001
	n, err := OrderTabs([]string{"/dev/ttys002", "/dev/ttys005", "/dev/ttys003"})
	if err != nil || n != 2 {
		t.Fatalf("moved %d, %v", n, err)
	}
	want := []string{"swap-window -d -s @1 -t @3", "select-window -t @0"}
	if !slices.Equal(ran, want) {
		t.Errorf("ran %q, want %q", ran, want)
	}
}

func TestOrderTabsNeitherTerminal(t *testing.T) {
	if _, err := OrderTabs([]string{"/dev/ttys001"}); err == nil {
		t.Error("no tmux or iTerm: want an error")
	}
}

type fakeITerm struct {
	wins      []iTermWindow
	reordered map[string][]string
}

func (f *fakeITerm) Windows() ([]iTermWindow, error) { return f.wins, nil }
func (f *fakeITerm) ReorderTabs(w string, ids []string) error {
	f.reordered[w] = ids
	return nil
}
func (f *fakeITerm) Close() error { return nil }

func TestOrderITerm(t *testing.T) {
	defer func(a func() (iTermAPI, error), s func() (map[string]string, error)) {
		ITermAPI, ITermSessionTTYs = a, s
	}(ITermAPI, ITermSessionTTYs)
	fake := &fakeITerm{reordered: map[string][]string{}, wins: []iTermWindow{
		{ID: "w1", Tabs: []iTermTab{{"1", []string{"ccs"}}, {"2", []string{"s-b"}}, {"3", []string{"s-a"}}}},
		{ID: "w2", Tabs: []iTermTab{{"4", []string{"s-c"}}, {"5", []string{"shell"}}}},
	}}
	ITermAPI = func() (iTermAPI, error) { return fake, nil }
	ITermSessionTTYs = func() (map[string]string, error) {
		return map[string]string{"ccs": "/dev/ttys000", "s-a": "/dev/ttys001", "s-b": "/dev/ttys002", "s-c": "/dev/ttys003", "shell": "/dev/ttys009"}, nil
	}
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	n, err := OrderTabs([]string{"/dev/ttys001", "/dev/ttys002", "/dev/ttys003"})
	if err != nil || n != 2 {
		t.Fatalf("moved %d, %v", n, err)
	}
	if got := fake.reordered["w1"]; !slices.Equal(got, []string{"1", "3", "2"}) {
		t.Errorf("w1: %v", got)
	}
	if _, ok := fake.reordered["w2"]; ok {
		t.Error("w2 was already in order")
	}
}

func TestOrderITermAPIOff(t *testing.T) {
	defer func(a func() (iTermAPI, error)) { ITermAPI = a }(ITermAPI)
	ITermAPI = func() (iTermAPI, error) { return nil, errITermAPIOff }
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if _, err := OrderTabs([]string{"/dev/ttys001"}); !errors.Is(err, errITermAPIOff) {
		t.Errorf("got %v", err)
	}
}

// listSessionsResponse builds a ListSessionsResponse: one window, its first
// tab split in two (one pane nested a level down), its second minimized.
func listSessionsResponse() []byte {
	summary := func(id string) []byte { return pbBytes(nil, 1, []byte(id)) }
	link := func(field int, data []byte) []byte { return pbBytes(nil, 2, pbBytes(nil, field, data)) }
	inner := link(1, summary("s2"))
	root := append(link(1, summary("s1")), link(2, inner)...)
	tab1 := append(pbBytes(nil, 2, []byte("10")), pbBytes(nil, 3, root)...)
	tab2 := append(pbBytes(nil, 2, []byte("11")), pbBytes(nil, 6, summary("s3"))...)
	win := append(pbBytes(nil, 1, tab1), pbBytes(nil, 1, tab2)...)
	win = append(win, pbBytes(nil, 2, []byte("pty-W"))...)
	win = pbVarint(win, 4, 7) // number, skipped
	return pbBytes(nil, 1, win)
}

func TestParseITermWindows(t *testing.T) {
	wins, err := parseITermWindows(listSessionsResponse())
	if err != nil {
		t.Fatal(err)
	}
	if len(wins) != 1 || wins[0].ID != "pty-W" || len(wins[0].Tabs) != 2 {
		t.Fatalf("got %+v", wins)
	}
	if tab := wins[0].Tabs[0]; tab.ID != "10" || !slices.Equal(tab.Sessions, []string{"s1", "s2"}) {
		t.Errorf("tab 1: %+v", tab)
	}
	if tab := wins[0].Tabs[1]; tab.ID != "11" || !slices.Equal(tab.Sessions, []string{"s3"}) {
		t.Errorf("tab 2: %+v", tab)
	}
	if _, err := parseITermWindows([]byte{0x0a, 0x05, 0x01}); err == nil {
		t.Error("truncated message: want an error")
	}
}

// TestITermConn runs the handshake, a list and a reorder against a fake
// iTerm on a pipe.
func TestITermConn(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		r := bufio.NewReader(server)
		req, err := http.ReadRequest(r)
		if err != nil {
			done <- err
			return
		}
		if req.Header.Get("X-Iterm2-Cookie") != "C" || req.Header.Get("X-Iterm2-Key") != "K" || req.Header.Get("Sec-WebSocket-Protocol") != "api.iterm2.com" {
			done <- errors.New("bad headers: " + strings.Join(req.Header.Values("X-Iterm2-Cookie"), ","))
			return
		}
		server.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		ws := &iTermConn{c: server, r: r}
		answer := func(field int, body []byte) {
			msg, err := ws.readMessage() // client frames are masked; readMessage unmasks
			if err != nil {
				done <- err
				return
			}
			var id uint64
			pbEach(msg, func(num int, v uint64, _ []byte) {
				if num == 1 {
					id = v
				}
			})
			ws.writeFrame(0x9, []byte("ping")) // the client must answer and carry on
			pong := make([]byte, 10)           // header, mask and "ping"
			if _, err := io.ReadFull(r, pong); err != nil || pong[0] != 0x8a {
				done <- errors.New("no pong")
				return
			}
			ws.writeFrame(0x2, pbBytes(pbVarint(nil, 1, id+100), field, nil)) // someone else's answer
			ws.writeFrame(0x2, pbBytes(pbVarint(nil, 1, id), field, body))
		}
		answer(pbListSessions, listSessionsResponse())
		msg, err := ws.readMessage()
		if err != nil {
			done <- err
			return
		}
		var req125 []byte
		pbEach(msg, func(num int, _ uint64, data []byte) {
			if num == pbReorderTabs {
				req125 = data
			}
		})
		if string(req125) != string(reorderTabsRequest("pty-W", []string{"11", "10"})) {
			done <- errors.New("reorder request differs")
			return
		}
		ws.writeFrame(0x2, pbBytes(pbVarint(nil, 1, 2), pbReorderTabs, pbVarint(nil, 4, 3)))
		done <- nil
	}()
	ic, err := iTermHandshake(client, "C", "K")
	if err != nil {
		t.Fatal(err)
	}
	wins, err := ic.Windows()
	if err != nil || len(wins) != 1 || wins[0].ID != "pty-W" {
		t.Fatalf("windows %+v, %v", wins, err)
	}
	if err := ic.ReorderTabs("pty-W", []string{"11", "10"}); err == nil || !strings.Contains(err.Error(), "invalid tab id") {
		t.Errorf("status 3: got %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestITermHandshakeRefused(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		defer server.Close()
		http.ReadRequest(bufio.NewReader(server))
		server.Write([]byte("HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n"))
	}()
	if _, err := iTermHandshake(client, "C", "K"); !errors.Is(err, errITermAPIOff) {
		t.Errorf("got %v", err)
	}
}

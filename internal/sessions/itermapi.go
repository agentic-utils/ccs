package sessions

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-utils/ccs/internal/proc"
)

// iTerm's API (the one its Python library speaks): a websocket on a unix
// socket, protobuf messages from iTerm's api.proto. ccs speaks the few it
// needs itself, so it needs neither Python nor new dependencies. It works only
// with Settings > General > Magic > Enable Python API on.

// ITermAPISocket is where iTerm listens for API connections.
var ITermAPISocket = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "iTerm2", "private", "socket")
}

// ITermCookie asks iTerm for an API cookie and key over AppleScript, which
// iTerm trusts without asking (the way its own scripts get one).
var ITermCookie = func() (cookie, key string, err error) {
	out, err := proc.RunBounded(5*time.Second, nil, "osascript", "-e", `tell application "iTerm2" to request cookie and key for app named "ccs"`)
	if err != nil {
		return "", "", iTermError(err)
	}
	cookie, key, _ = strings.Cut(strings.TrimSpace(string(out)), " ")
	return cookie, key, nil
}

// errITermAPIOff says what to turn on when iTerm refuses or isn't listening.
var errITermAPIOff = errors.New("iTerm's API is off: turn on iTerm > Settings > General > Magic > Enable Python API")

type iTermConn struct {
	c    net.Conn
	r    *bufio.Reader
	next int64
}

func dialITermAPI() (*iTermConn, error) {
	cookie, key, err := ITermCookie()
	if err != nil {
		return nil, err
	}
	c, err := net.DialTimeout("unix", ITermAPISocket(), 2*time.Second)
	if err != nil {
		return nil, errITermAPIOff
	}
	ic, err := iTermHandshake(c, cookie, key)
	if err != nil {
		c.Close()
		return nil, err
	}
	return ic, nil
}

// iTermHandshake upgrades c to the API's websocket. Every read and write on
// it has a deadline, so a stuck iTerm can't hang ccs.
func iTermHandshake(c net.Conn, cookie, key string) (*iTermConn, error) {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	nonce := make([]byte, 16)
	rand.Read(nonce)
	req, _ := http.NewRequest("GET", "ws://localhost/", nil)
	req.Header = http.Header{
		"Upgrade":                  {"websocket"},
		"Connection":               {"Upgrade"},
		"Sec-WebSocket-Key":        {base64.StdEncoding.EncodeToString(nonce)},
		"Sec-WebSocket-Version":    {"13"},
		"Sec-WebSocket-Protocol":   {"api.iterm2.com"},
		"Origin":                   {"ws://localhost/"},
		"X-Iterm2-Library-Version": {"python 2.30"}, // iTerm gates features on the Python library's version
		"X-Iterm2-Disable-Auth-Ui": {"true"},
		"X-Iterm2-Advisory-Name":   {"ccs"},
		"X-Iterm2-Cookie":          {cookie},
		"X-Iterm2-Key":             {key},
	}
	if err := req.Write(c); err != nil {
		return nil, apiErr(err)
	}
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		return nil, apiErr(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, errITermAPIOff
		}
		return nil, fmt.Errorf("iTerm API: %s", resp.Status)
	}
	return &iTermConn{c: c, r: r}, nil
}

func (ic *iTermConn) Close() error { return ic.c.Close() }

// apiErr says what went wrong talking to iTerm without the socket's path,
// which a *net.OpError would otherwise put in ccs's error row.
func apiErr(err error) error {
	var op *net.OpError
	if errors.As(err, &op) {
		return fmt.Errorf("iTerm API: %s: %v", op.Op, op.Err)
	}
	return fmt.Errorf("iTerm API: %v", err)
}

// writeFrame sends one masked binary frame, as a websocket client must.
func (ic *iTermConn) writeFrame(op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = binary.BigEndian.AppendUint64(append(hdr, 0x80|127), uint64(n))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr = append(hdr, mask...)
	body := make([]byte, len(payload))
	for i, b := range payload {
		body[i] = b ^ mask[i%4]
	}
	_, err := ic.c.Write(append(hdr, body...))
	return err
}

// readMessage reads one whole message, answering pings on the way.
func (ic *iTermConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(ic.r, h[:]); err != nil {
			return nil, err
		}
		fin, op, masked := h[0]&0x80 != 0, h[0]&0x0f, h[1]&0x80 != 0
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(ic.r, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(ic.r, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 64<<20 {
			return nil, errors.New("iTerm API: message too large")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(ic.r, mask[:]); err != nil {
				return nil, err
			}
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(ic.r, body); err != nil {
			return nil, err
		}
		if masked {
			for i := range body {
				body[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8:
			return nil, errors.New("iTerm API: connection closed")
		case 0x9:
			if err := ic.writeFrame(0xA, body); err != nil {
				return nil, err
			}
			continue
		case 0xA:
			continue
		}
		msg = append(msg, body...)
		if fin {
			return msg, nil
		}
	}
}

// call sends a ClientOriginatedMessage with the request in field, and
// returns the answer's field of the same number.
func (ic *iTermConn) call(field int, req []byte) ([]byte, error) {
	ic.next++
	id := ic.next
	ic.c.SetDeadline(time.Now().Add(5 * time.Second))
	msg := pbVarint(nil, 1, uint64(id))
	msg = pbBytes(msg, field, req)
	if err := ic.writeFrame(0x2, msg); err != nil {
		return nil, apiErr(err)
	}
	for {
		b, err := ic.readMessage()
		if err != nil {
			return nil, apiErr(err)
		}
		var gotID int64
		var answer []byte
		var errText string
		err = pbEach(b, func(num int, v uint64, data []byte) {
			switch num {
			case 1:
				gotID = int64(v)
			case 2:
				errText = string(data)
			case field:
				answer = data
			}
		})
		if err != nil {
			return nil, err
		}
		if gotID != id {
			continue // a notification, or an answer to something else
		}
		if errText != "" {
			return nil, fmt.Errorf("iTerm API: %s", errText)
		}
		if answer == nil {
			answer = []byte{}
		}
		return answer, nil
	}
}

const (
	pbListSessions = 106 // ClientOriginatedMessage.list_sessions_request / ServerOriginatedMessage.list_sessions_response
	pbReorderTabs  = 125 // .reorder_tabs_request / .reorder_tabs_response
)

// Windows is ListSessionsRequest: each window's id and its tabs, in order.
func (ic *iTermConn) Windows() ([]iTermWindow, error) {
	b, err := ic.call(pbListSessions, nil)
	if err != nil {
		return nil, err
	}
	return parseITermWindows(b)
}

// parseITermWindows reads a ListSessionsResponse: windows (1), each with tabs
// (1) and window_id (2); a tab has tab_id (2), a split tree root (3),
// minimized_sessions (6) and floating_panes (12).
func parseITermWindows(b []byte) ([]iTermWindow, error) {
	var wins []iTermWindow
	var perr error
	err := pbEach(b, func(num int, _ uint64, data []byte) {
		if num != 1 || perr != nil {
			return
		}
		var w iTermWindow
		perr = pbEach(data, func(num int, _ uint64, data []byte) {
			switch num {
			case 2:
				w.ID = string(data)
			case 1:
				var t iTermTab
				err := pbEach(data, func(num int, _ uint64, data []byte) {
					switch num {
					case 2:
						t.ID = string(data)
					case 3:
						t.Sessions = append(t.Sessions, splitTreeSessions(data)...)
					case 6:
						t.Sessions = append(t.Sessions, summaryID(data))
					case 12: // floating_panes: FloatingPane, its session (1)
						pbEach(data, func(num int, _ uint64, data []byte) {
							if num == 1 {
								t.Sessions = append(t.Sessions, summaryID(data))
							}
						})
					}
				})
				if err != nil && perr == nil {
					perr = err
				}
				w.Tabs = append(w.Tabs, t)
			}
		})
		wins = append(wins, w)
	})
	if err == nil {
		err = perr
	}
	return wins, err
}

// splitTreeSessions is every session in a SplitTreeNode: links (2), each a
// session (1) or another node (2).
func splitTreeSessions(b []byte) []string {
	var ids []string
	pbEach(b, func(num int, _ uint64, data []byte) {
		if num != 2 {
			return
		}
		pbEach(data, func(num int, _ uint64, data []byte) {
			switch num {
			case 1:
				ids = append(ids, summaryID(data))
			case 2:
				ids = append(ids, splitTreeSessions(data)...)
			}
		})
	})
	return ids
}

// summaryID is a SessionSummary's unique_identifier (1).
func summaryID(b []byte) string {
	id := ""
	pbEach(b, func(num int, _ uint64, data []byte) {
		if num == 1 {
			id = string(data)
		}
	})
	return id
}

// iTermReorderStatus names ReorderTabsResponse.Status.
var iTermReorderStatus = map[uint64]string{1: "invalid assignment", 2: "invalid window id", 3: "invalid tab id"}

// ReorderTabs is ReorderTabsRequest with one Assignment (3): window_id (1)
// and every tab of the window in its new order (2).
func (ic *iTermConn) ReorderTabs(windowID string, tabIDs []string) error {
	b, err := ic.call(pbReorderTabs, reorderTabsRequest(windowID, tabIDs))
	if err != nil {
		return err
	}
	var status uint64
	if err := pbEach(b, func(num int, v uint64, _ []byte) {
		if num == 4 {
			status = v
		}
	}); err != nil {
		return err
	}
	if status != 0 {
		return fmt.Errorf("iTerm API: reorder tabs: %s", iTermReorderStatus[status])
	}
	return nil
}

func reorderTabsRequest(windowID string, tabIDs []string) []byte {
	a := pbBytes(nil, 1, []byte(windowID))
	for _, t := range tabIDs {
		a = pbBytes(a, 2, []byte(t))
	}
	return pbBytes(nil, 3, a)
}

// pbVarint appends field num as a varint.
func pbVarint(b []byte, num int, v uint64) []byte {
	b = binary.AppendUvarint(b, uint64(num)<<3)
	return binary.AppendUvarint(b, v)
}

// pbBytes appends field num as length-delimited bytes.
func pbBytes(b []byte, num int, data []byte) []byte {
	b = binary.AppendUvarint(b, uint64(num)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(data)))
	return append(b, data...)
}

// pbEach calls fn for each field of a protobuf message: a varint's value, or
// a length-delimited field's bytes. Fixed-width fields are skipped.
func pbEach(b []byte, fn func(num int, v uint64, data []byte)) error {
	bad := errors.New("iTerm API: malformed message")
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return bad
		}
		b = b[n:]
		num := int(tag >> 3)
		switch tag & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return bad
			}
			b = b[n:]
			fn(num, v, nil)
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return bad
			}
			fn(num, 0, b[n:n+int(l)])
			b = b[n+int(l):]
		case 1:
			if len(b) < 8 {
				return bad
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return bad
			}
			b = b[4:]
		default:
			return bad
		}
	}
	return nil
}

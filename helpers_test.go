package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeRelease serves tag's checksums.txt and this platform's archive holding
// binary as "ccs"; tamper corrupts the published checksum.
func fakeRelease(t *testing.T, tag string, binary []byte, tamper bool) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{"README.md": []byte("readme"), "ccs": binary} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	gz.Close()
	asset := fmt.Sprintf("ccs_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(buf.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	if tamper {
		sums = strings.Repeat("0", 64) + "  " + asset + "\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tag + "/checksums.txt":
			io.WriteString(w, sums)
		case "/" + tag + "/" + asset:
			w.Write(buf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := releaseDownloadURL
	releaseDownloadURL = srv.URL
	t.Cleanup(func() { releaseDownloadURL = old })
}

// firstOfBatch runs the first command of a tea.Batch (the upgrade itself;
// the rest is the 1s redraw tick, which would just sleep).
func firstOfBatch(cmd tea.Cmd) tea.Msg {
	if batch, ok := cmd().(tea.BatchMsg); ok {
		return batch[0]()
	}
	return cmd()
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestMain(m *testing.M) {
	// Keep tests from writing into the real update log.
	dir, _ := os.MkdirTemp("", "ccs-test-log")
	updateLogPath = filepath.Join(dir, "update.log")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func usageTestLine(id, reqID string, ts time.Time, in, create, c5, c1, read, out int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":%q,"message":{"id":%q,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}}}`,
		ts.UTC().Format(time.RFC3339), reqID, id, in, create, read, out, c5, c1) + "\n"
}

func mouseModel(t *testing.T) model {
	t.Helper()
	var convs []Conversation
	for i := 0; i < 10; i++ {
		c := Conversation{SessionID: fmt.Sprint("s", i), LastTimestamp: fmt.Sprintf("2026-09-29T10:%02d:00Z", 59-i)}
		for j := 0; j < 40; j++ {
			c.Messages = append(c.Messages, Message{Role: "user", Text: fmt.Sprintf("message %d", j)})
		}
		convs = append(convs, c)
	}
	m := initialModel(buildItems(convs), "", nil)
	m.width, m.height = 120, 40
	return m
}

func strip2(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// chatModel has two conversations, the second live (pid of this process).
func chatModel(t *testing.T) model {
	t.Helper()
	dir := t.TempDir()
	old := getSessionsDir
	getSessionsDir = func() string { return dir }
	t.Cleanup(func() { getSessionsDir = old })
	items := buildItems([]Conversation{
		{SessionID: "idle", Title: "Quiet one", LastTimestamp: "2026-09-29T11:00:00Z", Messages: []Message{{Role: "user", Text: "hello"}}},
		{SessionID: "live", Title: "Busy one", LastTimestamp: "2026-09-29T10:00:00Z", Messages: []Message{{Role: "user", Text: "hi"}}},
	})
	m := initialModel(items, "", nil)
	m.width, m.height = 120, 40
	m.live = map[string]bool{"live": true}
	m.livePIDs = map[string]int{"live": os.Getpid()}
	return m
}

func key(m model, k tea.KeyMsg) (model, tea.Cmd) {
	res, cmd := m.Update(k)
	return res.(model), cmd
}

// fakeSocket listens on a unix socket in a temp dir and returns what one
// connection sent.
func fakeSocket(t *testing.T) (path string, got chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ccs-sock") // unix socket paths must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path = filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		data, _ := io.ReadAll(c) // until the sender closes its write side
		c.Close()
		got <- string(data)
	}()
	return path, got
}

// fakeTerminal puts fake ps and tmux on PATH; tmux logs what it's asked.
func fakeTerminal(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "tmux.log")
	os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\necho ttys099\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nif [ \"$1\" = list-panes ]; then echo '/dev/ttys099 main:0.1'; exit 0; fi\nprintf '%s|' \"$@\" >> "+log+"\necho >> "+log+"\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TMUX", "fake")
	return log
}

// fakeCswap puts a fake cswap on PATH (and nothing else from the user's
// PATH, so the real one can never run). It prints canned list JSON, records
// every call's arguments, and fails "switch" when CSWAP_FAIL is set.
func fakeCswap(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls")
	list := `{"schemaVersion":1,"activeAccountNumber":1,"accounts":[` +
		`{"number":1,"email":"me@work.example","organizationName":"Work","active":true,"usageStatus":"unavailable","usage":null,"lastGoodUsage":{"fiveHour":{"pct":21},"sevenDay":{"pct":24}}},` +
		`{"number":2,"email":"me@home.example","organizationName":"Home","active":false,"usageStatus":"ok","usage":{"fiveHour":{"pct":4},"sevenDay":{"pct":10}}},` +
		`{"number":3,"email":"old@example.com","organizationName":"Old","active":false,"usageStatus":"relogin_required"}]}`
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n" +
		"case \"$1\" in\n" +
		"  list) echo '" + list + "' ;;\n" +
		"  switch) if [ -n \"$CSWAP_FAIL\" ]; then echo 'Error: account 2 needs re-login' >&2; exit 1; fi ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "cswap"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	return logPath
}

// runAcct feeds a key to the model and runs any command it returns (and the
// ones those return), like bubbletea would, skipping the allowance fetch.
func runAcct(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	res, cmd := m.Update(msg)
	m = res.(model)
	for cmd != nil {
		out := cmd()
		if b, ok := out.(tea.BatchMsg); ok {
			cmd = nil
			for _, c := range b {
				if c == nil {
					continue
				}
				if r := c(); r != nil {
					if _, isAllow := r.(allowanceMsg); isAllow {
						continue
					}
					res, next := m.Update(r)
					m = res.(model)
					if next != nil {
						cmd = next
					}
				}
			}
			continue
		}
		if out == nil {
			break
		}
		res, cmd = m.Update(out)
		m = res.(model)
	}
	return m
}

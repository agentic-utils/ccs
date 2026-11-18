package mcp

// A QR code of ccs serve's address, for a phone's camera: printed when ccs
// serve starts (not --preview, whose 127.0.0.1 link means the phone itself),
// by `ccs serve qr`, and in the TUI's ^X B popup.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the light border around the code, in modules: scanners need
// one, and the terminal around it may be dark.
const qrQuiet = 2

// QRCode is url as a terminal QR code: two modules a row, drawn with ▀ in
// explicit colours (dark modules black on white), so it reads the same on a
// dark or a light terminal. width is its width in cells.
func QRCode(url string) (text string, width int, err error) {
	c, err := qr.Encode(url, qr.M)
	if err != nil {
		return "", 0, err
	}
	m := make([][]bool, c.Size)
	for y := range m {
		m[y] = make([]bool, c.Size)
		for x := range m[y] {
			m[y][x] = c.Black(x, y)
		}
	}
	text = renderQR(m)
	return text, c.Size + 2*qrQuiet, nil
}

// renderQR draws a module matrix (true dark) with a qrQuiet border, two
// rows a line: each cell is ▀ with the top module's colour in front and the
// bottom one's behind.
func renderQR(m [][]bool) string {
	n := len(m)
	dark := func(x, y int) bool {
		x, y = x-qrQuiet, y-qrQuiet
		return x >= 0 && y >= 0 && x < n && y < n && m[y][x]
	}
	colour := func(d bool) string {
		if d {
			return "16" // black
		}
		return "231" // white
	}
	var b strings.Builder
	side := n + 2*qrQuiet
	for y := 0; y < side; y += 2 {
		for x := 0; x < side; x++ {
			fmt.Fprintf(&b, "\033[38;5;%s;48;5;%sm▀", colour(dark(x, y)), colour(dark(x, y+1)))
		}
		b.WriteString("\033[0m\n")
	}
	return b.String()
}

// ServeURL is the address the installed agent serves (its --host), from
// its plist.
func ServeURL() (string, error) {
	raw, err := os.ReadFile(ServePlistPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("ccs serve isn't installed: ccs serve install <flags> sets it up (docs/remote.md)")
	} else if err != nil {
		return "", err
	}
	host := plistHost(raw)
	if host == "" {
		return "", errors.New("the agent runs ccs serve --preview, which only this Mac can open: there's no address for a phone")
	}
	return "https://" + host + "/", nil
}

// plistHost is the --host among a plist's ProgramArguments, "" if none.
func plistHost(raw []byte) string {
	d := xml.NewDecoder(strings.NewReader(string(raw)))
	var args []string
	inArgs, key := false, ""
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var k string
				d.DecodeElement(&k, &t)
				key = k
			case "array":
				inArgs = key == "ProgramArguments"
			case "string":
				var v string
				d.DecodeElement(&v, &t)
				if inArgs {
					args = append(args, v)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				inArgs = false
			}
		}
	}
	for i, a := range args {
		if (a == "--host" || a == "-host") && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--host="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(a, "-host="); ok {
			return v
		}
	}
	return ""
}

// printQR writes url's QR code and the address under it.
func printQR(w interface{ Write([]byte) (int, error) }, url string) {
	text, _, err := QRCode(url)
	if err != nil {
		fmt.Fprintf(w, "(no QR code: %v)\n%s\n", err, url)
		return
	}
	fmt.Fprintf(w, "%sScan it with your phone's camera, or open %s\n", text, url)
}

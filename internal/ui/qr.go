package ui

import (
	"fmt"
	"html/template"
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the empty margin a QR code needs around it, in modules.
const qrQuiet = 4

// qrMatrix encodes text as a QR code (error correction M) and returns
// its rows, '#' for a dark module, without the quiet zone.
func qrMatrix(text string) ([]string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, err
	}
	rows := make([]string, c.Size)
	for y := range rows {
		var b strings.Builder
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				b.WriteByte('#')
			} else {
				b.WriteByte('.')
			}
		}
		rows[y] = b.String()
	}
	return rows, nil
}

// qrSVG draws text as a QR code in inline SVG: dark on white in both
// themes, as scanners expect, with its quiet zone. It is drawn into the
// page itself, never served from a URL, since it holds a secret.
func qrSVG(text string) (template.HTML, error) {
	rows, err := qrMatrix(text)
	if err != nil {
		return "", err
	}
	n := len(rows) + 2*qrQuiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="qr" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code for your authenticator app"><rect width="%d" height="%d" fill="#fff"/><path fill="#0F172A" d="`, n, n, n, n)
	for y, row := range rows {
		for x := 0; x < len(row); {
			if row[x] != '#' {
				x++
				continue
			}
			run := 1
			for x+run < len(row) && row[x+run] == '#' {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", x+qrQuiet, y+qrQuiet, run, run)
			x += run
		}
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String()), nil //nolint:gosec // built here from numbers only
}

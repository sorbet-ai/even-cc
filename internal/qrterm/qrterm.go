// Package qrterm renders a QR code for a terminal with half-block characters.
package qrterm

import (
	"strings"

	"rsc.io/qr"
)

// Render returns the QR code for text, two modules per character row, with
// a quiet zone. Dark modules are drawn as spaces on a light background so it
// scans on dark and light terminal themes alike.
func Render(text string) (string, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return "", err
	}
	const quiet = 2
	n := code.Size
	dark := func(x, y int) bool {
		if x < 0 || y < 0 || x >= n || y >= n {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < n+quiet; y += 2 {
		b.WriteString("  ")
		for x := -quiet; x < n+quiet; x++ {
			top, bottom := dark(x, y), dark(x, y+1)
			switch {
			case !top && !bottom:
				b.WriteString("█")
			case top && !bottom:
				b.WriteString("▄")
			case !top && bottom:
				b.WriteString("▀")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

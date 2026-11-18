package mirror

import (
	"fmt"
	"strings"

	"github.com/hinshun/vt10x"
)

// vt10x's glyph attribute bits (unexported there).
const (
	attrReverse   = 1 << 0
	attrUnderline = 1 << 1
	attrBold      = 1 << 2
	attrItalic    = 1 << 4
	attrBlink     = 1 << 5
)

// renderScreen repaints the model's screen as terminal bytes: clear, every
// cell with its attributes, then input modes, cursor position and
// visibility. A client resets its emulator and applies this, then follows
// the output stream.
func renderScreen(t vt10x.Terminal, bracketed bool) []byte {
	t.Lock()
	defer t.Unlock()
	cols, rows := t.Size()
	mode := t.Mode()
	var b strings.Builder
	b.WriteString("\x1b[0m")
	if mode&vt10x.ModeAltScreen != 0 {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[H\x1b[2J")
	for y := 0; y < rows; y++ {
		// trailing default blanks are left to the clear
		last := -1
		for x := cols - 1; x >= 0; x-- {
			g := t.Cell(x, y)
			if (g.Char != ' ' && g.Char != 0) || g.BG != vt10x.DefaultBG || g.Mode&attrReverse != 0 {
				last = x
				break
			}
		}
		if last < 0 {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
		var cur vt10x.Glyph
		cur.FG, cur.BG = vt10x.DefaultFG, vt10x.DefaultBG
		for x := 0; x <= last; x++ {
			g := t.Cell(x, y)
			if g.Mode != cur.Mode || g.FG != cur.FG || g.BG != cur.BG {
				b.WriteString(sgr(g))
				cur = g
			}
			if g.Char == 0 {
				b.WriteByte(' ')
			} else {
				b.WriteRune(g.Char)
			}
		}
		b.WriteString("\x1b[0m")
	}
	if mode&vt10x.ModeAppCursor != 0 {
		b.WriteString("\x1b[?1h")
	}
	if mode&vt10x.ModeAppKeypad != 0 {
		b.WriteString("\x1b=")
	}
	if bracketed {
		b.WriteString("\x1b[?2004h")
	}
	c := t.Cursor()
	fmt.Fprintf(&b, "\x1b[%d;%dH", c.Y+1, c.X+1)
	if t.CursorVisible() {
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	return []byte(b.String())
}

func sgr(g vt10x.Glyph) string {
	p := []string{"0"}
	if g.Mode&attrBold != 0 {
		p = append(p, "1")
	}
	if g.Mode&attrItalic != 0 {
		p = append(p, "3")
	}
	if g.Mode&attrUnderline != 0 {
		p = append(p, "4")
	}
	if g.Mode&attrBlink != 0 {
		p = append(p, "5")
	}
	if g.Mode&attrReverse != 0 {
		p = append(p, "7")
	}
	p = append(p, color(g.FG, false)...)
	p = append(p, color(g.BG, true)...)
	return "\x1b[" + strings.Join(p, ";") + "m"
}

// color maps a vt10x color to SGR parameters. vt10x keeps truecolor as
// 0xRRGGBB in the same field as palette indexes, so values below 256 are
// read as the palette.
func color(c vt10x.Color, bg bool) []string {
	base := 30
	if bg {
		base = 40
	}
	switch {
	case c == vt10x.DefaultFG || c == vt10x.DefaultBG || c == vt10x.DefaultCursor:
		return nil
	case c < 8:
		return []string{fmt.Sprint(base + int(c))}
	case c < 16:
		return []string{fmt.Sprint(base + 60 + int(c) - 8)}
	case c < 256:
		return []string{fmt.Sprint(base + 8), "5", fmt.Sprint(int(c))}
	default:
		return []string{fmt.Sprint(base + 8), "2", fmt.Sprint(int(c>>16) & 0xff), fmt.Sprint(int(c>>8) & 0xff), fmt.Sprint(int(c) & 0xff)}
	}
}

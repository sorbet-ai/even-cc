// Package jsonx encodes JSON the way Node's JSON.stringify does (no HTML
// escaping, raw U+2028/U+2029) and handles key-order-preserving values.
package jsonx

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// Marshal encodes v like JSON.stringify: compact, no HTML escaping, and
// U+2028/U+2029 left unescaped.
func Marshal(v any) []byte {
	b, err := Encode(v)
	if err != nil {
		// Only unsupported values (channels, NaN) end up here; callers that
		// must not send `null` use Encode.
		return []byte(`null`)
	}
	return b
}

// Encode is Marshal with the encoding error.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return unescapeLineSeparators(b), nil
}

// unescapeLineSeparators turns   and   escapes back into raw
// characters, skipping escaped backslashes (\\u2028 stays literal).
func unescapeLineSeparators(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u202`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != '\\' || i+1 >= len(b) {
			out = append(out, c)
			continue
		}
		if b[i+1] == 'u' && i+5 < len(b) && b[i+2] == '2' && b[i+3] == '0' && b[i+4] == '2' && (b[i+5] == '8' || b[i+5] == '9') {
			if b[i+5] == '8' {
				out = append(out, " "...)
			} else {
				out = append(out, " "...)
			}
			i += 5
			continue
		}
		// any other escape: copy both bytes so an escaped backslash is skipped
		out = append(out, c, b[i+1])
		i++
	}
	return out
}

// UTF16Len is the JS string length of s.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// SliceUTF16 returns the first n UTF-16 code units of s (JS s.slice(0, n)).
// A surrogate pair that would be split is dropped entirely.
func SliceUTF16(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if count+w > n {
			return s[:i]
		}
		count += w
	}
	return s
}

// TruncateBytes cuts s to at most max bytes on a rune boundary.
func TruncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

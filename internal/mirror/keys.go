package mirror

import "strings"

// Faceclaw's Terminal sends only text (voice or the phone keyboard), plus
// Enter and Esc from its window menu, so menus like Claude Code's folder
// trust or permission prompts can't be navigated. A submitted line made
// only of key words ("down", "down down enter", "shift tab", "two") is
// pressed as those keys instead of typed.

type keyWord struct {
	words  []string
	normal string
	app    string // in application-cursor mode, if different
}

var keyWords = []keyWord{
	{[]string{"shift", "tab"}, "\x1b[Z", ""},
	{[]string{"back", "tab"}, "\x1b[Z", ""},
	{[]string{"page", "up"}, "\x1b[5~", ""},
	{[]string{"page", "down"}, "\x1b[6~", ""},
	{[]string{"control", "c"}, "\x03", ""},
	{[]string{"ctrl", "c"}, "\x03", ""},
	{[]string{"arrow", "up"}, "\x1b[A", "\x1bOA"},
	{[]string{"arrow", "down"}, "\x1b[B", "\x1bOB"},
	{[]string{"up"}, "\x1b[A", "\x1bOA"},
	{[]string{"down"}, "\x1b[B", "\x1bOB"},
	{[]string{"right"}, "\x1b[C", "\x1bOC"},
	{[]string{"left"}, "\x1b[D", "\x1bOD"},
	{[]string{"enter"}, "\r", ""},
	{[]string{"return"}, "\r", ""},
	{[]string{"confirm"}, "\r", ""},
	{[]string{"escape"}, "\x1b", ""},
	{[]string{"esc"}, "\x1b", ""},
	{[]string{"cancel"}, "\x1b", ""},
	{[]string{"tab"}, "\t", ""},
	{[]string{"space"}, " ", ""},
	{[]string{"backspace"}, "\x7f", ""},
}

var digitWords = map[string]string{
	"one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
	"six": "6", "seven": "7", "eight": "8", "nine": "9",
	"1": "1", "2": "2", "3": "3", "4": "4", "5": "5", "6": "6", "7": "7", "8": "8", "9": "9",
}

// keysFor returns the key presses a line of key words stands for, or false
// when the line is ordinary text. appCursor selects the arrow encoding.
func keysFor(text string, appCursor bool) ([]string, bool) {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == '!' || r == '?' || r == '-' || r == '\t'
	})
	if len(words) == 0 || len(words) > 20 {
		return nil, false
	}
	var keys []string
	for i := 0; i < len(words); {
		if d, ok := digitWords[words[i]]; ok {
			keys = append(keys, d)
			i++
			continue
		}
		matched := false
		for _, k := range keyWords {
			if i+len(k.words) > len(words) {
				continue
			}
			same := true
			for j, w := range k.words {
				if words[i+j] != w {
					same = false
					break
				}
			}
			if same {
				seq := k.normal
				if appCursor && k.app != "" {
					seq = k.app
				}
				keys = append(keys, seq)
				i += len(k.words)
				matched = true
				break
			}
		}
		if !matched {
			return nil, false
		}
	}
	return keys, true
}

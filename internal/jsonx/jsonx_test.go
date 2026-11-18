package jsonx

import (
	"encoding/json"
	"testing"
)

func TestMarshalLikeJS(t *testing.T) {
	got := string(Marshal(map[string]string{"a": "<b>&   \\u2028"}))
	want := `{"a":"<b>&` + "  " + ` \\u2028"}`
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
	if got := string(Marshal(struct{ F float64 }{1.4539427999999999})); got != `{"F":1.4539427999999999}` {
		t.Errorf("float %s", got)
	}
	if got := string(Marshal(struct{ F float64 }{0})); got != `{"F":0}` {
		t.Errorf("zero %s", got)
	}
}

func TestCapStringsKeepsOrder(t *testing.T) {
	raw := json.RawMessage(`{"z":"abcdef","a":[1,{"y":"xyz12345"}],"n":null,"b":true}`)
	if got := string(CapStrings(raw, 3)); got != `{"z":"abc…","a":[1,{"y":"xyz…"}],"n":null,"b":true}` {
		t.Errorf("got %s", got)
	}
	if got := string(CapStrings(raw, 0)); got != string(raw) {
		t.Errorf("unchanged %s", got)
	}
}

func TestUTF16(t *testing.T) {
	s := "a😀b"
	if UTF16Len(s) != 4 || SliceUTF16(s, 2) != "a" || SliceUTF16(s, 3) != "a😀" {
		t.Error("utf16 slicing")
	}
	if TruncateBytes("héllo", 2) != "h" {
		t.Error("byte truncation")
	}
}

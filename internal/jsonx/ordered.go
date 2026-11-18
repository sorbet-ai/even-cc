package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Kind of an ordered JSON value.
type Kind int

const (
	Null Kind = iota
	Bool
	Number
	String
	Array
	Object
)

// Member is one key/value pair of an object, in source order.
type Member struct {
	Key   string
	Value *Value
}

// Value is a JSON value that keeps object key order.
type Value struct {
	Kind Kind
	Bool bool
	Num  json.Number
	Str  string
	Arr  []*Value
	Obj  []Member
}

// Parse decodes raw JSON into an ordered Value.
func Parse(raw []byte) (*Value, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (*Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &Value{Kind: Object, Obj: []Member{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("jsonx: bad object key %v", kt)
				}
				child, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				v.Obj = append(v.Obj, Member{Key: key, Value: child})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return v, nil
		case '[':
			v := &Value{Kind: Array, Arr: []*Value{}}
			for dec.More() {
				child, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				v.Arr = append(v.Arr, child)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return v, nil
		}
		return nil, fmt.Errorf("jsonx: unexpected delimiter %v", t)
	case string:
		return &Value{Kind: String, Str: t}, nil
	case json.Number:
		return &Value{Kind: Number, Num: t}, nil
	case bool:
		return &Value{Kind: Bool, Bool: t}, nil
	case nil:
		return &Value{Kind: Null}, nil
	}
	return nil, fmt.Errorf("jsonx: unexpected token %v", tok)
}

// Get returns the member value for key, or nil.
func (v *Value) Get(key string) *Value {
	if v == nil || v.Kind != Object {
		return nil
	}
	for _, m := range v.Obj {
		if m.Key == key {
			return m.Value
		}
	}
	return nil
}

// MarshalJSON encodes the value keeping key order.
func (v *Value) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	v.encode(&buf)
	return buf.Bytes(), nil
}

func (v *Value) encode(buf *bytes.Buffer) {
	if v == nil {
		buf.WriteString("null")
		return
	}
	switch v.Kind {
	case Null:
		buf.WriteString("null")
	case Bool:
		buf.WriteString(strconv.FormatBool(v.Bool))
	case Number:
		buf.WriteString(v.Num.String())
	case String:
		buf.Write(Marshal(v.Str))
	case Array:
		buf.WriteByte('[')
		for i, c := range v.Arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			c.encode(buf)
		}
		buf.WriteByte(']')
	case Object:
		buf.WriteByte('{')
		for i, m := range v.Obj {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(Marshal(m.Key))
			buf.WriteByte(':')
			m.Value.encode(buf)
		}
		buf.WriteByte('}')
	}
}

// CapStrings returns raw with every string value longer than max UTF-16
// units cut to max and suffixed with "…". Key order is preserved. max <= 0
// returns raw unchanged.
func CapStrings(raw json.RawMessage, max int) json.RawMessage {
	if max <= 0 || len(raw) == 0 {
		return raw
	}
	v, err := Parse(raw)
	if err != nil {
		return raw
	}
	if !capValue(v, max) {
		return raw
	}
	b, _ := v.MarshalJSON()
	return b
}

func capValue(v *Value, max int) bool {
	changed := false
	switch v.Kind {
	case String:
		if UTF16Len(v.Str) > max {
			v.Str = SliceUTF16(v.Str, max) + "…"
			changed = true
		}
	case Array:
		for _, c := range v.Arr {
			if capValue(c, max) {
				changed = true
			}
		}
	case Object:
		for _, m := range v.Obj {
			if capValue(m.Value, max) {
				changed = true
			}
		}
	}
	return changed
}

package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrNotObject means the JSON value is valid but isn't an object.
var ErrNotObject = errors.New("not a JSON object")

// Object is a JSON object that remembers the order of its keys. Values are
// kept as raw JSON, so untouched values round-trip byte for byte (apart from
// indentation, which Marshal normalises).
type Object struct {
	members []member
}

type member struct {
	key string
	val json.RawMessage
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{} }

// ParseObject parses b, which must hold exactly one JSON object.
func ParseObject(b []byte) (*Object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ErrNotObject
	}
	o := &Object{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("parsing JSON key: %w", err)
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("parsing JSON: unexpected token %v", kt)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("parsing JSON value for %q: %w", key, err)
		}
		o.Set(key, raw)
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("parsing JSON: trailing data after object")
	}
	return o, nil
}

// Len returns the number of keys.
func (o *Object) Len() int { return len(o.members) }

// Keys returns the keys in order.
func (o *Object) Keys() []string {
	keys := make([]string, len(o.members))
	for i, m := range o.members {
		keys[i] = m.key
	}
	return keys
}

// Get returns the raw value for key.
func (o *Object) Get(key string) (json.RawMessage, bool) {
	for _, m := range o.members {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

// Set replaces key's value in place, or appends the key if it is new. If the
// input had the key more than once, the later copies are dropped, which
// matches how encoding/json reads duplicates (the last one wins) closely
// enough for settings files.
func (o *Object) Set(key string, val json.RawMessage) {
	for i, m := range o.members {
		if m.key == key {
			o.members[i].val = val
			o.dropDuplicates(key, i)
			return
		}
	}
	o.members = append(o.members, member{key: key, val: val})
}

func (o *Object) dropDuplicates(key string, keep int) {
	out := o.members[:keep+1]
	for _, m := range o.members[keep+1:] {
		if m.key != key {
			out = append(out, m)
		}
	}
	o.members = out
}

// Delete removes key and reports whether it was present.
func (o *Object) Delete(key string) bool {
	for i, m := range o.members {
		if m.key == key {
			o.members = append(o.members[:i], o.members[i+1:]...)
			return true
		}
	}
	return false
}

// MarshalJSON encodes the object compactly, keeping key order.
func (o *Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o.members {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(m.key)
		if err != nil {
			return nil, fmt.Errorf("encoding key %q: %w", m.key, err)
		}
		buf.Write(k)
		buf.WriteByte(':')
		if err := json.Compact(&buf, m.val); err != nil {
			return nil, fmt.Errorf("encoding value for %q: %w", m.key, err)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Indented encodes the object with two-space indentation and a trailing
// newline, the layout harness settings files use.
func (o *Object) Indented() ([]byte, error) {
	compact, err := o.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", "  "); err != nil {
		return nil, fmt.Errorf("indenting JSON: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// Array splits a raw JSON array into its elements, keeping each as raw JSON.
func Array(raw json.RawMessage) ([]json.RawMessage, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, fmt.Errorf("expected a JSON array: %w", err)
	}
	if elems == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("[]")) {
		return nil, errors.New("expected a JSON array, got null")
	}
	return elems, nil
}

// MarshalArray joins raw elements into a JSON array.
func MarshalArray(elems []json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, e := range elems {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(e)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

// Equal reports whether two raw JSON values are equal once whitespace is
// removed. Key order matters, which is what idempotency checks want.
func Equal(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

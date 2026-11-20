package jsonedit

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestParseObjectKeepsOrderAndValues(t *testing.T) {
	in := `{"z": 1, "a": {"n": 2.50, "big": 12345678901234567890}, "m": [true, null], "s": "é\n"}`
	o, err := ParseObject([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Keys(); !reflect.DeepEqual(got, []string{"z", "a", "m", "s"}) {
		t.Fatalf("keys = %v", got)
	}
	out, err := o.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"z":1,"a":{"n":2.50,"big":12345678901234567890},"m":[true,null],"s":"é\n"}`
	if string(out) != want {
		t.Fatalf("round trip\n got %s\nwant %s", out, want)
	}
}

func TestObjectSetDelete(t *testing.T) {
	o, err := ParseObject([]byte(`{"a":1,"b":2,"a":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := o.Get("a"); string(v) != "3" || o.Len() != 2 {
		t.Fatalf("duplicate key: a=%s len=%d", v, o.Len())
	}
	o.Set("b", json.RawMessage(`"x"`))
	o.Set("c", json.RawMessage(`[]`))
	if !o.Delete("a") || o.Delete("missing") {
		t.Fatal("Delete results wrong")
	}
	out, _ := o.MarshalJSON()
	if string(out) != `{"b":"x","c":[]}` {
		t.Fatalf("got %s", out)
	}
}

func TestParseObjectErrors(t *testing.T) {
	for _, in := range []string{``, `[]`, `"x"`, `{"a":}`, `{"a":1} {}`, `{"a":1`} {
		if _, err := ParseObject([]byte(in)); err == nil {
			t.Errorf("ParseObject(%q) succeeded", in)
		}
	}
	if _, err := ParseObject([]byte(`[1]`)); !errors.Is(err, ErrNotObject) {
		t.Errorf("array err = %v, want ErrNotObject", err)
	}
}

func TestArray(t *testing.T) {
	if _, err := Array(json.RawMessage(`null`)); err == nil {
		t.Error("null accepted as array")
	}
	if _, err := Array(json.RawMessage(`{}`)); err == nil {
		t.Error("object accepted as array")
	}
	elems, err := Array(json.RawMessage(` [ ] `))
	if err != nil || len(elems) != 0 {
		t.Errorf("empty array: %v %v", elems, err)
	}
	elems, _ = Array(json.RawMessage(`[{"a": 1}, 2]`))
	if string(MarshalArray(elems)) != `[{"a": 1},2]` {
		t.Errorf("MarshalArray = %s", MarshalArray(elems))
	}
}

func TestEqual(t *testing.T) {
	if !Equal(json.RawMessage(`{ "a" : [1, 2] }`), json.RawMessage(`{"a":[1,2]}`)) {
		t.Error("whitespace should not matter")
	}
	if Equal(json.RawMessage(`{"a":1,"b":2}`), json.RawMessage(`{"b":2,"a":1}`)) {
		t.Error("key order should matter")
	}
	if Equal(json.RawMessage(`{`), json.RawMessage(`{`)) {
		t.Error("invalid JSON compared equal")
	}
}

func TestStripJSONC(t *testing.T) {
	tests := []struct {
		name, in, want string
		changed        bool
	}{
		{"plain", `{"a": 1}`, `{"a": 1}`, false},
		{"line comment", "{\"a\": 1 // one\n}", "{\"a\": 1       \n}", true},
		{"block comment", `{/* x */"a": 1}`, `{       "a": 1}`, true},
		{"trailing commas", `{"a": [1, 2,], }`, `{"a": [1, 2 ]  }`, true},
		{"comment-like string", `{"u": "http://x/*y*/", "c": "a,]"}`, `{"u": "http://x/*y*/", "c": "a,]"}`, false},
		{"escaped quote", `{"q": "say \"//hi\"", }`, `{"q": "say \"//hi\""  }`, true},
		{"bom", "\xef\xbb\xbf{}", "{}", false},
		{"comma then comment", "{\"a\": 1, // c\n}", "{\"a\": 1      \n}", true},
		{"unterminated block", `{} /* x`, `{}     `, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := StripJSONC([]byte(tc.in))
			if string(got) != tc.want || changed != tc.changed {
				t.Fatalf("StripJSONC(%q) = %q, %v; want %q, %v", tc.in, got, changed, tc.want, tc.changed)
			}
		})
	}
}

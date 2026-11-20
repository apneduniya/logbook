package jsonedit

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSetMember(t *testing.T) {
	tests := []struct {
		name string
		src  string
		val  string // "" removes the member
		want string
	}{
		{
			name: "replace keeps comments",
			src:  "{\n  // c1\n  \"a\": 1,\n  \"hooks\": {\"x\": 1}, // c2\n  /* c3 */ \"b\": 2,\n}\n",
			val:  `{"y":[1]}`,
			want: "{\n  // c1\n  \"a\": 1,\n  \"hooks\": {\n    \"y\": [\n      1\n    ]\n  }, // c2\n  /* c3 */ \"b\": 2,\n}\n",
		},
		{
			name: "insert before closing line",
			src:  "{\n  \"a\": 1 // note\n}\n",
			val:  `{}`,
			want: "{\n  \"a\": 1, // note\n  \"hooks\": {}\n}\n",
		},
		{
			name: "insert after trailing comma",
			src:  "{\n  \"a\": 1,\n}\n",
			val:  `{}`,
			want: "{\n  \"a\": 1,\n  \"hooks\": {}\n}\n",
		},
		{
			name: "insert into empty object",
			src:  "{}",
			val:  `{}`,
			want: "{\n  \"hooks\": {}\n}",
		},
		{
			name: "insert on one line",
			src:  `{"a":1}`,
			val:  `1`,
			want: "{\"a\":1,\n  \"hooks\": 1\n}",
		},
		{
			name: "remove middle member",
			src:  "{\n  \"a\": 1,\n  \"hooks\": {},\n  // keep\n  \"b\": 2\n}\n",
			want: "{\n  \"a\": 1,\n  // keep\n  \"b\": 2\n}\n",
		},
		{
			name: "remove last member",
			src:  "{\n  \"a\": 1, // keep\n  \"hooks\": {}\n}\n",
			want: "{\n  \"a\": 1 // keep\n}\n",
		},
		{
			name: "remove only member",
			src:  `{"hooks": {}}`,
			want: `{}`,
		},
		{
			name: "remove absent member",
			src:  "{\"a\": 1} // x\n",
			want: "{\"a\": 1} // x\n",
		},
		{
			name: "byte order mark kept",
			src:  "\xef\xbb\xbf{\"hooks\": 1}",
			val:  `2`,
			want: "\xef\xbb\xbf{\"hooks\": 2}",
		},
		{
			name: "strings that look like comments",
			src:  "{\"u\": \"http://x/*y*/\", \"hooks\": 1}",
			val:  `2`,
			want: "{\"u\": \"http://x/*y*/\", \"hooks\": 2}",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var val json.RawMessage
			if tc.val != "" {
				val = json.RawMessage(tc.val)
			}
			got, err := SetMember([]byte(tc.src), "hooks", val)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got\n%q\nwant\n%q", got, tc.want)
			}
			plain, _ := StripJSONC(got)
			if !json.Valid(plain) {
				t.Fatalf("result isn't valid JSONC: %s", got)
			}
		})
	}
}

func TestSetMemberErrors(t *testing.T) {
	if _, err := SetMember([]byte(`{"hooks":1,"hooks":2}`), "hooks", nil); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate key: %v", err)
	}
	if _, err := SetMember([]byte(`[1]`), "hooks", json.RawMessage(`1`)); !errors.Is(err, ErrNotObject) {
		t.Fatalf("array: %v", err)
	}
	if _, err := SetMember([]byte(`{"a":`), "hooks", json.RawMessage(`1`)); err == nil {
		t.Fatal("truncated input accepted")
	}
}

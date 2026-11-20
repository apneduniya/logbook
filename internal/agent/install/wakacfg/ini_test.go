package wakacfg

import (
	"errors"
	"testing"
)

func TestDocGet(t *testing.T) {
	src := "top = 1\n; c\n[Settings] ; comment\nAPI_KEY: \"a;b\" ; c\napi_url = https://x/api/v1 # c\nexclude =\n    ^/tmp/\n    # skipped\n    ^/b/\nempty =\n[other]\napi_key = no\n[settings]\napi_key = last\n"
	d := parseDoc([]byte(src))
	tests := []struct {
		section, key, want string
		ok                 bool
	}{
		{"", "top", "1", true},
		{"settings", "api_url", "https://x/api/v1", true},
		{"settings", "api_key", "last", true}, // last occurrence wins
		{"settings", "exclude", "^/tmp/\n^/b/", true},
		{"settings", "empty", "", true},
		{"settings", "missing", "", false},
		{"other", "api_key", "no", true},
	}
	for _, tt := range tests {
		got, ok := d.get(tt.section, tt.key)
		if got != tt.want || ok != tt.ok {
			t.Errorf("get(%q, %q) = %q, %v; want %q, %v", tt.section, tt.key, got, ok, tt.want, tt.ok)
		}
	}
	if string(d.bytes()) != src {
		t.Fatalf("round trip changed the file:\n%q", d.bytes())
	}
}

func TestPlainValue(t *testing.T) {
	tests := map[string]string{
		` abc `:        "abc",
		`"a;b" ; c`:    "a;b",
		`'q' # c`:      "q",
		"`b`":          "b",
		`a # c`:        "a",
		`"a;b" x`:      `"a`,
		`http://h/x;y`: "http://h/x",
	}
	for in, want := range tests {
		if got := plainValue(in); got != want {
			t.Errorf("plainValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDocSet(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty file", "", "[settings]\nk = v\n"},
		{"replace keeps spelling and delimiter", "[settings]\nK:old\n", "[settings]\nK:v\n"},
		{"replace keeps spacing", "[settings]\nk  =  old # c\n", "[settings]\nk  =  v\n"},
		{"replace every occurrence and drop continuation", "[settings]\nk =\n  a\n  b\nx = 1\n[settings]\nk = 2\n", "[settings]\nk = v\nx = 1\n[settings]\nk = v\n"},
		{"add after last key, before trailing comment", "[settings]\na = 1\n\n# end\n", "[settings]\na = 1\nk = v\n\n# end\n"},
		{"add to existing empty section", "[settings]\n[git]\nx = 1\n", "[settings]\nk = v\n[git]\nx = 1\n"},
		{"add section at end", "[git]\nx = 1", "[git]\nx = 1\n\n[settings]\nk = v\n"},
		{"crlf kept", "[settings]\r\na = 1\r\n", "[settings]\r\na = 1\r\nk = v\r\n"},
		{"no trailing newline kept", "[settings]\na = 1", "[settings]\na = 1\nk = v"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := parseDoc([]byte(tt.in))
			if err := d.set("settings", "k", "v"); err != nil {
				t.Fatal(err)
			}
			if got := string(d.bytes()); got != tt.want {
				t.Fatalf("got\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestDocSetRejectsUnsafeValues(t *testing.T) {
	d := parseDoc(nil)
	for _, v := range []string{"a\nb", "a#b", "a;b", "a\rb"} {
		if err := d.set("settings", "k", v); !errors.Is(err, errBadValue) {
			t.Errorf("set(%q) = %v, want errBadValue", v, err)
		}
	}
}

func TestDocRemove(t *testing.T) {
	d := parseDoc([]byte("[settings]\nk = 1\n  more\na = 2\n[x]\nk = 3\n"))
	if !d.remove("settings", "k") {
		t.Fatal("remove() = false")
	}
	if got := string(d.bytes()); got != "[settings]\na = 2\n[x]\nk = 3\n" {
		t.Fatalf("got %q", got)
	}
	if d.remove("settings", "k") {
		t.Fatal("second remove() = true")
	}
}
